package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestASchedulingRowMustCarryAPlacement pins DAT-006's PRESENCE half for a
// scheduling-core kind, on both routes that could break it.
//
// This is a different fault from a placement that does not resolve. An unplaced
// row does not dangle — it points nowhere at all — and it fails differently: its
// ancestor chain resolves to nil, so it falls through to the workspace-root path
// and no scope selector finds it where an operator would look.
//
// DAT-006 covers "every row this contract defines", and DAT-005 enumerates the six
// scheduling-core kinds, so a placement is required for all of them. Only the two
// identity kinds enforced it before this.
func TestASchedulingRowMustCarryAPlacement(t *testing.T) {
	e := newEnv(t)

	t.Run("create with no scope_node is refused", func(t *testing.T) {
		res, raw := e.do(t, http.MethodPost, "/api/v1/schedules",
			[]byte(`{"name":"Unplaced"}`), nil)
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body %s", res.StatusCode, raw)
		}
		assertFieldCode(t, raw, "scope_node", "ROW_SCOPE_NODE_MISSING")
	})

	// The sharper case: a row that IS placed, walked off the tree by a PATCH. This
	// returned 200 before, silently detaching a correctly-placed row.
	t.Run("clearing an existing placement by PATCH is refused", func(t *testing.T) {
		site := e.createNode(t, siteNode(""))
		res, raw := e.do(t, http.MethodPost, "/api/v1/schedules",
			[]byte(`{"scope_node":"`+site+`","name":"Placed"}`), nil)
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("placed create = %d; body %s", res.StatusCode, raw)
		}
		var made struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &made); err != nil {
			t.Fatalf("decode: %v", err)
		}

		for _, spelling := range []string{`{"scope_node":null}`, `{"scope_node":""}`} {
			res, raw := e.do(t, http.MethodPatch, "/api/v1/schedules/"+made.ID,
				[]byte(spelling), map[string]string{"If-Match": "\"1\""})
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("PATCH %s = %d, want 422 — a placed row must not be walkable off the tree; body %s",
					spelling, res.StatusCode, raw)
			}
			assertFieldCode(t, raw, "scope_node", "ROW_SCOPE_NODE_MISSING")
		}
	})
}

// assertFieldCode asserts a Problem body carries a per-field error naming field
// with code. It reports the whole body on failure, because a refusal for the
// WRONG reason is the failure mode these tests exist to tell apart.
func assertFieldCode(t *testing.T, raw []byte, field, code string) {
	t.Helper()
	var p struct {
		Errors []struct {
			Field string `json:"field"`
			Code  string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode problem: %v (body %s)", err, raw)
	}
	for _, e := range p.Errors {
		if e.Field == field && e.Code == code {
			return
		}
	}
	t.Fatalf("no %s/%s among the per-field errors; body %s", field, code, raw)
}
