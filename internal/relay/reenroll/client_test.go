package reenroll_test

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	feederenroll "github.com/maaxton/waiveo-next/internal/feeder/enroll"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
)

// newTestFeeder builds a real feeder enrollment + re-enrollment server
// (internal/feeder/enroll) on an httptest TLS listener — the exact loopback
// shape reenroll.ReEnroll drives against in production.
func newTestFeeder(t *testing.T) (*httptest.Server, *signing.Identity) {
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
	return ts, id
}

func openStore(t *testing.T) *identity.Store {
	t.Helper()
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func certSerial(t *testing.T, certPEM []byte) string {
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

// TestReEnrollRecoversIdentitySameRelayIDReanchorsKey is Task 5's core oracle
// (REL-014/017/020/024, corpus REL-020): a relay holding an unrevoked
// most-recently-issued certificate re-enrolls by proving possession of that
// certificate's OWN private key over a fresh challenge nonce bound to a fresh
// CSR. It ends holding a fresh certificate under the SAME relay_id, a fresh
// private key, and a re-anchored desired_state_verification_key.
func TestReEnrollRecoversIdentitySameRelayIDReanchorsKey(t *testing.T) {
	ts, feederID := newTestFeeder(t)
	store := openStore(t)

	// Bootstrap the store to a real enrollment: relay_id + cert (serial A on the
	// feeder record) + the private key it will later prove possession of.
	if err := relayenroll.Run(ts.URL, store); err != nil {
		t.Fatalf("relayenroll.Run: %v", err)
	}
	before, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("Identity() before re-enroll: ok=%v err=%v", ok, err)
	}
	oldSerial := certSerial(t, before.CertPEM)

	if err := reenroll.ReEnroll(ts.URL, store); err != nil {
		t.Fatalf("ReEnroll: %v", err)
	}

	after, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("Identity() after re-enroll: ok=%v err=%v", ok, err)
	}

	// Same relay_id (REL-014).
	if after.RelayID != before.RelayID {
		t.Errorf("relay_id changed: before %q, after %q — re-enroll must keep the same relay_id", before.RelayID, after.RelayID)
	}
	// A fresh certificate (new serial), issued over the fresh keypair.
	newSerial := certSerial(t, after.CertPEM)
	if newSerial == "" || newSerial == oldSerial {
		t.Errorf("cert serial after re-enroll = %q, want a fresh serial distinct from %q", newSerial, oldSerial)
	}
	// A fresh private key (never the expired one), matching the new cert.
	if before.PrivateKey.Equal(after.PrivateKey) {
		t.Error("private key unchanged after re-enroll, want a freshly generated key")
	}
	block, _ := pem.Decode(after.CertPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse re-issued cert: %v", err)
	}
	certPub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok || !certPub.Equal(after.PrivateKey.Public()) {
		t.Error("re-issued cert public key is not the persisted fresh private key's own public key")
	}

	// Re-anchored desired_state_verification_key == the feeder's own signing pub (REL-017/024).
	key, ok, err := store.DesiredStateVerificationKey()
	if err != nil || !ok {
		t.Fatalf("DesiredStateVerificationKey() after re-enroll: ok=%v err=%v", ok, err)
	}
	if !key.Equal(feederID.SigningPub()) {
		t.Errorf("re-anchored key = %x, want the feeder signing pub %x", []byte(key), []byte(feederID.SigningPub()))
	}
}

// TestReEnrollSupersededCertRefused asserts REL-022: a relay presenting a
// certificate that is no longer the most-recently-issued for its relay_id (a
// newer one has since been issued) is refused CERT_EXPIRED_INELIGIBLE, and its
// persisted identity is left untouched.
func TestReEnrollSupersededCertRefused(t *testing.T) {
	ts, _ := newTestFeeder(t)
	store := openStore(t)

	if err := relayenroll.Run(ts.URL, store); err != nil {
		t.Fatalf("relayenroll.Run: %v", err)
	}
	// Capture serial-A identity, then advance the feeder record by completing one
	// re-enroll (record head becomes serial B).
	certA, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("Identity() (serial A): ok=%v err=%v", ok, err)
	}
	if err := reenroll.ReEnroll(ts.URL, store); err != nil {
		t.Fatalf("first ReEnroll (advance to serial B): %v", err)
	}

	// Roll the store back to the now-superseded serial-A identity and re-present it.
	if err := store.SetIdentity(certA.RelayID, certA.CertPEM, certA.PrivateKey); err != nil {
		t.Fatalf("roll store back to serial A: %v", err)
	}

	err = reenroll.ReEnroll(ts.URL, store)
	if err == nil {
		t.Fatal("ReEnroll with a superseded certificate succeeded, want CERT_EXPIRED_INELIGIBLE")
	}
	var reErr *reenroll.ReEnrollError
	if !errors.As(err, &reErr) {
		t.Fatalf("ReEnroll error = %v (%T), want a *reenroll.ReEnrollError", err, err)
	}
	if reErr.Code != reenroll.CodeExpiredIneligible {
		t.Errorf("refusal code = %q, want %q", reErr.Code, reenroll.CodeExpiredIneligible)
	}

	// The store still holds the serial-A identity it was rolled back to — a
	// refusal issues nothing and persists nothing.
	after, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("Identity() after refusal: ok=%v err=%v", ok, err)
	}
	if certSerial(t, after.CertPEM) != certSerial(t, certA.CertPEM) {
		t.Error("persisted certificate changed after a refused re-enroll, want it untouched")
	}
}
