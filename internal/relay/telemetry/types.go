package telemetry

import "encoding/json"

// Entry is one telemetry-buffer record. Seq is the per-relay monotonically
// increasing integer REL-091 assigns at record time; Schema is one of the
// five events/1 registered schemas this channel carries (REL-095); Payload is
// that schema's own field shape, carried unmodified (REL-090/095) — this
// package never redefines a schema's fields. Subject is the latest-only
// supersession key REL-094 keys a discard on (schema + Subject) — the
// relevant payload field per schema (e.g. device_id for device.heartbeat,
// relay_id for box.vitals); it is a Buffer bookkeeping field only and is
// never part of the {seq,schema,payload} wire shape REL-090 defines.
//
// TraceID is the correlation id REL-006 requires a relay/1 message to carry
// when its work traces to a single originating operation, "so one identifier
// correlates the operation across the app peer, the relay, and any durable
// record the operation eventually produces". This entry IS the relay's own
// record of that work, and the durable record it eventually produces is the
// events/1 envelope the app peer reconstructs from it — whose `trace_id`
// EVT-010 defines as "the originating operation's trace ID, propagated from
// wherever the event was recorded". The event is recorded HERE, at the relay,
// so this is the field that carries it; without it the app-side ingest has
// nothing to propagate and has to fabricate an uncorrelated value, which makes
// api/1 API-063's promise false for every relay-originated event.
//
// It rides PER ENTRY, not per telemetry.push batch, for the reason REL-006
// itself gives: the trace identifies ONE originating operation. A push batches
// entries recorded independently over a whole disconnection window — a rule
// firing, a playback, a state transition — each with its own causal chain. A
// batch-level trace id would give unrelated events one shared value, which
// corrupts correlation in the other direction (a trace fanning out across work
// it never caused) rather than establishing it.
//
// It is optional on the wire (`omitempty`), which is a deliberate compatibility
// statement, not laxity: an older relay predating this field sends no trace_id
// at all, and the app-side ingest MUST still be able to deliver its events. See
// internal/app/eventingest.resolveTraceID for what the app peer does with an
// absent — or malformed — value, and why neither may poison the envelope.
// REL-090a is the requirement that publishes it: the entry MAY carry `trace_id`,
// the app peer MUST propagate a valid one into the reconstructed envelope
// unmodified, and an absent or malformed value MUST NOT cost the entry its
// delivery. (REL-105's "exactly these fields, no more" clause is scoped to the
// loss-marker object alone; REL-090's entry shape carries no such closure.)
type Entry struct {
	Seq     int64           `json:"seq"`
	Schema  string          `json:"schema"`
	Payload json.RawMessage `json:"payload"`
	TraceID string          `json:"trace_id,omitempty"`
	Subject string          `json:"-"`
	// RecordedAt is the relay's record-time wall-clock reading (the atMs
	// passed to Record), retained as durable retention/backoff bookkeeping
	// (the recorded_at column of a durable-backed queue) — like Subject, it is
	// a Buffer/store bookkeeping field only and never part of the
	// {seq,schema,payload} wire shape REL-090 defines (`json:"-"`).
	RecordedAt int64 `json:"-"`
}

// LossMarker is REL-100's loss-marker shape: exactly `{from_seq, to_seq,
// dropped_counts_by_schema, reason}`, no more (REL-105) — events/1 EVT-144's
// own named exception to its three-field subscriber-stream gap shape.
// FromSeq/ToSeq bound the dropped seq range (the lowest/highest seq dropped);
// DroppedCountsBySchema counts only durable-class entries actually dropped in
// that range, by schema (REL-104) — a latest-only discard (REL-094) MUST
// NEVER appear here; Reason is "buffer_exceeded" for every marker this
// contract's overflow policy produces (REL-101, ReasonBufferExceeded).
type LossMarker struct {
	FromSeq               int64          `json:"from_seq"`
	ToSeq                 int64          `json:"to_seq"`
	DroppedCountsBySchema map[string]int `json:"dropped_counts_by_schema"`
	Reason                string         `json:"reason"`
}

// ReasonBufferExceeded is REL-101's sole defined loss-marker Reason value.
const ReasonBufferExceeded = "buffer_exceeded"

// PushBatch is a telemetry.push body (REL-090): the buffered Entries (seq
// order) plus every LossMarker not yet acknowledged. LossMarkers MUST be
// present, possibly as an empty (non-nil) slice, on every push — a producer
// building a PushBatch MUST NOT leave it nil, since REL-090 requires the
// field on the wire even when there is nothing to report.
type PushBatch struct {
	Entries     []Entry      `json:"entries"`
	LossMarkers []LossMarker `json:"loss_markers"`
}

// SeqRange identifies one loss marker's {from_seq, to_seq} pair, as carried
// in an Ack's LossMarkersAcked (REL-092) to tell the relay which delivered
// loss markers the app peer has received.
type SeqRange struct {
	FromSeq int64 `json:"from_seq"`
	ToSeq   int64 `json:"to_seq"`
}

// Ack is a telemetry.ack body (REL-092): AckThroughSeq is the highest
// ordinary-entry seq the app peer received — the relay MUST NOT discard any
// buffered entry whose seq exceeds it, and MAY discard entries at or below it
// once acknowledged. LossMarkersAcked names which delivered loss markers the
// app peer has received, by their {from_seq, to_seq} pair.
type Ack struct {
	AckThroughSeq    int64      `json:"ack_through_seq"`
	LossMarkersAcked []SeqRange `json:"loss_markers_acked"`
}
