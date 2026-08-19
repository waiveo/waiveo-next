package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
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
// the check names each id and its replacement, exits 2 (work pending — the boot
// can repair it, and the operator is meant to read the list first), and — the
// point of a dry run — leaves the store exactly as it found it.
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
	if code := reportStoreIDs(dsn, &out); code != storeCheckPending {
		t.Fatalf("reportStoreIDs exit code = %d, want %d (work pending)\n%s", code, storeCheckPending, out.String())
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
	if code := reportStoreIDs(dsn, &out); code != storeCheckIncomplete {
		t.Fatalf("reportStoreIDs exit code = %d, want %d\n%s", code, storeCheckIncomplete, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "demo-playlist") {
		t.Fatalf("report does not name the offending id:\n%s", got)
	}
	if !strings.Contains(got, "REFUSE TO START") {
		t.Fatalf("report does not warn that the boot will refuse:\n%s", got)
	}
	// And the sections BELOW the refusal still ran. They used to be suppressed by
	// the early return, so an operator fixed the one named id, restarted, and met
	// everything that had been hidden behind it.
	for _, want := range []string{
		"org-kind scope node",
		"device first-seen ledger:",
		"durable event log:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("a blocked row id suppressed the section %q that comes after it:\n%s", want, got)
		}
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
	if code := reportStoreIDs(dsn, &out); code != storeCheckPending {
		t.Fatalf("reportStoreIDs exit code = %d, want %d (work pending)\n%s", code, storeCheckPending, out.String())
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
// It exits 2 in both: the row ids are fine and the feeder will start, so this is
// not "I could not tell you" — but an owner API that 404s forever is not "nothing
// to do" either, and 0 is reserved for a store the next boot changes nothing in
// and that needs nobody. What must not happen is silence, or a pass.
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
			if code := reportStoreIDs(dsn, &out); code != storeCheckPending {
				t.Fatalf("reportStoreIDs exit code = %d, want %d (needs a human)\n%s", code, storeCheckPending, out.String())
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
	if code := reportStoreIDs(dsn, &out); code != storeCheckPending {
		t.Fatalf("reportStoreIDs exit code = %d, want %d (work pending)\n%s", code, storeCheckPending, out.String())
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
	// Recorded NOW, not at ts=0. An event stamped at the epoch is past every
	// retention window this policy configures, so the report would correctly say
	// the next boot deletes all three — a true statement about a fixture that was
	// meant to be about what the log HOLDS. See
	// TestStoreCheckPlansTheEventLogEviction for the eviction half.
	now := time.Now().UnixMilli()
	for i, row := range []struct{ id, schema, class string }{
		{"01J9F00000000000000000000A", "audit.event", "audit-long"},
		{"01J9F00000000000000000000B", "audit.event", "audit-long"},
		{"01J9F00000000000000000000C", "box.vitals", "telemetry-standard"},
	} {
		if _, err := db.Exec(
			`INSERT INTO events (id, schema, ts, scope_node, trace_id, cost_class, retention_class, origin, origin_principal, payload)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.id, row.schema, now-int64(i), "", "", "", row.class, "internal", "", `{}`); err != nil {
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
	for _, want := range []string{
		"3 event(s) retained", "audit-long=2", "telemetry-standard=1",
		// And what the next boot's sweep does with them, which is the half that
		// let exit 0 promise "the next boot changes nothing".
		"the next boot's retention sweep retires none of them",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report does not carry %q:\n%s", want, got)
		}
	}
}

// storeFingerprint is everything about a store that must not change when a check
// merely LOOKS at it: the database file's bytes and its mode.
//
// It also folds in the size of the write-ahead log, because that is where a
// committed change would live if one had been made. What it deliberately does
// NOT require is the ABSENCE of sidecars: SQLite cannot read a WAL-mode database
// without its shared-memory index, so any reader — including a strictly
// read-only one — creates a `-shm` and a zero-length `-wal` if they are not
// already there. Those carry no content and no change; a non-empty `-wal` would.
func storeFingerprint(t *testing.T, dsn string) string {
	t.Helper()
	body, err := os.ReadFile(dsn)
	if err != nil {
		t.Fatalf("read %s: %v", dsn, err)
	}
	info, err := os.Stat(dsn)
	if err != nil {
		t.Fatalf("stat %s: %v", dsn, err)
	}
	sum := sha256.Sum256(body)
	wal := int64(0)
	if walInfo, err := os.Stat(dsn + "-wal"); err == nil {
		wal = walInfo.Size()
	}
	journal := "absent"
	if _, err := os.Stat(dsn + "-journal"); err == nil {
		journal = "present"
	}
	return fmt.Sprintf("sha256=%x size=%d mode=%o wal=%d journal=%s",
		sum[:8], len(body), info.Mode().Perm(), wal, journal)
}

// TestStoreCheckNamesAMissingColumnAndChangesNothing is the pre-deploy gate for
// issue #194's class of defect, and the assertion that keeps it a DRY RUN.
//
// An operator restarting a box onto a build that declares a new column has to be
// able to see that from the outside, before taking the restart. The check used
// to print the plan in the future tense and then, four lines later, open the
// store for real — which is what applies it. Pointed at the pre-deploy backup
// (the obvious thing to compare against, and often the only rollback copy) it
// converted that backup to this build's schema: column added, epoch stamped,
// mode forced to 0600, -wal/-shm left beside it. Asked a second time it reported
// the store as conforming, so the reading could not even be repeated.
//
// So the file is fingerprinted before and after — bytes, mode, and any WAL
// CONTENT — and the check is run TWICE and must answer identically. Not
// "sidecars": storeFingerprint deliberately does not require their absence and
// cannot see a `-shm` at all, and claiming an assertion this test does not make
// is how a reader comes to believe the sidecars are checked.
func TestStoreCheckNamesAMissingColumnAndChangesNothing(t *testing.T) {
	dsn := seedStoreFileForCheck(t)

	// discovered_devices as the build before open_ports created it, on a file
	// with the mode an older build left (the check must not "tidy" that either —
	// it is not the process that owns this store).
	db, err := sql.Open("sqlite", "file:"+dsn)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE discovered_devices DROP COLUMN open_ports`); err != nil {
		t.Fatalf("forge the pre-open_ports shape: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	if err := os.Chmod(dsn, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	before := storeFingerprint(t, dsn)

	var out bytes.Buffer
	if code := reportStoreIDs(dsn, &out); code != storeCheckPending {
		t.Fatalf("reportStoreIDs exit code = %d, want %d (a column to add is work pending)\n%s",
			code, storeCheckPending, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"1 column(s) are missing",
		"discovered_devices.open_ports",
		"existing rows take the column's declared default",
		"this check has NOT added them",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the check must name the missing column; %q absent from:\n%s", want, got)
		}
	}

	if after := storeFingerprint(t, dsn); after != before {
		t.Fatalf("`-store-check` must not touch the store it is asked about — run against a pre-deploy backup, "+
			"that is the backup gone.\n  before: %s\n   after: %s", before, after)
	}

	// A reading that cannot be taken twice is not a reading. The second run must
	// say exactly what the first did — bar the file's mtime line, which is a fact
	// about the file rather than about the reading and does not change here.
	var again bytes.Buffer
	if code := reportStoreIDs(dsn, &again); code != storeCheckPending {
		t.Fatalf("second reportStoreIDs exit code = %d, want %d\n%s", code, storeCheckPending, again.String())
	}
	if again.String() != got {
		t.Fatalf("the check answered differently the second time; the first run changed the store.\n"+
			"--- first ---\n%s\n--- second ---\n%s", got, again.String())
	}
	if after := storeFingerprint(t, dsn); after != before {
		t.Fatalf("the second run changed the store.\n  before: %s\n   after: %s", before, after)
	}
	t.Logf("-store-check output:\n%s", got)
}

// TestStoreCheckSaysSoWhenEveryColumnIsPresent: the conforming answer has to be
// stated, not merely implied by silence — "nothing to report" and "the check did
// not look" must not read the same. And a conforming store is not written to
// either: no epoch re-stamp, no chmod, no sidecar.
func TestStoreCheckSaysSoWhenEveryColumnIsPresent(t *testing.T) {
	dsn := seedStoreFileForCheck(t)
	before := storeFingerprint(t, dsn)

	var out bytes.Buffer
	if code := reportStoreIDs(dsn, &out); code != 0 {
		t.Fatalf("reportStoreIDs exit code = %d, want 0\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "every column this build declares is present") {
		t.Fatalf("report did not state the store's shape conforms:\n%s", out.String())
	}
	if after := storeFingerprint(t, dsn); after != before {
		t.Fatalf("a check over a conforming store must still write nothing.\n  before: %s\n   after: %s", before, after)
	}
}

// TestStoreCheckRefusesAWorkspaceANewerBuildWrote: the check must give the same
// answer the boot would, and give it about the SCHEMA too.
//
// Reporting a column plan for a workspace a newer build wrote is worse than
// reporting nothing: the newer build's columns read as columns "this build no
// longer declares", which is a written case for hand-dropping a column that
// build's rows depend on — and the addition it promises will never happen,
// because the open refuses (ARC-041/104).
func TestStoreCheckRefusesAWorkspaceANewerBuildWrote(t *testing.T) {
	dsn := seedStoreFileForCheck(t)

	db, err := sql.Open("sqlite", "file:"+dsn)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE discovered_devices ADD COLUMN link_local TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatalf("forge the newer build's column: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, store.PlatformSchemaEpoch+1)); err != nil {
		t.Fatalf("stamp the newer epoch: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	before := storeFingerprint(t, dsn)

	var out bytes.Buffer
	code := reportStoreIDs(dsn, &out)
	if code != 1 {
		t.Fatalf("reportStoreIDs exit code = %d, want 1 for a workspace this build cannot open\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "newer than this build understands") {
		t.Fatalf("the refusal must say why; got:\n%s", got)
	}
	// And it must say what the boot ACTUALLY does. "Refuse to start" and "up,
	// answering 503s on every route but /healthz" are different incidents with
	// different first moves, and this refusal is the second one
	// (cmd/waiveo-feeder/maintenance.go routes both InspectSchema errors there).
	for _, want := range []string{"MAINTENANCE MODE", "503", "DOWNGRADE"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the report does not say what the next boot does (%q absent):\n%s", want, got)
		}
	}
	if strings.Contains(got, "refuse to start") || strings.Contains(got, "REFUSE TO START") {
		t.Fatalf("the report says the feeder will refuse to start, when it will be up and serving 503s:\n%s", got)
	}
	for _, mustNotSay := range []string{
		"will be added when the store is next opened",
		"no longer declares it",
	} {
		if strings.Contains(got, mustNotSay) {
			t.Fatalf("a workspace this build cannot open must not be described as drift (%q):\n%s", mustNotSay, got)
		}
	}
	if after := storeFingerprint(t, dsn); after != before {
		t.Fatalf("a refused check must change nothing.\n  before: %s\n   after: %s", before, after)
	}
}

// TestStoreCheckOnAPathWithNoStoreCreatesNothing: the check runs against
// whatever path an operator types, and a typo — or a box before its first boot —
// must not be answered by CREATING the store and then reporting that the store
// it just made is in good order.
//
// # And it must not be answered with a PASS either
//
// This exited 0 with "every column this build declares is present." as its first
// line, and that pairing is the most dangerous output the tool had. The
// documented pre-deploy invocation carried no path at all, the systemd unit sets
// no WAIVEO_FEEDER_STORE, and the default is relative to the cwd — so run from
// anywhere but /opt/waiveo-next (i.e. from the directory an ssh session lands in)
// the happy-path command printed a positive schema assertion about a file it had
// never opened, and exited 0 at the exact moment of a deploy decision.
//
// A check that looked at nothing is not a check that found nothing wrong. So the
// affirmative sentence is gone, and the code is 1: "I could not tell you." A box
// genuinely before its first boot is the one case where that reads as a false
// alarm, and it is the right trade — the operator of a box that has never run
// knows it, while the operator holding a mistyped path does not.
func TestStoreCheckOnAPathWithNoStoreCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "not-created-yet.db")

	var out bytes.Buffer
	if code := reportStoreIDs(dsn, &out); code != storeCheckIncomplete {
		t.Fatalf("reportStoreIDs exit code = %d, want %d — a path holding no store is a question this check "+
			"could not answer, and a deploy gate must not read it as a pass\n%s", code, storeCheckIncomplete, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "there is NO STORE at") {
		t.Fatalf("the check must say the path is empty; got:\n%s", got)
	}
	if strings.Contains(got, "every column this build declares is present") {
		t.Fatalf("the check asserted a healthy schema about a file that does not exist:\n%s", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("a check must not create the store it was asked about; found %v", names)
	}
}

// TestStoreCheckNamesThePendingTableAndTheSeedItEnables is #196's residual, in
// the one place it was observed: run against box .12 before its upgrade, this
// check named the pending `discovered_devices.relay_last_seen` COLUMN and never
// the pending `device_first_seen` TABLE that arrived in the same build — because
// the column planner passes over an absent table by design and nothing carried
// the fact out.
//
// It matters more than a missing line. The boot that creates that table also runs
// the one-time seed into it, and the seed is irreversible: an adopted value is
// frozen as that device's age, a refused one leaves the device with no age at
// all, and nothing afterwards moves either. This check is the only moment an
// operator can see which is about to happen to which device.
//
// # It then RUNS the boot it described, because the description was false
//
// An earlier version of this test asserted the sentences and stopped. That is
// how the sentences came to be wrong: the check said a refused device "carries no
// age until a fresh report plants one" and the boot log said the same, while the
// backfill only declined to copy the value into the LEDGER and never touched
// `discovered_devices.first_seen` — the column the boot restore reads and the API
// serves. Seconds after printing that line the same boot restored `first_seen =
// 1000000` into the read model, where the console renders it "20586d ago"; a
// FUTURE refused value was worse, since the console clamps a negative age and
// drew "just now" for a device the log said had no age whatever. Three statements
// about one device, mutually inconsistent, none of them checked.
//
// So the assertions run in two halves that must agree: what `-store-check`
// PROMISES the next boot will do, and what the next boot — a real store.Open plus
// the real restoreDeviceRegistry — actually leaves an operator reading.
func TestStoreCheckNamesThePendingTableAndTheSeedItEnables(t *testing.T) {
	dsn := seedStoreFileForCheck(t)

	// The file a pre-ledger build left: mirror rows carrying a `first_seen` written
	// off the reporting relay's clock, and no ledger table at all.
	//
	// The ids are DERIVED rather than invented, because the second half of this
	// test restores the rows through the real intake, which derives the same id
	// from (site, driver, native_id) and would otherwise never meet the fixture's.
	const (
		checkSite   = "site"
		checkDriver = "roku-ecp"
		nativeGood  = "uuid:roku:ecp:X00500RESCUABLE"
		nativeBad   = "uuid:roku:ecp:X00500NEVERSET0"
	)
	rescuableID := deviceid.Device(checkSite, checkDriver, nativeGood)
	refusedID := deviceid.Device(checkSite, checkDriver, nativeBad)

	db, err := sql.Open("sqlite", "file:"+dsn)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	for _, row := range []struct {
		deviceID  string
		nativeID  string
		firstSeen int64
	}{
		// A plausible instant months before now: the rescuable population.
		{rescuableID, nativeGood, 1_787_098_315_675},
		// A relay whose clock was never set: the refused population.
		{refusedID, nativeBad, 1_000_000},
	} {
		if _, err := db.Exec(
			`INSERT INTO discovered_devices
			   (device_id, relay_id, scope_node, driver, native_id, device_class,
			    name, address, model, serial, first_seen, last_seen, entities, open_ports, relay_last_seen)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.deviceID, "relay-from-the-old-build", checkSite, checkDriver, row.nativeID, "media-player",
			"Lobby Roku", "192.168.50.31", "", "", row.firstSeen, 1_787_098_400_000, `[]`, `[]`, 0,
		); err != nil {
			t.Fatalf("seed the pre-ledger mirror row %s: %v", row.deviceID, err)
		}
	}
	if _, err := db.Exec(`DROP TABLE device_first_seen`); err != nil {
		t.Fatalf("forge the pre-ledger file shape: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	before := storeFingerprint(t, dsn)

	var out bytes.Buffer
	if code := reportStoreIDs(dsn, &out); code != storeCheckPending {
		t.Fatalf("reportStoreIDs exit code = %d, want %d (a pending table and an irreversible seed are work pending)\n%s",
			code, storeCheckPending, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"1 table(s) this build declares are missing",
		"device_first_seen",
		"creating a table cannot lose a row",
		"the table is not on this store yet",
		"will adopt 1 value(s) from the pre-ledger column",
		"origin=" + store.FirstSeenAdopted,
		"OVERSTATES it if that relay's clock ran behind",
		rescuableID + "  first_seen=1787098315675",
		"will refuse 1 value(s) as implausible",
		refusedID + "  first_seen=1000000",
		"below the plausibility floor",
		"this check has NOT seeded anything",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the check must name the pending table and the seed it enables; %q absent from:\n%s", want, got)
		}
	}
	// The guarantee neither this check nor the code behind it can make — see
	// internal/app/store's TestTheSeedSaysWhatItRescuedAndWhatItRefused.
	if strings.Contains(got, "LOWER BOUND on") {
		t.Fatalf("the check states an unqualified lower-bound guarantee on a value that can OVERSTATE an age:\n%s", got)
	}

	if after := storeFingerprint(t, dsn); after != before {
		t.Fatalf("`-store-check` must not touch the store it is asked about.\n  before: %s\n   after: %s", before, after)
	}
	t.Logf("-store-check output:\n%s", got)

	// --- and now the boot the check just described ---
	//
	// Nothing below re-reads a log. It asks the read model an operator's own
	// question — "how old is this device" — through the same restore the feeder
	// runs at boot, because that is the surface the promise was made about.
	booted, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("open the store the check described: %v", err)
	}
	t.Cleanup(func() { _ = booted.Close() })

	registry := devices.New(checkSite, store.WallClockMs)
	counts, err := restoreDeviceRegistry(context.Background(), booted, registry)
	if err != nil {
		t.Fatalf("restoreDeviceRegistry: %v", err)
	}

	adopted, ok := registry.Device(rescuableID)
	if !ok {
		t.Fatalf("the adopted device is not in the restored registry")
	}
	if adopted.FirstSeen != 1_787_098_315_675 {
		t.Errorf("the adopted device's age = %d, want the rescued 1787098315675", adopted.FirstSeen)
	}
	if adopted.FirstSeenOrigin != store.FirstSeenAdopted {
		t.Errorf("the adopted device reports origin %q, want %q — a value served without its provenance is "+
			"indistinguishable from an instant this deployment observed",
			adopted.FirstSeenOrigin, store.FirstSeenAdopted)
	}

	refused, ok := registry.Device(refusedID)
	if !ok {
		t.Fatalf("the refused device is not in the restored registry — a refusal must not drop the device")
	}
	if refused.FirstSeen != 0 {
		t.Fatalf("the check and the boot log both say the refused device carries NO age until a fresh report "+
			"plants one; the restored read model serves first_seen = %d, which the console renders as an age. "+
			"That is the sentence being false, not a rounding difference", refused.FirstSeen)
	}
	if refused.FirstSeenOrigin != "" {
		t.Errorf("a device with no age reports origin %q, want empty", refused.FirstSeenOrigin)
	}
	// The device is still LISTED and still carries its sighting: a refusal is
	// about the age and nothing else.
	if refused.LastSeen != 1_787_098_400_000 {
		t.Errorf("the refused device's last_seen = %d, want the mirrored 1787098400000 — refusing an age must "+
			"not lose the sighting", refused.LastSeen)
	}
	if counts.Restored != 2 || counts.Aged != 1 {
		t.Errorf("restore counts = %+v, want 2 restored and exactly 1 with a stored first_seen; the boot log's "+
			"own count must agree with what the read model serves", counts)
	}

	// And the ledger listing the boot log's truncation referral points at is real:
	// `-store-check` run AFTER the seed enumerates the rows and their origins.
	var post bytes.Buffer
	if code := reportStoreIDs(dsn, &post); code != storeCheckPending {
		// Still 2 after a successful boot, and correctly so: the refused value is
		// STILL on the file and that device still has no age, which is a state a
		// human resolves (a report on a working clock, or a retire) and not one the
		// next boot clears.
		t.Fatalf("post-boot reportStoreIDs exit code = %d, want %d\n%s", code, storeCheckPending, post.String())
	}
	postOut := post.String()
	for _, want := range []string{
		"device first-seen ledger: 1 row(s)",
		rescuableID + "  first_seen=1787098315675  origin=" + store.FirstSeenAdopted,
		"1 of those 1 listed value(s) are NOT instants this deployment observed",
		// The refusal keeps being named, forever, because the value is still on
		// the file and the device still has no age.
		refusedID + "  first_seen=1000000",
	} {
		if !strings.Contains(postOut, want) {
			t.Fatalf("after the seed, `-store-check` must list the ledger in full — the boot log's truncation "+
				"sends the operator here; %q absent from:\n%s", want, postOut)
		}
	}
	t.Logf("post-boot -store-check output:\n%s", postOut)
}

// TestStoreCheckReportsAnAnsweredLedger: the conforming answer has to be stated
// rather than implied by silence, on the same rule the column report follows —
// "nothing to seed" and "the check did not look" must not read the same.
func TestStoreCheckReportsAnAnsweredLedger(t *testing.T) {
	dsn := seedStoreFileForCheck(t)

	var out bytes.Buffer
	if code := reportStoreIDs(dsn, &out); code != 0 {
		t.Fatalf("reportStoreIDs exit code = %d, want 0\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "device first-seen ledger: 0 row(s); 0 of 0 mirrored device(s) have a stored first_seen") {
		t.Fatalf("the check must state the ledger's census even when it is empty:\n%s", got)
	}
	if !strings.Contains(got, "the next boot will seed nothing into it") {
		t.Fatalf("the check must say the next boot seeds nothing, rather than saying nothing:\n%s", got)
	}
	if strings.Contains(got, "table(s) this build declares are missing") {
		t.Fatalf("a conforming store must not be reported as missing a table:\n%s", got)
	}
}
