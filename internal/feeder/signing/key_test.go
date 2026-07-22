package signing

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestServingLeafIsECDSAP256 is the browser-compatibility guard: the TLS leaf
// the feeder actually serves (assembled in cmd/waiveo-feeder exactly the way
// this test assembles it, tls.X509KeyPair(id.TLSCertPEM(), id.TLSKeyPEM()))
// MUST carry an ECDSA P-256 public key, not Ed25519. Real browsers
// (Chrome/Safari/Firefox) and macOS LibreSSL curl reject an Ed25519 server
// leaf outright ("peer doesn't support any of the certificate's signature
// algorithms"), so the embedded-SPA browser->feeder HTTPS path is unreachable
// with an Ed25519 leaf even though Go/Node clients accept it and keep every
// automated check green. ECDSA P-256 is universally supported by every TLS
// client that matters here.
//
// This asserts ONLY the TLS-serving leaf's algorithm. The feeder's Ed25519
// desired-state SIGNING identity is a separate key and MUST stay Ed25519 —
// TestSigningIdentityStaysEd25519 locks that half so this change never bleeds
// into the enrollment-anchored trust the relay verifies snapshots against.
func TestServingLeafIsECDSAP256(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate error: %v", err)
	}

	// Assemble the tls.Certificate exactly as cmd/waiveo-feeder/main.go does,
	// so this tests the leaf the listener would actually present.
	cert, err := tls.X509KeyPair(id.TLSCertPEM(), id.TLSKeyPEM())
	if err != nil {
		t.Fatalf("tls.X509KeyPair(TLSCertPEM, TLSKeyPEM): %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("assembled tls.Certificate has no leaf DER")
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse serving leaf: %v", err)
	}

	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("serving leaf public key is %T, want *ecdsa.PublicKey (real browsers reject Ed25519 server certs)", leaf.PublicKey)
	}
	if pub.Curve != elliptic.P256() {
		t.Fatalf("serving leaf curve = %v, want P-256", pub.Curve.Params().Name)
	}

	// The private half must be an ECDSA P-256 key too, and pair with the leaf.
	priv, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("serving private key is %T, want *ecdsa.PrivateKey", cert.PrivateKey)
	}
	if !priv.PublicKey.Equal(pub) {
		t.Fatal("serving private key does not pair with the leaf's public key")
	}
}

// TestSigningIdentityStaysEd25519 locks the other half of the split-algorithm
// invariant: the desired-state SIGNING key stays Ed25519 even as the TLS leaf
// moves to ECDSA. A relay learns this signing key at enrollment and verifies
// every pulled snapshot/lease against it (#28 enrollment-anchored trust), so
// its algorithm must not drift with the TLS-serving change.
func TestSigningIdentityStaysEd25519(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate error: %v", err)
	}
	if _, ok := id.SigningPriv().Public().(ed25519.PublicKey); !ok {
		t.Fatalf("signing public half is %T, want ed25519.PublicKey", id.SigningPriv().Public())
	}
	if len(id.SigningPub()) != ed25519.PublicKeySize {
		t.Fatalf("SigningPub() length = %d, want %d (ed25519)", len(id.SigningPub()), ed25519.PublicKeySize)
	}
}

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
