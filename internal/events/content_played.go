package events

import "encoding/json"

// content.played (EVT-050) publishes one playback record for a screen: which
// asset played, on which screen, under which compiled program revision
// (EVT-052, the revision in force at t_start), over what window, why it
// occurred (cause), and how it ended (completion). Per EVT-051 it is emitted
// once t_end is known — this file only validates the payload's shape; when to
// emit it is a producer-side (schedule/player) concern.

// The default cost/retention classing for a content.played event, sourced here
// (co-located with the schema, EVT-050) so class.go indexes it without a second
// copy of the strings. The corpus pins neither for this schema; both are
// operational-telemetry-tier, matching the events/1 wire-shape examples and the
// events1 conformance driver's fixture class (content.played is a Durable-class
// telemetry schema on the relay channel, REL-093/095).
const (
	contentPlayedCostClass      = "telemetry"
	contentPlayedRetentionClass = "telemetry-standard"
)

// validCauses is the closed EVT-050 content.played.cause enum.
var validCauses = map[string]struct{}{
	"scheduled": {},
	"preempted": {},
	"fallback":  {},
	"manual":    {},
	"loop":      {},
}

// validCompletions is the closed EVT-050 content.played.completion enum.
var validCompletions = map[string]struct{}{
	"completed":   {},
	"interrupted": {},
	"skipped":     {},
}

// validateContentPlayed enforces the EVT-050 content.played field definition:
// asset_ref/program_revision are required strings; screen_id is a ULID;
// t_start/t_end are required int64 Timestamps; cause and completion are the
// closed EVT-050 enums; power_evidence is an optional object. A missing or
// out-of-enum field is an EVT-013 delivery-gate rejection.
func validateContentPlayed(raw json.RawMessage) error {
	m, err := payloadObject(raw, "content.played")
	if err != nil {
		return err
	}
	if err := requireStringField(m, "asset_ref"); err != nil {
		return err
	}
	if err := requireULIDField(m, "screen_id"); err != nil {
		return err
	}
	if err := requireStringField(m, "program_revision"); err != nil {
		return err
	}
	if _, err := requireInt64Field(m, "t_start"); err != nil {
		return err
	}
	if _, err := requireInt64Field(m, "t_end"); err != nil {
		return err
	}
	if _, err := requireEnumField(m, "cause", validCauses, "scheduled, preempted, fallback, manual, loop (EVT-050)"); err != nil {
		return err
	}
	if _, err := requireEnumField(m, "completion", validCompletions, "completed, interrupted, skipped (EVT-050)"); err != nil {
		return err
	}
	if err := optionalObjectField(m, "power_evidence"); err != nil {
		return err
	}
	return nil
}
