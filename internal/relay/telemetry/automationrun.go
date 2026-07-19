package telemetry

import (
	"encoding/json"

	"github.com/maaxton/waiveo-next/internal/rules/engine"
)

// automationRunPayload is the events/1 automation.run payload shape this relay
// channel emits from an edge-rules RunDisposition (EVT-040/041). It carries the
// fields a RunDisposition determines: the rule id, the mode-level disposition
// (ran/skipped/restarted — EVT-041), and misfire_caught (the orthogonal caught-up
// -misfire marker, EVT-041, never a fourth disposition value). The remaining
// events/1 automation.run fields — rule_revision, trigger_snapshot,
// condition_results, action_outcomes, and the envelope's origin (EVT-042) — are
// assembled by the events/1 producer from the firing's full context, not from the
// engine's disposition record; this emitter fills only what the RunDisposition
// carries and does NOT redefine the schema (REL-095 — events/1 is its sole source).
type automationRunPayload struct {
	RuleID          string `json:"rule_id"`
	ModeDisposition string `json:"mode_disposition"`
	MisfireCaught   bool   `json:"misfire_caught"`
}

// AutomationRunEntry maps the edge-rules engine's RunDisposition (RUL-246 —
// RuleID/Disposition/Mode/MisfireCaught) to a telemetry entry's three
// Buffer.Record inputs: the schema name (always automation.run, a durable-class
// schema — REL-093, so the buffer retains it and never coalesces it), the
// events/1 automation.run payload (EVT-040/041 — mode_disposition from the
// disposition, misfire_caught, the rule id), and the supersession subject.
//
// The subject is the rule id: automation.run is durable-class, so subject never
// drives a supersession discard (that is a latest-only behavior, REL-094) — it is
// the entry's natural identity, carried only as Buffer bookkeeping and never as
// part of the {seq, schema, payload} wire shape (REL-090). d.Mode is engine
// evaluation state, not an events/1 automation.run payload field, so it is not
// emitted. atMs is the relay's record-time wall reading, reserved for future
// retention/timestamp bookkeeping (Buffer.Record establishes recording order),
// and is accepted for a stable signature but not yet part of the payload.
//
// A malformed payload can only arise from an unmarshalable disposition string,
// which the closed engine.Disposition type precludes; json.Marshal of the fixed
// three-field struct therefore never errors, and the payload is returned directly.
func AutomationRunEntry(d engine.RunDisposition, atMs int64) (schema string, payload json.RawMessage, subject string) {
	_ = atMs
	p := automationRunPayload{
		RuleID:          d.RuleID,
		ModeDisposition: string(d.Disposition),
		MisfireCaught:   d.MisfireCaught,
	}
	raw, err := json.Marshal(p)
	if err != nil {
		// Unreachable: automationRunPayload is three JSON-safe scalar fields.
		panic("telemetry: marshaling automation.run payload: " + err.Error())
	}
	return SchemaAutomationRun, json.RawMessage(raw), d.RuleID
}
