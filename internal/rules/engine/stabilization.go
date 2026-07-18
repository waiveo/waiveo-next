package engine

import "time"

// Stabilization window (RUL-301).
//
// A state or numeric trigger MUST NOT dispatch its rule's actions until a
// bounded stabilization window has elapsed after the engine reaches a defined
// readiness point — an engine restart, an entity enrollment, or a
// generation-apply that makes the trigger new or changed. The window is applied
// per qualifying trigger at (trigger, entity) granularity (RUL-304): each
// trigger a readiness point makes newly eligible serves its OWN window from that
// point, not one engine-wide gate deferring every trigger against a single
// shared signal. The window is measured on the engine's monotonic clock so a
// wall-clock adjustment can neither shorten nor extend it (RUL-024/301), and it
// is fallback-timed: a maximum monotonic wait applies even if the readiness
// signal the window keys to never fully arrives, so the gate cannot wedge open
// (releaseStabilized fires the held transition once the deadline passes).
//
// Readiness points modeled here: a fresh engine applying its first generation
// (an engine restart) and a generation-apply that builds a new/changed trigger
// both arm through armStabilization, called on every freshly-built rule instance
// in ApplyGeneration. A trigger carried forward unchanged across a generation
// swap is NOT re-armed (RUL-303) — it keeps whatever window state it already had
// (typically elapsed), so a routine edit elsewhere in the generation never
// re-gates it.
//
// The gate is off by default. New leaves stabWindowMs at zero so a caller that
// has not opted in — every Part 1-3 test and the Load compatibility path — is
// never deferred; the relay opts in by calling SetStabilizationWindow. The
// contract fixes only the mechanism (bounded, config-visible, fallback-timed);
// concrete duration defaults are proposed for review separately (RUL-301
// draft-note), so DefaultStabilizationWindow is the documented recommended value
// a caller may pass, not a figure the engine silently imposes.

// DefaultStabilizationWindow is the recommended small bounded stabilization
// window (RUL-301) a caller passes to SetStabilizationWindow when it has no
// operator-configured value of its own. It is deliberately conservative: long
// enough to absorb a burst of post-readiness reconfirmations, short enough not
// to noticeably delay a genuine transition. The contract does not fix a binding
// default (RUL-301 draft-note), so the engine imposes none — this constant only
// names a sensible starting point for the relay's own config to reference.
const DefaultStabilizationWindow = 500 * time.Millisecond

// SetStabilizationWindow configures the stabilization window (RUL-301): the
// bounded monotonic span each newly-eligible state/numeric trigger defers its
// first dispatch through. A non-positive d disables the gate (the effective
// default); DefaultStabilizationWindow is the recommended value when a caller
// wants the gate on without an operator-configured figure. It takes effect for
// triggers armed by subsequent readiness points (a later generation-apply); a
// window already open keeps the deadline it was armed with.
func (e *Engine) SetStabilizationWindow(d time.Duration) {
	if d <= 0 {
		e.stabWindowMs = 0
		return
	}
	e.stabWindowMs = d.Milliseconds()
}

// armStabilization opens a stabilization window (RUL-301) for every state/numeric
// trigger of a freshly-built rule instance — the triggers a readiness point
// (generation-apply making them new/changed, or a fresh engine's first apply,
// i.e. an engine restart) made newly eligible. Each trigger's window runs from
// the current monotonic instant to that instant plus the configured span, so
// every trigger serves its own window (RUL-304), not one shared gate. It is a
// no-op when the gate is off (stabWindowMs == 0), so an un-opted-in caller is
// never deferred. Schedule triggers (time/time_pattern/sun) are wall-clock and
// carry no stabilization window. ApplyGeneration calls this before
// carryUnchangedTriggers so a trigger carried forward unchanged overwrites its
// freshly-armed slot and is thereby NOT re-gated (RUL-303).
func (e *Engine) armStabilization(ri *ruleInstance) {
	if e.stabWindowMs <= 0 {
		return
	}
	deadline := e.clk.Mono() + e.stabWindowMs
	for _, tr := range ri.triggers {
		if tr.kind != "state" && tr.kind != "numeric" {
			continue
		}
		tr.stabArmed = true
		tr.stabDeadline = deadline
	}
}

// dispatchFire resolves a state/numeric trigger firing through the stabilization
// gate (RUL-301): while the trigger's window is open the firing is held pending
// (stabilizationHeld) and produces no disposition now — it dispatches when
// releaseStabilized opens the gate. An unarmed trigger, or one whose window has
// elapsed, fires immediately through fireRule. Every state/numeric fire site
// (Observe and the Tick `for`-hold advance) routes through here.
func (e *Engine) dispatchFire(ri *ruleInstance, tr *triggerRuntime) []RunDisposition {
	if e.stabilizationHeld(tr) {
		return nil
	}
	// tr.entityID is the firing trigger's subject entity — the edge Expression
	// scope for this firing's log/params (RUL-282).
	return e.fireRule(ri, tr.entityID)
}

// stabilizationHeld reports whether a firing of tr must be held for its
// stabilization window (RUL-301), recording the held firing on the trigger when
// so. An unarmed trigger is never held. An armed trigger whose monotonic
// deadline has already passed is disarmed here (the gate opens permanently for
// it) and not held — this lazily opens the gate on an Observe that arrives after
// the deadline with no intervening Tick. Otherwise the trigger is still inside
// its window: the firing is marked pending and held.
func (e *Engine) stabilizationHeld(tr *triggerRuntime) bool {
	if !tr.stabArmed {
		return false
	}
	if e.clk.Mono() >= tr.stabDeadline {
		tr.stabArmed = false
		return false
	}
	tr.stabPending = true
	return true
}

// releaseStabilized opens the stabilization gate for every trigger whose window
// has now elapsed (RUL-301), dispatching a held transition once — this is the
// bounded fallback that fires even if no further readiness signal arrives, so the
// gate cannot wedge open. It disarms each elapsed trigger (the gate opens
// permanently for it) and, for one that held a firing, releases that
// already-matched firing through fireRule — it is NOT re-evaluated against a
// later observation. Tick calls this each tick after advancing holds.
func (e *Engine) releaseStabilized() []RunDisposition {
	var out []RunDisposition
	for _, ri := range e.rules {
		for _, tr := range ri.triggers {
			if !tr.stabArmed || e.clk.Mono() < tr.stabDeadline {
				continue
			}
			tr.stabArmed = false
			if tr.stabPending {
				tr.stabPending = false
				// The held firing's subject entity scopes its Expressions (RUL-282).
				out = append(out, e.fireRule(ri, tr.entityID)...)
			}
		}
	}
	return out
}
