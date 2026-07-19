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
	mu       sync.Mutex
	capacity int
	nextSeq  int64
	entries  []Entry
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
	_ = atMs

	b.mu.Lock()
	defer b.mu.Unlock()

	if class, _ := ClassOf(schema); class == LatestOnly {
		b.supersede(schema, subject)
	}

	b.nextSeq++
	e := Entry{Seq: b.nextSeq, Schema: schema, Payload: payload, Subject: subject}
	b.entries = append(b.entries, e)
	return e
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
