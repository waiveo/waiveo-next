package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/events"
)

// TestAFileBackedStoreIsTightenedAtRest pins what Open does to the database file
// itself, and it exists because of a survivor in issue #179's sweep — the
// `dsn != ":memory:"` check that wraps the tightening.
//
// I first wrote this believing that check was an equivalent mutant. It is not,
// and the mistake is worth recording because it was a mistake about what
// disabling a guard DOES. Disabling the condition does not widen the block to
// run for `:memory:` as well; it makes the condition false, so the tightening
// runs for NOTHING. The reason no test caught that is simpler than any theory
// about the guard: every test in this package opened `:memory:`, where there is
// no file to be left loose. The branch that matters had no test because the
// fixture had no file.
//
// The stake is in Open's own comment: this database "is secret material at rest,
// and was sitting at the umask default. It holds the sealed webhook signing
// secrets, the durable audit log, and the install records naming which key
// vouched for the code that is running". The WAL sidecar carries recently
// changed page images, so leaving it readable leaks exactly what the database
// mode protects.
//
// The snapshot copy's 0600 was already pinned in workspace_test.go. The live
// database's was not, which is the odder gap of the two: the snapshot is a copy
// an operator asks for, and this is the file the process runs on.
func TestAFileBackedStoreIsTightenedAtRest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspace.db")

	s, err := store.Open(path, store.WallClockMs)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// A write, so the WAL sidecar actually exists to be checked. Without it the
	// sidecar assertions below would pass by finding nothing.
	if err := s.SeedDemo(context.Background(), seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the database: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("the database is mode %04o, want 0600 — it holds the sealed webhook signing secrets, the durable "+
			"audit log, and the records naming which key vouched for the running code", got)
	}

	// The sidecars go with it: -wal holds recently changed page images, so a
	// readable one leaks what the database mode protects.
	for _, suffix := range []string{"-wal", "-shm"} {
		side := path + suffix
		si, err := os.Stat(side)
		if os.IsNotExist(err) {
			continue // not every mode produces both
		}
		if err != nil {
			t.Fatalf("stat %s: %v", side, err)
		}
		if got := si.Mode().Perm(); got != 0o600 {
			t.Errorf("%s is mode %04o, want 0600 — the WAL carries recently changed page images, so leaving it "+
				"readable leaks exactly what the database mode is protecting", filepath.Base(side), got)
		}
	}
}

// TestOpeningAnExistingLooseDatabaseTightensIt is the case the comment says
// prompted the check: a database that "was sitting at the umask default".
//
// Tightening only at creation would leave every store that predates the check
// loose forever, which is the population the fix exists for.
func TestOpeningAnExistingLooseDatabaseTightensIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspace.db")

	// Create it, then deliberately loosen it the way a umask default would.
	s, err := store.Open(path, store.WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("loosen: %v", err)
	}

	s2, err := store.Open(path, store.WallClockMs)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("re-opening a 0644 database left it at %04o — tightening only at creation would leave every store "+
			"that predates this check loose forever, which is the population it exists for", got)
	}
}

// TestOpenEventLogRefusesANilStore.
//
// OpenEventLog is exported and takes the store as a parameter; the method form
// beside it is the only in-repo caller and always passes a real one. So this is a
// boundary check for a caller not yet written — and without it the very next
// lines read s.nowMs and take s.mu, so the failure is a nil dereference rather
// than a refusal.
func TestOpenEventLogRefusesANilStore(t *testing.T) {
	if _, err := store.OpenEventLog(nil, events.DefaultRetentionPolicy(), nil, nil); err == nil {
		t.Error("OpenEventLog accepted a nil store — the next lines read its clock and take its lock")
	}
}

// TestSeedDemoRefusesAnEmptyAssetSet.
//
// The refs become the demo playlist's items in playback order, and the seeded
// content daypart points at that playlist. With none, the seed does not fail
// visibly — it produces a complete, well-formed demo whose content daypart
// resolves to an EMPTY playlist, which is a screen scheduled to show nothing
// during exactly the hours the demo exists to demonstrate.
func TestSeedDemoRefusesAnEmptyAssetSet(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	before := gen(t, s)
	if err := s.SeedDemo(ctx); err == nil {
		t.Error("SeedDemo accepted an empty asset set — the seeded content daypart would point at an empty " +
			"playlist, scheduling a screen to show nothing during the hours the demo exists to show something")
	}
	if after := gen(t, s); after != before {
		t.Errorf("generation moved %d -> %d on a refused seed", before, after)
	}

	// The control: one ref seeds, and the generation moves.
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo with one ref: %v", err)
	}
	if after := gen(t, s); after == before {
		t.Error("a successful seed did not move the generation")
	}
}
