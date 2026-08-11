// Package telemetry implements the relay's durable telemetry buffer and
// upstream channel (contracts/relay-1.md REL-090-097, "Telemetry upstream" +
// "Loss marker"): a bounded, seq-ordered buffer of events/1 registered-schema
// entries, durable-class retention vs. latest-only supersession, drop-oldest
// overflow with exact loss-marker accounting, and a batch push/ack cursor
// channel to the app peer over an injectable upstream transport.
package telemetry

// Class distinguishes the two retention behaviors REL-093/094 assign to the
// five events/1 registered schemas this channel carries: Durable (retained
// and eventually delivered, or explicitly loss-marked — never silently
// coalesced) and LatestOnly (a buffered-undelivered entry may be discarded
// once a newer entry of the same schema+subject is recorded, with no loss).
//
// Durable is iota 0, so a zero-value Class defaults to the stricter,
// never-silently-drop behavior rather than to LatestOnly.
type Class int

const (
	// Durable is REL-093's retention class: content.played, automation.run,
	// and entity.state_changed. Every such entry MUST be retained and
	// eventually delivered, or explicitly loss-marked (Loss marker section);
	// it MUST NOT be silently coalesced or superseded by a later entry.
	Durable Class = iota
	// LatestOnly is REL-094's retention class: device.heartbeat and
	// box.vitals. A buffered-but-not-yet-delivered entry of either schema MAY
	// be discarded once a newer entry of the same schema and subject has been
	// recorded — that discard is not loss and is never counted in a loss
	// marker (REL-104).
	LatestOnly
)

// The six events/1 registered schemas this channel carries (REL-095) — the
// ones REL-093/094 name, plus screen.interaction, which is carried here for the
// reason REL-095 exists at all: the relay is where a viewer's press is OBSERVED
// (a player POSTs it to /player/v1/interaction), so the only way it reaches the
// app's durable event log is up this channel. events/1's own catalog
// additionally registers audit.event and variable.changed (EVT-020), but relay/1
// does not carry either over this channel, so ClassOf reports them unknown
// (ok=false).
const (
	// SchemaEntityStateChanged is events/1 EVT-030's schema name.
	SchemaEntityStateChanged = "entity.state_changed"
	// SchemaAutomationRun is events/1 EVT-040's schema name.
	SchemaAutomationRun = "automation.run"
	// SchemaContentPlayed is events/1 EVT-050's schema name.
	SchemaContentPlayed = "content.played"
	// SchemaScreenInteraction is events/1 EVT-055's schema name.
	SchemaScreenInteraction = "screen.interaction"
	// SchemaDeviceHeartbeat is events/1 EVT-060's schema name.
	SchemaDeviceHeartbeat = "device.heartbeat"
	// SchemaBoxVitals is events/1 EVT-070's schema name.
	SchemaBoxVitals = "box.vitals"
)

// schemaClass is the REL-093/094 classification table for exactly the six
// registered schemas this channel carries (REL-095).
//
// screen.interaction is DURABLE, and specifically not latest-only: EVT-056
// forbids coalescing two presses, because they are two facts about what a person
// did. A latest-only classing here would discard the earlier of two rapid
// presses before it was ever delivered — silently, and counted as no loss at all
// (REL-104) — which is precisely the under-report the event exists to prevent.
var schemaClass = map[string]Class{
	SchemaContentPlayed:      Durable,
	SchemaScreenInteraction:  Durable,
	SchemaAutomationRun:      Durable,
	SchemaEntityStateChanged: Durable,
	SchemaDeviceHeartbeat:    LatestOnly,
	SchemaBoxVitals:          LatestOnly,
}

// ClassOf reports schema's REL-093/094 retention Class. ok is false for any
// schema outside the six this channel carries (REL-095) — including
// events/1's own audit.event and variable.changed, and any pack-contributed
// schema (events/1 EVT-021's namespaced `<publisher>/<name>.<local-name>`
// form) — this channel defines no behavior for those.
func ClassOf(schema string) (class Class, ok bool) {
	class, ok = schemaClass[schema]
	return class, ok
}
