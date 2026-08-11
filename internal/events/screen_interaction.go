package events

import "encoding/json"

// screen.interaction (EVT-055) publishes ONE viewer interaction with a screen:
// a press the person standing in front of the panel made on an interactive
// element of the content it was showing (data-model/1 DAT-043's slide-layer
// `ping_name`, drawn by the player as a focusable region and posted back on OK).
//
// It is the only registered schema whose subject is a HUMAN ACTION rather than a
// machine observation, and the only one whose causal direction runs screen →
// platform. Every other event in this catalog reports something the platform
// already knew it had asked for; this one reports something only the room can
// tell it. That is why the payload names both WHAT was pressed (`interaction`,
// the authored element name an automation matches on) and WHAT WAS ON SCREEN at
// the time (`lease_id`, and `slide_id` when the content carried one): without
// the second pair, a press is attributable to a screen but not to the content
// that solicited it, and "the lobby call button was pressed" cannot be told from
// "the lobby screen was showing something else entirely and a stale element
// fired".
//
// `screen_id` is resolved by the relay from the presented channel credential and
// is never a self-asserted field of the request body — see
// internal/relay/playerserver's own interaction handler. A payload that could
// name any screen would let one paired screen manufacture another's interactions.

// The default cost/retention classing for a screen.interaction event, sourced
// here (co-located with the schema, EVT-055) so class.go indexes it without a
// second copy of the strings. EVT-056 fixes the retention tier at the DURABLE
// telemetry one and forbids coalescing: two presses are two facts about what a
// person did, and a latest-only tier would silently merge them — under-reporting
// the one thing this event exists to record.
const (
	screenInteractionCostClass      = "telemetry"
	screenInteractionRetentionClass = "telemetry-standard"
)

// SchemaScreenInteraction is events/1 EVT-055's schema name.
const SchemaScreenInteraction = "screen.interaction"

// validateScreenInteraction enforces the EVT-055 screen.interaction field
// definition: screen_id is a ULID; interaction and lease_id are required
// non-empty strings; at is a required int64 Timestamp; slide_id is an optional
// string.
//
// `interaction` is checked for presence only, never normalized — EVT-057 makes
// the verbatim-carriage rule explicit, because an automation matches this value
// exactly (rules/1 RUL-081) and any hop that trims or case-folds it makes a rule
// that reads correctly never fire. The producer-side grammar that keeps the
// value matchable in the first place is wire.ValidPingName, applied where the
// element is AUTHORED; re-deriving it here would put a second copy of that
// grammar on the delivery path, where tightening it later would start dropping
// already-authored events.
func validateScreenInteraction(raw json.RawMessage) error {
	m, err := payloadObject(raw, "screen.interaction")
	if err != nil {
		return err
	}
	if err := requireULIDField(m, "screen_id"); err != nil {
		return err
	}
	if err := requireStringField(m, "interaction"); err != nil {
		return err
	}
	if err := requireStringField(m, "lease_id"); err != nil {
		return err
	}
	if _, err := requireInt64Field(m, "at"); err != nil {
		return err
	}
	return optionalStringField(m, "slide_id")
}
