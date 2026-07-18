package signing

import (
	"crypto/ed25519"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadOrCreateGeneratesKey confirms LoadOrCreate on an empty dir
// generates a fresh, well-formed ed25519 signing key with no error.
func TestLoadOrCreateGeneratesKey(t *testing.T) {
	dir := t.TempDir()

	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate(%q) error: %v", dir, err)
	}

	pub := id.SigningPub()
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("SigningPub() length = %d, want %d (ed25519.PublicKeySize)", len(pub), ed25519.PublicKeySize)
	}
}

// TestLoadOrCreatePersistsAcrossCalls is the core persistence property: a
// second LoadOrCreate against the same dir must return the SAME public key
// as the first, not mint a fresh one — otherwise a relay's enrollment-time
// trust anchor would go stale on every feeder restart.
func TestLoadOrCreatePersistsAcrossCalls(t *testing.T) {
	dir := t.TempDir()

	id1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreate(%q) error: %v", dir, err)
	}

	id2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreate(%q) error: %v", dir, err)
	}

	if !id1.SigningPub().Equal(id2.SigningPub()) {
		t.Fatalf("SigningPub() changed across calls: first = %x, second = %x", id1.SigningPub(), id2.SigningPub())
	}
}

// TestLoadOrCreateDifferentDirsProduceDifferentKeys is a sanity check that
// LoadOrCreate is not returning some hardcoded or process-global key: two
// distinct, freshly-generated dirs must get distinct identities.
func TestLoadOrCreateDifferentDirsProduceDifferentKeys(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	idA, err := LoadOrCreate(dirA)
	if err != nil {
		t.Fatalf("LoadOrCreate(%q) error: %v", dirA, err)
	}
	idB, err := LoadOrCreate(dirB)
	if err != nil {
		t.Fatalf("LoadOrCreate(%q) error: %v", dirB, err)
	}

	if idA.SigningPub().Equal(idB.SigningPub()) {
		t.Fatal("two independently-generated identities in different dirs produced the same public key")
	}
}

// TestLoadOrCreateKeyFilePermissions confirms the private key material
// (both the ed25519 signing key and the TLS private key) lands on disk
// with 0600 permissions — never group/world readable.
func TestLoadOrCreateKeyFilePermissions(t *testing.T) {
	dir := t.TempDir()

	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatalf("LoadOrCreate(%q) error: %v", dir, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("os.ReadDir(%q) error: %v", dir, err)
	}

	var checked int
	for _, e := range entries {
		if e.IsDir() || !strings.Contains(strings.ToLower(e.Name()), "key") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("entry.Info() for %q error: %v", e.Name(), err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("file %q has perm %04o, want 0600", e.Name(), perm)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no private-key files found under the identity dir to check permissions on")
	}
}

// TestDefaultDirIsGitIgnored confirms the make-dev-local directory
// LoadOrCreate's key material is meant to live under (DefaultDir) is
// excluded by .gitignore — key material must never be committed.
func TestDefaultDirIsGitIgnored(t *testing.T) {
	root, err := repoRootForTest()
	if err != nil {
		t.Skipf("could not determine repo root via git (git unavailable?): %v", err)
	}

	target := filepath.Join(root, DefaultDir)

	cmd := exec.Command("git", "check-ignore", "-q", target)
	cmd.Dir = root
	if err := cmd.Run(); err != nil {
		t.Fatalf("git check-ignore -q %s: %v (want DefaultDir=%q to be git-ignored)", target, err, DefaultDir)
	}
}

func repoRootForTest() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
