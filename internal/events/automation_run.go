package events

import "encoding/json"

// automation.run (EVT-040) publishes one automation-rule evaluation's outcome.
// It is the typed projection of the rules engine's run disposition: the same
// schema serves an edge (relay) evaluation and an app-side (internal) one,
// distinguished only by the envelope's origin (EVT-042). mode_disposition is a
// closed enum {ran,skipped,restarted} (EVT-041); misfire_caught is an ORTHOGONAL
// marker of a firing's own origin, never a fourth disposition value — a caught
// misfire still resolves to ran/skipped/restarted on its own merits (rules/1
// RUL-246). This file is a leaf: it maps the engine outcome from plain values in
// AutomationRunEnvelope and never imports internal/rules, so the event catalog
// and the engine's disposition cannot drift.

// The default cost/retention classing for an automation.run event. The corpus
// pins neither for this schema; both are operational-telemetry-tier, matching
// the other change-feed schemas.
const (
	automationRunCostClass      = "telemetry"
	automationRunRetentionClass = "telemetry-standard"
)

// validModeDispositions is the closed EVT-041 mode_disposition enum. misfire is
// deliberately NOT a member — it is orthogonal (EVT-041/RUL-246).
var validModeDispositions = map[string]struct{}{
	"ran":       {},
	"skipped":   {},
	"restarted": {},
}

// automationRunPayload is the EVT-040 automation.run payload shape used to
// MARSHAL a payload in AutomationRunEnvelope. The trigger/condition/action
// members are carried verbatim as raw JSON so the constructor never reshapes the
// engine's own snapshot.
type automationRunPayload struct {
	RuleID           string          `json:"rule_id"`
	RuleRevision     int             `json:"rule_revision"`
	TriggerSnapshot  json.RawMessage `json:"trigger_snapshot"`
	ConditionResults json.RawMessage `json:"condition_results"`
	ActionOutcomes   json.RawMessage `json:"action_outcomes"`
	ModeDisposition  string          `json:"mode_disposition"`
	MisfireCaught    bool            `json:"misfire_caught"`
}

// validateAutomationRun enforces the EVT-040 automation.run field definition:
// rule_id is a ULID; rule_revision is an integer; trigger_snapshot is an object;
// condition_results and action_outcomes are arrays (an empty array is valid, as
// a skipped occurrence has no outcomes); mode_disposition is the closed EVT-041
// enum; and misfire_caught is a required boolean, orthogonal to the disposition
// (never a fourth disposition value, RUL-246).
func validateAutomationRun(raw json.RawMessage) error {
	m, err := payloadObject(raw, "automation.run")
	if err != nil {
		return err
	}
	if err := requireULIDField(m, "rule_id"); err != nil {
		return err
	}
	if _, err := requireIntField(m, "rule_revision"); err != nil {
		return err
	}
	if err := requireObjectField(m, "trigger_snapshot"); err != nil {
		return err
	}
	if err := requireArrayField(m, "condition_results"); err != nil {
		return err
	}
	if err := requireArrayField(m, "action_outcomes"); err != nil {
		return err
	}
	md, ok := m["mode_disposition"]
	if !ok {
		return ValidationError{Field: "mode_disposition", Detail: "required"}
	}
	var disposition string
	if json.Unmarshal(md, &disposition) != nil {
		return ValidationError{Field: "mode_disposition", Detail: "must be a string"}
	}
	if _, ok := validModeDispositions[disposition]; !ok {
		return ValidationError{Field: "mode_disposition", Detail: "must be one of ran, skipped, restarted (EVT-041)"}
	}
	if _, err := requireBoolField(m, "misfire_caught"); err != nil {
		return err
	}
	return nil
}

// AutomationRunEnvelope builds a durable-event envelope for one automation-rule
// evaluation, mapping the rules-engine outcome from plain values (never engine
// types — internal/events is a leaf). It derives origin from the evaluator —
// relay for an edge-classified rule, internal for any app-side evaluation
// (EVT-042) — carries the disposition verbatim onto mode_disposition, and passes
// misfire_caught through unchanged as its orthogonal marker (EVT-041/RUL-246).
// This constructor is the anti-drift seam between the event catalog and the
// engine's RunDisposition.
func AutomationRunEnvelope(scopeNode, traceID, ruleID string, ruleRevision int, trigger, conditions, actions json.RawMessage, disposition string, misfireCaught bool, evaluator string, id string, ts int64) Envelope {
	origin := "internal"
	if evaluator == "relay" {
		origin = "relay"
	}
	payload, _ := json.Marshal(automationRunPayload{
		RuleID:           ruleID,
		RuleRevision:     ruleRevision,
		TriggerSnapshot:  trigger,
		ConditionResults: conditions,
		ActionOutcomes:   actions,
		ModeDisposition:  disposition,
		MisfireCaught:    misfireCaught,
	})
	return Envelope{
		ID:             id,
		Schema:         SchemaAutomationRun,
		TS:             ts,
		ScopeNode:      scopeNode,
		TraceID:        traceID,
		CostClass:      automationRunCostClass,
		RetentionClass: automationRunRetentionClass,
		Origin:         origin,
		Payload:        payload,
	}
}
