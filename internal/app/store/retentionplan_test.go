package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/events"
)

// retentionplan_test.go covers the half of the retention sweep that had never
// been asked for: what it WOULD do.
//
// `waiveo-feeder -store-check` promises an operator "what does the next boot
// change in this store", and the boot's first act on the event log is Prune. The
// check reported the RETAINED counts and nothing else, so exit 0 — "every section
// ran, every section answered, and the next boot changes nothing in this store" —
// printed over a store the boot was about to delete rows from.

// planStore opens a store whose event log runs on a clock the test controls.
func planStore(t *testing.T, policy events.RetentionPolicy, nowMs int64) (*Store, *EventLog) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.db")
	s, err := Open(path, func() int64 { return nowMs })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	log, err := s.EventLog(policy, func() int64 { return nowMs }, func(error) {})
	if err != nil {
		t.Fatalf("EventLog: %v", err)
	}
	return s, log
}

// storeEvent writes one event straight into the table, bypassing Append.
//
// Deliberately not through Append: that path enforces the appended class's row
// cap on every insert, so a store built through it can never be over its cap and
// the sweep's cap pass would have nothing to act on. A box that has been running
// arrives at these shapes through a policy change, a build that knew a different
// cap, or simply the passage of time past a window — none of which go through an
// append.
func storeEvent(t *testing.T, s *Store, id, class string, tsMs int64) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO events (id, schema, ts, scope_node, trace_id, cost_class, retention_class, origin, origin_principal, payload)
		 VALUES (?, 'automation.run/1', ?, '', '', '', ?, '', '', '{}')`, id, tsMs, class); err != nil {
		t.Fatalf("store event %s: %v", id, err)
	}
}

// TestPlanPruneMatchesWhatPruneDoes is the anti-drift assertion, and the reason
// the sweep was restructured into cuts: the plan and the act are the same code,
// so a report that says "the next boot deletes N" cannot be wrong about N.
//
// The fixture exercises BOTH passes at once and the interaction between them,
// which is where a plan written as a second implementation goes wrong: the window
// pass and the row cap overlap, and a plan that ran both over the untrimmed table
// would count the rows both match twice.
func TestPlanPruneMatchesWhatPruneDoes(t *testing.T) {
	const now = int64(1787130000000)
	policy := events.NewRetentionPolicy(map[string]events.ClassRetention{
		// Both a window and a cap, deliberately tight, so both fire on one class.
		"telemetry-standard": {Window: 7 * 24 * time.Hour, MaxRows: 4},
		"audit-long":         {Window: 400 * 24 * time.Hour, MaxRows: 0},
	}, events.ClassRetention{Window: 400 * 24 * time.Hour, MaxRows: 4096})
	s, log := planStore(t, policy, now)

	day := int64(24 * time.Hour / time.Millisecond)
	// Four past the window, eight inside it — so the window takes 4 and the cap
	// then takes 4 of the 8 SURVIVORS, for 8 total. A plan that ran both passes
	// over the untrimmed table would double-count the rows both match and say
	// 4 + (12 - 4) = 12; a plan blind to the cap would say 4.
	for i := 0; i < 4; i++ {
		storeEvent(t, s, fmt.Sprintf("01J9EXP1RED%015d", i), "telemetry-standard", now-30*day)
	}
	for i := 0; i < 8; i++ {
		storeEvent(t, s, fmt.Sprintf("01J9FRESH00%015d", i), "telemetry-standard", now-1*day)
	}
	// Uncapped and inside its window: the sweep must leave the audit tier alone.
	storeEvent(t, s, "01J9AUD1T000000000000000A", "audit-long", now-day)

	plan, err := log.PlanPrune()
	if err != nil {
		t.Fatalf("PlanPrune: %v", err)
	}
	if plan.AtMs != now {
		t.Fatalf("the plan was judged at %d, want the log's own clock %d", plan.AtMs, now)
	}

	before, _, err := log.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	watermarkBefore := log.EvictedThrough()

	pruned, err := log.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if plan.Rows != pruned.Rows {
		t.Fatalf("the plan said %d row(s) and the sweep removed %d — the report and the boot disagree about the same store",
			plan.Rows, pruned.Rows)
	}
	if len(plan.ByClass) != len(pruned.ByClass) {
		t.Fatalf("plan by class %v, sweep by class %v", plan.ByClass, pruned.ByClass)
	}
	for class, n := range pruned.ByClass {
		if plan.ByClass[class] != n {
			t.Fatalf("class %q: plan %d, sweep %d", class, plan.ByClass[class], n)
		}
	}
	// EvictedThrough is the log's MONOTONIC watermark after the sweep, so the
	// relation to a plan is "the higher of what was already evicted and what this
	// sweep takes" — not equality, which would be an accident of a fresh log.
	wantWatermark := plan.EvictsThrough
	if watermarkBefore > wantWatermark {
		wantWatermark = watermarkBefore
	}
	if pruned.EvictedThrough != wantWatermark {
		t.Fatalf("the plan named %q as the highest id it would evict (watermark was %q) and the sweep left the watermark at %q",
			plan.EvictsThrough, watermarkBefore, pruned.EvictedThrough)
	}
	if plan.EvictsThrough != "01J9FRESH00000000000000003" {
		t.Fatalf("the plan's highest eviction is %q; the cap's fourth survivor-by-id is 01J9FRESH00000000000000003",
			plan.EvictsThrough)
	}
	// And the arithmetic itself, so "they agree" cannot be satisfied by two
	// planners that are wrong in the same way.
	if pruned.Rows != 8 {
		t.Fatalf("the sweep removed %d row(s), want 8 (4 past the window, then 4 over a cap of 4 among the 8 survivors)", pruned.Rows)
	}
	after, _, err := log.Count()
	if err != nil {
		t.Fatalf("Count after: %v", err)
	}
	if before-after != pruned.Rows {
		t.Fatalf("the log went from %d to %d events over a sweep reporting %d", before, after, pruned.Rows)
	}
	t.Logf("planned %d %v through %s; swept %d %v through %s",
		plan.Rows, plan.ByClass, plan.EvictsThrough, pruned.Rows, pruned.ByClass, pruned.EvictedThrough)
}

// TestPlanPruneChangesNothing: the plan runs on the path `-store-check` uses, and
// that path must not be able to delete a single event. It is asserted over the
// database file's bytes, not over a count, because a count is what a sweep would
// leave looking right if it also rewrote the watermark.
func TestPlanPruneChangesNothing(t *testing.T) {
	const now = int64(1787130000000)
	path := filepath.Join(t.TempDir(), "ro.db")
	s, err := Open(path, func() int64 { return now })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	log, err := s.EventLog(events.DefaultRetentionPolicy(), func() int64 { return now }, func(error) {})
	if err != nil {
		t.Fatalf("EventLog: %v", err)
	}
	day := int64(24 * time.Hour / time.Millisecond)
	for i := 0; i < 3; i++ {
		storeEvent(t, s, fmt.Sprintf("01J9EXP1RED%015d", i), "telemetry-standard", now-30*day)
	}
	_ = log
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	ro, err := OpenReadOnly(path, func() int64 { return now })
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	roLog, err := ro.EventLog(events.DefaultRetentionPolicy(), func() int64 { return now }, func(error) {})
	if err != nil {
		t.Fatalf("EventLog read-only: %v", err)
	}
	plan, err := roLog.PlanPrune()
	if err != nil {
		t.Fatalf("PlanPrune over a read-only handle: %v", err)
	}
	if plan.Rows != 3 {
		t.Fatalf("plan = %d row(s), want 3", plan.Rows)
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("Close read-only: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(body) != string(after) {
		t.Fatalf("planning the sweep changed the store file (%d byte(s) -> %d)", len(body), len(after))
	}
}

// TestReadOnlyCloseDoesNotCheckpoint: OpenReadOnly's contract is that it cannot
// change anything about the store, and Close fired `PRAGMA wal_checkpoint
// (TRUNCATE)` at it regardless — a write, to the `-wal` of a database this
// process does not own, potentially while a live feeder is mid-transaction.
//
// It fails on a `mode=ro` handle and the error was discarded, so it looked
// harmless. It was observable: every such close moved the store's `PRAGMA
// data_version` on an unrelated connection — SQLite's "somebody committed"
// counter — which made -store-check's torn-read witness accuse the store of
// moving under a report whose only writer was the report itself.
func TestReadOnlyCloseDoesNotCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quiet.db")
	s, err := Open(path, WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	witness, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("witness open: %v", err)
	}
	witness.SetMaxOpenConns(1)
	defer witness.Close()
	dataVersion := func() int64 {
		t.Helper()
		var v int64
		if err := witness.QueryRowContext(context.Background(), `PRAGMA data_version`).Scan(&v); err != nil {
			t.Fatalf("data_version: %v", err)
		}
		return v
	}

	before := dataVersion()
	for i := 0; i < 3; i++ {
		ro, err := OpenReadOnly(path, WallClockMs)
		if err != nil {
			t.Fatalf("OpenReadOnly %d: %v", i, err)
		}
		if err := ro.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
	if after := dataVersion(); after != before {
		t.Fatalf("three read-only open/close cycles moved data_version %d -> %d; a read-only handle must not "+
			"write to the store, and a diagnostic that does cannot then report on who did", before, after)
	}
}
