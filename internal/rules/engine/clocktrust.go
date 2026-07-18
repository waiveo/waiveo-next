package engine

import (
	"errors"
	"strconv"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
)

// SetClockTrust sets the engine's clock trust state (RUL-370/371) to `trusted` or
// `untrusted` and, on an untrusted->trusted transition, immediately re-evaluates
// the untrusted window (RUL-371) — returning the dispositions of any firings that
// replay produces.
//
// While `untrusted` the wall clock is unverified, so (RUL-370) `time`,
// `time_pattern`, and `sun` triggers do not fire live (dispatchSchedule suppresses
// them and leaves the wall cursor untouched) and time-based conditions evaluate
// against the persisted floor rather than the wall (effectiveClk). Monotonic time
// is never floored, so `for`-holds and delays are unaffected (RUL-361).
//
// On the `untrusted`->`trusted` transition, every `time`/`time_pattern`/`sun`
// occurrence whose scheduled instant fell within the untrusted window is a MISSED
// occurrence, re-evaluated IMMEDIATELY (not deferred to the next natural Tick,
// RUL-371) and routed through its trigger's declared `misfire` policy — the same
// category of handling as an occurrence missed while the engine was offline, never
// a silent drop and never an ordinary live tick. The untrusted window is
// (lastTickWall, now] where `now` is the newly-trusted wall reading (the engine's
// clock) and lastTickWall is the last-evaluated trusted instant; replayMissedSchedules
// clamps the lower bound up to the persisted floor so no caught-up fire rests on a
// clock reading earlier than the floor (RUL-370), and advances the wall cursor so a
// subsequent live Tick resumes with no gap or re-enumeration.
//
// Any state other than `trusted`/`untrusted` is refused (fail-closed) and leaves
// the prior trust state unchanged. A trusted->trusted or untrusted->untrusted call
// is idempotent and replays nothing.
func (e *Engine) SetClockTrust(st string) ([]RunDisposition, error) {
	switch st {
	case "untrusted":
		e.clockUntrusted = true
		return nil, nil
	case "trusted":
		wasUntrusted := e.clockUntrusted
		e.clockUntrusted = false
		if !wasUntrusted {
			return nil, nil
		}
		// RUL-371: re-evaluate the untrusted window immediately, replaying its
		// occurrences through each trigger's misfire policy. Trust is already
		// cleared, so replayed conditions evaluate against the newly-trusted wall.
		return e.replayMissedSchedules(e.lastTickWall, e.clk.WallMillis()), nil
	default:
		return nil, errors.New("engine: unknown clock trust state " + strconv.Quote(st))
	}
}

// effectiveClk is the clock condition and action evaluation reads: the live clock
// while trusted, or a floored clock while untrusted (RUL-370) — one whose wall
// reading is pinned to the persisted floor so a time/sun condition (or a now()
// expression) never resolves against an unverified wall, while monotonic time is
// delegated unchanged so holds/delays are untouched (RUL-361). Schedule triggers
// are suppressed entirely while untrusted (dispatchSchedule), so this floor governs
// only the condition/action side.
func (e *Engine) effectiveClk() clock.Clock {
	if e.clockUntrusted {
		return flooredClock{mono: e.clk, wall: e.scheduleFloor}
	}
	return e.clk
}

// flooredClock presents the persisted trust floor as the wall reading (RUL-370)
// while delegating monotonic time to the underlying clock unchanged (RUL-361), so a
// clock-trust change can neither move nor freeze any duration-based mechanism.
type flooredClock struct {
	mono clock.Clock
	wall int64
}

func (f flooredClock) WallMillis() int64 { return f.wall }
func (f flooredClock) Mono() int64       { return f.mono.Mono() }
