// Package eventsse is the app-side live /events/v1 SSE server (events/1
// EVT-100–105, 130–144): the GET handler a subscriber connects to to WATCH the
// platform in real time. It reads the shared events.EventLog the telemetry
// ingest (internal/app/eventingest) writes into, resolves the connection's
// resume point via events.Resolve (fresh | resumed | gap | RESUME_FROM_INVALID),
// streams the resolved backlog using the built SSE framing
// (events.SSEEventLine / events.SSEGapLine), and then pushes every NEW event
// live as the ingest appends it — flushing the ResponseWriter after each frame.
//
// events.EventLog is documented as NOT safe for concurrent use: "the live
// transport (deferred) owns the synchronization boundary." This package is that
// live transport. The boundary is Hub: the single object the ingest writes
// through (Hub.Append) and every subscriber reads through, so a POST
// /telemetry/v1/push appending on one goroutine never races an SSE read on
// another (they share Hub.mu). Hub is also the fan-out — an Append wakes EVERY
// connected subscriber, not just whichever one happens to win a shared receive,
// so a new event pushes live to all of them (EVT-100).
//
// It re-implements none of the log machinery: the ordering/retention log
// (events.EventLog), the four-outcome resume resolution (events.Resolve), the
// EVT-103/104 line framing (events.SSEEventLine / events.SSEGapLine), and the
// api/1 Problem shape (apihttp.WriteProblem) are all called, not rebuilt.
//
// Deferred, documented seams: the WebSocket binding (Go stdlib has no websocket;
// the WS frame logic in internal/events/delivery.go is already conformance-driven
// but a live WS server is a dependency decision), and binding authentication
// (EVT-110–114) + the roles-based visible set (EVT-120), which land with the
// security model — the POC endpoint is unauthenticated.
package eventsse

import (
	stdlog "log"
	"net/http"
	"sync"

	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// Hub is the app-side live-transport boundary over the shared events.EventLog:
// the single object the telemetry ingest writes through and every SSE subscriber
// reads through. It exists because events.EventLog is not safe for concurrent
// use and delegates its synchronization to "the live transport" — this is that
// transport, so every log mutation (Append) and every log read (subscribe /
// after) is serialized under mu. It is also the fan-out registry: each Append
// wakes every registered subscriber, where a single shared channel would deliver
// the wake to only one of N waiting subscribers (EVT-100).
type Hub struct {
	mu   sync.Mutex
	log  *events.EventLog
	subs map[*subscriber]struct{}
	// done is closed once by Close for a graceful server shutdown; every
	// subscriber loop selects on it and returns, so an otherwise-endless SSE
	// stream can be torn down without waiting for the client to disconnect.
	done     chan struct{}
	shutdown bool
}

// subscriber is one connected SSE stream's wake mailbox: a buffered(1) channel
// the Hub coalesces wakes into. A full buffer means "already signalled, not yet
// drained" — one pending wake is enough because a drain reads the WHOLE
// newly-appended tail via after(lastID), so coalescing loses no event. The
// per-subscriber channel is the fan-out unit: Append signals every subscriber's
// channel, where a single shared channel would wake only one (EVT-100).
type subscriber struct {
	ch chan struct{}
}

// Subscription is a registered SSE stream's handle on the Hub. head is the
// fresh-subscribe watermark (the newest retained id at the instant this
// subscriber was registered), captured atomically with registration so no event
// appended during the handshake can slip between "connected" and "watermark
// taken" (EVT-132).
type Subscription struct {
	hub  *Hub
	sub  *subscriber
	head string
}

// NewHub returns a live-transport Hub owning log. After this call, log MUST be
// mutated only through Hub.Append and read only through the Hub, so the
// EventLog's single-writer/single-reader contract holds across the concurrent
// ingest and subscriber goroutines a real net/http server runs them on.
func NewHub(log *events.EventLog) *Hub {
	return &Hub{log: log, subs: make(map[*subscriber]struct{}), done: make(chan struct{})}
}

// Append records env in the shared log and wakes every connected subscriber. It
// is the ingest's single write entry point — internal/app/eventingest holds the
// Hub as its EventSink, so a telemetry-derived event flows Append -> wake -> live
// push with no separate notify wiring (EVT-100). Append takes the same lock the
// subscriber reads take, so the backing slice is never resized concurrently with
// a read (the data-race-free boundary events.EventLog requires). Each wake is a
// non-blocking send: a subscriber that has not yet drained its previous wake
// keeps just the one pending, because the drain reads the entire After(lastID)
// tail.
func (h *Hub) Append(env events.Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log.Append(env)
	for s := range h.subs {
		select {
		case s.ch <- struct{}{}:
		default: // already signalled and not yet drained — coalesced
		}
	}
}

// Close ends every live subscriber stream for a graceful server shutdown by
// closing the shared done channel their loops select on. Idempotent.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.shutdown {
		h.shutdown = true
		close(h.done)
	}
}

// subscribe registers a wake mailbox and, atomically under the same lock,
// resolves the resume outcome and snapshots the current head. Doing all three
// under one lock hold is what makes the live push loss-free: from the instant
// subscribe returns, the subscriber is registered (so every LATER Append wakes
// it) AND its fresh watermark / resumed backlog is a snapshot consistent with
// that registration point. Nothing appended concurrently with the handshake can
// fall into a gap — it is either before the snapshot (already in the backlog or
// at/below the head watermark, i.e. pre-existing) or after it (wakes the
// registered subscriber and is delivered live) (EVT-132). A rejected resume_from
// registers nothing and returns a nil Subscription.
func (h *Hub) subscribe(resumeFrom string) (*Subscription, events.ResumeOutcome, *events.ResumeError) {
	h.mu.Lock()
	defer h.mu.Unlock()
	outcome, rerr := events.Resolve(h.log, resumeFrom)
	if rerr != nil {
		return nil, outcome, rerr
	}
	s := &subscriber{ch: make(chan struct{}, 1)}
	h.subs[s] = struct{}{}
	return &Subscription{hub: h, sub: s, head: h.headLocked()}, outcome, nil
}

// after returns the retained tail strictly after id, read under the lock so it
// never races a concurrent Append's slice mutation. EventLog.After returns a
// fresh copy, so the result is safe to stream after the lock is released.
func (h *Hub) after(id string) []events.Envelope {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.log.After(id)
}

// drain is one wake's worth of live delivery for a subscriber last at lastID: the
// not-yet-delivered tail, plus a buffer_exceeded gap frame when events past
// lastID have aged out of retention before this drain (a mid-stream slow-consumer
// drop, EVT-142/143). Both are computed under ONE lock hold so they are a
// consistent snapshot — the gap's to_id equals the first retained id the tail
// then delivers. This is the live-loop analogue of Resolve's connect-time
// retention_expired gap: a discontinuity is always marked, never silently a
// truncated tail with a bare id jump. On an unbounded (or not-lagged) log there
// is no eviction past lastID, so gap is nil and this is a plain tail read.
func (h *Hub) drain(lastID string) (*events.GapFrame, []events.Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tail := h.log.After(lastID)
	if h.log.EvictedAfter(lastID) {
		// The subscriber's own last-delivered point (and undelivered events after
		// it) aged out: mark the loss and resume AT the oldest retained id, which
		// is exactly where After(lastID) picks the tail up.
		g := events.BufferExceededGap(lastID, h.log.OldestRetainedID())
		return &g, tail
	}
	return nil, tail
}

// headLocked is the newest retained id — the fresh-subscribe watermark; the
// caller holds mu. It is "" for an empty log (after("") then yields the whole
// first live batch).
func (h *Hub) headLocked() string {
	all := h.log.After("")
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1].ID
}

// wake is the subscriber's wake channel; the live loop blocks on it.
func (sub *Subscription) wake() <-chan struct{} { return sub.sub.ch }

// close removes the subscription from the Hub's fan-out set on disconnect.
// Idempotent for a given Subscription; the wake channel is never closed (Append
// only ever sends), so a late in-flight Append cannot panic sending to it.
func (sub *Subscription) close() {
	sub.hub.mu.Lock()
	delete(sub.hub.subs, sub.sub)
	sub.hub.mu.Unlock()
}

// server is the GET /events/v1 SSE handler. It holds the Hub (the shared
// read/write boundary over the event log) and streams each connection's resolved
// backlog then every live Append, on the connection goroutine, woken by the
// Hub's per-subscriber fan-out.
type server struct {
	hub *Hub
	// logf records a non-fatal streaming hiccup (an envelope that fails to
	// serialize, EVT-103) — it defaults to the stdlib logger and is a field so a
	// corrupt frame is logged and skipped, never emitted.
	logf func(format string, args ...any)
}

// New returns the GET /events/v1 SSE handler streaming hub's log to subscribers.
// hub is the shared live-transport boundary the telemetry ingest
// (internal/app/eventingest) writes through: each ingest Append records the
// event AND wakes every connected subscriber to drain the newly-appended tail,
// so new events push live to all of them (EVT-100). The same hub instance is
// passed to eventingest.New as its EventSink, which is what wires the writer's
// Append to this reader's wake.
func New(hub *Hub) http.Handler {
	return &server{hub: hub, logf: stdlog.Printf}
}

// ServeHTTP handles an SSE subscribe (EVT-100–105, 130–144). It selects the SSE
// binding from the request shape (EVT-001/100 — a WS upgrade is refused, the live
// WS server is deferred), resolves the resume point (Last-Event-ID header over
// resume_from query, EVT-102) via the Hub, streams the resolved backlog (a
// leading event: gap for a retention_expired resume, EVT-104/140), then blocks
// streaming each newly-appended event live until the client disconnects (request
// context) or the server shuts down (Hub.Close) — a malformed or unrecorded
// resume_from is a RESUME_FROM_INVALID Problem written before any event
// (EVT-134).
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)

	if r.Method != http.MethodGet {
		apihttp.WriteProblem(w, r, traceID, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method Not Allowed")
		return
	}

	// Binding selection at the shared path (EVT-001/100): a WS upgrade selects the
	// WS binding, which is deferred (Go stdlib has no websocket); only an
	// Accept: text/event-stream request is served here.
	switch events.SelectBinding(r.Header.Get("Upgrade") != "", r.Header.Get("Accept")) {
	case events.BindingSSE:
		// proceed
	case events.BindingWS:
		apihttp.WriteProblem(w, r, traceID, http.StatusNotImplemented, "WS_BINDING_DEFERRED", "The events/1 WebSocket binding is not yet served; connect over SSE (Accept: text/event-stream)")
		return
	default:
		apihttp.WriteProblem(w, r, traceID, http.StatusNotAcceptable, "SSE_REQUIRED", "events/1 requires Accept: text/event-stream")
		return
	}

	// The SSE stream needs an incrementally flushable writer; without one there is
	// no live push, so refuse rather than buffer the whole (unbounded) stream.
	flusher, ok := w.(http.Flusher)
	if !ok {
		apihttp.WriteProblem(w, r, traceID, http.StatusInternalServerError, "STREAMING_UNSUPPORTED", "Streaming is not supported")
		return
	}

	// Resolve the resume point: a Last-Event-ID header (a browser-native reconnect)
	// takes precedence over a resume_from query parameter (EVT-102).
	resumeFrom := r.Header.Get("Last-Event-ID")
	if resumeFrom == "" {
		resumeFrom = r.URL.Query().Get("resume_from")
	}

	// Register as a live subscriber AND snapshot the resume outcome + fresh
	// watermark atomically (Hub.subscribe holds one lock across all three). This
	// happens BEFORE the 200/headers are written, so an event the ingest appends
	// concurrently with the handshake is never lost to the window between
	// announcing the connection live and capturing the watermark (EVT-132/143).
	sub, outcome, rerr := s.hub.subscribe(resumeFrom)
	if rerr != nil {
		// A malformed or never-recorded resume_from is refused before any event is
		// delivered — never silently treated as a fresh subscribe (EVT-134).
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, rerr.Code, "The resume_from was malformed or names an event the platform never recorded")
		return
	}
	defer sub.close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// lastID tracks the highest id already delivered to this subscriber, so the
	// live loop's after(lastID) yields exactly the not-yet-seen tail (gap-free,
	// duplicate-free, EVT-133/143).
	var lastID string
	switch outcome.Result {
	case events.ResumeResultFresh:
		// Fresh: deliver only events from connection time forward (EVT-132), so
		// watermark at the head snapshotted with registration — the live loop
		// streams strictly after it.
		lastID = sub.head
	case events.ResumeResultResumed:
		// Resume strictly after the requested id; if its backlog is empty (the id
		// is the head), the watermark is that id itself.
		lastID = resumeFrom
	case events.ResumeResultGap:
		// A retention_expired discontinuity: mark it before any event (EVT-104/140),
		// then delivery resumes AT to_id inclusive via outcome.Events below.
		s.writeString(w, events.SSEGapLine(*outcome.Gap))
		lastID = outcome.ResumeAtID
	}

	// Stream the resolved backlog in id order (After(resume_from) for a clean
	// resume; the from-oldest inclusive slice for a gap; empty for fresh).
	for _, env := range outcome.Events {
		s.writeEvent(w, env)
		lastID = env.ID
	}
	flusher.Flush()

	// Live: drain the newly-appended tail on every wake until the client goes away
	// (request context) or the server shuts down (Hub.Close closes done).
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.hub.done:
			return
		case <-sub.wake():
			// Drain the not-yet-delivered tail. If the subscriber lagged far enough
			// behind on a bounded log that undelivered events aged out before this
			// wake, drain returns a buffer_exceeded gap first — the mid-stream
			// analogue of the connect-time retention_expired gap, so a discontinuity
			// is marked, never a silent id jump (EVT-142/143).
			gap, tail := s.hub.drain(lastID)
			if gap != nil {
				s.writeString(w, events.SSEGapLine(*gap))
				lastID = gap.ToID
			}
			for _, env := range tail {
				s.writeEvent(w, env)
				lastID = env.ID
			}
			flusher.Flush()
		}
	}
}

// writeEvent frames env as an SSE event line (EVT-103) and writes it. An
// envelope that fails to serialize is logged and skipped — never emitted as a
// corrupt line (SSEEventLine's own contract) — so one bad record can't poison the
// stream.
func (s *server) writeEvent(w http.ResponseWriter, env events.Envelope) {
	line, err := events.SSEEventLine(env)
	if err != nil {
		s.logf("eventsse: dropping unserializable event id %q: %v (EVT-103)", env.ID, err)
		return
	}
	s.writeString(w, line)
}

// writeString writes str to w, ignoring the error: a mid-stream write failure is
// a dropped subscriber connection, which the request-context select already
// observes and closes on — there is nothing to recover here.
func (s *server) writeString(w http.ResponseWriter, str string) {
	_, _ = w.Write([]byte(str))
}
