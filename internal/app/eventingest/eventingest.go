// Package eventingest is the app-side telemetry ingest (REL-090, events/1
// EVT-010/011/013): the POST /telemetry/v1/push handler the relay's telemetry
// channel delivers to. It reads a telemetry.push batch and, for each record
// {seq, schema, payload}, reconstructs a full events/1 durable-event Envelope —
// assigning the recording-order id (EVT-011), origin: relay (EVT-042), the site
// scope_node, a fresh trace_id, ts, and the schema's cost/retention class — then
// validates it (EVT-013: an invalid record is dropped and logged, NEVER
// appended) and appends it to the shared events.EventLog the /events/v1 SSE
// server reads. It acks the highest CONTIGUOUS seq it has durably processed
// (REL-092), and is idempotent on the telemetry seq (REL-097 / EVT-135: a
// redelivered record is not re-appended — the dedup is on the seq, before a
// fresh id is minted, since the app assigns each record its own id).
//
// It reuses the built wire types (telemetry.PushBatch/Entry/Ack), the built
// events.EventLog/Validate/ClassFor, and apihttp's Problem/Trace-Id — it
// re-implements none of them.
package eventingest

import (
	"encoding/json"
	stdlog "log"
	"net/http"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// ingest is the POST /telemetry/v1/push handler. It owns the write side of the
// shared events.EventLog and the at-least-once bookkeeping: which telemetry seqs
// it has terminally processed (appended-if-valid or dropped-if-invalid) and the
// contiguous ack high-water. Its state is guarded by mu, so concurrent pushes
// (the relay flushes serially, but the seam is unauthenticated and shared) never
// race the log or the cursor.
type ingest struct {
	log           *events.EventLog
	siteScopeNode string
	idSeq         func() string
	// logf records an EVT-013 drop; it defaults to the stdlib logger and is a
	// field so a test can assert a dropped record is logged, not silently lost.
	logf func(format string, args ...any)

	mu sync.Mutex
	// processed holds telemetry seqs terminally handled but not yet subsumed by
	// ackThrough — the gap set above the contiguous high-water. A seq at or below
	// ackThrough is known-processed and pruned from here, so this stays bounded to
	// the (normally empty) set of out-of-order-ahead seqs.
	processed map[int64]bool
	// ackThrough is the highest seq S such that every seq through S has been
	// terminally processed with no hole — the REL-092 ack cursor. The relay
	// advances retention (discards seq <= S) only on this value.
	ackThrough int64
}

// New returns the POST /telemetry/v1/push handler writing into log. siteScopeNode
// is the site's scope-node ULID stamped onto every ingested event's scope_node
// (the REL-090 wire record carries no per-record scope, so the site node is
// authoritative; a per-record subject-derived scope is a deferred concern).
// idSeq mints each ingested event's recording-order id (EVT-011) — a ULID
// generator whose values are lexicographically time-ordered. The log is shared,
// unmodified, with the /events/v1 SSE reader.
func New(log *events.EventLog, siteScopeNode string, idSeq func() string) http.Handler {
	return &ingest{
		log:           log,
		siteScopeNode: siteScopeNode,
		idSeq:         idSeq,
		logf:          stdlog.Printf,
		processed:     make(map[int64]bool),
	}
}

// ServeHTTP reads a telemetry.push batch and responds with the telemetry.ack
// (REL-090/092). A non-POST or an undecodable body is refused with an api/1
// Problem (API-010) before any ingest; a decodable batch always responds 200
// with the current ack cursor, even when every record in it was a redelivery.
func (in *ingest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)
	if r.Method != http.MethodPost {
		apihttp.WriteProblem(w, r, traceID, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method Not Allowed")
		return
	}
	var batch telemetry.PushBatch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Malformed telemetry.push body")
		return
	}

	ack := in.ingestBatch(batch)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ack)
}

// ingestBatch appends each not-yet-processed record's reconstructed envelope to
// the log (dropping+logging an invalid one, EVT-013), advances the contiguous
// ack high-water (REL-092), and acknowledges the batch's loss markers so the
// relay retires them (REL-092/102). It is idempotent on seq (REL-097): a seq at
// or below the high-water, or already in the gap set, is skipped before an id is
// minted, so a redelivered record is never double-appended.
func (in *ingest) ingestBatch(batch telemetry.PushBatch) telemetry.Ack {
	in.mu.Lock()
	defer in.mu.Unlock()

	for _, e := range batch.Entries {
		if e.Seq <= in.ackThrough || in.processed[e.Seq] {
			continue // already terminally processed — idempotent on seq (REL-097)
		}
		in.processOne(e)
		in.processed[e.Seq] = true
	}

	// Advance the contiguous high-water over every terminally-processed seq — a
	// dropped-invalid seq advances it too, so one un-fixable record never wedges
	// the channel; a genuine hole (a seq never received) stops it (REL-092).
	for in.processed[in.ackThrough+1] {
		in.ackThrough++
	}
	// Prune seqs the high-water now subsumes: they are known-processed via the
	// e.Seq <= ackThrough test, so the gap set stays bounded.
	for s := range in.processed {
		if s <= in.ackThrough {
			delete(in.processed, s)
		}
	}

	// Acknowledge the loss markers this batch delivered so the relay stops
	// re-sending them (REL-092/102); this increment produces none, but the ack
	// is honest about what it received.
	acked := make([]telemetry.SeqRange, 0, len(batch.LossMarkers))
	for _, m := range batch.LossMarkers {
		acked = append(acked, telemetry.SeqRange{FromSeq: m.FromSeq, ToSeq: m.ToSeq})
	}
	return telemetry.Ack{AckThroughSeq: in.ackThrough, LossMarkersAcked: acked}
}

// processOne reconstructs one wire record into an events/1 envelope and appends
// it, or drops+logs it if it fails validation (EVT-013). The caller holds mu.
func (in *ingest) processOne(e telemetry.Entry) {
	env, err := in.buildEnvelope(e)
	if err != nil {
		in.logf("eventingest: dropping telemetry seq %d schema %q: %v (EVT-013)", e.Seq, e.Schema, err)
		return
	}
	in.log.Append(env)
}

// buildEnvelope assigns the app-side envelope fields onto a wire record and
// validates the result (EVT-010/011/013). It carries the payload through
// byte-for-byte (REL-090 — the relay already mapped it, this layer never
// re-maps a schema's payload) and returns an error for any record it cannot turn
// into a deliverable envelope (an unclassed schema, or a payload that fails
// events.Validate).
func (in *ingest) buildEnvelope(e telemetry.Entry) (events.Envelope, error) {
	cost, retention, ok := events.ClassFor(e.Schema)
	if !ok {
		return events.Envelope{}, events.ValidationError{Field: "schema", Detail: "no registered cost/retention class for " + e.Schema}
	}
	env := events.Envelope{
		ID:             in.idSeq(),
		Schema:         e.Schema,
		TS:             time.Now().UnixMilli(),
		ScopeNode:      in.siteScopeNode,
		TraceID:        ulid.New(),
		CostClass:      cost,
		RetentionClass: retention,
		Origin:         "relay",
		Payload:        e.Payload,
	}
	if err := events.Validate(env); err != nil {
		return events.Envelope{}, err
	}
	return env, nil
}
