package main

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	_ "modernc.org/sqlite"
)

// storeCheckAssetRef is a fixture content id the demo playlist item points at.
const storeCheckAssetRef = "sha256:3a5439d0a1f4b2c6e7889900aabbccddeeff00112233445566778899aabbccdd"

// seedStoreFileForCheck writes a seeded store to a temp file and returns its path.
func seedStoreFileForCheck(t *testing.T) string {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")
	s, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := s.SeedDemo(context.Background(), storeCheckAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return dsn
}

// TestStoreCheckReportsAConformingStore: the operator's pre-restart check on a
// store written by this build says, in so many words, that the restart will not
// touch it — and exits 0.
func TestStoreCheckReportsAConformingStore(t *testing.T) {
	dsn := seedStoreFileForCheck(t)

	var out bytes.Buffer
	if code := reportStoreIDs(dsn, &out); code != 0 {
		t.Fatalf("reportStoreIDs exit code = %d, want 0\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "already a canonical ULID") {
		t.Fatalf("report did not say the store conforms:\n%s", out.String())
	}
}

// TestStoreCheckListsThePendingRewrites: against a store an older build wrote,
// the check names each id and its replacement, exits 0 (the boot can repair it),
// and — the point of a dry run — leaves the store exactly as it found it.
func TestStoreCheckListsThePendingRewrites(t *testing.T) {
	dsn := seedStoreFileForCheck(t)

	// The screen scope node as it was spelled before a row id had to be a
	// canonical ULID, written straight into the file the way that build did.
	const legacyScreen = "01J8Z4DEMOSCREENFIRSTPHOTN"
	const currentScreen = "01J8Z4DEM0SCREENF1RSTPH0TN"
	db, err := sql.Open("sqlite", "file:"+dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	for _, tbl := range []string{"scope_nodes", "playlists", "schedules", "dayparts", "preset_batches"} {
		if _, err := db.Exec(
			`UPDATE `+tbl+` SET id = replace(id, ?, ?), scope_node = replace(scope_node, ?, ?), body = replace(body, ?, ?)`,
			currentScreen, legacyScreen, currentScreen, legacyScreen, currentScreen, legacyScreen); err != nil {
			db.Close()
			t.Fatalf("regress %s: %v", tbl, err)
		}
	}
	db.Close()

	before := storeGenerationForCheck(t, dsn)

	var out bytes.Buffer
	if code := reportStoreIDs(dsn, &out); code != 0 {
		t.Fatalf("reportStoreIDs exit code = %d, want 0\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, legacyScreen) || !strings.Contains(got, currentScreen) {
		t.Fatalf("report does not name the rewrite %s -> %s:\n%s", legacyScreen, currentScreen, got)
	}
	if !strings.Contains(got, "nothing has been changed") {
		t.Fatalf("report does not state that it wrote nothing:\n%s", got)
	}
	if after := storeGenerationForCheck(t, dsn); after != before {
		t.Fatalf("the check advanced the generation %d -> %d; it must write nothing", before, after)
	}
}

// TestStoreCheckRefusesWhatTheBootWouldRefuse: an id no fold can rescue makes the
// check exit non-zero and say the feeder will not start, so the operator learns
// it from a dry run rather than from a box that stopped serving.
func TestStoreCheckRefusesWhatTheBootWouldRefuse(t *testing.T) {
	dsn := seedStoreFileForCheck(t)

	db, err := sql.Open("sqlite", "file:"+dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := db.Exec(`UPDATE playlists SET id = ?`, "demo-playlist"); err != nil {
		db.Close()
		t.Fatalf("plant unfoldable id: %v", err)
	}
	db.Close()

	var out bytes.Buffer
	if code := reportStoreIDs(dsn, &out); code != 1 {
		t.Fatalf("reportStoreIDs exit code = %d, want 1\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "demo-playlist") {
		t.Fatalf("report does not name the offending id:\n%s", got)
	}
	if !strings.Contains(got, "refuse to start") {
		t.Fatalf("report does not warn that the boot will refuse:\n%s", got)
	}
}

func storeGenerationForCheck(t *testing.T, dsn string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	var g int64
	if err := db.QueryRow(`SELECT generation FROM meta WHERE id = 1`).Scan(&g); err != nil {
		t.Fatalf("read generation: %v", err)
	}
	return g
}
