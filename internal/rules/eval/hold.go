package eval

import "github.com/maaxton/waiveo-next/internal/rules/clock"

// HoldKind selects which of the two `for`-hold semantics a Hold applies
// (RUL-024 vs RUL-026). The two are complementary readings of the same
// primitive — RUL-024's "discard the pending fire and re-arm" behavior — over
// opposite notions of what breaks the hold, so a Hold carries the kind chosen
// at construction from the trigger's shape.
type HoldKind int

const (
	// BoundedHold is the `for` of a bounded trigger (RUL-023 state cases 1–3,
	// and every numeric trigger per RUL-033): the matched condition is a LEVEL
	// that must hold continuously for the declared span (RUL-024). Step's
	// rawMatched is "the level currently holds"; a fresh entry into the level
	// (a rising edge) arms the hold, the level staying satisfied continues it,
	// and the level ceasing to hold discards the pending fire.
	BoundedHold HoldKind = iota
	// DebounceHold is the `for` of an unscoped `state` trigger (RUL-023 case 4)
	// declaring `for` (RUL-026): there is no level to hold, so `for` is a
	// settle/debounce keyed to the ABSENCE of further change. Step's rawMatched
	// is "a qualifying change occurred this observation"; each change (re)arms
	// the window from that instant, and the trigger fires only once no further
	// qualifying change arrives for the full declared span.
	DebounceHold
)

// Hold wraps a trigger's raw fire signal with the RUL-024/026 `for`-hold on
// monotonic time. It is the sole normative duration-hold trigger primitive
// (RUL-310): it prevents a trigger from firing until its matched condition has
// held (bounded) or settled (debounce) for a declared span of MONOTONIC time
// (RUL-024/026/361), read only through clock.Mono() so that a wall-clock
// adjustment can neither shorten nor extend a hold in progress.
//
// Hold is not safe for concurrent use; the engine drives one Hold per
// (trigger, entity) run serially from Observe/Tick.
type Hold struct {
	kind      HoldKind
	forMillis int64 // declared `for` in monotonic milliseconds; 0 == absent

	armed     bool  // a hold is currently counting
	startMono int64 // clock.Mono() at which the current hold armed
	fired     bool  // this hold already dispatched; do not re-fire until re-armed

	// lastMatched carries, for a BoundedHold, whether the level was matched at
	// the previous Step — the rising-edge detector that distinguishes a fresh
	// entry into the level (which arms) from the level merely continuing to
	// hold (which does not re-arm). Unused for a DebounceHold.
	lastMatched bool
}

// NewHold builds a Hold of the given kind whose `for` is forSeconds seconds
// (RUL-024: a non-negative integer number of seconds; a negative value is
// clamped to 0, and 0 behaves as if `for` were absent). The hold begins
// disarmed with no level knowledge; the engine may Seed a bounded hold's
// initial level after an engine restart (RUL-360).
func NewHold(kind HoldKind, forSeconds int) *Hold {
	f := int64(forSeconds)
	if f < 0 {
		f = 0
	}
	return &Hold{kind: kind, forMillis: f * 1000}
}

// Seed sets a BoundedHold's remembered level (RUL-360): after an engine restart
// the engine re-seeds the hold with whether the durable state currently
// satisfies the target level, so that the first post-restart observation — a
// bare reconfirmation of the unchanged durable state (RUL-025/300) — is seen as
// the level continuing to hold, NOT as a fresh entry that would arm a new hold.
// A hold begins counting again only once its underlying condition FRESHLY
// re-matches after restart. Seed is a no-op's-worth of state for a DebounceHold
// (which has no level), harmless to call.
func (h *Hold) Seed(matched bool) {
	h.lastMatched = matched
}

// Reset zeroes an in-progress hold (RUL-360/361): no partial timer state
// survives an engine restart, and none is reconstructed from persisted elapsed
// time. The disarm is total — armed, the start instant, and the fired latch are
// all cleared, and the level memory is cleared so the hold carries no
// pre-restart notion of the level. The engine pairs Reset with Seed when it
// knows the durable post-restart level (see Seed). Reset never sets the
// misfire_caught marker: an interrupted in-progress hold is a distinct
// mechanism from misfire policy (RUL-362).
func (h *Hold) Reset() {
	h.armed = false
	h.startMono = 0
	h.fired = false
	h.lastMatched = false
}

// Step advances the hold by one observation/tick and reports whether the
// trigger fires now. now is read only via now.Mono() (RUL-024/361). rawMatched
// is interpreted per the hold's kind: for a BoundedHold it is "the matched
// level currently holds"; for a DebounceHold it is "a qualifying change
// occurred this observation". A hold fires at most once per continuous
// hold/settle: after it fires it stays latched until the level drops (bounded)
// or the next change re-arms it (debounce).
func (h *Hold) Step(now clock.Clock, rawMatched bool) bool {
	if h.kind == DebounceHold {
		return h.stepDebounce(now, rawMatched)
	}
	return h.stepBounded(now, rawMatched)
}

// stepBounded implements the RUL-024 bounded-level hold.
func (h *Hold) stepBounded(now clock.Clock, matched bool) bool {
	if !matched {
		// The level ceased to hold: discard any pending fire and disarm
		// (RUL-024). The next entry into the level is a fresh transition.
		h.armed = false
		h.fired = false
		h.lastMatched = false
		return false
	}

	rising := !h.lastMatched
	h.lastMatched = true
	if rising {
		// A fresh entry into the matched level arms the hold from this instant.
		h.armed = true
		h.startMono = now.Mono()
		h.fired = false
	}

	if h.armed && !h.fired && now.Mono()-h.startMono >= h.forMillis {
		h.fired = true
		return true
	}
	return false
}

// stepDebounce implements the RUL-026 unscoped-trigger settle/debounce.
func (h *Hold) stepDebounce(now clock.Clock, changed bool) bool {
	if changed {
		// A qualifying change (re)arms the settle window from this instant
		// (RUL-026). for:0 behaves as if `for` were absent: fire immediately.
		h.armed = true
		h.startMono = now.Mono()
		h.fired = false
		if h.forMillis == 0 {
			h.fired = true
			h.armed = false
			return true
		}
		return false
	}

	// A quiet observation: fire once the full span has elapsed with no further
	// qualifying change since the last one.
	if h.armed && !h.fired && now.Mono()-h.startMono >= h.forMillis {
		h.fired = true
		h.armed = false
		return true
	}
	return false
}
