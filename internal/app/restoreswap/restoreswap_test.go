package restoreswap

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The property under test throughout is archive/1 ARC-107: a restore that fails
// after manifest validation leaves the destination in its pre-restore state.
// Staging makes that true by construction rather than by a rollback step, and
// these drive the construction — including from each point a crash can land in.

const (
	liveContent   = "the store that is serving"
	stagedContent = "the store the restore built"
)

// swapDir returns a directory holding a live store, and its path.
func swapDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	live := filepath.Join(dir, "app.db")
	if err := os.WriteFile(live, []byte(liveContent), 0o600); err != nil {
		t.Fatalf("seed live store: %v", err)
	}
	return live
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func writeStaged(content string) func(string) error {
	return func(p string) error { return os.WriteFile(p, []byte(content), 0o600) }
}

// TestAStagedRestoreIsAdoptedAtTheNextBoot is the whole happy path.
func TestAStagedRestoreIsAdoptedAtTheNextBoot(t *testing.T) {
	live := swapDir(t)

	if Pending(live) {
		t.Fatal("a fresh directory reports a pending restore")
	}
	if err := Stage(live, writeStaged(stagedContent)); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	// Staging must not have touched what is serving.
	if got := read(t, live); got != liveContent {
		t.Fatalf("staging changed the LIVE store to %q — nothing may be replaced until everything has succeeded "+
			"(ARC-107)", got)
	}
	if !Pending(live) {
		t.Fatal("a completed staging does not report as pending")
	}

	adopted, err := Adopt(live)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !adopted {
		t.Fatal("Adopt reported no swap on a directory with a pending one")
	}
	if got := read(t, live); got != stagedContent {
		t.Errorf("after adoption the live store is %q, want the restored one", got)
	}
	_, _, previous, marker := Paths(live)
	if got := read(t, previous); got != liveContent {
		t.Errorf("the pre-restore copy is %q, want the store that was serving — a destination that boots badly on "+
			"restored data must still have the bytes it had before", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Error("the pending marker survived adoption, so the next boot would try to adopt again")
	}
}

// TestAFailedStagingLeavesTheLiveStoreUntouched is ARC-107 for the case the
// rollback guarantee is actually about: the restore failed partway.
func TestAFailedStagingLeavesTheLiveStoreUntouched(t *testing.T) {
	live := swapDir(t)
	wantErr := errors.New("the archive ran out halfway")

	err := Stage(live, func(p string) error {
		// A real failure writes some bytes first, then gives up.
		if err := os.WriteFile(p, []byte("half a stor"), 0o600); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Stage error = %v, want it to wrap the writer's own", err)
	}
	if got := read(t, live); got != liveContent {
		t.Errorf("a failed restore changed the live store to %q", got)
	}
	if Pending(live) {
		t.Error("a FAILED staging left a pending marker — the next boot would adopt a store the restore never finished")
	}
	_, staged, _, _ := Paths(live)
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Error("a failed staging left its partial file behind for an operator to puzzle over")
	}
}

// TestACrashBeforeTheMarkerAdoptsNothing is the first crash point: the staged
// store is complete on disk, and the process died before marking it.
//
// It must be inert. The marker is the only signal that a staging finished, and
// a complete-looking file without one is indistinguishable from no restore —
// which is correct, because none completed.
func TestACrashBeforeTheMarkerAdoptsNothing(t *testing.T) {
	live := swapDir(t)
	_, staged, _, _ := Paths(live)
	if err := os.WriteFile(staged, []byte(stagedContent), 0o600); err != nil {
		t.Fatalf("simulate a staged store: %v", err)
	}

	adopted, err := Adopt(live)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if adopted {
		t.Fatal("a staged store with no marker was adopted — an interrupted restore would go live")
	}
	if got := read(t, live); got != liveContent {
		t.Errorf("the live store became %q", got)
	}
}

// TestACrashAfterTheLiveStoreMovedAsideFinishesTheSwap is the second crash
// point: Adopt renamed live away and died before renaming staged in.
func TestACrashAfterTheLiveStoreMovedAsideFinishesTheSwap(t *testing.T) {
	live := swapDir(t)
	if err := Stage(live, writeStaged(stagedContent)); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	_, _, previous, _ := Paths(live)
	if err := os.Rename(live, previous); err != nil {
		t.Fatalf("simulate the first rename: %v", err)
	}

	adopted, err := Adopt(live)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !adopted {
		t.Fatal("a half-completed swap was not finished")
	}
	if got := read(t, live); got != stagedContent {
		t.Errorf("live = %q, want the restored store", got)
	}
	if got := read(t, previous); got != liveContent {
		t.Errorf("pre-restore copy = %q, want the store that was serving", got)
	}
}

// TestACrashAfterTheSwapOnlyClearsTheMarker is the third crash point: the
// rename pair completed and the marker removal did not.
//
// The adoption already happened, so this must not swap anything a second time —
// it would put the RESTORED store into the pre-restore slot and leave nothing
// live.
func TestACrashAfterTheSwapOnlyClearsTheMarker(t *testing.T) {
	live := swapDir(t)
	_, _, previous, marker := Paths(live)
	// The state after both renames: live holds the restored bytes, previous
	// holds what was serving, marker is still there.
	if err := os.WriteFile(previous, []byte(liveContent), 0o600); err != nil {
		t.Fatalf("seed previous: %v", err)
	}
	if err := os.WriteFile(live, []byte(stagedContent), 0o600); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	if err := os.WriteFile(marker, []byte("pending\n"), 0o600); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	adopted, err := Adopt(live)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !adopted {
		t.Error("an already-completed swap reported as not adopted")
	}
	if got := read(t, live); got != stagedContent {
		t.Errorf("live = %q — a second swap ran and displaced the restored store", got)
	}
	if got := read(t, previous); got != liveContent {
		t.Errorf("previous = %q — the pre-restore copy was overwritten by a second swap", got)
	}
	if Pending(live) {
		t.Error("the stale marker survived, so every subsequent boot would repeat this")
	}
}

// TestAdoptIsIdempotent: running it twice is running it once. A boot loop must
// not walk the store backwards one rename at a time.
func TestAdoptIsIdempotent(t *testing.T) {
	live := swapDir(t)
	if err := Stage(live, writeStaged(stagedContent)); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	for i := range 3 {
		if _, err := Adopt(live); err != nil {
			t.Fatalf("Adopt #%d: %v", i+1, err)
		}
	}
	if got := read(t, live); got != stagedContent {
		t.Errorf("after three Adopts the live store is %q", got)
	}
	_, _, previous, _ := Paths(live)
	if got := read(t, previous); got != liveContent {
		t.Errorf("after three Adopts the pre-restore copy is %q — repeated adoption walked the stores around", got)
	}
}

// TestAMarkerWithNeitherStoreIsRefusedRatherThanGuessed pins the one state that
// is not recoverable. Choosing a side would mean guessing which store an
// operator meant to be running, and the pre-restore copy is on disk for them.
func TestAMarkerWithNeitherStoreIsRefusedRatherThanGuessed(t *testing.T) {
	live := swapDir(t)
	_, _, _, marker := Paths(live)
	if err := os.Remove(live); err != nil {
		t.Fatalf("remove live: %v", err)
	}
	if err := os.WriteFile(marker, []byte("pending\n"), 0o600); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	if _, err := Adopt(live); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Adopt error = %v, want ErrIncomplete", err)
	}
}

// TestDiscardLeavesTheLiveStoreServing covers an operator abandoning a staged
// restore, and covers the removal ORDER: the marker must go first, so an
// interrupted discard leaves an inert staged file rather than a marker pointing
// at a store that is gone.
func TestDiscardLeavesTheLiveStoreServing(t *testing.T) {
	live := swapDir(t)
	if err := Stage(live, writeStaged(stagedContent)); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := Discard(live); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if Pending(live) {
		t.Error("a discarded restore still reports as pending")
	}
	adopted, err := Adopt(live)
	if err != nil {
		t.Fatalf("Adopt after Discard: %v", err)
	}
	if adopted {
		t.Error("a discarded restore was adopted at the next boot")
	}
	if got := read(t, live); got != liveContent {
		t.Errorf("live = %q after a discard", got)
	}
}

// TestStagingTwiceUsesTheSecondAttempt: a retried restore must not adopt the
// remains of the attempt before it.
func TestStagingTwiceUsesTheSecondAttempt(t *testing.T) {
	live := swapDir(t)
	if err := Stage(live, writeStaged("the first attempt")); err != nil {
		t.Fatalf("first Stage: %v", err)
	}
	if err := Stage(live, writeStaged(stagedContent)); err != nil {
		t.Fatalf("second Stage: %v", err)
	}
	if _, err := Adopt(live); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := read(t, live); got != stagedContent {
		t.Errorf("live = %q, want the SECOND attempt's store", got)
	}
}

// TestARetriedStagingDoesNotInheritTheFirstAttemptsBytes pins that Stage clears
// a previous attempt before calling the writer.
//
// TestStagingTwiceUsesTheSecondAttempt above cannot see this, and the reason is
// worth writing down: it stages with os.WriteFile, which truncates, so the
// second attempt overwrites the first whether or not anything cleared it. The
// writer is CALLER-SUPPLIED, and a caller that opens the path without O_TRUNC —
// or streams into it — would otherwise adopt a store made of the new bytes
// followed by the tail of an abandoned one, which is a valid-looking file that
// no restore ever produced.
func TestARetriedStagingDoesNotInheritTheFirstAttemptsBytes(t *testing.T) {
	live := swapDir(t)
	const long = "a first attempt whose store is considerably longer than the second"
	const short = "the second attempt"

	// A writer that appends rather than truncating, which Stage must render
	// harmless by removing the path first.
	appendWriter := func(content string) func(string) error {
		return func(p string) error {
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.WriteString(content)
			return err
		}
	}

	if err := Stage(live, appendWriter(long)); err != nil {
		t.Fatalf("first Stage: %v", err)
	}
	if err := Stage(live, appendWriter(short)); err != nil {
		t.Fatalf("second Stage: %v", err)
	}
	if _, err := Adopt(live); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := read(t, live); got != short {
		t.Errorf("the adopted store is %q, want exactly %q — a retried restore inherited bytes from the attempt "+
			"before it, producing a store no restore ever wrote", got, short)
	}
}
