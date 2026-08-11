package wire

// screencadence.go declares — in ONE place — the timings that decide whether a
// screen reads `live`, `fetching` or `stale`, plus the windows derived from
// them.
//
// # The defect this file exists to make impossible
//
// The live/stale window was originally a hand-picked 45 000 ms, justified in a
// comment as "four missed 10-second polls plus slack" against PLY-082's
// draft-note poll cadence. The number and the cadence it had to exceed lived in
// different files with only a comment joining them, so nothing could tell anyone
// when the justification stopped being true. Everything below exists to remove
// that gap: the player's timings are mirrored HERE, the windows are COMPUTED
// from them, and tests in this package (screencadence_test.go) pin both the
// derivations and the mirrors against the shipped BrightScript.
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
// # The second correction, same round — the loop had a term nobody had counted
//
// The replacement derivation was "poll interval plus request timeout", and it
// claimed to be "the largest value the iteration can have while the screen is
// still on the healthy path at all". That was false against the shipped player,
// which makes a SECOND blocking HTTP call per iteration: `wvAckLease`
// (player-v3/source/Program.brs:365, its own `timeoutMs: 8000` at :774), a lease
// acknowledgement sent after the content is materialised and before the sleep.
// A bound that omits a whole synchronous round trip is not a bound. It is
// counted below, and the fence that would have caught it —
// TestTheHealthyCadenceIsAnEXPRESSIONOverThePlayersTimings — reads this file's
// own syntax tree, because the previous fence compared the constant against the
// same sum it was written from and a hand-entered `18_000` passed it.
//
// So this file contains no measurement. Every window is DERIVED from timings the
// player's own source states, all mirrored under test. A screen in retry backoff
// is outside the derivation on purpose: it is genuinely degraded, and reading
// `stale` is the correct answer for it, not a flapping artifact to widen a
// window against.
//
// # Why this package
//
// These are properties of the `screen.status` frame's timing, and this is the
// package both ends of that frame already import: cmd/waiveo-relay drives its
// report ticker from ScreenStatusReportIntervalMs, internal/app/screens draws
// its thresholds from ScreenLiveWindowMs and ScreenContentTransferWindowMs. The
// relay tree deliberately imports no internal/app package (a real layering
// boundary — check the relay's imports), so a constant both sides must agree on
// cannot live on either side.

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
// `/player/v1/program` call passes (player-v3/source/Program.brs:135), mirrored
// here and pinned by TestProgramPullRequestTimeoutMirrorsTheShippedPlayer.
//
// It is in the cadence because the relay stamps `last_pull_age_ms` when it
// HANDLES the request (internal/relay/playerserver/screenstatus.go), so whatever
// of that exchange happens after the stamp rides inside the age. Past this
// timeout the player abandons the request, counts a failure and waits a backoff
// instead of the poll interval — i.e. it has left the healthy path, which is
// what makes this a bound and not an estimate.
const ProgramPullRequestTimeoutMs int64 = 8_000

// LeaseAckRequestTimeoutMs is the SECOND blocking round trip a healthy iteration
// makes: `wvAckLease` POSTs `/player/v1/lease/ack` (PLY-091) with its own
// `timeoutMs`, mirrored here and pinned by
// TestLeaseAckRequestTimeoutMirrorsTheShippedPlayer.
//
// It is inside the loop, not beside it — `wvDoProgram` calls it synchronously
// before returning (Program.brs:365) and PlayerTask sleeps only after that
// returns — so a healthy iteration is genuinely poll + program + ack, and a
// cadence that counted only the program request understated the loop by a whole
// eight seconds. That understatement is what made the previous round's
// "survives a whole missed pull" arithmetic come out green: 2×18 000 + 10 000 =
// 46 000 against a 54 000 window, where the honest 2×26 000 + 10 000 = 62 000
// does not fit and never did.
//
// The ack is best-effort in the player (a failure does not stop rendering), but
// best-effort still BLOCKS: the call waits out its own timeout before the player
// moves on, so it bounds the iteration whether it succeeds or not.
const LeaseAckRequestTimeoutMs int64 = 8_000

// ProgramContentFetchTimeoutMs is the player's per-asset transfer timeout —
// `wvContentFetchTimeoutMs()` (Program.brs:590), mirrored here and pinned by
// TestContentFetchTimeoutMirrorsTheShippedPlayer.
//
// It is deliberately NOT part of the healthy cadence, and it is deliberately not
// ignored either. See ScreenContentTransferWindowMs for both halves of that
// decision, which is the one this file gets asked about most.
const ProgramContentFetchTimeoutMs int64 = 120_000

// HealthyProgramPullCadenceMs is the largest pull-to-pull interval a screen can
// produce while still being HEALTHY: everything one iteration of the player's
// loop can spend, measured the way the relay measures it.
//
// The relay stamps the pull when it handles the program request, so the terms
// after that stamp are, in the order the player performs them:
//
//	ProgramPullRequestTimeoutMs   the rest of the program exchange; past this
//	                              the player abandons it and is no longer healthy
//	LeaseAckRequestTimeoutMs      wvAckLease, a second synchronous round trip
//	ProgramPollIntervalMs         wvSleepInterruptible, the ordinary wait
//
// which is 26 000 ms. A cache-HIT pass through `wvEnsureContent` sits between
// the first two terms and costs a map lookup by design — the player's own
// comment there records that re-hashing cached content "would be re-asking a
// question whose answer cannot have changed, at the cost of a full file read
// every ten seconds". A cache MISS is a different case and is answered by
// ScreenContentTransferWindowMs, not by inflating this.
//
// It is an upper BOUND derived from the player's own timings, not a measurement
// of any particular screen, and that is the point: a bound cannot go stale the
// way a measurement can. For scale, the field figure it has to cover is a real
// screen's ~10-11 s pull-to-pull (see the correction above).
const HealthyProgramPullCadenceMs int64 = ProgramPollIntervalMs + ProgramPullRequestTimeoutMs + LeaseAckRequestTimeoutMs

// ScreenStatusReportIntervalMs is how often a connected relay re-reports its
// full per-screen observation set upward. cmd/waiveo-relay's report ticker is
// built from this value rather than from a constant of its own, so the app
// peer's staleness arithmetic and the relay's reporting rate cannot drift.
//
// It matters to the windows because the app peer's `last_pull_age_ms` is the age
// the RELAY measured plus however long the report has been sitting here: a
// perfectly healthy screen can therefore present an age of one whole pull
// cadence plus one whole report interval, purely from the two clocks being out
// of phase.
const ScreenStatusReportIntervalMs int64 = 10_000

// ScreenLiveWindowCadenceMultiple is how many whole healthy pull cadences the
// live window spans.
//
// TWO, and — unlike the 3 it replaces — the number is not a taste. It is the
// only multiple that satisfies both ends at once, now that the cadence counts
// the ack:
//
//   - It must EXCEED a healthy screen's worst honest age, which is one cadence
//     plus one report interval (36 000 ms), or every healthy screen reads stale
//     part of every cycle. 2 gives 52 000 ms, which additionally absorbs one
//     failed pull and its first backoff (8 000 + 2 000 → 46 000 ms).
//   - It must stay BELOW the player's retry-backoff cap of 60 000 ms, or a
//     screen failing every single pull — the 2026-08 case, a genuinely broken
//     wall — still reads live. 3 would give 78 000 ms and do exactly that.
//
// So the two properties bracket the window into (36 000, 60 000) and 2 is the
// only whole multiple inside it. What that costs, stated plainly: a screen that
// fails TWO consecutive pulls (age 58 000 ms) reads stale for a cycle. That is a
// screen whose player is in backoff having failed twice in a row, which is the
// thing this column is for, and both pins are in screencadence_test.go so the
// trade cannot be quietly re-made.
const ScreenLiveWindowCadenceMultiple int64 = 2

// ScreenLiveWindowMs is the live/stale threshold applied to the freshest contact
// a screen has made, COMPUTED from the cadence bound above rather than written
// down beside it. Changing any mirrored player timing moves the window with it;
// that is the entire point.
const ScreenLiveWindowMs int64 = HealthyProgramPullCadenceMs * ScreenLiveWindowCadenceMultiple

// ScreenContentTransferWindowMs is the second threshold, and the answer to the
// question this file kept deferring: what to do about a screen that is DOWNLOADING
// newly assigned content.
//
// # What is actually true
//
// The transfer is inside the loop. `wvEnsureContent` on a cache miss calls
// `wvHttpGetToFile(..., wvContentFetchTimeoutMs())` — 120 000 ms — and then
// SHA-256s the whole stored file, all of it serialised before the ack and before
// the sleep, and all of it inside the measured age (the relay stamps the pull at
// handleProgram; internal/app/screens adds the report's own age on top).
// Measured on real hardware, a 14.6 MiB cache miss peaked at 13 021 ms and never
// flapped — consistent with in-loop serialisation at LAN speed. A 60 MB video on
// building wifi is ~40 s of transfer plus an on-device hash, which puts the age
// past the live window.
//
// The previously stated justification for ignoring this — "a screen mid-transfer
// of a program it has not rendered yet is a screen an operator should be able to
// see is not yet showing the new content" — was FALSE, and is withdrawn.
// Never-wipe (PlayerTask.brs, the `everSucceeded` branch) means the screen goes
// on rendering the outgoing program for the whole transfer. The wall is working.
// Reporting it `stale` is W2-18's symptom with a new cause.
//
// # Why this is a third state and not a wider window
//
// Folding the fetch bound into the live window would make it 3 × 146 000 ms —
// seven minutes in which a dead screen reads `live`. That is the withdrawn
// 180 000 ms mistake with a bigger number, and the whole reason this file
// distinguishes a bound from a wish.
//
// So the app peer names it instead, from a signal ALREADY on the wire. The relay
// mints a `lease_id` on every served pull (playerserver/program.go) and the
// player acknowledges every Lease it materialises (Program.brs:365), so on the
// healthy path a pull and its ack pair one-to-one and the gap between them is
// exactly the content phase. A screen whose most recent pull has not been
// acknowledged is a screen inside that gap. internal/app/screens reports it
// `fetching`: not `live`, because nothing has confirmed anything; not `stale`,
// because a working wall is mid-transfer.
//
// # Why it is bounded, and what the bound does not cover
//
// Unbounded, `fetching` would swallow the other explanation for that same
// silence — a screen that lost power between its pull and its ack — and hide it
// forever. So the state expires: one whole content-fetch timeout past the live
// window, which is the player's own statement of the longest a single transfer
// may legitimately take.
//
// What that does not cover, stated rather than glossed: a Lease needing more
// than one whole fetch timeout in TOTAL — a first fill of several large videos
// on a bad link — reaches `stale` before it finishes. Nothing bounds how many
// assets one Lease can require, so no finite window covers every case; this one
// covers a whole transfer at the player's own limit, and the alternative is a
// state with no ceiling. If that case shows up in the field, the fix is a
// player-side heartbeat during a long fetch, not a bigger number here.
const ScreenContentTransferWindowMs int64 = ScreenLiveWindowMs + ProgramContentFetchTimeoutMs
