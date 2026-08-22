package relay1

import (
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/app/screens"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// screenstatus.go drives REL-119/119a/119b against the committed
// internal/app/screens registry — the app peer's real consumer of
// `screen.status` — rather than re-implementing the view it maintains.
//
// The frame carries screen liveness, which is the `screens.live` number a
// deployment reads to answer "is anything playing". It shipped for weeks
// governed by no contract clause at all; the clause exists now, and this is what
// turns it from a specified rule into a driven one.

// rel119Input is exactly the corpus fields this stage reads: one relay's
// identity, the instant its report was taken, and two successive reports — a
// populated one and the EMPTY one whose whole job is to clear the first.
type rel119Input struct {
	RelayID      string                   `json:"relay_id"`
	TakenAtMs    int64                    `json:"taken_at_ms"`
	FirstReport  []wire.ScreenStatusEntry `json:"first_report"`
	SecondReport []wire.ScreenStatusEntry `json:"second_report"`
}

// rel119Expected is the subset of the expected block this stage diffs the live
// registry against.
type rel119Expected struct {
	ScreensAfterFirstReport            int    `json:"screens_after_first_report"`
	ScreensAfterEmptyReport            int    `json:"screens_after_empty_report"`
	NeverObservedAgeMs                 int64  `json:"never_observed_age_ms"`
	NeverObservedIsDistinctFromJustNow bool   `json:"never_observed_is_distinct_from_just_now"`
	HandedRevision                     string `json:"handed_revision"`
	AcceptedRevision                   string `json:"accepted_revision"`
	IntentAndAcceptanceAreSeparate     bool   `json:"intent_and_acceptance_are_separate"`
}

func decodeREL119(c corpus.Case) (rel119Input, rel119Expected, error) {
	var in rel119Input
	var exp rel119Expected
	rawIn, err := json.Marshal(c.Input)
	if err != nil {
		return in, exp, fmt.Errorf("re-marshal input: %w", err)
	}
	if err := json.Unmarshal(rawIn, &in); err != nil {
		return in, exp, fmt.Errorf("decode input: %w", err)
	}
	rawExp, err := json.Marshal(c.Expected)
	if err != nil {
		return in, exp, fmt.Errorf("re-marshal expected: %w", err)
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		return in, exp, fmt.Errorf("decode expected: %w", err)
	}
	return in, exp, nil
}

func driveREL119(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-119")
	if !ok {
		rep.Fail("REL-119", contract, "case not found in frozen corpus")
		return
	}
	in, exp, err := decodeREL119(c)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus case: %v", err))
		return
	}

	// The registry's clock is fixed at the report instant, so every age it
	// derives is the corpus's own arithmetic rather than wall-clock drift.
	reg, err := screens.NewRegistry(func() int64 { return in.TakenAtMs })
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("build screens registry: %v", err))
		return
	}

	var diffs []report.Diff

	if err := reg.ApplyScreenStatus(in.RelayID, in.TakenAtMs, in.FirstReport); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("apply first report: %v", err))
		return
	}
	first := reg.Statuses()
	if len(first) != exp.ScreensAfterFirstReport {
		diffs = append(diffs, report.Diff{
			Field:    "screens_after_first_report",
			Expected: exp.ScreensAfterFirstReport, Actual: len(first),
		})
	}

	// REL-119a — the never-observed sentinel, and that it stays TELLABLE from a
	// just-now zero. A consumer that folded them together would rank a screen
	// that has never pulled as the most recently active on the page.
	var never, justNow *screens.Status
	for i := range first {
		switch first[i].LastPullAgeMs {
		case exp.NeverObservedAgeMs:
			never = &first[i]
		case 0:
			justNow = &first[i]
		}
	}
	if never == nil {
		diffs = append(diffs, report.Diff{
			Field:    "never_observed_age_ms",
			Expected: exp.NeverObservedAgeMs, Actual: "no screen carried the never-observed sentinel",
		})
	}
	if exp.NeverObservedIsDistinctFromJustNow && never != nil && justNow != nil &&
		never.LastPullAgeMs == justNow.LastPullAgeMs {
		diffs = append(diffs, report.Diff{
			Field:    "never_observed_is_distinct_from_just_now",
			Expected: true, Actual: false,
		})
	}

	// REL-119b — intent stays separate from acceptance. The screen that pulled
	// carries both; they must not have collapsed into one value.
	var handed *screens.Status
	for i := range first {
		if first[i].ProgramRevision == exp.HandedRevision {
			handed = &first[i]
		}
	}
	if handed == nil {
		diffs = append(diffs, report.Diff{
			Field:    "handed_revision",
			Expected: exp.HandedRevision, Actual: "no screen reported it",
		})
	} else {
		if handed.AckedProgramRevision != exp.AcceptedRevision {
			diffs = append(diffs, report.Diff{
				Field:    "accepted_revision",
				Expected: exp.AcceptedRevision, Actual: handed.AckedProgramRevision,
			})
		}
		if exp.IntentAndAcceptanceAreSeparate && handed.ProgramRevision == handed.AckedProgramRevision {
			diffs = append(diffs, report.Diff{
				Field:    "intent_and_acceptance_are_separate",
				Expected: true, Actual: false,
			})
		}
	}

	// REL-119 — the empty report is a REPLACE, and replacing with nothing is
	// what clears the view. A consumer treating an empty array as "no news"
	// would keep describing screens the relay has forgotten, indefinitely,
	// because nothing else will ever clear them.
	if err := reg.ApplyScreenStatus(in.RelayID, in.TakenAtMs, in.SecondReport); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("apply empty report: %v", err))
		return
	}
	if got := len(reg.Statuses()); got != exp.ScreensAfterEmptyReport {
		diffs = append(diffs, report.Diff{
			Field:    "screens_after_empty_report",
			Expected: exp.ScreensAfterEmptyReport, Actual: got,
		})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "screen.status view diverged from the contract", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"a full-set replace clears the relay's view when empty; the never-observed sentinel stays distinct from a just-now age; intent and acceptance remain separate")
}
