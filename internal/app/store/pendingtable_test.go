package store_test

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	_ "modernc.org/sqlite"
)

// pendingtable_test.go covers the schema difference the reports could not
// express: a whole TABLE this build declares that an existing store file does not
// have.
//
// # The asymmetry it closes
//
// A missing COLUMN goes through a plan → apply → verify → report mechanism that
// exists because of #194, and is named with its definition in the boot log and in
// `-store-check`. A missing TABLE went through applySchemaDDL — a bare list of
// `CREATE TABLE IF NOT EXISTS` execs — and `IF NOT EXISTS` is precisely what
// erases the distinction a report would carry. The one place that DID know
// (planSchemaColumns) threw the fact away at a `continue`.
//
// That is not a hypothetical. Box .12 was upgraded onto a build that added both
// `discovered_devices.relay_last_seen` and the whole `device_first_seen` table.
// Its `-store-check` named the pending COLUMN and never the pending TABLE, so the
// one-time, irreversible seed that ran seconds later against 64 devices was
// invisible before the restart and after it.
//
// # Why the fix had to be the SHAPE and not the message
//
// Reporting a pending table through the column planner — by dropping that
// `continue` — classifies `device_first_seen.device_id` as an unaddable PRIMARY
// KEY and `first_seen` as an unaddable NOT NULL, which returns
// SchemaMigrationBlockedError, which the feeder maps to maintenance mode. The
// naive fix puts every store missing the table, INCLUDING every fresh install,
// permanently into maintenance mode. TestAStoreMissingADeclaredTableStillOpens is
// the standing guard on that, and it is the reason this file exists at a distance
// from the message assertions rather than beside them.

// preLedgerTableStore returns the path of a store this build wrote in every
// respect except that `device_first_seen` has been removed from the file — a box
// that has been running since before the ledger existed.
func preLedgerTableStore(t *testing.T) string {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")
	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Dropping the table is how the OLD file shape is forged in a fixture. It is
	// not something any code path does — nothing in this package drops a table —
	// and it destroys no authored data: the fixture's store has none.
	db, err := sql.Open("sqlite", "file:"+dsn+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE device_first_seen`); err != nil {
		t.Fatalf("forge the pre-ledger file shape: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	return dsn
}

// TestInspectSchemaNamesATableThisBuildWillCreate is the pre-restart half: an
// operator running `-store-check` against a box before upgrading it must be told
// that a table is about to arrive, because the boot that creates it also runs the
// one-time seed into it.
func TestInspectSchemaNamesATableThisBuildWillCreate(t *testing.T) {
	dsn := preLedgerTableStore(t)

	m, err := store.InspectSchema(dsn)
	if err != nil {
		t.Fatalf("InspectSchema: %v", err)
	}
	if len(m.Created) != 1 || m.Created[0] != "device_first_seen" {
		t.Fatalf("InspectSchema reported Created=%v, want exactly [device_first_seen]", m.Created)
	}
	// It is a CREATE, not an ALTER, and saying so through the column lists would
	// mis-state the mechanism as well as the fact.
	for _, a := range m.Added {
		if a.Table == "device_first_seen" {
			t.Errorf("a pending table must not be reported as a pending column addition: %+v", a)
		}
	}
	for _, d := range m.Divergent {
		if d.Table == "device_first_seen" {
			t.Errorf("a pending table is repaired seconds later and is not drift: %+v", d)
		}
	}

	// After the boot that creates it, the check has nothing to say.
	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after, err := store.InspectSchema(dsn)
	if err != nil {
		t.Fatalf("second InspectSchema: %v", err)
	}
	if len(after.Created) != 0 {
		t.Fatalf("after the table was created the check still reports Created=%v", after.Created)
	}
}

// TestAFreshInstallReportsNoPendingTables is the gate that keeps the new list
// usable. Every declared table is absent from a store that does not exist yet, so
// an ungated report would make every first boot shout about thirty tables it is
// about to create — which is the noise the original `continue` was written to
// prevent, and the reason the fact was dropped rather than carried.
func TestAFreshInstallReportsNoPendingTables(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")

	// A file that exists and holds NONE of the declared tables: an install, not an
	// upgrade. (A path with no file at all is already answered by InspectSchema's
	// own early return, which this does not go through.)
	db, err := sql.Open("sqlite", "file:"+dsn+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE something_else (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create the fixture file: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	m, err := store.InspectSchema(dsn)
	if err != nil {
		t.Fatalf("InspectSchema: %v", err)
	}
	if len(m.Created) != 0 {
		t.Fatalf("a store holding none of this build's tables is an install, and must not be reported as %d pending "+
			"table(s): %v", len(m.Created), m.Created)
	}
}

// TestOpenLogsTheTableItCreated is the boot half, on the same terms as the column
// migration's log: the repair is named, a store that needed none says nothing,
// and the line is a fact rather than an intention — it is emitted after the DDL
// ran, so a boot whose DDL failed never claims to have created anything.
func TestOpenLogsTheTableItCreated(t *testing.T) {
	var out bytes.Buffer
	restore := captureLog(t, &out)

	dsn := preLedgerTableStore(t)
	out.Reset()

	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	created := out.String()
	for _, want := range []string{
		"created missing table device_first_seen",
		"this build declares it and the file did not have it",
		"created 1 missing table(s)",
		dsn,
	} {
		if !strings.Contains(created, want) {
			t.Fatalf("the boot log must account for the table it created; %q absent from:\n%s", want, created)
		}
	}

	out.Reset()
	again, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if strings.Contains(out.String(), "created missing table") {
		t.Fatalf("a store that already had every table must not report a creation:\n%s", out.String())
	}

	// And a fresh install is silent too, for the reason the plan is gated.
	out.Reset()
	fresh, err := store.Open(filepath.Join(t.TempDir(), "fresh.db"), store.WallClockMs)
	if err != nil {
		t.Fatalf("Open a fresh store: %v", err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if strings.Contains(out.String(), "created missing table") {
		t.Fatalf("a first boot must not narrate the thirty tables it creates:\n%s", out.String())
	}
	restore()
}

// TestAStoreMissingADeclaredTableStillOpens is the standing guard on the shape
// decision, and the most important assertion in this file.
//
// The obvious way to make a pending table visible is to stop skipping it in
// planSchemaColumns and let the column comparison report it. That does report it
// — as `device_first_seen.device_id` being an unaddable PRIMARY KEY and
// `first_seen` an unaddable NOT NULL — which is a SchemaMigrationBlockedError,
// which the feeder maps to maintenance mode. Every box missing the table,
// including every fresh install, would then refuse to serve. This test fails the
// moment anyone tries it.
//
// The two halves are asserted TOGETHER, and that is the point of the test rather
// than an economy. Taken alone, "the store still opens" was already true before
// any of this existed — it is the behaviour of the `continue` that dropped the
// fact — so a test asserting only that guards a wrong fix nobody has written yet
// while proving nothing about the one that shipped. Taken alone, "the check names
// the table" is satisfied by the version that routes the box into maintenance
// mode. The shape decision is the CONJUNCTION: reported AND opened. Only asserting
// both can fail in either direction.
func TestAStoreMissingADeclaredTableStillOpens(t *testing.T) {
	dsn := preLedgerTableStore(t)

	m, err := store.InspectSchema(dsn)
	if err != nil {
		t.Fatalf("a store missing a declared table must be INSPECTABLE, not refused: %v", err)
	}
	if len(m.Created) != 1 || m.Created[0] != "device_first_seen" {
		t.Fatalf("the pending table must be NAMED as well as tolerated; Created=%v. Silence here is box .12's "+
			"exact state: a `-store-check` that reported the pending relay_last_seen COLUMN and never the "+
			"pending device_first_seen TABLE whose one-time irreversible seed ran seconds later", m.Created)
	}
	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("a store missing a declared table must OPEN — the DDL creates it — not be routed to maintenance "+
			"mode: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ledger, err := s.InspectDeviceFirstSeen(t.Context())
	if err != nil {
		t.Fatalf("InspectDeviceFirstSeen: %v", err)
	}
	if !ledger.Present {
		t.Fatalf("the open did not create the table it reported as pending")
	}
}
