package identity

import (
	"path/filepath"
	"testing"
)

// TestClockFloorAdvancesOnlyNeverRegresses is the REL-130/132 monotonicity
// property: SetClockFloor persists a higher value and reports advanced=true,
// but a subsequent call with a LOWER value leaves the persisted floor
// unchanged and reports advanced=false — the store enforces "advance only,
// never regress" itself, independent of whatever the caller believes it has
// verified (REL-132's source-verification is the caller's own concern).
func TestClockFloorAdvancesOnlyNeverRegresses(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	advanced, err := store.SetClockFloor(1000)
	if err != nil {
		t.Fatalf("SetClockFloor(1000): %v", err)
	}
	if !advanced {
		t.Fatalf("SetClockFloor(1000) advanced = false, want true (first floor ever set)")
	}

	advanced, err = store.SetClockFloor(500)
	if err != nil {
		t.Fatalf("SetClockFloor(500): %v", err)
	}
	if advanced {
		t.Fatalf("SetClockFloor(500) advanced = true, want false (500 < persisted floor 1000)")
	}

	ms, ok, err := store.ClockFloor()
	if err != nil {
		t.Fatalf("ClockFloor(): %v", err)
	}
	if !ok || ms != 1000 {
		t.Fatalf("ClockFloor() = (%d, %v), want (1000, true) — a lower SetClockFloor call MUST NOT regress the floor", ms, ok)
	}
}

// TestClockFloorEqualValueDoesNotAdvance asserts that setting the SAME value
// the floor already holds is not treated as an advance either — REL-130's
// "advance only" means strictly greater, not greater-or-equal.
func TestClockFloorEqualValueDoesNotAdvance(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.SetClockFloor(750); err != nil {
		t.Fatalf("SetClockFloor(750): %v", err)
	}

	advanced, err := store.SetClockFloor(750)
	if err != nil {
		t.Fatalf("SetClockFloor(750) again: %v", err)
	}
	if advanced {
		t.Fatalf("SetClockFloor(750) advanced = true on an equal value, want false")
	}

	ms, ok, err := store.ClockFloor()
	if err != nil {
		t.Fatalf("ClockFloor(): %v", err)
	}
	if !ok || ms != 750 {
		t.Fatalf("ClockFloor() = (%d, %v), want (750, true)", ms, ok)
	}
}

// TestClockFloorSurvivesRestart is the REL-130 offline-continuity property:
// a floor persisted through one Store is read back, unchanged, through a
// fresh Store opened on the same path after a clean restart.
func TestClockFloorSurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	store1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

	if _, err := store1.SetClockFloor(1_752_800_000_000); err != nil {
		t.Fatalf("SetClockFloor: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	ms, ok, err := store2.ClockFloor()
	if err != nil {
		t.Fatalf("ClockFloor() after restart: %v", err)
	}
	if !ok || ms != 1_752_800_000_000 {
		t.Fatalf("ClockFloor() after restart = (%d, %v), want (1752800000000, true)", ms, ok)
	}
}

// TestFreshStoreHasNoClockFloor asserts a store that has never had
// SetClockFloor called reports ok=false, not a zero-value floor or an error
// — mirroring the ok-bool convention Identity/LastAppliedGeneration already
// use for "nothing persisted yet".
func TestFreshStoreHasNoClockFloor(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ms, ok, err := store.ClockFloor()
	if err != nil {
		t.Fatalf("ClockFloor() on a fresh store: %v", err)
	}
	if ok {
		t.Fatalf("ClockFloor() on a fresh store = (%d, true), want ok=false", ms)
	}
}
