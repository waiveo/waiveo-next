package relay1

import (
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// scanacceptance.go drives REL-127 against the committed wire constructors —
// the ones every producer of a `discovery.scan_result` goes through.
//
// The rule under test is about SHAPE, and shape is exactly what prose cannot
// pin: all three answers are a small struct with the same members, and the
// differences between "it started", "one was already running" and "I refused"
// are which of those members are set. Getting one wrong produces a result that
// decodes cleanly and means something else — a busy relay reported as a failure,
// or a refusal an operator cannot read.

type rel127Input struct {
	StartedScanID        string `json:"started_scan_id"`
	AlreadyRunningScanID string `json:"already_running_scan_id"`
	RefusalCode          string `json:"refusal_code"`
	RefusalMessage       string `json:"refusal_message"`
}

type rel127Shape struct {
	OK            bool `json:"ok"`
	Started       bool `json:"started"`
	CarriesScanID bool `json:"carries_scan_id"`
	CarriesError  bool `json:"carries_error"`
}

type rel127Expected struct {
	Accepted rel127Shape `json:"accepted"`
	Busy     rel127Shape `json:"busy"`
	Refused  rel127Shape `json:"refused"`
}

func decodeREL127(c corpus.Case) (rel127Input, rel127Expected, error) {
	var in rel127Input
	var exp rel127Expected
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

func shapeOf(b wire.DiscoveryScanResultBody) rel127Shape {
	return rel127Shape{
		OK:            b.OK,
		Started:       b.Started,
		CarriesScanID: b.ScanID != "",
		CarriesError:  b.Error != nil,
	}
}

func diffShape(name string, want, got rel127Shape) []report.Diff {
	var out []report.Diff
	if want != got {
		out = append(out, report.Diff{Field: name, Expected: want, Actual: got})
	}
	return out
}

func driveREL127(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-127")
	if !ok {
		rep.Fail("REL-127", contract, "case not found in frozen corpus")
		return
	}
	in, exp, err := decodeREL127(c)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus case: %v", err))
		return
	}

	var diffs []report.Diff
	accepted := wire.NewDiscoveryScanAccepted(in.StartedScanID)
	busy := wire.NewDiscoveryScanBusy(in.AlreadyRunningScanID)
	refused := wire.NewDiscoveryScanError(in.RefusalCode, in.RefusalMessage)

	diffs = append(diffs, diffShape("accepted", exp.Accepted, shapeOf(accepted))...)
	diffs = append(diffs, diffShape("busy", exp.Busy, shapeOf(busy))...)
	diffs = append(diffs, diffShape("refused", exp.Refused, shapeOf(refused))...)

	// The busy answer must name the RUNNING scan, not the one that was asked
	// for: an operator who double-clicked needs the id progress is reported
	// under, and echoing a fresh id would point them at a scan that never began.
	if busy.ScanID != in.AlreadyRunningScanID {
		diffs = append(diffs, report.Diff{
			Field: "busy.scan_id", Expected: in.AlreadyRunningScanID, Actual: busy.ScanID,
		})
	}
	// A refusal's code must survive to the operator. An `ok:false` with an empty
	// code is the unreadable refusal REL-127 forbids.
	if refused.Error == nil || refused.Error.Code != in.RefusalCode {
		diffs = append(diffs, report.Diff{
			Field: "refused.error.code", Expected: in.RefusalCode, Actual: refused.Error,
		})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "scan-result shapes diverged from the contract", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"a started scan, a benign repeat and a refusal are distinguishable by shape; the busy answer names the running scan; a refusal carries a readable code")
}
