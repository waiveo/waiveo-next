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
// constants (no second copy of the strings). A registered schema whose producer
// is not yet wired — and any pack-namespaced schema (EVT-021/022) — is
// deliberately absent: ClassFor reports ok=false for it, so an ingester
// drops+logs a record it cannot class rather than minting an envelope with an
// empty, un-validatable class (EVT-013).
var classBySchema = map[string]schemaClass{
	SchemaAutomationRun: {cost: automationRunCostClass, retention: automationRunRetentionClass},
}

// ClassFor returns the cost_class and retention_class the platform assigns an
// event of the given registered schema (EVT-010). ok is false when no class is
// pinned for schema — an unclassed registered schema (producer not yet wired) or
// a pack-namespaced schema — so a caller never builds an envelope with an empty
// class that Validate would reject.
func ClassFor(schema string) (costClass, retentionClass string, ok bool) {
	c, ok := classBySchema[schema]
	if !ok {
		return "", "", false
	}
	return c.cost, c.retention, true
}
