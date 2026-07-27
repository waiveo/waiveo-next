package eventsse

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// persistence_test.go drives the SSE binding over the PERSISTENT event log
// rather than the in-memory one — the wiring a deployment actually runs
// (cmd/waiveo-feeder) — across a real process boundary: the store is closed and
// reopened from the same file, and a brand-new Hub and handler are built over
// the reopened one. Nothing in the second half can be answered from state the
// first half left in memory.
//
// The rest of this package's tests stay on the in-memory log on purpose: they
// drive stream SEMANTICS (auth, scope, selector, schemas, fresh/resume/gap
// framing), which the substrate must not change. This file drives the one thing
// only the persistent substrate can answer.

// feederStack is one "process": a store, its event log, a Hub over it and a live
// SSE server. Closing it is that process ending.
type feederStack struct {
	store *store.Store
	log   *store.EventLog
	hub   *Hub
	srv   *httptest.Server
}

func (f *feederStack) close() {
	f.srv.Close()
	f.hub.Close()
	f.store.Close()
}

// bootFeederStack opens (or reopens) the whole stack over dsn, exactly as the
// feeder does: the store, the durable event log with an injected clock, the Hub,
// and the live authenticated handler.
func bootFeederStack(t *testing.T, dsn string, nowMs func() int64) *feederStack {
	t.Helper()
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	log, err := st.EventLog(events.DefaultRetentionPolicy(), nowMs, func(err error) {
		t.Errorf("the durable event log reported a storage failure: %v", err)
	})
	if err != nil {
		st.Close()
		t.Fatalf("open event log: %v", err)
	}
	hub := NewHub(log)
	return &feederStack{store: st, log: log, hub: hub, srv: httptest.NewServer(newTestServer(hub))}
}

// TestSSE_ResumeAcrossAFeederRestartDeliversTheBacklog is the durability
// property stated where a subscriber can see it. A client observes an event,
// keeps its id, the feeder restarts, and the client reconnects with that id as
// its resume_from: it must get every event recorded after it, in id order, with
// neither a gap nor a duplicate (EVT-133).
//
// Over the old in-memory log this same reconnect resolved to
// RESUME_FROM_INVALID — the id named an event that no longer existed anywhere —
// so the client's only recourse was to start fresh and silently lose everything
// recorded while it was away.
func TestSSE_ResumeAcrossAFeederRestartDeliversTheBacklog(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	clock := int64(1_752_537_600_000)
	now := func() int64 { return clock }

	// Process 1: three events recorded, the subscriber last saw idA.
	first := bootFeederStack(t, dsn, now)
	first.hub.Append(classed(t, autoEnv(idA)))
	first.hub.Append(classed(t, autoEnv(idB)))
	first.hub.Append(classed(t, autoEnv(idC)))
	first.close()

	// Process 2: same file, new everything.
	second := bootFeederStack(t, dsn, now)
	defer second.close()

	br, closeConn := dialSSE(t, second.srv, "resume_from="+idA, nil)
	defer closeConn()

	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idB {
		t.Fatalf("a cursor held across a restart must resume with the backlog after it: want event %s; got event=%q id=%q", idB, f.event, f.id)
	}
	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idC {
		t.Fatalf("the backlog must continue in id order: want event %s; got event=%q id=%q", idC, f.event, f.id)
	}

	// And the restarted process is still a LIVE stream, not just a replay.
	second.hub.Append(classed(t, autoEnv(idD)))
	f := readFrameWithin(t, br, 2*time.Second)
	if f.event != "event" || f.id != idD {
		t.Fatalf("live delivery must continue after the resumed backlog: want event %s; got event=%q id=%q", idD, f.event, f.id)
	}
	var env events.Envelope
	if err := json.Unmarshal([]byte(f.data), &env); err != nil {
		t.Fatalf("frame data must be the envelope JSON (EVT-103): %v data=%s", err, f.data)
	}
	if env.ID != idD || env.Schema != events.SchemaAutomationRun {
		t.Fatalf("the streamed envelope must be the appended one; got id=%q schema=%q", env.ID, env.Schema)
	}
}

// TestSSE_AgedOutCursorAcrossARestartIsAMarkedGap: the other resume outcome,
// end to end over the persisted log. Events expire by their retention class
// (driven by advancing the injected clock and running the sweep — nothing waits),
// the feeder restarts, and a client resuming from an expired id gets a gap frame
// naming its own point and the oldest point still deliverable, then delivery
// resuming AT that point — never a silent hole (EVT-140/141/143).
func TestSSE_AgedOutCursorAcrossARestartIsAMarkedGap(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	epoch := int64(1_752_537_600_000)
	clock := epoch
	now := func() int64 { return clock }

	telemetry := classed(t, autoEnv(idA)).RetentionClass
	window := events.DefaultRetentionPolicy().For(telemetry).Window
	if window <= 0 {
		t.Fatalf("automation.run's retention class %q must configure a window", telemetry)
	}

	first := bootFeederStack(t, dsn, now)
	first.hub.Append(atMs(classed(t, autoEnv(idA)), epoch))
	first.hub.Append(atMs(classed(t, autoEnv(idB)), epoch))
	// idC is recorded a full window later, so it survives the sweep below.
	first.hub.Append(atMs(classed(t, autoEnv(idC)), epoch+window.Milliseconds()))
	first.close()

	clock = epoch + window.Milliseconds() + 1
	second := bootFeederStack(t, dsn, now)
	defer second.close()
	if _, err := second.log.Prune(); err != nil {
		t.Fatalf("retention sweep: %v", err)
	}

	br, closeConn := dialSSE(t, second.srv, "resume_from="+idA, nil)
	defer closeConn()

	g := readFrameWithin(t, br, 2*time.Second)
	if g.event != "gap" {
		t.Fatalf("an expired cursor must emit an event: gap first (EVT-104/140); got event=%q", g.event)
	}
	if g.id != idC {
		t.Fatalf("the gap frame's id must be to_id, the oldest still-deliverable id %s; got %q", idC, g.id)
	}
	var marker struct {
		FromID string `json:"from_id"`
		ToID   string `json:"to_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(g.data), &marker); err != nil {
		t.Fatalf("gap data must be the {from_id,to_id,reason} marker: %v data=%s", err, g.data)
	}
	if marker.FromID != idA || marker.ToID != idC || marker.Reason != events.ReasonRetentionExpired {
		t.Fatalf("the loss marker must name the requested point, the resume point and retention_expired (EVT-140/141); got %+v", marker)
	}

	// Delivery resumes AT to_id inclusive: no silent loss between the gap and
	// the first event.
	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idC {
		t.Fatalf("delivery must resume AT to_id (%s) inclusive (EVT-143); got event=%q id=%q", idC, f.event, f.id)
	}
}

// TestSSE_RestartedProcessNeverMintsAnIDBelowTheStoredHead is the id-ordering
// half of a restart (EVT-011). A generator's monotonicity is per-process; the log
// is not. If a restarted feeder minted an id below one already stored — which a
// box whose clock came back low would do with a fresh generator — that event
// would be appended BEHIND a connected subscriber's cursor and never delivered
// to it at all.
//
// The feeder seeds its generator from the stored head, which is what this drives:
// the id minted after the restart sorts above every stored id, and the event it
// carries actually reaches a subscriber resuming from the pre-restart head.
func TestSSE_RestartedProcessNeverMintsAnIDBelowTheStoredHead(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	clock := int64(1_752_537_600_000)
	now := func() int64 { return clock }

	// The stored head is deliberately FAR in the future relative to the wall
	// clock a restarted process reads, which is the shape of a box whose clock
	// came back low (security-model/1 SEC-066-068) — and the case where a fresh
	// ulid.Monotonic() would mint underneath the log.
	if !ulid.Valid(futureHeadID) {
		t.Fatalf("fixture %q is not a canonical ULID", futureHeadID)
	}

	first := bootFeederStack(t, dsn, now)
	first.hub.Append(classed(t, autoEnv(idA)))
	first.hub.Append(classed(t, autoEnv(futureHeadID)))
	first.close()

	second := bootFeederStack(t, dsn, now)
	defer second.close()

	head, err := second.log.Head()
	if err != nil {
		t.Fatalf("read the stored head: %v", err)
	}
	if head != futureHeadID {
		t.Fatalf("the reopened log's head must be the highest stored id %s; got %q", futureHeadID, head)
	}

	next := ulid.MonotonicFrom(head)
	minted := next()
	if minted <= head {
		t.Fatalf("a restarted process must not mint an id at or below the stored head: minted %q, head %q (EVT-011)", minted, head)
	}

	// Proof that it matters: a subscriber resuming from the stored head receives
	// the newly minted event. With an id below the head it would sort into the
	// already-delivered range and never be offered.
	br, closeConn := dialSSE(t, second.srv, "resume_from="+head, nil)
	defer closeConn()
	second.hub.Append(classed(t, autoEnv(minted)))
	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != minted {
		t.Fatalf("the post-restart event must reach a subscriber resuming from the stored head; want %s, got event=%q id=%q", minted, f.event, f.id)
	}
}

// atMs returns env recorded at ms — the recording timestamp retention is
// evaluated against (EVT-010's `ts`).
func atMs(env events.Envelope, ms int64) events.Envelope {
	env.TS = ms
	return env
}

// classed stamps env with the cost/retention classes the platform actually
// assigns its schema, read from the same index a producer reads
// (events.ClassFor) rather than written down here. The shared autoEnv fixture
// carries placeholder class strings, which is fine for the stream-semantics
// tests that ignore them and not fine for a test about retention.
func classed(t *testing.T, env events.Envelope) events.Envelope {
	t.Helper()
	cost, retention, ok := events.ClassFor(env.Schema)
	if !ok {
		t.Fatalf("schema %q carries no pinned class; this fixture cannot represent a real producer's event", env.Schema)
	}
	env.CostClass, env.RetentionClass = cost, retention
	return env
}

// futureHeadID is a canonical ULID whose 48-bit timestamp sits tens of
// thousands of years ahead of any real clock reading: it differs from a
// present-day id only in the second character, which carries 2^40 milliseconds
// of weight. That is the shape a box whose clock came back low sees in its own
// stored log, and the case a fresh per-process id generator gets wrong.
//
// It is written out rather than minted, so the fixture does not depend on the
// generator under test — and it is deliberately nowhere near the top of the
// ULID range, so a generator seeded from it has room to ascend.
const futureHeadID = "02J8Z3K4N5P6Q7R8S9T0V1W2Y9"
