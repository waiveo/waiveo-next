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
	s, err := store.Open(dsn, store.WallClockMs)
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

// TestStoreCheckReportsAMissingOrgRoot: the check's whole purpose is that a
// restart holds no surprises, and "the next boot will create a scope node" is
// exactly the kind of surprise it exists to remove. It also names the dangling
// reference itself, because a store can be perfectly canonical and still be
// missing the node the rest of it points at — the state that 404s every
// owner-gated route while the box looks healthy.
func TestStoreCheckReportsAMissingOrgRoot(t *testing.T) {
	dsn := seedStoreFileForCheck(t)

	// The org root a store seeded before the seed inserted one is missing, and
	// the site that names it.
	const orgID = "01J8Z0DEM00RGANCEST0RB0VND"
	const siteID = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	db, err := sql.Open("sqlite", "file:"+dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM scope_nodes WHERE id = ?`, orgID); err != nil {
		db.Close()
		t.Fatalf("delete the org row: %v", err)
	}
	db.Close()

	before := storeGenerationForCheck(t, dsn)

	var out bytes.Buffer
	if code := reportStoreIDs(dsn, &out); code != 0 {
		t.Fatalf("reportStoreIDs exit code = %d, want 0\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"no org-kind scope node",
		"will re-create it as " + orgID,
		"scope node " + siteID + " names parent " + orgID,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report does not carry %q:\n%s", want, got)
		}
	}
	if after := storeGenerationForCheck(t, dsn); after != before {
		t.Fatalf("the check advanced the generation %d -> %d; it must write nothing", before, after)
	}
}

// TestStoreCheckReportsAWorkspaceNothingCanRestore is the harder half of the
// same job. The check is easy to get right for the state the boot repairs by
// itself, and that is the state that needs it least: the box fixes it and says
// so. These two are the ones where the check IS the remedy — no org node, no
// automatic repair possible, and an operator who otherwise learns about it as an
// unexplained 404 on every owner-gated route.
//
// It exits 0 in both, deliberately: the row ids are fine, the feeder will start,
// and the subcommand's exit code answers the question it was asked. What must not
// happen is silence.
func TestStoreCheckReportsAWorkspaceNothingCanRestore(t *testing.T) {
	const orgID = "01J8Z0DEM00RGANCEST0RB0VND"
	const siteID = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"

	for _, tc := range []struct {
		name  string
		sql   []string
		wants []string
	}{{
		// No org row, and the orphan stops naming one: nothing left to re-create
		// the root under.
		name: "nothing names a missing parent",
		sql: []string{
			`DELETE FROM scope_nodes WHERE id = '` + orgID + `'`,
			`UPDATE scope_nodes SET body = replace(body, '"parent_id":"` + orgID + `"', '"parent_id":null') WHERE id = '` + siteID + `'`,
		},
		wants: []string{"NOT create one", "no id to re-create the root under"},
	}, {
		// The tree emptied one leaf at a time on a box that already had no org
		// row. Past generation 0 the seed can never re-fire.
		name:  "no scope nodes at all, past the seed gate",
		sql:   []string{`DELETE FROM scope_nodes`},
		wants: []string{"NOT create one", "no scope nodes at all"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := seedStoreFileForCheck(t)
			db, err := sql.Open("sqlite", "file:"+dsn)
			if err != nil {
				t.Fatalf("raw open: %v", err)
			}
			for _, stmt := range tc.sql {
				if _, err := db.Exec(stmt); err != nil {
					db.Close()
					t.Fatalf("fixture %q: %v", stmt, err)
				}
			}
			db.Close()

			before := storeGenerationForCheck(t, dsn)
			var out bytes.Buffer
			if code := reportStoreIDs(dsn, &out); code != 0 {
				t.Fatalf("reportStoreIDs exit code = %d, want 0\n%s", code, out.String())
			}
			got := out.String()
			for _, want := range tc.wants {
				if !strings.Contains(got, want) {
					t.Fatalf("report does not carry %q — the operator learns nothing about a store that 404s every owner route:\n%s", want, got)
				}
			}
			if after := storeGenerationForCheck(t, dsn); after != before {
				t.Fatalf("the check advanced the generation %d -> %d; it must write nothing", before, after)
			}
		})
	}
}

// TestStoreCheckDoesNotContradictItselfAboutTheOrgRoot: the org-root diagnosis
// reads the store as it stands, while the boot canonicalizes row ids first and
// heals second. On one shape those two disagree — the repair refuses to fold an
// id another row also carries, and the boot's canonicalization renames that row
// on its own account and carries the reference with it, so the boot DOES heal.
//
// A flat "the next boot will NOT create one" printed under a list of pending
// rewrites is a report contradicting itself, and an operator has no way to tell
// which half to believe.
func TestStoreCheckDoesNotContradictItselfAboutTheOrgRoot(t *testing.T) {
	dsn := seedStoreFileForCheck(t)

	// A store an older build wrote, missing its org root, with one playlist
	// carrying the same identifier the orphaned site names.
	const legacyOrg = "01J8Z0DEMOORGANCESTORBOUND"
	const currentOrg = "01J8Z0DEM00RGANCEST0RB0VND"
	const siteID = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	db, err := sql.Open("sqlite", "file:"+dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	for _, stmt := range []string{
		`UPDATE scope_nodes SET body = replace(body, '` + currentOrg + `', '` + legacyOrg + `')`,
		`DELETE FROM scope_nodes WHERE id = '` + currentOrg + `'`,
		`INSERT INTO playlists (id, revision, external_id, labels, scope_node, created_at, updated_at, body)
		 VALUES ('` + legacyOrg + `', 1, '', '{}', '` + siteID + `', 1, 1,
		   '{"id":"` + legacyOrg + `","scope_node":"` + siteID + `","name":"Namesake","items":[],"revision":1,"created_at":1,"updated_at":1}')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatalf("fixture %q: %v", stmt, err)
		}
	}
	db.Close()

	var out bytes.Buffer
	if code := reportStoreIDs(dsn, &out); code != 0 {
		t.Fatalf("reportStoreIDs exit code = %d, want 0\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "will be canonicalized at the next boot") {
		t.Fatalf("the fixture produced no pending rewrites, so this test is not exercising the disagreement:\n%s", got)
	}
	if strings.Contains(got, "the next boot will NOT create one") {
		t.Fatalf("the report flatly denies a heal the boot performs, directly under the rewrites that make it possible:\n%s", got)
	}
	for _, want := range []string{
		"against the store as it stands",
		"canonicalizes the row ids above BEFORE it heals",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report does not say which store it is describing (%q):\n%s", want, got)
		}
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

// TestStoreCheckReportsTheDurableEventLog: the same store now carries the
// events/1 durable event log, and the canonicalization pass reaches into it (a
// stored event's scope_node follows a renamed scope node). An operator deciding
// whether to restart should be able to see how much history that covers, per
// retention class — the audit tier especially, since security-model/1 SEC-150
// makes those records the platform's only audit trail.
func TestStoreCheckReportsTheDurableEventLog(t *testing.T) {
	dsn := seedStoreFileForCheck(t)

	var empty bytes.Buffer
	if code := reportStoreIDs(dsn, &empty); code != 0 {
		t.Fatalf("reportStoreIDs exit code = %d, want 0\n%s", code, empty.String())
	}
	if !strings.Contains(empty.String(), "durable event log: empty") {
		t.Fatalf("a store holding no events must say so:\n%s", empty.String())
	}

	db, err := sql.Open("sqlite", "file:"+dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	for i, row := range []struct{ id, schema, class string }{
		{"01J9F00000000000000000000A", "audit.event", "audit-long"},
		{"01J9F00000000000000000000B", "audit.event", "audit-long"},
		{"01J9F00000000000000000000C", "box.vitals", "telemetry-standard"},
	} {
		if _, err := db.Exec(
			`INSERT INTO events (id, schema, ts, scope_node, trace_id, cost_class, retention_class, origin, origin_principal, payload)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.id, row.schema, int64(i), "", "", "", row.class, "internal", "", `{}`); err != nil {
			db.Close()
			t.Fatalf("insert event %d: %v", i, err)
		}
	}
	db.Close()

	var out bytes.Buffer
	if code := reportStoreIDs(dsn, &out); code != 0 {
		t.Fatalf("reportStoreIDs exit code = %d, want 0\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{"3 event(s) retained", "audit-long=2", "telemetry-standard=1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("report does not carry %q:\n%s", want, got)
		}
	}
}
