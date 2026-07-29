package store_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/events"
)

// eventlog_test.go drives the persistent half of events/1's durable event log:
// the property EVT-010 states as "retained for a bounded window regardless of
// whether any subscriber is connected", which for anything that has to outlive a
// restart means a real close and a real reopen of a real file — never an
// in-memory stand-in that shares its state with the "restarted" reader.
//
// Fixture ULIDs (fixture-ULID convention; no secrets). They are hand-written so
// their ORDER is visible in the source: E1 < E2 < E3 < E4 lexicographically,
// which for a ULID is also recording order (EVT-011).
const (
	evtE1 = "01J8ZA000000000000000000E1"
	evtE2 = "01J8ZA000000000000000000E2"
	evtE3 = "01J8ZA000000000000000000E3"
	evtE4 = "01J8ZA000000000000000000E4"
	evtE5 = "01J8ZA000000000000000000E5"

	evtScopeNode = "01J8ZA00SC0PEN0DE000000000"
	evtTraceID   = "01J8ZA00TRACE00000000000ID"
	evtPrincipal = "01J8ZA00PR1NC1PA1000000000"
)

// evtEpoch is the pinned instant every fixture event below is recorded at. Every
// clock in this file is derived from it, so nothing here reads the wall clock and
// no test waits for time to pass.
const evtEpoch int64 = 1_752_537_600_000

// fakeClock is the injected clock retention is evaluated against. Advance moves
// it; nothing sleeps.
type fakeClock struct{ ms int64 }

func (c *fakeClock) now() int64              { return c.ms }
func (c *fakeClock) advance(d time.Duration) { c.ms += d.Milliseconds() }

// telemetryEvent and auditEvent build envelopes on the two retention tiers the
// shipping policy configures. The class strings come from the REAL constructors
// (events.AuditEventEnvelope / events.BoxVitalsEnvelope would be overkill here)
// via events.ClassFor, which is the same index a producer reads — so a test
// event is classed exactly as the platform would class it, never by a literal
// copied into this file.
func classedEvent(t *testing.T, id, schema string, ts int64) events.Envelope {
	t.Helper()
	cost, retention, ok := events.ClassFor(schema)
	if !ok {
		t.Fatalf("schema %q carries no pinned class; this fixture cannot represent a real producer's event", schema)
	}
	return events.Envelope{
		ID:              id,
		Schema:          schema,
		TS:              ts,
		ScopeNode:       evtScopeNode,
		TraceID:         evtTraceID,
		CostClass:       cost,
		RetentionClass:  retention,
		Origin:          "internal",
		OriginPrincipal: evtPrincipal,
		Payload:         json.RawMessage(`{"probe":"` + id + `"}`),
	}
}

func telemetryEvent(t *testing.T, id string, ts int64) events.Envelope {
	t.Helper()
	return classedEvent(t, id, events.SchemaBoxVitals, ts)
}

func auditEvent(t *testing.T, id string, ts int64) events.Envelope {
	t.Helper()
	return classedEvent(t, id, events.SchemaAuditEvent, ts)
}

// openEventLog opens a store at dsn and its event log, failing the test on any
// error. errs collects everything the log reports through its error sink, so a
// test that expected a clean run can assert nothing was swallowed.
func openEventLog(t *testing.T, dsn string, clock *fakeClock) (*store.Store, *store.EventLog, *[]error) {
	t.Helper()
	st, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var errs []error
	log, err := st.EventLog(events.DefaultRetentionPolicy(), clock.now, func(err error) { errs = append(errs, err) })
	if err != nil {
		st.Close()
		t.Fatalf("open event log: %v", err)
	}
	return st, log, &errs
}

func idsOf(envs []events.Envelope) []string {
	out := make([]string, 0, len(envs))
	for _, e := range envs {
		out = append(out, e.ID)
	}
	return out
}

func requireNoSinkErrors(t *testing.T, errs *[]error) {
	t.Helper()
	if len(*errs) != 0 {
		t.Fatalf("the event log reported storage failures it had to answer conservatively for: %v", *errs)
	}
}

// TestEventLog_SurvivesRestart is the whole point of the file: events recorded
// before a process ends are still there, byte for byte, after it is restarted
// (EVT-010's "durable event"). The store is CLOSED and REOPENED from the same
// file — not merely re-read through the same handle — so nothing about the
// result can come from state the first process left in memory.
func TestEventLog_SurvivesRestart(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	clock := &fakeClock{ms: evtEpoch}

	st, log, errs := openEventLog(t, dsn, clock)
	want := []events.Envelope{
		telemetryEvent(t, evtE1, evtEpoch),
		auditEvent(t, evtE2, evtEpoch),
		telemetryEvent(t, evtE3, evtEpoch),
	}
	for _, env := range want {
		log.Append(env)
	}
	requireNoSinkErrors(t, errs)
	st.Close()

	st2, log2, errs2 := openEventLog(t, dsn, clock)
	defer st2.Close()

	got := log2.After("")
	if len(got) != len(want) {
		t.Fatalf("a restart must not lose events inside the retention window: recorded %d, read back %v", len(want), idsOf(got))
	}
	for i, env := range got {
		if env.ID != want[i].ID || env.Schema != want[i].Schema || env.TS != want[i].TS ||
			env.ScopeNode != want[i].ScopeNode || env.TraceID != want[i].TraceID ||
			env.CostClass != want[i].CostClass || env.RetentionClass != want[i].RetentionClass ||
			env.Origin != want[i].Origin || env.OriginPrincipal != want[i].OriginPrincipal {
			t.Fatalf("event %d came back altered across the restart:\n before %+v\n  after %+v", i, want[i], env)
		}
		if string(env.Payload) != string(want[i].Payload) {
			t.Fatalf("event %s payload changed across the restart: before %s after %s", env.ID, want[i].Payload, env.Payload)
		}
	}
	requireNoSinkErrors(t, errs2)
}

// TestEventLog_ResumeAcrossRestartIsAClean133Resume: the cursor a subscriber
// held before the restart still names a retained event afterwards, so its
// reconnect is a clean `resumed` with the backlog strictly after it — gap-free
// and duplicate-free (EVT-133). Before this log was persistent that same
// reconnect resolved to RESUME_FROM_INVALID, because the id it named no longer
// existed anywhere.
func TestEventLog_ResumeAcrossRestartIsAClean133Resume(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	clock := &fakeClock{ms: evtEpoch}

	st, log, _ := openEventLog(t, dsn, clock)
	log.Append(telemetryEvent(t, evtE1, evtEpoch))
	log.Append(auditEvent(t, evtE2, evtEpoch))
	log.Append(telemetryEvent(t, evtE3, evtEpoch))
	st.Close()

	st2, log2, errs := openEventLog(t, dsn, clock)
	defer st2.Close()

	outcome, rerr := events.Resolve(log2, evtE1)
	if rerr != nil {
		t.Fatalf("a cursor held across a restart must still resolve; got %s (EVT-133/134)", rerr.Code)
	}
	if outcome.Result != events.ResumeResultResumed {
		t.Fatalf("resume_result must be resumed for a still-retained cursor; got %q", outcome.Result)
	}
	if outcome.Gap != nil {
		t.Fatalf("a retained cursor resumes with NO gap; got %+v", outcome.Gap)
	}
	if got := idsOf(outcome.Events); len(got) != 2 || got[0] != evtE2 || got[1] != evtE3 {
		t.Fatalf("the resumed backlog must be exactly the events after the cursor, in id order; got %v", got)
	}
	requireNoSinkErrors(t, errs)
}

// TestEventLog_AgedOutCursorAcrossRestartIsAMarkedGap: the other half of the
// resume contract over a persisted store. A cursor whose event has since been
// expired by its retention window resolves to a retention_expired GAP naming the
// requested point and the oldest point still deliverable above it, and delivery
// resumes AT that point inclusive — never a silent hole (EVT-140/141/143).
//
// The expiry is driven by advancing the injected clock past the telemetry window
// and calling Prune; nothing here waits.
func TestEventLog_AgedOutCursorAcrossRestartIsAMarkedGap(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	clock := &fakeClock{ms: evtEpoch}
	policy := events.DefaultRetentionPolicy()
	telemetryWindow := policy.For(telemetryClass(t)).Window

	st, log, _ := openEventLog(t, dsn, clock)
	// E1 and E2 are recorded at the epoch; E3 a full window later, so a sweep
	// after E3's own recording time expires the first two and keeps it.
	log.Append(telemetryEvent(t, evtE1, evtEpoch))
	log.Append(telemetryEvent(t, evtE2, evtEpoch))
	log.Append(telemetryEvent(t, evtE3, evtEpoch+telemetryWindow.Milliseconds()))
	st.Close()

	// A NEW process: the sweep and the resume both run against a reopened file.
	clock.advance(telemetryWindow + time.Millisecond)
	st2, log2, errs := openEventLog(t, dsn, clock)
	defer st2.Close()

	pruned, err := log2.Prune()
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned.Rows != 2 {
		t.Fatalf("the two events older than the telemetry window must expire; pruned %d (%v)", pruned.Rows, pruned.ByClass)
	}
	if pruned.EvictedThrough != evtE2 {
		t.Fatalf("the eviction watermark must name the highest expired id %s; got %q", evtE2, pruned.EvictedThrough)
	}

	outcome, rerr := events.Resolve(log2, evtE1)
	if rerr != nil {
		t.Fatalf("an aged-out cursor is a marked gap, never a rejection (EVT-143); got %s", rerr.Code)
	}
	if outcome.Result != events.ResumeResultGap {
		t.Fatalf("resume_result must be gap for an expired cursor; got %q", outcome.Result)
	}
	if outcome.Gap == nil || outcome.Gap.Reason != events.ReasonRetentionExpired {
		t.Fatalf("the loss marker must be reason retention_expired (EVT-141); got %+v", outcome.Gap)
	}
	if outcome.Gap.FromID == nil || *outcome.Gap.FromID != evtE1 {
		t.Fatalf("from_id must be the subscriber's own requested point %s; got %+v", evtE1, outcome.Gap.FromID)
	}
	if outcome.Gap.ToID != evtE3 || outcome.ResumeAtID != evtE3 {
		t.Fatalf("to_id must be the oldest still-deliverable id %s; got to_id=%q resume_at=%q", evtE3, outcome.Gap.ToID, outcome.ResumeAtID)
	}
	if got := idsOf(outcome.Events); len(got) != 1 || got[0] != evtE3 {
		t.Fatalf("delivery must resume AT to_id inclusive, with no silent loss; got %v", got)
	}
	requireNoSinkErrors(t, errs)
}

// TestEventLog_EvictionWatermarkSurvivesRestart: the mid-stream loss check
// (EVT-142/143) reads a watermark, and a watermark that reset on restart would
// answer "nothing was ever lost" for exactly the subscribers that reconnected
// across it. The eviction happens in one process; the question is asked in the
// next.
func TestEventLog_EvictionWatermarkSurvivesRestart(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	clock := &fakeClock{ms: evtEpoch}
	window := events.DefaultRetentionPolicy().For(telemetryClass(t)).Window

	st, log, _ := openEventLog(t, dsn, clock)
	log.Append(telemetryEvent(t, evtE1, evtEpoch))
	log.Append(telemetryEvent(t, evtE2, evtEpoch))
	log.Append(telemetryEvent(t, evtE3, evtEpoch+window.Milliseconds()))
	clock.advance(window + time.Millisecond)
	if _, err := log.Prune(); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !log.EvictedAfter(evtE1) {
		t.Fatal("a subscriber last at E1 lost E2 to expiry; EvictedAfter must say so before the restart too")
	}
	st.Close()

	st2, log2, _ := openEventLog(t, dsn, clock)
	defer st2.Close()

	if got := log2.EvictedThrough(); got != evtE2 {
		t.Fatalf("the persisted eviction watermark must come back as %s; got %q", evtE2, got)
	}
	if !log2.EvictedAfter(evtE1) {
		t.Fatal("a subscriber last at E1 lost E2 before it could be delivered; a restart must not turn that into 'no gap' (EVT-142/143)")
	}
	if log2.EvictedAfter(evtE3) {
		t.Fatal("a subscriber caught up past everything evicted has no gap; the restart must not invent one")
	}
}

// TestEventLog_RetentionClassesExpireIndependently is EVT-082 exercised against
// real storage rather than asserted over the policy table: at an instant past the
// telemetry window but well inside the audit window, the telemetry rows are gone
// and the audit record recorded at the SAME instant is still there.
//
// That is the audit trail outliving the operational telemetry recorded alongside
// it — the property EVT-082 says the retention_class field exists to express,
// and the one SEC-150 leans on by making audit.event the platform's only audit
// mechanism.
func TestEventLog_RetentionClassesExpireIndependently(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	clock := &fakeClock{ms: evtEpoch}
	policy := events.DefaultRetentionPolicy()
	telemetryWindow := policy.For(telemetryClass(t)).Window
	auditWindow := policy.For(events.AuditRetentionClass).Window

	st, log, errs := openEventLog(t, dsn, clock)
	defer st.Close()

	// Recorded in the same millisecond, on the two different tiers.
	log.Append(telemetryEvent(t, evtE1, evtEpoch))
	log.Append(auditEvent(t, evtE2, evtEpoch))
	log.Append(telemetryEvent(t, evtE3, evtEpoch))

	clock.advance(telemetryWindow + time.Millisecond)
	pruned, err := log.Prune()
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned.Rows != 2 {
		t.Fatalf("only the two telemetry events are past their window; pruned %d (%v)", pruned.Rows, pruned.ByClass)
	}
	if got := idsOf(log.After("")); len(got) != 1 || got[0] != evtE2 {
		t.Fatalf("the audit record must outlive the telemetry recorded alongside it (EVT-082); retained %v", got)
	}

	// And it keeps outliving it right up to its own window, which is the part
	// that makes the relationship real rather than a one-sweep coincidence. A
	// window is a floor — "retained for a bounded window" (EVT-010) — so a
	// record whose age is EXACTLY the window is still inside it, and only an
	// older one expires.
	clock.ms = evtEpoch + auditWindow.Milliseconds()
	if _, err := log.Prune(); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !log.Has(evtE2) {
		t.Fatalf("a record aged exactly its own %v window is still inside it", auditWindow)
	}

	clock.advance(time.Millisecond)
	if _, err := log.Prune(); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if log.Has(evtE2) {
		t.Fatalf("past its own window the audit record expires too — retention is bounded for every class (EVT-010)")
	}
	requireNoSinkErrors(t, errs)
}

// TestEventLog_TelemetryFloodNeverEvictsAudit: the per-class row cap, which is
// the other half of EVT-082. A global oldest-first cap would delete the oldest
// audit record to admit the newest telemetry one; here the audit record is the
// oldest thing in the log and a flood well past the telemetry cap goes straight
// past it.
func TestEventLog_TelemetryFloodNeverEvictsAudit(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	clock := &fakeClock{ms: evtEpoch}
	// A deliberately tiny telemetry cap: the shipping cap is 4096, and writing
	// 4097 rows to prove a boundary would say nothing the policy's own MaxRows
	// does not already say. What has to be proven is the CLASS boundary.
	policy := events.NewRetentionPolicy(map[string]events.ClassRetention{
		telemetryClass(t):          {Window: time.Hour, MaxRows: 2},
		events.AuditRetentionClass: {Window: 400 * 24 * time.Hour, MaxRows: 0},
	}, events.ClassRetention{Window: time.Hour, MaxRows: 2})

	st, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	log, err := st.EventLog(policy, clock.now, func(err error) { t.Errorf("event log: %v", err) })
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}

	// The audit record is the OLDEST id in the log — first in line under any
	// global oldest-first cap.
	log.Append(auditEvent(t, evtE1, evtEpoch))
	log.Append(telemetryEvent(t, evtE2, evtEpoch))
	log.Append(telemetryEvent(t, evtE3, evtEpoch))
	log.Append(telemetryEvent(t, evtE4, evtEpoch))
	log.Append(telemetryEvent(t, evtE5, evtEpoch))

	got := idsOf(log.After(""))
	if len(got) != 3 || got[0] != evtE1 || got[1] != evtE4 || got[2] != evtE5 {
		t.Fatalf("the telemetry tier must trim to its own cap and leave the audit record alone; retained %v", got)
	}
	if w := log.EvictedThrough(); w != evtE3 {
		t.Fatalf("the capped-out telemetry must be marked, not silently dropped (EVT-143): watermark %q, want %s", w, evtE3)
	}
}

// TestEventLog_AppendIsIdempotentOnID: at-least-once delivery makes the envelope
// id a subscriber's dedup key (EVT-135), which only works if the log itself
// treats a redelivered id as the same record rather than a second one.
func TestEventLog_AppendIsIdempotentOnID(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	clock := &fakeClock{ms: evtEpoch}
	st, log, errs := openEventLog(t, dsn, clock)
	defer st.Close()

	log.Append(telemetryEvent(t, evtE1, evtEpoch))
	log.Append(telemetryEvent(t, evtE1, evtEpoch))
	if got := idsOf(log.After("")); len(got) != 1 {
		t.Fatalf("a redelivered id must not create a second row (EVT-135); retained %v", got)
	}
	requireNoSinkErrors(t, errs)
}

// TestEventLog_RefusesANonULIDID: every id this store persists is a canonical
// ULID (DAT-005a), and an event id is additionally the ordering key every resume
// and every gap bound is computed from (EVT-011). One that sorts by different
// rules is refused at the boundary and reported, never stored.
func TestEventLog_RefusesANonULIDID(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	clock := &fakeClock{ms: evtEpoch}
	st, log, errs := openEventLog(t, dsn, clock)
	defer st.Close()

	bad := telemetryEvent(t, evtE1, evtEpoch)
	bad.ID = "not-a-ulid"
	log.Append(bad)

	if got := idsOf(log.After("")); len(got) != 0 {
		t.Fatalf("a non-ULID event id must not be stored; retained %v", got)
	}
	if len(*errs) != 1 {
		t.Fatalf("a refused append must be REPORTED — a durable record that was not written is not a silent condition; sink got %v", *errs)
	}
}

// TestEventLog_SubstrateQuestionsMatchTheInMemoryLog holds the persistent
// implementation to the same answers internal/events states for the in-memory
// one: a retained id is never aged out, an id ahead of the whole log was never
// recorded (EVT-134's rejection, not EVT-141's gap), and OldestRetainedAfter is
// strictly after its argument.
func TestEventLog_SubstrateQuestionsMatchTheInMemoryLog(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	clock := &fakeClock{ms: evtEpoch}
	window := events.DefaultRetentionPolicy().For(telemetryClass(t)).Window

	st, log, _ := openEventLog(t, dsn, clock)
	defer st.Close()

	if got := log.HeadID(); got != "" {
		t.Fatalf("an empty log has no head; got %q", got)
	}
	if log.AgedOut(evtE1) {
		t.Fatal("an empty log has evicted nothing, so no id has aged out of it")
	}

	log.Append(telemetryEvent(t, evtE1, evtEpoch))
	log.Append(telemetryEvent(t, evtE2, evtEpoch+window.Milliseconds()))
	log.Append(telemetryEvent(t, evtE3, evtEpoch+window.Milliseconds()))

	if got := log.HeadID(); got != evtE3 {
		t.Fatalf("HeadID must be the newest retained id %s; got %q", evtE3, got)
	}
	if got := log.OldestRetainedAfter(evtE1); got != evtE2 {
		t.Fatalf("OldestRetainedAfter must be STRICTLY after its argument; got %q", got)
	}
	if got := log.OldestRetainedAfter(evtE3); got != "" {
		t.Fatalf("nothing is retained after the head; got %q", got)
	}
	if log.AgedOut(evtE4) {
		t.Fatal("an id ahead of the whole log was never recorded — EVT-134's rejection, not EVT-141's gap")
	}

	clock.advance(window + time.Millisecond)
	if _, err := log.Prune(); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !log.AgedOut(evtE1) {
		t.Fatal("E1 was expired by its retention window, so a resume from it has aged out (EVT-141)")
	}
	if log.AgedOut(evtE2) || log.AgedOut(evtE3) {
		t.Fatal("a RETAINED id must never report as aged out — it resumes cleanly (EVT-133)")
	}
}

// TestEventLog_AnAuditRecordBelowTheWatermarkStillResumesCleanly is the
// mixed-class case a single-horizon log cannot produce, and the one that would
// silently misbehave if aged-out were still "below the oldest retained id" or if
// to_id were still "the oldest retained id".
//
// Setup: an old audit record, then newer telemetry that expires. The audit
// record now sits BELOW the eviction watermark while remaining perfectly
// retained. Resuming from it must be a clean resume, not a gap; and resuming
// from an expired telemetry id must name a to_id ABOVE it, not the audit record
// the subscriber already has.
func TestEventLog_AnAuditRecordBelowTheWatermarkStillResumesCleanly(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	clock := &fakeClock{ms: evtEpoch}
	window := events.DefaultRetentionPolicy().For(telemetryClass(t)).Window

	st, log, errs := openEventLog(t, dsn, clock)
	defer st.Close()

	log.Append(auditEvent(t, evtE1, evtEpoch))     // oldest id, long-lived
	log.Append(telemetryEvent(t, evtE2, evtEpoch)) // expires
	log.Append(telemetryEvent(t, evtE3, evtEpoch)) // expires
	log.Append(telemetryEvent(t, evtE4, evtEpoch+window.Milliseconds()))

	clock.advance(window + time.Millisecond)
	if _, err := log.Prune(); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := log.EvictedThrough(); got != evtE3 {
		t.Fatalf("watermark must be the highest expired id %s; got %q", evtE3, got)
	}
	if got := log.OldestRetainedID(); got != evtE1 {
		t.Fatalf("the audit record is now the oldest retained id AND sits below the watermark; got %q", got)
	}

	// The audit cursor is retained, so it resumes cleanly even though it is
	// below the watermark.
	outcome, rerr := events.Resolve(log, evtE1)
	if rerr != nil {
		t.Fatalf("a retained audit cursor must resolve; got %s", rerr.Code)
	}
	if outcome.Result != events.ResumeResultResumed {
		t.Fatalf("a retained cursor resumes cleanly regardless of what expired above it; got %q", outcome.Result)
	}
	if got := idsOf(outcome.Events); len(got) != 1 || got[0] != evtE4 {
		t.Fatalf("the backlog after the audit cursor is what is still retained above it; got %v", got)
	}

	// The expired telemetry cursor gaps to the oldest id ABOVE it — never back
	// down to the audit record, which would resume delivery behind the
	// subscriber and replay what it already had as the far side of a gap.
	expired, rerr := events.Resolve(log, evtE2)
	if rerr != nil {
		t.Fatalf("an expired cursor is a marked gap, not a rejection; got %s", rerr.Code)
	}
	if expired.Result != events.ResumeResultGap || expired.Gap == nil {
		t.Fatalf("resume_result must be gap; got %q", expired.Result)
	}
	if expired.Gap.ToID != evtE4 {
		t.Fatalf("to_id must be the oldest deliverable id ABOVE the lost point (%s), not the oldest retained id (%s); got %q",
			evtE4, evtE1, expired.Gap.ToID)
	}
	if got := idsOf(expired.Events); len(got) != 1 || got[0] != evtE4 {
		t.Fatalf("delivery resumes AT to_id inclusive; got %v", got)
	}
	requireNoSinkErrors(t, errs)
}

// telemetryClass reads the retention class the platform actually stamps on an
// operational telemetry schema, so this file never writes the class string down
// itself.
func telemetryClass(t *testing.T) string {
	t.Helper()
	_, retention, ok := events.ClassFor(events.SchemaBoxVitals)
	if !ok {
		t.Fatal("box.vitals must carry a pinned retention class")
	}
	return retention
}
