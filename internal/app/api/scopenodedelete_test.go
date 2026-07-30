package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// scopenodedelete_test.go drives data-model/1's three scope-node deletion
// refusals through the LIVE handler — DAT-020 (a node still carrying a child),
// DAT-021 (a node still named as another row's scope_node) and DAT-022 (the
// tree's org node, which no ordinary write may remove at all).
//
// Each case asserts the PUBLISHED code, not merely a non-2xx. A delete can fail
// for four reasons that all look alike from a status line — the id is out of
// reach, the caller may not write there, the If-Match is missing or stale, or one
// of these three rules refuses — and a refusal for the wrong reason reads exactly
// like the right one. The ordering case at the bottom pins which answer wins when
// several apply at once.

// orgNode is a scope-node create body for the tree's single org-kind root: no
// parent, and none of the three geo columns (DAT-032 forbids them on an org).
func orgNode(name string) datamodel.ScopeNode {
	return datamodel.ScopeNode{Kind: "org", Name: name}
}

// TestDeleteScopeNodeCarryingAChildIsRefused: DAT-020. A site with a screen
// under it cannot be deleted; delete the child first and the same request
// succeeds, so the refusal is about the child rather than about the node.
func TestDeleteScopeNodeCarryingAChildIsRefused(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))
	screenID := e.createNode(t, screenNode("", siteID, ""))

	resp, raw := e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+siteID, nil, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete of a node carrying a child = %d, want 409 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "SCOPE_NODE_NOT_EMPTY")

	// Nothing was removed — neither the node nor, obviously, its child.
	if resp, _ := e.do(t, http.MethodGet, "/api/v1/scope-nodes/"+siteID, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the refused node is gone: GET = %d, want 200", resp.StatusCode)
	}

	// Re-parenting or deleting every child is what clears it (DAT-020's own
	// remedy), and the site's revision is untouched by the refusal, so the SAME
	// If-Match still applies.
	if resp, raw := e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+screenID, nil, map[string]string{"If-Match": `"1"`}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete of the child = %d, want 204 (body %s)", resp.StatusCode, raw)
	}
	if resp, raw := e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+siteID, nil, map[string]string{"If-Match": `"1"`}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete of the now-empty node = %d, want 204 (body %s)", resp.StatusCode, raw)
	}
}

// TestDeleteScopeNodeInUseIsRefused: DAT-021, once per authored family that can
// be placed at a node. A screen row and a device row are the two DAT-004
// identity rows the requirement names outright; a playlist and a schedule are
// scheduling-core rows; an automation and a webhook endpoint are neither, and
// are here because they carry the same scope_node in the same role and would
// dangle identically.
func TestDeleteScopeNodeInUseIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body func(t *testing.T, node string) []byte
		// noun is the family the refusal's detail must name, article and all, so
		// an operator is told WHAT is still there rather than only that something
		// is. It is spelled out per case rather than derived, so the derivation
		// cannot mark its own homework.
		noun string
	}{
		{"screen row", "/api/v1/screens", func(t *testing.T, n string) []byte {
			return mustJSON(t, screenFixture(n, "Lobby Screen", nil))
		}, "(a screen row)"},
		{"adopted device", "/api/v1/adopted-devices", func(t *testing.T, n string) []byte {
			return mustJSON(t, adoptedDeviceFixture(n, "Lobby Roku", "roku-ecp", "10.0.0.41", nil))
		}, "(an adopted device row)"},
		{"playlist", "/api/v1/playlists", func(t *testing.T, n string) []byte {
			return mustJSON(t, playlistFixture(n, nil))
		}, "(a playlist row)"},
		{"schedule", "/api/v1/schedules", func(t *testing.T, n string) []byte {
			return mustJSON(t, scheduleFixture(n))
		}, "(a schedule row)"},
		{"automation", "/api/v1/automations", func(_ *testing.T, n string) []byte {
			return edgeAutomationBody("", n, nil)
		}, "(an automation row)"},
		{"webhook endpoint", "/api/v1/webhook-endpoints", func(t *testing.T, n string) []byte {
			return mustJSON(t, map[string]any{
				"name": "Ops Endpoint", "scope_node": n, "url": "https://hooks.example.invalid/waiveo",
			})
		}, "(a webhook endpoint row)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			siteID := e.createNode(t, siteNode(""))
			screenNodeID := e.createNode(t, screenNode("", siteID, ""))
			e.uploadContent(t, playlistFixtureAsset)
			rowID := decodeID(t, e.createOK(t, tc.path, tc.body(t, screenNodeID)))

			resp, raw := e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+screenNodeID, nil, map[string]string{"If-Match": `"1"`})
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("delete of a node a %s is placed at = %d, want 409 (body %s)", tc.name, resp.StatusCode, raw)
			}
			p := assertProblem(t, resp, raw, "SCOPE_NODE_IN_USE")
			if detail, _ := p["detail"].(string); !strings.Contains(detail, tc.noun) {
				t.Fatalf("refusal detail %q does not name %s", detail, tc.noun)
			}

			// Remove the row and the node deletes cleanly: the reference was the
			// whole of the refusal.
			if resp, raw := e.do(t, http.MethodDelete, tc.path+"/"+rowID, nil, map[string]string{"If-Match": `"1"`}); resp.StatusCode != http.StatusNoContent {
				t.Fatalf("delete of the %s = %d, want 204 (body %s)", tc.name, resp.StatusCode, raw)
			}
			if resp, raw := e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+screenNodeID, nil, map[string]string{"If-Match": `"1"`}); resp.StatusCode != http.StatusNoContent {
				t.Fatalf("delete of the now-unreferenced node = %d, want 204 (body %s)", resp.StatusCode, raw)
			}
		})
	}
}

// TestDeleteScopeNodeAPackRowIsPlacedAtIsRefused: DAT-021 for the one kind of
// row that is not one of this surface's own families. A pack's collection rows
// carry the same `scope_node` in the same role — the pack-data surface authorizes
// and visibility-filters them by it exactly as it does every other row — so a node
// one of them sits under is in use, whoever declared the collection.
//
// It gets its own case rather than a row in the table above because pack_rows is
// the single entry in the placement scan that is NOT derived from the resource
// kinds: it is named by hand, and it identifies its rows by `entity_id` rather
// than `id`. Both are exactly the sort of thing that is written once and never
// exercised.
func TestDeleteScopeNodeAPackRowIsPlacedAtIsRefused(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))
	nodeID := e.createNode(t, screenNode("", siteID, ""))
	e.installDataPack(t)

	resp, row := e.createRow(t, map[string]any{"scope_node": nodeID, "name": "Burger", "price": 9.5}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create pack row = %d (%+v)", resp.StatusCode, row)
	}

	resp, raw := e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+nodeID, nil, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete of a node a pack row is placed at = %d, want 409 (body %s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "SCOPE_NODE_IN_USE")
	// No family noun: a pack collection is not one of this surface's own
	// resource families, so the refusal names the rule and stops there.
	if detail, _ := p["detail"].(string); strings.Contains(detail, "(") {
		t.Fatalf("refusal detail %q names a resource family for a pack row, which has none", detail)
	}

	entityID, _ := row["entity_id"].(string)
	if resp, raw := e.do(t, http.MethodDelete, menuRowsPath+"/"+entityID, nil, map[string]string{"If-Match": `"1"`}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete of the pack row = %d, want 204 (body %s)", resp.StatusCode, raw)
	}
	if resp, raw := e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+nodeID, nil, map[string]string{"If-Match": `"1"`}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete of the now-unreferenced node = %d, want 204 (body %s)", resp.StatusCode, raw)
	}
}

// TestDeleteOrgScopeNodeIsRefused: DAT-022, on an org node that is EMPTY —
// nothing beneath it, nothing placed at it. DAT-020 and DAT-021 have nothing to
// say about it, so the only rule that can refuse is the org rule itself, and a
// pass here cannot be explained by either of the other two.
func TestDeleteOrgScopeNodeIsRefused(t *testing.T) {
	e := newEnv(t)
	orgID := e.createNode(t, orgNode("Demo Org"))

	resp, raw := e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+orgID, nil, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete of the org node = %d, want 409 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "SCOPE_NODE_ORG_UNDELETABLE")

	if resp, _ := e.do(t, http.MethodGet, "/api/v1/scope-nodes/"+orgID, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the org node is gone after a refused delete: GET = %d, want 200", resp.StatusCode)
	}
}

// TestDeleteOrgScopeNodeIsRefusedIndependentOfItsChildren: DAT-022 is
// "independent of DAT-020's own child-emptiness test", so an org node that is
// ALSO non-empty answers with the org refusal rather than the emptiness one.
// Emptying it changes nothing.
func TestDeleteOrgScopeNodeIsRefusedIndependentOfItsChildren(t *testing.T) {
	e := newEnv(t)
	orgID := e.createNode(t, orgNode("Demo Org"))
	siteID := e.createNode(t, siteUnder(orgID))

	resp, raw := e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+orgID, nil, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete of a populated org node = %d, want 409 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "SCOPE_NODE_ORG_UNDELETABLE")

	if resp, raw := e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+siteID, nil, map[string]string{"If-Match": `"1"`}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete of the org's only child = %d, want 204 (body %s)", resp.StatusCode, raw)
	}
	resp, raw = e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+orgID, nil, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete of the now-empty org node = %d, want 409 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "SCOPE_NODE_ORG_UNDELETABLE")
}

// TestScopeNodeDeleteRefusalOrdering pins where the three refusals sit among the
// ones that were already there. Two properties, and they pull in opposite
// directions:
//
//   - Nothing may leak existence. A caller who cannot READ the node gets the
//     404 an unminted id gets, and a caller who may read but not write gets the
//     403 — both BEFORE any rule that inspects the rest of the store, so neither
//     can learn from a 409 that a node exists, let alone that it is populated.
//   - The org refusal precedes the If-Match precondition, for the reason the
//     unwritable-placement refusal already does: no ETag will ever make the org
//     node deletable, so answering 428 would invite a retry of an operation the
//     caller can never complete. The other two are the opposite — a caller CAN
//     clear them — so they stay behind the precondition, where a delete that
//     will proceed has to present a fresh revision like any other.
func TestScopeNodeDeleteRefusalOrdering(t *testing.T) {
	e := newEnv(t)
	orgID := e.createNode(t, orgNode("Demo Org"))
	siteID := e.createNode(t, siteUnder(orgID))
	e.createNode(t, screenNode("", siteID, ""))

	// A principal bound only under the site cannot see the org node at all.
	outsider := e.principalWith(t, roleAt{siteID, auth.RoleOwner})
	resp, raw := e.as(t, outsider, http.MethodDelete, "/api/v1/scope-nodes/"+orgID, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete of an out-of-reach org node = %d, want 404 — a 409 would confirm it exists (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")

	// A viewer at the populated site can read it and gets 403 — never the 409
	// that would additionally tell them the node has children.
	viewer := e.principalWith(t, roleAt{siteID, auth.RoleViewer})
	resp, raw = e.as(t, viewer, http.MethodDelete, "/api/v1/scope-nodes/"+siteID, nil, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a viewer's delete of a populated node = %d, want 403 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "FORBIDDEN")

	// The precondition still governs the two clearable refusals: a populated
	// node with no If-Match is 428, and the caller is told to come back with a
	// revision rather than about a state they can change.
	resp, raw = e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+siteID, nil, nil)
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("delete of a populated node with no If-Match = %d, want 428 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "IF_MATCH_REQUIRED")

	// A stale If-Match likewise answers 412 before the emptiness rule runs.
	resp, raw = e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+siteID, nil, map[string]string{"If-Match": `"99"`})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("delete of a populated node with a stale If-Match = %d, want 412 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "REVISION_CONFLICT")

	// The org node answers its own refusal with no If-Match at all.
	resp, raw = e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+orgID, nil, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete of the org node with no If-Match = %d, want 409 — an ETag can never make it deletable (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "SCOPE_NODE_ORG_UNDELETABLE")
}

// siteUnder is siteNode parented at a real org node rather than at the package's
// never-created subtree-boundary id.
func siteUnder(orgID string) datamodel.ScopeNode {
	n := siteNode("")
	n.ParentID = &orgID
	return n
}
