package api_test

// bodyschema_test.go drives the property that the surface enforces the request
// bodies `api/openapi.yaml` DECLARES, rather than declaring one shape and
// accepting another.
//
// Before it, no generic resource family checked a body against its declared
// schema at all: `required`, `additionalProperties: false`, `minLength`,
// `pattern` and the rest were documentation. The visible consequence was a row
// created with `"scope_node": ""` — placed at a node that does not exist, and
// thereafter invisible to every principal whose binding is not the root
// sentinel.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// declaredSchemaFamilies are the create paths this gate covers, with a create
// body that is valid EXCEPT for its `scope_node` — the member the defect was
// reported against. Both families' declared Create schemas type `scope_node` as
// a Ulid (a 26-character pattern), and neither had any other check on it: an
// automation and a webhook endpoint were both stored with an empty placement.
func declaredSchemaFamilies(t *testing.T) []struct {
	path string
	body map[string]any
} {
	t.Helper()
	return []struct {
		path string
		body map[string]any
	}{
		{"/api/v1/automations", map[string]any{
			"name": "Placeless", "scope_node": "", "mode": "single",
			"triggers": []any{map[string]any{"type": "state", "entity_id": autoScreenEntity, "to": []string{"on"}}},
			"actions":  []any{map[string]any{"type": "notify", "message": "hi"}},
		}},
		{"/api/v1/webhook-endpoints", map[string]any{
			"name": "Placeless", "scope_node": "", "url": "https://hooks.example.invalid/waiveo",
		}},
	}
}

// TestCreateRejectsAPlacementTheDeclaredSchemaForbids: a create whose
// `scope_node` is empty is refused 422 before any write, on every family whose
// request body api/openapi.yaml declares. Without the gate both of these bodies
// are stored 201 — the reported defect.
func TestCreateRejectsAPlacementTheDeclaredSchemaForbids(t *testing.T) {
	e := newEnv(t)
	for _, tc := range declaredSchemaFamilies(t) {
		resp, raw := e.do(t, http.MethodPost, tc.path, mustJSON(t, tc.body), nil)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("POST %s with an empty scope_node: %d, want 422 (body %s)", tc.path, resp.StatusCode, raw)
		}
		p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
		if detail, _ := p["detail"].(string); !strings.Contains(detail, "scope_node") {
			t.Errorf("POST %s detail = %q, want it to name scope_node", tc.path, detail)
		}
		// Nothing was written: the refusal is a pre-write gate, not a
		// after-the-fact complaint about a row that already landed.
		listResp, listRaw := e.do(t, http.MethodGet, tc.path, nil, nil)
		if listResp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d (body %s)", tc.path, listResp.StatusCode, listRaw)
		}
		var page struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(listRaw, &page); err != nil {
			t.Fatalf("decode %s page: %v (%s)", tc.path, err, listRaw)
		}
		if len(page.Items) != 0 {
			t.Fatalf("%s holds %d rows after a refused create; the write executed", tc.path, len(page.Items))
		}
	}
}

// TestCreateRejectsAMemberTheDeclaredSchemaDoesNotDeclare: every Create schema
// on this surface is `additionalProperties: false`, and an undeclared member was
// previously merged into the stored row verbatim. This is the half of the
// declared shape that has nothing to do with any one field.
func TestCreateRejectsAMemberTheDeclaredSchemaDoesNotDeclare(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))

	body := map[string]any{
		"name": "Smuggler", "scope_node": siteID, "url": "https://hooks.example.invalid/waiveo",
		"delivery_state": "healthy",
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/webhook-endpoints", mustJSON(t, body), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create carrying an undeclared member: %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	if detail, _ := p["detail"].(string); !strings.Contains(detail, "delivery_state") {
		t.Errorf("detail = %q, want it to name the undeclared member", detail)
	}
}

// TestPatchRejectsABodyThatViolatesItsDeclaredUpdateSchema pins the same gate on
// the update half, and pins that the row is untouched: a PATCH is a
// read-modify-write, so a body refused after the merge would still have been
// merged.
func TestPatchRejectsABodyThatViolatesItsDeclaredUpdateSchema(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))

	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", edgeAutomationBody("", siteID, nil), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create an automation: %d (body %s)", resp.StatusCode, raw)
	}
	id := decodeID(t, raw)

	// An empty `name` is a member the declared AutomationUpdate types minLength
	// 1, and nothing downstream checks an automation's name: the rules/1 compile
	// gate reads triggers/conditions/actions, and the store's datamodel arm reads
	// only that the ids are ULIDs.
	patch := mustJSON(t, map[string]any{"name": ""})
	resp, raw = e.do(t, http.MethodPatch, "/api/v1/automations/"+id, patch, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("patch with an empty name: %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")

	resp, raw = e.do(t, http.MethodGet, "/api/v1/automations/"+id, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read the automation back: %d (body %s)", resp.StatusCode, raw)
	}
	var row struct {
		Name     string `json:"name"`
		Revision int    `json:"revision"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode the automation: %v (%s)", err, raw)
	}
	if row.Name != "Demo Automation" || row.Revision != 1 {
		t.Fatalf("row after a refused patch = %+v, want the original name at revision 1", row)
	}
}

// TestDeclaredCreateBodyStillPasses is the false-positive guard: the gate must
// refuse only what the document refuses. A conformant create body on every gated
// family still lands 201.
func TestDeclaredCreateBodyStillPasses(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))

	for _, tc := range []struct {
		path string
		body map[string]any
	}{
		{"/api/v1/screens", map[string]any{"name": "Lobby", "scope_node": siteID}},
		{"/api/v1/adopted-devices", map[string]any{
			"name": "Roku", "scope_node": siteID, "driver": "roku", "native_id": "10.0.0.41",
		}},
		{"/api/v1/webhook-endpoints", map[string]any{
			"name": "Hook", "scope_node": siteID, "url": "https://hooks.example.invalid/waiveo",
		}},
	} {
		resp, raw := e.do(t, http.MethodPost, tc.path, mustJSON(t, tc.body), nil)
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("POST %s with a conformant body: %d, want 201 (body %s)", tc.path, resp.StatusCode, raw)
		}
	}
}
