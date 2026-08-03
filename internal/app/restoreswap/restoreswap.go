// Package restoreswap is the offline swap a restore applies through: a new
// store is built BESIDE the live one, and the process adopts it at its next
// boot rather than replacing anything while it is serving.
//
// # Why a swap and not an in-place apply
//
// A restore has to replace the destination's live relational store, put
// embedded assets into the CAS, install the pack lockfile and re-wrap secrets —
// and then, if anything fails after manifest validation, put all of it back
// exactly as it was (archive/1 ARC-107). Doing that in place means quiescing
// every in-flight request, SSE subscriber, relay connection and job runner,
// then doing it twice if the restore fails.
//
// Staging makes ARC-107 trivially true instead of carefully true: nothing the
// live workspace uses is touched until everything has already succeeded. A
// failed restore is not rolled back, because it was never applied. The cost is
// a restart and a brief unavailability, which is an acceptable price for a rare,
// deliberate operation.
//
// # The crash-safety argument
//
// Two files and one marker, in an order chosen so that every interruption lands
// in a state the next boot can name:
//
//	Stage:  write <staged>, fsync it, THEN write <marker>.
//	Adopt:  rename live -> previous, rename staged -> live, remove <marker>.
//
// The marker is written LAST in Stage, so a crash mid-write leaves a partial
// staged file with no marker — indistinguishable from no restore at all, which
// is exactly right, because none completed. The marker is removed LAST in
// Adopt, so a crash mid-adopt leaves the marker present and the next boot
// resumes from whichever half-state it finds. Adopt is therefore idempotent:
// running it twice on the same directory is the same as running it once.
//
// The live store is RENAMED aside rather than deleted, so a destination that
// boots badly on the restored data still has the bytes it had before. Nothing
// here removes that copy; reclaiming it is an operator decision, because the
// one moment it matters is the one moment nobody wants it silently gone.
package restoreswap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// File names, all siblings of the live store so a single directory holds the
// whole swap and an operator inspecting it sees the entire state.
const (
	stagedSuffix   = ".incoming"
	previousSuffix = ".pre-restore"
	markerSuffix   = ".restore-pending"
)

// ErrIncomplete reports a staging directory whose state no boot-time rule
// covers: the marker says a restore is pending, and neither the staged store
// nor the live one is present.
//
// It is an error rather than a silent recovery because the only ways to reach
// it are outside this package — something deleted a file mid-swap. Choosing a
// side would mean guessing which of two stores an operator meant to be running,
// and the pre-restore copy is still on disk for them to choose from.
var ErrIncomplete = errors.New("restoreswap: a restore is marked pending but neither the staged nor the live store is present")

// Paths returns the four paths a swap of livePath involves.
func Paths(livePath string) (live, staged, previous, marker string) {
	return livePath, livePath + stagedSuffix, livePath + previousSuffix, livePath + markerSuffix
}

// Stage builds the incoming store at the staging path via write, and — only if
// write returns nil — marks the restore pending.
//
// write receives the path it must produce and is responsible for its content.
// It is called with any previous staging attempt already removed, so a caller
// never appends to an abandoned one.
//
// The live store is not touched, on any path through this function. A caller
// whose write fails has changed nothing an operator can observe, which is what
// makes ARC-107's rollback guarantee hold without a rollback step.
func Stage(livePath string, write func(stagedPath string) error) error {
	_, staged, _, marker := Paths(livePath)
	if err := os.Remove(staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("restoreswap: clear a previous staging attempt: %w", err)
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("restoreswap: clear a previous pending marker: %w", err)
	}
	if err := write(staged); err != nil {
		// Leave nothing behind that a later boot could mistake for a completed
		// staging. There is no marker yet, so this is belt-and-braces rather
		// than load-bearing — but a half-written store sitting beside a live one
		// is a thing an operator will eventually find and have to reason about.
		_ = os.Remove(staged)
		return fmt.Errorf("restoreswap: build the staged store: %w", err)
	}
	if err := fsyncPath(staged); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("restoreswap: fsync the staged store: %w", err)
	}
	// The marker is written last, and its presence is the ONLY signal that a
	// staged store is complete. Everything above this line is reversible by
	// deleting one file nobody is reading.
	if err := os.WriteFile(marker, []byte("pending\n"), 0o600); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("restoreswap: mark the restore pending: %w", err)
	}
	return fsyncPath(filepath.Dir(marker))
}

// Pending reports whether a completed staging is waiting to be adopted.
func Pending(livePath string) bool {
	_, _, _, marker := Paths(livePath)
	_, err := os.Stat(marker)
	return err == nil
}

// Discard abandons a staged restore, leaving the live store as it is.
//
// The marker goes FIRST. A crash between the two removals leaves a staged file
// with no marker, which is inert; the reverse order would leave a marker
// pointing at a store that is no longer there, which is ErrIncomplete's
// unrecoverable state.
func Discard(livePath string) error {
	_, staged, _, marker := Paths(livePath)
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("restoreswap: remove the pending marker: %w", err)
	}
	if err := os.Remove(staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("restoreswap: remove the staged store: %w", err)
	}
	return nil
}

// Adopt completes a pending swap, and reports whether one was adopted.
//
// Call it at boot, before the store is opened. With no marker it does nothing
// and returns false, which is every ordinary boot.
//
// # Every state it can find, and why each resolves the way it does
//
//	marker, staged, live      → the ordinary case: live steps aside, staged
//	                            becomes live.
//	marker, staged, no live   → a crash after live was renamed aside. The
//	                            staged store is still the intended one, and the
//	                            previous copy is already safe under its own
//	                            name, so the swap simply finishes.
//	marker, no staged, live   → a crash after staged became live, before the
//	                            marker was removed. The adoption ALREADY
//	                            happened; only the marker is stale.
//	marker, no staged, no live → ErrIncomplete. Nothing here can tell which
//	                            store was meant to be running.
func Adopt(livePath string) (bool, error) {
	live, staged, previous, marker := Paths(livePath)
	if _, err := os.Stat(marker); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("restoreswap: read the pending marker: %w", err)
	}

	stagedExists := exists(staged)
	liveExists := exists(live)

	switch {
	case !stagedExists && !liveExists:
		return false, ErrIncomplete
	case !stagedExists && liveExists:
		// The swap already completed; clear the stale marker and report it as
		// adopted, because from a caller's point of view a restore did land.
		if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("restoreswap: clear the marker after an already-completed swap: %w", err)
		}
		return true, nil
	}

	if liveExists {
		// Any earlier pre-restore copy is replaced: the useful one is the store
		// that was live immediately before THIS restore, not one from an
		// attempt two restores ago that no longer matches anything.
		if err := os.Remove(previous); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("restoreswap: clear an older pre-restore copy: %w", err)
		}
		if err := os.Rename(live, previous); err != nil {
			return false, fmt.Errorf("restoreswap: move the live store aside: %w", err)
		}
	}
	if err := os.Rename(staged, live); err != nil {
		return false, fmt.Errorf("restoreswap: adopt the staged store: %w", err)
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("restoreswap: clear the pending marker: %w", err)
	}
	return true, fsyncPath(filepath.Dir(live))
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// fsyncPath flushes a file or directory to disk.
//
// The DIRECTORY fsync is the one that matters and the one that is easy to
// forget: on most filesystems a rename is not durable until the directory
// holding it is synced, so a power loss right after Adopt could otherwise
// resurrect the pre-restore layout with the marker already gone.
func fsyncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
