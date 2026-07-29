package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
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

// TestClockFloorNowTracksTheWallClockAboveTheFloor is the OTHER half of
// SEC-066's clamp, and the half nothing else in this tree asserts.
//
// Now() is max(wall, floor). Every other test here, and the frozen SEC-066 case,
// exercises the side where the floor WINS — a host clock rolled back below the
// floor. Nothing exercised the side where the wall wins, which meant a Now()
// that simply returned the floor whenever one was established passed the entire
// corpus and the entire package. That mutation is not a subtle one: with the
// auth store now opened on this clock (cmd/waiveo-feeder), it freezes the app's
// notion of time at the instant of the last verified advance forever — every
// grant issued at the same millisecond, every ttl measured from a clock that
// never moves, every TOTP step pinned to one window, and every audit record
// stamped with the same time.
//
// The floor is a LOWER BOUND, never a substitute for the clock.
func TestClockFloorNowTracksTheWallClockAboveTheFloor(t *testing.T) {
	dir := t.TempDir()
	wall := int64(5_000)
	floor, err := OpenClockFloor(dir, func() int64 { return wall })
	if err != nil {
		t.Fatalf("OpenClockFloor: %v", err)
	}
	if advanced, err := floor.Advance(10_000, TimeSourceVerifiable); err != nil || !advanced {
		t.Fatalf("seed the floor: advanced=%v err=%v", advanced, err)
	}
	// Below the floor: the floor wins (the case the frozen corpus covers).
	if got := floor.Now(); got != 10_000 {
		t.Fatalf("Now() = %d with wall below the floor, want the floor 10000", got)
	}

	// At and above the floor: the WALL wins, and keeps winning as it advances.
	for _, tc := range []struct{ wallMs, want int64 }{
		{10_000, 10_000},
		{10_001, 10_001},
		{1 << 45, 1 << 45},
	} {
		wall = tc.wallMs
		if got := floor.Now(); got != tc.want {
			t.Errorf("Now() = %d with wall %d above a floor of 10000, want %d — the floor is a lower bound, not the clock",
				got, tc.wallMs, tc.want)
		}
	}

	// And the floor did not quietly ratchet up to follow the wall: only a
	// verifiable Advance moves it (SEC-067), so a bare host reading must leave it
	// exactly where it was however far ahead the host has run.
	if got := floor.FloorMs(); got != 10_000 {
		t.Errorf("floor = %d after Now() read a much later wall clock, want 10000 — Now must not launder a wall reading into the floor", got)
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

// TestDestroyLocalAuthStateDestroysTheClockFloor is the assertion behind the
// lifecycle claim clockfloor.go makes: "a factory reset that destroys the
// credential store has no business leaving a clock floor behind". Before this
// existed the sentence was true of nothing — the reset removed every row and
// left clock-floor.json exactly where it was, so a re-provisioned box inherited
// the previous owner's ratchet.
func TestDestroyLocalAuthStateDestroysTheClockFloor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	floor, err := OpenClockFloor(dir, func() int64 { return 1_000 })
	if err != nil {
		t.Fatalf("OpenClockFloor: %v", err)
	}
	if advanced, err := floor.Advance(9_000_000, TimeSourceVerifiable); err != nil || !advanced {
		t.Fatalf("seed the floor: advanced=%v err=%v", advanced, err)
	}
	path := filepath.Join(dir, clockFloorFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the floor was not persisted before the reset: %v", err)
	}

	st, err := Open(":memory:", floor.Now, ulid.New, WithClockFloor(floor))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if _, err := st.CreatePrincipal(ctx, KindUser, "someone"); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}

	if err := st.DestroyLocalAuthState(ctx); err != nil {
		t.Fatalf("DestroyLocalAuthState: %v", err)
	}

	if got := floor.FloorMs(); got != 0 {
		t.Errorf("floor = %d after a factory reset, want 0", got)
	}
	if got := floor.Assessment(); got != ClockUntrusted {
		t.Errorf("assessment = %q after a factory reset, want untrusted", got)
	}
	// The FILE, not merely the in-memory value: the next boot reads this
	// directory, and a box handed on with the previous owner's floor still on
	// disk is clamped to their clock with no reachable way down (the console
	// reset verb, SEC-075, has no transport in this tree).
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("clock-floor.json survived a factory reset (stat err = %v)", err)
	}
	// And the rows really went, so this is a reset that also destroyed the floor
	// rather than a floor reset wearing a reset's name.
	if n, err := st.CountPrincipals(ctx); err != nil || n != 0 {
		t.Errorf("principals after the reset = %d (err %v), want 0", n, err)
	}
}

// TestDestroyLocalAuthStateWithNoFloorWired proves the floor step is optional
// rather than required: a store opened without WithClockFloor still destroys its
// rows instead of failing on a nil floor.
func TestDestroyLocalAuthStateWithNoFloorWired(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:", func() int64 { return 1_000 }, ulid.New)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if _, err := st.CreatePrincipal(ctx, KindUser, "someone"); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if err := st.DestroyLocalAuthState(ctx); err != nil {
		t.Fatalf("DestroyLocalAuthState with no floor wired: %v", err)
	}
	if n, err := st.CountPrincipals(ctx); err != nil || n != 0 {
		t.Errorf("principals after the reset = %d (err %v), want 0", n, err)
	}
}
