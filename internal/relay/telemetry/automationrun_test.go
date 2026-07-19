package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/engine"
)

// This file (Task 5) gates the automation.run emitter: AutomationRunEntry maps
// the edge-rules engine's RunDisposition (RuleID/Disposition/Mode/MisfireCaught)
// to an events/1 automation.run payload (EVT-040/041 — mode_disposition
// ran/skipped/restarted, misfire_caught, the rule id), classified durable-class
// (REL-093) so the buffer retains it and never coalesces it. The expected
// mode_disposition/misfire_caught/rule_id are asserted against the frozen
// events/1 EVT-040/041 corpus documents, not invented — the emitter fills
// exactly the payload fields RunDisposition carries; the remaining events/1
// automation.run fields (rule_revision, trigger_snapshot, condition_results,
// action_outcomes, and the envelope's origin) are assembled by the events/1
// producer from the firing context and are out of this relay channel's scope
// (a deferred events/1 validator owns full-payload validation).

const (
	evt040Corpus          = "../../../conformance/corpora/events-1/EVT-040-valid-automation-run.json"
	evt041MisfireCorpus   = "../../../conformance/corpora/events-1/EVT-041-valid-automation-run-misfire-caught.json"
	evt041RestartedCorpus = "../../../conformance/corpora/events-1/EVT-041-valid-automation-run-restarted.json"
	evt041SkippedCorpus   = "../../../conformance/corpora/events-1/EVT-041-valid-automation-run-skipped-internal.json"
)

// evtRunCase is the events/1 automation.run corpus document this emitter oracle
// reads: the RunDisposition inputs it maps (rule_id, rule_revision, mode,
// disposition, misfire_caught, and the trigger/condition/action context the
// automation.run payload also reports) and the full expected envelope.payload —
// all seven EVT-040 mandatory fields, none optional. The three context fields are
// json.RawMessage so the exact events/1 JSON is compared as a tree, never
// redefined.
type evtRunCase struct {
	CaseID string `json:"case_id"`
	Input  struct {
		RuleID        string          `json:"rule_id"`
		RuleRevision  int             `json:"rule_revision"`
		Mode          string          `json:"mode"`
		Disposition   string          `json:"disposition"`
		MisfireCaught bool            `json:"misfire_caught"`
		Trigger       json.RawMessage `json:"trigger"`
		Conditions    json.RawMessage `json:"conditions"`
		Actions       json.RawMessage `json:"actions"`
	} `json:"input"`
	Expected struct {
		Envelope struct {
			Schema  string `json:"schema"`
			Payload struct {
				RuleID           string          `json:"rule_id"`
				RuleRevision     int             `json:"rule_revision"`
				TriggerSnapshot  json.RawMessage `json:"trigger_snapshot"`
				ConditionResults json.RawMessage `json:"condition_results"`
				ActionOutcomes   json.RawMessage `json:"action_outcomes"`
				ModeDisposition  string          `json:"mode_disposition"`
				MisfireCaught    bool            `json:"misfire_caught"`
			} `json:"payload"`
		} `json:"envelope"`
	} `json:"expected"`
}

func loadEvtRunCase(t *testing.T, path string) evtRunCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("reading events/1 automation.run corpus %s: %v", path, err)
	}
	var c evtRunCase
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return c
}

// dispositionFrom rebuilds the engine's RunDisposition from a corpus case's
// input — the exact struct the engine hands the emitter as a rule fires.
func dispositionFrom(c evtRunCase) engine.RunDisposition {
	return engine.RunDisposition{
		RuleID:           c.Input.RuleID,
		Disposition:      engine.Disposition(c.Input.Disposition),
		Mode:             c.Input.Mode,
		MisfireCaught:    c.Input.MisfireCaught,
		RuleRevision:     c.Input.RuleRevision,
		TriggerSnapshot:  c.Input.Trigger,
		ConditionResults: c.Input.Conditions,
		ActionOutcomes:   c.Input.Actions,
	}
}

// jsonEqual reports whether two JSON documents are equal as generic trees
// (order/whitespace insensitive). Unlike jsonTreeEqual's subset semantics, this
// asserts full equality — the emitted context field must match the corpus's
// expected payload exactly, no missing keys.
func jsonEqual(t *testing.T, got, want json.RawMessage) bool {
	t.Helper()
	var gt, wt any
	if err := json.Unmarshal(got, &gt); err != nil {
		t.Fatalf("decode got %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &wt); err != nil {
		t.Fatalf("decode want %s: %v", want, err)
	}
	return reflect.DeepEqual(gt, wt)
}

// TestAutomationRunEntryMapsEVT040_041 drives each frozen events/1 automation.run
// corpus case (EVT-040 ran, EVT-041 misfire-caught/restarted/skipped) through the
// emitter and asserts the produced schema + payload's rule_id/mode_disposition/
// misfire_caught match the corpus's own expected envelope.payload (EVT-040/041).
func TestAutomationRunEntryMapsEVT040_041(t *testing.T) {
	for _, path := range []string{evt040Corpus, evt041MisfireCorpus, evt041RestartedCorpus, evt041SkippedCorpus} {
		c := loadEvtRunCase(t, path)
		if c.Expected.Envelope.Schema != SchemaAutomationRun {
			t.Fatalf("%s: corpus envelope schema = %q, want %q", c.CaseID, c.Expected.Envelope.Schema, SchemaAutomationRun)
		}

		schema, payload, subject := AutomationRunEntry(dispositionFrom(c), 0)

		if schema != SchemaAutomationRun {
			t.Errorf("%s: schema = %q, want %q", c.CaseID, schema, SchemaAutomationRun)
		}
		// The emitted entry is durable-class (REL-093) — never coalesced.
		if class, ok := ClassOf(schema); !ok || class != Durable {
			t.Errorf("%s: ClassOf(%q) = (%v,%v), want (Durable,true)", c.CaseID, schema, class, ok)
		}
		if subject != c.Input.RuleID {
			t.Errorf("%s: subject = %q, want the rule id %q", c.CaseID, subject, c.Input.RuleID)
		}

		// All seven EVT-040 mandatory payload fields must be present and match the
		// corpus's own expected envelope.payload — none is optional, so a payload
		// missing rule_revision/trigger_snapshot/condition_results/action_outcomes
		// is non-conformant even though the corpus names the case "valid".
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(payload, &raw); err != nil {
			t.Fatalf("%s: payload not valid JSON: %v (%s)", c.CaseID, err, payload)
		}
		wantKeys := []string{"rule_id", "rule_revision", "trigger_snapshot", "condition_results", "action_outcomes", "mode_disposition", "misfire_caught"}
		for _, k := range wantKeys {
			if _, ok := raw[k]; !ok {
				t.Errorf("%s: payload missing EVT-040 mandatory field %q: %s", c.CaseID, k, payload)
			}
		}
		if len(raw) != len(wantKeys) {
			t.Errorf("%s: payload has %d fields, want exactly %d (EVT-040): %s", c.CaseID, len(raw), len(wantKeys), payload)
		}

		var got struct {
			RuleID          string `json:"rule_id"`
			RuleRevision    int    `json:"rule_revision"`
			ModeDisposition string `json:"mode_disposition"`
			MisfireCaught   bool   `json:"misfire_caught"`
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("%s: payload not valid JSON: %v (%s)", c.CaseID, err, payload)
		}
		if got.RuleID != c.Expected.Envelope.Payload.RuleID {
			t.Errorf("%s: rule_id = %q, want %q", c.CaseID, got.RuleID, c.Expected.Envelope.Payload.RuleID)
		}
		if got.RuleRevision != c.Expected.Envelope.Payload.RuleRevision {
			t.Errorf("%s: rule_revision = %d, want %d", c.CaseID, got.RuleRevision, c.Expected.Envelope.Payload.RuleRevision)
		}
		if got.ModeDisposition != c.Expected.Envelope.Payload.ModeDisposition {
			t.Errorf("%s: mode_disposition = %q, want %q", c.CaseID, got.ModeDisposition, c.Expected.Envelope.Payload.ModeDisposition)
		}
		if got.MisfireCaught != c.Expected.Envelope.Payload.MisfireCaught {
			t.Errorf("%s: misfire_caught = %v, want %v", c.CaseID, got.MisfireCaught, c.Expected.Envelope.Payload.MisfireCaught)
		}
		// The three context fields are carried through byte-for-byte, so compare
		// them as JSON trees against the corpus's own expected payload.
		if !jsonEqual(t, raw["trigger_snapshot"], c.Expected.Envelope.Payload.TriggerSnapshot) {
			t.Errorf("%s: trigger_snapshot = %s, want %s", c.CaseID, raw["trigger_snapshot"], c.Expected.Envelope.Payload.TriggerSnapshot)
		}
		if !jsonEqual(t, raw["condition_results"], c.Expected.Envelope.Payload.ConditionResults) {
			t.Errorf("%s: condition_results = %s, want %s", c.CaseID, raw["condition_results"], c.Expected.Envelope.Payload.ConditionResults)
		}
		if !jsonEqual(t, raw["action_outcomes"], c.Expected.Envelope.Payload.ActionOutcomes) {
			t.Errorf("%s: action_outcomes = %s, want %s", c.CaseID, raw["action_outcomes"], c.Expected.Envelope.Payload.ActionOutcomes)
		}
	}
}

// TestAutomationRunEntryRejectsNonEVT041Disposition proves the emitter enforces
// EVT-041's closed {ran,skipped,restarted} mode_disposition enum. engine.Canceled
// is a real engine.Disposition (the RUL-380 generation-swap teardown outcome
// ApplyGeneration produces) but has no automation.run representation; feeding it
// to the emitter is a wiring error that must be caught, never emitted as an
// out-of-enum "canceled" value onto the durable wire (REL-090/095, EVT-041).
func TestAutomationRunEntryRejectsNonEVT041Disposition(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("AutomationRunEntry(engine.Canceled) did not panic; an out-of-enum mode_disposition would reach the durable wire (EVT-041)")
		}
	}()
	AutomationRunEntry(engine.RunDisposition{
		RuleID:      "01J8Z3K4N5P6Q7R8S9T0V1W2YC",
		Disposition: engine.Canceled,
		Mode:        "restart",
	}, 0)
}

// TestAutomationRunEntryBuffersDurableNeverCoalesced proves the emitter's output
// is durable-class end-to-end: two automation.run entries for the SAME rule id
// (same subject) both survive in the buffer — a durable entry is never superseded
// (REL-093), unlike a latest-only heartbeat (REL-094).
func TestAutomationRunEntryBuffersDurableNeverCoalesced(t *testing.T) {
	ranC := loadEvtRunCase(t, evt040Corpus)
	skippedC := loadEvtRunCase(t, evt041SkippedCorpus)

	buf := NewBuffer(500)
	s1, p1, subj1 := AutomationRunEntry(dispositionFrom(ranC), 0)
	s2, p2, subj2 := AutomationRunEntry(dispositionFrom(skippedC), 0)
	buf.Record(s1, p1, subj1, 0)
	buf.Record(s2, p2, subj2, 0)

	pending := buf.Pending()
	if len(pending) != 2 {
		t.Fatalf("Pending = %d entries, want 2 (durable automation.run entries never coalesce, REL-093)", len(pending))
	}
	if pending[0].Seq != 1 || pending[1].Seq != 2 {
		t.Fatalf("seqs = [%d,%d], want [1,2] (monotonic, REL-091)", pending[0].Seq, pending[1].Seq)
	}
	if len(buf.PendingLossMarkers()) != 0 {
		t.Fatalf("PendingLossMarkers = %d, want 0 (durable retention is not loss)", len(buf.PendingLossMarkers()))
	}
}
