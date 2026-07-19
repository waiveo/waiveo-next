package relay1

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
)

// This file drives REL-110 (device.candidates full-set report, REL-110/111)
// and REL-112/113 (device.command resolve→dispatch, COMMAND_UNRESOLVED)
// directly against the committed internal/relay/deviceplane package —
// Store and CommandSurface — rather than re-implementing candidate
// tracking or command resolution.

// rel110Input decodes exactly the REL-110 corpus fields this stage reads:
// the relay's device.candidates report to reproduce through Store, the
// dispatched device.command messages, and the fixture device-class/command
// vocabulary the corpus stages them against.
type rel110Input struct {
	DeviceCandidatesReport  json.RawMessage   `json:"device_candidates_report"`
	Commands                []json.RawMessage `json:"commands"`
	TargetEntityDeviceClass string            `json:"target_entity_device_class"`
	MediaPlayerCommands     []string          `json:"media_player_commands"`
}

// rel110Expected is the subset of REL-110's expected block this stage
// diffs the live Store/CommandSurface behavior against.
type rel110Expected struct {
	CandidateViewReplacesPriorView          bool              `json:"candidate_view_replaces_prior_view"`
	Results                                 []json.RawMessage `json:"results"`
	UnresolvedCommandAttemptedAgainstDevice bool              `json:"unresolved_command_attempted_against_device"`
}

func decodeRel110Input(c corpus.Case) (rel110Input, error) {
	raw, err := json.Marshal(c.Input)
	if err != nil {
		return rel110Input{}, fmt.Errorf("marshal corpus input: %w", err)
	}
	var in rel110Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return rel110Input{}, fmt.Errorf("unmarshal corpus input: %w", err)
	}
	return in, nil
}

func decodeRel110Expected(c corpus.Case) (rel110Expected, error) {
	raw, err := json.Marshal(c.Expected)
	if err != nil {
		return rel110Expected{}, fmt.Errorf("marshal corpus expected: %w", err)
	}
	var exp rel110Expected
	if err := json.Unmarshal(raw, &exp); err != nil {
		return rel110Expected{}, fmt.Errorf("unmarshal corpus expected: %w", err)
	}
	return exp, nil
}

// rel110CandidateWire is the device.candidates wire shape this stage
// decodes the corpus's device_candidates_report into, to drive it through
// Store's own public mutators (Observe/Adopt/Ignore) rather than by poking
// fields directly.
type rel110CandidateWire struct {
	RelayID string `json:"relay_id"`
	Body    struct {
		Candidates []struct {
			Match        json.RawMessage        `json:"match"`
			Provenance   deviceplane.Provenance `json:"provenance"`
			Status       deviceplane.Status     `json:"status"`
			IgnoredUntil *string                `json:"ignored_until"`
			FirstSeen    int64                  `json:"first_seen"`
			LastSeen     int64                  `json:"last_seen"`
		} `json:"candidates"`
	} `json:"body"`
}

// recordingController is a deviceplane.DeviceController fake that records
// every dispatch call it receives and always succeeds — the driver's
// stand-in for the physical ECP/Roku adapter, which is out of scope for
// REL-110/112/113's resolve→dispatch proof.
type recordingController struct {
	calls []recordedDispatch
}

type recordedDispatch struct {
	entityID, command string
	params            map[string]any
}

func (r *recordingController) Dispatch(entityID, command string, params map[string]any) error {
	r.calls = append(r.calls, recordedDispatch{entityID: entityID, command: command, params: params})
	return nil
}

// shapeDiff marshals got and compares it, as a generic JSON tree, to
// wantRaw — robust to field order/whitespace while asserting full shape and
// values. Returns nil when they match.
func shapeDiff(field string, got any, wantRaw json.RawMessage) *report.Diff {
	gotRaw, err := json.Marshal(got)
	if err != nil {
		return &report.Diff{Field: field, Expected: string(wantRaw), Actual: fmt.Sprintf("marshal error: %v", err)}
	}
	var gotTree, wantTree any
	if err := json.Unmarshal(gotRaw, &gotTree); err != nil {
		return &report.Diff{Field: field, Expected: string(wantRaw), Actual: fmt.Sprintf("re-decode error: %v", err)}
	}
	if err := json.Unmarshal(wantRaw, &wantTree); err != nil {
		return &report.Diff{Field: field, Expected: fmt.Sprintf("decode error: %v", err), Actual: string(gotRaw)}
	}
	if !reflect.DeepEqual(gotTree, wantTree) {
		return &report.Diff{Field: field, Expected: string(wantRaw), Actual: string(gotRaw)}
	}
	return nil
}

// driveREL110 drives the candidate full-set report (REL-110/111) and the
// device.command resolve→dispatch surface (REL-112/113) directly against a
// real deviceplane.Store and deviceplane.CommandSurface: the corpus's
// device_candidates_report is reproduced by driving Store through its own
// Observe/Adopt/Ignore mutators, then a fresh probe observation proves
// Report() replaces the prior view rather than accumulating a delta
// (REL-111); every dispatched command is executed through a real
// CommandSurface built on the corpus's own fixture vocabulary
// (registry.FixtureRegistry), diffing each device.command_result against
// the corpus's expected results and asserting an unresolved command never
// reaches the physical device (REL-113).
func driveREL110(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-110")
	if !ok {
		rep.Fail("REL-110", contract, "case not found in frozen corpus")
		return
	}

	in, err := decodeRel110Input(c)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus input: %v", err))
		return
	}
	exp, err := decodeRel110Expected(c)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus expected: %v", err))
		return
	}

	var diffs []report.Diff

	var wire rel110CandidateWire
	if err := json.Unmarshal(in.DeviceCandidatesReport, &wire); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus device_candidates_report: %v", err))
		return
	}

	store := deviceplane.NewStore(wire.RelayID)
	for _, cand := range wire.Body.Candidates {
		m, err := deviceplane.ParseMatch(cand.Match)
		if err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("parsing corpus candidate match %s: %v", cand.Match, err))
			return
		}
		store.Observe(m, cand.Provenance, cand.FirstSeen)
		if cand.LastSeen != cand.FirstSeen {
			store.Observe(m, cand.Provenance, cand.LastSeen)
		}
		switch cand.Status {
		case deviceplane.StatusAdopted:
			store.Adopt(m.Key())
		case deviceplane.StatusIgnored:
			store.Ignore(m.Key(), cand.IgnoredUntil)
		}
	}

	firstReport := store.Report()
	if d := shapeDiff("device_candidates_report", firstReport, in.DeviceCandidatesReport); d != nil {
		diffs = append(diffs, *d)
	}

	if !exp.CandidateViewReplacesPriorView {
		diffs = append(diffs, report.Diff{Field: "candidate_view_replaces_prior_view (corpus self-check)", Expected: true, Actual: false})
	} else {
		// Prove full-set replace, not accumulation (REL-111): observing one
		// brand-new candidate must grow the reported set by exactly one,
		// retaining every prior candidate — never a delta log.
		probe, err := deviceplane.ParseMatch(json.RawMessage(`{"macOui":"AABBCC"}`))
		if err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("parsing probe match: %v", err), diffs...)
			return
		}
		store.Observe(probe, deviceplane.ProvenanceDiscovered, 1)
		grown := store.Report()
		if len(grown.Body.Candidates) != len(firstReport.Body.Candidates)+1 {
			diffs = append(diffs, report.Diff{Field: "candidate_view_replaces_prior_view (grown-by-one probe, REL-111)", Expected: len(firstReport.Body.Candidates) + 1, Actual: len(grown.Body.Candidates)})
		}
		for _, want := range firstReport.Body.Candidates {
			found := false
			for _, got := range grown.Body.Candidates {
				if got.Match.Key() == want.Match.Key() {
					found = true
					break
				}
			}
			if !found {
				diffs = append(diffs, report.Diff{Field: "candidate_view_replaces_prior_view (prior candidate retained, REL-111)", Expected: want.Match.Key(), Actual: "missing from grown report"})
			}
		}
	}

	if len(in.Commands) != len(exp.Results) {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("corpus has %d commands but %d expected results", len(in.Commands), len(exp.Results)), diffs...)
		return
	}

	controller := &recordingController{}
	resolve := func(entityID string) (deviceID, deviceClass string, ok bool) {
		// The corpus dispatches every command against the one adopted
		// entity its commands name; anything else resolves to no device
		// class (REL-113's "no adopted entity" rejection path).
		for _, raw := range in.Commands {
			var cmd struct {
				Body struct {
					EntityID string `json:"entity_id"`
				} `json:"body"`
			}
			_ = json.Unmarshal(raw, &cmd)
			if cmd.Body.EntityID == entityID {
				return entityID, in.TargetEntityDeviceClass, true
			}
		}
		return "", "", false
	}
	surface := deviceplane.NewCommandSurface(controller, registry.FixtureRegistry{}, resolve)

	unresolvedInResults := 0
	for i, rawCmd := range in.Commands {
		var cmd deviceplane.DeviceCommand
		if err := json.Unmarshal(rawCmd, &cmd); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("decoding corpus command[%d]: %v", i, err), diffs...)
			return
		}

		var wantResult struct {
			Body struct {
				OK    bool `json:"ok"`
				Error *struct {
					Code string `json:"code"`
				} `json:"error"`
			} `json:"body"`
		}
		if err := json.Unmarshal(exp.Results[i], &wantResult); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("decoding expected result[%d]: %v", i, err), diffs...)
			return
		}
		unresolved := !wantResult.Body.OK && wantResult.Body.Error != nil && wantResult.Body.Error.Code == "COMMAND_UNRESOLVED"
		if unresolved {
			unresolvedInResults++
		}

		callsBefore := len(controller.calls)
		got := surface.Execute(cmd)
		if d := shapeDiff(fmt.Sprintf("device.command_result[%s]", cmd.ID), got, exp.Results[i]); d != nil {
			diffs = append(diffs, *d)
		}
		if unresolved && len(controller.calls) != callsBefore {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("unresolved_command_attempted_against_device[%s] (REL-113)", cmd.ID), Expected: false, Actual: true})
		}
	}

	if exp.UnresolvedCommandAttemptedAgainstDevice {
		diffs = append(diffs, report.Diff{Field: "unresolved_command_attempted_against_device (corpus self-check)", Expected: false, Actual: true})
	}
	if unresolvedInResults == 0 {
		diffs = append(diffs, report.Diff{Field: "expected results include at least one COMMAND_UNRESOLVED (REL-113)", Expected: ">=1", Actual: 0})
	}
	if len(controller.calls) != len(in.Commands)-unresolvedInResults {
		diffs = append(diffs, report.Diff{Field: "controller dispatch count (resolved commands only, REL-113)", Expected: len(in.Commands) - unresolvedInResults, Actual: len(controller.calls)})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "device-candidate + command diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}
