package store_test

import (
	"context"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/events"
)

// clock_test.go defends the ONE property Open's required clock argument exists
// to establish: every stamp this store writes comes from the clock its opener
// named, and nothing in the package reaches for the host's reading beside it.
//
// It matters because of what a deployment passes there. The app's persisted
// monotonic clock floor (internal/app/auth.ClockFloor, security-model/1
// SEC-066) reports max(host wall clock, persisted floor) — so while the floor is
// 0 it is indistinguishable from time.Now, and a store that quietly read the host
// clock would look correct in every test and on every box that has never
// established a floor. The two readings separate the instant a floor is
// established above the host clock, and separate in the worst direction: a
// resource row stamped BEHIND the credential row and the audit event written for
// the same request.
//
// So these cases pass a clock whose reading could not have come from the host —
// far enough from any plausible time.Now that a regression to a hardcoded wall
// clock cannot coincidentally satisfy them.

// floorClampedMs is a reading well above any wall clock this test could observe:
// it is what ClockFloor.Now returns once a floor has been established above the
// host's reading. Roughly the year 4000 in epoch milliseconds.
const floorClampedMs int64 = 64_060_588_800_000

// TestOpenRefusesWithoutAClock: the clock is REQUIRED, not defaulted. There is
// deliberately no wall-clock fallback — a default that silently reads the host
// clock is exactly the defect the argument was added to remove, and a seam
// nobody is forced to fill is a seam that stays unfilled.
func TestOpenRefusesWithoutAClock(t *testing.T) {
	s, err := store.Open(":memory:", nil)
	if err == nil {
		_ = s.Close()
		t.Fatal("Open with a nil clock succeeded; a store that can be opened without naming its clock will eventually be opened without one")
	}
}

// TestCreateStampsBaselineFromTheInjectedClock: a row's created_at/updated_at
// baseline is the opener's reading, not the host's.
//
// This is the case that fails if store.Open ever goes back to hardcoding
// time.Now: the injected reading is centuries away from any wall clock, so a
// host-clock stamp cannot accidentally match it.
func TestCreateStampsBaselineFromTheInjectedClock(t *testing.T) {
	reading := floorClampedMs
	s, err := store.Open(":memory:", func() int64 { return reading })
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	res, err := s.Create(ctx, store.KindScopeNode, mustJSON(t, siteNode()))
	if err != nil {
		t.Fatalf("create site node: %v", err)
	}
	if res.CreatedAt != floorClampedMs {
		t.Errorf("created_at = %d, want the injected reading %d — the store stamped from a clock its opener did not choose",
			res.CreatedAt, floorClampedMs)
	}
	if res.UpdatedAt != floorClampedMs {
		t.Errorf("updated_at = %d, want the injected reading %d", res.UpdatedAt, floorClampedMs)
	}

	// And the stamp is READ from the clock per write, not captured once at Open:
	// a floor that advances mid-process must move the next row's stamp with it.
	reading = floorClampedMs + 60_000
	screen := screenNode()
	res2, err := s.Create(ctx, store.KindScopeNode, mustJSON(t, screen))
	if err != nil {
		t.Fatalf("create screen node: %v", err)
	}
	if res2.CreatedAt != reading {
		t.Errorf("created_at after the clock advanced = %d, want %d — the store cached a reading instead of taking one",
			res2.CreatedAt, reading)
	}

	// An UPDATE re-stamps updated_at from the same clock and leaves created_at
	// where the create put it.
	reading = floorClampedMs + 120_000
	renamed := screen
	renamed.Name = "Renamed Screen"
	updated, err := s.Update(ctx, store.KindScopeNode, screen.ID, 1, mustJSON(t, renamed))
	if err != nil {
		t.Fatalf("update screen node: %v", err)
	}
	if updated.UpdatedAt != reading {
		t.Errorf("updated_at after update = %d, want the injected reading %d", updated.UpdatedAt, reading)
	}
	if updated.CreatedAt != floorClampedMs+60_000 {
		t.Errorf("created_at after update = %d, want the create's reading %d", updated.CreatedAt, floorClampedMs+60_000)
	}
}

// TestEventLogInheritsTheStoresClock: the durable event log rides this store, so
// a nil clock at OpenEventLog inherits the store's rather than falling back to
// the host's. It used to fall back to a bare time.Now, which made a nil a silent
// third clock in a process — and this log's retention decides how long the
// store's own audit trail survives, so measuring that against a different
// reading than the events were stamped with measures a skew rather than an age.
func TestEventLogInheritsTheStoresClock(t *testing.T) {
	s, err := store.Open(":memory:", func() int64 { return floorClampedMs })
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	log, err := s.EventLog(events.DefaultRetentionPolicy(), nil, func(error) {})
	if err != nil {
		t.Fatalf("EventLog: %v", err)
	}
	// One audit record stamped at the HOST's reading. The audit tier's window is
	// 400 days, so this record is well inside it when measured against the host
	// clock and roughly two thousand years past it when measured against the
	// store's. The sweep must therefore retire it — and the ONLY way it can is by
	// having inherited the store's clock. A fallback to time.Now would keep the
	// record and this case would fail.
	log.Append(auditEvent(t, evtE1, store.WallClockMs()))

	pruned, err := log.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned.Rows != 1 {
		t.Fatalf("the retention sweep retired %d row(s), want 1 — a nil clock at OpenEventLog fell back to the host's reading instead of the store's", pruned.Rows)
	}
}
