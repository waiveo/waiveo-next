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
	entries      []Entry
	markers      []LossMarker
	seqHighWater int64
	// preload is what LoadTelemetry hands back on construction.
	preloadEntries []Entry
	preloadMarkers []LossMarker
	// opLog records the durable operations in order, so a test can assert the
	// overflow path takes the atomic AppendWithEviction and not the separate
	// prune-then-save-markers pair the pre-fix code used (REL-103).
	opLog []string
}

func (f *fakeDurableStore) AppendTelemetry(e Entry) error {
	f.opLog = append(f.opLog, "append")
	if class, ok := ClassOf(e.Schema); !ok || class != Durable {
		return nil // latest-only / unknown: not durable (REL-094)
	}
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeDurableStore) AppendWithEviction(appended Entry, pruneThroughSeq int64, markers []LossMarker) error {
	f.opLog = append(f.opLog, "append_with_eviction")
	if class, ok := ClassOf(appended.Schema); ok && class == Durable {
		f.entries = append(f.entries, appended)
	}
	kept := f.entries[:0]
	for _, e := range f.entries {
		if e.Seq > pruneThroughSeq {
			kept = append(kept, e)
		}
	}
	f.entries = kept
	f.markers = append([]LossMarker(nil), markers...)
	return nil
}

func (f *fakeDurableStore) PruneTelemetry(ackThroughSeq int64) error {
	f.opLog = append(f.opLog, "prune")
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
	f.opLog = append(f.opLog, "save_markers")
	f.markers = append([]LossMarker(nil), markers...)
	return nil
}

func (f *fakeDurableStore) SaveSeqHighWater(seq int64) error {
	if seq > f.seqHighWater {
		f.seqHighWater = seq
	}
	return nil
}

func (f *fakeDurableStore) LoadSeqHighWater() (int64, error) {
	return f.seqHighWater, nil
}

func (f *fakeDurableStore) LoadTelemetry() ([]Entry, []LossMarker, error) {
	return f.preloadEntries, f.preloadMarkers, nil
}

// reopen models a relay restart: a fresh store whose reload set (and persisted
// seq high-water) is the durable state this one holds right now.
func (f *fakeDurableStore) reopen() *fakeDurableStore {
	return &fakeDurableStore{
		preloadEntries: append([]Entry(nil), f.entries...),
		preloadMarkers: append([]LossMarker(nil), f.markers...),
		seqHighWater:   f.seqHighWater,
	}
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

// TestOverflowPersistsAtomicallyNotAsSeparateWrites is the REL-096/103 oracle
// for the write-ordering fix: an overflow-triggering Record MUST mirror its
// durable effect through the single atomic AppendWithEviction, never as a
// separate PruneTelemetry-then-SaveLossMarkers pair — the pre-fix split let a
// crash between the prune and the marker save leave an evicted durable entry
// gone with no loss marker accounting for it (silent durable loss REL-103
// forbids). Here capacity 1 with two durable records evicts seq1; the op log
// must show the atomic call and NOT the un-atomic prune/save-markers pair.
func TestOverflowPersistsAtomicallyNotAsSeparateWrites(t *testing.T) {
	store := &fakeDurableStore{}
	buf, err := NewDurableBuffer(store, 1)
	if err != nil {
		t.Fatalf("NewDurableBuffer: %v", err)
	}
	buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:a","screen_id":"s"}`), "s", 1) // seq1
	buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:b","screen_id":"s"}`), "s", 2) // seq2 → evict seq1
	if err := buf.StoreErr(); err != nil {
		t.Fatalf("StoreErr: %v", err)
	}

	for _, op := range store.opLog {
		if op == "prune" || op == "save_markers" {
			t.Fatalf("overflow persisted via un-atomic %q; the eviction+prune+marker write must be one atomic AppendWithEviction (REL-103). opLog=%v", op, store.opLog)
		}
	}
	var atomic int
	for _, op := range store.opLog {
		if op == "append_with_eviction" {
			atomic++
		}
	}
	if atomic != 1 {
		t.Fatalf("AppendWithEviction called %d times, want exactly 1 for the single overflow (opLog=%v)", atomic, store.opLog)
	}

	// The atomic call durably reflects BOTH the drop (seq1 gone) and the loss
	// marker accounting for it — never one without the other.
	if len(store.entries) != 1 || store.entries[0].Seq != 2 {
		t.Fatalf("durable entries = %+v, want only seq2 (seq1 evicted, REL-096)", store.entries)
	}
	if len(store.markers) != 1 || store.markers[0].FromSeq != 1 || store.markers[0].ToSeq != 1 {
		t.Fatalf("durable markers = %+v, want one [1,1] marker accounting for the evicted seq1 (REL-103)", store.markers)
	}
}

// TestSeqHighWaterResumesAboveLatestOnlyAcrossRestart is the REL-091 oracle for
// the seq-monotonicity fix: a latest-only entry consumes a seq from the SAME
// shared counter as durable entries but is NOT persisted (REL-094), so a
// restart that resumed only from the reloaded durable max would reissue that
// latest-only seq to a different entry. The relay must instead resume above the
// persisted seq high-water. Scenario: durable seq1 (persisted), latest-only
// seq2 (high-water only) — the pre-crash push carried both to the app peer.
// After a restart the next durable Record MUST be seq3, never a second seq2.
func TestSeqHighWaterResumesAboveLatestOnlyAcrossRestart(t *testing.T) {
	store := &fakeDurableStore{}
	buf, err := NewDurableBuffer(store, 500)
	if err != nil {
		t.Fatalf("NewDurableBuffer: %v", err)
	}
	d := buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:a","screen_id":"s"}`), "s", 1)
	if d.Seq != 1 {
		t.Fatalf("durable seq = %d, want 1", d.Seq)
	}
	h := buf.Record(SchemaDeviceHeartbeat, json.RawMessage(`{"device_id":"dev","power_state":"on"}`), "dev", 2)
	if h.Seq != 2 {
		t.Fatalf("latest-only seq = %d, want 2", h.Seq)
	}
	if err := buf.StoreErr(); err != nil {
		t.Fatalf("StoreErr: %v", err)
	}
	// Only the durable seq1 is in the reload set; the latest-only seq2 is not.
	if len(store.entries) != 1 || store.entries[0].Seq != 1 {
		t.Fatalf("durable entries = %+v, want only seq1 (latest-only not persisted, REL-094)", store.entries)
	}

	// Restart: a fresh buffer reloads the durable backlog + the seq high-water.
	buf2, err := NewDurableBuffer(store.reopen(), 500)
	if err != nil {
		t.Fatalf("NewDurableBuffer (reopen): %v", err)
	}
	next := buf2.Record(SchemaAutomationRun, json.RawMessage(`{"rule_id":"r","mode_disposition":"ran"}`), "r", 3)
	if next.Seq != 3 {
		t.Fatalf("post-restart seq = %d, want 3 — must not reissue the latest-only seq 2 the app peer already saw (REL-091)", next.Seq)
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
