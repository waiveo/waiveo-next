package identity

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"
)

// revoked_test.go covers the durable half of relay/1 REL-123: a generation's
// `revocation_and_site.revoked` set (REL-066) rides the SAME atomic
// last-applied row write as its {generation, hash, screen_programs}, so it
// survives a power cycle exactly as they do and is enforceable on a boot that
// cannot reach the app peer at all.

// TestLastAppliedRevokedEmptyPlaceholderOnFreshStore asserts a store that has
// never applied a generation reports the REL-060 empty placeholder (`[]`), not
// nil and not an error — the offline boot read (desiredstate.ServedRevocation)
// then decodes cleanly to an empty set rather than failing the boot.
func TestLastAppliedRevokedEmptyPlaceholderOnFreshStore(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	got, err := store.LastAppliedRevokedScreens()
	if err != nil {
		t.Fatalf("LastAppliedRevokedScreens() on a fresh store: %v", err)
	}
	if !bytes.Equal(got, []byte("[]")) {
		t.Errorf("LastAppliedRevokedScreens() on a fresh store = %q, want %q (REL-060 empty placeholder)", got, "[]")
	}
}

// TestApplyGenerationPersistsRevokedAcrossReopen is the property that makes
// REL-123 enforceable "regardless of connectivity" across a RESTART and not
// merely across a disconnection the process happened to survive: the set is
// read back from a store closed and reopened from the same on-disk file, with
// no live app peer anywhere in the read path.
func TestApplyGenerationPersistsRevokedAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	programs := []byte(`[{"screen_id":"01J8Z3K4N5P6Q7R8S9T0V1W2X6","program_revision":"rev-99"}]`)
	revoked := []byte(`["01J8Z3K4N5P6Q7R8S9T0V1W2X6","01J8Z3K4N5P6Q7R8S9T0V1W2X7"]`)
	if err := store.ApplyGeneration(42, "sha256:beef", programs, revoked, nil); err != nil {
		t.Fatalf("ApplyGeneration: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close(): %v", err)
	}

	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (reopen): %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	got, err := reopened.LastAppliedRevokedScreens()
	if err != nil {
		t.Fatalf("LastAppliedRevokedScreens after reopen: %v", err)
	}
	if !bytes.Equal(got, revoked) {
		t.Errorf("LastAppliedRevokedScreens after reopen = %q, want %q — a synced revocation that does not survive a restart is not enforced regardless of connectivity (REL-123)", got, revoked)
	}

	// The programs and the generation came back too: the point of the single
	// row write is that the four values agree about which generation they are,
	// so a test that only checked one of them could pass on a torn row.
	gotPrograms, err := reopened.LastAppliedScreenPrograms()
	if err != nil {
		t.Fatalf("LastAppliedScreenPrograms after reopen: %v", err)
	}
	if !bytes.Equal(gotPrograms, programs) {
		t.Errorf("LastAppliedScreenPrograms after reopen = %q, want %q", gotPrograms, programs)
	}
	gen, hash, ok, err := reopened.LastAppliedGeneration()
	if err != nil || !ok {
		t.Fatalf("LastAppliedGeneration after reopen: ok=%v err=%v", ok, err)
	}
	if gen != 42 || hash != "sha256:beef" {
		t.Errorf("LastAppliedGeneration after reopen = {%d, %q}, want {42, \"sha256:beef\"}", gen, hash)
	}
}

// TestApplyGenerationReplacesRevokedWholesale pins the direction a revocation
// can be TAKEN BACK in durable storage. `revoked` is a set the app peer
// restates in full on every snapshot (REL-066 defines no negative entry), so a
// generation that no longer names a screen must leave the persisted row no
// longer naming it either — otherwise a withdrawal survives only until the next
// restart, which reinstates a revocation the app peer had lifted.
func TestApplyGenerationReplacesRevokedWholesale(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.ApplyGeneration(1, "sha256:aa", nil, []byte(`["screen-a"]`), nil); err != nil {
		t.Fatalf("ApplyGeneration(1): %v", err)
	}
	if err := store.ApplyGeneration(2, "sha256:bb", nil, nil, nil); err != nil {
		t.Fatalf("ApplyGeneration(2): %v", err)
	}

	got, err := store.LastAppliedRevokedScreens()
	if err != nil {
		t.Fatalf("LastAppliedRevokedScreens: %v", err)
	}
	if !bytes.Equal(got, []byte("[]")) {
		t.Errorf("persisted revoked after a generation that revokes nothing = %q, want %q — the set is restated in full, not accumulated", got, "[]")
	}
}

// TestOpenMigratesPreRevokedLastAppliedRow asserts a last_applied_generation
// created by an EARLIER build keeps working when it is upgraded. `CREATE TABLE
// IF NOT EXISTS` is a no-op against an existing table, so without the migration
// an already-deployed relay would keep a four-column row and fail every
// ApplyGeneration — no generation applying at all.
func TestOpenMigratesPreRevokedLastAppliedRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	// The table exactly as the build before `revoked` created it, with a row in
	// it, so the migration has to preserve real state rather than an empty one.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE last_applied_generation (
		id              INTEGER PRIMARY KEY CHECK (id = 1),
		generation      INTEGER NOT NULL,
		hash            TEXT NOT NULL,
		screen_programs BLOB NOT NULL DEFAULT '[]'
	)`); err != nil {
		t.Fatalf("create pre-revoked last_applied_generation: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO last_applied_generation (id, generation, hash, screen_programs) VALUES (1, 9, 'sha256:old', '[{"screen_id":"s1"}]')`,
	); err != nil {
		t.Fatalf("seed pre-revoked row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw handle: %v", err)
	}

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open over a pre-revoked store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The migrated row reads back as revoking nothing — the safe default, and
	// not a lost statement: the old build had nowhere to persist one.
	got, err := store.LastAppliedRevokedScreens()
	if err != nil {
		t.Fatalf("LastAppliedRevokedScreens over a migrated store: %v", err)
	}
	if !bytes.Equal(got, []byte("[]")) {
		t.Errorf("migrated revoked = %q, want %q", got, "[]")
	}

	// The pre-existing generation and programs survived the migration.
	gen, hash, ok, err := store.LastAppliedGeneration()
	if err != nil || !ok || gen != 9 || hash != "sha256:old" {
		t.Errorf("LastAppliedGeneration after migration = {%d, %q} ok=%v err=%v, want {9, \"sha256:old\"}", gen, hash, ok, err)
	}

	// And the migrated table accepts a new apply carrying a revocation.
	if err := store.ApplyGeneration(10, "sha256:new", nil, []byte(`["s1"]`), nil); err != nil {
		t.Fatalf("ApplyGeneration over a migrated store: %v", err)
	}
	got, err = store.LastAppliedRevokedScreens()
	if err != nil {
		t.Fatalf("LastAppliedRevokedScreens after a post-migration apply: %v", err)
	}
	if !bytes.Equal(got, []byte(`["s1"]`)) {
		t.Errorf("post-migration revoked = %q, want %q", got, `["s1"]`)
	}
}
