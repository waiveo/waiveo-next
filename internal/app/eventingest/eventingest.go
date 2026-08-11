// Package eventingest is the app-side telemetry ingest (REL-090, events/1
// EVT-010/011/013): the POST /telemetry/v1/push handler the relay's telemetry
// channel delivers to. It reads a telemetry.push batch and, for each record
// {seq, schema, payload, trace_id}, reconstructs a full events/1 durable-event
// Envelope — assigning the recording-order id (EVT-011), origin: relay
// (EVT-042), the site scope_node, ts, and the schema's cost/retention class,
// and PROPAGATING the record's own trace_id (EVT-010, relay/1 REL-006, api/1
// API-063 — see resolveTraceID) — then
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
// What ACTS on an ingested event is a second, separate seam — an EventDeliverer.
// Recording and acting are split precisely because acting is unbounded work (the
// app peer fires rules/1 `event` triggers from it, which reaches devices over the
// network), and the one durable telemetry channel every relay shares must not be
// held while it runs.
//
// # The durability contract this ingest provides
//
// Stated plainly, because the previous statement of it was not true and the
// difference cost the guarantee it claimed:
//
//	ack_through_seq S means: every ordinary entry at or below S has been
//	terminally handled — appended to the durable event log and then handed to
//	the app-side deliverer, whose run CONCLUDED — or terminally dropped as
//	invalid (EVT-013). Nothing at or below S is still in flight.
//
// The relay may therefore discard its retained copies of everything at or below
// S (REL-092), which is exactly what it does. Nothing above S is promised, and
// an entry the relay pushed that is NOT covered by S stays retained and comes
// back on the next flush — which is how at-least-once is preserved across a
// crash, a restart, or a request that never got its response.
//
// Two things follow, and both are load-bearing:
//
//   - DELIVERY IS ASYNCHRONOUS AND THE ACK CURSOR TRAILS IT. The push request
//     appends, enqueues, and returns; a single background runner delivers in
//     append order and the cursor advances only past entries whose delivery has
//     concluded. The alternative — deliver synchronously, then answer — is what
//     this file did, and it did NOT hold the guarantee: the cursor was advanced
//     under the lock BEFORE delivery ran, so the relay's own 2-second retry
//     (which is how it behaves when its 5-second client deadline elapses) was
//     answered 200 with a cursor already covering an entry still being
//     delivered, and the relay pruned it. A crash there lost the press with the
//     ack given. Making the request wait cannot fix that, because the request is
//     not the only reader of the cursor.
//
//   - THE REQUEST IS BOUNDED. A batch is bounded by the delivery queue's free
//     depth (deliveryQueueDepth), and entries beyond it are simply not taken:
//     they are not acked, so the relay retains them and re-pushes. The relay's
//     buffer IS the queue (1024 durable entries, REL-096 drop-oldest), so
//     backpressure costs nothing but a later flush, where an unbounded delivery
//     loop inside the request cost a 5-second client deadline against work whose
//     per-device timeout alone is 15 seconds.
//
// A wedged deliverer cannot hold the cursor forever: each delivery runs under
// deliveryBudget. That bound is a WEDGE bound, not a work budget — the deliverer's
// own per-command timeout is what paces real work — and it is the reason
// "concluded" above can be promised at all.
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
	"context"
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
// It is the write half of the shared event log: any events.Log satisfies it
// directly (the in-memory events.EventLog, the persistent store one), and in
// production the eventsse.Hub satisfies it too — the Hub's Append serializes the
// write against concurrent SSE reads and wakes every subscriber, which is what
// makes a telemetry-derived event push live (EVT-100).
type EventSink interface {
	Append(events.Envelope)
}

// EventDeliverer is the POST-APPEND handoff: every envelope this ingest durably
// appended is offered to it, in the order it was appended, with the ingest's own
// lock released and BEFORE the ack cursor advances past that envelope's seq.
//
// It exists as a seam separate from EventSink because the two have opposite
// requirements, and collapsing them into one Append cost this path its liveness.
// A sink's job is to RECORD, which is bounded work under this ingest's own
// mutex; a deliverer's job is to ACT on the event, which for the app peer means
// firing every `event` trigger that matches it (rules/1 RUL-080/081) — listing
// and parsing the deployment's rules, then dispatching device commands over a
// relay connection with a 15-second timeout, per matching rule, per entry. Done
// from inside Append that ran with in.mu held, so one unreachable device
// head-of-line blocked the platform's single durable telemetry ingest for EVERY
// relay: `device.heartbeat`, `entity.state_changed` and `content.played`
// delivery stalled deployment-wide behind one screen's button press.
//
// The three properties of where it now runs:
//
//   - OFF THE LOCK. Another relay's push proceeds while a rule run is in
//     flight. The lock's only job is this ingest's own seq bookkeeping, which
//     is what it now covers and all it covers.
//   - OFF THE REQUEST. The push returns as soon as the batch is appended and
//     queued. Holding the request open for the run was the previous answer and
//     it does not survive contact with the relay's own client: 5 seconds to give
//     up, 2 seconds to retry, against work bounded at 15 seconds per device.
//   - AHEAD OF THE ACK CURSOR. The cursor this ingest reports never covers an
//     entry whose delivery has not concluded — see the package doc's contract.
//     That is the property "deliver, then answer" was supposed to provide and
//     did not, because the cursor advanced under the lock before delivery ran
//     and any CONCURRENT push (a retry, most of all) read the advanced value.
//
// What the ordering does and does not promise: delivery follows APPEND order,
// globally, because one runner performs it. Append order across concurrent
// pushes is itself arbitrary and never could be otherwise — a relay pushes
// strictly in seq order (REL-090) so its own events stay ordered relative to
// each other, but two relays' pushes were already interleaved by the HTTP layer
// before they reached here. A deliverer that needs a total order over the
// deployment must read the event log, which is what records it.
//
// A deliverer is given a context carrying its push's VALUES, with that request's
// cancellation removed and deliveryBudget applied. It should honour it: the
// budget is what lets a wedged run be declared concluded so the cursor can move.
type EventDeliverer func(context.Context, events.Envelope)

// deliveryQueueDepth bounds how many appended-but-not-yet-delivered envelopes
// this ingest will hold, and therefore how much of one push it will take.
//
// It is the ONLY bound on the request's work, and it is deliberately a queue
// depth rather than a time budget: a batch arriving after a long disconnection
// carries up to the relay's whole retained buffer (1024 durable entries,
// automationhost.telemetryCapacity), and the app has no way to make delivering
// them fast. What it can do is take a bounded prefix and leave the rest where it
// already safely is. Entries not taken are not acked, so the relay retains them
// (REL-097) and re-pushes on its next flush, 2 seconds later. Backpressure onto
// a buffer built to hold exactly this is the cheapest correct answer; the
// alternatives are an unbounded delivery loop inside a request the relay's
// client abandons after 5 seconds, or a queue that drops a durable-class event
// when it fills, which REL-103 forbids in as many words.
const deliveryQueueDepth = 256

// deliveryBudget bounds ONE envelope's delivery so a deliverer that never
// returns cannot pin the ack cursor for good.
//
// It is a wedge bound, not a work budget. The real pacing is the deliverer's
// own: an event-fired rule run dispatches device commands with a 15-second
// timeout each (internal/app/api's deviceCommandTimeout), applied per target.
// This is set well above that so a legitimately slow multi-target run finishes
// on its own terms, and finite so a run that has stopped making progress is
// eventually declared concluded — which is what lets the contract in this
// package's doc promise anything at all about seqs at or below the cursor.
const deliveryBudget = 60 * time.Second

// queuedDelivery is one appended envelope awaiting its handoff, with the context
// its push arrived under — values, no cancellation. See ServeHTTP for why the
// request's own cancellation is dropped, and drainQueue for the budget applied
// on top.
type queuedDelivery struct {
	ctx context.Context
	seq int64
	env events.Envelope
}

// Ingest is the POST /telemetry/v1/push handler. It owns the write side of the
// shared event log (via an EventSink) and the at-least-once bookkeeping: which
// telemetry seqs it has terminally processed (appended-if-valid or
// dropped-if-invalid), which are still in delivery, and the two cursors that
// distinguishes. Its own bookkeeping is guarded by mu, so concurrent pushes never
// race the cursors — a relay flushes serially, but nothing stops two enrolled
// relays (or one relay redialing, or one relay retrying a push whose response it
// never saw) from having a push in flight at the same instant; the sink owns the
// log's own synchronization boundary.
//
// It is exported so a deployment can Drain it at shutdown. It satisfies
// http.Handler and is used as one everywhere else.
type Ingest struct {
	sink          EventSink
	siteScopeNode string
	idSeq         func() string
	// authorize decides whether the presented relay identity may push
	// (REL-016/041). A nil authorizer refuses every request: an ingest that
	// could be constructed without one is an ingest that will eventually be
	// constructed without one, and the fail-closed answer to that wiring bug is
	// an ingest nobody can write to (SEC-005).
	authorize RelayAuthorizer
	// nowMs stamps each reconstructed envelope's ts (New's required argument),
	// and nothing in this package reaches for time.Now beside it. It cannot be
	// nil: New refuses to build an ingest without one, so an event stamped from
	// a clock nobody chose is not a state this type can be in.
	nowMs func() int64
	// logf records an EVT-013 drop; it defaults to the stdlib logger and is a
	// field so a test can assert a dropped record is logged, not silently lost.
	logf func(format string, args ...any)
	// deliver is the post-append handoff (EventDeliverer). A nil deliverer is a
	// deployment with no app-side consumer of ingested events — legitimate, and
	// the state every ingest that predates this seam is in.
	deliver EventDeliverer

	mu sync.Mutex
	// processed holds telemetry seqs terminally handled within the current batch,
	// used for intra-batch dedup and to compute the cursor advance. Because a gap
	// below the cursor is jumped rather than held open (REL-092), every processed
	// seq is subsumed by receivedThrough at the end of each batch and pruned, so
	// this map drains to empty and never grows across batches.
	processed map[int64]bool
	// receivedThrough is the highest ordinary-entry seq RECEIVED — NOT a
	// no-gap-contiguous high-water. A gap left below it by a loss-marked overflow
	// (REL-096) or a latest-only supersession (REL-094) is jumped, not held.
	//
	// It is the IDEMPOTENCE cursor and nothing else: a seq at or below it has
	// already been appended-or-dropped and is skipped before an id is minted
	// (REL-097, EVT-135). It is deliberately NOT the value this ingest acks —
	// separating the two is the whole of the fix. When they were one field, an
	// entry became un-redeliverable (at or below the cursor) and prunable by the
	// relay (at or below the ack) at the same instant, which is exactly one
	// instant too early: it happened before the entry had been delivered.
	receivedThrough int64
	// ackThrough is the REL-092 ack cursor — the value this ingest reports and
	// the relay advances retention on (discards seq <= S). It never covers an
	// entry whose delivery has not concluded: recomputeAckLocked holds it one
	// below the lowest pending seq. Monotonic by construction.
	ackThrough int64
	// pending is the set of appended seqs whose delivery has not concluded. It is
	// what holds ackThrough back, and it is bounded by deliveryQueueDepth — which
	// is also what bounds how much of a batch appendBatch will take.
	pending map[int64]bool
	// queue is the FIFO the single delivery runner drains, in append order.
	queue []queuedDelivery
	// draining reports whether a runner is alive; drained is closed by that runner
	// when the queue empties, and is nil while idle. One runner at a time is what
	// keeps delivery ordered and keeps the goroutine count at zero when idle.
	draining bool
	drained  chan struct{}
}

// Ingest is an http.Handler.
var _ http.Handler = (*Ingest)(nil)

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
//
// nowMs is the clock an ingested envelope's ts is stamped from, and it is
// REQUIRED for the reason auth.Open's and store.Open's are: this layer used to
// read time.Now directly, so a relay's telemetry entered the app's own durable
// log stamped from the bare host clock while the audit records describing the
// same request came from the app's persisted monotonic clock floor (SEC-066).
// Those agree only while the floor is 0; the day a time source advances it, an
// event would carry a ts BEHIND the auth row it describes, and a reader
// reconstructing what happened would see the effect before the cause.
//
// A nil clock panics HERE rather than at the first push. This is built once, at
// boot, from a single call site; a nil that survived construction would surface
// as a recovered nil-deref inside net/http — one 500 per push, with the batch
// lost and nothing naming the wiring bug that caused it.
//
// deliver is the post-append handoff (EventDeliverer) — the app peer passes its
// `event`-trigger dispatcher here, and a deployment with no app-side consumer
// passes nil. It is a REQUIRED positional argument rather than an option so
// that every construction states the answer: an ingest that silently defaulted
// to "nothing consumes these events" is the shape the whole `event` trigger
// path was already stuck in before it was wired at all.
func New(sink EventSink, siteScopeNode string, idSeq func() string, nowMs func() int64, authorize RelayAuthorizer, deliver EventDeliverer) *Ingest {
	if nowMs == nil {
		panic("eventingest: New requires a clock (a deployment passes the app's clock-floor reading, never time.Now)")
	}
	return &Ingest{
		sink:          sink,
		siteScopeNode: siteScopeNode,
		idSeq:         idSeq,
		nowMs:         nowMs,
		authorize:     authorize,
		logf:          stdlog.Printf,
		deliver:       deliver,
		processed:     make(map[int64]bool),
		pending:       make(map[int64]bool),
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
func (in *Ingest) authenticate(w http.ResponseWriter, r *http.Request, traceID string) bool {
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
func (in *Ingest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)
	if r.Method != http.MethodPost {
		// METHOD_NOT_ALLOWED is now PUBLISHED by api/1's registry, which is what
		// API-011 requires and what it previously lacked.
		//
		// Answering with VALIDATION_FAILED instead was the wrong repair: API-013a
		// binds that code to a body or query-parameter failure carrying 422 or
		// 400, and a wrong method is neither — so a 405 wearing it repurposes a
		// published code, which API-012 forbids in as many words. Trading an
		// unpublished code for a misused one is not a fix.
		//
		// The Allow header is not decoration: RFC 9110 §15.5.6 makes it a MUST on
		// every 405, and it is the only part of the response that tells a client
		// what to do instead.
		w.Header().Set("Allow", http.MethodPost)
		apihttp.WriteProblem(w, r, traceID, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
			"Method Not Allowed")
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

	// The delivery context is the request's VALUES without its cancellation
	// (context.WithoutCancel): a rule run started by this batch belongs to the
	// deployment, not to the TCP connection that happened to carry the push, and
	// cancelling a device dispatch because a relay hung up mid-push would leave
	// half a rule run behind with the other half never attempted. The bound the
	// run DOES get is deliveryBudget, applied where the run starts (drainQueue) —
	// which is a bound this ingest chose, rather than one an unrelated TCP
	// connection's lifetime imposed.
	ack := in.ingestBatch(context.WithoutCancel(r.Context()), batch)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ack)
}

// ingestBatch appends each not-yet-processed record's reconstructed envelope to
// the log (dropping+logging an invalid one, EVT-013), queues the appended ones
// for delivery, and returns the ack this request answers with — the cursor as it
// stands, which by construction does not cover anything still awaiting delivery.
// It also acknowledges the batch's loss markers so the relay retires them
// (REL-092/102). It is idempotent on seq (REL-097): a seq at or below the
// received cursor, or already in the current batch's set, is skipped before an
// id is minted, so a redelivered record is never double-appended.
//
// It does not WAIT for delivery, and that is the deliberate half of the fix. The
// ack it returns is honest without waiting, because the cursor is gated on
// delivery rather than on this request finishing — see the package doc's
// contract, and appendBatch for the gate itself.
func (in *Ingest) ingestBatch(ctx context.Context, batch telemetry.PushBatch) telemetry.Ack {
	return in.appendBatch(ctx, batch)
}

// appendBatch is the locked phase: it appends this batch's envelopes, records
// them as pending delivery, advances the received cursor, recomputes the ack
// cursor, and starts the delivery runner if one is not already alive.
//
// # The two cursors
//
// receivedThrough advances over everything terminally handled, so a redelivery
// is skipped. ackThrough is held one below the lowest PENDING seq, so it never
// covers an entry whose delivery has not concluded. When these were one field
// the ingest answered a concurrent retry with a cursor covering an in-flight
// entry, the relay pruned its retained copy on that answer, and a crash during
// the delivery lost the event with the ack already given — the precise failure
// REL-103 forbids and the one the old synchronous-delivery ordering was believed
// to prevent.
//
// # The bound
//
// Entries are taken only while there is room in the delivery queue
// (deliveryQueueDepth). When there is not, the loop BREAKS rather than skipping
// ahead: the entries are in seq order and receivedThrough is a high-water, so
// stepping over one would put it permanently out of reach of a redelivery. An
// entry not taken is not in receivedThrough, is not acked, and comes back on the
// relay's next flush.
func (in *Ingest) appendBatch(ctx context.Context, batch telemetry.PushBatch) telemetry.Ack {
	in.mu.Lock()
	defer in.mu.Unlock()

	for _, e := range batch.Entries {
		if e.Seq <= in.receivedThrough || in.processed[e.Seq] {
			continue // already terminally processed — idempotent on seq (REL-097)
		}
		if in.deliver != nil && len(in.pending) >= deliveryQueueDepth {
			// Backpressure. See this function's doc for why it BREAKS.
			in.logf("eventingest: delivery queue full at %d entries; taking no more of this batch — "+
				"the remainder stays un-acked and the relay re-pushes it (REL-092/097)", len(in.pending))
			break
		}
		if env, ok := in.processOne(e); ok && in.deliver != nil {
			in.pending[e.Seq] = true
			in.queue = append(in.queue, queuedDelivery{ctx: ctx, seq: e.Seq, env: env})
		}
		in.processed[e.Seq] = true
	}

	// Advance the RECEIVED cursor to the highest ordinary-entry seq taken this
	// batch (REL-092) — NOT a no-gap-contiguous high-water. A gap left below it is
	// jumped, never wedged on: the relay pushes strictly in seq order (REL-090)
	// and only ever leaves a hole for a loss-marked overflow (REL-096, delivered
	// as a marker this batch acknowledges) or a latest-only supersession (REL-094,
	// which by design produces no marker) — in both cases the missing seq is
	// terminally accounted for and will never be delivered, so moving past it is
	// safe. A dropped-invalid seq (EVT-013) counts as received and advances it
	// too, so one un-fixable record never wedges the channel.
	for s := range in.processed {
		if s > in.receivedThrough {
			in.receivedThrough = s
		}
	}
	// Drain the gap set: every terminally-processed seq is now at or below the
	// received cursor (gaps are jumped, not held open), so it carries nothing
	// forward and stays bounded — its only remaining job is intra-batch dedup. A
	// seq at or below the cursor is known-processed via the e.Seq test above.
	for s := range in.processed {
		if s <= in.receivedThrough {
			delete(in.processed, s)
		}
	}
	in.recomputeAckLocked()
	in.startDeliveryLocked()

	// Acknowledge the loss markers this batch delivered so the relay stops
	// re-sending them (REL-092/102). A marker does not itself drive the cursor —
	// that tracks ordinary entries only (REL-092); the higher entries above a
	// marked gap are what carry it past them — but loss_markers_acked keeps the
	// ack honest about which markers were received.
	acked := make([]telemetry.SeqRange, 0, len(batch.LossMarkers))
	for _, m := range batch.LossMarkers {
		acked = append(acked, telemetry.SeqRange{FromSeq: m.FromSeq, ToSeq: m.ToSeq})
	}
	return telemetry.Ack{AckThroughSeq: in.ackThrough, LossMarkersAcked: acked}
}

// recomputeAckLocked raises the reported ack cursor to the highest received seq
// that no pending delivery sits at or below. The caller holds mu.
//
// It only ever RAISES: a cursor the relay has already acted on cannot be taken
// back, and by construction it never needs to be — nothing is ever added to
// pending at or below a seq the cursor already covers, because such a seq is
// skipped as already-processed before it can be appended.
func (in *Ingest) recomputeAckLocked() {
	ack := in.receivedThrough
	for seq := range in.pending {
		if seq-1 < ack {
			ack = seq - 1
		}
	}
	if ack > in.ackThrough {
		in.ackThrough = ack
	}
}

// startDeliveryLocked ensures exactly one delivery runner is alive when the queue
// is non-empty. The caller holds mu.
//
// One runner rather than one goroutine per batch: it is what makes delivery
// follow append order globally, and it is what keeps the goroutine count at zero
// while the box is quiet. The runner exits when the queue empties, so nothing is
// parked on a channel for the process's lifetime.
func (in *Ingest) startDeliveryLocked() {
	if in.draining || len(in.queue) == 0 {
		return
	}
	in.draining = true
	in.drained = make(chan struct{})
	go in.drainQueue()
}

// drainQueue is the single delivery runner: it takes the head of the queue, runs
// the deliverer for it OFF the lock and under deliveryBudget, then records that
// seq as concluded and lets the ack cursor advance over it.
//
// Marking the seq delivered AFTER the deliverer returns is the whole ordering
// the contract rests on. The budget is what guarantees it returns.
func (in *Ingest) drainQueue() {
	for {
		in.mu.Lock()
		if len(in.queue) == 0 {
			in.draining = false
			close(in.drained)
			in.drained = nil
			in.mu.Unlock()
			return
		}
		item := in.queue[0]
		// Release the reference as well as the slot, so a delivered envelope is
		// not retained by the backing array until the queue is reused.
		in.queue[0] = queuedDelivery{}
		in.queue = in.queue[1:]
		deliver := in.deliver
		in.mu.Unlock()

		func() {
			ctx, cancel := context.WithTimeout(item.ctx, deliveryBudget)
			defer cancel()
			deliver(ctx, item.env)
		}()

		in.mu.Lock()
		delete(in.pending, item.seq)
		in.recomputeAckLocked()
		in.mu.Unlock()
	}
}

// Drain blocks until every envelope this ingest has appended has had its
// delivery concluded, or until ctx is done — returning ctx's error in that case.
//
// It is what a deployment calls on shutdown, on the same budget as the other
// drains (cmd/waiveo-feeder), and it is the honest thing to do with work whose
// ack has deliberately NOT been given yet: what finishes inside the window is
// acked on the next flush, and what does not is simply never acked, so the relay
// still holds it and re-pushes after the restart. Nothing is lost either way,
// which is why missing the budget is a log line and not a failure.
func (in *Ingest) Drain(ctx context.Context) error {
	for {
		in.mu.Lock()
		waitOn := in.drained
		in.mu.Unlock()
		if waitOn == nil {
			return nil
		}
		select {
		case <-waitOn:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// processOne reconstructs one wire record into an events/1 envelope and appends
// it, or drops+logs it if it fails validation (EVT-013). It reports the appended
// envelope so the caller can hand it to the deliverer once the lock is released;
// a dropped record reports ok=false and is delivered to nobody, which is the
// point of appending first — what acts on an event is exactly what the durable
// log holds. The caller holds mu.
func (in *Ingest) processOne(e telemetry.Entry) (events.Envelope, bool) {
	env, err := in.buildEnvelope(e)
	if err != nil {
		in.logf("eventingest: dropping telemetry seq %d schema %q: %v (EVT-013)", e.Seq, e.Schema, err)
		return events.Envelope{}, false
	}
	in.sink.Append(env)
	return env, true
}

// buildEnvelope assigns the app-side envelope fields onto a wire record and
// validates the result (EVT-010/011/013). It carries the payload through
// byte-for-byte (REL-090 — the relay already mapped it, this layer never
// re-maps a schema's payload) and PROPAGATES the record's own trace id rather
// than fabricating one (see resolveTraceID). It returns an error for any record
// it cannot turn into a deliverable envelope (an unclassed schema, or a payload
// that fails events.Validate).
func (in *Ingest) buildEnvelope(e telemetry.Entry) (events.Envelope, error) {
	cost, retention, ok := events.ClassFor(e.Schema)
	if !ok {
		return events.Envelope{}, events.ValidationError{Field: "schema", Detail: "no registered cost/retention class for " + e.Schema}
	}
	env := events.Envelope{
		ID:             in.idSeq(),
		Schema:         e.Schema,
		TS:             in.nowMs(),
		ScopeNode:      in.siteScopeNode,
		TraceID:        in.resolveTraceID(e),
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

// resolveTraceID decides the envelope's trace_id from the wire record's own
// (relay/1 REL-006, events/1 EVT-010, api/1 API-063). The caller holds mu.
//
// The rule is: propagate a canonical ULID, substitute a fresh one for anything
// else, and NEVER let the incoming value decide whether the event is delivered.
//
// Propagating is the whole point. EVT-010 defines trace_id as "the originating
// operation's trace ID, propagated from wherever the event was recorded", and a
// relay-originated event was recorded at the relay — so the value the relay set
// at record time IS the correct one, and minting a fresh one here (as this did
// before the wire record carried a trace at all) made API-063's cross-component
// correlation promise false for every event real hardware produces. It is a
// pure carry-through: this layer never rewrites a trace it can use.
//
// The substitution rules are where the type contract is defended, and the two
// non-ULID cases are genuinely different:
//
//   - ABSENT is an expected, legitimate shape, not a defect. The wire field is
//     optional (telemetry.Entry), so a relay built before it existed sends no
//     trace_id and there is nothing to propagate. A fresh ULID keeps the event
//     deliverable and is logged at nothing — an older peer would otherwise emit
//     one log line per entry across its entire backlog, which buries the case
//     below that actually needs attention.
//   - MALFORMED is a defect in the pushing peer and is logged. This relay's own
//     buffer normalizes at record time and cannot produce one; a value arriving
//     here that is not a canonical ULID means some peer is putting a
//     non-conformant identifier on the wire, and that is worth surfacing exactly
//     once per record rather than absorbing in silence.
//
// What neither case may do is fail the event. It would be easy to argue that a
// malformed trace_id should be handed to events.Validate and dropped by the
// EVT-013 gate — but consider what that costs: an automation.run is
// durable-class (REL-093), the relay retains it until acknowledged, and this
// ingest acks a dropped-invalid seq as RECEIVED (so one un-fixable record cannot
// wedge the channel). Dropping on a bad trace id therefore turns a defect in
// CORRELATION METADATA into permanent, unrecoverable loss of the event's actual
// content — with no loss marker, because the relay's accounting shows it
// delivered. REL-103 forbids exactly that silent durable-class loss, and
// trace_id is not subject data: nothing about what happened is carried in it.
// Substituting keeps the payload, keeps EVT-010's ULID type intact, and leaves
// a log line naming the record whose correlation was broken.
//
// A tempting third option is to fall back to the PUSH REQUEST's own Trace-Id
// (apihttp.TraceID) instead of minting. It is rejected: one push carries a whole
// disconnection window's entries, recorded independently, so that fallback would
// stamp one trace across a batch of unrelated operations — asserting a causal
// link that does not exist, which is worse than admitting there is none.
func (in *Ingest) resolveTraceID(e telemetry.Entry) string {
	if ulid.Valid(e.TraceID) {
		return e.TraceID // propagate (EVT-010, REL-006, API-063)
	}
	if e.TraceID != "" {
		in.logf("eventingest: telemetry seq %d schema %q carried a malformed trace_id %q "+
			"(events/1 EVT-010 types it as a ULID); substituting a fresh one — the event is still delivered, "+
			"but it no longer correlates to the operation that produced it (api/1 API-063)",
			e.Seq, e.Schema, e.TraceID)
	}
	return ulid.New()
}
