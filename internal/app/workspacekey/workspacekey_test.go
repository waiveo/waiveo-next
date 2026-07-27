package workspacekey_test

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/workspacekey"
)

// testKeyID is the ULID LoadOrCreate's injected id source mints. Spelled out
// rather than generated so a failing assertion prints the same value every run.
const testKeyID = "01J8Z3K4N5P6Q7R8S9T0V1W2ZB"

func newID() string { return testKeyID }

// wrapKey is a stand-in for the sub-key `archive/1` ARC-011 derives for
// data-key wrapping: 32 bytes, the width chacha20poly1305.NewX requires.
func wrapKey() []byte { return make([]byte, 32) }

func loadKey(t *testing.T, dir string) *workspacekey.Key {
	t.Helper()
	k, err := workspacekey.LoadOrCreate(dir, newID)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	return k
}

// TestLoadOrCreateIsStableAcrossCalls pins the property an already-written
// archive depends on: a second load of the same directory yields the SAME
// private half and the same id, so a container signed yesterday still verifies
// today.
func TestLoadOrCreateIsStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	first := loadKey(t, dir)
	second := loadKey(t, dir)

	if !first.Private().Equal(second.Private()) {
		t.Error("a reload produced a different private half; every already-written archive would stop verifying")
	}
	if first.KeyID() != second.KeyID() {
		t.Errorf("a reload produced signer_key_id %q, want the persisted %q", second.KeyID(), first.KeyID())
	}
}

// TestPersistedKeyMaterialIsOwnerOnly: the private half, the data key and the id
// are secrets at rest, in a directory nobody else may even traverse.
//
// The directory is pre-created 0755 on purpose. os.MkdirAll applies its mode
// only to a directory it creates, so a key directory that already exists — the
// second boot of every deployment, and any directory an operator made by hand —
// keeps whatever mode it had unless something actively tightens it.
func TestPersistedKeyMaterialIsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("pre-create key dir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("pre-chmod key dir: %v", err)
	}

	loadKey(t, dir)

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat key dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("key directory mode = %04o, want 0700", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read key dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the key directory holds no files at all after LoadOrCreate")
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", e.Name(), got)
		}
	}
}

// TestDestroyRemovesPersistedMaterial: SEC-121's destruction leaves nothing on
// disk to reload.
func TestDestroyRemovesPersistedMaterial(t *testing.T) {
	dir := t.TempDir()
	k := loadKey(t, dir)

	if err := k.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read key dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("key directory still holds %v after Destroy", names)
	}
}

// TestDestroyIsIdempotent: a second delete request must not fail merely because
// the first one succeeded.
func TestDestroyIsIdempotent(t *testing.T) {
	k := loadKey(t, t.TempDir())
	if err := k.Destroy(); err != nil {
		t.Fatalf("first Destroy: %v", err)
	}
	if err := k.Destroy(); err != nil {
		t.Errorf("second Destroy: %v, want nil — destruction is idempotent by nature", err)
	}
}

// TestDestroyedKeyRefusesToWrapItsDataKey is the regression this file exists
// for.
//
// Destroy used to zero the data key IN PLACE and leave it at its full length,
// while WrapDataKey tested only that length. The wrap therefore SUCCEEDED after
// destruction, sealing 32 zero bytes and returning them as the workspace's data
// key — a manifest whose `data_key_wrap` protects nothing, produced by an export
// the API reported as `succeeded`.
func TestDestroyedKeyRefusesToWrapItsDataKey(t *testing.T) {
	k := loadKey(t, t.TempDir())
	if _, err := k.WrapDataKey(wrapKey()); err != nil {
		t.Fatalf("precondition: WrapDataKey on a live key: %v", err)
	}

	if err := k.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	wrapped, err := k.WrapDataKey(wrapKey())
	if err == nil {
		t.Fatalf("WrapDataKey returned %q after Destroy, want an error — a destroyed key must not wrap anything", wrapped)
	}
	if !errors.Is(err, workspacekey.ErrDestroyed) {
		t.Errorf("WrapDataKey error = %v, want one wrapping ErrDestroyed", err)
	}
}

// TestDestroyedKeyReportsNoDataKey: destruction must be OBSERVABLE to every
// consumer, not only to the one that happens to call the operation that fails.
func TestDestroyedKeyReportsNoDataKey(t *testing.T) {
	k := loadKey(t, t.TempDir())
	if !k.DataKeyPresent() {
		t.Fatal("precondition: a freshly established key reports no data key")
	}
	if k.Destroyed() {
		t.Fatal("precondition: a freshly established key reports itself destroyed")
	}

	if err := k.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if k.DataKeyPresent() {
		t.Error("DataKeyPresent() is true after Destroy")
	}
	if !k.Destroyed() {
		t.Error("Destroyed() is false after Destroy")
	}
}

// TestDestroyedKeyHasNothingToSignWith: the signing half and the id an archive
// header would name it by are both gone, so a caller that reaches for them gets
// a value the archive writer refuses (an empty signer, an empty signer_key_id)
// rather than a zeroed key it would sign with.
func TestDestroyedKeyHasNothingToSignWith(t *testing.T) {
	k := loadKey(t, t.TempDir())
	if len(k.Private()) != ed25519.PrivateKeySize {
		t.Fatalf("precondition: private half is %d bytes, want %d", len(k.Private()), ed25519.PrivateKeySize)
	}

	if err := k.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if got := len(k.Private()); got != 0 {
		t.Errorf("Private() is %d bytes after Destroy, want 0", got)
	}
	if got := len(k.Public()); got != 0 {
		t.Errorf("Public() is %d bytes after Destroy, want 0", got)
	}
	if got := k.KeyID(); got != "" {
		t.Errorf("KeyID() = %q after Destroy, want the empty string", got)
	}
}

// TestPrivateIsNotAliasedIntoTheCaller: Private returns a copy, so destroying
// the key cannot reach back into key material a caller already holds and zero it
// mid-signature. The alias is what made "the key is zeroed in place" a
// data race as well as a correctness bug.
func TestPrivateIsNotAliasedIntoTheCaller(t *testing.T) {
	k := loadKey(t, t.TempDir())
	held := k.Private()

	if err := k.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	zero := make([]byte, ed25519.PrivateKeySize)
	if held.Equal(ed25519.PrivateKey(zero)) {
		t.Error("the private half a caller already held was zeroed by Destroy — Private() handed out an alias of the key's own storage")
	}
}
