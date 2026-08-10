package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// ARC-041/104: a boot MUST refuse to open a workspace whose platform schema
// epoch is newer than this binary understands — "exactly as the destination
// would refuse to open a live workspace at that epoch" — rather than run this
// build's (older) migrations over a file a newer build wrote. The epoch is
// recorded in the SQLite file itself (PRAGMA user_version) so the refusal is
// decidable from the file alone, before any table is touched.

func openRawUserVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

func setRawUserVersion(t *testing.T, path string, v int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA user_version = " + itoa(v)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
}

func itoa(v int) string {
	// small, dependency-free; v is a test-controlled small int
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// A fresh store records this build's epoch on disk, and reopening it succeeds.
func TestOpenStampsAndReopensAtCurrentEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ws.db")

	s, err := store.Open(path, store.WallClockMs)
	if err != nil {
		t.Fatalf("fresh Open: %v", err)
	}
	_ = s.Close()

	if got := openRawUserVersion(t, path); got != store.PlatformSchemaEpoch {
		t.Fatalf("on-disk epoch = %d, want %d (a fresh store must stamp its epoch)", got, store.PlatformSchemaEpoch)
	}

	s2, err := store.Open(path, store.WallClockMs)
	if err != nil {
		t.Fatalf("reopen at the same epoch must succeed: %v", err)
	}
	_ = s2.Close()
}

// A legacy file that predates the epoch marker (user_version 0) opens and is
// upgraded to the current epoch — an existing box must not be bricked by the
// marker's introduction.
func TestOpenUpgradesAPreEpochFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ws.db")
	s, err := store.Open(path, store.WallClockMs)
	if err != nil {
		t.Fatalf("fresh Open: %v", err)
	}
	_ = s.Close()

	setRawUserVersion(t, path, 0) // simulate a file written before the marker existed
	s2, err := store.Open(path, store.WallClockMs)
	if err != nil {
		t.Fatalf("a pre-epoch (user_version 0) file must open: %v", err)
	}
	_ = s2.Close()
	if got := openRawUserVersion(t, path); got != store.PlatformSchemaEpoch {
		t.Fatalf("after opening a pre-epoch file, on-disk epoch = %d, want it upgraded to %d", got, store.PlatformSchemaEpoch)
	}
}

// The refusal: a file whose epoch is newer than this build understands must NOT
// open, must report a typed EpochTooNew error carrying both numbers, and must
// leave the file untouched (no migration ran).
func TestOpenRefusesANewerEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ws.db")
	s, err := store.Open(path, store.WallClockMs)
	if err != nil {
		t.Fatalf("fresh Open: %v", err)
	}
	_ = s.Close()

	newer := store.PlatformSchemaEpoch + 1
	setRawUserVersion(t, path, newer)

	s2, err := store.Open(path, store.WallClockMs)
	if err == nil {
		_ = s2.Close()
		t.Fatal("Open accepted a workspace at a newer epoch than it understands (ARC-041)")
	}
	var ete *store.EpochTooNewError
	if !errors.As(err, &ete) {
		t.Fatalf("error = %v (%T), want *store.EpochTooNewError", err, err)
	}
	if ete.OnDisk != newer || ete.Understood != store.PlatformSchemaEpoch {
		t.Fatalf("EpochTooNewError = {OnDisk:%d, Understood:%d}, want {%d, %d}",
			ete.OnDisk, ete.Understood, newer, store.PlatformSchemaEpoch)
	}
	// The file must be untouched: its epoch is still the newer value, proving no
	// migration downgraded or rewrote it.
	if got := openRawUserVersion(t, path); got != newer {
		t.Fatalf("a refused file's epoch changed to %d — Open must not touch a file it refuses", got)
	}
}
