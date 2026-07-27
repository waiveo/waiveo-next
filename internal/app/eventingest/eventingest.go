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
//
// It appends through an EventSink rather than a bare *events.EventLog: in
// production the sink is the eventsse.Hub, whose Append records the event AND
// wakes every connected /events/v1 subscriber under the shared synchronization
// boundary events.EventLog delegates to "the live transport" — so a
// telemetry-derived event pushes live with no separate notify wiring, and the
// ingest write never races an SSE read (EVT-100). A bare *events.EventLog also
// satisfies EventSink, so a test can append and read it directly.
//
// # Who may push, and why this is not an intake door
//
// This route was previously unauthenticated. It is now authenticated as exactly
// what its only caller is: an ENROLLED RELAY, identified by the mutual-TLS client
// certificate this same feeder issued it at enrollment (relay/1 REL-003/041) and
// checked against the enrollment registry's revocation record on every request
// (REL-016) — the identical two checks internal/feeder/relayconn performs before
// the persistent connection's handshake proceeds.
//
// That is deliberately NOT one of api/1 API-092's two intake schemes (an
// HMAC-signed body, or a scoped single-purpose ingest token), and the reason
// matters. API-092 governs an operation "a caller invokes without holding, and
// without being able to obtain, a platform session or API key", and it defers the
// choice of scheme to "the contract or contract section introducing that intake
// feature". No contract introduces /telemetry/v1/push as an intake feature. What
// relay/1 REL-090 introduces is a MESSAGE — `telemetry.push {entries,
// loss_markers}` — that the relay uploads to its app peer; this HTTP route is a
// transport for that message, standing in for a frame on the relay/1 channel, and
// relay/1 already fixes that channel's peer authentication. The caller is not a
// credential-less webhook: it holds an enrollment-issued identity, it already
// presents it on /relay/v1, and this feeder can already verify it. Minting a
// second, weaker credential for the same peer to reach the same event log would
// add an attack surface without adding an identity.
//
// A request carrying no verified client certificate is refused 401 /
// AUTH_REQUIRED before the body is read; a certificate whose relay identity this
// feeder never enrolled, or whose serial the enrollment registry records as
// revoked, is refused 403 / FORBIDDEN. Both refusals are ordinary api/1 Problem
// documents (API-010), and neither reveals anything the caller does not already
// know about its own credential.
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

// RelayAuthorizer reports whether the relay identity presented by a verified
// client certificate may push telemetry: relayID is the certificate's subject
// common name (the relay's own identity, never a self-asserted field of the
// request body) and serial is its SerialNumber rendered exactly as the
// enrollment issuance record keys it.
//
// The production implementation answers "this feeder enrolled that relay AND has
// not revoked that serial" (relay/1 REL-016/041). It is a func rather than a
// concrete dependency so the ingest does not have to import the enrollment
// server, and so a test can authorize a certificate it minted itself — without
// any bypass that would let the handler ship one code path and be tested on
// another.
type RelayAuthorizer func(relayID, serial string) bool

// EventSink is the append target the ingest writes reconstructed envelopes to.
// It is the write half of the shared event log: *events.EventLog satisfies it
// directly, and in production the eventsse.Hub satisfies it too — the Hub's
// Append serializes the write against concurrent SSE reads and wakes every
// subscriber, which is what makes a telemetry-derived event push live (EVT-100).
type EventSink interface {
	Append(events.Envelope)
}

// ingest is the POST /telemetry/v1/push handler. It owns the write side of the
// shared event log (via an EventSink) and the at-least-once bookkeeping: which
// telemetry seqs it has terminally processed (appended-if-valid or
// dropped-if-invalid) and the highest-received ack cursor. Its own bookkeeping
// is guarded by mu, so concurrent pushes never race the cursor — a relay flushes
// serially, but nothing stops two enrolled relays (or one relay redialing) from
// having a push in flight at the same instant; the sink owns the log's own
// synchronization boundary.
type ingest struct {
	sink          EventSink
	siteScopeNode string
	idSeq         func() string
	// authorize decides whether the presented relay identity may push
	// (REL-016/041). A nil authorizer refuses every request: an ingest that
	// could be constructed without one is an ingest that will eventually be
	// constructed without one, and the fail-closed answer to that wiring bug is
	// an ingest nobody can write to (SEC-005).
	authorize RelayAuthorizer
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

// New returns the POST /telemetry/v1/push handler writing into sink. In
// production sink is the eventsse.Hub shared with the /events/v1 SSE reader, so
// each append also wakes the live subscribers. siteScopeNode is the site's
// scope-node ULID stamped onto every ingested event's scope_node (the REL-090
// wire record carries no per-record scope, so the site node is authoritative; a
// per-record subject-derived scope is a deferred concern). idSeq mints each
// ingested event's recording-order id (EVT-011). It MUST be strictly ascending
// across successive calls — including ids minted within the same millisecond,
// which a whole PushBatch reconstructed in one loop iteration routinely is — so
// the log stores and an SSE subscriber streams them in recording order, never
// inverting or silently dropping one (REL-094/097, EVT-135/143). Pass a
// monotonic generator (ulid.Monotonic()), NOT plain ulid.New, whose independent
// random tail leaves same-millisecond ids unordered.
// authorize decides which enrolled relay identity may push (see this package's
// doc); a nil authorizer refuses every request.
func New(sink EventSink, siteScopeNode string, idSeq func() string, authorize RelayAuthorizer) http.Handler {
	return &ingest{
		sink:          sink,
		siteScopeNode: siteScopeNode,
		idSeq:         idSeq,
		authorize:     authorize,
		logf:          stdlog.Printf,
		processed:     make(map[int64]bool),
	}
}

// authenticate resolves the pushing relay's identity from its mutual-TLS client
// certificate and authorizes it, writing the refusal Problem itself and reporting
// whether the request may proceed. It runs BEFORE the body is read, so an
// unauthorized caller never reaches the decoder, let alone the event log.
//
// The identity is the certificate's, never a field of the request — the rule
// relay/1 REL-003/041 already states for the persistent connection ("the relay's
// identity is the mTLS client certificate's, never the self-asserted relay_id").
// It requires a VERIFIED chain rather than merely a presented certificate: the
// listener serving this route verifies a given client certificate against the
// enrollment CA, and insisting on VerifiedChains here means a listener wired
// without that pool cannot silently turn this check into "any self-signed
// certificate will do".
func (in *ingest) authenticate(w http.ResponseWriter, r *http.Request, traceID string) bool {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 {
		apihttp.WriteProblem(w, r, traceID, http.StatusUnauthorized, "AUTH_REQUIRED",
			"A verified relay client certificate is required to push telemetry")
		return false
	}
	leaf := r.TLS.PeerCertificates[0]
	relayID := leaf.Subject.CommonName
	// The presented serial, rendered exactly as the enrollment issuance record
	// keys it (big.Int.Text(16) — see enroll.issueRelayCert).
	serial := leaf.SerialNumber.Text(16)
	if in.authorize == nil || !in.authorize(relayID, serial) {
		apihttp.WriteProblem(w, r, traceID, http.StatusForbidden, "FORBIDDEN",
			"The presented relay identity is not enrolled, or its certificate has been revoked")
		return false
	}
	return true
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
	// REL-003/041/016: the pushing relay's mTLS identity, resolved and authorized
	// before a single byte of the body is read.
	if !in.authenticate(w, r, traceID) {
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
	in.sink.Append(env)
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
