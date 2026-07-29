// Package eventsse is the app-side live /events/v1 server (events/1 EVT-090–105,
// 130–144): the GET handler a subscriber connects to to WATCH the platform in
// real time, over EITHER of the contract's two bindings. It reads the shared
// events.Log the telemetry ingest (internal/app/eventingest) and the auth plane's
// auditor write into, resolves the connection's resume point against that
// principal's own visible set via events.ResolveVisible (fresh | resumed | gap
// | RESUME_FROM_INVALID), streams the resolved backlog,
// and then pushes every NEW event live as it is appended.
//
// EVT-001 puts BOTH bindings on the one path and distinguishes them by the
// request's own shape: a WS `Upgrade` header selects the WebSocket binding
// (ws.go), an `Accept: text/event-stream` header with no Upgrade selects SSE
// (this file). They are two framings of one stream, not two implementations: the
// subscription (`open`), the delivery predicate (`filterFor` → events.Filter),
// the backlog/live delivery order and the gap marking (`deliverBacklog`,
// `drainOnce`) are shared code both bindings run, and the only thing either one
// owns privately is how it puts a frame on its own wire (`sink`). That is what
// makes "a WS subscriber and an SSE subscriber over the same log see the same
// envelopes" a structural property rather than a claim maintained by hand.
//
// The substrate is the events.Log INTERFACE, not one storage choice: a
// deployment passes the persistent SQLite implementation
// (internal/app/store.(*Store).EventLog), so a restart takes neither the audit
// trail nor any subscriber's resumable cursor with it; a fixture passes the
// in-memory events.EventLog.
//
// events.Log's contract says an implementation is NOT required to be safe for
// concurrent use: "the live transport owns the synchronization boundary." This
// package is that live transport. The boundary is Hub: the single object the
// ingest writes through (Hub.Append) and every subscriber reads through, so a
// POST /telemetry/v1/push appending on one goroutine never races a subscriber's
// read on another (they share Hub.mu). Hub is also the fan-out — an Append wakes EVERY
// connected subscriber, not just whichever one happens to win a shared receive,
// so a new event pushes live to all of them (EVT-100).
//
// It re-implements none of the log machinery: the ordering/retention log
// (events.Log), the four-outcome resume resolution (events.ResolveVisible), the
// EVT-093/094 WS frames and EVT-103/104 SSE lines (events.Event / events.Gap /
// events.HelloAck / events.SSEEventLine / events.SSEGapLine), the EVT-096 close
// vocabulary (events.CloseReason), and the api/1 Problem shape
// (apihttp.WriteProblem) are all called, not rebuilt.
//
// Binding authentication (EVT-110–114) is enforced by internal/app/auth's
// middleware, mounted here rather than by the caller so the stream cannot be
// served without it: a connection is authenticated by the session cookie or an
// `Authorization: Bearer` API key and REFUSED BEFORE any upgrade or stream
// begins (EVT-113), and a revocation tears the live stream down rather than
// merely blocking the next connect (EVT-114 — see the revocation select in each
// binding's live loop). Both bindings authenticate through the one
// Authenticate call in ServeHTTP, ahead of binding selection, so the WS binding
// cannot answer an unauthenticated upgrade and cannot be made to differ from
// SSE on which credentials it accepts or where they may ride.
//
// Scope-node filtering (EVT-120–124) is enforced here too, per event at delivery
// time (EVT-123), over BOTH the resolved backlog and the live tail. A connection
// carries one events.Filter built from three inputs: the principal's readable
// scope-node set (auth.CanRead over the scope tree — the SAME primitive api/1's
// own read scoping uses, never a second implementation of SEC-010's inheritance),
// the optional `selector` parsed under api/1's grammar, and the optional
// `schemas` restriction — carried as query parameters on SSE (EVT-101: it has no
// later client-to-server frame, so both must arrive on the initial request) and
// as `hello` fields on WS (EVT-091), resolved by the same `open`. The filter
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
package eventsse

import (
	"context"
	stdlog "log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
)

// Hub is the app-side live-transport boundary over the shared events.EventLog:
// the single object the telemetry ingest writes through and every subscriber on
// either binding reads through. It exists because events.EventLog is not safe for concurrent
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
	// onAppend (OnAppend) runs after every appended envelope, outside the lock —
	// the seam a non-subscriber consumer of the log hangs off. The subscriber
	// fan-out above cannot serve it: a subscriber is a connection with a cursor
	// and a visible set, and the outbound-webhook delivery loop is neither. It
	// is guarded by mu so a hook registered at startup is never observed torn.
	onAppend func()
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
	// Outside the lock, in the same spirit as the store's post-commit hook: a
	// hook that read the log back would otherwise deadlock against the very
	// Append that invoked it, and one that blocked would stall every producer.
	if hook := h.appendUnderLock(env); hook != nil {
		hook()
	}
}

// appendUnderLock is Append's locked half, split out so the unlock stays a defer
// (a panicking substrate must not leave the Hub permanently locked) while the
// OnAppend hook still runs outside it. It returns the hook rather than calling
// it.
func (h *Hub) appendUnderLock(env events.Envelope) func() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log.Append(env)
	for s := range h.subs {
		select {
		case s.ch <- struct{}{}:
		default: // already signalled and not yet drained — coalesced
		}
	}
	return h.onAppend
}

// OnAppend registers fn to run after every appended envelope, on the appending
// goroutine and after the lock is released.
//
// It is the seam a consumer that is NOT a stream subscriber hangs off — the
// outbound-webhook delivery loop, whose "subscription" is a persisted cursor on
// a row rather than a live connection. Registering here rather than wrapping the
// Hub is what makes the wake unmissable: this type's own contract is that every
// log mutation goes through Append, so a producer added later cannot forget to
// notify a wrapper it does not know exists.
//
// fn MUST NOT block and MUST NOT re-enter the Hub — it runs on the producer's
// goroutine, so anything slow belongs behind a non-blocking signal of the hook's
// own (Loop.Notify is exactly that shape). Passing nil clears the hook.
// Intended to be set once at startup, before any producer is running.
func (h *Hub) OnAppend(fn func()) {
	h.mu.Lock()
	h.onAppend = fn
	h.mu.Unlock()
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

// subscriberCount reports how many subscribers are currently registered for the
// fan-out — observability for tests and operators. A connection that
// disconnected cleanly leaves this count where it found it; one that leaked its
// registration would not.
func (h *Hub) subscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
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
//
// visible is the connecting principal's own visible set (EVT-120), and the
// resume point is resolved against it rather than against the whole log: a
// resume_from naming an event outside that set is refused exactly as one naming
// an event that was never recorded is, so a subscriber cannot use the difference
// between the two answers to discover whether an arbitrary event id exists
// (EVT-134a).
func (h *Hub) subscribe(resumeFrom string, visible func(events.Envelope) bool) (*Subscription, events.ResumeOutcome, *events.ResumeError) {
	h.mu.Lock()
	defer h.mu.Unlock()
	outcome, rerr := events.ResolveVisible(h.log, resumeFrom, visible)
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

// drain is one wake's worth of live delivery for a subscriber last at lastID:
// the not-yet-considered tail, plus whether any event past lastID aged out of
// retention before this drain (a mid-stream slow-consumer drop, EVT-142/143).
// Both are read under ONE lock hold so they are a consistent snapshot of the
// substrate at this wake. On an unbounded (or not-lagged) log nothing past
// lastID is ever evicted, so evicted is false and this is a plain tail read.
//
// It reports the CONDITION, not the marker. Composing the marker needs two
// things the Hub does not have and must not guess at: the subscriber's own
// last-DELIVERED id, which is EVT-140's from_id and differs from the watermark
// this is called with (the watermark advances over every event the connection
// CONSIDERED, including ones it was not allowed to send), and the subscriber's
// visible set, which EVT-134a makes the bound on to_id. Both belong to the
// connection, so the marker is built there — see stream.
func (h *Hub) drain(lastID string) (evicted bool, tail []events.Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.log.EvictedAfter(lastID), h.log.After(lastID)
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

// server is the GET /events/v1 handler serving BOTH events/1 bindings. It holds
// the Hub (the shared read/write boundary over the event log) and streams each
// connection's resolved backlog then every live Append, on the connection
// goroutine, woken by the Hub's per-subscriber fan-out.
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

	// The stream's time bounds (EVT-095/105a/142/142a). They are fields rather
	// than constants so a test drives the behavior on an injected cadence
	// instead of waiting out the production one — the same seam
	// internal/feeder/relayconn exposes for the relay/1 connection, and the
	// injectable clock the contract's own conformance notes call for in place
	// of wall-clock sleeps. See ws.go for what the first three bound.
	writeTimeout time.Duration
	pingInterval time.Duration
	pongTimeout  time.Duration
	// gapDeferral bounds how long a mid-stream buffer_exceeded marker may wait
	// for a resume point inside the subscriber's own visible set (EVT-142a).
	gapDeferral time.Duration
}

// Option configures the handler New returns.
type Option func(*server)

// WithHeartbeat overrides both bindings' keepalive cadence: on WS a ping on a
// connection otherwise idle for interval, whose pong must arrive within timeout
// or the connection is closed IDLE_TIMEOUT (EVT-095); on SSE a comment line on a
// connection otherwise idle for the same interval (EVT-105a — SSE has no
// client-to-server frame to answer with, so it has no timeout half). The
// defaults are the contract's own 30s/10s.
func WithHeartbeat(interval, timeout time.Duration) Option {
	return func(s *server) { s.pingInterval, s.pongTimeout = interval, timeout }
}

// WithGapDeferral overrides how long a mid-stream buffer_exceeded marker may be
// deferred while no id inside the subscriber's visible set is available to name
// as its to_id (EVT-142a). A marker still unresolved at the bound ends the
// connection SLOW_CONSUMER_DISCONNECTED rather than staying pending, so a
// deferral cannot become permanent silent loss.
func WithGapDeferral(d time.Duration) Option {
	return func(s *server) { s.gapDeferral = d }
}

// WithWriteTimeout overrides the WS binding's per-frame write deadline — the
// bound EVT-142 draws the line at between gapping a slow subscriber and
// disconnecting it SLOW_CONSUMER_DISCONNECTED.
func WithWriteTimeout(d time.Duration) Option {
	return func(s *server) { s.writeTimeout = d }
}

// ScopeNodesFunc returns the platform's current scope-node set — the tree
// SEC-010's binding inheritance is resolved over, and therefore the tree a
// subscriber's visible set (EVT-120) is computed against. The app store's own
// ScopeNodes read satisfies it; a caller with no tree at all (a fixture) may
// return an empty slice, which leaves a workspace-root-bound principal reading
// everything and a narrower principal reading nothing, exactly as auth.Resolve's
// root fallback defines.
type ScopeNodesFunc func(context.Context) ([]datamodel.ScopeNode, error)

// New returns the GET /events/v1 handler streaming hub's log to subscribers over
// either binding (EVT-001: one path, the binding selected by the request's own
// shape). hub is the shared live-transport boundary the telemetry ingest
// (internal/app/eventingest) writes through: each ingest Append records the
// event AND wakes every connected subscriber to drain the newly-appended tail,
// so new events push live to all of them (EVT-100). The same hub instance is
// passed to eventingest.New as its EventSink, which is what wires the writer's
// Append to this reader's wake. scopeNodes supplies the scope tree each
// connection's visible set is resolved against (EVT-120).
func New(hub *Hub, authn *auth.Authenticator, scopeNodes ScopeNodesFunc, opts ...Option) http.Handler {
	s := &server{
		hub: hub, authn: authn, scopeNodes: scopeNodes, logf: stdlog.Printf,
		writeTimeout: defaultWriteTimeout,
		pingInterval: defaultPingInterval,
		pongTimeout:  defaultPongTimeout,
		gapDeferral:  defaultGapDeferral,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
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

// subscribeRequest is one connection's subscribe parameters, however its own
// binding carried them: SSE reads all three off the initial request (query
// parameters plus the Last-Event-ID header, EVT-101/102), WS reads all three off
// the `hello` frame (EVT-091). Naming them as one value is what lets the two
// bindings share `open` below rather than each resolving a subscription its own
// way.
type subscribeRequest struct {
	// selector is the raw, unparsed api/1 label selector (EVT-121).
	selector string
	// schemas is the optional registered-schema restriction (EVT-124), in
	// whatever spelling the binding carried it — parseSchemas normalizes both.
	schemas []string
	// resumeFrom is the resume cursor (EVT-130–134); empty means a fresh
	// subscribe.
	resumeFrom string
}

// openError is a refusal raised before any event is delivered. It carries an
// api/1 Problem's fields for the SSE binding, which answers with an HTTP
// response, AND the events/1 taxonomy code the WS binding names in its close
// reason (EVT-096) — one refusal, two renderings, so the two bindings cannot
// disagree about WHICH conditions refuse a subscription.
type openError struct {
	status int
	code   string
	title  string
	detail string
}

// open resolves one connection's subscription: the parsed selector, the delivery
// filter, and the live registration plus its resume outcome — in that order, so
// a refusal costs no registration and lands before any event is delivered
// (EVT-134). Both bindings call it, which is what keeps "what may this
// subscriber see, and where does its delivery start?" a single answer.
func (s *server) open(ctx context.Context, principal auth.Principal, req subscribeRequest) (*Subscription, events.ResumeOutcome, events.Filter, *openError) {
	selector, perr := apiselector.Parse(req.selector)
	if perr != nil {
		// API-045: a selector that does not PARSE is a statement about the
		// REQUEST's syntax — 400 / SELECTOR_INVALID, naming the offending term.
		// It reveals nothing about which scope nodes exist, which is exactly why
		// it may be an error where an out-of-reach node may not (EVT-122).
		return nil, events.ResumeOutcome{}, events.Filter{}, &openError{perr.Status, perr.Code, perr.Title, perr.Detail}
	}

	// The connection's delivery predicate (EVT-120–124), resolved from the
	// authenticated principal's own bindings before any event is written.
	filter, err := s.filterFor(ctx, principal, selector, parseSchemas(req.schemas))
	if err != nil {
		s.logf("eventsse: resolving the subscriber's visible set: %v (EVT-120)", err)
		return nil, events.ResumeOutcome{}, events.Filter{}, &openError{
			http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"The subscriber's visible scope-node set could not be resolved.",
		}
	}

	// Register as a live subscriber AND snapshot the resume outcome + fresh
	// watermark atomically (Hub.subscribe holds one lock across all three). This
	// happens BEFORE the binding announces the stream (the SSE 200, the WS
	// hello-ack), so an event the ingest appends concurrently with the handshake
	// is never lost to the window between announcing the connection live and
	// capturing the watermark (EVT-132/143).
	//
	// The resume point is resolved against THIS principal's visible set — the
	// filter's scope-node half, not the whole filter, since a client's own
	// selector and schemas restrictions narrow what it is sent, never what it is
	// entitled to resume from (EVT-120/121/124).
	sub, outcome, rerr := s.hub.subscribe(req.resumeFrom, filter.Visible)
	if rerr != nil {
		// A resume_from that is malformed, names an event the platform never
		// recorded, or names one outside this subscriber's visible set is refused
		// before any event is delivered — never silently treated as a fresh
		// subscribe (EVT-134/134a). The three are ONE refusal, deliberately: a
		// distinguishable answer for the third would tell a caller that an id it
		// may not read nevertheless exists, which is the probe EVT-122 forbids
		// against scope nodes and EVT-134a forbids against event ids. The detail
		// below therefore describes the outcome, not which of the three produced
		// it.
		return nil, events.ResumeOutcome{}, events.Filter{}, &openError{
			http.StatusBadRequest, rerr.Code, "Bad Request",
			"The resume_from was malformed or does not name an event this subscriber can resume from.",
		}
	}
	return sub, outcome, filter, nil
}

// sink is one binding's frame writer — the ONLY thing that differs between WS
// and SSE delivery. Everything above it (which events a subscriber may see, in
// what order, and where a gap is marked) is shared, so the two bindings cannot
// drift into two different streams over the same log.
//
// An implementation reports a write failure so the caller can classify it; a
// sink that cannot fail (SSE, whose writes are best-effort into an already-
// disconnecting response) returns nil.
type sink interface {
	// event writes one delivered envelope (EVT-093 / EVT-103).
	event(env events.Envelope) error
	// gap writes one loss marker (EVT-094 / EVT-104).
	gap(g events.GapFrame) error
	// flush pushes everything written so far to the client.
	flush() error
}

// stream is ONE connection's delivery state, shared by both bindings. It exists
// because a mid-stream loss marker (EVT-140/142) cannot be composed from the
// substrate alone: every field of it is a property of the connection.
//
// considered and delivered are deliberately two values, not one. The watermark
// the live loop reads the tail after must advance over every envelope the
// connection took into account — including the ones EVT-120/123 forbade it to
// send — or a suppressed event is re-offered on every later wake. from_id is the
// opposite quantity: EVT-140 defines it as "the last id successfully delivered
// before a mid-stream gap", and naming the watermark there would put an id from
// outside the subscriber's visible set on the wire, which is the same disclosure
// EVT-134a closes for to_id. One value cannot be both.
type stream struct {
	// considered is the highest id this connection has taken into account: the
	// argument the next Hub.drain reads the tail strictly after.
	considered string
	// delivered is the last id this connection actually WROTE to its subscriber
	// — EVT-140's from_id, and by construction an id inside the visible set,
	// since nothing else is ever written.
	delivered string
	// anchor is the resume_from this connection opened with (empty for a fresh
	// subscribe). It is the subscriber's own last-known point before anything
	// has been delivered on this connection, which is the other reading EVT-140
	// gives from_id, and it is inside the visible set because ResolveVisible
	// refused it otherwise (EVT-134a).
	anchor string
	// pending records that events past this connection's point aged out before
	// it could consider them, and no id inside its visible set was available to
	// name as the marker's to_id yet (EVT-142a). pendingFrom is the from_id
	// captured at the instant the loss was detected — nil when no last-known
	// point exists, which EVT-140 spells null.
	pending     bool
	pendingFrom *string
	// deferral bounds how long pending may stay true (EVT-142a). It is armed
	// ONCE, when the marker becomes pending, and never re-armed while it stays
	// pending: re-arming it per drain would let a stream with steady
	// out-of-scope traffic defer the same marker forever, which is the silent
	// loss the bound exists to prevent.
	deferral *time.Timer
	armed    bool
	bound    time.Duration
}

// newStream is one connection's delivery state, with its deferral timer created
// stopped — a connection that never lags never arms it. The immediate Stop is
// safe even for a bound so short the timer has already fired: since Go 1.23 a
// timer channel is unbuffered and Stop guarantees no stale value is delivered
// afterwards, so expiry() cannot fire for a marker that was never pending.
func newStream(anchor string, bound time.Duration) *stream {
	t := time.NewTimer(bound)
	t.Stop()
	return &stream{anchor: anchor, deferral: t, bound: bound}
}

// lastKnownPoint is EVT-140's from_id: "the subscriber's own last-known point
// (the requested resume_from, or the last id successfully delivered before a
// mid-stream gap; null only when no such point exists)". The delivered id wins
// where both exist, being the later of the two.
func (st *stream) lastKnownPoint() *string {
	switch {
	case st.delivered != "":
		v := st.delivered
		return &v
	case st.anchor != "":
		v := st.anchor
		return &v
	default:
		return nil
	}
}

// markLoss records a mid-stream discontinuity. A second loss while a marker is
// already pending is folded into it rather than queued: from_id has not moved
// (nothing has been delivered since), and the eventual to_id covers both, so one
// marker describes the whole range — which is exactly what EVT-140's
// {from_id,to_id} shape says.
func (st *stream) markLoss() {
	if st.pending {
		return
	}
	st.pending, st.pendingFrom = true, st.lastKnownPoint()
}

// resolveLoss consumes the pending marker, naming toID as the point delivery
// resumes at. The caller has already established that toID is inside the
// subscriber's visible set (EVT-134a).
func (st *stream) resolveLoss(toID string) events.GapFrame {
	g := events.BufferExceededGap(st.pendingFrom, toID)
	st.pending, st.pendingFrom = false, nil
	if st.armed {
		st.deferral.Stop()
		st.armed = false
	}
	return g
}

// arm starts the deferral bound the first time a marker is left pending.
func (st *stream) arm() {
	if st.pending && !st.armed {
		st.armed = true
		st.deferral.Reset(st.bound)
	}
}

// expiry fires when a pending marker has gone undeliverable for the whole bound
// (EVT-142a). Both bindings select on it; neither may ignore it.
func (st *stream) expiry() <-chan time.Time { return st.deferral.C }

// stop releases the deferral timer when the connection ends.
func (st *stream) stop() { st.deferral.Stop() }

// deliverBacklog writes the resolved backlog to sk, advancing st's watermark
// over every envelope it considered and its delivered id over every envelope it
// actually sent.
//
// The backlog is scope-filtered exactly as the live tail is: a REPLAYED event
// outside the subscriber's visible set is no more deliverable than a live one
// (EVT-120/123 say "an event", not "a live event").
func (s *server) deliverBacklog(sk sink, sub *Subscription, outcome events.ResumeOutcome, st *stream, filter events.Filter) error {
	switch outcome.Result {
	case events.ResumeResultFresh:
		// Fresh: deliver only events from connection time forward (EVT-132), so
		// watermark at the head snapshotted with registration — the live loop
		// streams strictly after it.
		st.considered = sub.head
	case events.ResumeResultResumed:
		// Resume strictly after the requested id; if its backlog is empty (the id
		// is the head), the watermark is that id itself.
		st.considered = st.anchor
	case events.ResumeResultGap:
		// A retention_expired discontinuity: mark it before any event
		// (EVT-094/104/140), then delivery resumes AT to_id inclusive via
		// outcome.Events below.
		if err := sk.gap(*outcome.Gap); err != nil {
			return err
		}
		st.considered = outcome.ResumeAtID
	}

	for _, env := range outcome.Events {
		st.considered = env.ID
		if !filter.Allows(env) {
			continue
		}
		if err := sk.event(env); err != nil {
			return err
		}
		st.delivered = env.ID
	}
	return sk.flush()
}

// drainOnce is one wake's worth of live delivery, advancing st in place.
//
// If the subscriber lagged far enough behind on a bounded log that events aged
// out past its watermark before this wake, the loss is marked — the mid-stream
// analogue of the connect-time retention_expired gap, so a discontinuity is
// never a silent id jump (EVT-142/143).
//
// The marker is emitted ahead of the first event in the tail that this
// subscriber may SEE, and names that event's id as to_id. It is resolved against
// filter.Visible rather than filter.Allows for the same reason the connect-time
// resume is (open): the visible set is the security boundary EVT-120 draws,
// while the selector and schemas restrictions are the client's own narrowing of
// what it is sent, never of what it is entitled to be told about (EVT-121/124).
// Resolving to_id over the WHOLE tail instead would name the oldest retained id
// above the watermark whoever is asking, which on a shared log is routinely an
// event this subscriber may not read — a marker reporting that the event exists
// and, a ULID being time-ordered, roughly when. That is the probe EVT-122
// forbids against scope nodes and EVT-134a forbids against ids.
//
// Where the tail holds nothing visible, the marker stays pending and the
// watermark advances anyway (EVT-142a): holding the watermark would re-read the
// whole retained tail on every subsequent wake, under the lock every Append
// takes, for a subscriber that is by definition receiving nothing.
//
// EVT-123's boundary is applied per event here, at delivery time — never
// delegated to whatever the subscriber's own selector claimed.
func (s *server) drainOnce(sk sink, st *stream, filter events.Filter) error {
	evicted, tail := s.hub.drain(st.considered)
	if evicted {
		st.markLoss()
	}
	for _, env := range tail {
		st.considered = env.ID
		if st.pending && filter.Visible(env) {
			if err := sk.gap(st.resolveLoss(env.ID)); err != nil {
				return err
			}
		}
		if !filter.Allows(env) {
			continue
		}
		if err := sk.event(env); err != nil {
			return err
		}
		st.delivered = env.ID
	}
	st.arm()
	return sk.flush()
}

// ServeHTTP serves both events/1 bindings at the one path EVT-001 fixes,
// selecting between them from the request's own shape: a WS `Upgrade` header
// selects the WS binding, an `Accept: text/event-stream` header with no Upgrade
// selects SSE. Authentication (EVT-110–113) and the revocation watch (EVT-114)
// are resolved once, ahead of that selection, so neither binding can be reached
// without them.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)

	// EVT-113: authentication is resolved BEFORE the binding is selected and
	// before any header is written, "never with a WS/SSE-level frame, since no
	// session has been established yet to frame one over". A refusal here is an
	// ordinary api/1 Problem with events/1's AUTH_REQUIRED code — and for the WS
	// binding it lands before the upgrade, so there is no socket to frame over
	// either.
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
		// GET is the only method either binding uses: an SSE subscribe is a GET,
		// and so is a WS upgrade.
		apihttp.WriteProblemExt(w, r, traceID, http.StatusMethodNotAllowed, codeRequestInvalid, "Method Not Allowed",
			"events/1 is a watch channel: both bindings connect with GET.", nil)
		return
	}

	switch events.SelectBinding(r.Header.Get("Upgrade") != "", r.Header.Get("Accept")) {
	case events.BindingSSE:
		s.serveSSE(w, r, traceID, principal, revoked)
	case events.BindingWS:
		s.serveWS(w, r, traceID, principal, revoked)
	default:
		apihttp.WriteProblemExt(w, r, traceID, http.StatusNotAcceptable, codeRequestInvalid, "Not Acceptable",
			"events/1 selects its binding from the request's own shape: send an Upgrade header for the WebSocket binding, "+
				"or Accept: text/event-stream for the SSE binding.", nil)
	}
}

// codeRequestInvalid is the code every "this request does not select an events/1
// binding" refusal carries: a method other than GET, a shape that is neither a
// WS upgrade nor an SSE subscribe, a WS upgrade offering no events/1 subprotocol.
//
// events/1's own Error taxonomy publishes no code for a malformed REQUEST — its
// registry covers authentication, the two client-supplied fields that can be
// wrong (selector, resume_from), the two disconnect classes, webhook delivery,
// and the two generic server-side entries. So this one is drawn by NAME from
// api/1's registry, which does publish it — the same reuse-by-name discipline
// player/1's PLY-007 defines, api/1's own API-013 applies, and this binding
// already relies on for FORBIDDEN (auth.EventsCodes).
//
// What matters is that it comes from a PUBLISHED registry at all. A code minted
// here and published nowhere — which is what this surface used to answer a WS
// upgrade with — is a value no client's error handling can be written against,
// and it is exactly what API-011's "a server MUST NOT emit a code value outside
// the registry" forbids.
const codeRequestInvalid = "VALIDATION_FAILED"

// serveSSE handles an SSE subscribe (EVT-100–105, 130–144): it resolves the
// resume point (Last-Event-ID header over resume_from query, EVT-102) via the
// Hub, streams the resolved backlog (a leading event: gap for a
// retention_expired resume, EVT-104/140), then blocks streaming each
// newly-appended event live until the client disconnects (request context) or
// the server shuts down (Hub.Close) — a resume_from that is malformed, or that
// this subscriber cannot resolve (never recorded, or recorded outside its
// visible set — one indistinguishable refusal for both), is a
// RESUME_FROM_INVALID Problem written before any event (EVT-134/134a).
func (s *server) serveSSE(w http.ResponseWriter, r *http.Request, traceID string, principal auth.Principal, revoked <-chan struct{}) {
	// The SSE stream needs an incrementally flushable writer; without one there is
	// no live push, so refuse rather than buffer the whole (unbounded) stream.
	// This is a fault in the server the handler was mounted in, not anything the
	// client did — the taxonomy's unclassified server-side entry.
	flusher, ok := w.(http.Flusher)
	if !ok {
		apihttp.WriteProblemExt(w, r, traceID, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"This server cannot stream incrementally, so it cannot serve the SSE binding.", nil)
		return
	}

	// EVT-101: `selector` and `schemas` arrive as query parameters on the initial
	// request, since SSE offers no later client-to-server frame to carry them.
	// EVT-102: a Last-Event-ID header (a browser-native reconnect) takes
	// precedence over a resume_from query parameter.
	query := r.URL.Query()
	resumeFrom := r.Header.Get("Last-Event-ID")
	if resumeFrom == "" {
		resumeFrom = query.Get("resume_from")
	}

	sub, outcome, filter, oerr := s.open(r.Context(), principal, subscribeRequest{
		selector:   query.Get("selector"),
		schemas:    query["schemas"],
		resumeFrom: resumeFrom,
	})
	if oerr != nil {
		apihttp.WriteProblemExt(w, r, traceID, oerr.status, oerr.code, oerr.title, oerr.detail, nil)
		return
	}
	defer sub.close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// st tracks the highest id already considered for this subscriber, so the
	// live loop's After(considered) yields exactly the not-yet-seen tail
	// (gap-free, duplicate-free, EVT-133/143), alongside the last id actually
	// delivered and any deferred loss marker.
	sk := &sseSink{w: w, flusher: flusher, logf: s.logf}
	st := newStream(resumeFrom, s.gapDeferral)
	defer st.stop()
	_ = s.deliverBacklog(sk, sub, outcome, st, filter)

	// EVT-105a: an SSE stream that has sent nothing for the keepalive interval
	// emits a comment line. It is a Timer rather than a Ticker so "idle" means
	// idle SINCE THE LAST FRAME, exactly as the WS binding's ping does — and
	// without it an idle stream, a stream holding a deferred marker, and a
	// connection that died in a middlebox are all the same zero bytes.
	idle := time.NewTimer(s.pingInterval)
	defer idle.Stop()

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
		case <-st.expiry():
			// EVT-142a: a mid-stream loss marker this subscriber's visible set
			// never gave a resume point for. Ending the stream is the only
			// remaining honest answer — the alternatives are naming an id it
			// may not read, or leaving it connected to a stream that has
			// already lost something and will never say so. SSE has no close
			// frame to name a code in, so the stream simply ends and the
			// client reconnects; its own cursor then draws whichever answer
			// the connect-time resolution gives it (EVT-133/141/134).
			s.logf("eventsse: ending an SSE stream whose buffer_exceeded marker found no visible resume point within %s (EVT-142a)", s.gapDeferral)
			return
		case <-sub.wake():
			_ = s.drainOnce(sk, st, filter)
			idle.Reset(s.pingInterval)
		case <-idle.C:
			sk.keepalive()
			idle.Reset(s.pingInterval)
		}
	}
}

// sseSink is the SSE binding's frame writer (EVT-103/104).
//
// Its writes are best-effort: a mid-stream write failure is a dropped subscriber
// connection, which the request-context select already observes and closes on,
// so there is nothing to recover and nothing to classify — every method returns
// nil. An envelope that fails to serialize is logged and SKIPPED rather than
// emitted as a corrupt line (SSEEventLine's own contract), so one bad record
// cannot poison the stream.
type sseSink struct {
	w       http.ResponseWriter
	flusher http.Flusher
	logf    func(format string, args ...any)
}

func (s *sseSink) event(env events.Envelope) error {
	line, err := events.SSEEventLine(env)
	if err != nil {
		s.logf("eventsse: dropping unserializable event id %q: %v (EVT-103)", env.ID, err)
		return nil
	}
	_, _ = s.w.Write([]byte(line))
	return nil
}

func (s *sseSink) gap(g events.GapFrame) error {
	_, _ = s.w.Write([]byte(events.SSEGapLine(g)))
	return nil
}

func (s *sseSink) flush() error {
	s.flusher.Flush()
	return nil
}

// keepalive writes an SSE comment line on an otherwise idle stream (EVT-105a).
//
// A line beginning ":" is a comment the EventSource specification requires a
// client to ignore, so this is invisible to the subscriber's event handling and
// still real bytes on the wire — which is the whole point: it is what makes a
// stream that is merely quiet distinguishable from one that has stalled, been
// dropped by a middlebox, or is holding a loss marker it cannot yet name
// (EVT-142a). It is not part of the shared sink interface: the WS binding has
// EVT-095's ping/pong for the same job, on a transport that can answer.
func (s *sseSink) keepalive() {
	_, _ = s.w.Write([]byte(": keepalive\n\n"))
	s.flusher.Flush()
}
