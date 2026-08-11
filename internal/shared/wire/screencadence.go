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
//
// # This window is only half of "a broken wall reads stale"
//
// The second bullet above is a property of the LIVE window, and for one round it
// was mistaken for a property of the read model. It is not: a screen past this
// window is not live, but it is only STALE if nothing else claims it first, and
// `fetching` claimed it. The 2026-08 screen — 200 on every program pull, a 403
// on every content fetch, never an ack — has a `last_pull_age_ms` that tops out
// at 78 000 ms, inside ScreenContentTransferWindowMs, with the pull permanently
// unacknowledged. Bounding `fetching` by pull AGE alone therefore captured it
// forever, and the tolerance bought by reducing 3 to 2 was handed straight back.
// ScreenFetchingMaxUnackedPulls is the other half; read its doc with this one.
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
// # What "unacknowledged" actually means, since it is weaker than it sounds
//
// The relay does NOT correlate an ack with the lease it acknowledges.
// playerserver's noteLeaseAck stamps `lastAckMs` on ANY arriving ack and reads
// no `lease_id` from it, so "this pull is unacknowledged" is really "the last
// ack this relay saw predates the last pull it served". On the shipped player
// the two coincide — one pull, one ack, in order, on one thread — and that is
// why the inference is sound today. It is not sound in general: a player that
// acknowledged out of order, or retried an old ack, would reset the signal.
// TestTheAckFollowsTheContentFetch pins the ordering the inference rests on; the
// correlation itself is not pinned because it does not exist yet.
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
//
// # This window is NOT sufficient on its own — see ScreenFetchingMaxUnackedPulls
//
// An age bound expires `fetching` for a screen that went quiet. It does nothing
// at all for a screen that is LOUD: the 2026-08 wall re-pulled every 60 000 ms
// forever and never acked, so its `last_pull_age_ms` reset before it could ever
// leave this window. The read model needs a second bound, on how many pulls have
// gone unacknowledged rather than on how old one of them is.
const ScreenContentTransferWindowMs int64 = ScreenLiveWindowMs + ProgramContentFetchTimeoutMs

// OutstandingPullsWhileTransferring is how many unacknowledged pulls a screen
// that is genuinely materialising content has outstanding: exactly ONE.
//
// It is a FACT about the shipped player's loop shape, not a tolerance. The
// transfer is serialised between the pull and the ack — `wvDoProgram` pulls,
// runs `wvEnsureContent` over every asset, then calls `wvAckLease`, and
// PlayerTask sleeps only after that returns (Program.brs:337/365) — so a screen
// in the middle of a download has made no further pull to be waiting on. There
// is one Lease in flight, ageing, and that is the whole of what `fetching` was
// ever describing.
//
// Which is the observation the 2026-08 case turns on: a screen presenting a
// SECOND unacknowledged pull is not slow at fetching. It abandoned the first
// Lease without acknowledging it, which is what the shipped player does when
// `wvEnsureContent` fails — it returns before the ack (Program.brs:337) and
// retries on a backoff. That is a failing iteration, not a transfer.
const OutstandingPullsWhileTransferring int64 = 1

// ScreenFailedPullTolerance is how many consecutive FAILED player iterations
// this file's judgements give a screen the benefit of the doubt for.
//
// ONE, and it is the tolerance the live window already has rather than a second
// taste: the window (52 000 ms) covers a healthy screen's worst honest age plus
// one failed pull and its first backoff (46 000 ms) and does NOT cover two
// (58 000 ms). Both directions are computed from the player's own backoff in
// screencadence_test.go — TestTheLiveWindowCoversAHealthyScreenAndOneFailedPull
// and TestLiveWindowStillDistinguishesAFailedScreen — and both are now written
// in terms of THIS constant, so the number cannot be changed in one place and
// left in the other.
//
// Naming it is the point. A transient failure is worth absorbing, in every
// judgement here, by the same amount; two different tolerances in two clauses of
// one function is how a screen ends up live by one rule and fetching by another.
const ScreenFailedPullTolerance int64 = 1

// ScreenFetchingMaxUnackedPulls is the SECOND bound on `fetching`, and the one
// that makes the state mean "making progress" rather than "not acknowledged".
//
// # Why an age bound was not enough
//
// ScreenContentTransferWindowMs expires `fetching` for a screen that stopped.
// The 2026-08 wall did not stop. It got a 200 on every program pull (so the
// relay re-stamped `lastPullMs` every time), failed `wvEnsureContent` on a 403,
// returned before `wvAckLease`, and retried at the 60 000 ms backoff cap —
// forever. Its app-side age topped out at 78 000 ms (60 000 backoff + 8 000
// request timeout + 10 000 report interval), comfortably inside the 172 000 ms
// transfer window, with the last pull permanently unacknowledged. Both clauses
// of the old rule were satisfied on every single sample, so the console said
// `fetching` about a wall that had never fetched anything and never would.
//
// That was not a cosmetic mislabel. It disabled two things at once: the
// operator-facing chip said "Collecting content" — and, in the phrasing of the
// day, that the previous program was still up — about a screen that was doing
// neither, and the fleet roll-up
// (internal/app/api/diagnostics.go) grades `down` on `Live == 0 && Fetching ==
// 0`, so an entire site in this state read `degraded` forever and never `down`.
// The fleet-dark alarm was switched off for exactly the failure it exists for.
//
// # The bound
//
// A screen that is transferring has ONE pull outstanding
// (OutstandingPullsWhileTransferring — it cannot have two, the fetch is inside
// the loop). Absorb one failed iteration on top of it, the same tolerance the
// live window gives (ScreenFailedPullTolerance), and the third consecutive
// unacknowledged pull is a screen that is failing, which is a screen the console
// must be allowed to call `stale`.
//
// For the 2026-08 wall that is about six seconds: its backoff runs 2 s, 4 s, 8 s
// … so the third unacknowledged pull lands almost immediately and the screen
// spends the rest of its broken life judged by age alone — live at the bottom of
// its sawtooth, stale at the top, which is what a screen in retry backoff is
// supposed to read (TestAScreenInRetryBackoffIsAllowedToReadStale).
//
// # What it costs
//
// A screen whose fetch fails twice transiently and then succeeds on a third,
// long transfer reads `stale` during that transfer instead of `fetching`. That
// is a screen that has failed two consecutive iterations, which the live window
// already calls stale on age alone, so the two rules agree — which is the reason
// the tolerance is shared rather than picked twice.
//
// # The counter this is applied to
//
// internal/relay/playerserver keeps it: one `unackedPulls` per screen,
// incremented when a program pull is served a Lease WITH CONTENT and reset to
// zero when an ack arrives. It rides the `screen.status` frame as
// `unacked_pulls`. Two caveats, both of which change what a number here means:
//
//   - noteLeaseAck correlates nothing (the same caveat the age comparison has),
//     so ANY ack resets the counter and "N unacknowledged pulls" means "N
//     content-bearing pulls served since the last ack of any kind".
//   - an EMPTY Lease is not counted. It carries nothing to fetch and gets no ack
//     — the shipped player returns before wvAckLease on an empty content array,
//     the same way it does on a failed fetch — so counting one would accumulate
//     an obligation that can never be discharged. That is the ordinary state of
//     a freshly paired screen (terminalDefault) and of every screen scheduled
//     blank overnight, and while those counted, the first real program a box
//     served read `stale` mid-download and a one-screen site graded `down`.
const ScreenFetchingMaxUnackedPulls int64 = OutstandingPullsWhileTransferring + ScreenFailedPullTolerance
