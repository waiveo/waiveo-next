package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// The maintenance surface is what an operator (and the fleet dashboard) reads
// when a box cannot open its workspace because the file is at a newer schema
// epoch than this build understands. /healthz must report the degraded state as
// a 200 body — a liveness probe reading only the status code must not mistake a
// diagnosable maintenance boot for a dead process — and every other route must
// refuse with 503 so nothing looks served.

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body: %v (%q)", err, rec.Body.String())
	}
	return m
}

func TestMaintenanceHealthzReportsTheEpochReason(t *testing.T) {
	cause := &store.EpochTooNewError{OnDisk: 4, Understood: 2}
	mux := maintenanceMux(cause)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	// 200 with the maintenance flag in the body, not a transport failure.
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200 (maintenance is reported in the body, not the code)", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["maintenance_mode"] != true {
		t.Fatalf("maintenance_mode = %v, want true", body["maintenance_mode"])
	}
	if body["status"] != "maintenance" {
		t.Fatalf("status = %v, want maintenance", body["status"])
	}
	// The numbers an operator needs to know which side is behind must survive to
	// the wire (JSON decodes them as float64).
	epoch, ok := body["workspace_schema_epoch"].(map[string]any)
	if !ok {
		t.Fatalf("workspace_schema_epoch missing or wrong shape: %v", body["workspace_schema_epoch"])
	}
	if epoch["on_disk"] != float64(4) || epoch["understood"] != float64(2) {
		t.Fatalf("epoch numbers = %v, want on_disk 4 / understood 2", epoch)
	}
}

func TestMaintenanceRefusesEveryOtherRouteWith503(t *testing.T) {
	mux := maintenanceMux(&store.EpochTooNewError{OnDisk: 2, Understood: 1})

	for _, path := range []string{"/", "/api/v1/scope-nodes", "/relay/v1", "/content/x"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s status = %d, want 503 while in maintenance", path, rec.Code)
		}
		if body := decodeBody(t, rec); body["maintenance_mode"] != true {
			t.Errorf("%s body maintenance_mode = %v, want true", path, body["maintenance_mode"])
		}
	}
}
