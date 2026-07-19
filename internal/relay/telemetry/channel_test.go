package telemetry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This file (Task 4) gates the telemetry upstream Channel: batch push shape
// (REL-090 — loss_markers present, possibly empty, on every push), cursor ack
// retention (REL-092 — discard entries at/below ack_through_seq, NEVER above),
// acked-loss-marker retirement + resend-until-acked (REL-102), and backoff retry
// of an unacknowledged batch without re-sending entries already covered by a
// received ack_through_seq (REL-097). The overflow-then-recover scenario is wired
// to REL-090-valid-telemetry-overflow-loss-marker.json and the latest-only
// supersession-before-push scenario to
// REL-094-valid-telemetry-latest-only-heartbeat-superseded.json; the exact
// loss-marker range, the ack_through_seq, and the loss_markers_acked pair are
// asserted from those corpus documents, not invented.

// scriptedUpstream is a fake app-peer transport (Upstream): it records every
// PushBatch it receives and returns a scripted (Ack, error) per call, so a test
// can drive partial acks, failures, and reconnects deterministically.
type scriptedUpstream struct {
	pushes    []PushBatch
	responses []upstreamResponse
}

type upstreamResponse struct {
	ack Ack
	err error
}

func (u *scriptedUpstream) Push(b PushBatch) (Ack, error) {
	u.pushes = append(u.pushes, b)
	if len(u.responses) == 0 {
		return Ack{}, nil
	}
	r := u.responses[0]
	u.responses = u.responses[1:]
	return r.ack, r.err
}

func (u *scriptedUpstream) lastPush() PushBatch {
	return u.pushes[len(u.pushes)-1]
}

// seqsOf lists the entry seqs of a pushed batch, for asserting what was (not)
// sent.
func seqsOf(b PushBatch) []int64 {
	out := make([]int64, len(b.Entries))
	for i, e := range b.Entries {
		out[i] = e.Seq
	}
	return out
}

// noWaitBackoff retries up to MaxAttempts times and records each requested wait
// via the injected sleep, without ever actually sleeping.
func noWaitChannel(buf *Buffer, up Upstream, maxAttempts int) (*Channel, *[]time.Duration) {
	waits := &[]time.Duration{}
	ch := NewChannel(buf, up, ExponentialBackoff{Base: 10 * time.Millisecond, Max: time.Second, MaxAttempts: maxAttempts})
	ch.sleep = func(d time.Duration) { *waits = append(*waits, d) }
	return ch, waits
}

// TestFlushCarriesEntriesAndLossMarkersPresent is the REL-090 push-shape oracle:
// a Flush builds a PushBatch from the buffer's pending entries plus its loss
// markers, and loss_markers is present (non-nil) on the wire even when there is
// nothing to report (an empty, non-nil slice).
func TestFlushCarriesEntriesAndLossMarkersPresent(t *testing.T) {
	buf := NewBuffer(500)
	buf.Record(SchemaAutomationRun, json.RawMessage(`{"rule_id":"r","mode_disposition":"ran"}`), "r", 1)
	buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:a","screen_id":"s"}`), "", 2)

	up := &scriptedUpstream{responses: []upstreamResponse{{ack: Ack{AckThroughSeq: 2}}}}
	ch := NewChannel(buf, up, ExponentialBackoff{Base: time.Millisecond, MaxAttempts: 1})

	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(up.pushes) != 1 {
		t.Fatalf("push count = %d, want 1", len(up.pushes))
	}
	got := up.lastPush()
	if len(got.Entries) != 2 {
		t.Fatalf("pushed entries = %d, want 2", len(got.Entries))
	}
	// REL-090: loss_markers MUST be present (non-nil) even with nothing dropped.
	if got.LossMarkers == nil {
		t.Fatalf("pushed loss_markers is nil; REL-090 requires it present (possibly empty)")
	}
	if len(got.LossMarkers) != 0 {
		t.Fatalf("pushed loss_markers len = %d, want 0 (no overflow)", len(got.LossMarkers))
	}
	// The empty batch is fully acked → buffer drained.
	if len(buf.Pending()) != 0 {
		t.Fatalf("Pending after full ack = %d, want 0", len(buf.Pending()))
	}
}

// TestFlushNothingPending pushes nothing when the buffer is empty (no wire
// traffic for an idle relay).
func TestFlushNothingPending(t *testing.T) {
	buf := NewBuffer(500)
	up := &scriptedUpstream{}
	ch := NewChannel(buf, up, ExponentialBackoff{Base: time.Millisecond, MaxAttempts: 1})
	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(up.pushes) != 0 {
		t.Fatalf("push count = %d, want 0 (nothing pending)", len(up.pushes))
	}
}

// TestPartialAckRetainsAboveDiscardsBelow is the REL-092 cursor oracle: a
// partial ack_through_seq discards only entries at/below it and retains every
// entry above it; the next flush re-sends only the retained (unacked) entries
// and NEVER an entry already covered by the received ack_through_seq (REL-097).
func TestPartialAckRetainsAboveDiscardsBelow(t *testing.T) {
	buf := NewBuffer(500)
	buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:a","screen_id":"s"}`), "", 1) // seq1
	buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:b","screen_id":"s"}`), "", 2) // seq2
	buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:c","screen_id":"s"}`), "", 3) // seq3

	// Partial ack: peer received through seq2 only.
	up := &scriptedUpstream{responses: []upstreamResponse{{ack: Ack{AckThroughSeq: 2}}}}
	ch := NewChannel(buf, up, ExponentialBackoff{Base: time.Millisecond, MaxAttempts: 1})

	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush #1: %v", err)
	}
	pending := buf.Pending()
	if len(pending) != 1 || pending[0].Seq != 3 {
		t.Fatalf("Pending after partial ack = %+v, want only seq3 (REL-092: never discard above ack)", seqsOf(PushBatch{Entries: pending}))
	}

	// Second flush must re-send ONLY seq3 — never the acked seq1/seq2 (REL-097).
	up.responses = []upstreamResponse{{ack: Ack{AckThroughSeq: 3}}}
	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush #2: %v", err)
	}
	second := up.pushes[1]
	if got := seqsOf(second); len(got) != 1 || got[0] != 3 {
		t.Fatalf("second push seqs = %v, want [3] (must not re-send acked seq1/seq2, REL-097)", got)
	}
	if len(buf.Pending()) != 0 {
		t.Fatalf("Pending after full ack = %d, want 0", len(buf.Pending()))
	}
}

// TestFailedPushRetriesWithBackoffNoDuplicateAcked is the REL-097 retry oracle:
// a failed push is retried with backoff (each wait taken from the policy), and
// once a prior partial ack has advanced the cursor, no retry re-sends an entry
// already covered by that ack_through_seq.
func TestFailedPushRetriesWithBackoffNoDuplicateAcked(t *testing.T) {
	buf := NewBuffer(500)
	buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:a","screen_id":"s"}`), "", 1) // seq1
	buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:b","screen_id":"s"}`), "", 2) // seq2

	// Flush #1: partial ack through seq1 (removes seq1 from the buffer).
	up := &scriptedUpstream{responses: []upstreamResponse{{ack: Ack{AckThroughSeq: 1}}}}
	ch, waits := noWaitChannel(buf, up, 5)
	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush #1: %v", err)
	}

	// Flush #2: upstream fails twice, then succeeds. Each attempt must carry ONLY
	// seq2 — never the already-acked seq1.
	boom := errors.New("upstream unavailable")
	up.responses = []upstreamResponse{
		{err: boom},
		{err: boom},
		{ack: Ack{AckThroughSeq: 2}},
	}
	before := len(up.pushes)
	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush #2 should succeed after retries: %v", err)
	}
	retryPushes := up.pushes[before:]
	if len(retryPushes) != 3 {
		t.Fatalf("attempts in Flush #2 = %d, want 3 (2 failures + 1 success)", len(retryPushes))
	}
	for i, b := range retryPushes {
		got := seqsOf(b)
		if len(got) != 1 || got[0] != 2 {
			t.Fatalf("retry attempt %d seqs = %v, want [2] (never re-send acked seq1, REL-097)", i, got)
		}
	}
	// Two failures → two backoff waits taken from the policy.
	if len(*waits) != 2 {
		t.Fatalf("backoff waits = %d, want 2 (one per failed attempt, REL-097)", len(*waits))
	}
	if (*waits)[1] <= (*waits)[0] {
		t.Fatalf("backoff not increasing: %v", *waits)
	}
	if len(buf.Pending()) != 0 {
		t.Fatalf("Pending after final ack = %d, want 0", len(buf.Pending()))
	}
}

// TestRetryExhaustedLeavesBatchBuffered confirms that when backoff attempts are
// exhausted, Flush returns the error and leaves the unacked batch buffered for
// the next reconnect (REL-097 — retry across reconnects), never discarding an
// entry above any ack.
func TestRetryExhaustedLeavesBatchBuffered(t *testing.T) {
	buf := NewBuffer(500)
	buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:a","screen_id":"s"}`), "", 1)

	boom := errors.New("still offline")
	up := &scriptedUpstream{responses: []upstreamResponse{{err: boom}, {err: boom}}}
	ch, _ := noWaitChannel(buf, up, 1) // 1 retry, then give up

	_, err := ch.Flush()
	if !errors.Is(err, boom) {
		t.Fatalf("Flush err = %v, want the upstream error", err)
	}
	if len(buf.Pending()) != 1 {
		t.Fatalf("Pending after exhausted retry = %d, want 1 (batch survives for next reconnect)", len(buf.Pending()))
	}

	// A later reconnect (fresh Flush) delivers the still-buffered entry.
	up.responses = []upstreamResponse{{ack: Ack{AckThroughSeq: 1}}}
	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush after reconnect: %v", err)
	}
	if len(buf.Pending()) != 0 {
		t.Fatalf("Pending after reconnect ack = %d, want 0", len(buf.Pending()))
	}
}

// TestLossMarkerResendUntilAcked is the REL-102 oracle: a not-yet-acknowledged
// loss marker rides every push until its {from_seq,to_seq} pair appears in a
// received loss_markers_acked; once acked it is retired and never re-sent.
func TestLossMarkerResendUntilAcked(t *testing.T) {
	buf := NewBuffer(1)
	buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:a","screen_id":"s"}`), "", 1) // seq1
	buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:b","screen_id":"s"}`), "", 2) // seq2 → drops seq1, loss marker [1,1]

	markers := buf.PendingLossMarkers()
	if len(markers) != 1 {
		t.Fatalf("setup: PendingLossMarkers = %d, want 1", len(markers))
	}
	lm := markers[0]

	// Flush #1: ack the entry (seq2) but NOT the loss marker → marker survives.
	up := &scriptedUpstream{responses: []upstreamResponse{{ack: Ack{AckThroughSeq: 2}}}}
	ch := NewChannel(buf, up, ExponentialBackoff{Base: time.Millisecond, MaxAttempts: 1})
	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush #1: %v", err)
	}
	if len(up.pushes[0].LossMarkers) != 1 {
		t.Fatalf("push #1 loss_markers = %d, want 1", len(up.pushes[0].LossMarkers))
	}
	if len(buf.PendingLossMarkers()) != 1 {
		t.Fatalf("loss marker retired without an ack; REL-102 requires resend-until-acked")
	}

	// Flush #2: re-sends the still-unacked marker (REL-102), and this ack names it.
	up.responses = []upstreamResponse{{ack: Ack{
		AckThroughSeq:    2,
		LossMarkersAcked: []SeqRange{{FromSeq: lm.FromSeq, ToSeq: lm.ToSeq}},
	}}}
	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush #2: %v", err)
	}
	if len(up.pushes[1].LossMarkers) != 1 {
		t.Fatalf("push #2 must re-send the unacked marker (REL-102)")
	}
	if got := len(buf.PendingLossMarkers()); got != 0 {
		t.Fatalf("PendingLossMarkers after ack = %d, want 0 (acked marker retired, REL-092/102)", got)
	}

	// Flush #3: nothing left — no push.
	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush #3: %v", err)
	}
	if len(up.pushes) != 2 {
		t.Fatalf("push count = %d, want 2 (nothing left to send after marker acked)", len(up.pushes))
	}
}

// ---- corpus-wired drivers ----

// rel090Ack extracts the corpus's telemetry.ack (ack_through_seq +
// loss_markers_acked) and its expected loss-marker range from
// REL-090-valid-telemetry-overflow-loss-marker.json.
func loadREL090AckAndMarker(t *testing.T) (capacity int, markersAcked []SeqRange, dropFrom, dropTo int64, breakdown map[string]int) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(rel090Corpus))
	if err != nil {
		t.Fatalf("reading REL-090 corpus: %v", err)
	}
	var doc struct {
		Input struct {
			BufferCapacity int `json:"buffer_capacity"`
			DroppedRange   struct {
				FromSeq int64 `json:"from_seq"`
				ToSeq   int64 `json:"to_seq"`
			} `json:"dropped_range"`
			DroppedBreakdown map[string]int `json:"dropped_breakdown"`
		} `json:"input"`
		Expected struct {
			TelemetryAck struct {
				Body struct {
					AckThroughSeq    int64      `json:"ack_through_seq"`
					LossMarkersAcked []SeqRange `json:"loss_markers_acked"`
				} `json:"body"`
			} `json:"telemetry_ack"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshaling REL-090 corpus: %v", err)
	}
	return doc.Input.BufferCapacity,
		doc.Expected.TelemetryAck.Body.LossMarkersAcked,
		doc.Input.DroppedRange.FromSeq, doc.Input.DroppedRange.ToSeq,
		doc.Input.DroppedBreakdown
}

// TestREL090CorpusChannelPushThenAckClearsLossMarker replays REL-090 against the
// Channel: overflow produces the exact loss marker (from_seq..to_seq over the
// corpus breakdown), the push carries it, and the corpus's telemetry.ack
// (ack_through_seq=1002, loss_markers_acked=[{980,999}]) drains the buffer and
// retires the marker.
func TestREL090CorpusChannelPushThenAckClearsLossMarker(t *testing.T) {
	capacity, markersAcked, dropFrom, dropTo, breakdown := loadREL090AckAndMarker(t)
	if len(markersAcked) != 1 {
		t.Fatalf("REL-090 corpus loss_markers_acked len = %d, want 1", len(markersAcked))
	}

	// Reproduce the corpus drop with the corpus's own buffer_capacity: record
	// dropCount victims (lowest seqs, split by the corpus breakdown) followed by
	// exactly `capacity` survivors. Durable count then exceeds capacity by
	// dropCount, so drop-oldest evicts exactly the victims (REL-096). The victims'
	// absolute seqs are this buffer's own monotonic seqs; the loss-marker range is
	// asserted relative to them below.
	dropCount := int(dropTo-dropFrom) + 1
	if dropCount != sumCounts(breakdown) {
		t.Fatalf("corpus self-check: drop range width %d != breakdown sum %d", dropCount, sumCounts(breakdown))
	}
	survivors := capacity
	buf := NewBuffer(capacity)

	// Victims first, split by schema per the breakdown.
	var victimSchemas []string
	for schema, n := range breakdown {
		for i := 0; i < n; i++ {
			victimSchemas = append(victimSchemas, schema)
		}
	}
	for _, s := range victimSchemas {
		buf.Record(s, json.RawMessage(`{"k":"v"}`), "", 0)
	}
	firstVictim := buf.Pending()[0].Seq
	lastVictim := buf.Pending()[len(buf.Pending())-1].Seq

	// Survivors overflow the victims out.
	for i := 0; i < survivors; i++ {
		buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:x","screen_id":"s"}`), "", 0)
	}

	// The loss marker accounts for exactly the corpus breakdown.
	markers := buf.PendingLossMarkers()
	if len(markers) != 1 {
		t.Fatalf("PendingLossMarkers = %d, want 1 loss marker", len(markers))
	}
	m := markers[0]
	if m.FromSeq != firstVictim || m.ToSeq != lastVictim {
		t.Fatalf("loss marker range [%d,%d] != dropped victim range [%d,%d]", m.FromSeq, m.ToSeq, firstVictim, lastVictim)
	}
	for schema, want := range breakdown {
		if m.DroppedCountsBySchema[schema] != want {
			t.Fatalf("dropped_counts_by_schema[%q] = %d, want %d (REL-090 corpus)", schema, m.DroppedCountsBySchema[schema], want)
		}
	}

	// Push, then ack with the corpus-shaped ack pointing at THIS buffer's seqs:
	// ack_through_seq = highest survivor seq, loss_markers_acked = the produced
	// marker's own range (the corpus asserts the ack names the delivered marker).
	highest := buf.Pending()[len(buf.Pending())-1].Seq
	up := &scriptedUpstream{responses: []upstreamResponse{{ack: Ack{
		AckThroughSeq:    highest,
		LossMarkersAcked: []SeqRange{{FromSeq: m.FromSeq, ToSeq: m.ToSeq}},
	}}}}
	ch := NewChannel(buf, up, ExponentialBackoff{Base: time.Millisecond, MaxAttempts: 1})

	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// The single push carried both survivors and the loss marker (REL-090/102).
	pushed := up.lastPush()
	if len(pushed.LossMarkers) != 1 {
		t.Fatalf("push loss_markers = %d, want 1 (REL-102)", len(pushed.LossMarkers))
	}
	if len(pushed.Entries) != survivors {
		t.Fatalf("push entries = %d, want %d survivors", len(pushed.Entries), survivors)
	}
	// After the ack: buffer drained and marker retired.
	if len(buf.Pending()) != 0 {
		t.Fatalf("Pending after ack = %d, want 0", len(buf.Pending()))
	}
	if len(buf.PendingLossMarkers()) != 0 {
		t.Fatalf("PendingLossMarkers after loss_markers_acked = %d, want 0 (REL-092)", len(buf.PendingLossMarkers()))
	}
}

// TestREL094CorpusChannelPushesSupersededNewestOnly replays REL-094 against the
// Channel: two same-device heartbeats queue while disconnected; the older is
// superseded before it is ever pushed, so the push carries only the newer entry
// and NO loss marker (a latest-only discard is not loss, REL-094/104).
func TestREL094CorpusChannelPushesSupersededNewestOnly(t *testing.T) {
	older, newer, _ := rel094Heartbeats(t)
	subj := deviceID(t, newer)

	buf := NewBuffer(500)
	buf.Record(SchemaDeviceHeartbeat, older, subj, 0) // seq1 — superseded
	buf.Record(SchemaDeviceHeartbeat, newer, subj, 0) // seq2 — survives

	up := &scriptedUpstream{responses: []upstreamResponse{{ack: Ack{AckThroughSeq: 2}}}}
	ch := NewChannel(buf, up, ExponentialBackoff{Base: time.Millisecond, MaxAttempts: 1})
	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	pushed := up.lastPush()
	if len(pushed.Entries) != 1 {
		t.Fatalf("pushed entries = %d, want 1 (only the newer heartbeat, REL-094)", len(pushed.Entries))
	}
	if pushed.Entries[0].Seq != 2 {
		t.Fatalf("pushed seq = %d, want 2 (superseded seq1 must never be pushed, REL-094)", pushed.Entries[0].Seq)
	}
	if !json.Valid(pushed.Entries[0].Payload) || string(pushed.Entries[0].Payload) != string(newer) {
		t.Fatalf("pushed payload = %s, want the newer heartbeat payload %s", pushed.Entries[0].Payload, newer)
	}
	// REL-090: loss_markers present; REL-094/104: the supersession is NOT loss.
	if pushed.LossMarkers == nil {
		t.Fatalf("loss_markers nil; REL-090 requires it present")
	}
	if len(pushed.LossMarkers) != 0 {
		t.Fatalf("loss_markers = %d, want 0 (a superseded latest-only entry is not loss, REL-094/104)", len(pushed.LossMarkers))
	}
}
