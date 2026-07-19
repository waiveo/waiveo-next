package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// evtRunCase is the subset of an events/1 automation.run corpus document this
// emitter oracle reads: the RunDisposition inputs it maps (rule_id, mode,
// disposition, misfire_caught) and the expected payload's own derivable fields.
type evtRunCase struct {
	CaseID string `json:"case_id"`
	Input  struct {
		RuleID        string `json:"rule_id"`
		Mode          string `json:"mode"`
		Disposition   string `json:"disposition"`
		MisfireCaught bool   `json:"misfire_caught"`
	} `json:"input"`
	Expected struct {
		Envelope struct {
			Schema  string `json:"schema"`
			Payload struct {
				RuleID          string `json:"rule_id"`
				ModeDisposition string `json:"mode_disposition"`
				MisfireCaught   bool   `json:"misfire_caught"`
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
		RuleID:        c.Input.RuleID,
		Disposition:   engine.Disposition(c.Input.Disposition),
		Mode:          c.Input.Mode,
		MisfireCaught: c.Input.MisfireCaught,
	}
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

		var got struct {
			RuleID          string `json:"rule_id"`
			ModeDisposition string `json:"mode_disposition"`
			MisfireCaught   bool   `json:"misfire_caught"`
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("%s: payload not valid JSON: %v (%s)", c.CaseID, err, payload)
		}
		if got.RuleID != c.Expected.Envelope.Payload.RuleID {
			t.Errorf("%s: rule_id = %q, want %q", c.CaseID, got.RuleID, c.Expected.Envelope.Payload.RuleID)
		}
		if got.ModeDisposition != c.Expected.Envelope.Payload.ModeDisposition {
			t.Errorf("%s: mode_disposition = %q, want %q", c.CaseID, got.ModeDisposition, c.Expected.Envelope.Payload.ModeDisposition)
		}
		if got.MisfireCaught != c.Expected.Envelope.Payload.MisfireCaught {
			t.Errorf("%s: misfire_caught = %v, want %v", c.CaseID, got.MisfireCaught, c.Expected.Envelope.Payload.MisfireCaught)
		}
	}
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
