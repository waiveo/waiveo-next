package wire

// screencadence.go declares — in ONE place — the three timings that decide
// whether a screen reads `live` or `stale`, plus the window derived from them.
//
// # The defect this file exists to make impossible
//
// The live/stale window was originally a hand-picked 45 000 ms, justified in a
// comment as "four missed 10-second polls plus slack" against PLY-082's
// draft-note poll cadence. That derivation was written down in prose and then
// stopped being true: a real player's pull-to-pull cadence in the field is not
// its nominal WAIT — it is the wait plus everything one iteration does before
// it waits again (a TLS handshake to the relay, the program pull, and the
// download-and-hash of any content that changed). Measured on real hardware,
// a HEALTHY screen's `last_pull_age_ms` is a clean sawtooth climbing to ~57 600
// ms and resetting to ~2 500 ms. Against a 45 000 ms line that screen reads
// `stale` for roughly a quarter of every cycle, so an operator watching the
// fleet page sees a healthy wall flap — which destroys the column's credibility
// exactly as thoroughly as reporting nothing would.
//
// The fix is not a bigger constant. A bigger constant is the same defect with a
// later expiry date, because the number and the cadence it is supposed to
// exceed lived in different files with only a comment joining them. So the
// cadences are declared HERE, together, the window is COMPUTED from them, and
// the relationship between them is asserted by tests in this package
// (screencadence_test.go) that fail if any one of the four is changed without
// the others being reconsidered:
//
//   - the derivation itself (the window IS a whole multiple of the cadence);
//   - the operating margin (the window survives a whole missed pull cycle plus
//     a whole late report, and still has slack);
//   - the mirror (ProgramPollIntervalMs still equals what the shipped
//     BrightScript player actually returns — that one reads player-v3's source).
//
// # Why this package
//
// These are properties of the `screen.status` frame's timing, and this is the
// package both ends of that frame already import: cmd/waiveo-relay drives its
// report ticker from ScreenStatusReportIntervalMs, internal/app/screens draws
// its threshold from ScreenLiveWindowMs. The relay tree deliberately imports no
// internal/app package (a real layering boundary — check the relay's imports),
// so a constant both sides must agree on cannot live on either side.

// ProgramPollIntervalMs is the player's nominal WAIT between two consecutive
// program pulls: the value player-v3's `wvProgramPollIntervalMs()` returns,
// mirrored into Go because nothing in Go can import BrightScript.
//
// It is a mirror and not a source, and it is checked as one:
// TestProgramPollIntervalMirrorsTheShippedPlayer reads
// player-v3/components/PlayerTask.brs and fails if the two stop agreeing. That
// test is the only thing standing between "somebody retunes the player" and
// "the console's staleness line silently keeps a derivation that no longer
// describes any screen".
const ProgramPollIntervalMs int64 = 10_000

// ObservedProgramPullCadenceMs is the pull-to-pull cadence a real screen
// actually achieves — ProgramPollIntervalMs plus one iteration's work — and it
// is the number the live window is derived from, because it is the one that
// describes how old a HEALTHY screen's last pull can get.
//
// It is a MEASUREMENT, not a target: 60 000 ms, rounded up from the ~55 100 ms
// sawtooth period observed on real hardware (age climbing to ~57 600 ms and
// resetting to ~2 500 ms). Rounded UP rather than to the nearest, because every
// consumer of this number is asking "how long may a healthy screen stay quiet",
// and under-stating that is what produced the flapping in the first place.
//
// If the player is ever retuned, this is the number to re-measure. The tests
// enforce that it cannot fall below the nominal wait, which is the one thing
// that is true by construction.
const ObservedProgramPullCadenceMs int64 = 60_000

// ScreenStatusReportIntervalMs is how often a connected relay re-reports its
// full per-screen observation set upward. cmd/waiveo-relay's report ticker is
// built from this value rather than from a constant of its own, so the app
// peer's staleness arithmetic and the relay's reporting rate cannot drift.
//
// It matters to the window because the app peer's `last_pull_age_ms` is the age
// the RELAY measured plus however long the report has been sitting here: a
// perfectly healthy screen can therefore present an age of one whole pull
// cadence plus one whole report interval, purely from the two clocks being out
// of phase.
const ScreenStatusReportIntervalMs int64 = 10_000

// ScreenLiveWindowCadenceMultiple is how many whole observed pull cadences the
// live window spans. Three — so a screen stays `live` through one completely
// missed pull and the late report that follows it, and only a screen that has
// missed two goes `stale`.
//
// The asymmetry that sets this number: calling a healthy screen stale costs an
// operator a wasted investigation and, repeated, their trust in the column.
// Calling a just-failed screen live costs them learning about it up to three
// minutes later, on a wall nobody was watching in the meantime. Those are not
// the same size, and the raw ages ride every response so a console that wants a
// tighter line can draw one.
const ScreenLiveWindowCadenceMultiple int64 = 3

// ScreenLiveWindowMs is the live/stale threshold applied to `last_pull_age_ms`,
// COMPUTED from the cadence above rather than written down beside it. Changing
// the cadence moves the window with it; that is the entire point.
const ScreenLiveWindowMs int64 = ObservedProgramPullCadenceMs * ScreenLiveWindowCadenceMultiple
