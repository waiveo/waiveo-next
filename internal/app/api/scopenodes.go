package api

import (
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

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
		// The three deletion refusals data-model/1 owns (DAT-020/021/022). The org
		// rule is decided from the row's own fields and refuses permanently, so it
		// rides the pre-precondition hook; the other two are cleared by writing
		// something else and are read out of the store inside the delete's own
		// transaction. See scopeNodeUndeletable / scopeNodeDeleteGuards below.
		undeletable:  scopeNodeUndeletable,
		deleteGuards: scopeNodeDeleteGuards,
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

// ---- deletion (data-model/1 DAT-020, DAT-021, DAT-022) --------------------
//
// A scope node is the platform's unit of placement: the tree hangs off it and
// every other row names it. Removing one therefore breaks two references nothing
// else in the write path notices.
//
// Neither is caught by the store's post-write revalidation, and the reason is
// worth stating because the shape invites the opposite assumption. Every write
// re-validates the full remaining row set, so the natural expectation is that an
// orphaned child fails revalidation and the delete rolls back. It does not:
// datamodel.BuildScopeTree is subtree-tolerant ON PURPOSE — a relay/1 snapshot
// carries a subtree, not the whole tree, so a node whose parent is absent is a
// subtree boundary rather than an error — and datamodel.ValidateRows checks a
// row's references to OTHER ROWS, never that its scope_node names a node that
// exists. Both are correct for what they do. They simply leave deletion
// unguarded, which is why these rules are stated here rather than assumed there.

// orgKind is the scope-node kind DAT-002 makes the tree's single root.
const orgKind = "org"

// scopeNodeUndeletable refuses a delete targeting the tree's org-kind node
// (DAT-022).
//
// It is decided from the row's own `kind` — no store read, no child count. That
// is DAT-022's own construction: the refusal holds "independent of DAT-020's own
// child-emptiness test", because deleting the org node "would destroy the tree's
// own root identity, not merely one node in it". So an org node with no children
// and nothing placed at it is refused exactly as a populated one is, and when
// both rules apply this one answers — it is checked in the handler, ahead of the
// precondition and ahead of the transaction the other two run in.
func scopeNodeUndeletable(f resourceFields) *deleteRefused {
	if f.Kind != orgKind {
		return nil
	}
	return &deleteRefused{
		Status: http.StatusConflict,
		Code:   "SCOPE_NODE_ORG_UNDELETABLE",
		Title:  "Conflict",
		Detail: "The tree's org-kind scope node cannot be deleted: it is the root every other node and row is placed under, not one node among them.",
	}
}

// scopeNodeDeleteGuards are the two referential rules a scope-node delete has to
// clear against the rest of the store, in the order the contract states them:
// DAT-020 (no child node still names it as parent_id) then DAT-021 (no row
// anywhere still names it as scope_node).
//
// Both run INSIDE the delete's transaction rather than as a pre-read in the
// handler, and that is not incidental. Each asks about rows OTHER writers can
// create: a caller could read "this node has no children", have a child created
// under it, and then commit the delete over the top — precisely the
// check-then-write race the external_id WriteGuard was moved into the
// transaction to close. Under the store's single-writer discipline, a guard
// reading through the transaction cannot be raced past.
func scopeNodeDeleteGuards(srv *server, id string) []store.DeleteGuard {
	return []store.DeleteGuard{
		func(v store.DeleteView) error {
			nodes, err := v.Rows(store.KindScopeNode)
			if err != nil {
				return err
			}
			for _, n := range nodes {
				// The target itself is skipped rather than assumed absent: DAT-003
				// permits a group under a group, so a node naming ITSELF as parent is
				// structurally valid to BuildScopeTree, and counting it as its own
				// child would make it undeletable forever.
				if n.ID == id {
					continue
				}
				if parseFields(n.Body).ParentID == id {
					return &deleteRefused{
						Status: http.StatusConflict,
						Code:   "SCOPE_NODE_NOT_EMPTY",
						Title:  "Conflict",
						Detail: "This scope node still carries at least one child scope node. Delete or re-parent every child first.",
					}
				}
			}
			return nil
		},
		func(v store.DeleteView) error {
			p, found, err := v.PlacedAt(id)
			if err != nil {
				return err
			}
			if !found {
				return nil
			}
			return &deleteRefused{
				Status: http.StatusConflict,
				Code:   "SCOPE_NODE_IN_USE",
				Title:  "Conflict",
				// The blocking thing is named as a ROW, and the sentence says so
				// outright, because the two nouns otherwise collide on the one
				// family where it matters most. Deleting a SCREEN-KIND SCOPE NODE
				// blocked by a SCREEN IDENTITY ROW would read as "this screen
				// cannot be deleted because a screen is placed at it" — and
				// DAT-004a makes those genuinely different rows with different
				// ids, a screen-kind node being "a placement classification,
				// never a screen identity in its own right". An operator who
				// reads them as one thing goes looking for the wrong record.
				Detail: "This scope node is still named as the scope_node of at least one other row" +
					srv.placedRowNoun(p.Table) + ". A row placed at a node is a separate record from the " +
					"node itself — delete or re-place every such row first.",
			}
		},
	}
}

// placedRowNoun renders " (a playlist row)" for the family a blocking row belongs
// to, or nothing when the table is not a mounted resource family — pack collection
// rows carry the same placement and are found by the same lookup, but they are
// not one of this surface's own families and have no displayName to borrow.
//
// The trailing "row" is not filler. Several displayNames are also the word an
// operator uses for a scope node of the matching kind — "screen" most of all — and
// DAT-004a makes a screen identity row and a screen-kind scope node distinct rows
// with distinct ids. Naming the blocker "a screen row" rather than "a screen"
// keeps the refusal from describing the node it is refusing to delete.
//
// The noun leaks nothing: every rule that reaches this point has already
// established the caller holds WRITE authority at the node, and write authority
// outranks read, so the caller could have listed the family for themselves.
func (srv *server) placedRowNoun(table string) string {
	for _, cfg := range srv.families {
		if string(cfg.kind) == table {
			return " (" + indefiniteArticle(cfg.displayName) + " " + cfg.displayName + " row)"
		}
	}
	return ""
}

// indefiniteArticle picks "a" or "an" for one of this surface's own display
// names. A leading-vowel test is the whole rule and it is sufficient here
// because the set it runs over is closed and visible — the displayName of every
// mounted family ("screen", "adopted device", "automation", ...) — not arbitrary
// English, where it would need a pronunciation dictionary.
func indefiniteArticle(noun string) string {
	if noun == "" {
		return "a"
	}
	switch noun[0] {
	case 'a', 'e', 'i', 'o', 'u', 'A', 'E', 'I', 'O', 'U':
		return "an"
	}
	return "a"
}
