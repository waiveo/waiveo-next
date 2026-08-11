package identity

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"
)

// deviceinventory_test.go covers the durable half of relay/1 REL-063/064: a
// generation's `device_inventory` section — the ADOPTED set, the relay's only
// authority for which devices it may drive — rides the SAME atomic last-applied
// row write as its {generation, hash, screen_programs, revoked}, so it survives
// a power cycle exactly as they do and is usable on a boot that cannot reach
// the app peer at all.
//
// The failure it exists to prevent is quieter than the revocation one. Every
// consumer of the adopted set fails CLOSED, so a relay that boots without it
// does not error, does not warn, and does not look unhealthy — it simply drives
// nothing and keeps no screen alive, which on the offline boot after a power
// outage means a wall of screens parked at the Roku Home screen showing nothing.

// wantInventory is a `device_inventory` section as a snapshot carries it
// (REL-063's `devices` plus REL-064's `pack_match_patterns`), spelled out as
// raw JSON because that is exactly the byte shape the durable row holds.
const wantInventory = `{"devices":[{"device_id":"01J8Z3K4N5P6Q7R8S9T0V1DEV1","driver":"roku","native_id":"uuid:roku:ecp:AA11","entities":[{"entity_id":"01J8Z3K4N5P6Q7R8S9T0V1ENT1","device_class":"media-player","enabled":true}]}],"pack_match_patterns":[]}`

// TestLastAppliedDeviceInventoryEmptyPlaceholderOnFreshStore asserts a store
// that has never applied a generation reports the REL-060 empty placeholder,
// not nil and not an error — the offline boot read
// (desiredstate.ServedDeviceInventory) then decodes cleanly to an empty
// inventory rather than failing the boot.
//
// The placeholder is object-shaped rather than `[]`, because `device_inventory`
// is an object of two arrays: "present and empty" for it means both arrays
// present and empty, which is what the signed section itself says.
func TestLastAppliedDeviceInventoryEmptyPlaceholderOnFreshStore(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	got, err := store.LastAppliedDeviceInventory()
	if err != nil {
		t.Fatalf("LastAppliedDeviceInventory() on a fresh store: %v", err)
	}
	if !bytes.Equal(got, []byte(emptyDeviceInventoryJSON)) {
		t.Errorf("LastAppliedDeviceInventory() on a fresh store = %q, want %q (REL-060 empty placeholder)", got, emptyDeviceInventoryJSON)
	}
}

// TestApplyGenerationPersistsDeviceInventoryAcrossReopen is the round trip that
// makes the adopted set available on a boot with no app peer: it is read back
// from a store closed and reopened from the same on-disk file, with nothing
// live anywhere in the read path.
//
// Before this, `device_inventory` reached the process on the returned Applied
// and stopped there. The row it should have ridden — the one holding the
// programs those very devices display — carried the other four fields only, so
// the adopted set existed for exactly as long as the process did.
func TestApplyGenerationPersistsDeviceInventoryAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	programs := []byte(`[{"screen_id":"01J8Z3K4N5P6Q7R8S9T0V1W2X6","program_revision":"rev-99"}]`)
	revoked := []byte(`["01J8Z3K4N5P6Q7R8S9T0V1W2X7"]`)
	if err := store.ApplyGeneration(42, "sha256:beef", programs, revoked, []byte(wantInventory)); err != nil {
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

	got, err := reopened.LastAppliedDeviceInventory()
	if err != nil {
		t.Fatalf("LastAppliedDeviceInventory after reopen: %v", err)
	}
	if !bytes.Equal(got, []byte(wantInventory)) {
		t.Errorf("LastAppliedDeviceInventory after reopen = %q, want %q — an adopted set that does not survive a restart leaves the relay driving nothing on the boot that follows a power cut", got, wantInventory)
	}

	// The other four came back too, at the same generation. The point of the
	// single row write is that the five values agree about which generation
	// they are, so a test that only checked the new one could pass on a torn
	// row — a relay driving generation N-1's adopted devices under generation
	// N's programs.
	gotPrograms, err := reopened.LastAppliedScreenPrograms()
	if err != nil {
		t.Fatalf("LastAppliedScreenPrograms after reopen: %v", err)
	}
	if !bytes.Equal(gotPrograms, programs) {
		t.Errorf("LastAppliedScreenPrograms after reopen = %q, want %q", gotPrograms, programs)
	}
	gotRevoked, err := reopened.LastAppliedRevokedScreens()
	if err != nil {
		t.Fatalf("LastAppliedRevokedScreens after reopen: %v", err)
	}
	if !bytes.Equal(gotRevoked, revoked) {
		t.Errorf("LastAppliedRevokedScreens after reopen = %q, want %q", gotRevoked, revoked)
	}
	gen, hash, ok, err := reopened.LastAppliedGeneration()
	if err != nil || !ok {
		t.Fatalf("LastAppliedGeneration after reopen: ok=%v err=%v", ok, err)
	}
	if gen != 42 || hash != "sha256:beef" {
		t.Errorf("LastAppliedGeneration after reopen = {%d, %q}, want {42, \"sha256:beef\"}", gen, hash)
	}
}

// TestApplyGenerationReplacesDeviceInventoryWholesale pins the direction an
// adoption can be TAKEN BACK in durable storage. REL-063's section is a
// complete statement of the adopted set — it has no negative entry — so a
// generation that no longer names a device must leave the persisted row no
// longer naming it either. Accumulating instead would make un-adoption survive
// only until the next restart, which is the shape of bug that has this relay
// re-launching a screen an operator explicitly handed back to the legacy stack.
func TestApplyGenerationReplacesDeviceInventoryWholesale(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.ApplyGeneration(1, "sha256:aa", nil, nil, []byte(wantInventory)); err != nil {
		t.Fatalf("ApplyGeneration(1): %v", err)
	}
	if err := store.ApplyGeneration(2, "sha256:bb", nil, nil, nil); err != nil {
		t.Fatalf("ApplyGeneration(2): %v", err)
	}

	got, err := store.LastAppliedDeviceInventory()
	if err != nil {
		t.Fatalf("LastAppliedDeviceInventory: %v", err)
	}
	if !bytes.Equal(got, []byte(emptyDeviceInventoryJSON)) {
		t.Errorf("persisted device_inventory after a generation that adopts nothing = %q, want %q — the set is restated in full, not accumulated", got, emptyDeviceInventoryJSON)
	}
}

// TestOpenMigratesPreDeviceInventoryLastAppliedRow asserts a
// last_applied_generation created by an EARLIER build keeps working when it is
// upgraded. `CREATE TABLE IF NOT EXISTS` is a no-op against an existing table,
// so without the migration every already-deployed relay would keep a
// five-column row and fail every ApplyGeneration on `no such column:
// device_inventory` — no generation applying at all, which is a harder outage
// than the one this column exists to fix.
func TestOpenMigratesPreDeviceInventoryLastAppliedRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	// The table exactly as the build before `device_inventory` created it, with
	// a row in it, so the migration has to preserve real state rather than an
	// empty one.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE last_applied_generation (
		id              INTEGER PRIMARY KEY CHECK (id = 1),
		generation      INTEGER NOT NULL,
		hash            TEXT NOT NULL,
		screen_programs BLOB NOT NULL DEFAULT '[]',
		revoked         BLOB NOT NULL DEFAULT '[]'
	)`); err != nil {
		t.Fatalf("create pre-device_inventory last_applied_generation: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO last_applied_generation (id, generation, hash, screen_programs, revoked) VALUES (1, 9, 'sha256:old', '[{"screen_id":"s1"}]', '["s2"]')`,
	); err != nil {
		t.Fatalf("seed pre-device_inventory row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw handle: %v", err)
	}

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open over a pre-device_inventory store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The migrated row reads back as adopting nothing — the safe default, and
	// not a lost statement: the old build had nowhere to persist one.
	got, err := store.LastAppliedDeviceInventory()
	if err != nil {
		t.Fatalf("LastAppliedDeviceInventory over a migrated store: %v", err)
	}
	if !bytes.Equal(got, []byte(emptyDeviceInventoryJSON)) {
		t.Errorf("migrated device_inventory = %q, want %q", got, emptyDeviceInventoryJSON)
	}

	// The pre-existing generation, programs and revocation survived.
	gen, hash, ok, err := store.LastAppliedGeneration()
	if err != nil || !ok || gen != 9 || hash != "sha256:old" {
		t.Errorf("LastAppliedGeneration after migration = {%d, %q} ok=%v err=%v, want {9, \"sha256:old\"}", gen, hash, ok, err)
	}
	gotRevoked, err := store.LastAppliedRevokedScreens()
	if err != nil {
		t.Fatalf("LastAppliedRevokedScreens over a migrated store: %v", err)
	}
	if !bytes.Equal(gotRevoked, []byte(`["s2"]`)) {
		t.Errorf("migrated revoked = %q, want %q — the migration must preserve the columns it is not adding", gotRevoked, `["s2"]`)
	}

	// And the migrated table accepts a new apply carrying an adopted set.
	if err := store.ApplyGeneration(10, "sha256:new", nil, nil, []byte(wantInventory)); err != nil {
		t.Fatalf("ApplyGeneration over a migrated store: %v", err)
	}
	got, err = store.LastAppliedDeviceInventory()
	if err != nil {
		t.Fatalf("LastAppliedDeviceInventory after a post-migration apply: %v", err)
	}
	if !bytes.Equal(got, []byte(wantInventory)) {
		t.Errorf("post-migration device_inventory = %q, want %q", got, wantInventory)
	}
}
