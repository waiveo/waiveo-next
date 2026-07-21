// Package eventingest is the app-side telemetry ingest (REL-090, events/1
// EVT-010/011/013): the POST /telemetry/v1/push handler the relay's telemetry
// channel delivers to. It reads a telemetry.push batch and, for each record
// {seq, schema, payload}, reconstructs a full events/1 durable-event Envelope —
// assigning the recording-order id (EVT-011), origin: relay (EVT-042), the site
// scope_node, a fresh trace_id, ts, and the schema's cost/retention class — then
// validates it (EVT-013: an invalid record is dropped and logged, NEVER
// appended) and appends it to the shared events.EventLog the /events/v1 SSE
// server reads. It acks the highest ordinary-entry seq it has RECEIVED
// (REL-092) — jumping any gap a loss-marked overflow (REL-096) or a latest-only
// supersession (REL-094) leaves below it, never wedging on it — and is
// idempotent on the telemetry seq (REL-097 / EVT-135: a
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
// highest-received ack cursor. Its state is guarded by mu, so concurrent pushes
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
	// processed holds telemetry seqs terminally handled within the current batch,
	// used for intra-batch dedup and to compute the cursor advance. Because a gap
	// below the cursor is jumped rather than held open (REL-092), every processed
	// seq is subsumed by ackThrough at the end of each batch and pruned, so this
	// map drains to empty and never grows across batches.
	processed map[int64]bool
	// ackThrough is the highest ordinary-entry seq RECEIVED — the REL-092 ack
	// cursor, NOT a no-gap-contiguous high-water. A gap left below it by a
	// loss-marked overflow (REL-096) or a latest-only supersession (REL-094) is
	// jumped, not held. The relay advances retention (discards seq <= S) only on
	// this value.
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
// the log (dropping+logging an invalid one, EVT-013), advances the ack cursor to
// the highest ordinary-entry seq received (REL-092, jumping any gap below it),
// and acknowledges the batch's loss markers so the relay retires them
// (REL-092/102). It is idempotent on seq (REL-097): a seq at or below the cursor,
// or already in the gap set, is skipped before an id is minted, so a redelivered
// record is never double-appended.
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

	// Advance the cursor to the highest ordinary-entry seq RECEIVED this batch
	// (REL-092) — NOT a no-gap-contiguous high-water. A gap left below it is
	// jumped, never wedged on: the relay pushes strictly in seq order (REL-090)
	// and only ever leaves a hole for a loss-marked overflow (REL-096, delivered
	// as a marker this batch acknowledges) or a latest-only supersession (REL-094,
	// which by design produces no marker) — in both cases the missing seq is
	// terminally accounted for and will never be delivered, so acking past it is
	// safe. A dropped-invalid seq (EVT-013) counts as received and advances it
	// too, so one un-fixable record never wedges the channel.
	for s := range in.processed {
		if s > in.ackThrough {
			in.ackThrough = s
		}
	}
	// Drain the gap set: every terminally-processed seq is now at or below the
	// cursor (gaps are jumped, not held open), so it carries nothing forward and
	// stays bounded — its only remaining job is intra-batch dedup. A seq at or
	// below the cursor is known-processed via the e.Seq <= ackThrough test.
	for s := range in.processed {
		if s <= in.ackThrough {
			delete(in.processed, s)
		}
	}

	// Acknowledge the loss markers this batch delivered so the relay stops
	// re-sending them (REL-092/102). A marker does not itself drive ackThrough —
	// that tracks ordinary entries only (REL-092); the higher entries above a
	// marked gap are what carry the cursor past it — but loss_markers_acked keeps
	// the ack honest about which markers were received.
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
