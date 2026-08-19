package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// The reference invariant — every plan entry renames a stored ROW, so no
// reference is ever moved on its own — is the one thing about this migration that
// cannot be reached from outside the package on a well-formed store: given a
// planner that only plans row renames, no input produces a plan that violates it.
//
// That is precisely why the guard needs its own tests. An invariant nothing can
// reach is an invariant nothing pins, and the first version of this guard was
// removable in full without a single test noticing. So these two tests reach for
// it directly: one calls the assertion with a plan the planner cannot currently
// produce, and one drives it through planIDRewrites, so deleting the CALL fails
// as loudly as deleting the function.

// idMigrationTestStore seeds a store and returns it, closed by the test's cleanup.
func idMigrationTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "feeder-store.db"), WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SeedDemo(context.Background(),
		"sha256:3a5439d0a1f4b2c6e7889900aabbccddeeff00112233445566778899aabbccdd"); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	return s
}

// TestAssertPlanRenamesStoredRowsRefusesAReferenceOnlyEntry states the invariant
// in the only terms it can be stated in: an entry whose From names no row of its
// own kind renames nothing, so all it can do is move a pointer, and it is
// refused.
//
// The identifier it plants is a real, canonical, STORED id — of a playlist. That
// is exactly the shape the first version of this guard could not see: keyed on the
// dangling parent's VALUE after the rewrite, a reference-only entry and a
// perfectly legitimate rename of the namesake row in another table look
// identical, and the guard fired on the legitimate one. Keyed on whether an entry
// names a row of its own kind, they do not look alike at all.
func TestAssertPlanRenamesStoredRowsRefusesAReferenceOnlyEntry(t *testing.T) {
	ctx := context.Background()
	s := idMigrationTestStore(t)

	const namesake = seedPlaylistID
	const fresh = "01JDDDDDDDDDDDDDDDDDDDDDDD"

	err := assertPlanRenamesStoredRows(ctx, s.db, []IDRewrite{
		{Kind: KindScopeNode, From: namesake, To: fresh},
	})
	if err == nil {
		t.Fatalf("assertPlanRenamesStoredRows accepted an entry that renames no scope node")
	}
	for _, want := range []string{"renames no stored row", string(KindScopeNode), namesake} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}

	// The same identifier under the kind that DOES hold it is fine: the guard is
	// about provenance, not about an identifier's spelling or its use elsewhere.
	if err := assertPlanRenamesStoredRows(ctx, s.db, []IDRewrite{
		{Kind: KindPlaylist, From: namesake, To: fresh},
	}); err != nil {
		t.Fatalf("assertPlanRenamesStoredRows refused a real playlist rename: %v", err)
	}

	// And a real plan over a real store passes, entry for entry — the assertion
	// must not be a refusal that happens to be quiet.
	plan, _, _, _, err := planIDRewrites(ctx, s.db)
	if err != nil {
		t.Fatalf("planIDRewrites: %v", err)
	}
	if err := assertPlanRenamesStoredRows(ctx, s.db, plan); err != nil {
		t.Fatalf("assertPlanRenamesStoredRows refused the planner's own plan: %v", err)
	}
	if err := assertPlanRenamesStoredRows(ctx, s.db, nil); err != nil {
		t.Fatalf("assertPlanRenamesStoredRows on an empty plan: %v", err)
	}
}

// TestPlanIDRewritesReChecksItsOwnPlan pins the WIRING, and with it the one
// property that makes the guard worth wiring in at all: planIDRewrites does not
// trust the id list it built the plan from — it asks the store again.
//
// An assertion fed the very map it is judging asserts nothing. So this drives the
// planner over a store that loses the planned row between the scan and the check.
// The refusal can only appear if planIDRewrites really re-reads and really
// refuses: delete the call and this fails, and so does replacing the second read
// with the ids the planning loop already had.
func TestPlanIDRewritesReChecksItsOwnPlan(t *testing.T) {
	ctx := context.Background()
	s := idMigrationTestStore(t)

	// Something to plan: the screen scope node under its pre-DAT-005a spelling,
	// as an older build's store holds it.
	const legacyScreen = "01J8Z4DEMOSCREENFIRSTPHOTN"
	if _, err := s.db.ExecContext(ctx, `UPDATE scope_nodes SET id = ? WHERE id = ?`,
		legacyScreen, seedScreenScopeNodeID); err != nil {
		t.Fatalf("regress the screen node's id: %v", err)
	}

	q := &vanishingRowQueryer{
		db:    s.db,
		table: "scope_nodes",
		gone:  `DELETE FROM scope_nodes WHERE id = '` + legacyScreen + `'`,
	}
	plan, _, _, _, err := planIDRewrites(ctx, q)
	if err == nil {
		t.Fatalf("planIDRewrites returned %+v for a row that no longer exists; it does not re-check what it planned", plan)
	}
	if !strings.Contains(err.Error(), "renames no stored row") {
		t.Fatalf("planIDRewrites failed with %v, want the reference-invariant refusal", err)
	}
	if !strings.Contains(err.Error(), legacyScreen) {
		t.Fatalf("the refusal %v does not name the entry it refused (%s)", err, legacyScreen)
	}
}

// vanishingRowQueryer forwards every read to a real database, and deletes one row
// just before the SECOND read of the named table — so the plan is built from the
// store as it was and every later read sees a store the plan no longer describes.
// Nothing in production behaves this way; it exists to make "the planner reads
// again" observable rather than a claim in a comment.
type vanishingRowQueryer struct {
	db    *sql.DB
	table string
	gone  string
	seen  int
}

func (v *vanishingRowQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, v.table) {
		v.seen++
		if v.seen == 2 {
			if _, err := v.db.ExecContext(ctx, v.gone); err != nil {
				return nil, err
			}
		}
	}
	return v.db.QueryContext(ctx, query, args...)
}

// TestPlanIDRewritesContinuesPastAnUnreadableTable is the sweep's degrade rule,
// on the case the file-level tests cannot forge: a table that IS there and
// cannot be read.
//
// The sweep walks thirteen tables and used to return the first read error, so a
// single unreadable table discarded every table it had already read AND every
// table it had not reached — and `-store-check` presented the result as the
// authoritative list of what the boot would rewrite. Here scope_nodes (table one
// of thirteen) refuses its read while a genuinely non-canonical id sits in
// playlists (table two), and the plan must name it.
func TestPlanIDRewritesContinuesPastAnUnreadableTable(t *testing.T) {
	ctx := context.Background()
	s := idMigrationTestStore(t)

	const legacyPlaylist = "01J8Z1DEMOPLAYLISTFIRSTPHO"
	if _, err := s.db.ExecContext(ctx, `UPDATE playlists SET id = ? WHERE id = ?`,
		legacyPlaylist, seedPlaylistID); err != nil {
		t.Fatalf("regress the playlist id: %v", err)
	}

	q := &refusingTableQueryer{db: s.db, table: "scope_nodes"}
	plan, blocked, _, unreadable, err := planIDRewrites(ctx, q)
	if err != nil {
		t.Fatalf("planIDRewrites aborted on an unreadable table instead of recording it: %v", err)
	}
	if len(blocked) != 0 {
		t.Fatalf("blocked = %+v, want none", blocked)
	}
	if len(unreadable) != 1 || unreadable[0].Kind != KindScopeNode || unreadable[0].Absent {
		t.Fatalf("unreadable = %+v, want exactly scope_nodes recorded as present-but-unreadable", unreadable)
	}
	found := false
	for _, rw := range plan {
		if rw.From == legacyPlaylist {
			found = true
		}
	}
	if !found {
		t.Fatalf("the sweep stopped at table one of thirteen: plan %+v does not name the non-canonical playlist id %s",
			plan, legacyPlaylist)
	}
}

// TestPlanIDRewritesSeparatesAnAbsentTableFromAnUnreadableOne: the two send an
// operator to opposite places, so the planner decides which it is by asking the
// file rather than by reading the driver's error text. An absent table holds no
// ids and the very next open creates it; the report used to render that as "the
// feeder will refuse to start".
func TestPlanIDRewritesSeparatesAnAbsentTableFromAnUnreadableOne(t *testing.T) {
	ctx := context.Background()
	s := idMigrationTestStore(t)

	if _, err := s.db.ExecContext(ctx, `DROP TABLE casts`); err != nil {
		t.Fatalf("drop the casts table: %v", err)
	}
	_, _, _, unreadable, err := planIDRewrites(ctx, s.db)
	if err != nil {
		t.Fatalf("planIDRewrites: %v", err)
	}
	if len(unreadable) != 1 || unreadable[0].Kind != KindCast || !unreadable[0].Absent {
		t.Fatalf("unreadable = %+v, want the casts table recorded as ABSENT", unreadable)
	}
}

// TestMigrateRowIDsRefusesAPartialSweep is the other half of the same change: a
// DRY RUN may report a partial reading, and the WRITE may never act on one. By
// the time MigrateRowIDs runs, applySchemaDDL has created every declared table,
// so a table it cannot read is a fault — and "every non-canonical id in the
// tables I could read" is not a plan.
func TestMigrateRowIDsRefusesAPartialSweep(t *testing.T) {
	ctx := context.Background()
	s := idMigrationTestStore(t)

	if _, err := s.db.ExecContext(ctx, `DROP TABLE casts`); err != nil {
		t.Fatalf("drop the casts table: %v", err)
	}
	if _, err := s.MigrateRowIDs(ctx); err == nil {
		t.Fatal("MigrateRowIDs acted on a sweep that skipped a table")
	} else if !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("MigrateRowIDs failed with %v, want a refusal naming the tables it could not read", err)
	}
}

// refusingTableQueryer forwards every read to a real database except the id read
// of one named table, which it fails — the "the table is there and I cannot read
// it" case, which a file fixture cannot produce for a table whose id column is a
// PRIMARY KEY (SQLite refuses to drop one).
type refusingTableQueryer struct {
	db    *sql.DB
	table string
}

func (r *refusingTableQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "SELECT id FROM "+r.table) {
		return nil, fmt.Errorf("disk I/O error (10)")
	}
	return r.db.QueryContext(ctx, query, args...)
}
