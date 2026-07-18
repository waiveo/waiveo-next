package state

// Snapshot is a point-in-time view of every relay-visible entity, keyed by
// entity ID. A structured condition's own EntityRef is evaluable against ANY
// relay-visible entity, not only the entity that fired the rule's trigger
// (RUL-101) — Snapshot is how condition evaluation resolves that reference.
type Snapshot map[string]Entity

// Get looks up an entity by ID, reporting ok=false when the ID is not present
// in the snapshot. An unresolved EntityRef is fail-closed (not-matched, per
// RUL-262/284's posture), never an error — every condition leaf that consults
// a Snapshot treats a false ok as "condition does not pass".
func (s Snapshot) Get(id string) (Entity, bool) {
	e, ok := s[id]
	return e, ok
}
