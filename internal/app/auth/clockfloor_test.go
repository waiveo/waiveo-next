package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// clockfloor_test.go covers the halves of the app-tier clock floor
// (SEC-066-068) the FROZEN CORPUS does not reach.
//
// It is deliberately complementary rather than duplicative: the
// conformance/drivers/securitymodel1 driver already exercises every behavior a
// corpus case declares, and re-asserting those here would only mean two places
// to update. What lives here is the behavior no case declares — an
// unauthenticated advance attempted at absurd magnitudes, a backwards verifiable
// reading, and the reset verb's effect on the persisted file.

// clockconsole_test.go covers the halves of the app-tier clock floor
// (SEC-066-068), the console binding's admission rule (SEC-072) and the
// credential-reset flow (SEC-052/053) that the FROZEN CORPUS does not reach.
//
// It is deliberately complementary rather than duplicative: the
// conformance/drivers/securitymodel1 driver already exercises every behavior a
// corpus case declares, and re-asserting those here would only mean two places
// to update. What lives here is the behavior no case declares — most
// importantly SEC-072's REFUSAL half, which the frozen corpus never exercises
// because it carries no `peer_uid: 1000` case.

func TestClockFloorRefusesAnUnauthenticatedAdvanceHoweverLarge(t *testing.T) {
	dir := t.TempDir()
	floor, err := OpenClockFloor(dir, func() int64 { return 1_000 })
	if err != nil {
		t.Fatalf("OpenClockFloor: %v", err)
	}
	if advanced, err := floor.Advance(5_000, TimeSourceVerifiable); err != nil || !advanced {
		t.Fatalf("seed the floor: advanced=%v err=%v", advanced, err)
	}
	// SEC-067's whole point: magnitude is not evidence. A claim decades in the
	// future from an unverifiable source is refused exactly as a small one is.
	for _, ts := range []int64{5_001, 1 << 40, 1 << 50} {
		advanced, err := floor.Advance(ts, TimeSourceUnauthenticated)
		if err != nil {
			t.Fatalf("Advance(%d, unauthenticated): %v", ts, err)
		}
		if advanced {
			t.Errorf("an unauthenticated claim of %d advanced the floor (SEC-067)", ts)
		}
	}
	if got := floor.FloorMs(); got != 5_000 {
		t.Errorf("floor = %d after three unauthenticated claims, want 5000", got)
	}
	if got := floor.Assessment(); got != ClockTrusted {
		t.Errorf("assessment = %q, want trusted — a verifiable value above the floor was accepted (SEC-068)", got)
	}
}

func TestClockFloorNeverMovesBackward(t *testing.T) {
	dir := t.TempDir()
	floor, err := OpenClockFloor(dir, func() int64 { return 1 })
	if err != nil {
		t.Fatalf("OpenClockFloor: %v", err)
	}
	if _, err := floor.Advance(9_000, TimeSourceVerifiable); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	// A LOWER verifiable reading is not news (SEC-066: "never moves backward").
	advanced, err := floor.Advance(8_999, TimeSourceVerifiable)
	if err != nil {
		t.Fatalf("Advance backwards: %v", err)
	}
	if advanced {
		t.Error("a verifiable value BELOW the floor reported an advance (SEC-066)")
	}
	if got := floor.FloorMs(); got != 9_000 {
		t.Errorf("floor = %d, want 9000", got)
	}
}

// TestClockFloorResetIsTheOnlyWayDown is SEC-075's clock-floor reset verb: the
// one sanctioned path that lowers a one-way ratchet, and the reason it exists
// (a single authenticated-but-wrong reading would otherwise strand a deployment
// in the future permanently).
func TestClockFloorResetIsTheOnlyWayDown(t *testing.T) {
	dir := t.TempDir()
	floor, err := OpenClockFloor(dir, func() int64 { return 1 })
	if err != nil {
		t.Fatalf("OpenClockFloor: %v", err)
	}
	if _, err := floor.Advance(1<<45, TimeSourceVerifiable); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if err := floor.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := floor.FloorMs(); got != 0 {
		t.Errorf("floor = %d after reset, want 0", got)
	}
	if got := floor.Assessment(); got != ClockUntrusted {
		t.Errorf("assessment = %q after reset, want untrusted (SEC-068: the reset verb resets it)", got)
	}
	// The persisted file went with it, so a restart does not resurrect the floor
	// the operator just reset.
	if _, err := os.Stat(filepath.Join(dir, clockFloorFile)); !os.IsNotExist(err) {
		t.Errorf("the persisted floor survived a reset (stat err = %v)", err)
	}
	reopened, err := OpenClockFloor(dir, func() int64 { return 42 })
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reopened.Now(); got != 42 {
		t.Errorf("Now() = %d after a reset and restart, want the bare host reading 42", got)
	}
}
