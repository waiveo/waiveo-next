package identity

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestServedScreenProgramsEmptyPlaceholderOnFreshStore asserts that a store
// which has never persisted any screen_programs reports the REL-060 empty
// placeholder (`[]`), not a nil/absent value — the offline serve path
// (desiredstate.ServedProgram) then decodes it to an empty program set
// rather than erroring, so a relay that has applied a generation carrying
// no screen_programs still serves cleanly offline.
func TestServedScreenProgramsEmptyPlaceholderOnFreshStore(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	got, err := store.LastAppliedScreenPrograms()
	if err != nil {
		t.Fatalf("LastAppliedScreenPrograms() on a fresh store: %v", err)
	}
	if !bytes.Equal(got, []byte("[]")) {
		t.Errorf("LastAppliedScreenPrograms() on a fresh store = %q, want %q (REL-060 empty placeholder)", got, "[]")
	}
}

// TestServedScreenProgramsRoundTrip confirms SetServedScreenPrograms persists
// the applied screen_programs JSON beside the last-applied {generation, hash}
// (REL-055) and LastAppliedScreenPrograms reads back exactly those bytes —
// the signed pointers a screen resolves against its own origin (REL-061),
// never asset bytes (REL-140).
func TestServedScreenProgramsRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The screen_programs facet is a facet of the last-applied generation
	// record, so the row must exist first (SetLastAppliedGeneration creates it).
	if err := store.SetLastAppliedGeneration(50, "sha256:deadbeef"); err != nil {
		t.Fatalf("SetLastAppliedGeneration: %v", err)
	}

	payload := []byte(`[{"screen_id":"01J8Z3K4N5P6Q7R8S9T0V1W2X6","program_revision":"rev-99","priority":"preempt","display":"content","content":[{"asset_ref":"sha256:cccc","url":"https://app.example/cas/cccc0000","expires_at":1752545000000}]}]`)
	if err := store.SetServedScreenPrograms(payload); err != nil {
		t.Fatalf("SetServedScreenPrograms: %v", err)
	}

	got, err := store.LastAppliedScreenPrograms()
	if err != nil {
		t.Fatalf("LastAppliedScreenPrograms: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("LastAppliedScreenPrograms() = %q, want %q", got, payload)
	}
}

// TestApplyGenerationSwapsGenerationAndProgramsTogether is the REL-056
// atomic-swap property at the store API: a single ApplyGeneration call moves
// {generation, hash} and screen_programs to the new generation together, so a
// reader NEVER sees the new generation paired with the prior generation's
// screen_programs. It stages gen 50 (a scheduled/content program), applies an
// emergency gen 51 (a preempt/blank assignment), and asserts both facets read
// back at gen 51 — the exact cross-generation pair the two-call apply could
// tear across a power-pull.
func TestApplyGenerationSwapsGenerationAndProgramsTogether(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	progs50 := []byte(`[{"screen_id":"S1","program_revision":"rev-50","priority":"scheduled","display":"content"}]`)
	progs51 := []byte(`[{"screen_id":"S1","program_revision":"rev-51","priority":"preempt","display":"blank"}]`)

	if err := store.ApplyGeneration(50, "sha256:50", progs50, nil, nil); err != nil {
		t.Fatalf("ApplyGeneration(50): %v", err)
	}
	if err := store.ApplyGeneration(51, "sha256:51", progs51, nil, nil); err != nil {
		t.Fatalf("ApplyGeneration(51): %v", err)
	}

	gen, hash, ok, err := store.LastAppliedGeneration()
	if err != nil || !ok {
		t.Fatalf("LastAppliedGeneration: gen=%d ok=%v err=%v", gen, ok, err)
	}
	if gen != 51 || hash != "sha256:51" {
		t.Errorf("LastAppliedGeneration = (%d, %q), want (51, sha256:51)", gen, hash)
	}
	got, err := store.LastAppliedScreenPrograms()
	if err != nil {
		t.Fatalf("LastAppliedScreenPrograms: %v", err)
	}
	// The crux: the screen_programs read back at gen 51 must be gen 51's
	// preempt/blank array, never gen 50's stale scheduled/content array — the
	// torn cross-generation mix REL-056 forbids.
	if !bytes.Equal(got, progs51) {
		t.Errorf("LastAppliedScreenPrograms after gen-51 swap = %q, want gen 51's %q (REL-056: no torn cross-generation mix)", got, progs51)
	}
}

// TestApplyGenerationEmptyPlaceholder confirms ApplyGeneration stores a nil/empty
// screen_programs as the REL-060 empty placeholder (`[]`), matching
// SetServedScreenPrograms, so the offline serve path decodes cleanly.
func TestApplyGenerationEmptyPlaceholder(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.ApplyGeneration(7, "sha256:7", nil, nil, nil); err != nil {
		t.Fatalf("ApplyGeneration(nil programs): %v", err)
	}
	got, err := store.LastAppliedScreenPrograms()
	if err != nil {
		t.Fatalf("LastAppliedScreenPrograms: %v", err)
	}
	if !bytes.Equal(got, []byte("[]")) {
		t.Errorf("LastAppliedScreenPrograms after ApplyGeneration(nil) = %q, want %q (REL-060 empty placeholder)", got, "[]")
	}
}

// TestServedScreenProgramsSurviveReopen is the REL-055 offline-continuity
// property applied to screen_programs: the persisted program survives a full
// close+reopen of the file-backed store (a power cycle), so a restarted relay
// serves its last-applied screen program purely from durable local storage,
// without first contacting its app peer.
func TestServedScreenProgramsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	payload := []byte(`[{"screen_id":"S1","program_revision":"rev-99","priority":"preempt","display":"content"}]`)

	store1, err := Open(path)
	if err != nil {
		t.Fatalf("Open (1): %v", err)
	}
	if err := store1.SetLastAppliedGeneration(50, "sha256:deadbeef"); err != nil {
		t.Fatalf("SetLastAppliedGeneration: %v", err)
	}
	if err := store1.SetServedScreenPrograms(payload); err != nil {
		t.Fatalf("SetServedScreenPrograms: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Close (1): %v", err)
	}

	store2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (2): %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	got, err := store2.LastAppliedScreenPrograms()
	if err != nil {
		t.Fatalf("LastAppliedScreenPrograms() on reopened store: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("LastAppliedScreenPrograms() after reopen = %q, want %q (REL-055 offline continuity)", got, payload)
	}
}

// TestOpenMigratesPreScreenProgramsLastAppliedRow asserts the OLDEST shape of
// last_applied_generation — the original {id, generation, hash}, before either
// column that has since been added to it — is migrated all the way forward.
//
// The row shape and the migration were added by the same commit, which is
// exactly what leaves the gap: `CREATE TABLE IF NOT EXISTS` is a no-op against a
// table an earlier build already created, so a store from before
// `screen_programs` existed never gets the column from the schema, and — until
// the migration named it — never got it from a migration either. Every
// ApplyGeneration against such a store fails on `no such column:
// screen_programs`, which is the same total apply outage the `revoked` migration
// exists to prevent one column later.
func TestOpenMigratesPreScreenProgramsLastAppliedRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	// The table exactly as the build before `screen_programs` created it, with a
	// row in it, so the migration has to preserve real state rather than an
	// empty one.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE last_applied_generation (
		id          INTEGER PRIMARY KEY CHECK (id = 1),
		generation  INTEGER NOT NULL,
		hash        TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create pre-screen_programs last_applied_generation: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO last_applied_generation (id, generation, hash) VALUES (1, 4, 'sha256:ancient')`,
	); err != nil {
		t.Fatalf("seed pre-screen_programs row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw handle: %v", err)
	}

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open over a pre-screen_programs store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Both added columns read back as the REL-060 empty placeholder.
	programs, err := store.LastAppliedScreenPrograms()
	if err != nil {
		t.Fatalf("LastAppliedScreenPrograms over a migrated store: %v", err)
	}
	if !bytes.Equal(programs, []byte("[]")) {
		t.Errorf("migrated screen_programs = %q, want %q", programs, "[]")
	}
	revoked, err := store.LastAppliedRevokedScreens()
	if err != nil {
		t.Fatalf("LastAppliedRevokedScreens over a migrated store: %v", err)
	}
	if !bytes.Equal(revoked, []byte("[]")) {
		t.Errorf("migrated revoked = %q, want %q", revoked, "[]")
	}

	// The pre-existing generation survived the migration.
	gen, hash, ok, err := store.LastAppliedGeneration()
	if err != nil || !ok || gen != 4 || hash != "sha256:ancient" {
		t.Errorf("LastAppliedGeneration after migration = {%d, %q} ok=%v err=%v, want {4, \"sha256:ancient\"}", gen, hash, ok, err)
	}

	// And the migrated table accepts a full apply — the operation a store stuck
	// on the old shape fails outright, taking every later generation with it.
	if err := store.ApplyGeneration(5, "sha256:new", []byte(`[{"screen_id":"s1"}]`), []byte(`["s1"]`), nil); err != nil {
		t.Fatalf("ApplyGeneration over a migrated store: %v", err)
	}
	programs, err = store.LastAppliedScreenPrograms()
	if err != nil {
		t.Fatalf("LastAppliedScreenPrograms after a post-migration apply: %v", err)
	}
	if !bytes.Equal(programs, []byte(`[{"screen_id":"s1"}]`)) {
		t.Errorf("post-migration screen_programs = %q, want %q", programs, `[{"screen_id":"s1"}]`)
	}
}
