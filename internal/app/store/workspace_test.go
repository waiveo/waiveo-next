package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// TestSnapshotIntoWritesAnOwnerOnlyFile is the regression for a cleartext copy
// of the entire workspace landing world-readable.
//
// `VACUUM INTO` creates its destination 0644 and offers no way to ask for
// anything else, so the snapshot an export takes — every row of every resource,
// openable by any SQLite client — used to sit at 0644 beside the 0600 encrypted
// container the export exists to produce. Anyone with a shell on the box could
// read the whole workspace out of the scratch file while the export ran.
//
// The destination directory is pre-created 0755 on purpose: os.MkdirAll applies
// its mode only to a directory it creates, so a directory that already exists —
// which the archive destination does, from the first export onward — keeps
// whatever mode it had unless something actively tightens it.
func TestSnapshotIntoWritesAnOwnerOnlyFile(t *testing.T) {
	ctx := context.Background()
	st := openSeededStore(t)

	dir := filepath.Join(t.TempDir(), "exports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("pre-create destination dir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("pre-chmod destination dir: %v", err)
	}
	path := filepath.Join(dir, "snapshot.sqlite")

	if err := st.SnapshotInto(ctx, path); err != nil {
		t.Fatalf("SnapshotInto: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("snapshot mode = %04o, want 0600 — this file is a cleartext copy of the whole workspace", got)
	}
	if info.Size() == 0 {
		t.Error("snapshot is empty; the mode assertion above would pass vacuously")
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat destination dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("destination directory mode = %04o, want 0700", got)
	}
}

// TestSnapshotIntoRefusesAnExistingDestination pins the refusal SnapshotInto
// deliberately leaves to SQLite: an export writing over an existing file is a
// caller bug, and papering over it with a pre-delete would hide it.
func TestSnapshotIntoRefusesAnExistingDestination(t *testing.T) {
	ctx := context.Background()
	st := openSeededStore(t)

	path := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := os.WriteFile(path, []byte("in the way"), 0o600); err != nil {
		t.Fatalf("pre-write destination: %v", err)
	}

	if err := st.SnapshotInto(ctx, path); err == nil {
		t.Fatal("SnapshotInto over an existing file = nil, want an error")
	}
}

// openSeededStore returns an open store holding real rows, so a snapshot of it
// has content rather than an empty schema.
func openSeededStore(t *testing.T) *store.Store {
	t.Helper()
	st := openMem(t)
	seedSiteScreen(t, st)
	return st
}
