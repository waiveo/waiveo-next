package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// This file (Task 3) gates the bounded buffer's drop-oldest overflow policy
// (REL-096) and its loss-marker accounting (REL-100/101/103/104/105), wired to
// REL-090-valid-telemetry-overflow-loss-marker.json. The drop breakdown
// (12 content.played + 8 entity.state_changed over a 20-seq range), the exact
// four-field loss-marker shape, the "dropped counts sum matches the dropped
// range" and "silent_loss:false" invariants, and REL-094's "a superseded
// latest-only entry is never counted in a marker" rule are all asserted against
// the corpus document, not invented.

// rel090Overflow is the subset of REL-090's overflow scenario this task drives
// the Buffer against: the capacity + drop breakdown/range the buffer must
// reproduce, the exact loss marker the corpus expects on the wire, and the
// four-field-shape / sum-matches-range / no-silent-loss expectations.
type rel090Overflow struct {
	Input struct {
		BufferCapacity int `json:"buffer_capacity"`
		DroppedRange   struct {
			FromSeq int64 `json:"from_seq"`
			ToSeq   int64 `json:"to_seq"`
		} `json:"dropped_range"`
		DroppedBreakdown map[string]int `json:"dropped_breakdown"`
		TelemetryPush    struct {
			Body struct {
				LossMarkers []LossMarker `json:"loss_markers"`
			} `json:"body"`
		} `json:"telemetry_push"`
	} `json:"input"`
	Expected struct {
		LossMarkerFieldCount                int      `json:"loss_marker_field_count"`
		LossMarkerFields                    []string `json:"loss_marker_fields"`
		DroppedCountsSumMatchesDroppedRange bool     `json:"dropped_counts_sum_matches_dropped_range"`
		SilentLoss                          bool     `json:"silent_loss"`
	} `json:"expected"`
}

func loadREL090(t *testing.T) rel090Overflow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(rel090Corpus))
	if err != nil {
		t.Fatalf("reading REL-090 corpus: %v", err)
	}
	var doc rel090Overflow
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshaling REL-090 corpus: %v", err)
	}
	return doc
}

// sumCounts totals a dropped_counts_by_schema map.
func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// TestOverflowDropsOldestDurableAndAccountsLossMarker is the REL-090/096 oracle:
// filling a bounded buffer past capacity with durable entries drops the
// lowest-seq ones first (drop-oldest) and produces a single loss marker whose
// dropped_counts_by_schema accounts for exactly the dropped entries by schema,
// in the exact four-field shape REL-100/105 names — matching the corpus's
// {content.played:12, entity.state_changed:8} breakdown over a 20-seq range.
func TestOverflowDropsOldestDurableAndAccountsLossMarker(t *testing.T) {
	corpus := loadREL090(t)
	cap := corpus.Input.BufferCapacity
	breakdown := corpus.Input.DroppedBreakdown
	dropTotal := sumCounts(breakdown) // 12 + 8 = 20

	b := NewBuffer(cap)

	// Record the drop victims first (lowest seqs): exactly the corpus breakdown,
	// contiguously, so their seq range has no latest-only gaps. The order within
	// the batch is irrelevant to the accounting.
	victims := 0
	for schema, count := range breakdown {
		for i := 0; i < count; i++ {
			b.Record(schema, json.RawMessage(`{"k":"v"}`), "", int64(1000+victims))
			victims++
		}
	}
	if victims != dropTotal {
		t.Fatalf("recorded %d victims, want %d", victims, dropTotal)
	}
	firstVictimSeq := b.Pending()[0].Seq
	lastVictimSeq := b.Pending()[victims-1].Seq

	// Now record exactly `cap` survivors (higher seqs); durable count becomes
	// cap+dropTotal, so the buffer must evict the dropTotal lowest-seq durable
	// entries (the victims) to get back to cap.
	for i := 0; i < cap; i++ {
		b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:aa","screen_id":"s"}`), "", int64(2000+i))
	}

	// Drop-oldest: exactly `cap` survivors remain, none with a victim seq.
	pending := b.Pending()
	if len(pending) != cap {
		t.Fatalf("Pending len = %d, want %d survivors after drop-oldest (REL-096)", len(pending), cap)
	}
	for _, e := range pending {
		if e.Seq <= lastVictimSeq {
			t.Fatalf("survivor seq %d is within the dropped victim range [%d,%d]; drop-oldest must evict the LOWEST seqs first (REL-096)", e.Seq, firstVictimSeq, lastVictimSeq)
		}
	}

	// Exactly one loss marker, accounting for exactly the dropped durable
	// entries by schema (REL-100/104).
	markers := b.PendingLossMarkers()
	if len(markers) != 1 {
		t.Fatalf("PendingLossMarkers len = %d, want 1 (single contiguous overflow drop)", len(markers))
	}
	m := markers[0]
	if !reflect.DeepEqual(m.DroppedCountsBySchema, breakdown) {
		t.Fatalf("dropped_counts_by_schema = %v, want the corpus breakdown %v (REL-104)", m.DroppedCountsBySchema, breakdown)
	}
	if m.Reason != ReasonBufferExceeded {
		t.Fatalf("marker reason = %q, want %q (REL-101)", m.Reason, ReasonBufferExceeded)
	}
	if m.FromSeq != firstVictimSeq || m.ToSeq != lastVictimSeq {
		t.Fatalf("marker range = [%d,%d], want the dropped victim range [%d,%d] (REL-100)", m.FromSeq, m.ToSeq, firstVictimSeq, lastVictimSeq)
	}

	// dropped_counts_sum_matches_dropped_range (corpus expected: true) — the 20
	// counted durable drops equal the 20-wide seq range, because this scenario's
	// range has no interleaved latest-only supersessions.
	rangeWidth := int(m.ToSeq-m.FromSeq) + 1
	sumMatchesRange := sumCounts(m.DroppedCountsBySchema) == rangeWidth
	if sumMatchesRange != corpus.Expected.DroppedCountsSumMatchesDroppedRange {
		t.Fatalf("dropped_counts_sum_matches_dropped_range = %v, want %v (corpus)", sumMatchesRange, corpus.Expected.DroppedCountsSumMatchesDroppedRange)
	}

	// silent_loss:false (REL-103) — every dropped durable entry is accounted for
	// by the marker; nothing vanished unmarked.
	recordedDurable := dropTotal + cap
	survivingDurable := len(pending) // all survivors are durable here
	silentLoss := (recordedDurable - survivingDurable) != sumCounts(m.DroppedCountsBySchema)
	if silentLoss != corpus.Expected.SilentLoss {
		t.Fatalf("silent_loss = %v, want %v (REL-103)", silentLoss, corpus.Expected.SilentLoss)
	}
}

// TestOverflowLossMarkerHasExactlyFourFields pins REL-100/105: a produced loss
// marker marshals to exactly the four field names the corpus lists, no more.
func TestOverflowLossMarkerHasExactlyFourFields(t *testing.T) {
	corpus := loadREL090(t)

	b := NewBuffer(1)
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:aa","screen_id":"s"}`), "", 1)
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:bb","screen_id":"s"}`), "", 2)

	markers := b.PendingLossMarkers()
	if len(markers) != 1 {
		t.Fatalf("PendingLossMarkers len = %d, want 1", len(markers))
	}
	blob, err := json.Marshal(markers[0])
	if err != nil {
		t.Fatalf("marshaling loss marker: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(blob, &fields); err != nil {
		t.Fatalf("unmarshaling loss marker: %v", err)
	}
	if len(fields) != corpus.Expected.LossMarkerFieldCount {
		t.Fatalf("loss marker field count = %d, want %d (REL-105): %s", len(fields), corpus.Expected.LossMarkerFieldCount, blob)
	}
	for _, name := range corpus.Expected.LossMarkerFields {
		if _, ok := fields[name]; !ok {
			t.Fatalf("loss marker missing corpus field %q: %s", name, blob)
		}
	}
	// The marker the buffer produces must marshal to the same shape the corpus's
	// own telemetry.push carries (field-for-field; seq/count values differ by
	// scenario, but the corpus marker's field set is the contract).
	corpusMarker := corpus.Input.TelemetryPush.Body.LossMarkers[0]
	cblob, err := json.Marshal(corpusMarker)
	if err != nil {
		t.Fatalf("marshaling corpus loss marker: %v", err)
	}
	var cfields map[string]json.RawMessage
	if err := json.Unmarshal(cblob, &cfields); err != nil {
		t.Fatalf("unmarshaling corpus loss marker: %v", err)
	}
	if len(cfields) != len(fields) {
		t.Fatalf("produced marker field count %d != corpus marker field count %d", len(fields), len(cfields))
	}
	for name := range cfields {
		if _, ok := fields[name]; !ok {
			t.Fatalf("produced marker missing corpus marker field %q", name)
		}
	}
}

// TestOverflowLatestOnlyNeverCountedInLossMarker is the REL-094/104 oracle for
// the overflow path: latest-only entries do not count toward the durable
// capacity and are never evicted or counted by overflow. A latest-only entry
// whose seq falls WITHIN a dropped durable range (because it was superseded and
// removed before the drop) MUST NOT appear in dropped_counts_by_schema, so the
// counted sum is strictly less than the raw seq-range width — exactly the
// distinction the corpus flags with dropped_counts_by_schema_includes_device_heartbeat:false.
func TestOverflowLatestOnlyNeverCountedInLossMarker(t *testing.T) {
	b := NewBuffer(2) // durable capacity = 2

	// seq1 durable A, seq2 heartbeat(subjX), seq3 durable B.
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:a1","screen_id":"s"}`), "", 1)
	b.Record(SchemaDeviceHeartbeat, json.RawMessage(`{"device_id":"X","power_state":"on"}`), "X", 2)
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:b1","screen_id":"s"}`), "", 3)

	// seq4 heartbeat(subjX) supersedes seq2 — the seq-2 latest-only entry is
	// discarded (not loss, REL-094) and leaves a gap in the seq space.
	b.Record(SchemaDeviceHeartbeat, json.RawMessage(`{"device_id":"X","power_state":"off"}`), "X", 4)

	// seq5, seq6 durable C, D — each pushes the durable count to 3 > 2, evicting
	// the lowest-seq durable (A@1, then B@3).
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:c1","screen_id":"s"}`), "", 5)
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:d1","screen_id":"s"}`), "", 6)

	markers := b.PendingLossMarkers()
	if len(markers) != 1 {
		t.Fatalf("PendingLossMarkers len = %d, want 1", len(markers))
	}
	m := markers[0]

	// The superseded heartbeat MUST NOT be counted (REL-104).
	if _, ok := m.DroppedCountsBySchema[SchemaDeviceHeartbeat]; ok {
		t.Fatalf("dropped_counts_by_schema includes %q, but a superseded latest-only entry is not loss (REL-104): %v", SchemaDeviceHeartbeat, m.DroppedCountsBySchema)
	}
	if got := m.DroppedCountsBySchema[SchemaContentPlayed]; got != 2 {
		t.Fatalf("content.played dropped count = %d, want 2 (durable A@1 + B@3)", got)
	}

	// The dropped durable range spans the gap left by the superseded seq-2
	// heartbeat, so the counted sum is strictly less than the raw range width.
	rangeWidth := int(m.ToSeq-m.FromSeq) + 1
	if m.FromSeq != 1 || m.ToSeq != 3 {
		t.Fatalf("marker range = [%d,%d], want [1,3] (lowest..highest dropped durable seq)", m.FromSeq, m.ToSeq)
	}
	if sumCounts(m.DroppedCountsBySchema) >= rangeWidth {
		t.Fatalf("counted drops %d should be < range width %d (a superseded latest-only sits in the range, REL-104)", sumCounts(m.DroppedCountsBySchema), rangeWidth)
	}

	// The buffer keeps the survivors: the latest heartbeat (seq4) + C@5 + D@6.
	pending := b.Pending()
	if len(pending) != 3 {
		t.Fatalf("Pending len = %d, want 3 (latest heartbeat + C + D)", len(pending))
	}
}

// TestLatestOnlyNotCountedTowardCapacity confirms the buffer is bounded by
// durable-class count (REL-096): a flood of distinct-subject latest-only
// entries neither triggers overflow nor evicts a durable entry.
func TestLatestOnlyNotCountedTowardCapacity(t *testing.T) {
	b := NewBuffer(2)
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:a","screen_id":"s"}`), "", 1)
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:b","screen_id":"s"}`), "", 2)
	for i := 0; i < 5; i++ {
		subj := string(rune('A' + i))
		b.Record(SchemaDeviceHeartbeat, json.RawMessage(`{"device_id":"`+subj+`","power_state":"on"}`), subj, int64(10+i))
	}
	if got := len(b.PendingLossMarkers()); got != 0 {
		t.Fatalf("PendingLossMarkers len = %d, want 0 (latest-only never overflows durable capacity, REL-096)", got)
	}
	if got := len(b.Pending()); got != 7 {
		t.Fatalf("Pending len = %d, want 7 (2 durable + 5 distinct-subject heartbeats)", got)
	}
}

// TestNoOverflowNoLossMarker confirms a buffer under capacity produces no loss
// markers.
func TestNoOverflowNoLossMarker(t *testing.T) {
	b := NewBuffer(500)
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:a","screen_id":"s"}`), "", 1)
	b.Record(SchemaAutomationRun, json.RawMessage(`{"rule_id":"r","mode_disposition":"ran"}`), "r", 2)
	if got := len(b.PendingLossMarkers()); got != 0 {
		t.Fatalf("PendingLossMarkers len = %d, want 0 (under capacity)", got)
	}
}

// TestPendingLossMarkersReturnsCopy confirms PendingLossMarkers returns a copy —
// including its DroppedCountsBySchema map — that the caller cannot use to mutate
// the buffer's internal accounting.
func TestPendingLossMarkersReturnsCopy(t *testing.T) {
	b := NewBuffer(1)
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:a","screen_id":"s"}`), "", 1)
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:b","screen_id":"s"}`), "", 2)

	first := b.PendingLossMarkers()
	if len(first) != 1 {
		t.Fatalf("PendingLossMarkers len = %d, want 1", len(first))
	}
	first[0].DroppedCountsBySchema[SchemaContentPlayed] = 999
	first[0].ToSeq = -1

	second := b.PendingLossMarkers()
	if second[0].DroppedCountsBySchema[SchemaContentPlayed] == 999 {
		t.Fatalf("PendingLossMarkers aliased the internal counts map; internal state was mutated")
	}
	if second[0].ToSeq == -1 {
		t.Fatalf("PendingLossMarkers aliased the internal marker; internal state was mutated")
	}
}
