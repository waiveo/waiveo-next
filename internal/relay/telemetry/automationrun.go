package telemetry

import (
	"encoding/json"

	"github.com/maaxton/waiveo-next/internal/rules/engine"
)

// automationRunPayload is the events/1 automation.run payload shape this relay
// channel emits from an edge-rules RunDisposition (EVT-040/041). It carries the
// full set of fields EVT-040 marks mandatory — none optional: the rule id, the
// applied rule_revision, the firing trigger_snapshot, the condition_results and
// action_outcomes of the run, the mode-level disposition (ran/skipped/restarted
// — EVT-041), and misfire_caught (the orthogonal caught-up-misfire marker,
// EVT-041, never a fourth disposition value). The three context fields
// (trigger_snapshot, condition_results, action_outcomes) are carried as the
// firing's own events/1 JSON, byte-for-byte unmodified: this emitter does NOT
// redefine the schema (REL-095 — events/1 is its sole source), it forwards what
// the RunDisposition record carries so the delivered wire entry's payload is
// exactly that schema's own field shape (REL-090). The envelope's origin
// (EVT-042) is an envelope concern, not a payload field, and is set by the push
// assembler, not here.
type automationRunPayload struct {
	RuleID           string          `json:"rule_id"`
	RuleRevision     int             `json:"rule_revision"`
	TriggerSnapshot  json.RawMessage `json:"trigger_snapshot"`
	ConditionResults json.RawMessage `json:"condition_results"`
	ActionOutcomes   json.RawMessage `json:"action_outcomes"`
	ModeDisposition  string          `json:"mode_disposition"`
	MisfireCaught    bool            `json:"misfire_caught"`
}

// AutomationRunEntry maps the edge-rules engine's RunDisposition (RUL-246) to a
// telemetry entry's three Buffer.Record inputs: the schema name (always
// automation.run, a durable-class schema — REL-093, so the buffer retains it and
// never coalesces it), the events/1 automation.run payload (EVT-040/041 — all
// seven mandatory fields the RunDisposition carries), and the supersession
// subject.
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
// mode_disposition is enforced against EVT-041's closed {ran,skipped,restarted}
// enum via modeDispositionOf: engine.Canceled (the RUL-380 generation-swap
// teardown outcome, a real engine.Disposition value ApplyGeneration produces) has
// no automation.run representation and never arises from a trigger firing, so
// feeding it here is a wiring error caught loudly rather than an out-of-enum value
// emitted onto the durable wire.
func AutomationRunEntry(d engine.RunDisposition, atMs int64) (schema string, payload json.RawMessage, subject string) {
	_ = atMs
	p := automationRunPayload{
		RuleID:           d.RuleID,
		RuleRevision:     d.RuleRevision,
		TriggerSnapshot:  orEmptyObject(d.TriggerSnapshot),
		ConditionResults: orEmptyArray(d.ConditionResults),
		ActionOutcomes:   orEmptyArray(d.ActionOutcomes),
		ModeDisposition:  modeDispositionOf(d.Disposition),
		MisfireCaught:    d.MisfireCaught,
	}
	raw, err := json.Marshal(p)
	if err != nil {
		// Unreachable: automationRunPayload's scalars are JSON-safe and its three
		// json.RawMessage context fields were already valid events/1 JSON when the
		// firing path recorded them onto the RunDisposition.
		panic("telemetry: marshaling automation.run payload: " + err.Error())
	}
	return SchemaAutomationRun, json.RawMessage(raw), d.RuleID
}

// orEmptyObject returns raw when it carries a value, or an empty JSON object when
// it is absent. orEmptyArray does the same with an empty JSON array. They keep
// the emitted automation.run payload valid against events/1 EVT-040 — which
// requires trigger_snapshot to be an OBJECT and condition_results/action_outcomes
// to be ARRAYS (an empty array is explicitly valid) — for a firing whose
// RunDisposition does not yet carry the trigger/condition/action snapshot. A bare
// mode-evaluation firing leaves those three context fields nil (rules/1: the
// live-evaluation snapshot is assembled by a later part of the engine), and a nil
// json.RawMessage marshals to `null`, which is NOT a valid object/array and would
// be dropped at the app ingest's EVT-013 gate (never reaching a subscriber). This
// is not re-mapping the schema: it fills the mandatory EVT-040 fields with their
// valid-empty form so the mapper always emits all seven mandatory fields validly,
// and it is transparent once the engine populates real context — a non-empty
// snapshot is carried through byte-for-byte unchanged (REL-090/095).
func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

func orEmptyArray(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("[]")
	}
	return raw
}

// modeDispositionOf maps an engine.Disposition to its EVT-041 mode_disposition
// wire value, enforcing EVT-041's closed {ran,skipped,restarted} enum. Ran,
// Skipped, and Restarted are the mode-evaluation outcomes of a trigger firing.
// engine.Canceled is a real engine.Disposition value — the RUL-380 outcome
// recorded when a generation swap tears down an in-flight run — but it is NOT an
// automation.run mode_disposition: it does not arise from a firing and has no
// events/1 representation. Emitting it would put an out-of-enum value onto a
// durable wire entry (REL-090/095), so any disposition outside the closed set is
// a wiring error and panics rather than corrupting the stream.
func modeDispositionOf(d engine.Disposition) string {
	switch d {
	case engine.Ran, engine.Skipped, engine.Restarted:
		return string(d)
	default:
		panic("telemetry: automation.run mode_disposition outside EVT-041's closed {ran,skipped,restarted} enum: " + string(d))
	}
}
