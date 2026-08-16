package signing

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/signhash"
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
	assertServingLeafECDSAP256(t, id)
}

// TestLoadSelfHealsStaleEd25519ServingLeaf is the regression guard for the
// self-healing load path. A feeder identity persisted BEFORE the ECDSA-leaf fix
// carries the old GenSelfSigned output: an Ed25519 signing key AND an Ed25519
// TLS leaf. TestServingLeafIsECDSAP256 above can never observe that state — it
// only ever calls LoadOrCreate(t.TempDir()), an always-fresh dir that takes the
// create() path under today's code. But .dev/feeder-keys is designed to PERSIST
// (the signing key is the relay's enrollment anchor) and the Makefile's dev-up
// clears .dev/relay-identity yet never .dev/feeder-keys — so a dev checkout that
// ran the stack once before this fix keeps a stale Ed25519 leaf on disk, and
// load() reloading it verbatim would re-serve the browser-incompatible leaf
// across every later `make dev`/`make web-dev` with no error.
//
// LoadOrCreate must repair such an identity in place on the next load: reissue
// an ECDSA P-256 serving leaf while preserving the enrollment-anchored Ed25519
// SIGNING key byte-for-byte, and PERSIST the reissued material so a subsequent
// restart reuses it (a stable pin for the relay's next enrollment) rather than
// re-minting a fresh leaf every load.
func TestLoadSelfHealsStaleEd25519ServingLeaf(t *testing.T) {
	dir := t.TempDir()

	// Seed a pre-fix identity straight to disk: a real Ed25519 signing key plus
	// an Ed25519 TLS leaf — exactly what the old GenSelfSigned wrote — so
	// LoadOrCreate takes its load() branch against genuinely stale material.
	signingPub := seedEd25519SigningKey(t, filepath.Join(dir, signingKeyFile))
	seedEd25519TLSLeaf(t, filepath.Join(dir, tlsCertFile), filepath.Join(dir, tlsKeyFile))

	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate(%q) error: %v", dir, err)
	}

	// The enrollment-anchored signing identity must survive the heal untouched.
	if !id.SigningPub().Equal(signingPub) {
		t.Fatalf("self-heal changed the signing pub: got %x, want seeded %x", id.SigningPub(), signingPub)
	}
	// The served leaf must now be ECDSA P-256, not the seeded Ed25519 leaf.
	assertServingLeafECDSAP256(t, id)

	// The heal must be persisted: a second load reuses the SAME ECDSA leaf (no
	// per-restart churn) and still yields the original signing key.
	reloaded, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreate(%q) error: %v", dir, err)
	}
	assertServingLeafECDSAP256(t, reloaded)
	if !bytes.Equal(id.TLSCertPEM(), reloaded.TLSCertPEM()) {
		t.Fatal("healed TLS leaf not persisted: second load re-minted a different cert")
	}
	if !reloaded.SigningPub().Equal(signingPub) {
		t.Fatalf("second load changed the signing pub: got %x, want seeded %x", reloaded.SigningPub(), signingPub)
	}
}

// assertServingLeafECDSAP256 fails t unless id's TLS serving material —
// assembled exactly as cmd/waiveo-feeder/main.go does,
// tls.X509KeyPair(id.TLSCertPEM(), id.TLSKeyPEM()) — is an ECDSA P-256 leaf
// whose private half pairs with it. That is the algorithm every real browser
// and macOS LibreSSL curl require of the feeder's HTTPS leaf; an Ed25519 leaf
// is rejected at handshake.
func assertServingLeafECDSAP256(t *testing.T, id *Identity) {
	t.Helper()

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

// seedEd25519SigningKey writes a real Ed25519 PKCS8 signing key to path (0600),
// mimicking a pre-fix persisted signing_key.pem, and returns its public half.
func seedEd25519SigningKey(t *testing.T, path string) ed25519.PublicKey {
	t.Helper()

	pub, priv := signhash.GenerateKey()
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal ed25519 signing key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write seeded signing key: %v", err)
	}
	return pub
}

// seedEd25519TLSLeaf writes a self-signed Ed25519 TLS leaf and its PKCS8 key to
// certPath (0644) / keyPath (0600) — the exact pre-fix GenSelfSigned output, so
// load() sees the stale, browser-incompatible serving material a real dev
// checkout carries after running the stack once before the ECDSA fix.
func seedEd25519TLSLeaf(t *testing.T, certPath, keyPath string) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 tls key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "waiveo-relay"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create ed25519 tls cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("write seeded tls cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal ed25519 tls key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write seeded tls key: %v", err)
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

// A SAN-less P-256 leaf is the SECOND stale generation: minted by the
// GenSelfSigned that predates the pack runtime, it satisfies every SPKI-pinning
// consumer and can never satisfy a stock verifying client — a pack — because
// Go matches SANs and never reads the CommonName. load() must reissue the
// CERTIFICATE over the SAME key: the key is fine, and a new key would be a new
// SPKI — every enrolled relay's pin broken (REL-137), a fleet outage bought to
// fix a cert's shape. The Ed25519 heal above replaces the key because the key
// IS its problem; this one must not.
func TestLoadSelfHealsSANLessServingLeafPreservingItsSPKI(t *testing.T) {
	dir := t.TempDir()
	signingPub := seedEd25519SigningKey(t, filepath.Join(dir, signingKeyFile))
	seedSANLessP256TLSLeaf(t, filepath.Join(dir, tlsCertFile), filepath.Join(dir, tlsKeyFile))
	seededSPKI := spkiOfCertFile(t, filepath.Join(dir, tlsCertFile))

	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate(%q) error: %v", dir, err)
	}
	if !id.SigningPub().Equal(signingPub) {
		t.Fatalf("self-heal changed the signing pub: got %x, want seeded %x", id.SigningPub(), signingPub)
	}
	assertServingLeafCoversLoopback(t, id)
	if !bytes.Equal(parseLeaf(t, id).RawSubjectPublicKeyInfo, seededSPKI) {
		t.Fatal("the SAN heal changed the SPKI — every enrolled relay's pin just broke; the cert must be reissued over the SAME key")
	}

	reloaded, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreate(%q) error: %v", dir, err)
	}
	if !bytes.Equal(id.TLSCertPEM(), reloaded.TLSCertPEM()) {
		t.Fatal("healed TLS leaf not persisted: second load re-minted a different cert")
	}
}

// A leaf that covers loopback but NOT a newly-configured specific listen host
// is stale for that configuration and must be reissued to cover both — over
// the same key, for the same reason as the SAN heal.
func TestLoadReissuesWhenTheConfiguredHostIsNotCovered(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	firstSPKI := parseLeaf(t, first).RawSubjectPublicKeyInfo

	id, err := LoadOrCreate(dir, "192.0.2.9")
	if err != nil {
		t.Fatalf("LoadOrCreate with host: %v", err)
	}
	leaf := parseLeaf(t, id)
	if err := leaf.VerifyHostname("192.0.2.9"); err != nil {
		t.Fatalf("reissued leaf does not cover the configured host: %v", err)
	}
	assertServingLeafCoversLoopback(t, id)
	if !bytes.Equal(leaf.RawSubjectPublicKeyInfo, firstSPKI) {
		t.Fatal("covering a new host changed the SPKI — the repair must reuse the existing key")
	}
}

// spkiOfCertFile reads a PEM certificate file and returns its raw
// SubjectPublicKeyInfo — the bytes every pin in this system is over.
func spkiOfCertFile(t *testing.T, path string) []byte {
	t.Helper()
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("%s did not decode to a CERTIFICATE block", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cert.RawSubjectPublicKeyInfo
}

func assertServingLeafCoversLoopback(t *testing.T, id *Identity) {
	t.Helper()
	leaf := parseLeaf(t, id)
	for _, name := range []string{"127.0.0.1", "::1", "localhost"} {
		if err := leaf.VerifyHostname(name); err != nil {
			t.Fatalf("serving leaf does not cover %q: %v — a pack's stock verification fails at handshake", name, err)
		}
	}
}

func parseLeaf(t *testing.T, id *Identity) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(id.TLSCertPEM())
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("TLSCertPEM did not decode to a CERTIFICATE block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse serving leaf: %v", err)
	}
	return leaf
}

// seedSANLessP256TLSLeaf writes the pre-pack-runtime GenSelfSigned output: an
// ECDSA P-256 self-signed leaf with a CommonName and no SANs at all.
func seedSANLessP256TLSLeaf(t *testing.T, certPath, keyPath string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate p256 tls key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "waiveo-relay"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create p256 tls cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal p256 tls key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("write seeded cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write seeded key: %v", err)
	}
}
