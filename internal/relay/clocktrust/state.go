package clocktrust

import (
	"fmt"
	"sync"
)

// TrustState is the relay's clock-trust state carried in hello.clock_state
// (relay/1 REL-038): "untrusted" or "trusted" (Clock trust). It is the state a
// time-based evaluation reads to decide whether to floor the wall reading to the
// persisted floor (REL-131) or trust the live clock.
type TrustState string

const (
	// StateUntrusted is the relay's clock state before any independently
	// verified time has been applied (REL-131): the wall reading is unattested,
	// so time-based evaluation uses the persisted floor and connect-time
	// peer-cert temporal validation defers (REL-136).
	StateUntrusted TrustState = "untrusted"
	// StateTrusted is the relay's clock state once a verified time (REL-132) has
	// been applied: the live clock may be trusted for time-based evaluation.
	StateTrusted TrustState = "trusted"
)

// FloorStore is the narrow slice of internal/relay/identity.Store the clock-trust
// controller advances its persisted verified floor against (REL-130/132).
// *identity.Store satisfies it; a fake satisfies it in tests. SetClockFloor is
// monotonic at the store — it refuses a non-advancing value and reports
// advanced=false — so the controller need not re-check the current floor.
type FloorStore interface {
	ClockFloor() (ms int64, ok bool, err error)
	SetClockFloor(ms int64) (advanced bool, err error)
}

// EngineTrustHook is the rules-engine re-evaluation hook the untrusted->trusted
// transition drives (REL-134 -> rules/1 RUL-371): the controller calls it with
// "trusted" on the transition, and the engine immediately re-evaluates every
// time-based trigger and condition against the newly trusted time rather than
// waiting for the next natural tick. In the live relay it adapts
// internal/rules/engine.Engine.SetClockTrust; a nil hook is a no-op, so a relay
// that tracks its clock-trust state before an engine is wired still works.
type EngineTrustHook func(state string) error

// Controller is the relay's connection-layer clock-trust state machine
// (relay/1 REL-132/134/136, clock_state REL-038). It owns three decoupled
// facts:
//
//   - the current TrustState (untrusted/trusted), the clock_state accessor
//     hello reports (REL-038) and the connect-time peer-cert validation reads
//     as its clockTrusted input (REL-136);
//   - the persisted verified floor it advances ONLY from a verified time
//     (ApplyVerifiedTime, REL-132), which is also the ONLY path to a trusted
//     clock — and the untrusted->trusted edge that fires the engine re-eval
//     (REL-134);
//   - a runtime clock a clock.hint adjusts (AcceptHint), which by construction
//     never touches the floor and never changes TrustState (REL-132): a hint is
//     advisory.
//
// Every boot starts untrusted (REL-131): a persisted floor is a past observed
// time, not a currently verified wall reading, so trust is re-earned each boot
// from a fresh verified time. Trust is a pure function of whether a verified
// time has been applied — independent of certificate validity, which is what
// keeps expired-certificate re-enrollment unblocked by a stale clock
// (REL-023/135). Safe for concurrent use.
type Controller struct {
	floor FloorStore
	clock *RuntimeClock
	hook  EngineTrustHook

	mu     sync.Mutex
	state  TrustState
	source string
}

// NewController returns an untrusted controller (REL-131) backed by floor (the
// persisted verified floor, REL-130/132), clock (the hint-adjustable runtime
// clock, REL-133), and hook (the engine re-eval driven on the untrusted->trusted
// transition, REL-134 — nil for a relay with no engine attached yet). The
// initial clock_state source is "cold_boot" until a verified time is applied.
func NewController(floor FloorStore, clock *RuntimeClock, hook EngineTrustHook) *Controller {
	return &Controller{
		floor:  floor,
		clock:  clock,
		hook:   hook,
		state:  StateUntrusted,
		source: "cold_boot",
	}
}

// State reports the relay's current clock-trust state (REL-038) — the value
// hello's clock_state.state carries.
func (c *Controller) State() TrustState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Trusted reports whether the relay's clock is currently trusted — the
// clockTrusted input the connect-time peer-cert validation reads
// (ValidatePeerCert/AppPeerTLSConfig, REL-136): while false, temporal
// validation of the app peer's certificate is deferred and identity rests on
// the enrollment-anchored key pin alone (REL-137).
func (c *Controller) Trusted() bool {
	return c.State() == StateTrusted
}

// ClockStateReport returns the {state, source} pair hello.clock_state carries
// (REL-038). source is an implementation-defined short string naming the
// relay's current time source; the app peer treats an unrecognized value as
// forward-compatible data, never a validation failure (REL-038).
func (c *Controller) ClockStateReport() (state string, source string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.state), c.source
}

// Clock returns the controller's hint-adjustable runtime clock so the wire
// clock.hint receiver (HintReceiver) and any runtime-wall reader share the one
// clock the controller's AcceptHint adjusts.
func (c *Controller) Clock() *RuntimeClock {
	return c.clock
}

// ApplyVerifiedTime records a time value the relay has verified INDEPENDENTLY of
// any unauthenticated claim (REL-132 — a timestamp signed under the
// desired-state verification key, or an external time source whose own
// authentication the relay can verify, e.g. authenticated NTP). It advances the
// persisted clock floor monotonically (SetClockFloor refuses a non-advancing
// value, REL-130) and, on the FIRST such application from the untrusted state,
// transitions clock_state to trusted and fires the engine re-evaluation hook
// exactly once (REL-134 -> RUL-371: re-evaluate every time-based trigger and
// condition immediately, not at the next natural tick).
//
// A clock.hint NEVER reaches this path (REL-132): it carries no verifiable
// signature, so it may adjust only the runtime clock (AcceptHint), never the
// floor and never the trust state. Returns whether the floor advanced and
// whether this call transitioned the state untrusted->trusted.
func (c *Controller) ApplyVerifiedTime(verifiedMs int64) (floorAdvanced bool, transitioned bool, err error) {
	advanced, ferr := c.floor.SetClockFloor(verifiedMs)
	if ferr != nil {
		return false, false, fmt.Errorf("clocktrust: advance verified floor: %w", ferr)
	}

	c.mu.Lock()
	transitioned = c.state == StateUntrusted
	c.state = StateTrusted
	c.source = "verified"
	c.mu.Unlock()

	if transitioned && c.hook != nil {
		// REL-134: the untrusted->trusted edge re-evaluates time-based rules
		// immediately (RUL-371). The transition is a fact of verified time; if
		// the engine re-eval itself errors, report it but keep transitioned=true
		// so the caller sees the clock did become trusted.
		if herr := c.hook(string(StateTrusted)); herr != nil {
			return advanced, true, fmt.Errorf("clocktrust: engine re-eval on untrusted->trusted transition (REL-134): %w", herr)
		}
	}
	return advanced, transitioned, nil
}

// AcceptHint applies a clock.hint to the relay's RUNTIME clock only (REL-133),
// bounded by the relay's own certificate not_after plus boundedGraceMs so an app
// peer cannot use a hint alone to make the relay believe its own expired
// credential is still valid. It NEVER advances the persisted floor and NEVER
// changes clock_state (REL-132): a hint is advisory, so an untrusted relay stays
// untrusted after any number of hints. nowMonoMs is the monotonic instant the
// hint is anchored against. Returns whether the hint was accepted.
func (c *Controller) AcceptHint(hint ClockHint, certNotAfterMs, boundedGraceMs, nowMonoMs int64) (accepted bool) {
	return c.clock.AcceptHint(hint, certNotAfterMs, boundedGraceMs, nowMonoMs)
}
