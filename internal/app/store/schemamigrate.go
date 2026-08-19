package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	stdlog "log"
	"os"
	"sort"
	"strings"
)

// This file converges a store file's COLUMNS on the shape this build declares.
//
// # The defect it closes
//
// Every table in this store is created with `CREATE TABLE IF NOT EXISTS`, which
// is a no-op against a table that already exists. So a column added to a table's
// DDL after a store file was created never appears on that file: a fresh install
// gets it, and every box that has been running since before the change never
// does — permanently, with no error at open and no way to grow out of it.
//
// The symptom is a runtime SQL failure on exactly the statements that name the
// new column, at a call site that has already decided (correctly, for a
// transient fault) to log and carry on. That is how `discovered_devices.
// open_ports` produced 1028 identical log lines over seven days on a live box
// while the durable device mirror neither loaded nor saved and nothing anywhere
// said so. The store had the column in code, in its writes and in its reads; the
// file simply did not.
//
// # Why this is generic rather than a fourth hand-written migration
//
// The store already carried three per-column migrations — pairing_grants.
// relay_id, packs.enabled, pack_installs.verifying_key — each a hand-written
// PRAGMA read plus one ALTER, each documenting the exact trap above. The
// mechanism was right and it still failed, because it is OPT-IN: adding a column
// obliges the author to remember to write a fourth one, and nothing — no test,
// no gate, no boot check — fails when they don't. Six columns have been added to
// an existing table in this repository's history; three got their migration and
// three did not. `open_ports` is simply the first of the three to reach a box
// that predated it.
//
// So the fix is a mechanism, not a migration: ONE pass that compares the file
// against what the DDL declares and adds whatever is missing. Adding a column
// then migrates itself, and the failure mode stops existing rather than being
// paid off once.
//
// # Where the expected column set comes from
//
// Not a hand-kept list beside the DDL — that would move this same bug up one
// level, to a list someone forgets to edit. The declared shape is read back out
// of a THROWAWAY `:memory:` database built by applySchemaDDL, the same function
// Open runs against the real file. SQLite is the authority, and the expected set
// is by construction "the columns a fresh store has" — the one definition that
// cannot be wrong.
//
// Two readings of that model are taken, because one of them is not enough:
//
//   - PRAGMA table_xinfo for the column's SHAPE (name, type, NOT NULL, DEFAULT,
//     primary-key position, and whether it is generated). table_xinfo rather
//     than table_info deliberately: table_info omits generated columns entirely,
//     so a generated column would be invisible on BOTH sides of the comparison
//     — absent from the model and absent from the file's reading — and the
//     migration would never plan it, never add it and never report it. That is
//     #194 verbatim, one level up, with the guard silent.
//   - the CREATE TABLE text (schemasql.go) for the column's DECLARATION, which
//     is what gets appended to the ALTER. Rendering the declaration from the
//     pragma instead drops UNIQUE, COLLATE, CHECK, REFERENCES and the generation
//     expression, and a column retrofitted without its constraints is a weaker
//     column than the same DDL gives a fresh install — permanently, and
//     invisibly, since the comparison would read through the same blind pragma
//     and call them identical forever.
//
// # What it deliberately does not do
//
// Only ADDITIONS. A column the file has and the build no longer declares, a
// column whose type or constraints changed, a table rebuild — none of that is
// touched. SQLite cannot do any of it with ALTER TABLE ADD COLUMN, guessing at
// it means writing over data on evidence this pass does not have, and the
// platform schema epoch (epoch.go, hook #50) is where a non-additive change
// belongs. Such drift is REPORTED, loudly, every boot, and left exactly as it
// is. Reporting rather than skipping is the point: the defect one level up from
// #194 is a difference nothing mentions.
//
// Never-wipe holds by construction: this pass issues ALTER TABLE ADD COLUMN and
// nothing else. It cannot drop a table, delete a row, or change a stored value.

// ColumnAddition is one column the migration added — or, from InspectSchema, one
// it WOULD add at the next open.
//
// Definition is the column's declaration as the DDL WROTE it, lifted verbatim
// out of the CREATE TABLE text, which is what gets appended to the ALTER. Taking
// it from the source text rather than re-rendering it from a pragma is what
// makes a migrated column identical to a fresh one rather than a stripped-down
// lookalike (see this file's header).
type ColumnAddition struct {
	Table      string
	Column     string
	Definition string
}

// SchemaDrift is one difference between the file and this build's DDL that
// adding a column cannot resolve. Reason is written for the operator reading a
// boot log, not for a caller switching on it.
type SchemaDrift struct {
	Table  string
	Column string
	Reason string
}

// SchemaMigration reports what a run did (or, from InspectSchema, would do).
//
// Added is empty on a conforming store, which is the overwhelmingly common case
// and the one the whole pass is tuned for: nothing is written, and no
// transaction is opened at all.
//
// Divergent is the DETECTOR half, and it is populated whether or not anything
// was added. It is every difference this pass will not repair — a column the
// file has that the build has stopped declaring, a changed type, a changed
// constraint, a table-level constraint the file's table does not carry. Nothing
// acts on one. It exists so that a store whose shape has diverged says so at
// every boot, in the boot log and in `-store-check`, instead of waiting to be
// discovered by whichever statement trips over it first.
//
// Created is a declared TABLE the file does not have. This pass does not create
// it — applySchemaDDL does, seconds later — so it is neither an Added nor a
// Divergent, and its own list is the SHAPE fix rather than a sentinel value
// squeezed into one of theirs. Both alternatives are wrong in ways that matter:
// Divergent is documented as "every difference this pass will not repair" and a
// missing table IS repaired, while Added would claim the columns arrive by ALTER,
// which for this table's PRIMARY KEY and NOT NULL columns is not merely
// inaccurate but impossible (whyNotAddable refuses both). Reporting a pending
// table THROUGH the column planner — by dropping the `continue` that skips it —
// would therefore classify every one of those columns as blocked and route every
// affected store, including every fresh install, permanently into maintenance
// mode. Hence a third list, populated at that same `continue` and gated so a
// fresh install stays quiet (planSchemaColumns).
//
// It is what box .12's `-store-check` could not say: it named the pending
// relay_last_seen COLUMN and never the pending device_first_seen TABLE, because
// nothing anywhere carried the fact.
type SchemaMigration struct {
	Created   []string
	Added     []ColumnAddition
	Divergent []SchemaDrift
}

// SchemaMigrationBlockedError is returned when this build CANNOT bring the
// stored schema up to its own shape. Either a declared column is missing from
// the file and SQLite cannot add it (a PRIMARY KEY column, a UNIQUE column, a
// NOT NULL column with no DEFAULT, a non-constant DEFAULT, a STORED generated
// column) — Blocked names each one and why — or the migration itself failed
// while writing, which Cause carries.
//
// The migration is all-or-nothing: on this error nothing was committed and the
// file is exactly as it was.
//
// It is a hard refusal rather than a warning, and that is deliberate. A declared
// column the file lacks is precisely the #194 condition: every statement naming
// it fails, permanently, at call sites built to tolerate a transient fault. A
// store in that state has to be dealt with by a human — the column needs a
// DEFAULT, or a table rebuild the schema epoch owns — and carrying on would
// reproduce the exact silence this file exists to end.
//
// The boot does NOT treat it as a crash. It is the same "cannot open the
// workspace, and a restart will not fix it" class as EpochTooNewError, so the
// feeder enters maintenance mode on it (cmd/waiveo-feeder/maintenance.go): a
// log.Fatal under a supervisor is a crash loop, and the one sentence naming the
// column would scroll past on a box that is now down and taking the relay with
// it.
//
// Reaching it by adding an ordinary column is entirely possible and is NOT
// guarded against here: 108 of this store's declared columns are bare NOT NULL
// with no DEFAULT, and a new one in that style is unaddable on every box that
// has the table. That is what the declared-schema golden
// (testdata/declared-schema.golden) exists to catch — in the pull request that
// writes the column, rather than at the next fleet boot.
type SchemaMigrationBlockedError struct {
	Blocked []SchemaDrift
	Cause   error
}

func (e *SchemaMigrationBlockedError) Error() string {
	if len(e.Blocked) == 0 && e.Cause != nil {
		return "store: cannot bring the stored schema up to this build's shape: " + e.Cause.Error()
	}
	parts := make([]string, 0, len(e.Blocked))
	for _, b := range e.Blocked {
		parts = append(parts, fmt.Sprintf("%s.%s: %s", b.Table, b.Column, b.Reason))
	}
	return "store: cannot bring the stored schema up to this build's shape: " + strings.Join(parts, "; ")
}

func (e *SchemaMigrationBlockedError) Unwrap() error { return e.Cause }

// columnShape is one column as PRAGMA table_xinfo reports it, from either side
// of the comparison: the throwaway model built from the DDL, or the file on
// disk.
//
// Hidden is table_xinfo's own column: 0 for an ordinary column, 2 for a VIRTUAL
// generated column and 3 for a STORED one (1 is a virtual-table hidden column,
// which this store has none of). It is carried because it is the ONLY reading
// that distinguishes a generated column from an ordinary one — and because
// table_info, which has no such column, cannot see generated columns at all.
type columnShape struct {
	Name    string
	Type    string
	NotNull bool
	Dflt    sql.NullString
	PK      int
	Hidden  int
}

// generated reports whether the column is computed rather than stored per row.
func (c columnShape) generated() bool { return c.Hidden == 2 || c.Hidden == 3 }

// tableShape is one table as it stands, on either side of the comparison: its
// columns' shapes, each column's verbatim declaration text, and its table-level
// constraints.
//
// Decl is keyed by column name and holds the source text (schemasql.go). It is
// empty for a table that does not exist, and — for a table on DISK — it is
// whatever CREATE TABLE actually created it, which is how a column that an
// earlier, blinder version of this migration added without its constraints can
// still be told apart from one the DDL declared.
type tableShape struct {
	Columns     []columnShape
	Decl        map[string]string
	Constraints []string
}

// quoteIdent wraps a SQLite identifier in double quotes. Every name that reaches
// it is one of this package's own table or column names — read out of the model
// database this package built from its own DDL constants, never caller input —
// so this is hygiene around string-built SQL that PRAGMA and ALTER cannot
// parameterize, not a defence against injection.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// declaredSchema returns the shape of a FRESH store: every table applySchemaDDL
// creates, keyed by table name.
//
// It builds one throwaway in-memory database, runs the real DDL over it, and
// reads the result back out. The indirection is the whole point — see this
// file's header. The cost is one in-memory database per Open, which is a few
// milliseconds once per process.
func declaredSchema(ctx context.Context) (map[string]tableShape, error) {
	// The same single-connection discipline Open uses, and for a sharper reason
	// here: a ":memory:" database belongs to its CONNECTION, so a second
	// connection would see an empty database and the model would come back bare.
	model, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return nil, fmt.Errorf("store: build the declared-schema model: %w", err)
	}
	model.SetMaxOpenConns(1)
	defer func() { _ = model.Close() }()

	if err := applySchemaDDL(model); err != nil {
		return nil, fmt.Errorf("store: build the declared-schema model: %w", err)
	}

	tables, err := tableNames(ctx, model)
	if err != nil {
		return nil, err
	}
	declared := make(map[string]tableShape, len(tables))
	for _, t := range tables {
		shape, err := readShape(ctx, model, t)
		if err != nil {
			return nil, err
		}
		declared[t] = shape
	}
	return declared, nil
}

// tableNames lists the ordinary tables in a database, sorted, with SQLite's own
// internal ones excluded.
func tableNames(ctx context.Context, q queryer) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list tables: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: list tables: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// tableExists reports whether the database holds an ordinary table by this name.
//
// It is how a reader that must survive a store the DDL has not run over yet asks
// its question — `-store-check` opens read-only and never creates a table, so a
// query naming one this build declares and the file has not got would fail with
// "no such table" where the honest answer is "not yet, and the next boot creates
// it". The boot itself never needs this: applySchemaDDL has run by then.
func tableExists(ctx context.Context, q queryer, table string) (bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = ?`, table)
	if err != nil {
		return false, fmt.Errorf("store: look for table %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	found := rows.Next()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: look for table %s: %w", table, err)
	}
	return found, nil
}

// readShape reads one table's columns in declaration order together with the
// declaration text each came from. A table that does not exist yields an empty
// shape and no error, which is how "the file does not have this table yet" is
// decided — a fresh store is every table absent, and applySchemaDDL creates them
// all a moment later.
func readShape(ctx context.Context, q queryer, table string) (tableShape, error) {
	cols, err := readTableColumns(ctx, q, table)
	if err != nil {
		return tableShape{}, err
	}
	if len(cols) == 0 {
		return tableShape{}, nil
	}

	createSQL, err := readCreateSQL(ctx, q, table)
	if err != nil {
		return tableShape{}, err
	}
	if createSQL == "" {
		return tableShape{Columns: cols}, nil
	}
	decl, _, constraints, err := tableDecls(createSQL)
	if err != nil {
		return tableShape{}, err
	}
	return tableShape{Columns: cols, Decl: decl, Constraints: constraints}, nil
}

// readCreateSQL returns the verbatim CREATE TABLE text SQLite stored for a
// table, or "" when the table is not there. It goes through QueryContext rather
// than QueryRow because the queryer seam this package shares between *sql.DB and
// *sql.Tx carries only the one method.
func readCreateSQL(ctx context.Context, q queryer, table string) (string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = ?`, table)
	if err != nil {
		return "", fmt.Errorf("store: read the declaration of %s: %w", table, err)
	}
	defer rows.Close()

	var createSQL sql.NullString
	if rows.Next() {
		if err := rows.Scan(&createSQL); err != nil {
			return "", fmt.Errorf("store: read the declaration of %s: %w", table, err)
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("store: read the declaration of %s: %w", table, err)
	}
	return createSQL.String, nil
}

// readTableColumns reads one table's columns through PRAGMA table_xinfo — which,
// unlike table_info, also reports GENERATED columns. See this file's header for
// why that distinction is load-bearing rather than a detail.
func readTableColumns(ctx context.Context, q queryer, table string) ([]columnShape, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA table_xinfo(`+quoteIdent(table)+`)`)
	if err != nil {
		return nil, fmt.Errorf("store: inspect %s: %w", table, err)
	}
	defer rows.Close()

	var out []columnShape
	for rows.Next() {
		var (
			cid     int
			c       columnShape
			notNull int
		)
		if err := rows.Scan(&cid, &c.Name, &c.Type, &notNull, &c.Dflt, &c.PK, &c.Hidden); err != nil {
			return nil, fmt.Errorf("store: inspect %s: %w", table, err)
		}
		c.NotNull = notNull != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// planSchemaColumns compares the file against the declared shape and works out
// what to do about every difference. It writes nothing, and is the single source
// of the plan for the read-only report (InspectSchema), for the migrating write,
// and for that write's own verification — so what an operator is shown before a
// restart is computed by the code that later acts, and the code that later acts
// is judged by the same reading.
//
// The three outcomes are distinct on purpose:
//
//   - adds — a declared column the file lacks, which ALTER TABLE ADD COLUMN can
//     retrofit. This is the repair.
//   - blocked — a declared column the file lacks that SQLite CANNOT add. Refused
//     outright rather than half-applied (SchemaMigrationBlockedError).
//   - divergent — everything else that does not match: a column the file has and
//     this build no longer declares, one whose type or constraints differ, or a
//     table-level constraint the two sides disagree on. Reported and left
//     untouched, never guessed at.
//
// Tables are walked in sorted order so two readings of an unchanged store are
// byte-identical, which is what makes the boot log and `-store-check` comparable
// between runs.
//
// A table the FILE holds that this build does not declare is passed over in
// silence: nothing here created it, nothing here reads it, and every statement
// this package issues names its columns explicitly (there is no `SELECT *` in
// the package), so it cannot affect anything. Reporting it would put a line in
// every boot log of any deployment that ever kept a table beside this store.
//
// A table the BUILD declares and the file does not have is the fourth outcome,
// and it is reported without being acted on here: `created` names it, and
// applySchemaDDL is what makes it exist a moment later. That list is empty on a
// fresh store by construction — when the file holds NONE of the declared tables
// it is an install rather than an upgrade, and a report shouting about thirty
// tables it is about to create is the noise the skip below was written to
// prevent. A file holding thirty of thirty-one is unambiguously an upgrade, and
// the one it is missing is exactly the fact box .12's check could not state.
func planSchemaColumns(ctx context.Context, q queryer, declared map[string]tableShape) (created []string, adds []ColumnAddition, blocked, divergent []SchemaDrift, err error) {
	tables := make([]string, 0, len(declared))
	for t := range declared {
		tables = append(tables, t)
	}
	sort.Strings(tables)

	var (
		absent  []string
		present int
	)
	for _, table := range tables {
		want := declared[table]
		onDisk, err := readShape(ctx, q, table)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if len(onDisk.Columns) == 0 {
			// The table is not on the file at all. applySchemaDDL creates it
			// whole, with every column, immediately after this pass — so there is
			// nothing to converge, and reporting it as DRIFT would both mis-state
			// the mechanism and make every fresh install shout about thirty missing
			// tables it is about to create. The `continue` stays exactly as it was;
			// what changes is that the fact is now carried out instead of dropped.
			absent = append(absent, table)
			continue
		}
		present++

		found := make(map[string]columnShape, len(onDisk.Columns))
		for _, c := range onDisk.Columns {
			found[c.Name] = c
		}

		for _, wantCol := range want.Columns {
			decl := want.Decl[wantCol.Name]
			got, present := found[wantCol.Name]
			if !present {
				if reason := whyNotAddable(wantCol, decl); reason != "" {
					blocked = append(blocked, SchemaDrift{Table: table, Column: wantCol.Name, Reason: reason})
					continue
				}
				adds = append(adds, ColumnAddition{
					Table:      table,
					Column:     wantCol.Name,
					Definition: decl,
				})
				continue
			}
			if reason := incompatible(wantCol, got); reason != "" {
				divergent = append(divergent, SchemaDrift{Table: table, Column: wantCol.Name, Reason: reason})
			}
		}

		declaredNames := make(map[string]struct{}, len(want.Columns))
		for _, c := range want.Columns {
			declaredNames[c.Name] = struct{}{}
		}
		for _, c := range onDisk.Columns {
			if _, ok := declaredNames[c.Name]; ok {
				continue
			}
			divergent = append(divergent, SchemaDrift{
				Table:  table,
				Column: c.Name,
				Reason: "the file has this column and this build no longer declares it; dropping a column is not an " +
					"additive change and destroys whatever it holds, so it is reported and left alone",
			})
		}
		divergent = append(divergent, constraintDrift(table, want, onDisk)...)
	}
	if present > 0 {
		// Sorted already: `tables` was walked in order. An upgrade, so name what is
		// missing; on a fresh install (present == 0) `absent` is every declared
		// table and is deliberately dropped.
		created = absent
	}
	return created, adds, blocked, divergent, nil
}

// constraintDrift reports TABLE-level constraints the two sides disagree on — a
// `PRIMARY KEY (a, b)`, a `UNIQUE (a, b)`, a table CHECK.
//
// It is here because ALTER TABLE ADD COLUMN cannot add one, so a table-level
// constraint introduced after a store was created is exactly the #194 shape one
// level up: enforced on every fresh install, absent on every existing box, and
// invisible to a column-by-column comparison. Nothing acts on it; it is said out
// loud at every boot until a schema-epoch migration rebuilds the table.
//
// The comparison is on the normalized declaration text as a SET, not in order,
// because ALTER TABLE ADD COLUMN appends the new column AFTER any trailing table
// constraint — so a migrated table's constraint sits in a different position
// than a fresh one's while being the same constraint.
func constraintDrift(table string, want, onDisk tableShape) []SchemaDrift {
	if onDisk.Decl == nil {
		// The file's CREATE TABLE text could not be read (an in-memory model
		// built without one, say). Nothing to compare, and inventing drift from
		// an absent reading would be worse than saying nothing.
		return nil
	}
	have := make(map[string]struct{}, len(onDisk.Constraints))
	for _, c := range onDisk.Constraints {
		have[canonicalConstraint(c)] = struct{}{}
	}
	declared := make(map[string]struct{}, len(want.Constraints))
	for _, c := range want.Constraints {
		declared[canonicalConstraint(c)] = struct{}{}
	}

	var out []SchemaDrift
	for _, c := range want.Constraints {
		if _, ok := have[canonicalConstraint(c)]; ok {
			continue
		}
		out = append(out, SchemaDrift{Table: table, Column: "(table constraint)", Reason: fmt.Sprintf(
			"this build declares %q and the file's table does not carry it; ALTER TABLE cannot add a table "+
				"constraint, so it is reported and left alone until a schema-epoch migration rebuilds the table", c)})
	}
	for _, c := range onDisk.Constraints {
		if _, ok := declared[canonicalConstraint(c)]; ok {
			continue
		}
		out = append(out, SchemaDrift{Table: table, Column: "(table constraint)", Reason: fmt.Sprintf(
			"the file's table carries %q and this build no longer declares it; dropping a table constraint is "+
				"not an additive change, so it is reported and left alone", c)})
	}
	return out
}

// whyNotAddable returns the reason SQLite would refuse to ADD this column, or ""
// when it can be added. Every case below was probed against this build's driver
// (modernc.org/sqlite, SQLite 3.53) rather than recited from the documentation,
// and each one is checked HERE rather than left to SQLite because two of them
// fail only on a table that has rows — an ALTER that succeeds on an empty table
// and refuses on a populated one splits the fleet on whether a box happened to
// have data, which is worse than refusing everywhere.
//
//	PRIMARY KEY            "Cannot add a PRIMARY KEY column"           (always)
//	UNIQUE                 "Cannot add a UNIQUE column"                (always)
//	NOT NULL, no DEFAULT   "Cannot add a NOT NULL column with          (always)
//	                        default value NULL"
//	DEFAULT (expr)         "Cannot add a column with non-constant      (rows only)
//	                        default"
//	DEFAULT CURRENT_*      "Cannot add a column with non-constant      (rows only)
//	                        default"
//	GENERATED … STORED     "cannot add a STORED column"                (rows only)
//
// UNIQUE and the generation kind are read from the column's DECLARATION TEXT,
// because neither is visible through a pragma on the column itself — which is
// also why the ALTER carries the declaration verbatim rather than a rendering of
// the pragma (see this file's header).
func whyNotAddable(c columnShape, decl string) string {
	if decl == "" {
		// No declaration text means the CREATE TABLE reader could not find this
		// column, and the ALTER would have nothing faithful to append. Refusing is
		// the only safe answer: rendering the column from its pragma row instead
		// is exactly the silent-weakening this reading exists to prevent.
		return "this build declares the column but its declaration could not be read from the DDL, so it cannot be added faithfully"
	}
	if c.PK != 0 {
		return "SQLite cannot ADD a PRIMARY KEY column to an existing table; this needs a table rebuild, which belongs to the schema epoch"
	}
	if declHasKeyword(decl, "UNIQUE") {
		return "SQLite cannot ADD a UNIQUE column to an existing table; this needs a table rebuild, which belongs to the schema epoch"
	}
	if c.Hidden == 3 {
		return "SQLite cannot ADD a STORED generated column to a table that has rows; declare it VIRTUAL, or rebuild the table under a schema epoch"
	}
	if c.NotNull && !c.Dflt.Valid && !c.generated() {
		return "SQLite cannot ADD a NOT NULL column with no DEFAULT; give the column a DEFAULT and every existing row gets it"
	}
	if c.Dflt.Valid {
		dflt := strings.TrimSpace(c.Dflt.String)
		if strings.HasPrefix(dflt, "(") {
			return "SQLite cannot ADD a column whose DEFAULT is not a constant; a computed default has no value to back-fill existing rows with"
		}
		switch strings.ToUpper(dflt) {
		case "CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP":
			// SQLite lists these separately from "an expression in parentheses",
			// and they are reported by the pragma WITHOUT parentheses — so the
			// paren test above does not see them. The ALTER succeeds on an empty
			// table and fails on one with rows, which is the fleet-splitting shape
			// this function exists to refuse uniformly.
			return "SQLite cannot ADD a column whose DEFAULT is " + dflt +
				"; it is not a constant, so there is no value to back-fill existing rows with"
		}
	}
	return ""
}

// incompatible describes how a column present on BOTH sides differs, or ""
// when they agree. Everything it can report is non-additive by definition — the
// column is already there, so ALTER TABLE ADD COLUMN has nothing to offer — and
// none of it is acted on.
//
// The comparison is on the declared spelling as SQLite reports it, upper-cased
// for the type (SQLite preserves the DDL's casing verbatim, and `TEXT` versus
// `text` is not a difference anyone means) and trimmed for the default.
func incompatible(want, got columnShape) string {
	var reasons []string
	if !strings.EqualFold(strings.TrimSpace(want.Type), strings.TrimSpace(got.Type)) {
		reasons = append(reasons, fmt.Sprintf("this build declares type %q and the file has %q", want.Type, got.Type))
	}
	if want.NotNull != got.NotNull {
		reasons = append(reasons, fmt.Sprintf("this build declares NOT NULL=%t and the file has %t", want.NotNull, got.NotNull))
	}
	if defaultText(want) != defaultText(got) {
		reasons = append(reasons, fmt.Sprintf("this build declares DEFAULT %s and the file has %s", defaultText(want), defaultText(got)))
	}
	if want.PK != got.PK {
		reasons = append(reasons, fmt.Sprintf("this build declares primary-key position %d and the file has %d", want.PK, got.PK))
	}
	if want.Hidden != got.Hidden {
		reasons = append(reasons, fmt.Sprintf("this build declares %s and the file has %s",
			generationOf(want), generationOf(got)))
	}
	if len(reasons) == 0 {
		return ""
	}
	return strings.Join(reasons, "; ") +
		" — changing a column in place is not an additive change, so it is reported and left alone"
}

// generationOf names a column's generation kind for the drift report.
func generationOf(c columnShape) string {
	switch c.Hidden {
	case 2:
		return "a VIRTUAL generated column"
	case 3:
		return "a STORED generated column"
	default:
		return "an ordinary stored column"
	}
}

// defaultText renders a column default for comparison and for the operator,
// distinguishing "no default" from a default that happens to be empty text.
func defaultText(c columnShape) string {
	if !c.Dflt.Valid {
		return "(none)"
	}
	return strings.TrimSpace(c.Dflt.String)
}

// migrateSchemaColumns brings the file's columns up to this build's declared
// shape and returns what it changed. Open calls it on every open, before any
// other statement touches a table.
//
// It is safe to run unattended at every boot because it is strictly additive: it
// issues ALTER TABLE ADD COLUMN and nothing else, so it cannot lose a row or
// alter a stored value, and a store that already conforms is not written to at
// all — no transaction is opened, which is what TestSchemaMigrationOpensNoWrite
// TransactionOnAConformingStore proves by running it over a read-only handle.
//
// When there IS work, all of it happens in one transaction: the plan is re-taken
// INSIDE that transaction rather than trusted from the unlocked reading above
// (the reading that decided to open it is an optimization, not the decision),
// the ALTERs are applied, and the plan is then re-taken a third time as the
// proof of idempotence — the migration's own output must be a store this same
// planner finds nothing left to do on. Anything short of that commits nothing.
//
// Every way this can fail returns *SchemaMigrationBlockedError, including a
// failure part-way through the ALTERs: the boot maps that type to maintenance
// mode, and an untyped error there would fall through to log.Fatal and
// crash-loop the box instead of leaving a diagnosable surface up.
func migrateSchemaColumns(ctx context.Context, db *sql.DB) (SchemaMigration, error) {
	declared, err := declaredSchema(ctx)
	if err != nil {
		return SchemaMigration{}, err
	}

	created, adds, blocked, divergent, err := planSchemaColumns(ctx, db, declared)
	if err != nil {
		return SchemaMigration{}, err
	}
	if len(blocked) > 0 {
		return SchemaMigration{}, &SchemaMigrationBlockedError{Blocked: blocked}
	}
	if len(adds) == 0 {
		// The conforming case, and the one that runs on every box on every boot
		// after the first. No transaction, no write, nothing but the reads that
		// produced the report.
		//
		// `created` still rides out: a store missing a whole TABLE and no columns
		// is precisely box .12's shape, and it is a conforming store by this pass's
		// reckoning.
		return SchemaMigration{Created: created, Divergent: divergent}, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return SchemaMigration{}, &SchemaMigrationBlockedError{Cause: fmt.Errorf("begin schema migration: %w", err)}
	}
	defer func() { _ = tx.Rollback() }()

	_, adds, blocked, _, err = planSchemaColumns(ctx, tx, declared)
	if err != nil {
		return SchemaMigration{}, err
	}
	if len(blocked) > 0 {
		return SchemaMigration{}, &SchemaMigrationBlockedError{Blocked: blocked}
	}
	for _, a := range adds {
		if _, err := tx.ExecContext(ctx,
			`ALTER TABLE `+quoteIdent(a.Table)+` ADD COLUMN `+a.Definition); err != nil {
			return SchemaMigration{}, &SchemaMigrationBlockedError{
				Cause: fmt.Errorf("add %s.%s (%s): %w", a.Table, a.Column, a.Definition, err)}
		}
	}

	// The third reading is the idempotence proof, asserted rather than assumed:
	// what this transaction is about to commit must be a store this same planner
	// finds nothing left to add on. It also supplies the divergence report, taken
	// AFTER the ALTERs, so the caller logs the store it is about to serve rather
	// than the one the transaction opened on.
	created, residualAdds, residualBlocked, divergent, err := planSchemaColumns(ctx, tx, declared)
	if err != nil {
		return SchemaMigration{}, err
	}
	if len(residualAdds) > 0 || len(residualBlocked) > 0 {
		return SchemaMigration{}, &SchemaMigrationBlockedError{Cause: fmt.Errorf(
			"adding %d column(s) left %d still missing and %d blocked; nothing was committed",
			len(adds), len(residualAdds), len(residualBlocked))}
	}
	if err := tx.Commit(); err != nil {
		return SchemaMigration{}, &SchemaMigrationBlockedError{Cause: fmt.Errorf("commit schema migration: %w", err)}
	}
	return SchemaMigration{Created: created, Added: adds, Divergent: divergent}, nil
}

// reportSchemaMigration is the boot log's account of what the pass did, on the
// same terms as the store's stored-row fault report (priorfaults.go): a
// conforming store says nothing at all, and anything else says it once, at open,
// in full.
//
// Each added column gets its OWN line naming the table and the column. This is
// the line whose absence cost seven days on box .12: the column was missing, the
// mirror could neither load nor save, and the only evidence anywhere was a
// per-write error at a call site that had already decided such errors were
// transient. A boot log that names the repair is what makes the next one of
// these a one-line answer.
func reportSchemaMigration(dsn string, m SchemaMigration) {
	for _, a := range m.Added {
		stdlog.Printf("store: added missing column %s.%s (%s) to %s; existing rows take its declared default",
			a.Table, a.Column, a.Definition, dsn)
	}
	if len(m.Added) > 0 {
		stdlog.Printf("store: brought %d column(s) in %s up to this build's schema", len(m.Added), dsn)
	}
	if len(m.Divergent) == 0 {
		return
	}
	stdlog.Printf("store: %s differs from this build's schema in %d way(s) that adding a column cannot fix; "+
		"nothing has been changed, and each stands until a schema-epoch migration addresses it", dsn, len(m.Divergent))
	// Bounded for the reason the stored-row fault report is bounded: a file
	// restored from something quite different can differ in every column, and a
	// boot log that scrolls the rest of startup away to say so is a report nobody
	// can use. InspectSchema returns the complete list.
	const shownDrift = 20
	for i, d := range m.Divergent {
		if i == shownDrift {
			stdlog.Printf("store: … and %d more; `waiveo-feeder -store-check` lists them all", len(m.Divergent)-shownDrift)
			break
		}
		stdlog.Printf("store: schema drift: %s.%s: %s", d.Table, d.Column, d.Reason)
	}
}

// reportCreatedTables is the boot log's account of the tables applySchemaDDL just
// brought into existence on an EXISTING store.
//
// It is called AFTER that DDL has run rather than beside reportSchemaMigration,
// and the ordering is the point: this store's reports state facts, not intentions
// (`added missing column`, `created missing table`), and at the moment the column
// pass reports, the table does not exist yet. A line claiming otherwise would be
// the only forward-looking sentence in the boot log, and it would be wrong on
// exactly the boot where the DDL failed.
//
// A fresh install says nothing — see planSchemaColumns for why `created` is empty
// there — and neither does a store that already had every table, which is every
// boot after the one that introduced one. `IF NOT EXISTS` is what erased this
// distinction from the DDL itself, and it is why the fact has to be carried out
// of the planner rather than observed at the CREATE.
func reportCreatedTables(dsn string, created []string) {
	for _, t := range created {
		stdlog.Printf("store: created missing table %s in %s; this build declares it and the file did not have it",
			t, dsn)
	}
	if len(created) > 0 {
		stdlog.Printf("store: created %d missing table(s) in %s; creating a table cannot lose a row, since it did "+
			"not exist to hold one", len(created), dsn)
	}
}

// InspectSchema reports what opening the store at dsn would do to its columns,
// writing nothing and opening it READ-ONLY. It is what `-store-check` runs
// before the store is opened for real, so an operator can see the next boot's
// schema changes before taking one.
//
// It has to open its own handle rather than take a *Store, and that is the whole
// reason it exists as a package function: by the time store.Open has returned,
// the migration has already run, and a report taken from that Store would
// forever say "nothing to do". Asked before the open, the same planner answers
// what the open is about to do.
//
// A path with no store yet reports nothing rather than an error: there is no
// file to converge, and Open will create one already carrying every column.
//
// It applies the SAME epoch gate Open does, and first. A file written at a newer
// platform schema epoch is one this build must not open (ARC-041/104) and — just
// as importantly — one it cannot describe: the columns a newer build added read
// here as columns "this build no longer declares", which is a written
// justification for hand-dropping a column that build's rows depend on. Refusing
// with EpochTooNewError is the only honest answer, and it is the same answer the
// next open will give.
//
// An *SchemaMigrationBlockedError here means the next open will REFUSE, and the
// store needs a human before the feeder is restarted onto this build.
func InspectSchema(dsn string) (SchemaMigration, error) {
	if dsn != ":memory:" {
		if _, err := os.Stat(dsn); errors.Is(err, os.ErrNotExist) {
			return SchemaMigration{}, nil
		}
	}
	// mode=ro, so this cannot write even by accident — including the schema
	// migration's own ALTERs, which is the property that makes running it against
	// a live box's store before a restart an honest check rather than a quiet
	// change.
	db, err := sql.Open("sqlite", "file:"+dsn+"?mode=ro")
	if err != nil {
		return SchemaMigration{}, fmt.Errorf("store: inspect schema of %s: %w", dsn, err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	onDiskEpoch, err := readSchemaEpoch(db)
	if err != nil {
		return SchemaMigration{}, fmt.Errorf("store: inspect schema of %s: %w", dsn, err)
	}
	if onDiskEpoch > PlatformSchemaEpoch {
		return SchemaMigration{}, &EpochTooNewError{OnDisk: onDiskEpoch, Understood: PlatformSchemaEpoch}
	}

	ctx := context.Background()
	declared, err := declaredSchema(ctx)
	if err != nil {
		return SchemaMigration{}, err
	}
	created, adds, blocked, divergent, err := planSchemaColumns(ctx, db, declared)
	if err != nil {
		return SchemaMigration{}, fmt.Errorf("store: inspect schema of %s: %w", dsn, err)
	}
	if len(blocked) > 0 {
		return SchemaMigration{}, &SchemaMigrationBlockedError{Blocked: blocked}
	}
	return SchemaMigration{Created: created, Added: adds, Divergent: divergent}, nil
}
