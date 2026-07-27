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
// Binding authentication (EVT-110–114) is enforced by internal/app/auth's
// middleware, mounted here rather than by the caller so the stream cannot be
// served without it: a connection is authenticated by the session cookie or an
// `Authorization: Bearer` API key and REFUSED BEFORE any upgrade or stream
// begins (EVT-113), and a revocation tears the live stream down rather than
// merely blocking the next connect (EVT-114 — see the revocation select in the
// live loop below).
//
// Scope-node filtering (EVT-120–124) is enforced here too, per event at delivery
// time (EVT-123), over BOTH the resolved backlog and the live tail. A connection
// carries one events.Filter built from three inputs: the principal's readable
// scope-node set (auth.CanRead over the scope tree — the SAME primitive api/1's
// own read scoping uses, never a second implementation of SEC-010's inheritance),
// the optional `selector` query parameter parsed under api/1's grammar, and the
// optional `schemas` query parameter (EVT-101 — SSE has no later client-to-server
// frame to carry either, so both must arrive on the initial request). The filter
// ANDs the three, so a selector can only ever intersect the visible set: naming an
// out-of-reach scope node yields an empty stream, never an error (EVT-121/122).
//
// The scope TREE is read once per connection, at connect. That is the same
// snapshot discipline api/1 applies per request — and the same one the principal's
// own role bindings already have, since they are resolved by the authenticator at
// connect. A stream is one request, so both snapshots share its lifetime; a
// binding revoked mid-stream is covered by EVT-114's credential-revocation
// teardown, but a scope node RE-PARENTED out of the principal's subtree mid-stream
// is not re-read until the client reconnects. That bound is stated here rather
// than left to be discovered.
//
// Deferred, documented seams: the WebSocket binding (Go stdlib has no websocket;
// the WS frame logic in internal/events/delivery.go is already conformance-driven
// but a live WS server is a dependency decision).
package eventsse

import (
	"context"
	stdlog "log"
	"net/http"
	"strings"
	"sync"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
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
	log  events.Log
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
// substrate's single-writer/single-reader contract holds across the concurrent
// ingest and subscriber goroutines a real net/http server runs them on.
//
// log is the events.Log substrate — the in-memory events.EventLog for a
// fixture, or the persistent SQLite implementation
// (internal/app/store.(*Store).EventLog) a deployment wires, so a restart does
// not take the audit trail and every resumable cursor with it.
func NewHub(log events.Log) *Hub {
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
		// it) aged out: mark the loss and resume AT the oldest retained id above
		// that point, which is exactly where After(lastID) picks the tail up.
		g := events.BufferExceededGap(lastID, h.log.OldestRetainedAfter(lastID))
		return &g, tail
	}
	return nil, tail
}

// headLocked is the newest retained id — the fresh-subscribe watermark; the
// caller holds mu. It is "" for an empty log (after("") then yields the whole
// first live batch). It asks the substrate for the head rather than reading the
// whole log and taking its last entry: on the persistent implementation that
// difference is one indexed MAX(id) versus loading every retained event into
// memory on every connect.
func (h *Hub) headLocked() string {
	return h.log.HeadID()
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
	// authn authenticates every connection before any upgrade or stream begins
	// (EVT-110/111/113). It is a required constructor argument rather than an
	// option: an event stream carries platform state, and a binding that could
	// be constructed without an authenticator is a binding that will eventually
	// be constructed without one.
	authn *auth.Authenticator
	// scopeNodes supplies the scope-node tree each connection's visible set
	// (EVT-120) is resolved against. Like authn it is a required constructor
	// argument: a subscriber's visible set is a security boundary, and a binding
	// that could be constructed without the tree that defines it is a binding
	// that will eventually be constructed without it.
	scopeNodes ScopeNodesFunc
	// logf records a non-fatal streaming hiccup (an envelope that fails to
	// serialize, EVT-103) — it defaults to the stdlib logger and is a field so a
	// corrupt frame is logged and skipped, never emitted.
	logf func(format string, args ...any)
}

// ScopeNodesFunc returns the platform's current scope-node set — the tree
// SEC-010's binding inheritance is resolved over, and therefore the tree a
// subscriber's visible set (EVT-120) is computed against. The app store's own
// ScopeNodes read satisfies it; a caller with no tree at all (a fixture) may
// return an empty slice, which leaves a workspace-root-bound principal reading
// everything and a narrower principal reading nothing, exactly as auth.Resolve's
// root fallback defines.
type ScopeNodesFunc func(context.Context) ([]datamodel.ScopeNode, error)

// New returns the GET /events/v1 SSE handler streaming hub's log to subscribers.
// hub is the shared live-transport boundary the telemetry ingest
// (internal/app/eventingest) writes through: each ingest Append records the
// event AND wakes every connected subscriber to drain the newly-appended tail,
// so new events push live to all of them (EVT-100). The same hub instance is
// passed to eventingest.New as its EventSink, which is what wires the writer's
// Append to this reader's wake. scopeNodes supplies the scope tree each
// connection's visible set is resolved against (EVT-120).
func New(hub *Hub, authn *auth.Authenticator, scopeNodes ScopeNodesFunc) http.Handler {
	return &server{hub: hub, authn: authn, scopeNodes: scopeNodes, logf: stdlog.Printf}
}

// filterFor builds this connection's delivery predicate (EVT-120–124) from the
// authenticated principal's own bindings, the scope tree, and the request's
// `selector` / `schemas` query parameters (EVT-101).
//
// The visible set is auth.CanRead over datamodel.ScopeTree.AncestorChain —
// literally the primitive api/1's read scoping calls, so "which nodes may this
// principal see?" has exactly one answer on both surfaces (EVT-120's "computed
// the same way any other api/1-governed read is scoped"). The per-node answer is
// memoized for the connection's lifetime: one stream can ask about the same
// placement node for every event it ever delivers, and the walk is pure over a
// fixed tree and a fixed binding set. The map is confined to the connection's own
// goroutine.
//
// A nil scopeNodes provider, or a failing read, returns an error rather than a
// permissive filter — an unresolvable visible set is answered with a refusal, not
// with the whole world (SEC-005).
func (s *server) filterFor(ctx context.Context, principal auth.Principal, selector apiselector.Selector, schemas []string) (events.Filter, error) {
	if s.scopeNodes == nil {
		return events.Filter{}, errNoScopeTree
	}
	nodes, err := s.scopeNodes(ctx)
	if err != nil {
		return events.Filter{}, err
	}
	tree, _ := datamodel.BuildScopeTree(nodes)

	bindings := principal.Bindings
	seen := make(map[string]bool)
	canRead := func(node string) bool {
		if v, ok := seen[node]; ok {
			return v
		}
		v := auth.CanRead(bindings, tree.AncestorChain(node))
		seen[node] = v
		return v
	}
	inSubtree := func(ancestor, node string) bool {
		if ancestor == node {
			return false
		}
		for _, id := range tree.AncestorChain(node) {
			if id == ancestor {
				return true
			}
		}
		return false
	}
	return events.NewFilter(canRead, selector, inSubtree, schemas), nil
}

// errNoScopeTree is the refusal for a handler constructed with no scope-node
// provider — a wiring bug, answered as an internal error rather than by falling
// back to an unfiltered stream.
var errNoScopeTree = errNoScopeTreeType{}

type errNoScopeTreeType struct{}

func (errNoScopeTreeType) Error() string {
	return "eventsse: no scope-node provider configured; a subscriber's visible set cannot be resolved (EVT-120)"
}

// parseSchemas reads the `schemas` query parameter into the EVT-124 restriction
// list (EVT-101 — on SSE it can only arrive on the initial request).
//
// Two spellings are accepted and unioned, because the contract fixes the FIELD
// (a list of registered schema names) and not a query encoding: a repeated
// parameter (`schemas=a&schemas=b`) and a comma-separated value (`schemas=a,b`).
// A comma cannot appear inside a schema name — EVT-021 fixes the value to a
// `<domain>.<name>` or `<publisher>/<name>.<local-name>` string — so splitting on
// it is unambiguous.
//
// Empty entries are dropped, and a parameter that yields NO names (absent,
// `schemas=`, `schemas=,,`) imposes no restriction at all rather than restricting
// delivery to the empty set. An unrecognized name is not an error: it is simply a
// member no event matches, which the filter answers as an empty stream — the same
// posture EVT-122 fixes for an out-of-reach scope node, and for the same reason.
func parseSchemas(values []string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			name := strings.TrimSpace(part)
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
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

	// EVT-113: authentication is resolved BEFORE the binding is selected and
	// before any header is written, "never with a WS/SSE-level frame, since no
	// session has been established yet to frame one over". A refusal here is an
	// ordinary api/1 Problem with events/1's AUTH_REQUIRED code.
	principal, ok := s.authn.Authenticate(w, r, auth.EventsCodes)
	if !ok {
		return
	}

	// EVT-114: register for this session's revocation BEFORE the stream opens,
	// so a revocation racing the handshake still tears the stream down rather
	// than slipping into the window between "authenticated" and "watching".
	revoked, unwatch := s.authn.Revocations().Watch(principal.SessionID)
	defer unwatch()

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

	// EVT-101: `selector` and `schemas` arrive as query parameters on the initial
	// request, since SSE offers no later client-to-server frame to carry them.
	// Both are read BEFORE the subscriber is registered, so a rejected selector
	// costs no registration and writes its Problem before any stream begins.
	query := r.URL.Query()
	selector, perr := apiselector.Parse(query.Get("selector"))
	if perr != nil {
		// API-045: a selector that does not PARSE is a statement about the
		// REQUEST's syntax — 400 / SELECTOR_INVALID, naming the offending term.
		// It reveals nothing about which scope nodes exist, which is exactly why
		// it may be an error where an out-of-reach node may not (EVT-122).
		apihttp.WriteProblemExt(w, r, traceID, perr.Status, perr.Code, perr.Title, perr.Detail, nil)
		return
	}

	// The connection's delivery predicate (EVT-120–124), resolved from the
	// authenticated principal's own bindings before any event is written.
	filter, err := s.filterFor(r.Context(), principal, selector, parseSchemas(query["schemas"]))
	if err != nil {
		s.logf("eventsse: resolving the subscriber's visible set: %v (EVT-120)", err)
		apihttp.WriteProblem(w, r, traceID, http.StatusInternalServerError, "INTERNAL", "The subscriber's visible scope-node set could not be resolved")
		return
	}

	// Resolve the resume point: a Last-Event-ID header (a browser-native reconnect)
	// takes precedence over a resume_from query parameter (EVT-102).
	resumeFrom := r.Header.Get("Last-Event-ID")
	if resumeFrom == "" {
		resumeFrom = query.Get("resume_from")
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
	// resume; the from-oldest inclusive slice for a gap; empty for fresh),
	// scope-filtered exactly as the live tail is: a REPLAYED event outside the
	// subscriber's visible set is no more deliverable than a live one (EVT-120/123
	// say "an event", not "a live event"). lastID advances over every CONSIDERED
	// envelope, not only every delivered one, so a suppressed event is never
	// re-offered by the live loop's After(lastID) tail read.
	for _, env := range outcome.Events {
		lastID = env.ID
		if !filter.Allows(env) {
			continue
		}
		s.writeEvent(w, env)
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
		case <-revoked:
			// EVT-114: the credential that authenticated this stream was
			// revoked. Ending the stream is the whole requirement — refusing
			// the NEXT connect would leave this already-open pipe delivering
			// platform state to a revoked credential indefinitely.
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
				// EVT-123: the boundary is applied per event, here at delivery
				// time — never delegated to whatever the subscriber's own
				// selector claimed. lastID advances over every considered
				// envelope so the next drain's tail is still gap-free.
				lastID = env.ID
				if !filter.Allows(env) {
					continue
				}
				s.writeEvent(w, env)
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
