package api_test

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	apispec "github.com/maaxton/waiveo-next/api"
)

// API-013b: a partial update carrying no members is refused 422, never treated
// as a successful no-op.
//
// The behaviour predates the requirement — every `*Update` schema in the
// document declares `minProperties: 1`, and the body-schema gate enforces it.
// What did not exist is any check that it is TRUE OF ALL OF THEM. Uniformity is
// the whole content of API-013b: a rule that holds for seven families and
// silently not the eighth is worse than no rule, because a client that learns
// the behaviour from one resource will meet a 200 no-op on another and have no
// way to know which it is talking to.
//
// I verified the uniformity by reading the document. This makes that reading
// executable, so a schema added or edited without `minProperties` fails here
// rather than quietly reintroducing the no-op on one family.

// updateSchemaBlocks returns each `*Update` schema in the embedded document,
// keyed by name, as its raw YAML lines.
//
// Read from apispec.OpenAPIYAML — the same embedded bytes the request-time gate
// validates against — so this cannot pass against a copy of the document that
// the running server does not use.
func updateSchemaBlocks(t *testing.T) map[string][]string {
	t.Helper()
	lines := strings.Split(string(apispec.OpenAPIYAML), "\n")
	// A component schema is a 4-space-indented `Name:` line; its body is every
	// following more-indented line.
	nameRe := regexp.MustCompile(`^    ([A-Za-z][A-Za-z0-9]*):\s*$`)
	out := map[string][]string{}
	var cur string
	for _, l := range lines {
		if m := nameRe.FindStringSubmatch(l); m != nil {
			cur = m[1]
			if strings.HasSuffix(cur, "Update") {
				out[cur] = nil
			}
			continue
		}
		if cur == "" {
			continue
		}
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "     ") {
			cur = ""
			continue
		}
		if _, ok := out[cur]; ok {
			out[cur] = append(out[cur], l)
		}
	}
	return out
}

// TestEveryUpdateSchemaRefusesAnEmptyBody is API-013b's uniformity, checked
// against the document the server actually serves.
func TestEveryUpdateSchemaRefusesAnEmptyBody(t *testing.T) {
	blocks := updateSchemaBlocks(t)
	if len(blocks) == 0 {
		t.Fatal("found no *Update schemas in the embedded document — the scan is broken, and a broken scan passes every " +
			"assertion below")
	}
	t.Logf("checked %d Update schema(s)", len(blocks))
	for name, body := range blocks {
		var found bool
		for _, l := range body {
			if strings.HasPrefix(strings.TrimSpace(l), "minProperties:") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s declares no minProperties, so a PATCH with an empty body is accepted as a no-op for that family "+
				"(API-013b). A rule that holds for every other family and silently not this one is worse than no rule: a "+
				"client that learned the behaviour elsewhere gets a 200 back describing a resource it did not write.", name)
		}
	}
}

// TestAnEmptyPatchIsRefusedOverHTTP drives the rule end to end on a real route,
// because a schema keyword present in the document proves nothing about whether
// the gate applies it.
//
// The control matters as much as the refusal: the same route, the same row, with
// one member present, must succeed. Without it, a family that rejected EVERY
// patch would pass the refusal assertion.
func TestAnEmptyPatchIsRefusedOverHTTP(t *testing.T) {
	e := newEnv(t)
	site := e.createNode(t, siteNode(""))

	res, raw := e.do(t, http.MethodPost, "/api/v1/schedules",
		[]byte(`{"scope_node":"`+site+`","name":"Placed"}`), nil)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d; body %s", res.StatusCode, raw)
	}
	var made struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &made); err != nil {
		t.Fatalf("decode create: %v (body %s)", err, raw)
	}
	id := made.ID

	// The ETag is re-read before each patch rather than reused from the create.
	// A stale If-Match answers 412 REVISION_CONFLICT, which is a precondition
	// failure and not a body-schema one — it would mask the very difference
	// these two cases exist to show.
	etag := func(t *testing.T) string {
		t.Helper()
		res, raw := e.do(t, http.MethodGet, "/api/v1/schedules/"+id, nil, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("re-read = %d; body %s", res.StatusCode, raw)
		}
		return res.Header.Get("ETag")
	}

	t.Run("an empty body is refused", func(t *testing.T) {
		res, raw := e.do(t, http.MethodPatch, "/api/v1/schedules/"+id,
			[]byte(`{}`), map[string]string{"If-Match": etag(t)})
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("PATCH {} = %d, want 422 (API-013b); body %s", res.StatusCode, raw)
		}
	})

	t.Run("the same patch with one member succeeds", func(t *testing.T) {
		res, raw := e.do(t, http.MethodPatch, "/api/v1/schedules/"+id,
			[]byte(`{"name":"Renamed"}`), map[string]string{"If-Match": etag(t)})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("PATCH with a member = %d, want 200; body %s", res.StatusCode, raw)
		}
	})
}
