package identity

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// telemetrytrace_test.go covers the durable half of the trace_id correlation
// chain (relay/1 REL-006, events/1 EVT-010): a buffered entry's record-time
// correlation id has to survive the very thing the durable queue exists for — a
// disconnect long enough to outlive the process — or the resumed backlog
// delivers events the app peer can only stamp with a fresh, uncorrelated value.

// TestTelemetryTraceIDSurvivesReopen: the entry reloaded after a restart carries
// the SAME trace_id the relay assigned when it recorded it.
func TestTelemetryTraceIDSurvivesReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	store1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	buf, err := telemetry.NewDurableBuffer(store1, 500)
	if err != nil {
		t.Fatalf("NewDurableBuffer: %v", err)
	}
	payload := json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z1","mode_disposition":"ran"}`)
	rec := buf.Record(telemetry.SchemaAutomationRun, payload, "01J8Z3K4N5P6Q7R8S9T0V1W2Z1", 1_752_800_000_000)
	if err := buf.StoreErr(); err != nil {
		t.Fatalf("StoreErr after Record: %v", err)
	}
	if !ulid.Valid(rec.TraceID) {
		t.Fatalf("Record assigned trace_id %q, not a canonical ULID", rec.TraceID)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	entries, _, err := store2.LoadTelemetry()
	if err != nil {
		t.Fatalf("LoadTelemetry after reopen: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("LoadTelemetry returned %d entries, want 1", len(entries))
	}
	if entries[0].TraceID != rec.TraceID {
		t.Fatalf("reloaded trace_id = %q, want the recorded %q — a resumed backlog must stay correlated to the operations that produced it (REL-006)",
			entries[0].TraceID, rec.TraceID)
	}

	// And the resumed Buffer keeps it, so the entry pushed after a restart is
	// the same correlated record it would have been before one.
	resumed, err := telemetry.NewDurableBuffer(store2, 500)
	if err != nil {
		t.Fatalf("NewDurableBuffer (resume): %v", err)
	}
	pending := resumed.Pending()
	if len(pending) != 1 || pending[0].TraceID != rec.TraceID {
		t.Fatalf("resumed buffer pending = %+v, want one entry carrying trace_id %q", pending, rec.TraceID)
	}
}

// TestTelemetryQueueMigratesToCarryTraceID: a relay whose store predates the
// trace_id column keeps working when it is upgraded. `CREATE TABLE IF NOT
// EXISTS` would leave the old five-column queue in place and every durable
// append would fail, so Open runs the migration — this drives a genuinely old
// table, not a simulated one.
func TestTelemetryQueueMigratesToCarryTraceID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	// A telemetry_queue exactly as an earlier build created it: no trace_id.
	old, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := old.Exec(`CREATE TABLE telemetry_queue (
		seq          INTEGER PRIMARY KEY,
		schema       TEXT NOT NULL,
		payload      BLOB NOT NULL,
		subject      TEXT NOT NULL,
		recorded_at  INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create pre-trace_id telemetry_queue: %v", err)
	}
	// A durable entry already queued under the old shape, as a real upgrade
	// would find: it has no trace at all, and must not be destroyed by the
	// migration.
	if _, err := old.Exec(
		`INSERT INTO telemetry_queue (seq, schema, payload, subject, recorded_at) VALUES (?, ?, ?, ?, ?)`,
		7, telemetry.SchemaAutomationRun, []byte(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z1","mode_disposition":"ran"}`), "01J8Z3K4N5P6Q7R8S9T0V1W2Z1", 1_752_800_000_000,
	); err != nil {
		t.Fatalf("seed pre-trace_id entry: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open over a pre-trace_id store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	entries, _, err := store.LoadTelemetry()
	if err != nil {
		t.Fatalf("LoadTelemetry after migration: %v", err)
	}
	if len(entries) != 1 || entries[0].Seq != 7 {
		t.Fatalf("the pre-existing backlog must survive the migration; got %+v", entries)
	}
	if entries[0].TraceID != "" {
		t.Errorf("a row written before trace_id existed must read back with an empty trace, not an invented one; got %q", entries[0].TraceID)
	}

	// The migrated queue accepts a new, traced append.
	buf, err := telemetry.NewDurableBuffer(store, 500)
	if err != nil {
		t.Fatalf("NewDurableBuffer: %v", err)
	}
	rec := buf.Record(telemetry.SchemaAutomationRun, json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","mode_disposition":"ran"}`), "01J8Z3K4N5P6Q7R8S9T0V1W2Z2", 1_752_800_001_000)
	if err := buf.StoreErr(); err != nil {
		t.Fatalf("durable append after migration: %v", err)
	}

	entries, _, err = store.LoadTelemetry()
	if err != nil {
		t.Fatalf("LoadTelemetry after append: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Seq == rec.Seq {
			found = true
			if e.TraceID != rec.TraceID {
				t.Errorf("appended trace_id = %q, want %q", e.TraceID, rec.TraceID)
			}
		}
	}
	if !found {
		t.Fatalf("the post-migration append is missing from the queue; got %+v", entries)
	}
}
