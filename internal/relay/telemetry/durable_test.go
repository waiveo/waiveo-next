package telemetry

import (
	"encoding/json"
	"testing"
)

// fakeDurableStore is an in-memory DurableStore for exercising the Buffer's
// durable write-through wiring in isolation from the real SQLite store — it
// mimics the durable-class gate and the seq-cursor prune the operational
// store enforces (REL-090/092/094).
type fakeDurableStore struct {
	entries []Entry
	markers []LossMarker
	// preload is what LoadTelemetry hands back on construction.
	preloadEntries []Entry
	preloadMarkers []LossMarker
}

func (f *fakeDurableStore) AppendTelemetry(e Entry) error {
	if class, ok := ClassOf(e.Schema); !ok || class != Durable {
		return nil // latest-only / unknown: not durable (REL-094)
	}
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeDurableStore) PruneTelemetry(ackThroughSeq int64) error {
	kept := f.entries[:0]
	for _, e := range f.entries {
		if e.Seq > ackThroughSeq {
			kept = append(kept, e)
		}
	}
	f.entries = kept
	return nil
}

func (f *fakeDurableStore) SaveLossMarkers(markers []LossMarker) error {
	f.markers = append([]LossMarker(nil), markers...)
	return nil
}

func (f *fakeDurableStore) LoadTelemetry() ([]Entry, []LossMarker, error) {
	return f.preloadEntries, f.preloadMarkers, nil
}

// TestNewDurableBufferReloadsPersistedEntries confirms a durable Buffer picks
// up its persisted backlog on construction (REL-090: entries buffered durably
// while disconnected resume after a restart) and continues seq assignment
// above the highest reloaded seq (REL-091 monotonicity across a restart).
func TestNewDurableBufferReloadsPersistedEntries(t *testing.T) {
	store := &fakeDurableStore{
		preloadEntries: []Entry{
			{Seq: 7, Schema: SchemaContentPlayed, Payload: json.RawMessage(`{"asset_ref":"sha256:a","screen_id":"s"}`), Subject: "s"},
			{Seq: 9, Schema: SchemaAutomationRun, Payload: json.RawMessage(`{"rule_id":"r","mode_disposition":"ran"}`), Subject: "r"},
		},
	}
	buf, err := NewDurableBuffer(store, 500)
	if err != nil {
		t.Fatalf("NewDurableBuffer: %v", err)
	}

	pending := buf.Pending()
	if len(pending) != 2 || pending[0].Seq != 7 || pending[1].Seq != 9 {
		t.Fatalf("reloaded Pending = %+v, want the two persisted entries (seq 7, 9)", pending)
	}

	// A new Record must continue monotonically ABOVE the highest reloaded seq.
	rec := buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:b","screen_id":"s2"}`), "s2", 1_000)
	if rec.Seq != 10 {
		t.Fatalf("post-reload Record seq = %d, want 10 (continue above highest reloaded seq 9, REL-091)", rec.Seq)
	}
}

// TestDurableBufferWritesThroughOnRecord confirms a durable-class Record is
// written to the store, while a latest-only Record is not (REL-093/094).
func TestDurableBufferWritesThroughOnRecord(t *testing.T) {
	store := &fakeDurableStore{}
	buf, err := NewDurableBuffer(store, 500)
	if err != nil {
		t.Fatalf("NewDurableBuffer: %v", err)
	}

	buf.Record(SchemaAutomationRun, json.RawMessage(`{"rule_id":"r","mode_disposition":"ran"}`), "r", 1)
	buf.Record(SchemaDeviceHeartbeat, json.RawMessage(`{"device_id":"d","power_state":"on"}`), "d", 2)
	if err := buf.StoreErr(); err != nil {
		t.Fatalf("StoreErr: %v", err)
	}

	if len(store.entries) != 1 || store.entries[0].Schema != SchemaAutomationRun {
		t.Fatalf("store.entries = %+v, want only the durable automation.run written through (REL-093/094)", store.entries)
	}
}

// TestDurableBufferPrunesStoreOnAck confirms an ack prunes the durable store
// through the same cursor it prunes the in-memory buffer with (REL-092), so
// the durable backlog does not outlive its acknowledgment.
func TestDurableBufferPrunesStoreOnAck(t *testing.T) {
	store := &fakeDurableStore{}
	buf, err := NewDurableBuffer(store, 500)
	if err != nil {
		t.Fatalf("NewDurableBuffer: %v", err)
	}
	buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:a","screen_id":"s"}`), "s", 1) // seq1
	buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:b","screen_id":"s"}`), "s", 2) // seq2
	if len(store.entries) != 2 {
		t.Fatalf("store.entries len = %d before ack, want 2", len(store.entries))
	}

	buf.applyAck(1, nil) // ack through seq1
	if err := buf.StoreErr(); err != nil {
		t.Fatalf("StoreErr: %v", err)
	}
	if len(store.entries) != 1 || store.entries[0].Seq != 2 {
		t.Fatalf("store.entries after ack(1) = %+v, want only seq2 (≤1 pruned, REL-092)", store.entries)
	}
}

// TestNilStoreBufferIsPlainInMemory confirms a Buffer with no durable store
// (the NewBuffer path) still works and never touches a store — durability is
// opt-in via NewDurableBuffer.
func TestNilStoreBufferIsPlainInMemory(t *testing.T) {
	b := NewBuffer(500)
	b.Record(SchemaAutomationRun, json.RawMessage(`{"rule_id":"r","mode_disposition":"ran"}`), "r", 1)
	if err := b.StoreErr(); err != nil {
		t.Fatalf("StoreErr on a non-durable buffer: %v", err)
	}
	if got := len(b.Pending()); got != 1 {
		t.Fatalf("Pending len = %d, want 1", got)
	}
}
