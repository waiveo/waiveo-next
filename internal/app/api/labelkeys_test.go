package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// labelkeys_test.go covers the label-key charset on the write path, and the
// bound on what a Problem echoes back.
//
// API-042 constrains label keys and values alike so "every label is expressible
// in a selector term without escaping". The VALUE half was enforced — its pattern
// sits on additionalProperties, which the schema validator applies — while the
// KEY half sat in a keyword the validator does not model, so it was declared,
// generated into both clients, and applied by nothing. A key carrying `,` `=` or
// a space stored fine and then could not be named by any selector: the row is
// there, and no scoped query finds it.

// badLabelKeys are keys the selector grammar cannot express.
var badLabelKeys = []string{
	"has,comma",
	"has=equals",
	"has!bang",
	"has space",
	"has(paren)",
	strings.Repeat("x", 300), // past the length the pattern allows
}

func TestALabelKeyOutsideTheCharsetIsRefused(t *testing.T) {
	for _, family := range []struct{ name, path string }{
		// A gated family, which runs the whole declared schema...
		{"screens", "/api/v1/screens"},
		// ...and one that runs only the member half, where the same rule has to
		// arrive by the other route.
		{"scope-nodes", "/api/v1/scope-nodes"},
	} {
		t.Run(family.name, func(t *testing.T) {
			for _, key := range badLabelKeys {
				e := newEnv(t)
				site := e.createNode(t, siteUnder(e.createNode(t, orgNode("Org"))))

				var body []byte
				if family.name == "screens" {
					body = mustJSON(t, map[string]any{"scope_node": site, "name": "S",
						"labels": map[string]string{key: "v"}})
				} else {
					body = mustJSON(t, map[string]any{"kind": "screen", "name": "S", "parent_id": site,
						"labels": map[string]string{key: "v"}})
				}
				resp, raw := e.do(t, http.MethodPost, family.path, body, nil)
				if resp.StatusCode != http.StatusUnprocessableEntity {
					t.Errorf("label key %.40q = %d, want 422 (body %s)", key, resp.StatusCode, raw)
				}
			}
		})
	}
}

func TestAValidLabelKeyIsAccepted(t *testing.T) {
	e := newEnv(t)
	site := e.createNode(t, siteUnder(e.createNode(t, orgNode("Org"))))

	// Every shape API-042 admits, including the namespaced form — a check that
	// refused these would break labelling entirely while passing every test above.
	labels := map[string]string{
		"env":                 "prod",
		"waiveo.example/tier": "gold",
		"has-dash":            "v",
		"has.dot":             "v",
		"has_underscore":      "v",
		"MixedCase9":          "v",
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/screens",
		mustJSON(t, map[string]any{"scope_node": site, "name": "S", "labels": labels}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("valid label keys = %d, want 201 (body %s)", resp.StatusCode, raw)
	}
}

// TestAProblemDoesNotAmplifyAClientSuppliedName: the detail never carries a
// value, but a KEY is a name and names are echoed — by the schema validator's own
// reason and by the JSON pointer built from nested keys. A 100 KB property name
// produced a 100 KB detail, and a 422 is under 500, so it was RETAINED and
// replayed verbatim for the idempotency window: the client controlled both the
// amplification and how long it was stored.
func TestAProblemDoesNotAmplifyAClientSuppliedName(t *testing.T) {
	e := newEnv(t)
	site := e.createNode(t, siteUnder(e.createNode(t, orgNode("Org"))))

	huge := strings.Repeat("k", 100_000)
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"undeclared member", map[string]any{"scope_node": site, "name": "S", huge: 1}},
		{"label key", map[string]any{"scope_node": site, "name": "S", "labels": map[string]string{huge: "v"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := e.do(t, http.MethodPost, "/api/v1/screens", mustJSON(t, tc.body), nil)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", resp.StatusCode)
			}
			// Generously bounded: the point is that the response does not scale with
			// the request, not that it hits a particular size.
			if len(raw) > 2_000 {
				t.Errorf("a %d-byte name produced a %d-byte Problem — the response scales with what the client sent",
					len(huge), len(raw))
			}
		})
	}
}
