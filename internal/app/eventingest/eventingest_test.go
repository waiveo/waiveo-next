package eventingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// siteScope is the frozen corpus site scope-node ULID the ingest stamps onto an
// ingested event's scope_node (the REL-090 wire record carries no per-record
// scope — telemetry.Entry.Subject is buffer-only `json:"-"` — so the site node
// is authoritative, EVT-010).
const siteScope = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"

// validAutomationRunPayload is the frozen corpus automation.run payload
// (conformance/corpora/events-1), so a reconstructed envelope carrying it passes
// events.Validate end to end (EVT-040/041).
func validAutomationRunPayload() json.RawMessage {
	return json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2YC","rule_revision":4,"trigger_snapshot":{"kind":"state"},"condition_results":[{"passed":true}],"action_outcomes":[{"status":"ok"}],"mode_disposition":"ran","misfire_caught":false}`)
}

// seqIDs returns an idSeq minting deterministic, ascending, valid ULIDs (fixture
// ids, EVT-011 recording order): a fixed 24-char corpus prefix plus a 2-symbol
// Crockford-base32 counter suffix, so successive ids sort in call order.
func seqIDs() func() string {
	const prefix = "01J8Z3K4N5P6Q7R8S9T0V1W2" // 24 Crockford chars, leading 0
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	n := 0
	return func() string {
		hi := alphabet[(n/32)%32]
		lo := alphabet[n%32]
		n++
		return prefix + string([]byte{hi, lo})
	}
}

// postBatch encodes batch as the REL-090 telemetry.push body, drives it through
// h, and returns the parsed telemetry.ack. It fails the test on a non-200 or a
// non-JSON ack.
func postBatch(t *testing.T, h http.Handler, batch telemetry.PushBatch) telemetry.Ack {
	t.Helper()
	body, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshaling push batch: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/telemetry/v1/push", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("telemetry push must respond 200; got %d body=%s", rec.Code, rec.Body.String())
	}
	var ack telemetry.Ack
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatalf("ack must be JSON {ack_through_seq}; got %v body=%s", err, rec.Body.String())
	}
	return ack
}

func autoEntry(seq int64, payload json.RawMessage) telemetry.Entry {
	return telemetry.Entry{Seq: seq, Schema: events.SchemaAutomationRun, Payload: payload}
}

func pushBatch(entries ...telemetry.Entry) telemetry.PushBatch {
	return telemetry.PushBatch{Entries: entries, LossMarkers: []telemetry.LossMarker{}}
}

// TestIngest_AppendsValidAutomationRunEnvelopeAndAcks: a pushed batch with one
// automation.run record appends one valid events/1 envelope to the shared log —
// origin: relay (EVT-042), a recording-order ULID id (EVT-011), the site scope
// node, the schema's cost/retention class, the payload intact — and acks its seq
// (REL-090/092, EVT-010/013).
func TestIngest_AppendsValidAutomationRunEnvelopeAndAcks(t *testing.T) {
	log := events.NewEventLog(0)
	h := New(log, siteScope, seqIDs())

	payload := validAutomationRunPayload()
	ack := postBatch(t, h, pushBatch(autoEntry(1, payload)))

	if ack.AckThroughSeq != 1 {
		t.Fatalf("ack_through_seq must be the appended seq; want 1 got %d", ack.AckThroughSeq)
	}
	got := log.After("")
	if len(got) != 1 {
		t.Fatalf("exactly one envelope must be appended; got %d", len(got))
	}
	env := got[0]
	if err := events.Validate(env); err != nil {
		t.Fatalf("the reconstructed envelope must validate (EVT-010/013); got %v", err)
	}
	if env.Origin != "relay" {
		t.Fatalf("origin must be relay (EVT-042); got %q", env.Origin)
	}
	if env.Schema != events.SchemaAutomationRun {
		t.Fatalf("schema must be carried through; got %q", env.Schema)
	}
	if !ulid.Valid(env.ID) {
		t.Fatalf("id must be a recording-order ULID (EVT-011); got %q", env.ID)
	}
	if !ulid.Valid(env.TraceID) {
		t.Fatalf("trace_id must be a valid ULID (EVT-010); got %q", env.TraceID)
	}
	if env.ScopeNode != siteScope {
		t.Fatalf("scope_node must be the site node; want %q got %q", siteScope, env.ScopeNode)
	}
	if env.CostClass != "telemetry" || env.RetentionClass != "telemetry-standard" {
		t.Fatalf("class must come from the schema class; got cost=%q retention=%q", env.CostClass, env.RetentionClass)
	}
	if env.TS <= 0 {
		t.Fatalf("ts must be assigned on ingest (EVT-010); got %d", env.TS)
	}
	// the payload is carried through byte-semantically unmodified (REL-090).
	var gotP, wantP any
	if err := json.Unmarshal(env.Payload, &gotP); err != nil {
		t.Fatalf("appended payload must be JSON; got %v", err)
	}
	if err := json.Unmarshal(payload, &wantP); err != nil {
		t.Fatalf("source payload must be JSON; got %v", err)
	}
	if !reflect.DeepEqual(gotP, wantP) {
		t.Fatalf("payload must be carried through intact; want %s got %s", payload, env.Payload)
	}
}

// TestIngest_DropsInvalidPayloadButAcksBatch: a record whose payload fails
// events.Validate is dropped and logged (EVT-013 — never appended), the batch
// still succeeds, and the ack advances past the terminally-dropped seq so the
// relay does not retry an un-fixable record forever (REL-097 progress).
func TestIngest_DropsInvalidPayloadButAcksBatch(t *testing.T) {
	log := events.NewEventLog(0)
	h := New(log, siteScope, seqIDs())

	var logged []string
	in := h.(*ingest)
	in.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	// seq 2's payload has a mode_disposition outside the EVT-041 closed enum.
	invalid := json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2YC","rule_revision":4,"trigger_snapshot":{},"condition_results":[],"action_outcomes":[],"mode_disposition":"exploded","misfire_caught":false}`)
	ack := postBatch(t, h, pushBatch(
		autoEntry(1, validAutomationRunPayload()),
		autoEntry(2, invalid),
		autoEntry(3, validAutomationRunPayload()),
	))

	got := log.After("")
	if len(got) != 2 {
		t.Fatalf("an invalid record must not be appended (EVT-013); want 2 envelopes got %d", len(got))
	}
	if len(logged) == 0 {
		t.Fatalf("a dropped record must be logged, not silently discarded (EVT-013)")
	}
	if ack.AckThroughSeq != 3 {
		t.Fatalf("the ack must advance past a terminally-dropped record; want 3 got %d", ack.AckThroughSeq)
	}
}

// TestIngest_IdempotentOnSeq: a redelivered telemetry seq is not double-appended
// (REL-097 at-least-once dedup on seq / EVT-135) — the app assigns a fresh id per
// record, so the dedup MUST be on the telemetry seq, before the id is minted.
func TestIngest_IdempotentOnSeq(t *testing.T) {
	log := events.NewEventLog(0)
	h := New(log, siteScope, seqIDs())

	batch := pushBatch(autoEntry(1, validAutomationRunPayload()))
	ack1 := postBatch(t, h, batch)
	ack2 := postBatch(t, h, batch) // redelivery of the same seq

	if got := log.After(""); len(got) != 1 {
		t.Fatalf("a redelivered seq must not be double-appended (REL-097/EVT-135); want 1 got %d", len(got))
	}
	if ack1.AckThroughSeq != 1 || ack2.AckThroughSeq != 1 {
		t.Fatalf("both acks must report seq 1; got %d then %d", ack1.AckThroughSeq, ack2.AckThroughSeq)
	}
}

// TestIngest_AckIsHighestSeqReceived_JumpsMarkedAndSupersededGaps: ack_through_seq
// is the highest ordinary-entry seq RECEIVED (REL-092), NOT a no-gap-contiguous
// high-water. A gap left below it — by a loss-marked buffer overflow (REL-096) or
// by a latest-only supersession that produces no marker at all (REL-094) — is
// jumped, never wedged on. This mirrors the REL-092 worked example and the
// REL-090 overflow corpus (REL-090-valid-telemetry-overflow-loss-marker.json),
// whose expected ack_through_seq is 1002 across BOTH a marked 980-999 drop and a
// bare, unaccounted gap at seq 1000.
func TestIngest_AckIsHighestSeqReceived_JumpsMarkedAndSupersededGaps(t *testing.T) {
	log := events.NewEventLog(0)
	h := New(log, siteScope, seqIDs())

	batch := telemetry.PushBatch{
		Entries: []telemetry.Entry{
			autoEntry(1001, validAutomationRunPayload()),
			autoEntry(1002, validAutomationRunPayload()),
		},
		// 980-999 is a loss-marked durable overflow; 1000 is a bare gap — a
		// latest-only supersession (REL-094), which by design produces no marker.
		LossMarkers: []telemetry.LossMarker{{
			FromSeq:               980,
			ToSeq:                 999,
			DroppedCountsBySchema: map[string]int{"content.played": 12},
			Reason:                telemetry.ReasonBufferExceeded,
		}},
	}
	ack := postBatch(t, h, batch)

	if ack.AckThroughSeq != 1002 {
		t.Fatalf("ack must jump the marked+bare gaps to the highest seq received (REL-092); want 1002 got %d", ack.AckThroughSeq)
	}
	if got := log.After(""); len(got) != 2 {
		t.Fatalf("both above-the-gap entries must be appended (no silent loss); want 2 got %d", len(got))
	}
	wantAcked := []telemetry.SeqRange{{FromSeq: 980, ToSeq: 999}}
	if !reflect.DeepEqual(ack.LossMarkersAcked, wantAcked) {
		t.Fatalf("the delivered loss marker must be acknowledged (REL-092/102); want %+v got %+v", wantAcked, ack.LossMarkersAcked)
	}
	// The gap set must DRAIN once the cursor jumps the gap: a wedged contiguous
	// cursor would retain every above-gap seq in `processed` forever — an
	// unbounded map leak for the life of the connection.
	if in := h.(*ingest); len(in.processed) != 0 {
		t.Fatalf("the gap set must drain once the cursor jumps the gap; got %d retained", len(in.processed))
	}
}

// TestIngest_AckJumpsLossMarkerFromOverflow: the exact REL-096 drop-oldest shape —
// after acking seq 1, a push of entry seq 5 alongside a buffer_exceeded loss
// marker for the dropped 2-4 range acks 5, not 1. A contiguous high-water would
// wedge at 1 forever behind the first overflow — the relay's ack-gated retention
// (REL-097) would then never prune the already-delivered seq-5 entry from its
// bounded buffer, re-pushing it and eventually re-dropping genuinely-durable
// telemetry as false loss.
func TestIngest_AckJumpsLossMarkerFromOverflow(t *testing.T) {
	log := events.NewEventLog(0)
	h := New(log, siteScope, seqIDs())

	if ack := postBatch(t, h, pushBatch(autoEntry(1, validAutomationRunPayload()))); ack.AckThroughSeq != 1 {
		t.Fatalf("a lone seq 1 must ack 1; got %d", ack.AckThroughSeq)
	}

	batch := telemetry.PushBatch{
		Entries: []telemetry.Entry{autoEntry(5, validAutomationRunPayload())},
		LossMarkers: []telemetry.LossMarker{{
			FromSeq:               2,
			ToSeq:                 4,
			DroppedCountsBySchema: map[string]int{events.SchemaAutomationRun: 3},
			Reason:                telemetry.ReasonBufferExceeded,
		}},
	}
	ack := postBatch(t, h, batch)
	if ack.AckThroughSeq != 5 {
		t.Fatalf("the ack must jump the loss-marked 2-4 gap to the highest seq received (REL-092); want 5 got %d", ack.AckThroughSeq)
	}
	if got := log.After(""); len(got) != 2 {
		t.Fatalf("the seq-5 entry must be durably appended above the gap (no silent loss); want 2 got %d", len(got))
	}
	// A seq inside the jumped, loss-marked range is terminally accounted for: the
	// relay (seq-ordered push, REL-090/097) will never deliver it, and a stray
	// redelivery must dedup, not re-append.
	if ack := postBatch(t, h, pushBatch(autoEntry(3, validAutomationRunPayload()))); ack.AckThroughSeq != 5 {
		t.Fatalf("a seq below the cursor must stay acked at 5, not regress; got %d", ack.AckThroughSeq)
	}
	if got := log.After(""); len(got) != 2 {
		t.Fatalf("a seq at or below the cursor must not be appended (idempotent, REL-097); want 2 got %d", len(got))
	}
}
