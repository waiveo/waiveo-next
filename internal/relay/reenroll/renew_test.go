package reenroll_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	feederenroll "github.com/maaxton/waiveo-next/internal/feeder/enroll"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
)

// mintCertPEM self-signs a throwaway certificate with the given validity
// window — the fixture ExpiresWithin's window math is exercised against.
func mintCertPEM(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "expires-within-fixture"},
		NotBefore:    notAfter.Add(-24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestExpiresWithin pins the proactive-renewal predicate's window math: due
// exactly when now >= NotAfter - window (so window=0 degenerates to the
// plain expired check), and a corrupt PEM is an error, never silently
// "not due".
func TestExpiresWithin(t *testing.T) {
	notAfter := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	certPEM := mintCertPEM(t, notAfter)
	const window = 30 * 24 * time.Hour

	cases := []struct {
		name   string
		now    time.Time
		window time.Duration
		want   bool
	}{
		{"well outside the window", notAfter.Add(-31 * 24 * time.Hour), window, false},
		{"exactly at NotAfter-window", notAfter.Add(-window), window, true},
		{"inside the window", notAfter.Add(-29 * 24 * time.Hour), window, true},
		{"already expired", notAfter.Add(time.Hour), window, true},
		{"window zero, not yet expired", notAfter.Add(-time.Second), 0, false},
		{"window zero, exactly at NotAfter", notAfter, 0, true},
	}
	for _, tc := range cases {
		got, err := reenroll.ExpiresWithin(certPEM, tc.now, tc.window)
		if err != nil {
			t.Errorf("%s: ExpiresWithin: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: ExpiresWithin = %v, want %v", tc.name, got, tc.want)
		}
	}

	if _, err := reenroll.ExpiresWithin([]byte("not a certificate"), notAfter, window); err == nil {
		t.Error("ExpiresWithin on a corrupt PEM = nil error, want an error (a corrupt store must never read as \"not due\")")
	}
}

// newTestFeederWithServer is newTestFeeder, additionally returning the
// enrollment server itself so a test can read its issuance record
// (MostRecentSerial / IsRevoked) — the app-peer-side oracle for what a
// renewal did and did not do to the old certificate.
func newTestFeederWithServer(t *testing.T) (*httptest.Server, *feederenroll.Server, *signing.Identity) {
	t.Helper()
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	srv, err := feederenroll.NewServer(id)
	if err != nil {
		t.Fatalf("feederenroll.NewServer: %v", err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	return ts, srv, id
}

// TestRenewReusesKeypairIssuesFreshCertSameRelayID is the proactive
// (ahead-of-expiry) renewal oracle (REL-014/015): Renew ends with a FRESH
// certificate (new serial, advanced feeder issuance record) under the SAME
// relay_id and — unlike the expired-cert ReEnroll, which deliberately
// rotates — over the SAME keypair, so the SPKI paired players pinned via
// fingerprint_commitment (REL-126/PLY-052) and the lease-signing key both
// survive the renewal. The superseded certificate is NOT revoked (REL-015's
// cutover sentence: superseded, never thereby revoked).
func TestRenewReusesKeypairIssuesFreshCertSameRelayID(t *testing.T) {
	ts, srv, feederID := newTestFeederWithServer(t)
	store := openStore(t)

	if err := relayenroll.Run(ts.URL, store); err != nil {
		t.Fatalf("relayenroll.Run: %v", err)
	}
	before, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("Identity() before renew: ok=%v err=%v", ok, err)
	}
	oldSerial := certSerial(t, before.CertPEM)

	if err := reenroll.Renew(ts.URL, store); err != nil {
		t.Fatalf("Renew: %v", err)
	}

	after, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("Identity() after renew: ok=%v err=%v", ok, err)
	}

	// Same relay_id (REL-014).
	if after.RelayID != before.RelayID {
		t.Errorf("relay_id changed: before %q, after %q — renewal must keep the same relay_id", before.RelayID, after.RelayID)
	}
	// A fresh certificate: new serial, and the feeder's issuance record head
	// advanced to it.
	newSerial := certSerial(t, after.CertPEM)
	if newSerial == "" || newSerial == oldSerial {
		t.Errorf("cert serial after renew = %q, want a fresh serial distinct from %q", newSerial, oldSerial)
	}
	if head, _, ok := srv.MostRecentSerial(after.RelayID); !ok || head != newSerial {
		t.Errorf("feeder MostRecentSerial = %q ok=%v, want the renewed serial %q", head, ok, newSerial)
	}
	// The SAME keypair (proactive renewal never rotates): persisted key
	// unchanged and the renewed certificate certifies it, so the SPKI —
	// the exact bytes players committed to — is byte-identical.
	if !before.PrivateKey.Equal(after.PrivateKey) {
		t.Error("private key changed after Renew, want the enrollment keypair reused (player-pinned SPKI must survive renewal)")
	}
	oldSPKI := certSPKI(t, before.CertPEM)
	newSPKI := certSPKI(t, after.CertPEM)
	if string(oldSPKI) != string(newSPKI) {
		t.Error("renewed certificate's SubjectPublicKeyInfo differs from the enrollment certificate's — the pinned commitment would break")
	}
	// Superseded, not revoked (REL-015/016): renewal advances the record; it
	// never revokes the old serial.
	if srv.IsRevoked(after.RelayID, oldSerial) {
		t.Error("old serial is revoked after renewal — renewal must supersede, never revoke (REL-015)")
	}
	// Re-anchored desired_state_verification_key present and correct.
	key, ok, err := store.DesiredStateVerificationKey()
	if err != nil || !ok {
		t.Fatalf("DesiredStateVerificationKey() after renew: ok=%v err=%v", ok, err)
	}
	if !key.Equal(feederID.SigningPub()) {
		t.Errorf("verification key = %x, want the feeder signing pub %x", []byte(key), []byte(feederID.SigningPub()))
	}
}

// TestRenewFailureLeavesIdentityUntouched: any renewal failure leaves the
// persisted identity exactly as it was (never-wipe, REL-142) — the current
// leaf may still be perfectly valid, and the renewal window gives the retry
// cadence thousands of later opportunities.
func TestRenewFailureLeavesIdentityUntouched(t *testing.T) {
	ts, _, _ := newTestFeederWithServer(t)
	store := openStore(t)

	if err := relayenroll.Run(ts.URL, store); err != nil {
		t.Fatalf("relayenroll.Run: %v", err)
	}
	before, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("Identity() before renew: ok=%v err=%v", ok, err)
	}

	// An unreachable feeder: the exchange fails before anything is issued.
	if err := reenroll.Renew("https://127.0.0.1:1", store); err == nil {
		t.Fatal("Renew against an unreachable feeder succeeded, want an error")
	}

	after, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("Identity() after failed renew: ok=%v err=%v", ok, err)
	}
	if certSerial(t, after.CertPEM) != certSerial(t, before.CertPEM) {
		t.Error("persisted certificate changed after a failed renew, want it untouched")
	}
	if !before.PrivateKey.Equal(after.PrivateKey) {
		t.Error("persisted private key changed after a failed renew, want it untouched")
	}
}

// certSPKI returns the DER SubjectPublicKeyInfo certPEM certifies — the
// exact bytes tlsboot's fingerprint commitment is computed over.
func certSPKI(t *testing.T, certPEM []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("cert did not PEM-decode: %q", certPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate: %v", err)
	}
	return cert.RawSubjectPublicKeyInfo
}
