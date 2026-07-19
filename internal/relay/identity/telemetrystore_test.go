package identity

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
)

// storeSatisfiesDurableStore is a compile-time assertion that *Store is a
// telemetry.DurableStore — the persistence backing a durable telemetry Buffer
// writes through (REL-090).
var _ telemetry.DurableStore = (*Store)(nil)

// TestDurableTelemetrySurvivesReopen is the REL-090 durability property: a
// durable-class automation.run entry recorded through a durable-backed Buffer
// is present, unchanged, after a clean restart (a fresh Store on the same
// path + LoadTelemetry). §52's bounded telemetry buffer is durable local
// state that survives a power cycle.
func TestDurableTelemetrySurvivesReopen(t *testing.T) {
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
		t.Fatalf("LoadTelemetry returned %d entries, want 1 (the committed durable automation.run)", len(entries))
	}
	got := entries[0]
	if got.Seq != rec.Seq {
		t.Errorf("reloaded seq = %d, want %d", got.Seq, rec.Seq)
	}
	if got.Schema != telemetry.SchemaAutomationRun {
		t.Errorf("reloaded schema = %q, want %q", got.Schema, telemetry.SchemaAutomationRun)
	}
	if string(got.Payload) != string(payload) {
		t.Errorf("reloaded payload = %q, want %q", got.Payload, payload)
	}
	if got.Subject != "01J8Z3K4N5P6Q7R8S9T0V1W2Z1" {
		t.Errorf("reloaded subject = %q, want %q", got.Subject, "01J8Z3K4N5P6Q7R8S9T0V1W2Z1")
	}
	if got.RecordedAt != 1_752_800_000_000 {
		t.Errorf("reloaded recorded_at = %d, want %d", got.RecordedAt, 1_752_800_000_000)
	}
}

// TestLatestOnlyTelemetryNotPersisted is the REL-094 supersession property at
// the durable layer: a latest-only device.heartbeat is NOT written to the
// durable queue (it is a periodic snapshot superseded in place, never durable
// local state), so it does not resurface after a reopen.
func TestLatestOnlyTelemetryNotPersisted(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	store1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	buf, err := telemetry.NewDurableBuffer(store1, 500)
	if err != nil {
		t.Fatalf("NewDurableBuffer: %v", err)
	}
	buf.Record(telemetry.SchemaDeviceHeartbeat, json.RawMessage(`{"device_id":"devA","power_state":"on"}`), "devA", 2_001)
	if err := buf.StoreErr(); err != nil {
		t.Fatalf("StoreErr after Record: %v", err)
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
		t.Fatalf("LoadTelemetry: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("LoadTelemetry returned %d entries, want 0 (latest-only device.heartbeat MUST NOT persist, REL-094)", len(entries))
	}
}

// TestAppendTelemetryRejectsLatestOnlyDirectly asserts the store itself, not
// just the buffer, refuses to durably persist a latest-only entry — the
// durable-class gate (REL-093/094) lives in the store so no caller can slip a
// superseded snapshot into durable state.
func TestAppendTelemetryRejectsLatestOnlyDirectly(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.AppendTelemetry(telemetry.Entry{Seq: 1, Schema: telemetry.SchemaBoxVitals, Payload: json.RawMessage(`{"relay_id":"x"}`), Subject: "x"}); err != nil {
		t.Fatalf("AppendTelemetry(box.vitals): %v", err)
	}
	entries, _, err := store.LoadTelemetry()
	if err != nil {
		t.Fatalf("LoadTelemetry: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("box.vitals was persisted (%d entries), want 0 (latest-only, REL-094)", len(entries))
	}
}

// TestPruneTelemetryRespectsAckCursor is the REL-092 retention property: prune
// durably removes every entry whose seq is at or below the ack cursor and
// keeps every entry above it — the relay MUST NOT discard an entry whose seq
// exceeds ack_through_seq.
func TestPruneTelemetryRespectsAckCursor(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for _, seq := range []int64{1, 2, 3, 4, 5} {
		e := telemetry.Entry{Seq: seq, Schema: telemetry.SchemaContentPlayed, Payload: json.RawMessage(`{"asset_ref":"sha256:aa","screen_id":"s"}`), Subject: "s", RecordedAt: seq}
		if err := store.AppendTelemetry(e); err != nil {
			t.Fatalf("AppendTelemetry(seq=%d): %v", seq, err)
		}
	}

	if err := store.PruneTelemetry(3); err != nil {
		t.Fatalf("PruneTelemetry(3): %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen to prove the prune is durable, not just an in-memory effect.
	store2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	entries, _, err := store2.LoadTelemetry()
	if err != nil {
		t.Fatalf("LoadTelemetry: %v", err)
	}
	var gotSeqs []int64
	for _, e := range entries {
		gotSeqs = append(gotSeqs, e.Seq)
	}
	if len(gotSeqs) != 2 || gotSeqs[0] != 4 || gotSeqs[1] != 5 {
		t.Fatalf("after PruneTelemetry(3), remaining seqs = %v, want [4 5] (≤3 discarded, >3 retained, REL-092)", gotSeqs)
	}
}

// TestLossMarkersSurviveReopen confirms the telemetry_loss_marker table
// durably holds not-yet-acknowledged loss markers (REL-100/103) — a
// durable-class drop's marker MUST NOT be lost across a restart, or the app
// peer would never learn of the gap.
func TestLossMarkersSurviveReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	markers := []telemetry.LossMarker{
		{
			FromSeq:               10,
			ToSeq:                 12,
			DroppedCountsBySchema: map[string]int{telemetry.SchemaContentPlayed: 2, telemetry.SchemaAutomationRun: 1},
			Reason:                telemetry.ReasonBufferExceeded,
		},
	}
	if err := store.SaveLossMarkers(markers); err != nil {
		t.Fatalf("SaveLossMarkers: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	_, got, err := store2.LoadTelemetry()
	if err != nil {
		t.Fatalf("LoadTelemetry: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("reloaded %d loss markers, want 1", len(got))
	}
	m := got[0]
	if m.FromSeq != 10 || m.ToSeq != 12 || m.Reason != telemetry.ReasonBufferExceeded {
		t.Errorf("reloaded marker range/reason = {%d,%d,%q}, want {10,12,%q}", m.FromSeq, m.ToSeq, m.Reason, telemetry.ReasonBufferExceeded)
	}
	if m.DroppedCountsBySchema[telemetry.SchemaContentPlayed] != 2 || m.DroppedCountsBySchema[telemetry.SchemaAutomationRun] != 1 {
		t.Errorf("reloaded dropped counts = %v, want content.played:2 automation.run:1", m.DroppedCountsBySchema)
	}
}

// TestSaveLossMarkersReplacesPriorSet confirms SaveLossMarkers is a full
// replace of the persisted marker set (the buffer holds the authoritative
// in-memory set and writes it through wholesale), not an accumulate — retiring
// an acknowledged marker (REL-092) must durably drop it.
func TestSaveLossMarkersReplacesPriorSet(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.SaveLossMarkers([]telemetry.LossMarker{
		{FromSeq: 1, ToSeq: 1, DroppedCountsBySchema: map[string]int{telemetry.SchemaContentPlayed: 1}, Reason: telemetry.ReasonBufferExceeded},
		{FromSeq: 2, ToSeq: 2, DroppedCountsBySchema: map[string]int{telemetry.SchemaContentPlayed: 1}, Reason: telemetry.ReasonBufferExceeded},
	}); err != nil {
		t.Fatalf("SaveLossMarkers (two): %v", err)
	}
	if err := store.SaveLossMarkers([]telemetry.LossMarker{
		{FromSeq: 2, ToSeq: 2, DroppedCountsBySchema: map[string]int{telemetry.SchemaContentPlayed: 1}, Reason: telemetry.ReasonBufferExceeded},
	}); err != nil {
		t.Fatalf("SaveLossMarkers (one): %v", err)
	}

	_, got, err := store.LoadTelemetry()
	if err != nil {
		t.Fatalf("LoadTelemetry: %v", err)
	}
	if len(got) != 1 || got[0].FromSeq != 2 {
		t.Fatalf("after replace, markers = %+v, want exactly the {2,2} marker", got)
	}
}

// TestDurableBufferPrunesStoreOnOverflow confirms REL-096 drop-oldest overflow
// write-through: when a durable-backed Buffer overflows, the evicted
// lowest-seq durable entries are durably removed from the queue and the
// accounting loss marker is durably recorded, so a reopened store reflects the
// bounded queue and the loss — not the pre-overflow contents.
func TestDurableBufferPrunesStoreOnOverflow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	store1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	buf, err := telemetry.NewDurableBuffer(store1, 2) // capacity 2 durable entries
	if err != nil {
		t.Fatalf("NewDurableBuffer: %v", err)
	}
	// Three durable content.played entries → seq1 evicted (drop-oldest), a
	// loss marker [1,1] opens.
	for i := 0; i < 3; i++ {
		buf.Record(telemetry.SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:aa","screen_id":"s"}`), "s", int64(1000+i))
	}
	if err := buf.StoreErr(); err != nil {
		t.Fatalf("StoreErr after overflow: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	entries, markers, err := store2.LoadTelemetry()
	if err != nil {
		t.Fatalf("LoadTelemetry: %v", err)
	}
	var seqs []int64
	for _, e := range entries {
		seqs = append(seqs, e.Seq)
	}
	if len(seqs) != 2 || seqs[0] != 2 || seqs[1] != 3 {
		t.Fatalf("after overflow, durable queue seqs = %v, want [2 3] (seq1 dropped-oldest, REL-096)", seqs)
	}
	if len(markers) != 1 || markers[0].FromSeq != 1 || markers[0].ToSeq != 1 {
		t.Fatalf("after overflow, loss markers = %+v, want one [1,1] marker (REL-096/103)", markers)
	}
}
