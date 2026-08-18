package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// schemasql_internal_test.go pins the CREATE-TABLE reader against the spellings
// that would make a naive split wrong. Every one of these is a comma, a paren or
// a keyword that must NOT be read as a separator or a constraint: the reader's
// output becomes the text of an ALTER, so a wrong reading is a wrong statement
// issued against a live store.
//
// TestEveryDeclaredTableIsReadableAsColumns covers the DDL this build actually
// has; these cover the ones it could grow.

func TestTableDeclsReadsTheSpellingsThatBreakANaiveSplit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		createSQL   string
		wantCols    []string
		wantDecl    map[string]string
		wantConstrs []string
	}{{
		name:      "a comma inside a string default",
		createSQL: `CREATE TABLE t (a TEXT NOT NULL DEFAULT 'x,y', b INTEGER)`,
		wantCols:  []string{"a", "b"},
		wantDecl:  map[string]string{"a": `a TEXT NOT NULL DEFAULT 'x,y'`, "b": "b INTEGER"},
	}, {
		name:        "a comma inside a CHECK, and a table constraint that starts with one",
		createSQL:   `CREATE TABLE t (a TEXT CHECK (a IN ('p','q')), b TEXT, PRIMARY KEY (a, b))`,
		wantCols:    []string{"a", "b"},
		wantDecl:    map[string]string{"a": `a TEXT CHECK (a IN ('p','q'))`, "b": "b TEXT"},
		wantConstrs: []string{"PRIMARY KEY (a, b)"},
	}, {
		name:      "a line comment between columns, as packs.enabled has",
		createSQL: "CREATE TABLE t (\n\ta TEXT,\n\t-- why this column exists, at length,\n\t-- over two lines\n\tb INTEGER NOT NULL DEFAULT 1\n)",
		wantCols:  []string{"a", "b"},
		wantDecl:  map[string]string{"a": "a TEXT", "b": "b INTEGER NOT NULL DEFAULT 1"},
	}, {
		name:      "a quoted identifier that is also a constraint keyword",
		createSQL: `CREATE TABLE t ("check" TEXT NOT NULL DEFAULT '', "unique" INTEGER)`,
		wantCols:  []string{"check", "unique"},
		wantDecl:  map[string]string{"check": `"check" TEXT NOT NULL DEFAULT ''`, "unique": `"unique" INTEGER`},
	}, {
		name:      "a generated column carrying its expression",
		createSQL: `CREATE TABLE t (ports TEXT NOT NULL DEFAULT '[]', n INTEGER GENERATED ALWAYS AS (json_array_length(ports)) VIRTUAL)`,
		wantCols:  []string{"ports", "n"},
		wantDecl: map[string]string{
			"ports": `ports TEXT NOT NULL DEFAULT '[]'`,
			"n":     `n INTEGER GENERATED ALWAYS AS (json_array_length(ports)) VIRTUAL`,
		},
	}, {
		name:        "a table-level UNIQUE and a FOREIGN KEY",
		createSQL:   `CREATE TABLE t (a TEXT, b TEXT, UNIQUE (a, b), FOREIGN KEY (a) REFERENCES u(id))`,
		wantCols:    []string{"a", "b"},
		wantDecl:    map[string]string{"a": "a TEXT", "b": "b TEXT"},
		wantConstrs: []string{"UNIQUE (a, b)", "FOREIGN KEY (a) REFERENCES u(id)"},
	}, {
		name:      "a paren inside a string default",
		createSQL: `CREATE TABLE t (a TEXT NOT NULL DEFAULT '(not an expression)', b TEXT)`,
		wantCols:  []string{"a", "b"},
		wantDecl:  map[string]string{"a": `a TEXT NOT NULL DEFAULT '(not an expression)'`, "b": "b TEXT"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			decls, order, constraints, err := tableDecls(tc.createSQL)
			if err != nil {
				t.Fatalf("tableDecls: %v", err)
			}
			if strings.Join(order, ",") != strings.Join(tc.wantCols, ",") {
				t.Fatalf("columns = %v, want %v", order, tc.wantCols)
			}
			for name, want := range tc.wantDecl {
				if decls[name] != want {
					t.Errorf("%s declaration = %q, want %q", name, decls[name], want)
				}
			}
			if strings.Join(constraints, " | ") != strings.Join(tc.wantConstrs, " | ") {
				t.Fatalf("table constraints = %v, want %v", constraints, tc.wantConstrs)
			}

			// The reading has to agree with SQLite's, which is the only authority
			// that matters: create the table and compare against table_xinfo.
			db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "probe.db"))
			if err != nil {
				t.Fatalf("open probe: %v", err)
			}
			defer func() { _ = db.Close() }()
			db.SetMaxOpenConns(1)
			if _, err := db.Exec(tc.createSQL); err != nil {
				t.Fatalf("create the probe table: %v", err)
			}
			got := xinfoColumns(t, db, "t")
			if len(got) != len(order) {
				t.Fatalf("the reader found %d columns and SQLite reports %d", len(order), len(got))
			}
			for _, name := range order {
				if _, ok := got[name]; !ok {
					t.Errorf("the reader found a column %q SQLite does not report", name)
				}
			}
		})
	}
}

// TestDeclHasKeywordIgnoresQuotedText: UNIQUE decides whether a column is
// planned as an addition or refused, and it is found by looking at the
// declaration's words. A column whose DEFAULT or name merely CONTAINS the word
// must not be refused, and one that really carries the constraint must not slip
// through.
func TestDeclHasKeywordIgnoresQuotedText(t *testing.T) {
	for _, tc := range []struct {
		decl string
		want bool
	}{
		{`native_key TEXT NOT NULL DEFAULT '' UNIQUE`, true},
		{`native_key TEXT NOT NULL DEFAULT '' unique collate nocase`, true},
		{`note TEXT NOT NULL DEFAULT 'this row is UNIQUE'`, false},
		{`"unique_hint" TEXT NOT NULL DEFAULT ''`, false},
		{`uniqueness INTEGER NOT NULL DEFAULT 0`, false},
		{`kind TEXT NOT NULL DEFAULT '' CHECK (kind <> 'UNIQUE')`, false},
	} {
		if got := declHasKeyword(tc.decl, "UNIQUE"); got != tc.want {
			t.Errorf("declHasKeyword(%q) = %t, want %t", tc.decl, got, tc.want)
		}
	}
}

// TestTableDeclsRefusesWhatItCannotRead: a statement with no column list is not
// something to guess at. The reader says so rather than returning an empty set
// that would read as "this table declares nothing".
func TestTableDeclsRefusesWhatItCannotRead(t *testing.T) {
	if _, _, _, err := tableDecls(`CREATE TABLE t AS SELECT 1`); err == nil {
		t.Fatalf("a CREATE ... AS SELECT has no column list and must not be read as one")
	}
	if _, _, _, err := tableDecls(`CREATE TABLE t (a TEXT`); err == nil {
		t.Fatalf("an unterminated column list must be refused")
	}
}

// TestCanonicalConstraintIgnoresFormattingButNotContent: a table constraint is
// compared between what this build declares and what the file's table carries,
// and a difference is reported at EVERY boot. Two builds spelling the same
// constraint with different spacing or casing must not produce that line
// forever; two builds declaring different constraints must.
func TestCanonicalConstraintIgnoresFormattingButNotContent(t *testing.T) {
	same := [][2]string{
		{"PRIMARY KEY (pack_id, file_kind, name)", "PRIMARY KEY(pack_id,file_kind,name)"},
		{"primary key (a, b)", "PRIMARY KEY (a,b)"},
		{"UNIQUE (driver, native_id)", "unique(driver , native_id)"},
	}
	for _, pair := range same {
		if canonicalConstraint(pair[0]) != canonicalConstraint(pair[1]) {
			t.Errorf("%q and %q are the same constraint; got %q and %q",
				pair[0], pair[1], canonicalConstraint(pair[0]), canonicalConstraint(pair[1]))
		}
	}
	differ := [][2]string{
		{"PRIMARY KEY (a, b)", "PRIMARY KEY (a, c)"},
		{"UNIQUE (a)", "UNIQUE (a, b)"},
		{"CHECK (kind <> 'x')", "CHECK (kind <> 'X')"},
	}
	for _, pair := range differ {
		if canonicalConstraint(pair[0]) == canonicalConstraint(pair[1]) {
			t.Errorf("%q and %q are different constraints and must not compare equal", pair[0], pair[1])
		}
	}
}
