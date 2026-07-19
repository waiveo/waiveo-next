package virtualplayer

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/shared/paircode"
)

// The reconnect-path classification vocabulary player/1 defines (PLY-133) plus
// the two credential-rejection error codes it turns on (PLY-072). These are the
// exact string values the PLY-136 corpus case asserts, kept as named constants
// so the pure decision below and its callers never drift on a literal.
const (
	// ClassUnreachable is PLY-133's "no response at the network level" — a
	// transient failure that clears nothing (PLY-135) and retries forever.
	ClassUnreachable = "unreachable"
	// ClassUnavailable is PLY-133's "a response was received, but the relay's
	// player/1 surface itself is not currently serving" — also transient.
	ClassUnavailable = "unavailable"
	// ClassResponseReceived is a reconnect that got an ordinary relay response
	// (success, or a typed Problem such as a token rejection) — the only
	// classification a credential-clearing decision can turn on (PLY-136).
	ClassResponseReceived = "response_received"

	// CodeChannelTokenRevoked is PLY-072/073's terminal token rejection: the
	// SOLE reconnect-path condition that clears a credential (PLY-136).
	CodeChannelTokenRevoked = "CHANNEL_TOKEN_REVOKED"
	// CodeChannelTokenExpired is PLY-072/073's renewable token rejection: retry
	// via renewal (PLY-074), never re-pair, never clear.
	CodeChannelTokenExpired = "CHANNEL_TOKEN_EXPIRED"
)

// RelayAddress is a relay's dial address a player persists (Server-locating,
// PLY-026) — the address the Reconnect state machine retries forever and MUST
// NOT wipe on any transient failure or on token revocation (never-wipe,
// PLY-130/136).
type RelayAddress struct {
	Host string
	Port int
}

// PersistedState is the credential material a player carries across a reconnect
// (PLY-026/046/070): the relay address it last used, plus whether it currently
// holds trust-anchor material and a channel token. It mirrors the PLY-136 corpus
// case's own player_persisted_state input, field for field.
type PersistedState struct {
	RelayAddress        RelayAddress
	TrustAnchorPresent  bool
	ChannelTokenPresent bool
}

// RelayResponse is the relevant subset of a relay's reconnect response (PLY-072):
// a typed error_code (empty on ordinary success) and its reason. It mirrors the
// PLY-136 corpus case's relay_response input.
type RelayResponse struct {
	ErrorCode string
	Reason    string
}

// ReconnectAttempt is one classified reconnect outcome (PLY-133): how the
// attempt itself resolved (ClassUnreachable / ClassUnavailable /
// ClassResponseReceived) plus, when a response was received, the relay's own
// typed response. It mirrors the PLY-136 corpus case's reconnect_attempt input,
// and is what a live probe (Session.PullProgram) produces from the wire.
type ReconnectAttempt struct {
	Classification string
	RelayResponse  RelayResponse
}

// ReconnectOutcome is the credential-clearing decision the Reconnect state
// machine reaches for a ReconnectAttempt (PLY-135/136/073) — exactly the PLY-136
// corpus case's expected block: which (if any) credential clears, whether the
// failure was network-level, and the player state the attempt leaves the client
// in. It is the differential-oracle-visible result Task 5's driver diffs.
type ReconnectOutcome struct {
	FailureClassification string
	NetworkLevelFailure   bool
	ChannelTokenCleared   bool
	TrustAnchorCleared    bool
	RelayAddressCleared   bool
	PlayerState           string
	ErrorCode             string
}

// The player-state and failure-classification labels ReconnectDecision emits.
// The two the PLY-136 corpus asserts (StateReturnsToPairingRedemption and
// FailExplicitCredentialRejection) are fixed by that oracle; the rest name the
// transient and renewal outcomes the same section governs (PLY-073/130/135).
const (
	// StateReturnsToPairingRedemption is PLY-073's terminal path: the player
	// clears its token and re-enters Pairing redemption.
	StateReturnsToPairingRedemption = "returns_to_pairing_redemption"
	// StateRetryWithCappedBackoff is PLY-130/131's transient path: retry the
	// persisted address forever, capped backoff, clearing nothing.
	StateRetryWithCappedBackoff = "retry_with_capped_backoff"
	// StateRenewsChannelToken is PLY-073/074's renewal path: renew the token
	// over the still-pinned connection, no re-pairing, clearing nothing.
	StateRenewsChannelToken = "renews_channel_token"
	// StateSteadyState is an ordinary successful reconnect: the player resumes
	// with its retained credentials (PLY-140).
	StateSteadyState = "steady_state"

	// FailExplicitCredentialRejection is the failure class of a revoked-token
	// reconnect (PLY-136): neither a network failure nor ordinary success.
	FailExplicitCredentialRejection = "explicit_credential_rejection"
	// FailRenewableCredentialExpiry is the failure class of an expired-token
	// reconnect (PLY-073): renewable, not terminal.
	FailRenewableCredentialExpiry = "renewable_credential_expiry"
	// FailOrdinarySuccess is not a failure at all — an ordinary served response.
	FailOrdinarySuccess = "ordinary_success"
)

// ReconnectDecision is the pure, network-free heart of player/1's Reconnect
// state machine credential-clearing rule (PLY-133/135/136/073/063): given a
// player's persisted credentials and one classified reconnect attempt, it
// decides what — if anything — to clear.
//
//   - A ClassUnreachable or ClassUnavailable failure is transient: it clears
//     NOTHING (not the token, not the trust anchor, not the address) and retries
//     the persisted address forever with capped backoff (PLY-130/131/135). Only
//     ClassUnreachable is a network-level failure (no response at all).
//   - A ClassResponseReceived attempt turns on the relay's typed error_code:
//     CodeChannelTokenRevoked is the SOLE reconnect condition that clears a
//     credential — it clears ONLY the channel token, keeps the persisted relay
//     address AND trust-anchor material (never-wipe, PLY-026/063/136), and
//     returns the player to Pairing redemption (PLY-073's terminal path).
//     CodeChannelTokenExpired is renewable (PLY-073/074): clears nothing, renews
//     the token, never re-pairs. An empty error_code is ordinary success. Any
//     other typed code is treated as transient (clears nothing, PLY-135).
//
// It performs no I/O; Session.PullProgram drives a live relay and produces the
// ReconnectAttempt this decides on.
func ReconnectDecision(state PersistedState, attempt ReconnectAttempt) ReconnectOutcome {
	switch attempt.Classification {
	case ClassUnreachable:
		// PLY-133: no response at the network level. PLY-135: clear nothing.
		return ReconnectOutcome{
			FailureClassification: ClassUnreachable,
			NetworkLevelFailure:   true,
			PlayerState:           StateRetryWithCappedBackoff,
		}
	case ClassResponseReceived:
		return decideOnResponse(attempt.RelayResponse)
	default:
		// ClassUnavailable, or any other non-response classification: a
		// response was received (or the relay's surface signalled it is not
		// serving) but nothing that clears a credential (PLY-133/135).
		return ReconnectOutcome{
			FailureClassification: ClassUnavailable,
			NetworkLevelFailure:   false,
			PlayerState:           StateRetryWithCappedBackoff,
		}
	}
}

// decideOnResponse resolves a ClassResponseReceived attempt on the relay's own
// typed error_code (PLY-072/073) — the branch where the sole credential-clearing
// condition (CodeChannelTokenRevoked) lives.
func decideOnResponse(resp RelayResponse) ReconnectOutcome {
	switch resp.ErrorCode {
	case CodeChannelTokenRevoked:
		// PLY-136: clear ONLY the channel token; keep address + trust anchor;
		// re-enter Pairing redemption.
		return ReconnectOutcome{
			FailureClassification: FailExplicitCredentialRejection,
			NetworkLevelFailure:   false,
			ChannelTokenCleared:   true,
			TrustAnchorCleared:    false,
			RelayAddressCleared:   false,
			PlayerState:           StateReturnsToPairingRedemption,
			ErrorCode:             resp.ErrorCode,
		}
	case CodeChannelTokenExpired:
		// PLY-073/074: renewable — clear nothing, renew, do NOT re-pair.
		return ReconnectOutcome{
			FailureClassification: FailRenewableCredentialExpiry,
			NetworkLevelFailure:   false,
			PlayerState:           StateRenewsChannelToken,
			ErrorCode:             resp.ErrorCode,
		}
	case "":
		// An ordinary served response: resume with retained credentials.
		return ReconnectOutcome{
			FailureClassification: FailOrdinarySuccess,
			NetworkLevelFailure:   false,
			PlayerState:           StateSteadyState,
		}
	default:
		// Any other typed rejection (INTERNAL, UNAVAILABLE, VALIDATION_FAILED, …)
		// is transient by default: PLY-135 — only PLY-136/063/073 clear anything.
		return ReconnectOutcome{
			FailureClassification: ClassUnavailable,
			NetworkLevelFailure:   false,
			PlayerState:           StateRetryWithCappedBackoff,
			ErrorCode:             resp.ErrorCode,
		}
	}
}

// Session is a paired, pinned player/1 client session against one relay: the
// channel token + screen_id it holds, over a connection already pinned to the
// relay's out-of-band commitment (Steady-state pinning, PLY-060). It is the
// reusable session Photon builds inline, exposed here so the Reconnect state
// machine can present an already-issued channel token on a fresh program pull
// and classify the relay's response — without re-running the whole first-photon
// and without wiping any persisted credential (never-wipe, PLY-026/140). The
// caller applies ReconnectDecision to the classified attempt to decide what, if
// anything, to clear.
type Session struct {
	// ChannelToken is the credential the player persists and presents (PLY-070);
	// a CodeChannelTokenRevoked pull is what drives clearing it (PLY-136).
	ChannelToken string
	// ScreenID is the screen this session is bound to (PLY-035).
	ScreenID string
	// RelayAddress is the persisted dial address this session pinned against
	// (PLY-026) — retained across every reconnect outcome.
	RelayAddress RelayAddress

	client   *http.Client
	base     string
	relayPub ed25519.PublicKey
	// commitment is the pinned relay's fingerprint commitment (PLY-052) — the
	// value pinnedClient re-checks on every connection. Retained so Relocate can
	// re-pin against the same relay at a new address (PLY-132/133) without
	// re-pairing.
	commitment []byte
}

// Pair runs the pairing prefix of the player/1 client thread for pairingCode —
// decode (PLY-024) -> TLS bootstrap fetch + OOB commitment pin (PLY-040/052) ->
// redeem the grant into a channel token (PLY-030–033) — and returns the pinned
// Session that redemption produced. It is exactly the prefix Photon runs before
// its first program pull, factored out so a steady-state reconnect can pull the
// program repeatedly on the retained token. On any failure it returns nil and a
// wrapped error, never a partial session.
func Pair(pairingCode string) (*Session, error) {
	host, port, grantSelector, commitment, err := paircode.Decode(pairingCode)
	if err != nil {
		return nil, fmt.Errorf("virtualplayer: Pair: decode pairing code: %w", err)
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	certDER, err := bootstrapFetchAndPin(addr, commitment)
	if err != nil {
		return nil, fmt.Errorf("virtualplayer: Pair: %w", err)
	}
	relayPub, err := publicKeyFromCertDER(certDER)
	if err != nil {
		return nil, fmt.Errorf("virtualplayer: Pair: relay certificate public key: %w", err)
	}

	client := pinnedClient(commitment)
	base := "https://" + addr

	pairResp, err := redeem(client, base, grantSelector)
	if err != nil {
		return nil, fmt.Errorf("virtualplayer: Pair: redeem: %w", err)
	}
	if pairResp.PairingStatus != "redeemed" || pairResp.ChannelToken == "" {
		return nil, fmt.Errorf("virtualplayer: Pair: redeem: pairing_status = %q, want %q with a channel_token", pairResp.PairingStatus, "redeemed")
	}

	return &Session{
		ChannelToken: pairResp.ChannelToken,
		ScreenID:     pairResp.ScreenID,
		RelayAddress: RelayAddress{Host: host, Port: port},
		client:       client,
		base:         base,
		relayPub:     relayPub,
		commitment:   commitment,
	}, nil
}

// PullProgram issues GET /player/v1/program on the session's retained channel
// token and classifies the relay's response into a ReconnectAttempt
// (PLY-072/133) — WITHOUT mutating any persisted credential (never-wipe;
// clearing is ReconnectDecision's call, not this probe's):
//
//   - a dial/transport-level failure (no response at all) is ClassUnreachable;
//   - a relay Problem response is ClassResponseReceived carrying its own typed
//     error_code, so CodeChannelTokenRevoked and CodeChannelTokenExpired stay
//     distinct on the wire (PLY-072);
//   - an ordinary success (a Lease that verifies against the pinned relay cert,
//     PLY-090) is ClassResponseReceived with an empty error_code.
//
// It returns a non-nil error only for a malformed success (a Lease whose
// signature does not verify) — a genuine integrity failure, not a reconnect
// classification.
func (s *Session) PullProgram() (ReconnectAttempt, error) {
	lease, err := pullProgram(s.client, s.base, s.ChannelToken)
	if err != nil {
		var prob *ProblemError
		if errors.As(err, &prob) {
			// A response WAS received — a typed relay rejection (PLY-072).
			return ReconnectAttempt{
				Classification: ClassResponseReceived,
				RelayResponse:  RelayResponse{ErrorCode: prob.Code, Reason: prob.Title},
			}, nil
		}
		// No response at the network level (PLY-133 unreachable).
		return ReconnectAttempt{Classification: ClassUnreachable}, nil
	}

	if err := verifyLeaseSignature(lease, s.relayPub); err != nil {
		return ReconnectAttempt{}, fmt.Errorf("virtualplayer: PullProgram: %w", err)
	}
	return ReconnectAttempt{Classification: ClassResponseReceived}, nil
}
