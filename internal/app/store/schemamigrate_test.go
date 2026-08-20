package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"

	_ "modernc.org/sqlite"
)

// schemamigrate_test.go drives the additive-column migration against a store an
// EARLIER BUILD created, not a simulated one: the fixtures here forge the exact
// on-disk shape box .12 was found in — a `discovered_devices` table with no
// `open_ports` column and rows already in it — and then open it with this build.
//
// That distinction is the whole reason this file exists. The test that shipped
// alongside the open_ports change reopened a store the SAME build had written,
// which always has the column, so it was structurally incapable of failing on
// the defect. A migration test that does not start from an older shape proves
// nothing about upgrades.

// preOpenPortsDiscoveredDevicesDDL is `discovered_devices` exactly as the build
// before commit 72bf10a created it: thirteen columns, no open_ports. Copied
// rather than derived, deliberately — this is a historical artefact, and a
// fixture that tracked the current DDL would stop reproducing the bug the day
// the DDL changed again.
const preOpenPortsDiscoveredDevicesDDL = `
CREATE TABLE discovered_devices (
	device_id    TEXT PRIMARY KEY,
	relay_id     TEXT NOT NULL,
	scope_node   TEXT NOT NULL,
	driver       TEXT NOT NULL,
	native_id    TEXT NOT NULL,
	device_class TEXT NOT NULL,
	name         TEXT NOT NULL DEFAULT '',
	address      TEXT NOT NULL DEFAULT '',
	model        TEXT NOT NULL DEFAULT '',
	serial       TEXT NOT NULL DEFAULT '',
	first_seen   INTEGER NOT NULL,
	last_seen    INTEGER NOT NULL,
	entities     TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX discovered_devices_relay ON discovered_devices (relay_id);
`

// preOpenPortsStore returns the path of a store carrying a genuinely old
// `discovered_devices`: this build's store in every other respect, with that one
// table replaced by its pre-open_ports shape and two mirrored rows already in
// it, as a box that has been running since before the column existed.
func preOpenPortsStore(t *testing.T) string {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")
	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+dsn+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Replacing the table is how the OLD shape is forged in a test fixture. It is
	// not what the migration does — the migration only ever ALTERs — and nothing
	// here stands in for authored data.
	if _, err := db.Exec(`DROP TABLE discovered_devices`); err != nil {
		t.Fatalf("drop current discovered_devices: %v", err)
	}
	if _, err := db.Exec(preOpenPortsDiscoveredDevicesDDL); err != nil {
		t.Fatalf("create pre-open_ports discovered_devices: %v", err)
	}
	for _, row := range []struct {
		deviceID, name string
	}{
		{"dev-mirrored-one", "Lobby Roku"},
		{"dev-mirrored-two", "Back Office Printer"},
	} {
		if _, err := db.Exec(
			`INSERT INTO discovered_devices
			   (device_id, relay_id, scope_node, driver, native_id, device_class,
			    name, address, model, serial, first_seen, last_seen, entities)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.deviceID, "relay-from-the-old-build", "site", "roku", row.deviceID, "media_player",
			row.name, "192.168.50.31", "", "", 1_752_800_000_000, 1_752_800_060_000, `[]`,
		); err != nil {
			t.Fatalf("seed pre-open_ports row %s: %v", row.deviceID, err)
		}
	}
	return dsn
}

// columnsOf reads a table's column names straight off the file, which is the
// only reading that can answer whether the migration actually changed the store
// rather than merely reporting that it would.
func columnsOf(t *testing.T, dsn, table string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dsn+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var (
			cid     int
			name    string
			colType string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	return out
}

func hasColumn(cols []string, want string) bool {
	for _, c := range cols {
		if c == want {
			return true
		}
	}
	return false
}

// TestOpenAddsOpenPortsToAStoreAnEarlierBuildCreated is issue #194 end to end.
//
// Before this migration existed, opening this store succeeded and then every
// statement naming open_ports failed forever: the mirror could neither be read
// at boot nor written on a report, and the only trace was one log line per
// minute at a call site that treats such errors as transient. The store had the
// column; the file did not, and nothing could ever add it.
func TestOpenAddsOpenPortsToAStoreAnEarlierBuildCreated(t *testing.T) {
	ctx := context.Background()
	dsn := preOpenPortsStore(t)

	if cols := columnsOf(t, dsn, "discovered_devices"); hasColumn(cols, "open_ports") {
		t.Fatalf("the fixture must start WITHOUT open_ports, else it proves nothing; got %v", cols)
	}

	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("Open over a pre-open_ports store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if cols := columnsOf(t, dsn, "discovered_devices"); !hasColumn(cols, "open_ports") {
		t.Fatalf("open_ports must be added by the open; got %v", cols)
	}

	// The rows the old build left are still there, and they read back through the
	// store's own decoder — which is the statement that used to fail outright.
	mirrored, err := s.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices after the migration: %v", err)
	}
	if len(mirrored) != 2 {
		t.Fatalf("the pre-existing mirrored rows must survive the migration; got %d: %+v", len(mirrored), mirrored)
	}
	for _, d := range mirrored {
		if len(d.OpenPorts) != 0 {
			t.Errorf("a row written before open_ports existed must read back with NO ports, not invented ones; %s got %v",
				d.DeviceID, d.OpenPorts)
		}
	}
	if mirrored[0].Name != "Lobby Roku" || mirrored[1].Name != "Back Office Printer" {
		t.Errorf("the migration must preserve every other field; got %q and %q", mirrored[0].Name, mirrored[1].Name)
	}

	// And the write path — the one that produced 1017 swallowed errors on box .12
	// — now works, carrying real ports through the column that was missing.
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-from-the-old-build", []store.DiscoveredDevice{{
		DeviceID:    "dev-mirrored-one",
		ScopeNode:   "site",
		Driver:      "roku",
		NativeID:    "dev-mirrored-one",
		DeviceClass: "media_player",
		Name:        "Lobby Roku",
		FirstSeen:   1_752_800_000_000,
		LastSeen:    1_752_800_600_000,
		OpenPorts:   []int{8060, 8443},
	}}); err != nil {
		t.Fatalf("ReplaceDiscoveredDevices after the migration: %v", err)
	}
	after, err := s.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	if len(after) != 1 || len(after[0].OpenPorts) != 2 || after[0].OpenPorts[0] != 8060 {
		t.Fatalf("the mirrored ports must round-trip through the migrated column; got %+v", after)
	}
}

// TestReopeningAMigratedStoreAddsNothing is the idempotence claim that lets this
// run unattended at every boot. The second open must find the store already
// conforming — the check is on the FILE, not on a remembered flag, so a store
// migrated by one process is a no-op for the next.
func TestReopeningAMigratedStoreAddsNothing(t *testing.T) {
	dsn := preOpenPortsStore(t)

	first, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	firstCols := columnsOf(t, dsn, "discovered_devices")

	// The read-only report is the honest way to ask "would the next open change
	// anything", and on a migrated store the answer must be no.
	plan, err := store.InspectSchema(dsn)
	if err != nil {
		t.Fatalf("InspectSchema after the migration: %v", err)
	}
	if len(plan.Added) != 0 {
		t.Fatalf("a migrated store must need nothing added; got %+v", plan.Added)
	}
	if len(plan.Divergent) != 0 {
		t.Fatalf("a migrated store must report no drift; got %+v", plan.Divergent)
	}

	second, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	secondCols := columnsOf(t, dsn, "discovered_devices")

	if len(firstCols) != len(secondCols) {
		t.Fatalf("the second open changed the table shape: %v then %v", firstCols, secondCols)
	}
	for i := range firstCols {
		if firstCols[i] != secondCols[i] {
			t.Fatalf("the second open changed the table shape: %v then %v", firstCols, secondCols)
		}
	}
}

// TestInspectSchemaReportsWhatTheNextOpenWillDo covers the operator's half: the
// report has to be available BEFORE the store is opened for real, because the
// open is what repairs it. Asked afterwards, every store looks conforming and
// the check is worthless.
func TestInspectSchemaReportsWhatTheNextOpenWillDo(t *testing.T) {
	dsn := preOpenPortsStore(t)

	plan, err := store.InspectSchema(dsn)
	if err != nil {
		t.Fatalf("InspectSchema: %v", err)
	}
	// Every column this build declares that the staged (older) file lacks, and
	// nothing else. The fixture predates all four additions the mirror has taken
	// since — the scan's port list, the relay stamp the last-seen rule compares
	// against, REL-110c's name rank and REL-110d's class rank
	// (discovereddevices.go) — so the plan naming exactly those four is the
	// report doing its job.
	planned := map[string]bool{}
	for _, a := range plan.Added {
		if a.Table != "discovered_devices" {
			t.Fatalf("a column addition outside the staged table: %+v", a)
		}
		planned[a.Column] = true
	}
	if len(plan.Added) != 4 || !planned["open_ports"] || !planned["relay_last_seen"] || !planned["name_rank"] || !planned["class_rank"] {
		t.Fatalf("want discovered_devices.open_ports, .relay_last_seen, .name_rank and .class_rank planned, got %+v", plan.Added)
	}
	if len(plan.Divergent) != 0 {
		t.Fatalf("a store that is merely missing a column has no non-additive drift; got %+v", plan.Divergent)
	}

	// And it wrote nothing: the column is still missing until an open adds it.
	if cols := columnsOf(t, dsn, "discovered_devices"); hasColumn(cols, "open_ports") {
		t.Fatalf("InspectSchema must not change the store; open_ports appeared: %v", cols)
	}
}

// TestInspectSchemaOnAPathWithNoStore: `-store-check` runs against whatever path
// an operator types — which was FALSE when this sentence was written (the flag
// took no path at all and reported on the cwd-relative default, #195) and is
// true now — and a path with no store yet is not an error at THIS level: Open
// would create one already carrying every column.
//
// The check one level up is stricter about the same fact, deliberately: it
// refuses to describe a path that holds nothing, because "every column this
// build declares is present" printed over a file that does not exist is how a
// mistyped path reads as a healthy box.
//
// The second assertion is the one with teeth: the path must still be EMPTY
// afterwards. A report that answers by creating the thing it was asked about is
// the defect this whole check was found to have.
func TestInspectSchemaOnAPathWithNoStore(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "not-created-yet.db")

	plan, err := store.InspectSchema(dsn)
	if err != nil {
		t.Fatalf("InspectSchema on an absent store: %v", err)
	}
	if len(plan.Added) != 0 || len(plan.Divergent) != 0 {
		t.Fatalf("an absent store has nothing to migrate; got %+v", plan)
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
		t.Fatalf("inspecting an absent store must create nothing; found %v", names)
	}
}

// TestInspectSchemaRefusesAWorkspaceANewerBuildWrote: the read-only report has
// to apply the SAME epoch gate the open does, and before it says anything about
// columns.
//
// Without it the report is not merely incomplete, it is actively misleading: the
// columns a NEWER build added read here as columns "this build no longer
// declares", and the drift line says dropping one destroys what it holds. That
// is a written justification, handed to an operator, for hand-dropping a column
// the newer build's rows depend on — while the addition it promises in the same
// breath will never happen, because the next open refuses (ARC-041/104).
func TestInspectSchemaRefusesAWorkspaceANewerBuildWrote(t *testing.T) {
	dsn := preOpenPortsStore(t)

	db, err := sql.Open("sqlite", "file:"+dsn+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	// A column a build one epoch ahead added, and that build's epoch marker.
	if _, err := db.Exec(`ALTER TABLE discovered_devices ADD COLUMN link_local TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatalf("forge the newer build's column: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, store.PlatformSchemaEpoch+1)); err != nil {
		t.Fatalf("stamp the newer epoch: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	plan, err := store.InspectSchema(dsn)
	var tooNew *store.EpochTooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("InspectSchema must refuse a newer-epoch workspace with *EpochTooNewError; got plan=%+v err=%v", plan, err)
	}
	if tooNew.OnDisk != store.PlatformSchemaEpoch+1 || tooNew.Understood != store.PlatformSchemaEpoch {
		t.Fatalf("the refusal must name both epochs; got %+v", tooNew)
	}
	if len(plan.Added) != 0 || len(plan.Divergent) != 0 {
		t.Fatalf("a refused inspection must describe nothing; got %+v", plan)
	}

	// And the open agrees, so the check and the boot never disagree about the
	// same file.
	if _, err := store.Open(dsn, store.WallClockMs); !errors.As(err, &tooNew) {
		t.Fatalf("Open must refuse the same workspace the same way; got %v", err)
	}
}

// TestOpenReportsButDoesNotRepairANonAdditiveDifference holds the line the
// migration must not cross. A column the file has and this build no longer
// declares cannot be dropped additively, and dropping it would destroy whatever
// it holds — so the store opens, serves, and SAYS SO, rather than silently
// skipping a difference (which is the defect one level up from #194) or guessing
// at a repair.
func TestOpenReportsButDoesNotRepairANonAdditiveDifference(t *testing.T) {
	dsn := preOpenPortsStore(t)

	db, err := sql.Open("sqlite", "file:"+dsn+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE ignored_devices ADD COLUMN retired_reason TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatalf("forge an undeclared column: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	plan, err := store.InspectSchema(dsn)
	if err != nil {
		t.Fatalf("InspectSchema: %v", err)
	}
	if len(plan.Divergent) != 1 ||
		plan.Divergent[0].Table != "ignored_devices" ||
		plan.Divergent[0].Column != "retired_reason" {
		t.Fatalf("want the undeclared column reported as drift, got %+v", plan.Divergent)
	}

	// The store still opens — an undeclared column is inert, because every
	// statement this package issues names its columns explicitly — and the column
	// is still there afterwards.
	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("Open over a store with an undeclared column: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if cols := columnsOf(t, dsn, "ignored_devices"); !hasColumn(cols, "retired_reason") {
		t.Fatalf("the migration must not drop a column it does not declare; got %v", cols)
	}
	// …and the additive work still happened alongside the report.
	if cols := columnsOf(t, dsn, "discovered_devices"); !hasColumn(cols, "open_ports") {
		t.Fatalf("reported drift must not suppress the repair; got %v", cols)
	}
}

// TestOpenIsUnaffectedByATableItDoesNotDeclare: a deployment may keep something
// of its own beside this store. It is none of the migration's business and must
// not appear in a boot report, or every such box logs drift forever.
func TestOpenIsUnaffectedByATableItDoesNotDeclare(t *testing.T) {
	dsn := preOpenPortsStore(t)

	db, err := sql.Open("sqlite", "file:"+dsn+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE somebody_elses_table (k TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatalf("create a foreign table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO somebody_elses_table (k, v) VALUES ('a', 'b')`); err != nil {
		t.Fatalf("seed the foreign table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	plan, err := store.InspectSchema(dsn)
	if err != nil {
		t.Fatalf("InspectSchema: %v", err)
	}
	if len(plan.Divergent) != 0 {
		t.Fatalf("a table this build does not declare is not drift; got %+v", plan.Divergent)
	}

	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ro, err := sql.Open("sqlite", "file:"+dsn+"?mode=ro")
	if err != nil {
		t.Fatalf("reopen raw db: %v", err)
	}
	defer func() { _ = ro.Close() }()
	var v string
	if err := ro.QueryRow(`SELECT v FROM somebody_elses_table WHERE k = 'a'`).Scan(&v); err != nil {
		t.Fatalf("the foreign table must be untouched: %v", err)
	}
	if v != "b" {
		t.Fatalf("the foreign table's row must be untouched; got %q", v)
	}
}

// TestOpenRefusesAColumnItCannotAddAndSaysWhich drives the refusal through a
// REAL open of a real store, rather than constructing the error value by hand.
//
// The distinction matters because the boot's behaviour hangs off the TYPE: a
// typed refusal degrades the feeder to maintenance mode with the reason on
// /healthz, and an untyped one reaches log.Fatal under Restart=always, which is
// a crash loop with the diagnosis only in the journal of a box that is now down.
// A test that builds the error itself proves the type exists; it does not prove
// anything ever produces it.
//
// `jobs.created_at INTEGER NOT NULL` is the store's own DDL, not a hypothetical:
// 108 of this build's declared columns are bare NOT NULL with no DEFAULT.
func TestOpenRefusesAColumnItCannotAddAndSaysWhich(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")
	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+dsn+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE jobs DROP COLUMN created_at`); err != nil {
		t.Fatalf("forge a store missing a bare NOT NULL column: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	before, err := os.ReadFile(dsn)
	if err != nil {
		t.Fatalf("read the forged store: %v", err)
	}

	_, err = store.Open(dsn, store.WallClockMs)
	var blocked *store.SchemaMigrationBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Open must refuse with *SchemaMigrationBlockedError; got %v", err)
	}
	if len(blocked.Blocked) != 1 || blocked.Blocked[0].Table != "jobs" || blocked.Blocked[0].Column != "created_at" {
		t.Fatalf("the refusal must name what it refused; got %+v", blocked.Blocked)
	}
	if !strings.Contains(err.Error(), "jobs.created_at") {
		t.Fatalf("the message an operator reads must name the column; got %q", err.Error())
	}

	// Never-wipe, and all-or-nothing: a refused migration writes nothing.
	after, err := os.ReadFile(dsn)
	if err != nil {
		t.Fatalf("re-read the forged store: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("a refused migration must leave the file byte-identical")
	}
}

// TestOpenLogsTheRepairItPerformed is the observability half, and it is tested
// because it is the half that rots: the mechanism can be right and silent, which
// is precisely how #194 cost seven days — the column was missing, every mirror
// write failed, and the only line anywhere was a per-write error at a call site
// that had already decided such errors were transient.
//
// The claims are: a repair is named, column by column; a conforming store says
// NOTHING (a boot log that reports a no-op every time trains people to skip it);
// and a difference the pass cannot repair is said out loud rather than skipped.
func TestOpenLogsTheRepairItPerformed(t *testing.T) {
	var out bytes.Buffer
	restore := captureLog(t, &out)

	dsn := preOpenPortsStore(t)
	out.Reset()

	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	repair := out.String()
	for _, want := range []string{
		"added missing column discovered_devices.open_ports",
		"existing rows take its declared default",
		dsn,
	} {
		if !strings.Contains(repair, want) {
			t.Fatalf("the boot log must account for the repair; %q absent from:\n%s", want, repair)
		}
	}

	// The second boot repaired nothing, and must therefore say nothing.
	out.Reset()
	again, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if strings.Contains(out.String(), "column") {
		t.Fatalf("a conforming store must not report a repair it did not make:\n%s", out.String())
	}

	// A difference the pass will not repair is reported at every boot, naming the
	// column, so a drifted store is never silently served as a healthy one.
	db, err := sql.Open("sqlite", "file:"+dsn+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE ignored_devices ADD COLUMN retired_reason TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatalf("forge an undeclared column: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	out.Reset()
	drifted, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("Open over a drifted store: %v", err)
	}
	if err := drifted.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(out.String(), "schema drift: ignored_devices.retired_reason") {
		t.Fatalf("unrepairable drift must be reported at every boot; got:\n%s", out.String())
	}
	restore()
}

// captureLog redirects the standard logger into buf for the duration of a test
// and returns a function that restores it early (it is restored on cleanup
// regardless). The store reports through the standard logger because that is
// what the feeder tees into journald and into the console log page.
func captureLog(t *testing.T, buf *bytes.Buffer) func() {
	t.Helper()
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	restore := func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}
	t.Cleanup(restore)
	return restore
}
