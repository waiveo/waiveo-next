package relay1

import (
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// deviceidentity.go drives REL-110b (both peers derive a device_id/entity_id
// from REL-153's identity tuple rather than assigning one) and REL-111a's
// multi-relay half, against the committed app-side read model
// (internal/app/devices) and the committed derivation
// (internal/shared/deviceid) — never a re-implementation of either.
//
// The case's own derived ids are FROZEN in the corpus. That is the point of
// driving it here: the derivation is a value both an app peer and every relay
// must compute identically (REL-110b), so it is a wire-visible constant, not an
// implementation detail either side may change alone.

// rel110bInput is the corpus input: the site the identities are scoped to, the
// ordered reports to apply, and one final report that empties a relay's view.
type rel110bInput struct {
	SiteScopeNode string          `json:"site_scope_node"`
	Reports       []rel110bReport `json:"reports"`
	FinalReport   rel110bReport   `json:"final_report"`
}

type rel110bReport struct {
	RelayID    string                 `json:"relay_id"`
	Candidates []wire.DeviceCandidate `json:"candidates"`
}

// rel110bExpected is the oracle: the row count, and per identity the derived
// ids plus which relay the row is attributed to at each stage.
type rel110bExpected struct {
	DeviceRowCount                 int   `json:"device_row_count"`
	DeviceRowCountAfterFinalReport int   `json:"device_row_count_after_final_report"`
	DerivedIDsAreCanonicalULIDs    *bool `json:"derived_ids_are_canonical_ulids"`
	RelayIDsAreCanonicalULIDs      bool  `json:"relay_ids_are_canonical_ulids"`
	DerivedRows                    []struct {
		Driver                  string            `json:"driver"`
		NativeID                string            `json:"native_id"`
		DeviceID                string            `json:"device_id"`
		EntityIDs               map[string]string `json:"entity_ids"`
		RelayIDAfterBothReports string            `json:"relay_id_after_both_reports"`
		RelayIDAfterFinalReport string            `json:"relay_id_after_final_report"`
	} `json:"derived_rows"`
}

func decodeInto(v any, from any, what string) error {
	raw, err := json.Marshal(from)
	if err != nil {
		return fmt.Errorf("marshal corpus %s: %w", what, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("unmarshal corpus %s: %w", what, err)
	}
	return nil
}

// driveREL110b applies the corpus's reports to a real read model in order and
// diffs the resulting rows against the frozen derived ids and attributions.
func driveREL110b(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-110b")
	if !ok {
		rep.Fail("REL-110b", contract, "case not found in frozen corpus")
		return
	}

	var in rel110bInput
	if err := decodeInto(&in, c.Input, "input"); err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	var exp rel110bExpected
	if err := decodeInto(&exp, c.Expected, "expected"); err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}

	var diffs []report.Diff

	registry := devices.New(in.SiteScopeNode)
	for i, r := range in.Reports {
		if err := registry.ApplyCandidates(r.RelayID, r.Candidates); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("report[%d] from %s was refused: %v", i, r.RelayID, err), diffs...)
			return
		}
	}

	if got := len(registry.Devices()); got != exp.DeviceRowCount {
		diffs = append(diffs, report.Diff{Field: "device_row_count (one row per identity, REL-153)", Expected: exp.DeviceRowCount, Actual: got})
	}

	rows := indexByID(registry)
	for _, want := range exp.DerivedRows {
		// The derivation itself, against the frozen value: both peers compute
		// this, so a change here is a change to the protocol.
		if got := deviceid.Device(in.SiteScopeNode, want.Driver, want.NativeID); got != want.DeviceID {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("derived device_id for (%s, %s)", want.Driver, want.NativeID), Expected: want.DeviceID, Actual: got})
		}
		for key, wantEntity := range want.EntityIDs {
			if got := deviceid.Entity(in.SiteScopeNode, want.Driver, want.NativeID, key); got != wantEntity {
				diffs = append(diffs, report.Diff{Field: fmt.Sprintf("derived entity_id for (%s, %s, %s)", want.Driver, want.NativeID, key), Expected: wantEntity, Actual: got})
			}
			if _, found := registry.Entity(wantEntity); !found {
				diffs = append(diffs, report.Diff{Field: fmt.Sprintf("entity %s resolvable in the read model", wantEntity), Expected: true, Actual: false})
			}
		}
		// Asserted the way RelayIDsAreCanonicalULIDs is eleven lines below — in both
		// directions, and requiring the expectation to be declared. As a plain bool
		// gating its own check, removing the key from the case removed the DAT-005a
		// assertion and the driver still passed.
		if canonical, missing := declaredBool("derived_ids_are_canonical_ulids", exp.DerivedIDsAreCanonicalULIDs); missing != nil {
			diffs = append(diffs, *missing)
		} else if canonical != ulid.Valid(want.DeviceID) {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("device_id %s is a canonical ULID (DAT-005a)", want.DeviceID), Expected: canonical, Actual: ulid.Valid(want.DeviceID)})
		}
		row, ok := rows[want.DeviceID]
		if !ok {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("device row %s present after both reports", want.DeviceID), Expected: true, Actual: false})
			continue
		}
		if row.RelayID != want.RelayIDAfterBothReports {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("device %s relay_id after both reports (most recent reporter, REL-153)", want.DeviceID), Expected: want.RelayIDAfterBothReports, Actual: row.RelayID})
		}
		if exp.RelayIDsAreCanonicalULIDs != ulid.Valid(row.RelayID) {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("relay_id %s is a canonical ULID", row.RelayID), Expected: exp.RelayIDsAreCanonicalULIDs, Actual: ulid.Valid(row.RelayID)})
		}
	}

	// The second relay stops reporting entirely. Its rows go; a row another
	// relay still reports survives and reverts to that relay (REL-111a).
	if err := registry.ApplyCandidates(in.FinalReport.RelayID, in.FinalReport.Candidates); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("final report from %s was refused: %v", in.FinalReport.RelayID, err), diffs...)
		return
	}
	if got := len(registry.Devices()); got != exp.DeviceRowCountAfterFinalReport {
		diffs = append(diffs, report.Diff{Field: "device_row_count_after_final_report (a device another relay still reports survives, REL-111a)", Expected: exp.DeviceRowCountAfterFinalReport, Actual: got})
	}
	rows = indexByID(registry)
	for _, want := range exp.DerivedRows {
		row, ok := rows[want.DeviceID]
		if !ok {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("device row %s present after the final report", want.DeviceID), Expected: true, Actual: false})
			continue
		}
		if row.RelayID != want.RelayIDAfterFinalReport {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("device %s relay_id after the final report", want.DeviceID), Expected: want.RelayIDAfterFinalReport, Actual: row.RelayID})
		}
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "derived device identity diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

func indexByID(r *devices.Registry) map[string]devices.Device {
	out := map[string]devices.Device{}
	for _, d := range r.Devices() {
		out[d.ID] = d
	}
	return out
}
