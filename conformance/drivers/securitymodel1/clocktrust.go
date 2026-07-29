package securitymodel1

import (
	"os"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/app/auth"
)

// driveClockFloorSurvivesRestart drives
// SEC-066-valid-monotonic-floor-survives-restart against the LIVE app-tier clock
// floor (internal/app/auth.ClockFloor).
//
// The restart is a REAL restart of the component, not a simulated one: the first
// ClockFloor establishes the case's own `persisted_clock_floor_ms` from a
// verifiable source and is then dropped, and a SECOND ClockFloor is opened over
// the same directory with a host clock that reads the case's own
// `restart.host_wall_clock_reading_ms` — earlier than the floor. Everything the
// second instance knows it had to read back off disk; nothing carries over in
// memory. A floor that was never persisted, or that was persisted and not
// consulted, fails here rather than passing on a value the first instance
// happened to still be holding.
//
// The expected `clock_assessment: untrusted` is the part of this case worth
// reading closely, and it is why the "have I verified time" bit is deliberately
// NOT persisted alongside the floor (see internal/app/auth/clockfloor.go's file
// doc): SEC-068 defines `untrusted` as "while it holds no independently verified
// time above the floor", and a freshly restarted app holds a floor it inherited
// rather than one it has verified in this boot.
func driveClockFloorSurvivesRestart(rep *report.Report, c corpus.Case) {
	k := newCheck(c)

	floorMs, okFloor := inputInt(c, "persisted_clock_floor_ms")
	wallMs, okWall := inputInt(c, "restart.host_wall_clock_reading_ms")
	if !okFloor || !okWall {
		k.fail(rep, "the frozen input block is missing a field this case is driven from (floor=%v wall=%v)", okFloor, okWall)
		return
	}

	dir, err := os.MkdirTemp("", "secmodel1-clockfloor-")
	if err != nil {
		k.fail(rep, "scratch dir: %v", err)
		return
	}
	defer os.RemoveAll(dir)

	// Boot 1: establish the floor from a verifiable source, exactly as a real
	// deployment would on learning authenticated network time. Seeding the file
	// by hand would prove the reader works and the writer nothing.
	before, err := auth.OpenClockFloor(dir, func() int64 { return floorMs })
	if err != nil {
		k.fail(rep, "open the clock floor: %v", err)
		return
	}
	advanced, err := before.Advance(floorMs, auth.TimeSourceVerifiable)
	if err != nil || !advanced {
		k.fail(rep, "seed the persisted floor: advanced=%v err=%v", advanced, err)
		return
	}

	// Boot 2: a fresh component over the same directory, with the host clock
	// rolled back below the floor.
	after, err := auth.OpenClockFloor(dir, func() int64 { return wallMs })
	if err != nil {
		k.fail(rep, "reopen the clock floor after restart: %v", err)
		return
	}

	adopted := after.Now()
	k.intAt("adopted_time_ms", adopted)
	k.boolAt("adopted_below_floor", adopted < after.FloorMs())
	k.stringAt("clock_assessment", after.Assessment())
	k.finish(rep)
}

// driveClockFloorAdvanceGate drives
// SEC-067-invalid-unauthenticated-time-claim-does-not-advance-floor against the
// same live component, offering it the case's own TWO candidates in the case's
// own order.
//
// The corpus makes this adversarial on purpose and the driver preserves that: the
// unauthenticated candidate's timestamp (1900000000000) is FAR HIGHER than the
// verifiable one (1752624000000). An implementation that took the largest
// candidate, or that treated "later than the floor" as sufficient evidence,
// passes neither assertion below — the floor after the unauthenticated claim must
// still read the ORIGINAL persisted value, not the claim's.
func driveClockFloorAdvanceGate(rep *report.Report, c corpus.Case) {
	k := newCheck(c)

	floorMs, okFloor := inputInt(c, "persisted_clock_floor_ms")
	assessmentBefore, okBefore := inputString(c, "assessment_before")
	unauthTS, okUnauth := inputInt(c, "candidates.0.ts_ms")
	unauthVerifiable, okUV := digInput(c, "candidates.0.verifiable")
	verifiedTS, okVerified := inputInt(c, "candidates.1.ts_ms")
	verifiableFlag, okVF := digInput(c, "candidates.1.verifiable")
	if !okFloor || !okBefore || !okUnauth || !okUV || !okVerified || !okVF {
		k.fail(rep, "the frozen input block is missing a field this case is driven from")
		return
	}

	dir, err := os.MkdirTemp("", "secmodel1-advancegate-")
	if err != nil {
		k.fail(rep, "scratch dir: %v", err)
		return
	}
	defer os.RemoveAll(dir)

	// The persisted floor the case starts from, established and then re-opened so
	// the component under test is in the same inherited-floor state the case
	// describes ("assessment_before": "untrusted").
	seed, err := auth.OpenClockFloor(dir, func() int64 { return floorMs })
	if err != nil {
		k.fail(rep, "open the clock floor: %v", err)
		return
	}
	if advanced, err := seed.Advance(floorMs, auth.TimeSourceVerifiable); err != nil || !advanced {
		k.fail(rep, "seed the persisted floor: advanced=%v err=%v", advanced, err)
		return
	}
	floor, err := auth.OpenClockFloor(dir, func() int64 { return floorMs })
	if err != nil {
		k.fail(rep, "reopen the clock floor: %v", err)
		return
	}
	if got := floor.Assessment(); got != assessmentBefore {
		k.diff("assessment_before (input precondition)", assessmentBefore, got)
	}

	// Candidate 1: the unauthenticated client claim. The source classification
	// comes from the CASE's own `verifiable` flag, not from a literal here, so a
	// corpus that reclassified it would drive a different call.
	unauthAdvanced, err := floor.Advance(unauthTS, timeSource(unauthVerifiable))
	if err != nil {
		k.fail(rep, "offer the unauthenticated candidate: %v", err)
		return
	}
	k.intAt("floor_after_unauthenticated_claim_ms", floor.FloorMs())
	k.boolAt("unauthenticated_claim_advanced_floor", unauthAdvanced)

	// Candidate 2: the independently verifiable value.
	verifiedAdvanced, err := floor.Advance(verifiedTS, timeSource(verifiableFlag))
	if err != nil {
		k.fail(rep, "offer the verifiable candidate: %v", err)
		return
	}
	k.intAt("floor_after_verifiable_value_ms", floor.FloorMs())
	k.boolAt("verifiable_value_advanced_floor", verifiedAdvanced)
	k.stringAt("assessment_after", floor.Assessment())
	k.finish(rep)
}

// driveClockFloorProvenanceGate drives
// SEC-067a-invalid-unauthenticated-claim-below-a-verifiable-value against the
// same live component, and it exists because the case above cannot decide
// SEC-067 on its own.
//
// SEC-067 is a statement about PROVENANCE: "an unauthenticated time claim MUST
// NOT by itself advance the floor". The case above offers an unauthenticated
// candidate whose timestamp is far HIGHER than its verifiable one, so its two
// candidates differ in two variables at once — where the value came from, and
// how big it is. An implementation that ignores provenance entirely and admits
// any candidate under some absolute plausibility cap passes that case (the
// year-2030 claim is refused for being implausible, the near one accepted for
// being plausible) while letting any client-supplied time walk the floor
// forward, which is the exact MUST-NOT.
//
// This case closes that hole by holding magnitude fixed as a variable and
// varying only provenance: its unauthenticated candidate is SMALLER than the
// verifiable value that follows, and both sit above the persisted floor. Any
// gate that would admit the verifiable value on plausibility alone must admit
// the unauthenticated one too, since the unauthenticated one is the LESS
// extreme of the two. The only rule that separates them is the source
// classification, which is what the requirement is about.
//
// Both halves of that isolation are asserted rather than assumed: the case
// declares `unauthenticated_claim_is_below_verifiable_value` and
// `unauthenticated_claim_is_above_floor` in its own expected block, and the
// driver computes both off the case's own input and diffs them. A later edit
// that raised the unauthenticated candidate back above the verifiable one would
// fail HERE, loudly, instead of quietly restoring the two-variable case this one
// replaces.
func driveClockFloorProvenanceGate(rep *report.Report, c corpus.Case) {
	k := newCheck(c)

	floorMs, okFloor := inputInt(c, "persisted_clock_floor_ms")
	assessmentBefore, okBefore := inputString(c, "assessment_before")
	unauthTS, okUnauth := inputInt(c, "candidates.0.ts_ms")
	unauthVerifiable, okUV := digInput(c, "candidates.0.verifiable")
	verifiedTS, okVerified := inputInt(c, "candidates.1.ts_ms")
	verifiableFlag, okVF := digInput(c, "candidates.1.verifiable")
	if !okFloor || !okBefore || !okUnauth || !okUV || !okVerified || !okVF {
		k.fail(rep, "the frozen input block is missing a field this case is driven from")
		return
	}

	// The case's own isolation invariant, checked before anything is concluded
	// from the case: the unauthenticated candidate must be the SMALLER of the two
	// and must still be a forward move from the floor. If either stops holding,
	// the case has stopped isolating provenance and its result means nothing.
	k.boolAt("unauthenticated_claim_is_below_verifiable_value", unauthTS < verifiedTS)
	k.boolAt("unauthenticated_claim_is_above_floor", unauthTS > floorMs)

	dir, err := os.MkdirTemp("", "secmodel1-provenance-")
	if err != nil {
		k.fail(rep, "scratch dir: %v", err)
		return
	}
	defer os.RemoveAll(dir)

	seed, err := auth.OpenClockFloor(dir, func() int64 { return floorMs })
	if err != nil {
		k.fail(rep, "open the clock floor: %v", err)
		return
	}
	if advanced, err := seed.Advance(floorMs, auth.TimeSourceVerifiable); err != nil || !advanced {
		k.fail(rep, "seed the persisted floor: advanced=%v err=%v", advanced, err)
		return
	}
	floor, err := auth.OpenClockFloor(dir, func() int64 { return floorMs })
	if err != nil {
		k.fail(rep, "reopen the clock floor: %v", err)
		return
	}
	if got := floor.Assessment(); got != assessmentBefore {
		k.diff("assessment_before (input precondition)", assessmentBefore, got)
	}

	unauthAdvanced, err := floor.Advance(unauthTS, timeSource(unauthVerifiable))
	if err != nil {
		k.fail(rep, "offer the unauthenticated candidate: %v", err)
		return
	}
	k.intAt("floor_after_unauthenticated_claim_ms", floor.FloorMs())
	k.boolAt("unauthenticated_claim_advanced_floor", unauthAdvanced)
	// The assessment is read BETWEEN the two candidates, which the case above
	// never does: SEC-068 defines `trusted` as holding independently verified
	// time above the floor, so a gate that admitted the unauthenticated claim as
	// evidence would report `trusted` here even if it had somehow left the floor
	// where it was.
	k.stringAt("assessment_after_unauthenticated_claim", floor.Assessment())

	verifiedAdvanced, err := floor.Advance(verifiedTS, timeSource(verifiableFlag))
	if err != nil {
		k.fail(rep, "offer the verifiable candidate: %v", err)
		return
	}
	k.intAt("floor_after_verifiable_value_ms", floor.FloorMs())
	k.boolAt("verifiable_value_advanced_floor", verifiedAdvanced)
	k.stringAt("assessment_after", floor.Assessment())
	k.finish(rep)
}

// timeSource maps a corpus candidate's own `verifiable` flag onto the closed
// TimeSource type the advance gate takes (SEC-067).
func timeSource(verifiable any) auth.TimeSource {
	if b, ok := verifiable.(bool); ok && b {
		return auth.TimeSourceVerifiable
	}
	return auth.TimeSourceUnauthenticated
}
