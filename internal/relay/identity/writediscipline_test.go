package identity

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// writediscipline_test.go pins verifyWriteDiscipline, the check that confirms
// the crash-safety PRAGMAs actually took on the connection.
//
// It is not bookkeeping. ApplyGeneration writes {generation, hash,
// screen_programs, revoked} as ONE row and rests its atomicity on "SQLite's own
// statement-level atomicity under the store's WAL + synchronous=FULL write
// discipline" — that is what REL-056's prohibition on a torn cross-generation
// state depends on. A silently-ignored PRAGMA leaves the store one power-pull
// away from exactly the state that argument says cannot exist, and it would run
// that way indefinitely without complaint.
//
// A mutation sweep found two of its three checks pinned by nothing. The HAPPY
// path was already covered — durability_test.go asserts the PRAGMAs Open
// actually establishes — so what was missing is the guard's REFUSALS: this is the
// kind of check that never fires in a healthy run, and it needs a test that
// arranges an unhealthy one rather than waiting for the field to provide it.
// Nothing here repeats the Open-level assertion.

// dbWithoutPragmas opens the same driver Open uses, WITHOUT the write-discipline
// PRAGMAs — the shape a DSN typo or a driver that quietly dropped an unknown
// _pragma would produce.
func dbWithoutPragmas(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestVerifyWriteDisciplineRefusesAnUndurableConnection covers each PRAGMA
// independently, because they fail independently: a DSN can carry two of the
// three and the store would then be durable in one respect and not another.
func TestVerifyWriteDisciplineRefusesAnUndurableConnection(t *testing.T) {
	// Nothing set at all: the first check to run reports.
	if err := verifyWriteDiscipline(dbWithoutPragmas(t)); err == nil {
		t.Fatal("a connection with none of the write-discipline PRAGMAs was accepted — the store would run " +
			"undurably and ApplyGeneration's atomicity argument would be false")
	}

	for _, tc := range []struct {
		name    string
		pragmas []string
		wantMsg string
	}{
		{
			name:    "journal_mode left at the default",
			pragmas: []string{"PRAGMA synchronous = FULL", "PRAGMA busy_timeout = 5000"},
			wantMsg: "journal_mode",
		},
		{
			name:    "synchronous below FULL",
			pragmas: []string{"PRAGMA journal_mode = WAL", "PRAGMA synchronous = NORMAL", "PRAGMA busy_timeout = 5000"},
			wantMsg: "synchronous",
		},
		{
			name:    "busy_timeout left at the default",
			pragmas: []string{"PRAGMA journal_mode = WAL", "PRAGMA synchronous = FULL"},
			wantMsg: "busy_timeout",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := dbWithoutPragmas(t)
			for _, p := range tc.pragmas {
				if _, err := db.Exec(p); err != nil {
					t.Fatalf("%s: %v", p, err)
				}
			}
			err := verifyWriteDiscipline(db)
			if err == nil {
				t.Fatalf("accepted a connection with %s", tc.name)
			}
			// The REASON matters: each PRAGMA is a separate durability property,
			// and an error naming the wrong one would send an operator to fix a
			// setting that was already correct.
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("refused with %q, want one naming %s", err, tc.wantMsg)
			}
		})
	}
}

// TestVerifyWriteDisciplineAcceptsAProperlyConfiguredConnection is the control,
// and it is what stops the cases above from passing against a check that refuses
// every connection — which would make the relay unable to open its store at all.
func TestVerifyWriteDisciplineAcceptsAProperlyConfiguredConnection(t *testing.T) {
	db := dbWithoutPragmas(t)
	for _, p := range []string{"PRAGMA journal_mode = WAL", "PRAGMA synchronous = FULL", "PRAGMA busy_timeout = 5000"} {
		if _, err := db.Exec(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if err := verifyWriteDiscipline(db); err != nil {
		t.Errorf("a properly configured connection was refused: %v", err)
	}
}
