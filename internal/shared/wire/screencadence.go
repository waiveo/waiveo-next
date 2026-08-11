package wire

// screencadence.go declares — in ONE place — the timings that decide whether a
// screen reads `live` or `stale`, plus the window derived from them.
//
// # The defect this file exists to make impossible
//
// The live/stale window was originally a hand-picked 45 000 ms, justified in a
// comment as "four missed 10-second polls plus slack" against PLY-082's
// draft-note poll cadence. The number and the cadence it had to exceed lived in
// different files with only a comment joining them, so nothing could tell anyone
// when the justification stopped being true. Everything below exists to remove
// that gap: the player's timings are mirrored HERE, the window is COMPUTED from
// them, and tests in this package (screencadence_test.go) pin both the
// derivation and the mirrors against the shipped BrightScript.
//
// # The correction of 2026-08 — read this before touching any number here
//
// The first attempt at that fix replaced the hand-picked window with a hand-
// picked CADENCE: a constant `ObservedProgramPullCadenceMs = 60_000`, described
// in this file and in commit e1fcc7a's message as a field measurement ("a clean
// sawtooth climbing to ~57 600 ms and resetting to ~2 500 ms", measured on The
// Hanger), from which a 180 000 ms window followed. Both the constant and the
// measurement were wrong, and the way they were wrong is worth keeping:
//
//   - 60 000 ms is not this player's poll interval. It is
//     `wvProgramRetryCapMs()` — the ceiling of its FAILURE backoff. The
//     ordinary interval is `wvProgramPollIntervalMs()`, which returns 10 000.
//   - The screen the ~60 s cadence was observed on was in that backoff. It was
//     rejecting every program (a content-URL 403 defect, since fixed), so it
//     was failing and retrying at the cap. The stale readings were a symptom of
//     a broken screen, not evidence that the window was too tight.
//   - Re-measured on the same screen once the 403 was fixed,
//     `last_pull_age_ms` sawtooths between ~9 400 ms and ~19 500 ms. It never
//     approaches 45 000 ms.
//
// The cost of the error was not academic: a 180 000 ms window means a screen
// that has genuinely died reads `live` for three minutes.
//
// So this file no longer contains a measurement at all. The cadence a healthy
// screen can present is DERIVED from two timings the player's own source
// states, both mirrored under test, and the window is derived from that. A
// screen in retry backoff is outside the derivation on purpose: it is genuinely
// degraded, and reading `stale` is the correct answer for it, not a flapping
// artifact to widen a window against.
//
// # Why this package
//
// These are properties of the `screen.status` frame's timing, and this is the
// package both ends of that frame already import: cmd/waiveo-relay drives its
// report ticker from ScreenStatusReportIntervalMs, internal/app/screens draws
// its threshold from ScreenLiveWindowMs. The relay tree deliberately imports no
// internal/app package (a real layering boundary — check the relay's imports),
// so a constant both sides must agree on cannot live on either side.

// ProgramPollIntervalMs is the player's WAIT between two consecutive program
// pulls on the ordinary (successful) path: the value player-v3's
// `wvProgramPollIntervalMs()` returns, mirrored into Go because nothing in Go
// can import BrightScript.
//
// It is a mirror and not a source, and it is checked as one:
// TestProgramPollIntervalMirrorsTheShippedPlayer reads
// player-v3/components/PlayerTask.brs and fails if the two stop agreeing. That
// test is the only thing standing between "somebody retunes the player" and
// "the console's staleness line silently keeps a derivation that no longer
// describes any screen".
//
// Note what it is NOT: the wait after a FAILED pull is
// `wvProgramRetryBackoffMs()`, which doubles to a 60 000 ms cap. Mistaking that
// cap for this interval is the whole of the 2026-08 correction above.
const ProgramPollIntervalMs int64 = 10_000

// ProgramPullRequestTimeoutMs is how long the player will wait on the program
// request itself before giving up on it — the `timeoutMs` its own
// `/player/v1/program` call passes (player-v3/source/Program.brs), mirrored here
// and pinned by TestProgramPullRequestTimeoutMirrorsTheShippedPlayer.
//
// It is the second half of the derivation because it is what BOUNDS one loop
// iteration's work. A pull cannot quietly take longer than this: at this point
// the player abandons the request, counts a failure, and waits a backoff instead
// of the poll interval. So "poll interval plus request timeout" is not an
// estimate of how long an iteration takes — it is the largest value the
// iteration can have while the screen is still on the healthy path at all.
//
// Everything else a healthy iteration does is the cache-HIT path through
// `wvEnsureContent`, which is a map lookup by design: the player's own comment
// there records that re-hashing cached content "would be re-asking a question
// whose answer cannot have changed, at the cost of a full file read every ten
// seconds — which is the defect being fixed". A screen that is DOWNLOADING newly
// assigned content is a different case, and a deliberately excluded one: that
// transfer is bounded by `wvContentFetchTimeoutMs()` (120 000 ms, generous
// because a video is a whole file), no honest window can span it, and a screen
// mid-transfer of a program it has not rendered yet is a screen an operator
// should be able to see is not yet showing the new content.
const ProgramPullRequestTimeoutMs int64 = 8_000

// HealthyProgramPullCadenceMs is the largest pull-to-pull interval a screen can
// produce while still being HEALTHY — the wait, plus the longest a pull can take
// without becoming a failure.
//
// It is an upper BOUND derived from the player's own two timings, not a
// measurement of any particular screen, and that is the point: a bound cannot go
// stale the way a measurement can, and a bound is what the question "how long
// may a healthy screen stay quiet" actually wants. For scale, the field figure
// it has to cover is a real screen's ~10-11 s pull-to-pull (see the correction
// above), so this bound sits comfortably above reality without being derived
// from a number anyone has to re-measure.
const HealthyProgramPullCadenceMs int64 = ProgramPollIntervalMs + ProgramPullRequestTimeoutMs

// ScreenStatusReportIntervalMs is how often a connected relay re-reports its
// full per-screen observation set upward. cmd/waiveo-relay's report ticker is
// built from this value rather than from a constant of its own, so the app
// peer's staleness arithmetic and the relay's reporting rate cannot drift.
//
// It matters to the window because the app peer's `last_pull_age_ms` is the age
// the RELAY measured plus however long the report has been sitting here: a
// perfectly healthy screen can therefore present an age of one whole pull
// cadence plus one whole report interval, purely from the two clocks being out
// of phase. That sum is exactly the ~19.5 s peak the field measurement shows.
const ScreenStatusReportIntervalMs int64 = 10_000

// ScreenLiveWindowCadenceMultiple is how many whole healthy pull cadences the
// live window spans. Three — so a screen stays `live` through one completely
// missed pull and the late report that follows it, and only a screen that has
// missed two goes `stale`.
//
// The asymmetry that sets this number: calling a healthy screen stale costs an
// operator a wasted investigation and, repeated, their trust in the column.
// Calling a just-failed screen live costs them learning about it later. Those
// are not the same size — but the second cost is real too, which is why this is
// 3 and not "however many it takes to stop anyone complaining". At today's
// numbers a dead screen is visible within 54 s; under the withdrawn 180 000 ms
// window it was three minutes. The raw ages ride every response, so a console
// that wants a tighter line can draw one.
const ScreenLiveWindowCadenceMultiple int64 = 3

// ScreenLiveWindowMs is the live/stale threshold applied to `last_pull_age_ms`,
// COMPUTED from the cadence bound above rather than written down beside it.
// Changing either mirrored player timing moves the window with it; that is the
// entire point.
const ScreenLiveWindowMs int64 = HealthyProgramPullCadenceMs * ScreenLiveWindowCadenceMultiple
