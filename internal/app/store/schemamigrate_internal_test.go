package store

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// schemamigrate_internal_test.go holds the claims that can only be made from
// inside the package: that the DECLARED shape is genuinely the DDL's own, that
// every column this build declares can be retrofitted onto a store that lacks it
// WITHOUT losing anything the declaration carries, that the ones which cannot be
// retrofitted refuse loudly, and that a conforming store is not merely left
// unchanged but is never written to at all.

// updateSchemaGolden regenerates testdata/declared-schema.golden. See
// TestDeclaredSchemaGoldenIsCurrent for why the golden exists.
var updateSchemaGolden = flag.Bool("update-schema-golden", false,
	"rewrite internal/app/store/testdata/declared-schema.golden from this build's DDL")

// xinfoColumns reads a table's columns through PRAGMA table_xinfo with the
// test's OWN query, deliberately not through this package's reader.
//
// That independence is the point. The migration compares a model of the DDL
// against the file, and if BOTH readings go through one blind pragma then a
// column neither of them can see is invisible to the comparison and to every
// test written on top of it — which is exactly how a GENERATED column would
// reproduce #194 with the guard silent. table_xinfo is the reading that sees
// them; table_info is not.
func xinfoColumns(t *testing.T, q queryer, table string) map[string]columnShape {
	t.Helper()
	rows, err := q.QueryContext(context.Background(), `PRAGMA table_xinfo(`+quoteIdent(table)+`)`)
	if err != nil {
		t.Fatalf("PRAGMA table_xinfo(%s): %v", table, err)
	}
	defer rows.Close()

	out := map[string]columnShape{}
	for rows.Next() {
		var (
			cid     int
			c       columnShape
			notNull int
		)
		if err := rows.Scan(&cid, &c.Name, &c.Type, &notNull, &c.Dflt, &c.PK, &c.Hidden); err != nil {
			t.Fatalf("scan table_xinfo(%s): %v", table, err)
		}
		c.NotNull = notNull != 0
		out[c.Name] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_xinfo(%s): %v", table, err)
	}
	return out
}

// createSQLOf reads a table's stored CREATE statement with the test's own query.
func createSQLOf(t *testing.T, q queryer, table string) string {
	t.Helper()
	sqlText, err := readCreateSQL(context.Background(), q, table)
	if err != nil {
		t.Fatalf("read the declaration of %s: %v", table, err)
	}
	return sqlText
}

// indexSignature is a table's index surface — every index SQLite holds for it,
// with whether it is UNIQUE and where it came from ('c' a CREATE INDEX, 'u' a
// UNIQUE constraint, 'pk' a primary key).
//
// It is compared alongside the column declarations because a UNIQUE constraint
// lives in an INDEX, not in the column's pragma row: a column retrofitted
// without its UNIQUE looks identical to a correct one through table_xinfo, and
// only the missing auto-index gives it away.
func indexSignature(t *testing.T, q queryer, table string) []string {
	t.Helper()
	type indexRow struct {
		name            string
		origin          string
		unique, partial int
	}
	// Read the list to completion and CLOSE it before asking about any one index.
	// Every handle in this package is capped at a single connection, so a nested
	// query issued while these rows are still open waits forever for a connection
	// the outer iteration is holding.
	var indexes []indexRow
	func() {
		rows, err := q.QueryContext(context.Background(), `PRAGMA index_list(`+quoteIdent(table)+`)`)
		if err != nil {
			t.Fatalf("PRAGMA index_list(%s): %v", table, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				seq int
				ix  indexRow
			)
			if err := rows.Scan(&seq, &ix.name, &ix.unique, &ix.origin, &ix.partial); err != nil {
				t.Fatalf("scan index_list(%s): %v", table, err)
			}
			indexes = append(indexes, ix)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate index_list(%s): %v", table, err)
		}
	}()

	var out []string
	for _, ix := range indexes {
		// The auto-index NAME carries a serial number that depends on the order
		// constraints were declared in, and a retrofitted table can legitimately
		// number them differently. What must match is the SHAPE: unique-ness,
		// origin, and the columns covered.
		var cols []string
		func() {
			icols, err := q.QueryContext(context.Background(), `PRAGMA index_info(`+quoteIdent(ix.name)+`)`)
			if err != nil {
				t.Fatalf("PRAGMA index_info(%s): %v", ix.name, err)
			}
			defer icols.Close()
			for icols.Next() {
				var (
					seqno, cid int
					colName    sql.NullString
				)
				if err := icols.Scan(&seqno, &cid, &colName); err != nil {
					t.Fatalf("scan index_info(%s): %v", ix.name, err)
				}
				cols = append(cols, colName.String)
			}
			if err := icols.Err(); err != nil {
				t.Fatalf("iterate index_info(%s): %v", ix.name, err)
			}
		}()
		out = append(out, fmt.Sprintf("unique=%d origin=%s partial=%d cols=%v", ix.unique, ix.origin, ix.partial, cols))
	}
	sort.Strings(out)
	return out
}

// TestDeclaredSchemaMatchesAFreshStore keeps the migration's model honest about
// the DDL it claims to describe.
//
// The migration compares the file against a model built by applySchemaDDL. If a
// table or a column were ever created somewhere OTHER than applySchemaDDL — an
// extra db.Exec added to Open, say — the model would not know about it, the
// migration would never converge it, and #194 would be back with a new hiding
// place. The same is true of a column the model's READING cannot see: this test
// takes the on-disk side through its own PRAGMA table_xinfo (see xinfoColumns)
// so that a model built on a blinder pragma fails here rather than agreeing with
// itself.
//
// What it does NOT prove is that a missing column can be retrofitted faithfully;
// that is TestEveryAddableColumnComesBackIdenticalAfterAnUpgrade, below.
func TestDeclaredSchemaMatchesAFreshStore(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")

	s, err := Open(dsn, WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	declared, err := declaredSchema(ctx)
	if err != nil {
		t.Fatalf("declaredSchema: %v", err)
	}

	actual, err := tableNames(ctx, s.db)
	if err != nil {
		t.Fatalf("read the fresh store's tables: %v", err)
	}
	if len(actual) != len(declared) {
		t.Fatalf("a fresh store has %d tables and the declared model has %d; a table created outside "+
			"applySchemaDDL is invisible to the migration.\n  on disk: %v", len(actual), len(declared), actual)
	}
	for _, table := range actual {
		want, ok := declared[table]
		if !ok {
			t.Fatalf("table %q exists in a fresh store but not in the declared model: its DDL runs somewhere "+
				"other than applySchemaDDL, so the migration can never converge its columns", table)
		}
		got := xinfoColumns(t, s.db, table)
		if len(got) != len(want.Columns) {
			t.Errorf("%s has %d columns on disk (read through PRAGMA table_xinfo) and %d in the declared model; "+
				"a column the model's reading cannot see is one the migration can never add",
				table, len(got), len(want.Columns))
			continue
		}
		for _, wantCol := range want.Columns {
			gotCol, ok := got[wantCol.Name]
			if !ok {
				t.Errorf("%s.%s is in the declared model and not on disk", table, wantCol.Name)
				continue
			}
			if reason := incompatible(wantCol, gotCol); reason != "" {
				t.Errorf("%s.%s differs between a fresh store and the declared model: %s", table, wantCol.Name, reason)
			}
			if want.Decl[wantCol.Name] == "" {
				t.Errorf("%s.%s has no declaration text in the model; the ALTER would have nothing to append",
					table, wantCol.Name)
			}
		}
	}

	// The same statement from the migration's own point of view: a store this
	// build just created has nothing to add and nothing to report.
	adds, blocked, divergent, err := planSchemaColumns(ctx, s.db, declared)
	if err != nil {
		t.Fatalf("planSchemaColumns over a fresh store: %v", err)
	}
	if len(adds) != 0 || len(blocked) != 0 || len(divergent) != 0 {
		t.Fatalf("a fresh store must be conforming; adds=%+v blocked=%+v divergent=%+v", adds, blocked, divergent)
	}
}

// TestEveryDeclaredTableIsReadableAsColumns pins the CREATE-TABLE reader
// (schemasql.go) to the DDL it actually has to read. The declaration text is
// what gets appended to an ALTER, so a table whose spelling the reader cannot
// follow is a table whose columns can never be retrofitted — and the failure
// belongs in the change that introduces the spelling, not on a box.
func TestEveryDeclaredTableIsReadableAsColumns(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")
	s, err := Open(dsn, WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	tables, err := tableNames(ctx, s.db)
	if err != nil {
		t.Fatalf("tableNames: %v", err)
	}
	if len(tables) < 20 {
		t.Fatalf("this store declares %d tables; the fixture is not exercising the real schema", len(tables))
	}
	for _, table := range tables {
		decls, order, _, err := tableDecls(createSQLOf(t, s.db, table))
		if err != nil {
			t.Errorf("%s: %v", table, err)
			continue
		}
		want := xinfoColumns(t, s.db, table)
		if len(order) != len(want) {
			t.Errorf("%s: the reader found %d columns (%v) and SQLite reports %d", table, len(order), order, len(want))
			continue
		}
		for _, name := range order {
			if _, ok := want[name]; !ok {
				t.Errorf("%s: the reader found a column %q SQLite does not report", table, name)
			}
			if decls[name] == "" {
				t.Errorf("%s.%s: the reader found the name but no declaration", table, name)
			}
		}
	}
}

// forgeMissingColumns rebuilds each named table WITHOUT the given columns, which
// is how a store an earlier build created is reproduced exactly: the table
// exists, it holds its other columns, and `CREATE TABLE IF NOT EXISTS` will
// never add the missing one.
//
// It rebuilds from the table's OWN stored CREATE text with items removed, so
// every surviving column keeps the spelling the DDL gave it.
func forgeMissingColumns(t *testing.T, dsn string, missing map[string][]string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dsn+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()

	tables := make([]string, 0, len(missing))
	for table := range missing {
		tables = append(tables, table)
	}
	sort.Strings(tables)

	for _, table := range tables {
		createSQL, err := readCreateSQL(context.Background(), db, table)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		items, err := createTableItems(createSQL)
		if err != nil {
			t.Fatalf("split %s: %v", table, err)
		}
		drop := map[string]bool{}
		for _, c := range missing[table] {
			drop[c] = true
		}
		var keep []string
		for _, item := range items {
			name, quoted := leadingIdent(item)
			if !quoted && tableConstraintKeywords[strings.ToUpper(name)] {
				keep = append(keep, item)
				continue
			}
			if drop[name] {
				continue
			}
			keep = append(keep, item)
		}
		if _, err := db.Exec(`DROP TABLE ` + quoteIdent(table)); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
		if _, err := db.Exec(`CREATE TABLE ` + quoteIdent(table) + ` (` + strings.Join(keep, ", ") + `)`); err != nil {
			t.Fatalf("re-create %s without %v: %v", table, missing[table], err)
		}
	}
}

// tableItemSet is a table's declaration as a comparable SET of items — one per
// column, one per table constraint — normalized. A set rather than a list
// because ALTER TABLE ADD COLUMN appends, so a retrofitted table carries its
// columns in a different ORDER than a fresh one and is otherwise the same table.
func tableItemSet(t *testing.T, q queryer, table string) map[string]bool {
	t.Helper()
	items, err := createTableItems(createSQLOf(t, q, table))
	if err != nil {
		t.Fatalf("split %s: %v", table, err)
	}
	out := map[string]bool{}
	for _, item := range items {
		out[item] = true
	}
	return out
}

// TestEveryAddableColumnComesBackIdenticalAfterAnUpgrade is the anti-regression
// guard for #194, stated as a property over the WHOLE declared schema rather
// than over the one column that happened to be reported.
//
// For every column this build declares and can retrofit, it forges the store an
// earlier build would have left — the table present, that column absent — opens
// it with this build, and requires the result to be indistinguishable from a
// fresh store: the same declaration text, verbatim, and the same index surface.
//
// "Indistinguishable" rather than "present" is the whole point, and it is what
// the previous guard could not say. A column declared UNIQUE, COLLATE, CHECK,
// REFERENCES or GENERATED came back stripped of all of it when the ALTER was
// rendered from PRAGMA table_info, and every later comparison read through that
// same blind pragma and called the result correct — a fleet permanently split
// between boxes that enforce a constraint and boxes that do not, with nothing
// anywhere saying so.
func TestEveryAddableColumnComesBackIdenticalAfterAnUpgrade(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	reference := filepath.Join(dir, "fresh.db")
	ref, err := Open(reference, WallClockMs)
	if err != nil {
		t.Fatalf("Open reference: %v", err)
	}
	t.Cleanup(func() { _ = ref.Close() })

	declared, err := declaredSchema(ctx)
	if err != nil {
		t.Fatalf("declaredSchema: %v", err)
	}

	missing := map[string][]string{}
	addable := 0
	for table, shape := range declared {
		for _, c := range shape.Columns {
			if whyNotAddable(c, shape.Decl[c.Name]) != "" {
				continue
			}
			missing[table] = append(missing[table], c.Name)
			addable++
		}
	}
	if addable < 30 {
		t.Fatalf("only %d declared columns are addable; this property has gone vacuous", addable)
	}
	t.Logf("forging the absence of %d addable column(s) across %d table(s)", addable, len(missing))

	upgraded := filepath.Join(dir, "upgraded.db")
	old, err := Open(upgraded, WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	forgeMissingColumns(t, upgraded, missing)

	// The plan an operator would be shown must name every one of them, and only
	// them, before anything is written.
	plan, err := InspectSchema(upgraded)
	if err != nil {
		t.Fatalf("InspectSchema over the forged store: %v", err)
	}
	if len(plan.Added) != addable {
		t.Fatalf("the plan names %d column(s) and %d are missing", len(plan.Added), addable)
	}

	migrated, err := Open(upgraded, WallClockMs)
	if err != nil {
		t.Fatalf("Open over the forged store: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })

	for table := range declared {
		wantItems := tableItemSet(t, ref.db, table)
		gotItems := tableItemSet(t, migrated.db, table)
		for item := range wantItems {
			if !gotItems[item] {
				t.Errorf("%s: a fresh store declares %q and the upgraded one does not — the retrofit dropped "+
					"part of the declaration, which no later comparison can see", table, item)
			}
		}
		for item := range gotItems {
			if !wantItems[item] {
				t.Errorf("%s: the upgraded store carries %q and a fresh one does not", table, item)
			}
		}
		wantIdx, gotIdx := indexSignature(t, ref.db, table), indexSignature(t, migrated.db, table)
		if strings.Join(wantIdx, "\n") != strings.Join(gotIdx, "\n") {
			t.Errorf("%s: index surface differs after the retrofit\n  fresh:    %v\n  upgraded: %v",
				table, wantIdx, gotIdx)
		}
		wantCols, gotCols := xinfoColumns(t, ref.db, table), xinfoColumns(t, migrated.db, table)
		if len(wantCols) != len(gotCols) {
			t.Errorf("%s: a fresh store has %d columns and the upgraded one %d", table, len(wantCols), len(gotCols))
		}
	}

	// And the upgraded store is now conforming to its own migration: a second
	// boot finds nothing to do and reports no drift.
	after, err := InspectSchema(upgraded)
	if err != nil {
		t.Fatalf("InspectSchema after the migration: %v", err)
	}
	if len(after.Added) != 0 || len(after.Divergent) != 0 {
		t.Fatalf("the migrated store must be conforming; got %+v", after)
	}
}

// TestEveryUnaddableColumnRefusesTheOpenByName is the other half of the same
// property. 108 of this store's declared columns are bare NOT NULL with no
// DEFAULT, which SQLite cannot retrofit onto a table that exists. Every one of
// them must produce a REFUSAL that names the column — not a partial migration,
// not a store that opens and then fails on every statement naming it.
func TestEveryUnaddableColumnRefusesTheOpenByName(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	declared, err := declaredSchema(ctx)
	if err != nil {
		t.Fatalf("declaredSchema: %v", err)
	}

	// One column per table, so every refusal is a separate, nameable case and the
	// primary-key columns (which cannot be removed without changing the table's
	// identity) stay out of it.
	cases := map[string]string{}
	for table, shape := range declared {
		for _, c := range shape.Columns {
			if c.PK != 0 || whyNotAddable(c, shape.Decl[c.Name]) == "" {
				continue
			}
			cases[table] = c.Name
			break
		}
	}
	if len(cases) < 10 {
		t.Fatalf("only %d tables have an unaddable column; this property has gone vacuous", len(cases))
	}

	tables := make([]string, 0, len(cases))
	for table := range cases {
		tables = append(tables, table)
	}
	sort.Strings(tables)

	for _, table := range tables {
		column := cases[table]
		t.Run(table+"."+column, func(t *testing.T) {
			dsn := filepath.Join(dir, table+".db")
			s, err := Open(dsn, WallClockMs)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			forgeMissingColumns(t, dsn, map[string][]string{table: {column}})
			before, err := os.ReadFile(dsn)
			if err != nil {
				t.Fatalf("read the forged store: %v", err)
			}

			_, err = Open(dsn, WallClockMs)
			var blocked *SchemaMigrationBlockedError
			if !errors.As(err, &blocked) {
				t.Fatalf("Open must refuse with *SchemaMigrationBlockedError; got %v", err)
			}
			named := false
			for _, b := range blocked.Blocked {
				if b.Table == table && b.Column == column {
					named = true
					if b.Reason == "" {
						t.Fatalf("a refusal with no reason is a refusal nobody can act on")
					}
				}
			}
			if !named {
				t.Fatalf("the refusal must name %s.%s; got %+v", table, column, blocked.Blocked)
			}
			// All-or-nothing: a refused migration writes nothing at all.
			after, err := os.ReadFile(dsn)
			if err != nil {
				t.Fatalf("re-read the forged store: %v", err)
			}
			if string(before) != string(after) {
				t.Fatalf("a refused migration must leave the file untouched")
			}

			// And `-store-check` refuses in the same terms, before any open.
			if _, err := InspectSchema(dsn); !errors.As(err, &blocked) {
				t.Fatalf("InspectSchema must refuse the same way; got %v", err)
			}
		})
	}
}

// TestSchemaMigrationOpensNoWriteTransactionOnAConformingStore is the safety
// claim that lets this run unattended at every boot, and it is proved rather
// than asserted: the migration is run against a READ-ONLY handle. A store that
// already conforms must complete without complaint, because it never begins a
// transaction — and `_txlock=immediate` on a read-only database would fail at
// BEGIN if it did.
//
// The second half is the control. Point the same read-only handle at a DRIFTED
// store and the call must fail, which is what proves the first half is measuring
// something: if the migration never wrote under any circumstances, the read-only
// success would be vacuous.
func TestSchemaMigrationOpensNoWriteTransactionOnAConformingStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	conforming := filepath.Join(dir, "conforming.db")
	s, err := Open(conforming, WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ro, err := sql.Open("sqlite", "file:"+conforming+"?mode=ro&_txlock=immediate")
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer func() { _ = ro.Close() }()
	ro.SetMaxOpenConns(1)

	m, err := migrateSchemaColumns(ctx, ro)
	if err != nil {
		t.Fatalf("migrateSchemaColumns over a read-only conforming store must succeed without writing: %v", err)
	}
	if len(m.Added) != 0 || len(m.Divergent) != 0 {
		t.Fatalf("a conforming store must need nothing; got %+v", m)
	}

	// The control: the same read-only handle over a store that IS missing a
	// declared column has to fail, because the repair is a write.
	drifted := filepath.Join(dir, "drifted.db")
	d, err := Open(drifted, WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+drifted+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`ALTER TABLE discovered_devices DROP COLUMN open_ports`); err != nil {
		t.Fatalf("forge the drift: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	roDrifted, err := sql.Open("sqlite", "file:"+drifted+"?mode=ro&_txlock=immediate")
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer func() { _ = roDrifted.Close() }()
	roDrifted.SetMaxOpenConns(1)

	_, err = migrateSchemaColumns(ctx, roDrifted)
	if err == nil {
		t.Fatalf("a drifted store must fail against a read-only handle; otherwise the conforming case above proves nothing")
	}
	// And it fails as the typed refusal, so the boot degrades to maintenance mode
	// rather than crash-looping on an untyped error from mid-ALTER.
	var blocked *SchemaMigrationBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("a failure while writing must be *SchemaMigrationBlockedError, or the boot log.Fatals on it; got %v", err)
	}
	if blocked.Cause == nil {
		t.Fatalf("a mid-migration failure must carry its cause; got %+v", blocked)
	}
}

// TestPlanRefusesAColumnSQLiteCannotAdd covers the guard that must REFUSE rather
// than half-apply, case by case. Each of these is a real SQLite restriction,
// probed against this build's driver — and three of them (a non-constant
// default, CURRENT_TIMESTAMP, a STORED generated column) fail only on a table
// that HAS ROWS, so leaving them to SQLite would let the same build succeed on
// an empty box and refuse on a populated one.
func TestPlanRefusesAColumnSQLiteCannotAdd(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")

	s, err := Open(dsn, WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cases := []struct {
		name string
		col  columnShape
		decl string
		want string
	}{
		{
			name: "not null with no default",
			col:  columnShape{Name: "sealed_at", Type: "INTEGER", NotNull: true},
			decl: "sealed_at INTEGER NOT NULL",
			want: "NOT NULL",
		},
		{
			name: "primary key",
			col:  columnShape{Name: "surrogate", Type: "INTEGER", PK: 1},
			decl: "surrogate INTEGER PRIMARY KEY",
			want: "PRIMARY KEY",
		},
		{
			name: "non-constant default",
			col: columnShape{Name: "stamped_at", Type: "INTEGER", NotNull: true,
				Dflt: sql.NullString{String: "(strftime('%s','now'))", Valid: true}},
			decl: "stamped_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))",
			want: "not a constant",
		},
		{
			// Reported by PRAGMA table_info WITHOUT parentheses, so the
			// parenthesized-expression test above does not see it. The ALTER
			// succeeds on an empty table and fails on one with rows.
			name: "CURRENT_TIMESTAMP default",
			col: columnShape{Name: "last_touched", Type: "TEXT", NotNull: true,
				Dflt: sql.NullString{String: "CURRENT_TIMESTAMP", Valid: true}},
			decl: "last_touched TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP",
			want: "CURRENT_TIMESTAMP",
		},
		{
			// Invisible to the column's pragma row: UNIQUE lives in an index.
			name: "unique",
			col: columnShape{Name: "native_key", Type: "TEXT", NotNull: true,
				Dflt: sql.NullString{String: "''", Valid: true}},
			decl: "native_key TEXT NOT NULL DEFAULT '' UNIQUE COLLATE NOCASE",
			want: "UNIQUE",
		},
		{
			name: "stored generated",
			col:  columnShape{Name: "port_count", Type: "INTEGER", Hidden: 3},
			decl: "port_count INTEGER GENERATED ALWAYS AS (json_array_length(open_ports)) STORED",
			want: "STORED",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := mustShape(t, ctx, s.db, "ignored_devices")
			base.Columns = append(base.Columns, tc.col)
			base.Decl[tc.col.Name] = tc.decl
			adds, blocked, _, err := planSchemaColumns(ctx, s.db, map[string]tableShape{"ignored_devices": base})
			if err != nil {
				t.Fatalf("planSchemaColumns: %v", err)
			}
			if len(adds) != 0 {
				t.Fatalf("a column SQLite cannot add must not be planned as an addition; got %+v", adds)
			}
			if len(blocked) != 1 || blocked[0].Column != tc.col.Name {
				t.Fatalf("want %s blocked with a reason, got %+v", tc.col.Name, blocked)
			}
			if !strings.Contains(blocked[0].Reason, tc.want) {
				t.Fatalf("the reason must say what SQLite objects to (%q); got %q", tc.want, blocked[0].Reason)
			}

			// The control that keeps this from being a test of a string: SQLite
			// really does refuse the statement this column would have produced,
			// against a table with a row in it.
			raw, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "probe.db"))
			if err != nil {
				t.Fatalf("open probe: %v", err)
			}
			defer func() { _ = raw.Close() }()
			if _, err := raw.Exec(`CREATE TABLE t (open_ports TEXT NOT NULL DEFAULT '[]')`); err != nil {
				t.Fatalf("create probe table: %v", err)
			}
			if _, err := raw.Exec(`INSERT INTO t (open_ports) VALUES ('[]')`); err != nil {
				t.Fatalf("seed probe table: %v", err)
			}
			if _, err := raw.Exec(`ALTER TABLE t ADD COLUMN ` + tc.decl); err == nil {
				t.Fatalf("SQLite accepted %q on a table with rows; this case is no longer a restriction and the "+
					"guard is refusing something it should allow", tc.decl)
			} else {
				t.Logf("SQLite refuses it too: %v", err)
			}
		})
	}
}

// TestPlanReportsAChangedColumnWithoutTouchingIt: a column whose declared TYPE
// has changed cannot be fixed by adding anything, and rewriting it in place
// would be a table rebuild over stored values. It is reported and left.
func TestPlanReportsAChangedColumnWithoutTouchingIt(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")

	s, err := Open(dsn, WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	retyped := mustShape(t, ctx, s.db, "discovered_devices")
	for i := range retyped.Columns {
		if retyped.Columns[i].Name == "last_seen" {
			retyped.Columns[i].Type = "TEXT"
		}
	}

	adds, blocked, divergent, err := planSchemaColumns(ctx, s.db, map[string]tableShape{"discovered_devices": retyped})
	if err != nil {
		t.Fatalf("planSchemaColumns: %v", err)
	}
	if len(adds) != 0 || len(blocked) != 0 {
		t.Fatalf("a retyped column is neither an addition nor a refusal; adds=%+v blocked=%+v", adds, blocked)
	}
	if len(divergent) != 1 || divergent[0].Column != "last_seen" {
		t.Fatalf("want last_seen reported as drift, got %+v", divergent)
	}
}

// TestPlanReportsATableConstraintTheFileDoesNotCarry: a table-level constraint
// (a composite PRIMARY KEY, a table UNIQUE, a table CHECK) is #194's shape one
// level up — enforced on every fresh install, absent on every box that predates
// it, and invisible to a column-by-column comparison. ALTER TABLE cannot add
// one, so it is reported at every boot and left alone.
func TestPlanReportsATableConstraintTheFileDoesNotCarry(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")

	s, err := Open(dsn, WallClockMs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	declared := mustShape(t, ctx, s.db, "discovered_devices")
	declared.Constraints = append(declared.Constraints, "UNIQUE (driver, native_id)")

	adds, blocked, divergent, err := planSchemaColumns(ctx, s.db, map[string]tableShape{"discovered_devices": declared})
	if err != nil {
		t.Fatalf("planSchemaColumns: %v", err)
	}
	if len(adds) != 0 || len(blocked) != 0 {
		t.Fatalf("a table constraint is neither an addition nor a per-column refusal; adds=%+v blocked=%+v", adds, blocked)
	}
	if len(divergent) != 1 || !strings.Contains(divergent[0].Reason, "UNIQUE (driver, native_id)") {
		t.Fatalf("want the missing table constraint reported, got %+v", divergent)
	}
}

// TestDeclaredSchemaGoldenIsCurrent is the AUTHORING guard, and the half this
// change would otherwise have skipped: every mechanism here is ENFORCEMENT, and
// enforcement cannot tell an author, in the pull request, that the column they
// just wrote is one no existing box can ever be given.
//
// 108 of this store's declared columns are bare `NOT NULL` with no DEFAULT —
// `first_seen INTEGER NOT NULL`, `created_at INTEGER NOT NULL`, `body TEXT NOT
// NULL`. That is the dominant style, so writing one more in it is the natural
// thing to do, every test stays green (a fresh store always has the column), and
// the failure lands on the fleet: every box that predates the column refuses to
// open, and the sentence naming it is in the journal of a box that is now in
// maintenance mode.
//
// The golden is the seam that makes that visible at authoring time. Any change
// to the declared schema fails this test; a change that ADDS a column to a table
// that already exists fails it with the rule the column has to satisfy.
func TestDeclaredSchemaGoldenIsCurrent(t *testing.T) {
	ctx := context.Background()
	declared, err := declaredSchema(ctx)
	if err != nil {
		t.Fatalf("declaredSchema: %v", err)
	}
	current := renderSchemaGolden(declared)

	const goldenPath = "testdata/declared-schema.golden"
	if *updateSchemaGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("create testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(current), 0o644); err != nil {
			t.Fatalf("write the golden: %v", err)
		}
		t.Logf("rewrote %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read the golden: %v\nregenerate it with:\n"+
			"    go test ./internal/app/store -run TestDeclaredSchemaGoldenIsCurrent -update-schema-golden", err)
	}
	if string(want) == current {
		return
	}

	// The schema changed. Say what changed, and — for a column added to a table
	// that already exists — whether an existing box can ever be given it.
	wantLines := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(want)), "\n") {
		wantLines[line] = true
	}
	goldenTables := map[string]bool{}
	for line := range wantLines {
		if table, _, ok := strings.Cut(line, "\t"); ok {
			goldenTables[table] = true
		}
	}

	var unaddable, other []string
	for _, line := range strings.Split(strings.TrimSpace(current), "\n") {
		if wantLines[line] {
			continue
		}
		table, rest, _ := strings.Cut(line, "\t")
		column, _, _ := strings.Cut(rest, "\t")
		if !goldenTables[table] {
			// A brand-new TABLE is created whole by CREATE TABLE, so its columns
			// are never retrofitted and the rule below does not apply to them.
			other = append(other, line)
			continue
		}
		shape, ok := declared[table]
		if !ok {
			other = append(other, line)
			continue
		}
		var col columnShape
		found := false
		for _, c := range shape.Columns {
			if c.Name == column {
				col, found = c, true
				break
			}
		}
		if !found {
			other = append(other, line)
			continue
		}
		if reason := whyNotAddable(col, shape.Decl[column]); reason != "" {
			unaddable = append(unaddable, fmt.Sprintf("  %s.%s — %s", table, column, reason))
			continue
		}
		other = append(other, line)
	}

	if len(unaddable) > 0 {
		t.Fatalf("this change adds a column to a table that already exists on every deployed box, and SQLite "+
			"cannot retrofit it — every such box would REFUSE TO OPEN its store (maintenance mode, not a crash, "+
			"but not serving either):\n%s\n\n"+
			"A column added to an existing table must be nullable or carry a CONSTANT DEFAULT, and must not be "+
			"PRIMARY KEY, UNIQUE, STORED-generated, or defaulted to CURRENT_TIME/DATE/TIMESTAMP. Give it a "+
			"DEFAULT (every existing row takes it), or rebuild the table under a new platform schema epoch.",
			strings.Join(unaddable, "\n"))
	}
	t.Fatalf("the declared schema changed and %s is stale.\n"+
		"New/changed lines:\n  %s\n\n"+
		"If the change is intended, regenerate the golden in the same commit:\n"+
		"    go test ./internal/app/store -run TestDeclaredSchemaGoldenIsCurrent -update-schema-golden",
		goldenPath, strings.Join(other, "\n  "))
}

// renderSchemaGolden writes the declared schema as sorted, diffable lines: one
// per column carrying its verbatim declaration, one per table constraint.
func renderSchemaGolden(declared map[string]tableShape) string {
	var lines []string
	for table, shape := range declared {
		for _, c := range shape.Columns {
			lines = append(lines, fmt.Sprintf("%s\t%s\t%s", table, c.Name, shape.Decl[c.Name]))
		}
		for _, c := range shape.Constraints {
			lines = append(lines, fmt.Sprintf("%s\t(table constraint)\t%s", table, c))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// mustShape reads a table's current shape or fails the test.
func mustShape(t *testing.T, ctx context.Context, q queryer, table string) tableShape {
	t.Helper()
	shape, err := readShape(ctx, q, table)
	if err != nil {
		t.Fatalf("readShape(%s): %v", table, err)
	}
	if shape.Decl == nil {
		shape.Decl = map[string]string{}
	}
	return shape
}
