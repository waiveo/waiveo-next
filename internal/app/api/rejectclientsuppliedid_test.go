package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// TestRejectClientSuppliedIDPresetBatchIdentityField pins the preset-batch
// identity-field variant of rejectClientSuppliedID: DAT-005's byte-exact
// exception names a preset-batch row's own identity field preset_id, not id,
// and resourceConfig.identity/identityField (api.go) branch on
// store.KindPresetBatch to honor it — but before this test, neither branch had
// coverage naming preset_id specifically: every existing rejectClientSuppliedID
// test exercises a scope-nodes or automations body, whose identity field
// happens to be the generic "id". A preset-batch client-supplied-identity
// rejection must name "preset_id", not "id", as the offending field — a wrong
// or missing preset-batch branch would silently let a client-supplied preset_id
// through (identity() would read the always-empty ID field instead) while this
// test still exercises the code path via a direct, unmounted call, independent
// of whether preset-batches are wired to an HTTP route yet.
func TestRejectClientSuppliedIDPresetBatchIdentityField(t *testing.T) {
	rs := &resource{cfg: resourceConfig{kind: store.KindPresetBatch, path: "preset-batches", resourceType: "preset-batches"}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/preset-batches", nil)

	w := httptest.NewRecorder()
	body := []byte(`{"preset_id":"01J8Z0SOMEPRESETBATCH00001","commands":[]}`)
	if !rs.rejectClientSuppliedID(w, req, body) {
		t.Fatalf("rejectClientSuppliedID(preset-batch, client-supplied preset_id) = false, want true")
	}
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}
	var p struct {
		Code   string `json:"code"`
		Errors []struct {
			Field string `json:"field"`
			Code  string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (body %s)", err, w.Body.Bytes())
	}
	if p.Code != "VALIDATION_FAILED" {
		t.Fatalf("problem code = %q, want VALIDATION_FAILED", p.Code)
	}
	if len(p.Errors) != 1 || p.Errors[0].Field != "preset_id" {
		t.Fatalf("errors = %+v, want exactly one entry naming field \"preset_id\"", p.Errors)
	}
	if p.Errors[0].Code != "ID_SERVER_ASSIGNED" {
		t.Fatalf("errors[0].code = %q, want ID_SERVER_ASSIGNED", p.Errors[0].Code)
	}

	// A preset-batch body carrying no preset_id (the legitimate create shape) is
	// not rejected at all — a false positive here would mean identity() is
	// reading the wrong field and every legitimate preset-batch create would be
	// refused.
	w2 := httptest.NewRecorder()
	if rs.rejectClientSuppliedID(w2, req, []byte(`{"commands":[]}`)) {
		t.Fatalf("rejectClientSuppliedID(preset-batch, no preset_id) = true, want false")
	}
}
