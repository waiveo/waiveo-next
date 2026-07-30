package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// TestReParentingIntoACycleIsRefused pins the route, not just the rule.
//
// Re-parenting a node under its own descendant leaves every reference resolving —
// both parents are real rows — so none of the per-row checks can see it. What is
// wrong is that the pair no longer hangs off the org root, which is DAT-002 read
// as a property of the tree. It is the last way a detached component could be
// reached: the four ways a reference could DANGLE are closed, and this one never
// dangled.
func TestReParentingIntoACycleIsRefused(t *testing.T) {
	e := newEnv(t)
	org := e.createNode(t, datamodel.ScopeNode{Kind: "org", Name: "Org",
		AccountState: "active", Entitlements: json.RawMessage(`{}`)})
	site := e.createNode(t, datamodel.ScopeNode{Kind: "site", ParentID: strp(org), Name: "Site",
		TZ: strp(siteTZ), Lat: f64p(siteLat), Long: f64p(siteLong)})
	groupA := e.createNode(t, datamodel.ScopeNode{Kind: "group", ParentID: strp(site), Name: "A"})
	groupB := e.createNode(t, datamodel.ScopeNode{Kind: "group", ParentID: strp(groupA), Name: "B"})

	// A under B, where B is already A's child.
	body := mustJSON(t, map[string]any{"parent_id": groupB})
	res, raw := e.do(t, http.MethodPatch, "/api/v1/scope-nodes/"+groupA, body,
		map[string]string{"If-Match": "\"1\""})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("re-parenting into a cycle: status = %d, want 422; body %s", res.StatusCode, raw)
	}
	assertProblem(t, res, raw, "VALIDATION_FAILED")

	// And the tree is unchanged — a refused PATCH stores nothing.
	after, afterRaw := e.do(t, http.MethodGet, "/api/v1/scope-nodes/"+groupA, nil, nil)
	if after.StatusCode != http.StatusOK {
		t.Fatalf("read back group A: %d %s", after.StatusCode, afterRaw)
	}
	var node struct {
		ParentID *string `json:"parent_id"`
	}
	if err := json.Unmarshal(afterRaw, &node); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if node.ParentID == nil || *node.ParentID != site {
		t.Errorf("group A's parent after the refusal = %v, want the site it started under — a refused write must store nothing", node.ParentID)
	}
}
