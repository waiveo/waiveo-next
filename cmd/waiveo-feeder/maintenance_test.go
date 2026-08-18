package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"

	_ "modernc.org/sqlite"
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
	cause, ok := maintenanceCauseFor(&store.EpochTooNewError{OnDisk: 4, Understood: 2})
	if !ok {
		t.Fatalf("a newer-epoch workspace must degrade to maintenance mode, not a crash")
	}
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
	cause, _ := maintenanceCauseFor(&store.EpochTooNewError{OnDisk: 2, Understood: 1})
	mux := maintenanceMux(cause)

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

// TestMaintenanceHandlesABlockedSchemaMigration: the newer-epoch refusal is not
// the only way this build can fail to open its own workspace. A column it
// declares that SQLite cannot retrofit is the same class from the operator's
// side — the store is intact, unchanged, and unopenable until a human decides
// something — and it must degrade the same way.
//
// The alternative is what the boot did before: an unrecognized error falls
// through maintenanceOnStoreOpenError to log.Fatalf, and under Restart=always
// the unit flaps forever. /healthz is connection-refused, the console log page
// cannot show lines from before the process started, and the one sentence naming
// the column exists only in the journal of a box that is now down and taking its
// relay with it.
//
// The error is produced by a REAL open of a REAL store rather than constructed
// here, because what is being tested is the routing of what actually happens.
func TestMaintenanceHandlesABlockedSchemaMigration(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")
	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+dsn+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	// `jobs.created_at INTEGER NOT NULL` — this build's own DDL, and the shape
	// 108 of its declared columns share. SQLite cannot add it back.
	if _, err := db.Exec(`ALTER TABLE jobs DROP COLUMN created_at`); err != nil {
		t.Fatalf("forge the blocked store: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	_, openErr := store.Open(dsn, store.WallClockMs)
	if openErr == nil {
		t.Fatalf("the forged store must not open")
	}

	cause, ok := maintenanceCauseFor(openErr)
	if !ok {
		t.Fatalf("a blocked schema migration must degrade to maintenance mode; it currently reaches log.Fatalf "+
			"and crash-loops the box. err = %v", openErr)
	}
	if cause.reason != "workspace_schema_migration_blocked" {
		t.Fatalf("reason = %q, want workspace_schema_migration_blocked", cause.reason)
	}

	rec := httptest.NewRecorder()
	maintenanceMux(cause).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["maintenance_mode"] != true {
		t.Fatalf("maintenance_mode = %v, want true", body["maintenance_mode"])
	}
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, "jobs.created_at") {
		t.Fatalf("the operator must be able to read which column blocked the open; detail = %q", detail)
	}
	blocked, ok := body["workspace_schema_blocked_columns"].([]any)
	if !ok || len(blocked) != 1 {
		t.Fatalf("the blocked columns must reach the wire; got %v", body["workspace_schema_blocked_columns"])
	}
	if remedy, _ := body["remedy"].(string); !strings.Contains(remedy, "DEFAULT") {
		t.Fatalf("the surface must say what would fix it; remedy = %q", remedy)
	}
}
