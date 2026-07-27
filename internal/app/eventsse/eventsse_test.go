package eventsse

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/events"
)

// siteScope is the frozen corpus site scope-node ULID stamped onto the test
// envelopes' scope_node (matches the eventingest fixtures so the two packages
// exercise the same shared-log envelope shape).
const siteScope = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"

// idPrefix is 24 Crockford base32 characters (leading 0) — a 2-symbol counter
// suffix makes a full 26-char valid ULID that sorts in suffix order, so
// idA < idB < idC < idD is also chronological id order (EVT-011/140).
const idPrefix = "01J8Z3K4N5P6Q7R8S9T0V1W2"

var (
	idA = idPrefix + "Y5"
	idB = idPrefix + "Y6"
	idC = idPrefix + "Y7"
	idD = idPrefix + "Y8"
)

// ulidSeq mints deterministic, ascending, valid 26-char ULIDs (the idPrefix plus
// a 2-symbol Crockford-base32 counter suffix), so successive ids sort in call
// order — the shape the ingest assigns (EVT-011) and the same generator the
// eventingest tests use, so a reconstructed envelope carrying one validates.
func ulidSeq() func() string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	n := 0
	return func() string {
		hi := alphabet[(n/32)%32]
		lo := alphabet[n%32]
		n++
		return idPrefix + string([]byte{hi, lo})
	}
}

// corpusAutomationRunPayload is the frozen corpus automation.run payload
// (conformance/corpora/events-1) — a real, serializable event body so a streamed
// SSEEventLine data: field carries an intact automation.run (EVT-040/103).
func corpusAutomationRunPayload() json.RawMessage {
	return json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2YC","rule_revision":4,"trigger_snapshot":{"kind":"state"},"condition_results":[{"passed":true}],"action_outcomes":[{"status":"ok"}],"mode_disposition":"ran","misfire_caught":false}`)
}

// autoEnv builds a valid automation.run durable-event envelope with id — the
// shape the ingest appends and the SSE server streams (EVT-010/103). The
// cost/retention class values are cosmetic here: SSEEventLine only serializes the
// envelope, it does not re-validate it (validation is the ingest's job, EVT-013).
func autoEnv(id string) events.Envelope {
	return events.Envelope{
		ID:             id,
		Schema:         events.SchemaAutomationRun,
		TS:             1752537000000,
		ScopeNode:      siteScope,
		TraceID:        idPrefix + "ZZ",
		CostClass:      "standard",
		RetentionClass: "operational",
		Origin:         "relay",
		Payload:        corpusAutomationRunPayload(),
	}
}

// everythingVisible is the visible-set predicate (EVT-120) the white-box Hub
// tests below pass to subscribe. They are about registration/watermark
// mechanics rather than about visibility, and their fixture principal reads the
// whole log — the scope-node boundary itself is driven end to end, through the
// real handler and a real scope tree, in scope_filter_test.go.
func everythingVisible(events.Envelope) bool { return true }

// dialSSE opens a streaming GET /events/v1 against srv with the given query and
// headers, asserts a 200 text/event-stream response, and returns a buffered
// reader over the live body plus a cancel that tears the connection down (firing
// the handler's request-context close path). The caller MUST call cancel.
// testAuth is the one seeded auth fixture every SSE test in this package
// connects as. The binding authenticates every connection before any stream
// begins (EVT-110/111/113), so there is no unauthenticated path left to drive;
// a test exercising the STREAM semantics presents a real credential first. One
// shared fixture suffices — no test here asserts which principal is connected,
// only how the stream behaves. The authentication behavior itself is driven by
// auth_binding_test.go.
var testAuth = sync.OnceValue(func() *authtest.Fixture {
	f, err := authtest.New(authtest.Config{})
	if err != nil {
		panic("eventsse: seed auth fixture: " + err.Error())
	}
	return f
})

// newTestServer mounts the live SSE handler over hub, authenticated as the
// shared fixture, over an EMPTY scope-node tree. The shared fixture is bound at
// the workspace root, and auth.Resolve's documented root fallback makes a
// root-bound principal read every placement — including one whose node the tree
// does not carry — so these stream-semantics tests see an unfiltered stream
// without any test-only bypass. Scope filtering itself is driven, with a real
// tree and two differently-bound principals, by scope_filter_test.go.
func newTestServer(hub *Hub) http.Handler {
	return New(hub, testAuth().Auth, emptyScopeTree)
}

// emptyScopeTree is the ScopeNodesFunc for a harness that authors no scope-node
// tree. See newTestServer: it is not a filtering bypass — the root-bound fixture
// principal still resolves through auth.CanRead, which answers "readable" for
// every placement precisely because the binding is at the workspace root.
func emptyScopeTree(context.Context) ([]datamodel.ScopeNode, error) { return nil, nil }

func dialSSE(t *testing.T, srv *httptest.Server, query string, header http.Header) (*bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	url := srv.URL + "/events/v1"
	if query != "" {
		url += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	// EVT-111: a browser's native EventSource cannot set custom headers, so the
	// SSE binding authenticates by the session cookie — which is what this dial
	// presents.
	testAuth().Authorize(req)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("dialing SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("SSE request must respond 200 (EVT-100); got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		cancel()
		t.Fatalf("SSE Content-Type must be text/event-stream (EVT-100); got %q", ct)
	}
	br := bufio.NewReader(resp.Body)
	return br, func() { cancel(); resp.Body.Close() }
}

type sseFrame struct {
	event string
	id    string
	data  string
}

// readFrameWithin reads exactly one SSE frame (lines up to the blank-line
// terminator) from br, failing the test if none arrives within d — so a missing
// live push or a wrongly-suppressed backlog is a hard failure, never a hang.
func readFrameWithin(t *testing.T, br *bufio.Reader, d time.Duration) sseFrame {
	t.Helper()
	type result struct {
		f   sseFrame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var f sseFrame
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				ch <- result{f, err}
				return
			}
			if line == "\n" { // blank line terminates the frame (EVT-103)
				ch <- result{f, nil}
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
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
		if r.err != nil {
			t.Fatalf("reading SSE frame: %v", r.err)
		}
		return r.f
	case <-time.After(d):
		t.Fatalf("timed out after %s waiting for an SSE frame", d)
		return sseFrame{}
	}
}

// TestSSE_FreshConnectionStreamsOnlyLiveAppends: a fresh SSE connection (no
// resume_from) does NOT replay the pre-existing backlog — it delivers only events
// recorded from connection time forward (EVT-132), pushing each new Append live
// (EVT-100/103).
func TestSSE_FreshConnectionStreamsOnlyLiveAppends(t *testing.T) {
	log := events.NewEventLog(0)
	log.Append(autoEnv(idA))
	log.Append(autoEnv(idB))
	hub := NewHub(log)

	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	br, closeConn := dialSSE(t, srv, "", nil)
	defer closeConn()

	// A live append after the fresh watermark is established must stream through —
	// the ingest's Append is what both records it and wakes the subscriber.
	hub.Append(autoEnv(idC))

	f := readFrameWithin(t, br, 2*time.Second)
	if f.event != "event" {
		t.Fatalf("live frame event field must be %q; got %q", "event", f.event)
	}
	if f.id != idC {
		t.Fatalf("fresh connection must stream only the live append %s, not the backlog; got id %q", idC, f.id)
	}
	var env events.Envelope
	if err := json.Unmarshal([]byte(f.data), &env); err != nil {
		t.Fatalf("frame data must be the envelope JSON (EVT-103); got %v data=%s", err, f.data)
	}
	if env.ID != idC || env.Origin != "relay" {
		t.Fatalf("streamed envelope must be the appended one (id %s, origin relay); got id=%q origin=%q", idC, env.ID, env.Origin)
	}
}

// TestSSE_ResumeFromStreamsBacklogThenLive: a resume_from at a retained id
// streams the backlog strictly after it, in id order, then continues live
// (EVT-102/133).
func TestSSE_ResumeFromStreamsBacklogThenLive(t *testing.T) {
	log := events.NewEventLog(0)
	log.Append(autoEnv(idA))
	log.Append(autoEnv(idB))
	log.Append(autoEnv(idC))
	hub := NewHub(log)

	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	br, closeConn := dialSSE(t, srv, "resume_from="+idA, nil)
	defer closeConn()

	// backlog after idA, in id order: idB then idC.
	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idB {
		t.Fatalf("first resumed frame must be event %s; got event=%q id=%q", idB, f.event, f.id)
	}
	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idC {
		t.Fatalf("second resumed frame must be event %s; got event=%q id=%q", idC, f.event, f.id)
	}

	// then a live append continues the same stream.
	hub.Append(autoEnv(idD))
	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idD {
		t.Fatalf("live continuation frame must be event %s; got event=%q id=%q", idD, f.event, f.id)
	}
}

// TestSSE_LastEventIDTakesPrecedenceOverResumeFromQuery: a Last-Event-ID header
// overrides a resume_from query parameter (EVT-102) — the browser-native
// reconnect path.
func TestSSE_LastEventIDTakesPrecedenceOverResumeFromQuery(t *testing.T) {
	log := events.NewEventLog(0)
	log.Append(autoEnv(idA))
	log.Append(autoEnv(idB))
	log.Append(autoEnv(idC))
	hub := NewHub(log)

	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	// query says resume from idA (would replay idB, idC); Last-Event-ID says idB,
	// which must win — so only idC streams.
	h := http.Header{}
	h.Set("Last-Event-ID", idB)
	br, closeConn := dialSSE(t, srv, "resume_from="+idA, h)
	defer closeConn()

	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idC {
		t.Fatalf("Last-Event-ID must take precedence (EVT-102): expected only event %s; got event=%q id=%q", idC, f.event, f.id)
	}
}

// TestSSE_MalformedResumeFromRejectedBeforeStream: a malformed resume_from is
// rejected with a RESUME_FROM_INVALID Problem before any event is delivered
// (EVT-134) — not treated as an omitted (fresh) resume.
func TestSSE_MalformedResumeFromRejectedBeforeStream(t *testing.T) {
	log := events.NewEventLog(0)
	h := newTestServer(NewHub(log))

	req := httptest.NewRequest(http.MethodGet, "/events/v1?resume_from=not_a_ulid", nil)
	req.Header.Set("Accept", "text/event-stream")
	testAuth().Authorize(req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a malformed resume_from must be a 400 Problem before the stream (EVT-134); got %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("rejection must be an api/1 Problem (application/problem+json); got %q", ct)
	}
	if strings.Contains(rec.Body.String(), "text/event-stream") {
		t.Fatalf("a rejected resume must not start a stream (EVT-134)")
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Problem body must be JSON; got %v", err)
	}
	if body.Code != events.ResumeFromInvalidCode {
		t.Fatalf("Problem code must be %s (EVT-134); got %q", events.ResumeFromInvalidCode, body.Code)
	}
}

// TestSSE_AgedOutResumeFromEmitsGapFirst: a resume_from older than the retention
// horizon is a retention_expired gap — an event: gap frame is streamed first
// (id = to_id = the oldest retained id), then delivery resumes at that id
// inclusive, never a silent loss (EVT-140/141/143).
func TestSSE_AgedOutResumeFromEmitsGapFirst(t *testing.T) {
	log := events.NewEventLog(2) // bounded: idA ages out when idC lands
	log.Append(autoEnv(idA))
	log.Append(autoEnv(idB))
	log.Append(autoEnv(idC))
	// retained now: idB, idC; oldest = idB.
	hub := NewHub(log)

	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	br, closeConn := dialSSE(t, srv, "resume_from="+idA, nil)
	defer closeConn()

	// frame 1: the gap loss marker, id = to_id = idB, reason retention_expired.
	g := readFrameWithin(t, br, 2*time.Second)
	if g.event != "gap" {
		t.Fatalf("aged-out resume must emit an event: gap first (EVT-104/140); got event=%q", g.event)
	}
	if g.id != idB {
		t.Fatalf("gap id must be to_id = the oldest retained id %s (EVT-104/141); got %q", idB, g.id)
	}
	var marker struct {
		FromID string `json:"from_id"`
		ToID   string `json:"to_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(g.data), &marker); err != nil {
		t.Fatalf("gap data must be the {from_id,to_id,reason} marker; got %v data=%s", err, g.data)
	}
	if marker.FromID != idA || marker.ToID != idB || marker.Reason != "retention_expired" {
		t.Fatalf("gap marker must be {from_id:%s,to_id:%s,reason:retention_expired} (EVT-140/141); got %+v", idA, idB, marker)
	}

	// then delivery resumes AT to_id inclusive: idB, then idC — no silent loss.
	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idB {
		t.Fatalf("gap resume must deliver AT to_id inclusive %s (EVT-143); got event=%q id=%q", idB, f.event, f.id)
	}
	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idC {
		t.Fatalf("gap resume must then deliver %s in order; got event=%q id=%q", idC, f.event, f.id)
	}
}

// TestSSE_MultipleSubscribersEachReceiveLiveAppend is the finding-1 regression:
// with TWO concurrently-connected fresh subscribers, a SINGLE Append must push
// the new event live to BOTH of them. A single shared notify channel would
// deliver the wake to only one blocked receiver, leaving the other asleep — this
// exercises the per-subscriber fan-out that guarantees every subscriber sees a
// live event (EVT-100).
func TestSSE_MultipleSubscribersEachReceiveLiveAppend(t *testing.T) {
	log := events.NewEventLog(0)
	hub := NewHub(log)

	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	br1, close1 := dialSSE(t, srv, "", nil)
	defer close1()
	br2, close2 := dialSSE(t, srv, "", nil)
	defer close2()

	// One Append must wake BOTH subscribers.
	hub.Append(autoEnv(idC))

	f1 := readFrameWithin(t, br1, 2*time.Second)
	f2 := readFrameWithin(t, br2, 2*time.Second)
	if f1.id != idC {
		t.Fatalf("subscriber 1 must receive the live append %s; got %q", idC, f1.id)
	}
	if f2.id != idC {
		t.Fatalf("subscriber 2 must ALSO receive the live append %s — a single shared channel would wake only one (EVT-100); got %q", idC, f2.id)
	}
}

// TestSSE_ConcurrentAppendsAndReadsAreRaceFree is the finding-2/4 regression: an
// SSE subscriber's live loop reads the shared log (hub.after) on one goroutine
// while a writer floods hub.Append on another. Both must go through the Hub's
// single lock, so under `go test -race` there is no unsynchronized read/write on
// the EventLog's backing slice, and every appended event is delivered exactly
// once, in id order, with none silently lost (EVT-135/143).
func TestSSE_ConcurrentAppendsAndReadsAreRaceFree(t *testing.T) {
	log := events.NewEventLog(0)
	hub := NewHub(log)

	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	br, closeConn := dialSSE(t, srv, "", nil)
	defer closeConn()

	const n = 40
	next := ulidSeq()
	want := make([]string, n)
	for i := range want {
		want[i] = next()
	}

	// Flood the log from a separate goroutine while the server's live loop reads.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, id := range want {
			hub.Append(autoEnv(id))
		}
	}()

	// Every appended event must arrive live, in ascending id order, none lost.
	for i := 0; i < n; i++ {
		f := readFrameWithin(t, br, 5*time.Second)
		if f.event != "event" || f.id != want[i] {
			t.Fatalf("live frame %d must be %s in order (no silent loss); got event=%q id=%q", i, want[i], f.event, f.id)
		}
	}
	wg.Wait()
}

// TestHub_SubscribeCapturesWatermarkAtomicallyWithRegistration is the finding-3
// regression at the boundary itself: Hub.subscribe must register the wake mailbox
// AND snapshot the fresh watermark under one lock hold. If those two steps were
// separated (as the pre-fix handler was — it wrote the 200 to announce the
// connection, THEN read the head as the watermark), an event appended in the gap
// would be neither in the backlog nor after the watermark, and silently dropped.
// Here: the head snapshot equals the pre-subscribe tail, the subscriber is
// already registered (a subsequent Append wakes it), and the appended event is
// strictly after the watermark — so it is delivered live, never lost (EVT-132).
func TestHub_SubscribeCapturesWatermarkAtomicallyWithRegistration(t *testing.T) {
	log := events.NewEventLog(0)
	log.Append(autoEnv(idA))
	hub := NewHub(log)

	sub, outcome, rerr := hub.subscribe("", everythingVisible)
	if rerr != nil {
		t.Fatalf("a fresh subscribe must not error; got %v", rerr)
	}
	defer sub.close()
	if outcome.Result != events.ResumeResultFresh {
		t.Fatalf("empty resume_from must resolve fresh; got %q", outcome.Result)
	}
	// The watermark is the head at the registration instant — the pre-existing idA.
	if sub.head != idA {
		t.Fatalf("fresh watermark must be the head snapshotted with registration (%s); got %q", idA, sub.head)
	}

	// An event appended AFTER subscribe returned must wake the already-registered
	// subscriber (registration happened inside subscribe, atomically) ...
	hub.Append(autoEnv(idB))
	select {
	case <-sub.wake():
	default:
		t.Fatalf("Append after subscribe must wake the registered subscriber — registration and the watermark snapshot must be one atomic step (EVT-132)")
	}
	// ... and be strictly after the watermark, so the live loop delivers it (the
	// pre-existing idA is excluded from a fresh subscriber's live tail).
	tail := hub.after(sub.head)
	if len(tail) != 1 || tail[0].ID != idB {
		t.Fatalf("the event appended at connection time must be in the deliverable tail after the watermark, never dropped (EVT-132/143); got %+v", tail)
	}
}

// TestSSE_FreshDeliversEveryAppendAfterConnect is the finding-3 regression at the
// handler: because the watermark is captured atomically with registration BEFORE
// the 200 is written, every event appended after the connection is established is
// delivered live, in order — nothing appended around connection time falls into a
// silent gap (EVT-132).
func TestSSE_FreshDeliversEveryAppendAfterConnect(t *testing.T) {
	log := events.NewEventLog(0)
	log.Append(autoEnv(idA)) // pre-existing — a fresh subscriber must NOT replay it
	hub := NewHub(log)

	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	br, closeConn := dialSSE(t, srv, "", nil)
	defer closeConn()

	for _, id := range []string{idB, idC, idD} {
		hub.Append(autoEnv(id))
		f := readFrameWithin(t, br, 2*time.Second)
		if f.event != "event" || f.id != id {
			t.Fatalf("every post-connect append must be delivered live in order (EVT-132); want %s got event=%q id=%q", id, f.event, f.id)
		}
	}
}

// TestHub_LiveDrainMarksBufferExceededGapOnRetentionDrop is the regression for
// the mid-stream slow-consumer loss (EVT-142/143): a connected subscriber whose
// live loop lags behind Append on a BOUNDED log has undelivered events aged out
// before it drains them. drain — exactly what the live loop runs on each wake —
// MUST mark that discontinuity with a buffer_exceeded gap and resume at the
// oldest retained id, never silently return the truncated tail with no signal.
//
// Before the fix the live loop called hub.after(lastID) directly: once lastID
// aged out, After just returned the surviving tail starting at the new oldest id,
// with no gap frame and no way for the caller to know entries were skipped — the
// silent loss EVT-143 forbids.
func TestHub_LiveDrainMarksBufferExceededGapOnRetentionDrop(t *testing.T) {
	log := events.NewEventLog(2) // bounded: memory-protecting retention horizon
	hub := NewHub(log)

	// A pre-existing event, then a fresh subscribe: the live watermark is idA.
	hub.Append(autoEnv(idA))
	sub, outcome, rerr := hub.subscribe("", everythingVisible)
	if rerr != nil {
		t.Fatalf("fresh subscribe must not error; got %v", rerr)
	}
	defer sub.close()
	if outcome.Result != events.ResumeResultFresh {
		t.Fatalf("empty resume_from must resolve fresh; got %q", outcome.Result)
	}
	lastID := sub.head // idA — the subscriber's last-delivered point
	if lastID != idA {
		t.Fatalf("fresh watermark must be the head idA; got %q", lastID)
	}

	// The subscriber has NOT drained yet. A burst appends idB, idC, idD faster
	// than it drains: retention 2 keeps only [idC, idD], so idB is appended then
	// aged out before the subscriber ever delivers it — a mid-stream drop.
	hub.Append(autoEnv(idB))
	hub.Append(autoEnv(idC)) // evicts idA
	hub.Append(autoEnv(idD)) // evicts idB (appended, never delivered)

	// The burst left a coalesced pending wake — the live loop WILL drain next.
	select {
	case <-sub.wake():
	default:
		t.Fatal("the burst must leave a pending wake for the live loop to drain")
	}

	// drain is exactly what the live loop runs on that wake.
	gap, tail := hub.drain(lastID)

	if gap == nil {
		t.Fatal("a mid-stream retention drop (idB appended then aged out before delivery) MUST emit a buffer_exceeded gap, never a silent truncation (EVT-142/143)")
	}
	if gap.Reason != events.ReasonBufferExceeded {
		t.Fatalf("mid-stream drop gap reason must be buffer_exceeded (EVT-142); got %q", gap.Reason)
	}
	if gap.FromID == nil || *gap.FromID != idA {
		t.Fatalf("gap from_id must be the subscriber's last-delivered id %s; got %v", idA, gap.FromID)
	}
	if gap.ToID != idC {
		t.Fatalf("gap to_id must be the oldest retained id %s (delivery resumes there); got %q", idC, gap.ToID)
	}
	// Delivery then resumes AT the oldest retained id inclusive — no further loss.
	if len(tail) != 2 || tail[0].ID != idC || tail[1].ID != idD {
		t.Fatalf("after the gap, drain must deliver the retained tail [idC idD] inclusive; got %v", ids(tail))
	}
}

// TestHub_LiveDrainNoGapWhenCaughtUp is the false-positive guard for the fix: a
// subscriber whose last-delivered id is still within (or ahead of) the retention
// window must NOT be handed a spurious gap. Here retention ages out only idA; a
// subscriber at idB still has every later event (idC) retained, so drain returns
// the clean tail with NO gap — buffer_exceeded is only for a genuine drop.
func TestHub_LiveDrainNoGapWhenCaughtUp(t *testing.T) {
	log := events.NewEventLog(2)
	hub := NewHub(log)

	hub.Append(autoEnv(idA))
	hub.Append(autoEnv(idB))
	hub.Append(autoEnv(idC)) // retained [idB idC]; only idA aged out

	gap, tail := hub.drain(idB) // subscriber last saw idB, which is still retained
	if gap != nil {
		t.Fatalf("a subscriber caught up to a still-retained id must NOT get a spurious gap; got %+v", *gap)
	}
	if len(tail) != 1 || tail[0].ID != idC {
		t.Fatalf("drain must return the clean tail [idC]; got %v", ids(tail))
	}
}

// ids extracts the ordered id list from a slice of envelopes for test messages.
func ids(evs []events.Envelope) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.ID)
	}
	return out
}

// TestHub_CloseEndsLiveSubscribers: Hub.Close ends every live subscriber stream
// for a graceful shutdown — a subscriber blocked in its live loop returns rather
// than hanging the server's shutdown forever (the seam the deferred WS transport
// and a real net/http server rely on).
func TestHub_CloseEndsLiveSubscribers(t *testing.T) {
	log := events.NewEventLog(0)
	hub := NewHub(log)

	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	br, closeConn := dialSSE(t, srv, "", nil)
	defer closeConn()

	// Prove the stream is live, then close the hub — the body must reach EOF.
	hub.Append(autoEnv(idA))
	if f := readFrameWithin(t, br, 2*time.Second); f.id != idA {
		t.Fatalf("stream must be live before shutdown; got %q", f.id)
	}

	hub.Close()
	hub.Close() // idempotent

	done := make(chan error, 1)
	go func() {
		_, err := br.ReadString('\n')
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("after Hub.Close the subscriber stream must end (EOF), not deliver another frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Hub.Close must end a blocked subscriber's live loop; the stream hung")
	}
}
