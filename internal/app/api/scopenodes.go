package api

import "github.com/maaxton/waiveo-next/internal/app/store"

// scopeNodesConfig is the resource configuration for the scope-node tree
// (openapi exemplar #1). A scope node's own placement — what a selector's
// `scope_node` / `scope_node subtree` term evaluates against — is the node
// itself (its own id), so a subtree selector selects a node and its descendants.
// Its external_id uniqueness is scoped by its PARENT (API-101: two nodes may
// share an external_id only under different parents), and its reserved intrinsic
// `kind` (org/site/group/screen) is exposed as a selectable label so
// `selector=kind=site` filters the tree by node kind without conflating it with
// a user-set label.
func scopeNodesConfig() resourceConfig {
	return resourceConfig{
		kind:         store.KindScopeNode,
		path:         "scope-nodes",
		resourceType: "scope-nodes",
		displayName:  "scope node",
		selLabels: func(f resourceFields) map[string]string {
			m := make(map[string]string, len(f.Labels)+1)
			for k, v := range f.Labels {
				m[k] = v
			}
			if f.Kind != "" {
				m["kind"] = f.Kind
			}
			return m
		},
		placement: func(f resourceFields) string { return f.ID },
		extScope:  func(f resourceFields) string { return f.ParentID },
		// A node is placed under its PARENT, so that is where authority over
		// placing it (or re-parenting it) comes from — never its own id, which on
		// a create is a ULID the tree has never seen and which would therefore
		// resolve only through the workspace-root fallback.
		writeScope: func(f resourceFields) string { return f.ParentID },
		// This family names NO createSchema/updateSchema, and that is deliberate
		// rather than an omission — see bodyschema.go's "What this does NOT cover".
		// A scope node's body is already validated field-by-field by
		// datamodel.BuildScopeTree inside the store write, which reports EVERY
		// failing member at once with data-model/1's own published per-field codes
		// (SCOPE_NODE_NAME_INVALID, SCOPE_NODE_KIND_INVALID, ...) in API-013's
		// multi-field `errors[]` shape — the exact shape api/1's own
		// API-013-valid-multi-field-validation-problem corpus case pins. A
		// fail-fast schema gate ahead of it would replace that richer, published,
		// corpus-pinned answer with a poorer one.
	}
}
