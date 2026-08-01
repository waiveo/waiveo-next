package api_test

import (
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth"
)

// webhookEndpointAt registers an endpoint placed at scopeNode and returns its id.
func webhookEndpointAt(t *testing.T, e *testEnv, scopeNode string) string {
	t.Helper()
	return decodeID(t, e.createOK(t, "/api/v1/webhook-endpoints", mustJSON(t, map[string]any{
		"name":       "Authz Fixture",
		"scope_node": scopeNode,
		"url":        "https://hooks.example.invalid/waiveo",
		"schemas":    []string{"automation.run"},
	})))
}

// TestWebhookEndpointOperationsAreAuthorizedPerNode pins BOTH guards in
// webhookEndpointFor, the helper the endpoint's three sub-operations share.
// Each could be deleted with go test ./... green.
//
// The two are different rules with different codes, and the helper is where the
// distinction lives: a node the caller cannot READ answers 404, because saying
// FORBIDDEN would confirm an endpoint exists there; a node it can read but not
// WRITE answers 403, because the caller has already been shown the row and the
// refusal discloses nothing beyond its own authority.
//
// Both principals below hold a binding SOMEWHERE, which is what makes the test
// about these guards rather than about the coarse role gate in auth's
// middleware — that gate refuses a POST on effective role alone, so a principal
// with no write authority anywhere never reaches the helper at all.
func TestWebhookEndpointOperationsAreAuthorizedPerNode(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	endpoint := webhookEndpointAt(t, e, tr.siteA)

	t.Run("read authority but not write: 403 on a write operation", func(t *testing.T) {
		mixed := e.principalWith(t, roleAt{tr.siteA, auth.RoleViewer}, roleAt{tr.siteB, auth.RoleOperator})
		resp, raw := e.as(t, mixed, http.MethodPost, "/api/v1/webhook-endpoints/"+endpoint+"/enable",
			mustJSON(t, map[string]any{"enabled": true}), nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("enabling an endpoint the caller may only read = %d, want 403 (body %s)", resp.StatusCode, raw)
		}
		assertProblem(t, resp, raw, "FORBIDDEN")
	})

	t.Run("no read authority at the node: 404 even on a read operation", func(t *testing.T) {
		elsewhere := e.principalWith(t, roleAt{tr.siteB, auth.RoleOperator})
		resp, raw := e.as(t, elsewhere, http.MethodGet, "/api/v1/webhook-endpoints/"+endpoint+"/delivery", nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("reading an endpoint outside the visible set = %d, want 404 — FORBIDDEN would confirm an "+
				"endpoint exists at a node this caller may not see (body %s)", resp.StatusCode, raw)
		}
		assertProblem(t, resp, raw, "NOT_FOUND")
	})

	// The `write` flag itself: a READ operation must NOT be held to write
	// authority. Without this row, a helper that applied canWrite to every
	// operation passes everything above — measured, that mutant survived until
	// this case existed — and a viewer would silently lose the ability to read
	// an endpoint it is entitled to see.
	t.Run("read authority is enough for a read operation", func(t *testing.T) {
		viewer := e.principalWith(t, roleAt{tr.siteA, auth.RoleViewer})
		resp, raw := e.as(t, viewer, http.MethodGet, "/api/v1/webhook-endpoints/"+endpoint+"/delivery", nil, nil)
		if resp.StatusCode == http.StatusForbidden {
			t.Fatalf("a viewer was refused a READ of an endpoint it may see = 403 — the helper is holding reads "+
				"to write authority (body %s)", raw)
		}
	})

	// The control, and the half that stops a helper refusing everyone from
	// satisfying both rows above.
	t.Run("write authority at the node: the operation proceeds", func(t *testing.T) {
		operator := e.principalWith(t, roleAt{tr.siteA, auth.RoleOperator})
		resp, raw := e.as(t, operator, http.MethodPost, "/api/v1/webhook-endpoints/"+endpoint+"/enable",
			mustJSON(t, map[string]any{"enabled": true}), nil)
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			t.Fatalf("a principal that DOES hold write authority at the endpoint's node was refused = %d (body %s)",
				resp.StatusCode, raw)
		}
	})
}
