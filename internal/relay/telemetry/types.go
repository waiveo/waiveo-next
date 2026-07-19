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
type Entry struct {
	Seq     int64           `json:"seq"`
	Schema  string          `json:"schema"`
	Payload json.RawMessage `json:"payload"`
	Subject string          `json:"-"`
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
