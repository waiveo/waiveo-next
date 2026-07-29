package virtualplayer_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/virtualplayer"
)

// ply136Case mirrors exactly the fields conformance/corpora/player-1/
// PLY-136-valid-token-revoked-reconnect-clears-token-only.json declares — the
// frozen oracle for the reconnect state machine's SOLE credential-clearing
// condition (PLY-072/073/136). The test parses that file directly rather than
// hard-coding its values, so a corpus edit either keeps this test honest or
// breaks it loudly.
type ply136Case struct {
	Input struct {
		PlayerPersistedState struct {
			RelayAddress struct {
				Host string `json:"host"`
				Port int    `json:"port"`
			} `json:"relay_address"`
			TrustAnchorPresent  bool `json:"trust_anchor_present"`
			ChannelTokenPresent bool `json:"channel_token_present"`
		} `json:"player_persisted_state"`
		ReconnectAttempt struct {
			Classification string `json:"classification"`
			RelayResponse  struct {
				ErrorCode string `json:"error_code"`
				Reason    string `json:"reason"`
			} `json:"relay_response"`
		} `json:"reconnect_attempt"`
	} `json:"input"`
	Expected struct {
		FailureClassification string `json:"failure_classification"`
		NetworkLevelFailure   bool   `json:"network_level_failure"`
		ChannelTokenCleared   bool   `json:"channel_token_cleared"`
		TrustAnchorCleared    bool   `json:"trust_anchor_cleared"`
		RelayAddressCleared   bool   `json:"relay_address_cleared"`
		PlayerState           string `json:"player_state"`
		ErrorCode             string `json:"error_code"`
	} `json:"expected"`
}

func loadPLY136(t *testing.T) ply136Case {
	t.Helper()
	_, self, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(self), "..", "..", "conformance", "corpora", "player-1", "PLY-136-valid-token-revoked-reconnect-clears-token-only.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read PLY-136 corpus case: %v", err)
	}
	var c ply136Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("unmarshal PLY-136 corpus case: %v", err)
	}
	return c
}

// TestReconnectDecisionPLY136 drives virtualplayer.ReconnectDecision against the
// frozen PLY-136 oracle: a reconnect that receives an explicit
// CHANNEL_TOKEN_REVOKED (not a network-level failure, not ordinary success) MUST
// clear ONLY the channel token, leave the persisted relay address AND
// trust-anchor material untouched (never-wipe, PLY-026/063/136), and return the
// player to Pairing redemption (PLY-073's terminal path) — every one of the
// corpus's seven expected fields asserted.
func TestReconnectDecisionPLY136(t *testing.T) {
	c := loadPLY136(t)

	state := virtualplayer.PersistedState{
		RelayAddress: virtualplayer.RelayAddress{
			Host: c.Input.PlayerPersistedState.RelayAddress.Host,
			Port: c.Input.PlayerPersistedState.RelayAddress.Port,
		},
		TrustAnchorPresent:  c.Input.PlayerPersistedState.TrustAnchorPresent,
		ChannelTokenPresent: c.Input.PlayerPersistedState.ChannelTokenPresent,
	}
	attempt := virtualplayer.ReconnectAttempt{
		Classification: c.Input.ReconnectAttempt.Classification,
		RelayResponse: virtualplayer.RelayResponse{
			ErrorCode: c.Input.ReconnectAttempt.RelayResponse.ErrorCode,
			Reason:    c.Input.ReconnectAttempt.RelayResponse.Reason,
		},
	}

	got := virtualplayer.ReconnectDecision(state, attempt)

	if got.FailureClassification != c.Expected.FailureClassification {
		t.Errorf("failure_classification = %q, want %q", got.FailureClassification, c.Expected.FailureClassification)
	}
	if got.NetworkLevelFailure != c.Expected.NetworkLevelFailure {
		t.Errorf("network_level_failure = %v, want %v", got.NetworkLevelFailure, c.Expected.NetworkLevelFailure)
	}
	if got.ChannelTokenCleared != c.Expected.ChannelTokenCleared {
		t.Errorf("channel_token_cleared = %v, want %v (PLY-136)", got.ChannelTokenCleared, c.Expected.ChannelTokenCleared)
	}
	if got.TrustAnchorCleared != c.Expected.TrustAnchorCleared {
		t.Errorf("trust_anchor_cleared = %v, want %v (PLY-136 never-wipe)", got.TrustAnchorCleared, c.Expected.TrustAnchorCleared)
	}
	if got.RelayAddressCleared != c.Expected.RelayAddressCleared {
		t.Errorf("relay_address_cleared = %v, want %v (PLY-026/136 never-wipe)", got.RelayAddressCleared, c.Expected.RelayAddressCleared)
	}
	if got.PlayerState != c.Expected.PlayerState {
		t.Errorf("player_state = %q, want %q (PLY-073)", got.PlayerState, c.Expected.PlayerState)
	}
	if got.ErrorCode != c.Expected.ErrorCode {
		t.Errorf("error_code = %q, want %q", got.ErrorCode, c.Expected.ErrorCode)
	}
}

// TestReconnectDecisionUnreachableClearsNothing pins PLY-135/130's contrast the
// corpus description itself draws: an `unreachable` connection failure (no
// response at the network level) MUST clear NOTHING — not the token, not the
// trust anchor, not the address — and MUST NOT return the player to Pairing
// redemption; it retries the persisted address with capped backoff, forever.
func TestReconnectDecisionUnreachableClearsNothing(t *testing.T) {
	state := virtualplayer.PersistedState{
		RelayAddress:        virtualplayer.RelayAddress{Host: "198.51.100.12", Port: 5173},
		TrustAnchorPresent:  true,
		ChannelTokenPresent: true,
	}
	got := virtualplayer.ReconnectDecision(state, virtualplayer.ReconnectAttempt{
		Classification: "unreachable",
	})

	if !got.NetworkLevelFailure {
		t.Errorf("network_level_failure = false, want true for an unreachable failure (PLY-133)")
	}
	if got.ChannelTokenCleared || got.TrustAnchorCleared || got.RelayAddressCleared {
		t.Errorf("unreachable cleared a credential: %+v — PLY-135 requires it clear nothing", got)
	}
	if got.PlayerState == "returns_to_pairing_redemption" {
		t.Errorf("unreachable returned to pairing redemption — PLY-135/140 forbid it (retry, don't re-pair)")
	}
}

// TestReconnectDecisionExpiredIsRenewableNotRepair pins PLY-073's other half:
// CHANNEL_TOKEN_EXPIRED is retryable via renewal, and MUST NOT be treated as a
// trigger to re-enter Pairing redemption OR to clear the token — the sharp line
// PLY-072/073 draw between EXPIRED (renew) and REVOKED (re-pair).
func TestReconnectDecisionExpiredIsRenewableNotRepair(t *testing.T) {
	state := virtualplayer.PersistedState{
		RelayAddress:        virtualplayer.RelayAddress{Host: "198.51.100.12", Port: 5173},
		TrustAnchorPresent:  true,
		ChannelTokenPresent: true,
	}
	got := virtualplayer.ReconnectDecision(state, virtualplayer.ReconnectAttempt{
		Classification: "response_received",
		RelayResponse:  virtualplayer.RelayResponse{ErrorCode: "CHANNEL_TOKEN_EXPIRED"},
	})

	if got.ChannelTokenCleared {
		t.Errorf("CHANNEL_TOKEN_EXPIRED cleared the token — PLY-073 requires renewal, not clearing")
	}
	if got.PlayerState == "returns_to_pairing_redemption" {
		t.Errorf("CHANNEL_TOKEN_EXPIRED returned to pairing redemption — PLY-073 forbids it (renew instead)")
	}
	if got.ErrorCode != "CHANNEL_TOKEN_EXPIRED" {
		t.Errorf("error_code = %q, want CHANNEL_TOKEN_EXPIRED", got.ErrorCode)
	}
}

// TestReconnectRevokedClearsTokenOnlyEndToEnd exercises BOTH committed pieces
// together: the virtual player pairs against a live in-process relay (retaining
// the relay address + trust anchor a real reconnect would keep), the relay
// revokes the paired screen (its REL-066 revocation surface), and a fresh
// program pull on the still-held token draws an explicit CHANNEL_TOKEN_REVOKED —
// DISTINCT from the CHANNEL_TOKEN_EXPIRED an expired token would draw. Feeding
// that observed attempt into ReconnectDecision yields the PLY-136 outcome:
// token cleared, address + trust anchor kept, back to Pairing redemption.
func TestReconnectRevokedClearsTokenOnlyEndToEnd(t *testing.T) {
	feederBaseURL, _ := bootTestFeeder(t)
	host, port, certDER, applied, relayServer := bootPreemptRelay(t, feederBaseURL)

	if len(applied.PairingGrants) == 0 {
		t.Fatalf("applied desired state carried no pairing_grants")
	}
	code, err := playerserver.FormPairingCode(host, port, applied.PairingGrants[0], certDER)
	if err != nil {
		t.Fatalf("FormPairingCode: %v", err)
	}

	sess, err := virtualplayer.Pair(code)
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if sess.ChannelToken == "" || sess.ScreenID == "" {
		t.Fatalf("Pair returned an incomplete session: %+v", sess)
	}

	// While unrevoked, a pull is ordinary success — no error_code — which is
	// exactly what makes the revoked response below a DISTINCT signal, not the
	// player's default failure mode.
	okAttempt, err := sess.PullProgram()
	if err != nil {
		t.Fatalf("PullProgram (pre-revoke): %v", err)
	}
	if okAttempt.RelayResponse.ErrorCode != "" {
		t.Fatalf("pre-revoke pull carried error_code %q, want an ordinary success", okAttempt.RelayResponse.ErrorCode)
	}

	// The relay's own revocation surface (REL-066): a newer generation's
	// `revoked` set names the paired screen, so it no longer validates.
	if err := relayServer.SetRevokedScreens(1, []string{sess.ScreenID}); err != nil {
		t.Fatalf("SetRevokedScreens: %v", err)
	}

	attempt, err := sess.PullProgram()
	if err != nil {
		t.Fatalf("PullProgram (post-revoke): %v", err)
	}
	if attempt.Classification != "response_received" {
		t.Errorf("post-revoke classification = %q, want response_received (PLY-136: not a network failure)", attempt.Classification)
	}
	if attempt.RelayResponse.ErrorCode != "CHANNEL_TOKEN_REVOKED" {
		t.Fatalf("post-revoke error_code = %q, want CHANNEL_TOKEN_REVOKED (PLY-072/073)", attempt.RelayResponse.ErrorCode)
	}

	state := virtualplayer.PersistedState{
		RelayAddress:        virtualplayer.RelayAddress{Host: host, Port: port},
		TrustAnchorPresent:  true,
		ChannelTokenPresent: true,
	}
	got := virtualplayer.ReconnectDecision(state, attempt)

	if !got.ChannelTokenCleared {
		t.Errorf("revoked reconnect did not clear the channel token (PLY-136)")
	}
	if got.TrustAnchorCleared {
		t.Errorf("revoked reconnect cleared the trust anchor — PLY-136 keeps it")
	}
	if got.RelayAddressCleared {
		t.Errorf("revoked reconnect cleared the relay address — PLY-026/136 never-wipe")
	}
	if got.PlayerState != "returns_to_pairing_redemption" {
		t.Errorf("player_state = %q, want returns_to_pairing_redemption (PLY-073)", got.PlayerState)
	}
}
