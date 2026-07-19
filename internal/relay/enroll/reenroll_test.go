package enroll

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
)

func mustSerial(t *testing.T, certPEM []byte) string {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("cert did not PEM-decode: %q", certPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate: %v", err)
	}
	return cert.SerialNumber.Text(16)
}

// TestRunDrivesReEnrollOnExpiredCert is Task 5's startup-wiring assertion
// (REL-020/023): when Run finds an already-enrolled store whose certificate
// has expired relative to the evaluated time, it drives the expired-certificate
// re-enrollment path instead of returning a no-op — ending with a fresh
// certificate under the SAME relay_id and a re-anchored verification key.
func TestRunDrivesReEnrollOnExpiredCert(t *testing.T) {
	ts, feederID := newTestFeeder(t)
	store := openStore(t)

	// A first Run against a fresh store performs ordinary enrollment (serial A,
	// a year-long validity window).
	if err := run(ts.URL, store, time.Now()); err != nil {
		t.Fatalf("initial run (fresh enroll): %v", err)
	}
	before, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("Identity() after fresh enroll: ok=%v err=%v", ok, err)
	}
	oldSerial := mustSerial(t, before.CertPEM)

	// A later run evaluated well past the certificate's not_after sees it as
	// expired and drives re-enrollment.
	future := time.Now().Add(400 * 24 * time.Hour)
	if err := run(ts.URL, store, future); err != nil {
		t.Fatalf("run (expired cert → re-enroll): %v", err)
	}

	after, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("Identity() after re-enroll: ok=%v err=%v", ok, err)
	}
	if after.RelayID != before.RelayID {
		t.Errorf("relay_id changed across re-enroll: %q → %q", before.RelayID, after.RelayID)
	}
	if s := mustSerial(t, after.CertPEM); s == oldSerial {
		t.Errorf("cert serial unchanged (%q) — run did not re-enroll on the expired cert", s)
	}
	key, ok, err := store.DesiredStateVerificationKey()
	if err != nil || !ok {
		t.Fatalf("DesiredStateVerificationKey() after re-enroll: ok=%v err=%v", ok, err)
	}
	if !key.Equal(feederID.SigningPub()) {
		t.Errorf("re-anchored key = %x, want the feeder signing pub %x", []byte(key), []byte(feederID.SigningPub()))
	}
}

// TestRunNoOpOnValidCert asserts the unchanged idempotent path: an
// already-enrolled store whose certificate is still valid at the evaluated time
// is a no-op — no re-enrollment, identity untouched, no fresh claim token burned.
func TestRunNoOpOnValidCert(t *testing.T) {
	ts, _ := newTestFeeder(t)
	store := openStore(t)

	if err := run(ts.URL, store, time.Now()); err != nil {
		t.Fatalf("initial run (fresh enroll): %v", err)
	}
	before, _, err := store.Identity()
	if err != nil {
		t.Fatalf("Identity(): %v", err)
	}

	// Evaluated within the year-long validity window → still valid → no-op.
	if err := run(ts.URL, store, time.Now()); err != nil {
		t.Fatalf("run (valid cert, no-op): %v", err)
	}
	after, _, err := store.Identity()
	if err != nil {
		t.Fatalf("Identity() after second run: %v", err)
	}
	if mustSerial(t, after.CertPEM) != mustSerial(t, before.CertPEM) {
		t.Error("cert serial changed on a valid-cert run, want an untouched no-op")
	}
}

// TestCertExpired exercises the expiry predicate directly against a freshly
// issued relay certificate: valid within its window, expired past not_after.
func TestCertExpired(t *testing.T) {
	ts, _ := newTestFeeder(t)
	store := openStore(t)
	if err := run(ts.URL, store, time.Now()); err != nil {
		t.Fatalf("fresh enroll: %v", err)
	}
	id, _, err := store.Identity()
	if err != nil {
		t.Fatalf("Identity(): %v", err)
	}

	if exp, err := certExpired(id.CertPEM, time.Now()); err != nil || exp {
		t.Errorf("certExpired(now) = %v, %v; want false, nil", exp, err)
	}
	if exp, err := certExpired(id.CertPEM, time.Now().Add(400*24*time.Hour)); err != nil || !exp {
		t.Errorf("certExpired(far future) = %v, %v; want true, nil", exp, err)
	}
}

// ensure the identity import is used even if the file's helpers change.
var _ = identity.DefaultPath
