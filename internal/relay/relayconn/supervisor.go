// supervisor.go is the relay-side reconnect supervisor: the owner of the
// persistent connection's LIFECYCLE, where Client owns exactly one live
// connection. It dials, hands the connected client to the owner (the
// pull-on-reconnect seam), waits for the connection to die, and re-dials
// under exponential backoff with jitter — forever, for every failure the
// relay/1 Error taxonomy marks recoverable-by-retry, and never for one it
// marks as needing an operator (the classification split
// cmd/waiveo-relay's hellorecovery established for the HTTP-era handshake,
// carried onto this transport's own *Refusal type):
//
//   - A transport-level failure (no typed refusal at all — the app peer
//     unreachable, mid-restart, a dropped socket) always retries.
//   - CHANNEL_BINDING_INVALID and RELAY_IDENTITY_MISMATCH are the two
//     refusals the taxonomy itself annotates "reconnect and retry the
//     handshake" — CHANNEL_BINDING_INVALID in particular is exactly what a
//     feeder restart's enrollment-registry amnesia produces, so retrying
//     until the peer-side condition clears is the posture that keeps a
//     relay from degrading permanently after a feeder restart (the field
//     defect hellorecovery closed).
//   - Any other typed refusal (PROTOCOL_VERSION_UNSUPPORTED, CERT_REVOKED,
//     …) is permanent: retrying an unchanged software pairing or a revoked
//     credential repeats the identical refusal forever — supervision ends
//     and the refusal is surfaced for an operator.
package relayconn

import (
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

// Default backoff bounds for the redial loop (SupervisorConfig overrides).
const (
	defaultInitialBackoff = 500 * time.Millisecond
	defaultMaxBackoff     = 30 * time.Second
)

// defaultRenewalCheckInterval is the mid-session cadence the proactive
// certificate-renewal predicate (SupervisorConfig.NeedsRenewal) is
// re-evaluated on while a connection is up. The renewal window is measured
// in days (REL-015's draft-note proposes 30), so an hourly check leaves
// hundreds of attempts before hard expiry while adding no measurable load.
const defaultRenewalCheckInterval = time.Hour

// RefusalIsRecoverable reports whether a Connect failure is one the Error
// taxonomy directs a relay to recover from by reconnecting and retrying
// (package doc above) — as opposed to one that needs an operator's
// intervention and so ends supervision.
func RefusalIsRecoverable(err error) bool {
	if err == nil {
		return false
	}
	var refused *Refusal
	if !errors.As(err, &refused) {
		return true // transport-level failure: always worth retrying
	}
	switch refused.Code {
	case "CHANNEL_BINDING_INVALID", "RELAY_IDENTITY_MISMATCH":
		return true
	default:
		return false
	}
}

// SupervisorConfig configures StartSupervisor.
type SupervisorConfig struct {
	// Connect performs one connection attempt — typically a closure over
	// Dial(Config{…}). Required.
	Connect func() (*Client, error)

	// OnConnected, when non-nil, runs on the supervisor goroutine after
	// every successful connection, before the supervisor waits on the
	// connection's death. This is the pull-on-reconnect seam: the owner
	// pulls desired state (since its persisted last-applied generation),
	// verifies+applies, and acks — so a relay that was offline through N
	// generations converges on reconnect without waiting for a nudge.
	OnConnected func(*Client)

	// OnPermanentRefusal, when non-nil, receives the typed refusal that
	// ended supervision (RefusalIsRecoverable=false) — the operator
	// surface. Done() is closed after it returns.
	OnPermanentRefusal func(*Refusal)

	// OnDisconnected, when non-nil, runs on the supervisor goroutine the
	// moment a live connection dies, BEFORE any redial: err is the
	// connection's own Err() (which half of the transport noticed, and
	// why) and connectedFor is how long it had been up.
	//
	// It exists because until HV-22 this loop was mute. A relay that lost
	// its app peer said nothing, redialled silently, was refused silently,
	// and repeated that for two and a half hours while its fleet went
	// dark — and the only evidence anywhere was write errors from a
	// different goroutine, aimed at a socket this loop had already
	// abandoned. Every state change this loop makes now has a seam an
	// owner can report from.
	//
	// The owner's FIRST obligation here is to stop using the dead client.
	// The supervisor drops its own reference (Client() goes nil), but a
	// binary that cached the connected client elsewhere holds a corpse
	// until the next OnConnected replaces it, which never comes while
	// redials keep failing.
	//
	// It does NOT run on Stop: an orderly shutdown is not a connection
	// loss, and reporting one would train an operator to ignore the line
	// that matters.
	OnDisconnected func(err error, connectedFor time.Duration)

	// OnConnectFailed, when non-nil, runs on the supervisor goroutine
	// after every failed connection attempt that will be retried:
	// consecutive counts this attempt and every failed one before it since
	// the last success (1 on the first), and retryIn is the jittered delay
	// before the next try.
	//
	// consecutive is passed rather than left for the owner to count
	// because the owner's real problem is VOLUME, not absence: an attempt
	// every 30s for an outage measured in hours is thousands of identical
	// lines, and a log that repeats itself is a log nobody reads. The
	// count is what lets an owner report the first failure in full and
	// then thin out, while still naming how long this has been going on.
	//
	// A refusal the taxonomy marks permanent does NOT come through here —
	// that ends supervision and goes to OnPermanentRefusal.
	OnConnectFailed func(err error, consecutive int, retryIn time.Duration)

	// InitialBackoff/MaxBackoff bound the exponential redial backoff
	// (doubled per consecutive failed attempt, jittered to [0.5x, 1.5x) so
	// a fleet of relays does not re-dial in lockstep after an app-peer
	// restart). Zero values take the defaults.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration

	// NeedsRenewal and Renew, when BOTH are non-nil, drive proactive
	// certificate renewal (REL-015). NeedsRenewal — the predicate deciding
	// whether the persisted leaf is inside its renewal window (or already
	// past expiry) — is evaluated at the top of every loop iteration, BEFORE
	// the connection attempt, and on RenewalCheckInterval while connected.
	// When it reports true, Renew performs ONE renewal attempt.
	//
	// A successful renewal changes only the persisted identity: per
	// REL-015's cutover sentence the fresh leaf takes effect at the next
	// connection attempt (Connect's Dial re-reads the store), and the
	// supervisor NEVER tears down a live connection over it — the
	// connection continues under the certificate it authenticated with. A
	// failed renewal is swallowed and retried on the next evaluation (the
	// window is deliberately wide; the owner's Renew closure is where a
	// failure gets logged), and never blocks dialing: the current leaf may
	// still be perfectly valid.
	//
	// Belt-and-suspenders: when a connection attempt fails with the TLS
	// stack's expired-leaf handshake refusal (a transport-level error, not
	// a typed *Refusal — see renewOnExpiredLeafHandshake), Renew is
	// triggered even if NeedsRenewal said false, covering the clock-skew
	// case where the relay's own clock believes an expired leaf is valid.
	NeedsRenewal func() bool
	Renew        func() error

	// RenewalCheckInterval overrides the mid-session cadence NeedsRenewal
	// is re-evaluated on while connected (defaultRenewalCheckInterval when
	// zero). Pre-dial evaluations are not affected — they ride every loop
	// iteration regardless.
	RenewalCheckInterval time.Duration

	// MinStableUptime is the minimum time a connection must survive for its
	// death to be treated as an isolated drop (redial immediately, ladder
	// reset) rather than as one more failed attempt (ladder engages). This
	// is the guard against an accept-then-drop peer: one that completes the
	// handshake and then kills every connection would otherwise never trip
	// the failure ladder at all — Connect keeps "succeeding" — and the loop
	// degenerates into a full-rate dial storm (TLS handshake + ed25519
	// channel-binding sign/verify + WS upgrade per iteration) that pins CPU
	// on BOTH peers of a Pi-class appliance. A zero value defaults to
	// MaxBackoff, which bounds the steady-flap dial rate at roughly one
	// attempt per MaxBackoff regardless of how the peer misbehaves.
	MinStableUptime time.Duration
}

// Supervisor keeps one relay persistently connected (package doc). Start
// with StartSupervisor; end with Stop.
type Supervisor struct {
	cfg     SupervisorConfig
	stop    chan struct{}
	done    chan struct{}
	stopOne sync.Once

	mu     sync.Mutex
	client *Client
}

// StartSupervisor starts the supervision loop on its own goroutine.
func StartSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.InitialBackoff == 0 {
		cfg.InitialBackoff = defaultInitialBackoff
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	if cfg.MinStableUptime == 0 {
		cfg.MinStableUptime = cfg.MaxBackoff
	}
	if cfg.RenewalCheckInterval == 0 {
		cfg.RenewalCheckInterval = defaultRenewalCheckInterval
	}
	s := &Supervisor{
		cfg:  cfg,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go s.run()
	return s
}

// Client returns the currently connected client, or nil while disconnected
// (dialing/backing off).
func (s *Supervisor) Client() *Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// Stop ends supervision: the current connection (if any) is closed and the
// loop exits. Safe to call more than once; returns without waiting — use
// Done to wait.
func (s *Supervisor) Stop() {
	s.stopOne.Do(func() { close(s.stop) })
}

// Done is closed when supervision has fully ended — after Stop, or after a
// permanent (non-recoverable) refusal.
func (s *Supervisor) Done() <-chan struct{} { return s.done }

func (s *Supervisor) run() {
	defer close(s.done)
	backoff := s.cfg.InitialBackoff
	// Consecutive failed Connect attempts since the last success — the
	// number OnConnectFailed hands the owner so it can thin a report that
	// would otherwise repeat every backoff interval for the length of the
	// outage (OnConnectFailed's doc).
	consecutive := 0
	for {
		select {
		case <-s.stop:
			return
		default:
		}

		// Proactive renewal rides the loop's own cadence, ahead of the dial
		// (SupervisorConfig.NeedsRenewal): a leaf inside its renewal window
		// — or already expired — is renewed FIRST, so the dial below
		// presents the fresh leaf deterministically, never depending on the
		// peer's handshake error to notice expiry. Retry pacing is the
		// loop's own: the backoff ladder while disconnected, the renewal
		// ticker while connected — never a hot loop against the feeder's
		// re-enroll rate limit (REL-025).
		s.maybeRenew()

		client, err := s.cfg.Connect()
		if err != nil {
			if !RefusalIsRecoverable(err) {
				var refused *Refusal
				errors.As(err, &refused)
				if s.cfg.OnPermanentRefusal != nil {
					s.cfg.OnPermanentRefusal(refused)
				}
				return
			}
			s.renewOnExpiredLeafHandshake(err)
			consecutive++
			delay := jitter(backoff)
			if s.cfg.OnConnectFailed != nil {
				s.cfg.OnConnectFailed(err, consecutive, delay)
			}
			if !s.sleep(delay) {
				return
			}
			backoff = min(backoff*2, s.cfg.MaxBackoff)
			continue
		}

		consecutive = 0
		connectedAt := time.Now()
		s.mu.Lock()
		s.client = client
		s.mu.Unlock()

		if s.cfg.OnConnected != nil {
			s.cfg.OnConnected(client)
		}

		// While connected, the proactive-renewal predicate keeps riding a
		// slow ticker (RenewalCheckInterval): a relay that stays connected
		// for months — the healthy steady state — must still renew ahead of
		// expiry, and a successful mid-session renewal deliberately does
		// NOT touch the live connection (REL-015's cutover sentence): the
		// fresh leaf simply rides the next redial, whenever this connection
		// naturally dies.
		var renewTicker *time.Ticker
		var renewTick <-chan time.Time
		if s.cfg.NeedsRenewal != nil && s.cfg.Renew != nil {
			renewTicker = time.NewTicker(s.cfg.RenewalCheckInterval)
			renewTick = renewTicker.C
		}

	connected:
		for {
			select {
			case <-s.stop:
				if renewTicker != nil {
					renewTicker.Stop()
				}
				_ = client.Close()
				s.clearClient()
				return
			case <-renewTick:
				s.maybeRenew()
			case <-client.Done():
				s.clearClient()
				// Report the death BEFORE the loop starts redialling, and
				// before anything sleeps: the owner's first job is to drop
				// its own reference to this client, and every millisecond
				// it holds one is a millisecond it can still write to a
				// dead socket (OnDisconnected's doc).
				if s.cfg.OnDisconnected != nil {
					s.cfg.OnDisconnected(client.Err(), time.Since(connectedAt))
				}
				break connected
			}
		}
		if renewTicker != nil {
			renewTicker.Stop() // per-connection ticker: never outlive the connection it paced
		}
		if time.Since(connectedAt) >= s.cfg.MinStableUptime {
			// The connection proved stable before dying (an isolated
			// drop, an app-peer restart): reset the ladder and re-dial
			// immediately — backoff exists to pace FAILURES, not to
			// delay recovery from a one-off death.
			backoff = s.cfg.InitialBackoff
			continue
		}
		// The connection died almost as soon as it was accepted. A peer
		// that completes the handshake and then drops every connection
		// is a failing peer in all but name — count this attempt against
		// the ladder exactly like a refused dial, or the loop becomes a
		// full-rate reconnect storm (MinStableUptime's doc).
		if !s.sleep(jitter(backoff)) {
			return
		}
		backoff = min(backoff*2, s.cfg.MaxBackoff)
	}
}

// maybeRenew evaluates the proactive-renewal predicate and, when due, runs
// one renewal attempt (SupervisorConfig.NeedsRenewal's doc). A Renew error
// is deliberately swallowed here — the owner's closure logs it, and the
// next evaluation (loop iteration or renewal tick) retries; a renewal
// failure must never surface as a connection failure.
func (s *Supervisor) maybeRenew() {
	if s.cfg.NeedsRenewal == nil || s.cfg.Renew == nil {
		return
	}
	if !s.cfg.NeedsRenewal() {
		return
	}
	_ = s.cfg.Renew()
}

// renewOnExpiredLeafHandshake triggers Renew when a connection attempt died
// on the peer's expired-leaf TLS refusal — the belt-and-suspenders half of
// expiry classification. The pre-dial NeedsRenewal check is the
// deterministic path; this fallback exists for clock skew, where the
// relay's own clock believes an expired leaf is still valid but the peer's
// TLS stack knows better. The refusal surfaces client-side only as an
// unexported *tls.permanentError whose string is
// "remote error: tls: expired certificate" (tls.AlertError does not match
// through errors.As — verified empirically), so a string match is the only
// classification available; it deliberately runs AFTER the typed-*Refusal
// path, so a taxonomy refusal can never reach it.
func (s *Supervisor) renewOnExpiredLeafHandshake(err error) {
	if s.cfg.Renew == nil || err == nil {
		return
	}
	var refused *Refusal
	if errors.As(err, &refused) {
		return // typed refusals are classified by the taxonomy, never renewed past
	}
	if !strings.Contains(err.Error(), "tls: expired certificate") {
		return
	}
	_ = s.cfg.Renew()
}

// sleep waits d or until Stop, reporting false when supervision should end.
func (s *Supervisor) sleep(d time.Duration) bool {
	select {
	case <-s.stop:
		return false
	case <-time.After(d):
		return true
	}
}

func (s *Supervisor) clearClient() {
	s.mu.Lock()
	s.client = nil
	s.mu.Unlock()
}

// jitter spreads d uniformly over [0.5d, 1.5d) so simultaneous losers of an
// app-peer restart do not re-dial in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d/2 + rand.N(d)
}
