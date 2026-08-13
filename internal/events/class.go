package events

// class.go exposes the EVT-010 cost_class/retention_class classification a
// registered schema carries, as the single in-code source of truth an ingester
// (internal/app/eventingest) reads when it reconstructs a wire record into a
// full envelope. The concrete vocabulary is maintained outside events/1
// (EVT-050/051), so the values live with each schema's own constructor (e.g.
// automation_run.go) — this file only indexes those definitions by schema name
// so an ingester assigns exactly the class the schema's own AutomationRunEnvelope
// would, and the two cannot drift.

// schemaClass pairs a registered schema's cost_class and retention_class.
type schemaClass struct {
	cost      string
	retention string
}

// classBySchema maps each registered schema whose class this codebase pins to
// its {cost_class, retention_class}, sourced from the schema file's own class
// constants (no second copy of the strings). It covers the five schemas the
// relay's telemetry channel carries (relay/1 REL-095) — the ones an ingester
// (internal/app/eventingest) reconstructs into envelopes — so every telemetry
// record reaches its payload validator and is classed, never dropped merely for
// lacking a class. audit.event is classed here too, though relay/1 does not
// relay it: it has an app-side producer (internal/app/auth emits one per
// security-model/1 SEC-150 flow), and its class is deliberately the long-lived
// one EVT-082 requires rather than the telemetry tier. variable.changed is
// classed here for the same reason and on the same tier: it now HAS an app-side
// producer (internal/app/api/variables.go emits one per committed variable
// write, data-model/1 DAT-137), and because its old_value makes this log the
// only surviving record of a variable's previous value — the row holds one
// value at a time and there is deliberately no second history table. A
// pack-namespaced schema stays unclassed
// (EVT-021/022). For an absent schema ClassFor reports ok=false, so a producer
// drops+logs a record it cannot class rather than minting an envelope with an
// empty, un-validatable class (EVT-013).
var classBySchema = map[string]schemaClass{
	SchemaAutomationRun:      {cost: automationRunCostClass, retention: automationRunRetentionClass},
	SchemaContentPlayed:      {cost: contentPlayedCostClass, retention: contentPlayedRetentionClass},
	SchemaScreenInteraction:  {cost: screenInteractionCostClass, retention: screenInteractionRetentionClass},
	SchemaEntityStateChanged: {cost: entityStateChangedCostClass, retention: entityStateChangedRetentionClass},
	SchemaDeviceHeartbeat:    {cost: deviceHeartbeatCostClass, retention: deviceHeartbeatRetentionClass},
	SchemaBoxVitals:          {cost: boxVitalsCostClass, retention: boxVitalsRetentionClass},
	SchemaAuditEvent:         {cost: auditEventCostClass, retention: auditEventRetentionClass},
	SchemaVariableChanged:    {cost: variableChangedCostClass, retention: variableChangedRetentionClass},
}

// ClassFor returns the cost_class and retention_class the platform assigns an
// event of the given registered schema (EVT-010). ok is false when no class is
// pinned for schema — an unclassed registered schema (producer not yet wired) or
// a pack-namespaced schema — so a caller never builds an envelope with an empty
// class that Validate would reject.
func ClassFor(schema string) (costClass, retentionClass string, ok bool) {
	if c, found := classBySchema[schema]; found {
		return c.cost, c.retention, true
	}
	// A PACK-CONTRIBUTED schema is classed by its shape rather than by this map.
	// EVT-021 makes the two namespaces mutually exclusive by construction — a
	// registered schema is a bare `<domain>.<name>` and a pack-contributed one is
	// `<publisher>/<name>.<local>`, which always carries the `/` from its owning
	// pack id — so the presence of a slash IS the discriminator, not a heuristic.
	//
	// This map cannot enumerate them: the set is whatever packs are installed,
	// which is deployment state and not something a compiled-in table can know.
	// Leaving them unclassed meant every pack event was dropped at its producer,
	// so `contributes.automation.events` was a declaration with no way to fire —
	// the automation side already anticipates the pack-namespaced name.
	if IsPackSchemaName(schema) {
		return packEventCostClass, packEventRetentionClass, true
	}
	return "", "", false
}

// The discriminator is IsPackSchemaName (validate.go), not a second copy of the
// rule: two functions deciding what "pack-namespaced" means is how they end up
// disagreeing, and this one already had an owner.

// The class every pack-contributed event carries.
//
// The TELEMETRY tier, deliberately, and not the audit tier. A pack's event
// volume is bounded by nothing this platform controls — a badly written
// extension can emit on a loop — so putting it on the long-lived tier would let
// one pack fill the disk with records the platform has promised to keep. An
// extension that needs an event kept for years is asking for an audit record,
// which is a first-party concern with a first-party producer.
//
// One class for all packs rather than a per-pack choice: letting a pack pick its
// own retention is letting it pick how much of the operator's disk it may have.
const (
	packEventCostClass      = "telemetry"
	packEventRetentionClass = "telemetry-standard"
)
