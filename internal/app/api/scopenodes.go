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
	}
}
