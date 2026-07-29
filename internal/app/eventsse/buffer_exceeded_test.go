package eventsse

// buffer_exceeded_test.go drives the MID-STREAM loss marker (EVT-140/142/142a/
// 143) through real subscribers on both bindings: a real HTTP/SSE connection and
// a real WebSocket, a real scope-node tree, two really-differently-bound
// principals, and a real bounded events.EventLog that really evicts.
//
// # Why it is driven here and not through Hub.drain
//
// Every field of a mid-stream marker is a property of the CONNECTION, not of the
// log: from_id is the last id this subscriber was actually sent (EVT-140), and
// to_id is bounded by what this subscriber may read (EVT-134a). A test that
// calls the drain helper supplies both of those itself, so it can only ever
// confirm that the helper does what the test already assumed — which is exactly
// how an earlier attempt at this behavior shipped with a from_id naming another
// principal's event and a stream that could go permanently silent, both
// invisible to its own tests.
//
// # How "the subscriber had not drained yet" is made deterministic
//
// A mid-stream drop is by definition a race the subscriber loses: events are
// appended and aged out between two of its wakes. Rather than try to win that
// race by timing, the burst is appended through appendQuietly, which takes the
// Hub's own lock (so it is not a data race) and does NOT signal the fan-out.
// The subscriber is parked in its select for the whole burst, and one ordinary
// Hub.Append afterwards produces exactly one wake and exactly one drain. That is
// the same observable a production burst produces — a wake is a buffered(1)
// coalescing signal, so N appends a subscriber is not scheduled during leave one
// pending wake and one drain over the whole tail.

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/events"
)

// countingLog is a real bounded events.EventLog that records how much of itself
// each tail read copies out. It is the oracle for the cost of a deferred marker:
// a drain whose watermark does not advance re-reads the whole retained tail on
// every wake, under the lock every Append takes.
type countingLog struct {
	*events.EventLog
	afterCalls     atomic.Int64
	afterEnvelopes atomic.Int64
}

func (l *countingLog) After(id string) []events.Envelope {
	out := l.EventLog.After(id)
	l.afterCalls.Add(1)
	l.afterEnvelopes.Add(int64(len(out)))
	return out
}

// midEnv is one mid-stream-loss test's world: a real auth store with a principal
// bound at site A, a real scope tree, a bounded counting log, and the live
// handler over both bindings.
type midEnv struct {
	hub  *Hub
	srv  *httptest.Server
	log  *countingLog
	cred authtest.Credential
	next func() string
}

// newMidEnv builds that world. retention bounds the log; opts tune the handler's
// own time bounds (the EVT-142a deferral, the EVT-095/105a keepalive) so a test
// drives the behavior on an injected cadence rather than waiting out production's.
func newMidEnv(t *testing.T, retention int, opts ...Option) *midEnv {
	t.Helper()
	// viewer at site A — the weakest role that reads at all, at ONE site, which
	// is what every non-owner principal holds. Nothing here passes by virtue of
	// an over-broad binding.
	fixture, err := authtest.New(authtest.Config{Role: auth.RoleViewer, ScopeNode: siteANode})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	t.Cleanup(fixture.Close)

	cl := &countingLog{EventLog: events.NewEventLog(retention)}
	hub := NewHub(cl)
	nodes := scopeFixtureNodes()
	srv := httptest.NewServer(New(hub, fixture.Auth, func(context.Context) ([]datamodel.ScopeNode, error) {
		return nodes, nil
	}, opts...))
	t.Cleanup(srv.Close)

	return &midEnv{hub: hub, srv: srv, log: cl, cred: fixture.Credential(), next: ulidSeq()}
}

// put appends one event at scope through the ordinary write path, waking every
// subscriber.
func (e *midEnv) put(scope string) string {
	id := e.next()
	e.hub.Append(scopedEnv(id, scope))
	return id
}

// appendQuietly records one event WITHOUT signalling the fan-out — the burst a
// subscriber is not scheduled during. It takes the Hub's own lock, so it is
// exactly as race-free as Append; the only thing it skips is the wake.
func (e *midEnv) appendQuietly(scope string) string {
	id := e.next()
	e.hub.mu.Lock()
	e.hub.log.Append(scopedEnv(id, scope))
	e.hub.mu.Unlock()
	return id
}

// waitDrains blocks until the live loop has run n tail reads. Every After on
// this log is one drain (the connect-time resume resolves through From, and the
// head watermark through HeadID), so the counter IS the drain count — a real
// signal from the code under test, not a sleep.
func (e *midEnv) waitDrains(t *testing.T, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.log.afterCalls.Load() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the live loop never reached drain #%d (got %d)", n, e.log.afterCalls.Load())
}

// dial opens an authenticated live SSE stream.
func (e *midEnv) dial(t *testing.T, query string) (*bufio.Reader, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	target := e.srv.URL + "/events/v1"
	if query != "" {
		target += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	e.cred.Authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("dialing SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		resp.Body.Close()
		t.Fatalf("the stream must open 200 (EVT-100); got %d", resp.StatusCode)
	}
	return bufio.NewReader(resp.Body), func() { cancel(); resp.Body.Close() }
}

// midFrame is one thing a subscriber can observe on an SSE stream: a frame, a
// keepalive comment (EVT-105a), the stream ending, or nothing at all. The last
// two are DIFFERENT observations and this file turns on the difference — a
// stream that ends has told its subscriber something, and a stream that says
// nothing has not.
type midFrame struct {
	sseFrame
	comment string
	eof     bool
	silent  bool
}

// observe reads whatever the stream does next within d.
func observe(br *bufio.Reader, d time.Duration) midFrame {
	type result struct{ f midFrame }
	ch := make(chan result, 1)
	go func() {
		var f midFrame
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				f.eof = true
				ch <- result{f}
				return
			}
			if line == "\n" {
				ch <- result{f}
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, ":"):
				// A comment (EVT-105a). It is still terminated by the blank
				// line, so keep reading rather than leaving that line in the
				// buffer for the next observation to trip over.
				f.comment = line
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	select {
	case r := <-ch:
		return r.f
	case <-time.After(d):
		return midFrame{silent: true}
	}
}

// gapMarker is the {from_id,to_id,reason} payload an SSE gap frame carries
// (EVT-104/140).
type gapMarker struct {
	FromID *string `json:"from_id"`
	ToID   string  `json:"to_id"`
	Reason string  `json:"reason"`
}

func parseGap(t *testing.T, f midFrame) gapMarker {
	t.Helper()
	if f.event != "gap" {
		t.Fatalf("expected a gap frame (EVT-104); got %+v", f)
	}
	var m gapMarker
	if err := json.Unmarshal([]byte(f.data), &m); err != nil {
		t.Fatalf("a gap's data: must be the {from_id,to_id,reason} marker; got %q (%v)", f.data, err)
	}
	return m
}

func fromID(m gapMarker) string {
	if m.FromID == nil {
		return "<null>"
	}
	return *m.FromID
}

// --- the defect: a marker naming an id its own recipient may not read ---------

// TestSSE_MidStreamGapNamesOnlyIDsItsSubscriberMayRead is the regression for a
// buffer_exceeded marker whose BOTH ENDS were resolved without reference to the
// subscriber the marker was being handed to.
//
// to_id was the oldest retained id above the subscriber's watermark whoever was
// asking, which on a shared log is routinely an event outside its visible set;
// from_id was the watermark itself, which advances over every event the
// connection CONSIDERED including the ones it was forbidden to send. Either one
// reports that an event exists and — a ULID being time-ordered — roughly when it
// was recorded, which is the probe EVT-122 forbids against scope nodes and
// EVT-134a forbids against event ids. Delivery already honoured the boundary;
// only the marker did not.
//
// The oracle is two-sided, as every scope assertion in this package is: bob's
// ids are provably absent from alice's marker, while alice's own delivered event
// proves the stream was genuinely working.
func TestSSE_MidStreamGapNamesOnlyIDsItsSubscriberMayRead(t *testing.T) {
	e := newMidEnv(t, 4)

	a0 := e.put(siteANode) // alice's resume anchor
	a1 := e.put(siteANode) // delivered in the backlog: her real last-known point

	br, done := e.dial(t, "resume_from="+a0)
	defer done()
	if f := observe(br, 3*time.Second); f.id != a1 {
		t.Fatalf("the backlog must deliver %s; got %+v", a1, f)
	}

	// One wake that CONSIDERS an event alice may not read and delivers nothing:
	// this is what moves the watermark off her last-delivered id.
	b0 := e.put(siteBNode)
	e.waitDrains(t, 1)

	// The loss. a2 is alice's own event, appended and aged out before she could
	// be woken for it; everything else retained above her watermark is bob's,
	// until a3 — the first id she may actually read, which is where her delivery
	// resumes.
	a2 := e.appendQuietly(siteANode)
	for i := 0; i < 5; i++ {
		e.appendQuietly(siteBNode)
	}
	a3 := e.put(siteANode)

	f := observe(br, 3*time.Second)
	if f.silent || f.eof {
		t.Fatalf("a visible event (%s) was evicted undelivered: the subscriber MUST be told (EVT-143); got %+v", a2, f)
	}
	m := parseGap(t, f)

	if m.Reason != events.ReasonBufferExceeded {
		t.Errorf("a mid-stream drop is buffer_exceeded (EVT-142); got %q", m.Reason)
	}
	if fromID(m) == b0 {
		t.Errorf("from_id = %s — an event placed outside alice's visible set. The marker's own from_id "+
			"discloses that an event she may not read exists, and when (EVT-134a)", b0)
	}
	if fromID(m) != a1 {
		t.Errorf("from_id = %s, want %s — EVT-140 defines it as the last id SUCCESSFULLY DELIVERED, "+
			"which is not the watermark: the watermark advances over suppressed events too", fromID(m), a1)
	}
	if m.ToID != a3 {
		t.Errorf("to_id = %s, want %s — EVT-134a requires the id delivery resumes at to be inside the "+
			"subscriber's own visible set, and a3 is the first retained id alice may read", m.ToID, a3)
	}
	for _, id := range e.outOfScopeIDs() {
		if m.ToID == id || fromID(m) == id {
			t.Errorf("the marker names %s, which is placed in bob's subtree (EVT-120/134a)", id)
		}
	}

	// EVT-143/094: the gap precedes the event it resumes at — a marker that
	// landed after the resumed event would not cover the discontinuity.
	if f := observe(br, 3*time.Second); f.event != "event" || f.id != a3 {
		t.Fatalf("delivery must resume AT to_id immediately after the gap; got %+v", f)
	}
}

// outOfScopeIDs is every retained id placed in bob's subtree — the ids alice's
// marker must never name.
func (e *midEnv) outOfScopeIDs() []string {
	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	var out []string
	for _, env := range e.hub.log.After("") {
		if env.ScopeNode == siteBNode || env.ScopeNode == screenBNode {
			out = append(out, env.ID)
		}
	}
	return out
}

// --- regression 1: a deferred marker must not become permanent silence --------

// TestSSE_DeferredGapIsBoundedNotHeldForever is the regression for "hold the
// marker until a visible resume point turns up".
//
// Holding it does not distinguish "nothing visible was lost" from "the visible
// events that were lost have already been evicted" — the log cannot answer that,
// because the evidence is exactly what was dropped. In the second case a held
// marker is silent loss with the subscriber still connected: EVT-143 says a
// server "MUST NOT drop any eligible event from a subscriber's stream without a
// corresponding gap covering it", and audit records are events (SEC-150), so a
// stream that quietly stops covering them is a way to lose an audit trail with
// nobody told. Worse, an SSE stream sends no bytes when it is merely idle, so a
// held stream is BYTE-IDENTICAL on the wire to a healthy one.
//
// EVT-142a therefore bounds the deferral: a marker with no visible resume point
// at the bound ends the connection instead of riding along forever.
func TestSSE_DeferredGapIsBoundedNotHeldForever(t *testing.T) {
	e := newMidEnv(t, 4, WithGapDeferral(150*time.Millisecond))

	a0 := e.put(siteANode)
	a1 := e.put(siteANode)

	br, done := e.dial(t, "resume_from="+a0)
	defer done()
	if f := observe(br, 3*time.Second); f.id != a1 {
		t.Fatalf("the backlog must deliver %s; got %+v", a1, f)
	}

	// alice's own event is evicted undelivered, and NOTHING she may read is
	// retained above her point — so no id exists that her marker could name.
	lost := e.appendQuietly(siteANode)
	for i := 0; i < 5; i++ {
		e.appendQuietly(siteBNode)
	}
	e.put(siteBNode)

	f := observe(br, 3*time.Second)
	if f.silent {
		t.Fatalf("the stream said NOTHING after %s was evicted undelivered. A held marker with no keepalive "+
			"is indistinguishable from a healthy idle stream, which makes it silent loss (EVT-142a/143)", lost)
	}
	if !f.eof {
		t.Fatalf("with no visible resume point the deferral must end at its bound (EVT-142a); got %+v", f)
	}
}

// TestSSE_DeferredGapIsEmittedAheadOfTheFirstVisibleEvent is the other half of
// the same rule: a deferral that DOES find a resume point inside the bound
// resolves normally, and the marker still lands ahead of the first event that
// crosses the discontinuity. Deferring is not dropping.
func TestSSE_DeferredGapIsEmittedAheadOfTheFirstVisibleEvent(t *testing.T) {
	e := newMidEnv(t, 4, WithGapDeferral(30*time.Second))

	a0 := e.put(siteANode)
	a1 := e.put(siteANode)

	br, done := e.dial(t, "resume_from="+a0)
	defer done()
	if f := observe(br, 3*time.Second); f.id != a1 {
		t.Fatalf("the backlog must deliver %s; got %+v", a1, f)
	}

	// Loss with nothing visible retained: the marker has nowhere to point yet.
	e.appendQuietly(siteANode)
	for i := 0; i < 5; i++ {
		e.appendQuietly(siteBNode)
	}
	e.put(siteBNode)
	e.waitDrains(t, 1)

	// Two more wakes carrying only bob's events. The marker is still pending and
	// the subscriber is still connected — and, critically, its watermark is NOT
	// pinned: those events are consumed, not re-read forever.
	e.put(siteBNode)
	e.waitDrains(t, 2)
	e.put(siteBNode)
	e.waitDrains(t, 3)

	// Now one alice may read. The held marker is emitted AHEAD of it.
	a4 := e.put(siteANode)

	m := parseGap(t, observe(br, 3*time.Second))
	if fromID(m) != a1 {
		t.Errorf("from_id = %s, want the last id delivered to alice (%s) — deferral must not move it", fromID(m), a1)
	}
	if m.ToID != a4 {
		t.Errorf("to_id = %s, want %s (the first id alice may read after the discontinuity)", m.ToID, a4)
	}
	if f := observe(br, 3*time.Second); f.event != "event" || f.id != a4 {
		t.Fatalf("the resumed event must follow its own marker; got %+v", f)
	}

	// A resolved marker is gone: a later wake carrying nothing visible must not
	// resurrect it, and the connection must not be ended for it.
	e.put(siteBNode)
	e.waitDrains(t, 5)
	a5 := e.put(siteANode)
	if f := observe(br, 3*time.Second); f.event != "event" || f.id != a5 {
		t.Fatalf("once resolved, the marker must not be re-emitted; got %+v", f)
	}
}

// --- regression 2: what a deferred marker costs the whole event plane ---------

// TestSSE_DeferredGapDoesNotRescanTheRetainedTail is the regression for pinning
// the watermark while a marker is pending.
//
// A pinned watermark makes every subsequent wake re-read the ENTIRE retained
// tail — a full envelope copy per retained entry — inside Hub.drain, which holds
// the same mutex every Append in the process takes. The subscriber that triggers
// it needs no special access: any principal bound to a scope node where nothing
// publishes is an ordinary binding, and a lagging one of those turns each append
// anywhere on the platform into a scan of the whole retention window. That is an
// availability amplifier against the event plane, introduced by the fix rather
// than found in the defect.
//
// The bound below is a shape assertion, not a benchmark: O(new events per wake)
// versus O(retained tail per wake) differ by the retention window, which is
// hundreds here and far more in production.
func TestSSE_DeferredGapDoesNotRescanTheRetainedTail(t *testing.T) {
	const retention = 500
	const wakes = 100
	e := newMidEnv(t, retention, WithGapDeferral(30*time.Second))

	a0 := e.put(siteANode)
	a1 := e.put(siteANode)

	br, done := e.dial(t, "resume_from="+a0)
	defer done()
	if f := observe(br, 3*time.Second); f.id != a1 {
		t.Fatalf("the backlog must deliver %s; got %+v", a1, f)
	}

	// Lag alice past the horizon with a full window of bob's events, so her
	// marker is pending with no visible resume point — the state a pin holds.
	e.appendQuietly(siteANode)
	for i := 0; i < retention+8; i++ {
		e.appendQuietly(siteBNode)
	}
	e.put(siteBNode)
	e.waitDrains(t, 1)

	// Steady state: one append per wake, none of it in alice's scope.
	envBefore := e.log.afterEnvelopes.Load()
	callsBefore := e.log.afterCalls.Load()
	for i := 0; i < wakes; i++ {
		e.put(siteBNode)
		e.waitDrains(t, callsBefore+int64(i)+1)
	}
	copied := e.log.afterEnvelopes.Load() - envBefore
	calls := e.log.afterCalls.Load() - callsBefore
	perWake := float64(copied) / float64(calls)
	t.Logf("MEASURED: %d wake(s) over a %d-entry retention window copied %d envelope(s) — %.1f per wake",
		calls, retention, copied, perWake)

	// One new event per wake, plus slack for a wake that coalesces two.
	if copied > 4*wakes {
		t.Errorf("a pending marker made each wake re-read the retained tail: %d envelope copies over %d wake(s) "+
			"(%.1f each) on a %d-entry window. The watermark MUST advance over the consumed tail while the "+
			"marker is deferred (EVT-142a)", copied, calls, perWake, retention)
	}
}

// --- EVT-140's null from_id ---------------------------------------------------

// TestSSE_GapFromIDIsNullWhenTheSubscriberHasNoKnownPoint pins EVT-140's "null
// only when no such point exists" on the WIRE. The reachable case is a fresh
// subscribe to an empty log that then lags past the horizon: nothing has been
// delivered and no resume_from was supplied, so there is no last-known point at
// all. Emitting "" there is neither an id nor null, and a client testing
// `from_id === null` to decide whether it has a prior position to reconcile
// against reads the empty string as a real one.
func TestSSE_GapFromIDIsNullWhenTheSubscriberHasNoKnownPoint(t *testing.T) {
	e := newMidEnv(t, 3, WithGapDeferral(30*time.Second))

	br, done := e.dial(t, "") // fresh subscribe, empty log: no known point at all
	defer done()

	for i := 0; i < 5; i++ {
		e.appendQuietly(siteANode)
	}
	e.put(siteANode)

	f := observe(br, 3*time.Second)
	m := parseGap(t, f)
	if m.FromID != nil {
		t.Errorf("from_id = %q, want null: this subscriber delivered nothing and resumed from nothing (EVT-140)", *m.FromID)
	}
	if !strings.Contains(f.data, `"from_id":null`) {
		t.Errorf("the marker must carry from_id as JSON null, never \"\" and never omitted (EVT-140); got %s", f.data)
	}
}

// --- EVT-105a: a quiet stream is not a stalled stream -------------------------

// TestSSE_KeepaliveDistinguishesAQuietStreamFromAStalledOne drives EVT-105a. WS
// has ping/pong (EVT-095) and a client can probe with it; SSE has no
// client-to-server frame at all (EVT-100), so without a server-sent keepalive an
// idle stream, a stream holding a deferred marker, and a connection a middlebox
// dropped an hour ago are the same zero bytes. The comment line is what makes
// them different — and an SSE comment is a line the EventSource specification
// requires a client to ignore, so it changes nothing a subscriber processes.
func TestSSE_KeepaliveDistinguishesAQuietStreamFromAStalledOne(t *testing.T) {
	e := newMidEnv(t, 0, WithHeartbeat(60*time.Millisecond, 20*time.Millisecond))

	br, done := e.dial(t, "")
	defer done()

	f := observe(br, 3*time.Second)
	if f.silent {
		t.Fatal("an idle SSE stream must emit a keepalive comment, or a live stream cannot be told from a dead one (EVT-105a)")
	}
	if !strings.HasPrefix(f.comment, ":") {
		t.Fatalf("the keepalive must be an SSE comment line (ignored by every conforming client); got %+v", f)
	}

	// It repeats, and it does not disturb delivery.
	if f := observe(br, 3*time.Second); !strings.HasPrefix(f.comment, ":") {
		t.Fatalf("the keepalive must repeat while the stream stays idle; got %+v", f)
	}
	id := e.put(siteANode)
	for {
		f := observe(br, 3*time.Second)
		if f.comment != "" {
			continue // a keepalive that raced the append
		}
		if f.event != "event" || f.id != id {
			t.Fatalf("keepalives must not disturb live delivery; got %+v", f)
		}
		return
	}
}

// --- the WS binding: the same marker, the same bound --------------------------

// midWS opens a WS subscriber against e, resuming from anchor.
func (e *midEnv) midWS(t *testing.T, anchor string) (*wsSubscriber, string) {
	t.Helper()
	return openWS(t, e.srv, e.cred, events.HelloFrame{ResumeFrom: anchor})
}

// TestWS_MidStreamGapNamesOnlyIDsItsSubscriberMayRead is the WS half of the
// marker rule (EVT-094). The two bindings share `open`, the filter, and
// `drainOnce`, so this is a structural property rather than a duplicated one —
// but a shared property that is only ever driven on one binding is a shared
// property nobody has checked.
func TestWS_MidStreamGapNamesOnlyIDsItsSubscriberMayRead(t *testing.T) {
	e := newMidEnv(t, 4, WithGapDeferral(30*time.Second))

	a0 := e.put(siteANode)
	a1 := e.put(siteANode)

	c, result := e.midWS(t, a0)
	if result != events.ResumeResultResumed {
		t.Fatalf("resume_result = %q; want resumed (EVT-133)", result)
	}
	if f := c.next(t, 2*time.Second); f.Type != events.FrameTypeEvent || f.Event.ID != a1 {
		t.Fatalf("the backlog must deliver %s; got %+v", a1, f)
	}

	b0 := e.put(siteBNode)
	e.waitDrains(t, 1)

	e.appendQuietly(siteANode)
	for i := 0; i < 5; i++ {
		e.appendQuietly(siteBNode)
	}
	a3 := e.put(siteANode)

	f := c.next(t, 3*time.Second)
	if f.Type != events.FrameTypeGap {
		t.Fatalf("expected a gap frame (EVT-094); got %+v", f)
	}
	got := "<null>"
	if f.FromID != nil {
		got = *f.FromID
	}
	if got == b0 {
		t.Errorf("from_id = %s, an event outside alice's visible set (EVT-134a)", b0)
	}
	if got != a1 {
		t.Errorf("from_id = %s, want the last id delivered (%s) (EVT-140)", got, a1)
	}
	if f.ToID != a3 {
		t.Errorf("to_id = %s, want %s (EVT-134a)", f.ToID, a3)
	}
	if f := c.next(t, 3*time.Second); f.Type != events.FrameTypeEvent || f.Event.ID != a3 {
		t.Fatalf("delivery must resume AT to_id right after the marker; got %+v", f)
	}
}

// TestWS_DeferredGapClosesSlowConsumerAtTheBound is EVT-142a's disposition on
// the binding that HAS a close vocabulary. The condition is EVT-142's own — this
// subscriber fell behind live delivery and undelivered events were dropped — so
// the close names the code EVT-142 already assigns that condition, and a client's
// reconnect logic needs no new case (EVT-096).
func TestWS_DeferredGapClosesSlowConsumerAtTheBound(t *testing.T) {
	e := newMidEnv(t, 4, WithGapDeferral(150*time.Millisecond))

	a0 := e.put(siteANode)
	a1 := e.put(siteANode)

	c, _ := e.midWS(t, a0)
	if f := c.next(t, 2*time.Second); f.Event.ID != a1 {
		t.Fatalf("the backlog must deliver %s; got %+v", a1, f)
	}

	e.appendQuietly(siteANode) // lost, undelivered
	for i := 0; i < 5; i++ {
		e.appendQuietly(siteBNode)
	}
	e.put(siteBNode)

	c.expectClose(t, events.CloseSlowConsumer, 3*time.Second)
}
