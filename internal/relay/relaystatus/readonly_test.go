package relaystatus

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestReadCreatesNoTables is the teeth behind "identity.Open is deliberately NOT
// used".
//
// identity.Open runs `CREATE TABLE IF NOT EXISTS` for eleven tables and three
// column migrations. Pointed at a live relay's store that would make this
// diagnostic WRITE the thing it inspects, and pointed at a store an earlier
// build wrote it would migrate it out from under the running process. Comparing
// file bytes cannot see that — a WAL commit lands in the sidecar — so this asks
// the database itself what tables exist, before and after.
func TestReadCreatesNoTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	// Materialise a real but empty SQLite file, with no schema at all.
	seed, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	if got := tableCount(t, path); got != 0 {
		t.Fatalf("the seed store already has %d tables; this test would prove nothing", got)
	}

	rep, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(rep.Problems) == 0 {
		t.Error("a store with no relay tables was reported clean")
	}
	if got := tableCount(t, path); got != 0 {
		t.Errorf("Read created %d table(s) in the store it was inspecting", got)
	}
}

func tableCount(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
