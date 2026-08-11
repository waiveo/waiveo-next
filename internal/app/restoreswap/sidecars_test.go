package restoreswap

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	// The SAME driver internal/app/store opens with, registered under the same
	// name. The WAL behaviour this file is about belongs to the driver, so a
	// test that faked it would prove nothing about the box.
	_ "modernc.org/sqlite"
)

// The property under test throughout this file: a WAL-mode store is a FILE SET,
// and every operation in this package moves the whole set or none of it.
//
// These are separate from restoreswap_test.go because they are testing a
// different thing. Those tests drive ARC-107's rollback shape with plain text
// files, which is the right instrument for the state machine and is exactly why
// they could not see this: a text file has no companions, and a cleanly closed
// SQLite database has none either. The bug only exists on the box a restore is
// FOR — the one that did not shut down cleanly.

// walContents writes a recognisable `-wal` beside storePath.
//
// It is not a real WAL; for every test but the last one in this file that does
// not matter, because what is under test is which FILES move where. The real
// SQLite replay is proved at the bottom of this file.
func seedSidecars(t *testing.T, storePath, walBody string) {
	t.Helper()
	if err := os.WriteFile(storePath+"-wal", []byte(walBody), 0o600); err != nil {
		t.Fatalf("seed %s-wal: %v", filepath.Base(storePath), err)
	}
	if err := os.WriteFile(storePath+"-shm", []byte("shared-memory index"), 0o600); err != nil {
		t.Fatalf("seed %s-shm: %v", filepath.Base(storePath), err)
	}
}

func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still exists — %s", filepath.Base(path), why)
	}
}

// TestAdoptClearsTheReplacedStoresWriteAheadLog is THE regression test for the
// silent non-restore.
//
// The old Adopt renamed the database file and nothing else, so the replaced
// store's `-wal` stayed in the live slot. SQLite recovers a WAL it finds beside
// the database it opens, so the next boot replayed the OLD workspace's committed
// frames straight over the restored one — and reported a successful restore
// while doing it.
func TestAdoptClearsTheReplacedStoresWriteAheadLog(t *testing.T) {
	live := swapDir(t)
	seedSidecars(t, live, "committed frames of the workspace being REPLACED")

	if err := Stage(live, writeStaged(stagedContent)); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := Adopt(live); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	if got := read(t, live); got != stagedContent {
		t.Errorf("live = %q, want the restored store", got)
	}
	mustNotExist(t, live+"-wal", "SQLite will recover the REPLACED workspace's committed frames over the store just restored, and report success")
	mustNotExist(t, live+"-shm", "a stale shared-memory index beside a database that is not the one it indexes")
}

// TestThePreRestoreCopyKeepsItsOwnWriteAheadLog is the mirror direction, and it
// is here because clearing the live slot by DELETING the companions would pass
// the test above while quietly breaking this one.
//
// The pre-restore copy exists for one purpose: a destination that boots badly on
// the restored data still has the bytes it had before. A copy of the database
// file without its `-wal` is that workspace rewound to its last checkpoint — the
// same silent data loss, aimed at the fallback instead of at the restore.
func TestThePreRestoreCopyKeepsItsOwnWriteAheadLog(t *testing.T) {
	live := swapDir(t)
	const frames = "the last hour of authoring, committed but not yet checkpointed"
	seedSidecars(t, live, frames)

	if err := Stage(live, writeStaged(stagedContent)); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := Adopt(live); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	_, _, previous, _ := Paths(live)
	if got := read(t, previous); got != liveContent {
		t.Errorf("pre-restore copy = %q, want the store that was serving", got)
	}
	if got := read(t, previous+"-wal"); got != frames {
		t.Errorf("the pre-restore copy's -wal is %q, want %q — the fallback an operator reaches for is missing "+
			"everything the box did since its last checkpoint", got, frames)
	}
}

// TestAnOlderPreRestoreCopysWriteAheadLogIsCleared: the pre-restore slot is
// reused, and reusing it half-way is the same bug one slot over — an older
// attempt's `-wal` recovered into THIS attempt's pre-restore copy.
func TestAnOlderPreRestoreCopysWriteAheadLogIsCleared(t *testing.T) {
	live := swapDir(t)
	_, _, previous, _ := Paths(live)
	if err := os.WriteFile(previous, []byte("a store from two restores ago"), 0o600); err != nil {
		t.Fatalf("seed an older pre-restore copy: %v", err)
	}
	seedSidecars(t, previous, "frames belonging to a store nobody kept")

	if err := Stage(live, writeStaged(stagedContent)); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := Adopt(live); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	if got := read(t, previous); got != liveContent {
		t.Errorf("pre-restore copy = %q, want the store that was serving", got)
	}
	mustNotExist(t, previous+"-wal", "an older pre-restore copy's frames would be recovered into the one that just replaced it")
	mustNotExist(t, previous+"-shm", "a shared-memory index from a store two restores ago")
}

// TestACrashWhileCarryingTheWriteAheadLogStillAdoptsCleanly is the crash point
// the carry ORDER exists for: the database was renamed aside and the process
// died before its `-wal` followed.
//
// The resume sees no live store, so it skips the move-aside entirely — and the
// orphaned `<live>-wal` is exactly what would be recovered over the restored
// store if the slot were not cleared unconditionally.
func TestACrashWhileCarryingTheWriteAheadLogStillAdoptsCleanly(t *testing.T) {
	live := swapDir(t)
	if err := Stage(live, writeStaged(stagedContent)); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	_, _, previous, _ := Paths(live)
	// The exact state: rename(live -> previous) happened, the -wal carry did not.
	if err := os.Rename(live, previous); err != nil {
		t.Fatalf("simulate the first rename: %v", err)
	}
	if err := os.WriteFile(live+"-wal", []byte("frames of the store that is now the pre-restore copy"), 0o600); err != nil {
		t.Fatalf("simulate the orphaned -wal: %v", err)
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
	mustNotExist(t, live+"-wal", "the orphan from the interrupted carry would be replayed over the restored store")
}

// TestAnAlreadyAdoptedStoreKeepsItsOwnWriteAheadLog is the OTHER mirror, and it
// is the one an over-eager "just delete every -wal in the live slot" fix breaks.
//
// In this state the swap already completed and only the marker is stale, so
// anything beside the live store belongs to the RESTORED workspace — written
// since it went live. Deleting it is this same defect with the operands swapped.
func TestAnAlreadyAdoptedStoreKeepsItsOwnWriteAheadLog(t *testing.T) {
	live := swapDir(t)
	_, _, previous, marker := Paths(live)
	if err := os.WriteFile(previous, []byte(liveContent), 0o600); err != nil {
		t.Fatalf("seed previous: %v", err)
	}
	if err := os.WriteFile(live, []byte(stagedContent), 0o600); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	const restoredFrames = "work done on the RESTORED workspace since it went live"
	seedSidecars(t, live, restoredFrames)
	if err := os.WriteFile(marker, []byte("pending\n"), 0o600); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	if _, err := Adopt(live); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := read(t, live+"-wal"); got != restoredFrames {
		t.Errorf("the adopted store's own -wal is %q, want %q — a stale marker cost the restored workspace its "+
			"most recent committed frames", got, restoredFrames)
	}
}

// TestStageRefusesAStagedStoreThatIsNotOneFile pins the invariant Adopt's
// unconditional slot-clearing rests on. Without it, `<live>-wal` present with no
// `<live>` is ambiguous — see ErrStagedStoreHasSidecars.
func TestStageRefusesAStagedStoreThatIsNotOneFile(t *testing.T) {
	live := swapDir(t)

	err := Stage(live, func(p string) error {
		if err := os.WriteFile(p, []byte(stagedContent), 0o600); err != nil {
			return err
		}
		// A writer that handed over a database it never checkpointed.
		return os.WriteFile(p+"-wal", []byte("uncheckpointed frames of the RESTORED workspace"), 0o600)
	})
	if !errors.Is(err, ErrStagedStoreHasSidecars) {
		t.Fatalf("Stage error = %v, want ErrStagedStoreHasSidecars", err)
	}
	if Pending(live) {
		t.Error("a refused staging left a pending marker — the next boot would adopt a store this package cannot swap safely")
	}
	if got := read(t, live); got != liveContent {
		t.Errorf("a refused staging changed the live store to %q", got)
	}
	_, staged, _, _ := Paths(live)
	mustNotExist(t, staged, "a refused staging left its database behind")
	mustNotExist(t, staged+"-wal", "a refused staging left the -wal it was refused for")
}

// TestARetriedStagingClearsTheAbandonedAttemptsWriteAheadLog: Stage clears the
// whole previous attempt. Leaving a `<staged>-wal` would have SQLite recover a
// discarded restore's frames into the fresh database the next attempt writes —
// a staged store made of two different archives.
func TestARetriedStagingClearsTheAbandonedAttemptsWriteAheadLog(t *testing.T) {
	live := swapDir(t)
	_, staged, _, _ := Paths(live)
	if err := os.WriteFile(staged, []byte("an abandoned attempt"), 0o600); err != nil {
		t.Fatalf("seed an abandoned attempt: %v", err)
	}
	seedSidecars(t, staged, "frames of an archive nobody restored")

	if err := Stage(live, writeStaged(stagedContent)); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	mustNotExist(t, staged+"-wal", "the abandoned attempt's frames would be recovered into the new staged store")
	mustNotExist(t, staged+"-shm", "the abandoned attempt's shared-memory index")
}

// TestDiscardRemovesTheWholeStagedStore: same reasoning as the retry, reached
// through the operator's abandon button instead.
func TestDiscardRemovesTheWholeStagedStore(t *testing.T) {
	live := swapDir(t)
	if err := Stage(live, writeStaged(stagedContent)); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	_, staged, _, _ := Paths(live)
	seedSidecars(t, staged, "frames written after the staging completed")

	if err := Discard(live); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	mustNotExist(t, staged, "a discarded restore left its database")
	mustNotExist(t, staged+"-wal", "a discarded restore left frames for the next staging to inherit")
	mustNotExist(t, staged+"-shm", "a discarded restore left its shared-memory index")
}

// ── The real thing ──────────────────────────────────────────────────────────

// openLikeTheStore opens path with the SAME driver, DSN and pragmas
// internal/app/store.Open uses. Mirrored rather than imported so this package
// keeps depending on nothing above it; the pragmas are what make the file set
// exist at all, so a copy that drifted would silently stop testing WAL.
func openLikeTheStore(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+
		"?_txlock=immediate"+
		"&_pragma=journal_mode(WAL)"+
		"&_pragma=synchronous(FULL)"+
		"&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func workspaceIn(t *testing.T, path string) []string {
	t.Helper()
	db := openLikeTheStore(t, path)
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM workspace ORDER BY name`)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestARestoreOverAnUNCLEANLYClosedStoreActuallyRestores is the whole finding,
// end to end, with a real SQLite database and no simulation anywhere.
//
// The shape is the disaster-recovery path itself, which is the only path where
// this happens: the box was power-cut (or took cmd/waiveo-feeder's log.Fatal,
// which skips the deferred Close), so the live store's `-wal` holds committed
// frames nobody checkpointed. An operator restores a backup. Before the fix this
// test's final assertion read [LIVE-WORKSPACE] — the restore reported success,
// the console said it landed, and the box came back with the workspace the
// operator was replacing.
//
// The live connection is deliberately NEVER closed: closing it is what
// checkpoints and unlinks the companions, and a clean shutdown is precisely the
// case that never had the bug.
func TestARestoreOverAnUNCLEANLYClosedStoreActuallyRestores(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "feeder-store.db")

	// The live workspace, with committed-but-uncheckpointed frames.
	liveDB := openLikeTheStore(t, live)
	mustExec(t, liveDB, `CREATE TABLE workspace (name TEXT PRIMARY KEY)`)
	mustExec(t, liveDB, `INSERT INTO workspace (name) VALUES ('LIVE-WORKSPACE')`)
	if _, err := os.Stat(live + "-wal"); err != nil {
		t.Fatalf("no %s-wal after a WAL-mode write: this test cannot prove anything without one (%v)",
			filepath.Base(live), err)
	}
	// NO liveDB.Close(): the power went out.

	// The archived workspace, produced the way production produces it: SQLite's
	// own `VACUUM INTO` (internal/app/store's SnapshotInto — ARC-083 forbids a
	// raw file copy), whose bytes workspacerestore.go writes straight to the
	// staged path.
	//
	// The payload matters and this test used to get it wrong. A cleanly CLOSED
	// WAL-mode database has journal mode 2/2 in its header; `VACUUM INTO` output
	// is a ROLLBACK-mode database, 1/1. Both reproduce the finding, so the old
	// fixture was not wrong about the outcome — but the claim attached to it was
	// "proved with the real driver and pragmas", and a payload production never
	// produces cannot carry that claim. It also hid a second question: what a
	// rollback-mode file does when something leaves a stale `-journal` beside it
	// (see TestAdoptClearsEVERYSidecarBeforeTheStagedStoreMovesIn).
	archived := filepath.Join(dir, "archived.db")
	archDB := openLikeTheStore(t, archived)
	mustExec(t, archDB, `CREATE TABLE workspace (name TEXT PRIMARY KEY)`)
	mustExec(t, archDB, `INSERT INTO workspace (name) VALUES ('ARCHIVED-WORKSPACE')`)
	snapshotPath := filepath.Join(dir, "archived-snapshot.db")
	mustExec(t, archDB, `VACUUM INTO '`+snapshotPath+`'`)
	if err := archDB.Close(); err != nil {
		t.Fatalf("close the archived store: %v", err)
	}
	snapshot, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read the archived snapshot: %v", err)
	}
	// The header's journal-mode bytes (offsets 18 and 19) are 1/1 for a
	// rollback-mode database and 2/2 for a WAL one. Asserted so this test cannot
	// quietly go back to staging the wrong kind of file.
	if len(snapshot) < 20 || snapshot[18] != 1 || snapshot[19] != 1 {
		t.Fatalf("the staged snapshot's header journal mode is %d/%d, want 1/1 — this fixture is not `VACUUM INTO` output and so is not what a restore actually hands over",
			snapshot[18], snapshot[19])
	}

	if err := Stage(live, func(staged string) error { return os.WriteFile(staged, snapshot, 0o600) }); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	adopted, err := Adopt(live)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !adopted {
		t.Fatal("Adopt reported no swap")
	}

	got := workspaceIn(t, live)
	if len(got) != 1 || got[0] != "ARCHIVED-WORKSPACE" {
		t.Fatalf("after a restore the box is serving %v, want [ARCHIVED-WORKSPACE].\n"+
			"The staged database was renamed into place and the replaced store's -wal was left beside it, so SQLite "+
			"recovered the OLD workspace's committed frames over the restored one. Nothing failed: the job reported "+
			"succeeded and the operator lost the restore.", got)
	}

	// And the fallback is a whole database, not one rewound to its last
	// checkpoint: the frames that were in the live -wal are readable from the
	// pre-restore copy.
	_, _, previous, _ := Paths(live)
	if got := workspaceIn(t, previous); len(got) != 1 || got[0] != "LIVE-WORKSPACE" {
		t.Fatalf("the pre-restore copy holds %v, want [LIVE-WORKSPACE] — the copy an operator falls back to lost "+
			"everything committed since the last checkpoint", got)
	}
}

// TestTheAdoptedStoreIsCleanFromEitherCrashPoint replaces an ARGUMENT with a
// test, which is the whole reason it exists.
//
// Adopt's doc used to justify the db-then-wal move-aside order by claiming the
// reverse "leaves the adopted store poisoned". That claim was false — inverting
// the two lines passes every other test in this package, because
// `removeSidecars(live)` runs unconditionally afterwards — and a false argument
// in a comment is worse than none, because the next person weakens the real
// guarantee while carefully preserving the fake one.
//
// So this pins the guarantee that IS real: whichever of the two renames was
// interrupted, the store that ends up live is the restored one and nothing of
// the replaced workspace is left beside it to be recovered into it.
func TestTheAdoptedStoreIsCleanFromEitherCrashPoint(t *testing.T) {
	const replacedFrames = "committed frames of the workspace being REPLACED"
	cases := []struct {
		name string
		// crash puts the directory into the state that interruption leaves.
		crash func(t *testing.T, live, previous string)
	}{
		{
			name: "interrupted after the database moved, before its log did",
			crash: func(t *testing.T, live, previous string) {
				if err := os.Rename(live, previous); err != nil {
					t.Fatalf("simulate rename(live, previous): %v", err)
				}
				// The log is still in the live slot, orphaned.
			},
		},
		{
			name: "interrupted after the log moved, before the database did",
			crash: func(t *testing.T, live, previous string) {
				if err := os.Rename(live+"-wal", previous+"-wal"); err != nil {
					t.Fatalf("simulate rename(live-wal, previous-wal): %v", err)
				}
				// The database is still in the live slot, without its log.
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := swapDir(t)
			seedSidecars(t, live, replacedFrames)
			if err := Stage(live, writeStaged(stagedContent)); err != nil {
				t.Fatalf("Stage: %v", err)
			}
			_, _, previous, _ := Paths(live)
			tc.crash(t, live, previous)

			adopted, err := Adopt(live)
			if err != nil {
				t.Fatalf("Adopt after the crash: %v", err)
			}
			if !adopted {
				t.Fatal("the interrupted swap was not finished")
			}
			if got := read(t, live); got != stagedContent {
				t.Errorf("live = %q, want the restored store", got)
			}
			for _, suffix := range sidecarSuffixes {
				mustNotExist(t, live+suffix, "a companion of the REPLACED store left in the live slot is recovered straight over the restored one")
			}
		})
	}
}

// TestAdoptClearsEVERYSidecarBeforeTheStagedStoreMovesIn drives the whole
// sidecarSuffixes list rather than naming `-wal`, which is what makes it catch
// the file the list was missing.
//
// `-journal` was not on the list. A restore's staged bytes are `VACUUM INTO`
// output — a ROLLBACK-mode database (journal_mode 1 in the header, not 2) — so a
// stray `<live>-journal` beside the newly adopted file is read by SQLite as that
// database's own hot journal and ROLLED BACK into it. Same defect as the `-wal`
// one this package was built for, in the other journal mode.
func TestAdoptClearsEVERYSidecarBeforeTheStagedStoreMovesIn(t *testing.T) {
	for _, suffix := range sidecarSuffixes {
		t.Run(suffix, func(t *testing.T) {
			live := swapDir(t)
			if err := os.WriteFile(live+suffix, []byte("state belonging to the store being REPLACED"), 0o600); err != nil {
				t.Fatalf("seed %s: %v", suffix, err)
			}
			if err := Stage(live, writeStaged(stagedContent)); err != nil {
				t.Fatalf("Stage: %v", err)
			}
			if _, err := Adopt(live); err != nil {
				t.Fatalf("Adopt: %v", err)
			}
			mustNotExist(t, live+suffix, "SQLite would recover it into the restored database, which is the silent non-restore this package exists to prevent")
			if got := read(t, live); got != stagedContent {
				t.Errorf("live = %q, want the restored store", got)
			}
		})
	}
}

// TestEveryCarriedSidecarFollowsItsDatabase is the other half of knowing about a
// companion: Adopt must not merely DELETE the ones it recognises, it must carry
// the data-bearing ones with the database they belong to, or the pre-restore
// copy an operator falls back to is silently missing whatever the box did most
// recently.
//
// Driven off carriedSuffixes for the same reason as the test above — a companion
// added to that list and not carried is exactly the half-a-pair this package
// keeps producing.
func TestEveryCarriedSidecarFollowsItsDatabase(t *testing.T) {
	live := swapDir(t)
	_, _, previous, _ := Paths(live)
	for _, suffix := range carriedSuffixes {
		if err := os.WriteFile(live+suffix, []byte("live state in "+suffix), 0o600); err != nil {
			t.Fatalf("seed %s: %v", suffix, err)
		}
	}
	if err := Stage(live, writeStaged(stagedContent)); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := Adopt(live); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	for _, suffix := range carriedSuffixes {
		if got := read(t, previous+suffix); got != "live state in "+suffix {
			t.Errorf("%s beside the pre-restore copy = %q, want the live store's own — the safety copy is rewound without it",
				suffix, got)
		}
	}
}

// TestStageRefusesEVERYSidecarBesideTheStagedStore is Stage's half of the same
// list. The single-file invariant is what Adopt's unconditional slot-clearing
// rests on, and an invariant enforced for two of three companions is not one.
func TestStageRefusesEVERYSidecarBesideTheStagedStore(t *testing.T) {
	for _, suffix := range sidecarSuffixes {
		t.Run(suffix, func(t *testing.T) {
			live := swapDir(t)
			err := Stage(live, func(p string) error {
				if err := os.WriteFile(p, []byte(stagedContent), 0o600); err != nil {
					return err
				}
				return os.WriteFile(p+suffix, []byte("left behind by the writer"), 0o600)
			})
			if !errors.Is(err, ErrStagedStoreHasSidecars) {
				t.Fatalf("Stage with a stray %s returned %v, want ErrStagedStoreHasSidecars", suffix, err)
			}
			if Pending(live) {
				t.Error("the restore was marked pending anyway; the refusal has to happen before the marker makes it load-bearing")
			}
		})
	}
}
