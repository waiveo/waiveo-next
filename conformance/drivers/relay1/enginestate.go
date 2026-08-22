package relay1

import (
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/app/enginestate"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// enginestate.go drives REL-116 and REL-118 against the committed
// internal/app/enginestate registry — the app peer's real consumer of
// `discovery.engine_state`.
//
// # REL-117 IS DELIBERATELY NOT DRIVEN HERE, and saying which rules a stage
// does NOT cover matters as much as which it does
//
// REL-117 is the rule that a relay reports its engine state both when a
// generation applies AND on connect. That is a relay-side LIFECYCLE property —
// it is about when a frame is sent, not about what a receiver does with it — so
// a registry stage cannot observe it at all: every report this file makes
// arrives by being handed over directly.
//
// It is guarded instead by cmd/waiveo-relay's AST wiring assertion, which fails
// if `engineState.resend` leaves the connect hook. That guard exists precisely
// because REL-070 suppresses re-applying an unchanged generation, so dropping
// the resend leaves every other test passing while a reconnected app peer holds
// no engine state for a relay that has been watching correctly throughout.
//
// Its traceability row stays TBD-wave1 rather than borrowing this case's
// coverage, which would claim a conformance case exercises something it never
// touches.

// rel116Input is exactly the corpus fields this stage reads: one relay that
// reports twice, and a second relay that never reports at all.
type rel116Input struct {
	ReportingRelayID string                        `json:"reporting_relay_id"`
	SilentRelayID    string                        `json:"silent_relay_id"`
	FirstReport      wire.DiscoveryEngineStateBody `json:"first_report"`
	SecondReport     wire.DiscoveryEngineStateBody `json:"second_report"`
	OneLaneReport    wire.DiscoveryEngineStateBody `json:"one_lane_report"`
}

// rel116Expected is the subset of the expected block this stage diffs the live
// registry against.
type rel116Expected struct {
	WatchingNothingAfterFirst               bool `json:"watching_nothing_after_first"`
	MDNSLaneAfterFirst                      bool `json:"mdns_lane_after_first"`
	MalformedAfterFirst                     int  `json:"malformed_after_first"`
	WatchingNothingAfterSecond              bool `json:"watching_nothing_after_second"`
	MalformedAfterSecond                    int  `json:"malformed_after_second"`
	StatesAfterBothReports                  int  `json:"states_after_both_reports"`
	SilentRelayIsAbsent                     bool `json:"silent_relay_is_absent"`
	WatchingNothingWithOneLaneHoldingAWatch bool `json:"watching_nothing_with_one_lane_holding_a_watch"`
}

func decodeREL116(c corpus.Case) (rel116Input, rel116Expected, error) {
	var in rel116Input
	var exp rel116Expected
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

func driveREL116(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-116")
	if !ok {
		rep.Fail("REL-116", contract, "case not found in frozen corpus")
		return
	}
	in, exp, err := decodeREL116(c)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus case: %v", err))
		return
	}

	reg := enginestate.New(func() int64 { return 1_700_000_000_000 })
	if reg == nil {
		rep.Fail(c.CaseID, contract, "enginestate.New returned no registry")
		return
	}

	var diffs []report.Diff

	// REL-116 — the watching-for-nothing report, with every count present at
	// zero and the lane flags that make those zeros readable.
	reg.ApplyDiscoveryEngineState(in.ReportingRelayID, in.FirstReport)
	first := reg.States()
	if len(first) != 1 {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("after one report the registry holds %d state(s), want 1", len(first)))
		return
	}
	if got := first[0].WatchingNothing; got != exp.WatchingNothingAfterFirst {
		diffs = append(diffs, report.Diff{Field: "watching_nothing_after_first", Expected: exp.WatchingNothingAfterFirst, Actual: got})
	}
	if got := first[0].MDNSLane; got != exp.MDNSLaneAfterFirst {
		diffs = append(diffs, report.Diff{Field: "mdns_lane_after_first", Expected: exp.MDNSLaneAfterFirst, Actual: got})
	}
	if got := first[0].Malformed; got != exp.MalformedAfterFirst {
		diffs = append(diffs, report.Diff{Field: "malformed_after_first", Expected: exp.MalformedAfterFirst, Actual: got})
	}

	// REL-118 — a newer report REPLACES wholesale. The second report zeroes the
	// undeliverable counts the first carried; a merge would leave the retired
	// generation's `malformed` standing in the current generation's view.
	reg.ApplyDiscoveryEngineState(in.ReportingRelayID, in.SecondReport)
	second := reg.States()
	if len(second) != exp.StatesAfterBothReports {
		diffs = append(diffs, report.Diff{Field: "states_after_both_reports", Expected: exp.StatesAfterBothReports, Actual: len(second)})
	}
	if len(second) > 0 {
		if got := second[0].WatchingNothing; got != exp.WatchingNothingAfterSecond {
			diffs = append(diffs, report.Diff{Field: "watching_nothing_after_second", Expected: exp.WatchingNothingAfterSecond, Actual: got})
		}
		if got := second[0].Malformed; got != exp.MalformedAfterSecond {
			diffs = append(diffs, report.Diff{Field: "malformed_after_second", Expected: exp.MalformedAfterSecond, Actual: got})
		}
	}

	// REL-116 — the judgement is over ALL lanes, not any one. This is the dev
	// box's own state: no SSDP watch and one BUILTIN mDNS watch. A relay holding
	// a watch in EITHER lane is discovering, so an alarm computed per-lane would
	// fire on a fleet that is working — and the two zero-zero reports above
	// cannot tell the two rules apart, because they agree there.
	reg.ApplyDiscoveryEngineState(in.ReportingRelayID, in.OneLaneReport)
	if got := reg.States()[0].WatchingNothing; got != exp.WatchingNothingWithOneLaneHoldingAWatch {
		diffs = append(diffs, report.Diff{
			Field:    "watching_nothing_with_one_lane_holding_a_watch",
			Expected: exp.WatchingNothingWithOneLaneHoldingAWatch, Actual: got,
		})
	}

	// REL-118 — a relay that has never reported is ABSENT, not zeroed. Zeroes
	// are the alarm; minting them for a silent relay raises it on every fresh
	// feeder, which is the false positive that would teach an operator to
	// ignore the signal entirely.
	if exp.SilentRelayIsAbsent {
		for _, st := range reg.States() {
			if st.RelayID == in.SilentRelayID {
				diffs = append(diffs, report.Diff{
					Field:    "silent_relay_is_absent",
					Expected: true,
					Actual:   "a relay that never reported was present in the view",
				})
			}
		}
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "engine-state view diverged from the contract", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"zero counts and lane flags survive a report; a newer report replaces wholesale rather than merging; a relay that never reported stays absent rather than zeroed")
}
