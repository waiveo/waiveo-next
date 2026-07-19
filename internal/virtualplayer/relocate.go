package virtualplayer

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
)

// The player/1 Reconnect state machine's backoff + re-locate tuning
// (PLY-131/132). PLY-131 fixes only that backoff MUST be capped (not the
// curve); PLY-132 fixes only that a fresh discovery attempt fires at a bounded
// multiple of ordinary retries (not the multiple). These are this software
// player's modeled values — the corpus exercises the ORDERING and CAPPING they
// produce (PLY-130), not elapsed real time.
const (
	// ReDiscoveryEveryNAttempts is the re-locate multiple (PLY-132): after this
	// many consecutive failed retries of the persisted address, the player runs
	// a fresh same-network discovery (PLY-021) so a relay that moved address is
	// rediscovered without operator intervention. PLY-132's draft-note proposes
	// every fourth attempt.
	ReDiscoveryEveryNAttempts = 4

	// BackoffBaseMillis is the first retry interval; each consecutive failure
	// doubles it (PLY-131), never past BackoffCapMillis.
	BackoffBaseMillis int64 = 1000
	// BackoffCapMillis is the hard ceiling on the retry interval (PLY-131):
	// retries continue indefinitely but the interval never grows past this.
	// PLY-131's draft-note proposes 60s.
	BackoffCapMillis int64 = 60000
)

// The wire results a reconnect sweep entry carries (PLY-133), matching the
// PLY-130 corpus's connection_attempts[].result values. ResultNoResponse maps
// to ClassUnreachable (no response at the network level); ResultUnavailable to
// ClassUnavailable (a response was received but the relay's player/1 surface is
// not serving); ResultSuccess is an ordinary reconnect.
const (
	ResultNoResponse  = "no_response"
	ResultUnavailable = "unavailable"
	ResultSuccess     = "success"
)

// DiscoveryResponse is a same-network discovery result (PLY-021/022): the
// service type advertised and the relay's player/1 location URL. It mirrors the
// PLY-130 corpus's discovery_response input; RelocateDecision parses the
// location's host:port to relocate the persisted address (PLY-132/133), and
// Session.Relocate drives the same relocation live.
type DiscoveryResponse struct {
	ST       string
	Location string
}

// ConnectionAttempt is one entry of a reconnect sweep (PLY-130): the attempt
// ordinal, the address dialed, the wire result (ResultNoResponse /
// ResultUnavailable / ResultSuccess), and — on the attempt where a fresh
// discovery fires (PLY-132) — the discovery response the environment returned.
// It mirrors the PLY-130 corpus's connection_attempts entries, field for field;
// ReDiscoveryTriggered is the corpus's own annotation, which RelocateDecision
// independently re-derives from its re-locate multiple.
type ConnectionAttempt struct {
	Attempt              int
	Address              RelayAddress
	Result               string
	ReDiscoveryTriggered bool
	DiscoveryResponse    *DiscoveryResponse
}

// RelocateOutcome is the aggregate result of driving the Reconnect/relocate
// state machine over a reconnect sweep (PLY-130) — exactly the PLY-130 corpus
// case's expected block: how the pre-success failures classify (PLY-133),
// whether backoff stayed capped (PLY-131), which attempt fired the fresh
// discovery (PLY-132), the address the player relocated to (PLY-132/133),
// whether either credential was cleared (never-wipe, PLY-026/130), whether it
// would retry forever, and the final reconnect result. It is the
// differential-oracle-visible result Task 5's driver diffs.
type RelocateOutcome struct {
	FailureClassificationBeforeRelocate string
	BackoffCapped                       bool
	ReDiscoveryFiredOnAttempt           int
	RelayAddressUpdatedTo               RelayAddress
	TrustAnchorCleared                  bool
	ChannelTokenCleared                 bool
	RetriesIndefinitely                 bool
	ReconnectResult                     string
}

// BackoffInterval is the capped reconnect backoff (PLY-131) for a given count
// of consecutive failures: an exponential curve from BackoffBaseMillis,
// doubling per failure, clamped so it NEVER exceeds BackoffCapMillis however
// many failures accrue. It is nondecreasing, reaches the cap, and returns 0 for
// zero failures — the property PLY-131 fixes (capped, unbounded retries),
// leaving the exact curve to measurement.
func BackoffInterval(consecutiveFailures int) int64 {
	if consecutiveFailures <= 0 {
		return 0
	}
	iv := BackoffBaseMillis
	for i := 1; i < consecutiveFailures; i++ {
		iv *= 2
		if iv >= BackoffCapMillis {
			return BackoffCapMillis
		}
	}
	return iv
}

// RelocateDecision is the pure, network-free heart of player/1's Reconnect +
// relocate state machine (PLY-022/026/130/131/132/133): given a player's
// persisted state and a sweep of classified connection attempts, it computes
// what the player does across the sweep WITHOUT ever wiping a credential.
//
//   - Each failed attempt is classified (PLY-133): ResultNoResponse ->
//     ClassUnreachable, ResultUnavailable -> ClassUnavailable. A connection
//     failure alone clears NOTHING — not the trust anchor, not the channel
//     token, not the persisted address (never-wipe, PLY-130/135) — however many
//     consecutive attempts fail; the player retries indefinitely.
//   - Backoff is always capped (PLY-131): BackoffCapped stays true because
//     BackoffInterval never exceeds the cap.
//   - On every ReDiscoveryEveryNAttempts-th consecutive failure the player fires
//     a fresh same-network discovery (PLY-132). If that attempt carries a
//     discovery response naming a new address, the player relocates its
//     persisted address to it (PLY-132/133) — an UPDATE, never a wipe.
//   - A successful attempt ends the sweep: the player persists whichever address
//     it succeeded on (PLY-026) and resumes with its retained credentials
//     (PLY-140).
//
// It performs no I/O; Session.Relocate + Session.PullProgram drive the same
// relocation against a live relay.
func RelocateDecision(state PersistedState, attempts []ConnectionAttempt) RelocateOutcome {
	out := RelocateOutcome{
		// never-wipe default: the persisted address is retained unless a
		// successful discovery relocates it (PLY-026/130).
		RelayAddressUpdatedTo: state.RelayAddress,
		BackoffCapped:         true,
		RetriesIndefinitely:   true,
	}

	seenUnreachable := false
	seenUnavailable := false
	consecutiveFailures := 0

	for _, at := range attempts {
		if at.Result == ResultSuccess {
			out.ReconnectResult = "success"
			// PLY-026: persist whichever address the player last successfully used.
			out.RelayAddressUpdatedTo = at.Address
			break
		}

		consecutiveFailures++
		switch at.Result {
		case ResultUnavailable:
			seenUnavailable = true
		default:
			seenUnreachable = true
		}

		// PLY-131: the interval is capped by construction; surface a violation
		// only if BackoffInterval ever broke that (it cannot, but the check keeps
		// the invariant honest).
		if BackoffInterval(consecutiveFailures) > BackoffCapMillis {
			out.BackoffCapped = false
		}

		// PLY-132: fire a fresh discovery on the re-locate multiple, and relocate
		// if it named a new address.
		if consecutiveFailures%ReDiscoveryEveryNAttempts == 0 {
			if out.ReDiscoveryFiredOnAttempt == 0 {
				out.ReDiscoveryFiredOnAttempt = at.Attempt
			}
			if at.DiscoveryResponse != nil {
				if addr, ok := ParseDiscoveryLocation(at.DiscoveryResponse.Location); ok {
					out.RelayAddressUpdatedTo = addr
				}
			}
		}
	}

	out.FailureClassificationBeforeRelocate = aggregateFailureClass(seenUnreachable, seenUnavailable)
	// never-wipe (PLY-130): connection failure alone never clears a credential.
	out.TrustAnchorCleared = false
	out.ChannelTokenCleared = false
	return out
}

// aggregateFailureClass reduces the per-attempt PLY-133 classifications seen
// across a sweep to a single operator-facing class: ClassUnreachable if every
// failure was network-level, ClassUnavailable if every failure was a served
// not-serving response, "mixed" if both occurred, and "" if there were no
// failures at all.
func aggregateFailureClass(seenUnreachable, seenUnavailable bool) string {
	switch {
	case seenUnreachable && seenUnavailable:
		return "mixed"
	case seenUnreachable:
		return ClassUnreachable
	case seenUnavailable:
		return ClassUnavailable
	default:
		return ""
	}
}

// ParseDiscoveryLocation extracts a relay's dial {host, port} from a discovery
// response location URL (PLY-022, e.g. "https://198.51.100.45:5173/player/v1").
// It returns false for a location it cannot resolve to a host — a malformed
// announcement the player ignores rather than relocating to.
func ParseDiscoveryLocation(location string) (RelayAddress, bool) {
	u, err := url.Parse(location)
	if err != nil {
		return RelayAddress{}, false
	}
	host := u.Hostname()
	if host == "" {
		return RelayAddress{}, false
	}
	addr := RelayAddress{Host: host}
	if portStr := u.Port(); portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil {
			return RelayAddress{}, false
		}
		addr.Port = p
	}
	return addr, true
}

// Relocate points a paired Session at the relay's re-announced new address
// (PLY-022/132/133) and resumes — WITHOUT re-pairing and WITHOUT touching the
// channel token or screen binding (never-wipe, PLY-026/130). It re-pins against
// the relay at its new address, which — a moved relay keeping its identity —
// MUST present the SAME committed certificate (PLY-060); a certificate that no
// longer matches the pin fails here and folds back into the retry-forever
// failure space (PLY-138), never a silent relocate to an impostor. On success
// the session's dial address and pinned transport are updated in place and the
// retained token is ready for the next PullProgram.
func (s *Session) Relocate(disc DiscoveryResponse) error {
	addr, ok := ParseDiscoveryLocation(disc.Location)
	if !ok {
		return fmt.Errorf("virtualplayer: Relocate: unresolvable discovery location %q", disc.Location)
	}
	dial := net.JoinHostPort(addr.Host, strconv.Itoa(addr.Port))

	// Re-pin against the moved relay: it must still present the committed cert.
	certDER, err := bootstrapFetchAndPin(dial, s.commitment)
	if err != nil {
		return fmt.Errorf("virtualplayer: Relocate: %w", err)
	}
	relayPub, err := publicKeyFromCertDER(certDER)
	if err != nil {
		return fmt.Errorf("virtualplayer: Relocate: relay certificate public key: %w", err)
	}

	// never-wipe (PLY-026/130): update the address + pinned transport only;
	// retain the channel token and screen binding untouched.
	s.RelayAddress = addr
	s.base = "https://" + dial
	s.client = pinnedClient(s.commitment)
	s.relayPub = relayPub
	return nil
}
