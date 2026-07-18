package expr

// SourceEntities returns the entity IDs a pipeline Expression's state/attr
// sources reference (RUL-281) — the exact set the edge cross-entity restriction
// (RUL-282) constrains. A literal Expression, a `now()` source, or a
// source with no literal entity-id argument contributes nothing. Because
// state/attr may appear only in the source position (RUL-292), a pipeline
// references at most the single entity fixed by its source stage; the slice is
// nil when there is no entity reference.
func SourceEntities(e Expr) []string {
	if e.Pipeline == nil {
		return nil
	}
	c := e.Pipeline.Source.Call
	if c == nil || (c.Name != "state" && c.Name != "attr") || len(c.Args) == 0 {
		return nil
	}
	id, ok := c.Args[0].Value.(string)
	if !ok {
		return nil
	}
	return []string{id}
}
