package telemetry

import (
	"encoding/json"
	"sync"
)

// Buffer is the relay's bounded, seq-ordered telemetry buffer (REL-090-096).
// It assigns each recorded entry a per-relay monotonically increasing seq
// (REL-091), retains every durable-class entry (REL-093), and supersedes a
// buffered-but-not-yet-delivered latest-only entry in place when a newer entry
// of the same schema and subject is recorded (REL-094) — a supersession that
// is NOT loss and is never counted in a loss marker (REL-104).
//
// capacity is the bound REL-096's drop-oldest overflow policy enforces; this
// task establishes it at construction, and Task 3 wires the overflow eviction
// and loss-marker accounting against it. A Buffer is safe for concurrent use
// by the entry producer (the engine / device plane) and the flusher (the
// upstream Channel).
type Buffer struct {
	mu          sync.Mutex
	capacity    int
	nextSeq     int64
	entries     []Entry
	lossMarkers []LossMarker
	// store, when non-nil (NewDurableBuffer), is the durable backing this
	// Buffer mirrors durable-class entries and loss markers into so they
	// survive a power-pull (REL-090). A nil store is a plain in-memory buffer.
	store DurableStore
	// storeErr holds the first durable write-through error since the last
	// StoreErr() read (Record/overflow/ack-prune cannot return one directly).
	storeErr error
}

// NewBuffer returns a Buffer bounded to capacity durable-class entries
// (REL-096); seq assignment (REL-091) begins at 1 on the first Record.
func NewBuffer(capacity int) *Buffer {
	return &Buffer{capacity: capacity}
}

// Record buffers one telemetry entry, assigning it the next monotonic seq
// (REL-091) at record time, and returns it.
//
// schema is one of the five events/1 registered schemas this channel carries
// (REL-095); payload is that schema's own field shape, carried unmodified
// (REL-090); subject is the latest-only supersession key REL-094 keys a
// discard on (the relevant payload field per schema — e.g. device_id for
// device.heartbeat, relay_id for box.vitals); atMs is the relay's record-time
// wall-clock reading (reserved for retention/backoff bookkeeping; recording
// order, not atMs, establishes which entry is "newer").
//
// A durable-class entry (REL-093) is always retained. A latest-only entry
// (REL-094 — device.heartbeat, box.vitals) first supersedes any
// buffered-not-yet-delivered entry of the same schema AND subject: that older
// entry is discarded (the newer one already reports everything it would have),
// and — per REL-104 — the discard is NOT loss and produces no loss marker.
func (b *Buffer) Record(schema string, payload json.RawMessage, subject string, atMs int64) Entry {
	b.mu.Lock()
	defer b.mu.Unlock()

	if class, _ := ClassOf(schema); class == LatestOnly {
		b.supersede(schema, subject)
	}

	b.nextSeq++
	e := Entry{Seq: b.nextSeq, Schema: schema, Payload: payload, Subject: subject, RecordedAt: atMs}
	b.entries = append(b.entries, e)
	evicted := b.enforceCapacity()
	b.persistRecord(e, evicted)
	return e
}

// persistRecord mirrors one Record into the durable store (REL-090): the new
// durable-class entry is appended (a latest-only entry is skipped by the
// store's own class gate), and any overflow-evicted lowest-seq durable entries
// (REL-096) are pruned from the store by their highest evicted seq — drop-oldest
// removes a contiguous run of the lowest durable seqs, so every remaining
// durable entry sits above that cursor — with the updated loss-marker set
// written through so the drop is durably accounted for (REL-103). Write-through
// errors are surfaced via StoreErr, never silently swallowed. The caller holds
// b.mu; a nil store makes this a no-op (plain in-memory buffer).
func (b *Buffer) persistRecord(e Entry, evicted []Entry) {
	if b.store == nil {
		return
	}
	b.noteStoreErr(b.store.AppendTelemetry(e))
	if len(evicted) == 0 {
		return
	}
	var maxEvicted int64
	for _, ev := range evicted {
		if ev.Seq > maxEvicted {
			maxEvicted = ev.Seq
		}
	}
	b.noteStoreErr(b.store.PruneTelemetry(maxEvicted))
	b.noteStoreErr(b.store.SaveLossMarkers(b.lossMarkers))
}

// enforceCapacity applies REL-096's bounded drop-oldest overflow policy: the
// Buffer is bounded to capacity DURABLE-class entries (latest-only entries are
// periodic snapshots, kept bounded per subject by supersession (REL-094), and
// are never counted toward capacity nor evicted by overflow). While the durable
// count exceeds capacity, the lowest-seq durable entry is evicted first
// (drop-oldest) and accounted for in a loss marker; a latest-only entry is never
// evicted here and never counted in a marker (REL-104). The caller holds b.mu.
func (b *Buffer) enforceCapacity() []Entry {
	var evicted []Entry
	for b.durableCount() > b.capacity {
		idx := b.lowestDurableIndex()
		if idx < 0 {
			break // no durable entry to evict (should not happen while over capacity)
		}
		e := b.entries[idx]
		b.recordDrop(e)
		evicted = append(evicted, e)
		b.removeAt(idx)
	}
	return evicted
}

// durableCount returns how many buffered entries are durable-class (REL-093) —
// the only entries capacity bounds (REL-096). The caller holds b.mu.
func (b *Buffer) durableCount() int {
	n := 0
	for _, e := range b.entries {
		if class, _ := ClassOf(e.Schema); class == Durable {
			n++
		}
	}
	return n
}

// lowestDurableIndex returns the index of the lowest-seq durable-class entry
// (entries are held in ascending seq order, so it is the first durable one), or
// -1 if none is buffered. The caller holds b.mu.
func (b *Buffer) lowestDurableIndex() int {
	for i, e := range b.entries {
		if class, _ := ClassOf(e.Schema); class == Durable {
			return i
		}
	}
	return -1
}

// recordDrop accounts for one evicted durable-class entry in the buffer's loss
// markers (REL-100/103): silent loss of a durable entry is forbidden, so each
// eviction extends the current open marker (drops are monotonic in seq, so the
// contiguous overflow run coalesces into one bounded-range marker whose range
// may span gaps left by superseded latest-only entries) or opens the first one.
// Reason is always buffer_exceeded (REL-101). The caller holds b.mu.
func (b *Buffer) recordDrop(e Entry) {
	if n := len(b.lossMarkers); n > 0 {
		m := &b.lossMarkers[n-1]
		if e.Seq < m.FromSeq {
			m.FromSeq = e.Seq
		}
		if e.Seq > m.ToSeq {
			m.ToSeq = e.Seq
		}
		m.DroppedCountsBySchema[e.Schema]++
		return
	}
	b.lossMarkers = append(b.lossMarkers, LossMarker{
		FromSeq:               e.Seq,
		ToSeq:                 e.Seq,
		DroppedCountsBySchema: map[string]int{e.Schema: 1},
		Reason:                ReasonBufferExceeded,
	})
}

// removeAt drops the entry at index i, preserving ascending seq order, and zeros
// the freed tail slot so the evicted Entry (and its payload) is not retained by
// the backing array. The caller holds b.mu.
func (b *Buffer) removeAt(i int) {
	copy(b.entries[i:], b.entries[i+1:])
	b.entries[len(b.entries)-1] = Entry{}
	b.entries = b.entries[:len(b.entries)-1]
}

// supersede drops any buffered entry of the given (latest-only) schema and
// subject so the caller can append the newer one in its place (REL-094). The
// caller holds b.mu. Only latest-only schemas reach here, so this never
// coalesces a durable-class entry (REL-093).
func (b *Buffer) supersede(schema, subject string) {
	kept := b.entries[:0]
	for _, e := range b.entries {
		if e.Schema == schema && e.Subject == subject {
			continue // superseded by the newer entry about to be recorded
		}
		kept = append(kept, e)
	}
	// Zero out the tail freed by the in-place filter so superseded Entry
	// values (and their payloads) are not retained by the backing array.
	for i := len(kept); i < len(b.entries); i++ {
		b.entries[i] = Entry{}
	}
	b.entries = kept
}

// Pending returns the buffered, not-yet-delivered entries in ascending seq
// order (REL-091), as a copy the caller may freely mutate without affecting
// the buffer's internal state.
func (b *Buffer) Pending() []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]Entry, len(b.entries))
	copy(out, b.entries)
	return out
}

// PendingLossMarkers returns the buffer's not-yet-acknowledged loss markers
// (REL-096/100), which ride the next telemetry.push alongside Pending entries so
// no durable-class drop is ever silently lost (REL-103). Each returned marker is
// a deep copy — including its DroppedCountsBySchema map — so the caller may
// freely mutate it without affecting the buffer's internal accounting; the
// result is a non-nil (possibly empty) slice.
func (b *Buffer) PendingLossMarkers() []LossMarker {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]LossMarker, len(b.lossMarkers))
	for i, m := range b.lossMarkers {
		counts := make(map[string]int, len(m.DroppedCountsBySchema))
		for k, v := range m.DroppedCountsBySchema {
			counts[k] = v
		}
		out[i] = LossMarker{
			FromSeq:               m.FromSeq,
			ToSeq:                 m.ToSeq,
			DroppedCountsBySchema: counts,
			Reason:                m.Reason,
		}
	}
	return out
}
