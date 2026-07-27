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
	"sync"
	"time"
)

// Default backoff bounds for the redial loop (SupervisorConfig overrides).
const (
	defaultInitialBackoff = 500 * time.Millisecond
	defaultMaxBackoff     = 30 * time.Second
)

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

	// InitialBackoff/MaxBackoff bound the exponential redial backoff
	// (doubled per consecutive failed attempt, jittered to [0.5x, 1.5x) so
	// a fleet of relays does not re-dial in lockstep after an app-peer
	// restart). Zero values take the defaults.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration

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
	for {
		select {
		case <-s.stop:
			return
		default:
		}

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
			if !s.sleep(jitter(backoff)) {
				return
			}
			backoff = min(backoff*2, s.cfg.MaxBackoff)
			continue
		}

		connectedAt := time.Now()
		s.mu.Lock()
		s.client = client
		s.mu.Unlock()

		if s.cfg.OnConnected != nil {
			s.cfg.OnConnected(client)
		}

		select {
		case <-s.stop:
			_ = client.Close()
			s.clearClient()
			return
		case <-client.Done():
			s.clearClient()
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
