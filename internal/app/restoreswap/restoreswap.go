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
//
// # A store is a FILE SET, never one file
//
// The store this swaps runs in SQLite's WAL mode (internal/app/store's
// `_pragma=journal_mode(WAL)`), and a WAL-mode database on disk is up to three
// files: `<store>`, `<store>-wal` and `<store>-shm`. A clean shutdown
// checkpoints and unlinks the two companions, so an orderly box shows one file
// — which is exactly why treating the swap as a single-file rename survived
// review and every test: the tests closed their stores.
//
// The box a restore is FOR did not shut down cleanly. That is the whole reason
// somebody is restoring. On that box `<live>-wal` holds committed frames of the
// OLD workspace, and renaming a restored database over `<live>` while leaving
// them there does not restore anything: SQLite finds a WAL beside the database
// it just opened, recovers it, and replays the old workspace's frames on top of
// the freshly adopted one. The job reports success, the console says the
// restore landed, and the operator silently gets (part of) the workspace they
// were replacing. There is no error anywhere.
//
// So every operation here moves, clears and reasons about the whole SET, and
// two rules make that tractable:
//
//   - THE STAGED STORE IS ONE FILE. Stage refuses to mark a restore pending if
//     the writer left companions behind, because no crash-safe ordering exists
//     for carrying them (see Adopt). It is an invariant established at the only
//     place that can establish it, and it is what the next rule rests on.
//   - THE LIVE SLOT'S COMPANIONS ARE ALWAYS STALE AT ADOPT TIME. Given the rule
//     above, anything named `<live>-wal`/`<live>-shm` when Adopt runs belongs to
//     the store being replaced, so Adopt clears the slot unconditionally before
//     moving the staged store in.
//
// The pre-restore copy gets the live store's WAL CARRIED WITH IT rather than
// deleted, for the mirror-image reason: `<previous>` without `<previous>-wal`
// is a database rewound to its last checkpoint, so the safety copy an operator
// falls back to would be silently missing whatever the box did most recently —
// the same defect pointed the other way.
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

// sidecarSuffixes are SQLite's WAL-mode companions to a database file, in the
// order they must be handled: `-wal` carries COMMITTED TRANSACTIONS that are not
// yet in the database file, `-shm` is a rebuildable index over it.
//
// They are named here rather than in internal/app/store because this package is
// the one that MOVES store files around, and a mover that knows about only one
// of the three files is the defect this list exists to prevent. The names are
// SQLite's own derivation (database path + suffix) and are not configurable.
const (
	walSuffix = "-wal"
	shmSuffix = "-shm"
)

var sidecarSuffixes = [...]string{walSuffix, shmSuffix}

// ErrStagedStoreHasSidecars reports a staged store the writer did not leave as a
// single file — a `<staged>-wal` or `<staged>-shm` beside it.
//
// It is a refusal rather than something Adopt carries, and the reason is that no
// crash-safe ordering for carrying it exists. Moving `<staged>-wal` before
// `<staged>` and crashing in between leaves `<live>-wal` present with no
// `<live>`, which is byte-for-byte the state a crash midway through moving the
// OLD store aside leaves — and Adopt must delete the companions in that state
// and must not in this one. Two states that demand opposite actions and cannot
// be told apart is not a thing to resolve at boot; it is a thing to make
// impossible at staging time, while the live store is still untouched and the
// caller can simply be told to hand over a checkpointed file.
var ErrStagedStoreHasSidecars = errors.New("restoreswap: the staged store is not a single file — its writer left a SQLite -wal/-shm beside it, which cannot be adopted crash-safely; checkpoint the database and hand over one file")

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
//
// It returns ErrStagedStoreHasSidecars if the writer left a `-wal`/`-shm` beside
// the staged store — see that error for why this is the place that has to refuse.
func Stage(livePath string, write func(stagedPath string) error) error {
	_, staged, _, marker := Paths(livePath)
	// The WHOLE previous attempt, companions included. Removing only the
	// database file would leave an abandoned `<staged>-wal` for SQLite to
	// recover INTO the next attempt's fresh database — a staged store made of
	// this restore's rows plus the tail of a restore that was thrown away.
	if err := removeStoreFiles(staged); err != nil {
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
		_ = removeStoreFiles(staged)
		return fmt.Errorf("restoreswap: build the staged store: %w", err)
	}
	// The single-file invariant Adopt rests on, checked before the marker makes
	// any of this load-bearing.
	if leftover, found := firstSidecar(staged); found {
		_ = removeStoreFiles(staged)
		return fmt.Errorf("%w (found %s)", ErrStagedStoreHasSidecars, filepath.Base(leftover))
	}
	if err := fsyncPath(staged); err != nil {
		_ = removeStoreFiles(staged)
		return fmt.Errorf("restoreswap: fsync the staged store: %w", err)
	}
	// The marker is written last, and its presence is the ONLY signal that a
	// staged store is complete. Everything above this line is reversible by
	// deleting one file nobody is reading.
	if err := os.WriteFile(marker, []byte("pending\n"), 0o600); err != nil {
		_ = removeStoreFiles(staged)
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
	// Companions too: a `<staged>-wal` left behind here would be recovered into
	// the NEXT restore's staged database, which is the discarded workspace
	// leaking into the one that replaces it.
	if err := removeStoreFiles(staged); err != nil {
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
//
// # Where the WAL goes, and why in this order
//
// The live store's `-wal` is CARRIED to the pre-restore copy, and the live slot
// is then cleared of companions before anything moves into it. The order within
// the move-aside is `<live>` first, THEN `<live>-wal`, which looks backwards and
// is not:
//
//   - db-then-wal: a crash between them leaves `<live>` gone and `<live>-wal`
//     orphaned. The resume sees no live store, skips the move-aside entirely,
//     and the unconditional slot-clearing below deletes the orphan — so the
//     restored store is adopted onto a clean slot. Correct.
//   - wal-then-db: the same crash leaves `<live>` present with its WAL already
//     removed, i.e. a LIVE store silently rewound to its last checkpoint, which
//     the resume then promotes to the pre-restore copy. That is the safety copy
//     being corrupted by the safety mechanism.
//
// Both orders cost the same thing in the crash case — a pre-restore copy missing
// its most recent frames — and only one of them can also leave the adopted store
// poisoned. The `-shm` is never carried anywhere: it is a rebuildable index, and
// a stale one is the only kind of SQLite companion that is genuinely disposable.
//
// The `marker, no staged, live` branch deliberately touches NO companions. There
// the adoption already happened, so any `<live>-wal` is the RESTORED store's own,
// written since it went live — deleting it would be this same bug with the
// operands swapped.
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
		// Any earlier pre-restore copy is replaced, companions included: the
		// useful one is the store that was live immediately before THIS restore,
		// and an older attempt's `-wal` left in place would be recovered into
		// this attempt's pre-restore copy.
		if err := removeStoreFiles(previous); err != nil {
			return false, fmt.Errorf("restoreswap: clear an older pre-restore copy: %w", err)
		}
		if err := os.Rename(live, previous); err != nil {
			return false, fmt.Errorf("restoreswap: move the live store aside: %w", err)
		}
		// The WAL follows the database it belongs to, so the copy an operator
		// falls back to is the workspace as it actually was, not as it was at
		// the last checkpoint.
		if err := renameIfExists(live+walSuffix, previous+walSuffix); err != nil {
			return false, fmt.Errorf("restoreswap: carry the live store's write-ahead log to the pre-restore copy: %w", err)
		}
	}
	// The live slot must hold NOTHING before the staged store moves in. By
	// Stage's single-file invariant every companion here belongs to the store
	// being replaced (or is the orphan a crash mid-carry left), and SQLite would
	// recover one straight over the restored database — the whole defect.
	if err := removeSidecars(live); err != nil {
		return false, fmt.Errorf("restoreswap: clear the replaced store's write-ahead log: %w", err)
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

// firstSidecar reports the first SQLite companion present beside storePath.
func firstSidecar(storePath string) (string, bool) {
	for _, suffix := range sidecarSuffixes {
		if p := storePath + suffix; exists(p) {
			return p, true
		}
	}
	return "", false
}

// removeSidecars unlinks storePath's SQLite companions, if any.
func removeSidecars(storePath string) error {
	for _, suffix := range sidecarSuffixes {
		if err := os.Remove(storePath + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// removeStoreFiles unlinks a store: the database file and its companions.
//
// The database goes LAST. Every reader of these paths keys off the database
// file's presence, so removing it first would publish "this store is gone"
// while its `-wal` is still there to be recovered into whatever takes the name
// next.
func removeStoreFiles(storePath string) error {
	if err := removeSidecars(storePath); err != nil {
		return err
	}
	if err := os.Remove(storePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// renameIfExists moves from to to, and does nothing if from is not there.
func renameIfExists(from, to string) error {
	if !exists(from) {
		return nil
	}
	return os.Rename(from, to)
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
