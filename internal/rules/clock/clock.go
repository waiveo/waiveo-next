// Package clock provides the injectable time source every edge-rules
// evaluation component reads through instead of calling time.Now() directly.
//
// Every duration mechanism this contract's vocabulary defines — a state or
// numeric trigger's `for` hold (RUL-024/026/033), a `delay` action's wait
// (RUL-190), and the (later-part) stabilization window (RUL-301) — MUST be
// evaluated on MONOTONIC time only, never wall-clock (RUL-024/361): a
// wall-clock adjustment (an NTP step, a DST transition, a manual clock
// change) MUST NOT be able to shorten or extend a hold in progress. Wall-clock
// time is used only where the contract itself calls for a local wall-clock
// instant — a `time`/`time_pattern`/`sun` trigger or a `time` condition's
// window (RUL-130/340, Part 3) — never for a duration.
//
// FakeClock is the test double every eval/engine package (Tasks 2-10) drives
// directly: the rules-1 corpus's own conformance notes call for exercising
// timing-dependent behavior "against an injectable/fake clock in the driver
// harness, not wall-clock sleeps in a static corpus."
package clock

// Clock is the time source every edge-rules component reads through.
type Clock interface {
	// WallMillis returns the current wall-clock instant, as Unix
	// milliseconds. Used only for time/sun triggers and conditions
	// (RUL-130/340, Part 3) — never to measure a duration.
	WallMillis() int64
	// Mono returns the current monotonic instant, in milliseconds. Only the
	// difference between two Mono() reads is meaningful — that delta is the
	// elapsed monotonic time between them (RUL-024/026/190/360/361). Mono
	// never decreases.
	Mono() int64
}

// FakeClock is a Clock test double holding two independently-settable
// counters: a monotonic reading advanced only forward via Advance, and a
// wall-clock reading assigned directly via SetWall. The two never interact —
// setting one never perturbs the other — so tests can exercise a wall-clock
// jump (or divergence from monotonic time) without disturbing a duration
// computation under test, and vice versa.
type FakeClock struct {
	mono int64
	wall int64
}

// NewFakeClock returns a FakeClock with both readings starting at zero.
func NewFakeClock() *FakeClock {
	return &FakeClock{}
}

// Advance moves the monotonic reading forward by ms milliseconds. ms MUST
// NOT be negative — monotonic time never runs backward — and Advance panics
// if it is. ms == 0 is a valid no-op (RUL-024: for:0 behaves as if for were
// absent).
func (c *FakeClock) Advance(ms int64) {
	if ms < 0 {
		panic("clock: FakeClock.Advance: negative duration")
	}
	c.mono += ms
}

// SetWall assigns the wall-clock reading directly, independent of Mono.
func (c *FakeClock) SetWall(ms int64) {
	c.wall = ms
}

// Mono implements Clock.
func (c *FakeClock) Mono() int64 { return c.mono }

// WallMillis implements Clock.
func (c *FakeClock) WallMillis() int64 { return c.wall }
