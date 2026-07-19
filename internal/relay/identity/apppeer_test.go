package identity

import (
	"bytes"
	"path/filepath"
	"testing"
)

// TestAppPeerTrustPinAbsentBeforeEnrollment: a fresh store, before any app-peer
// trust pin (REL-011) has been learned, reports "not present" rather than an
// empty pin or an error.
func TestAppPeerTrustPinAbsentBeforeEnrollment(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	_, ok, err := store.AppPeerTrustPin()
	if err != nil {
		t.Fatalf("AppPeerTrustPin(): %v", err)
	}
	if ok {
		t.Fatal("AppPeerTrustPin() ok = true on a fresh store, want false")
	}
}

// TestAppPeerTrustPinRoundTrip: SetAppPeerTrustPin/AppPeerTrustPin round-trips
// the enrollment-anchored app-peer leaf SubjectPublicKeyInfo (REL-011 trust_pin,
// the REL-137 pin material) byte-for-byte, and a later Set replaces it (a
// re-anchored key at re-enrollment, REL-017).
func TestAppPeerTrustPinRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	spki := []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00, 0xde, 0xad, 0xbe, 0xef}
	if err := store.SetAppPeerTrustPin(spki); err != nil {
		t.Fatalf("SetAppPeerTrustPin: %v", err)
	}

	got, ok, err := store.AppPeerTrustPin()
	if err != nil {
		t.Fatalf("AppPeerTrustPin(): %v", err)
	}
	if !ok {
		t.Fatal("AppPeerTrustPin() ok = false after Set, want true")
	}
	if !bytes.Equal(got, spki) {
		t.Errorf("pin round-trip = %x, want %x", got, spki)
	}

	// Re-anchor at re-enrollment (REL-017): a subsequent Set replaces the pin.
	rotated := []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00, 0xca, 0xfe, 0xba, 0xbe}
	if err := store.SetAppPeerTrustPin(rotated); err != nil {
		t.Fatalf("SetAppPeerTrustPin (rotate): %v", err)
	}
	got, ok, err = store.AppPeerTrustPin()
	if err != nil {
		t.Fatalf("AppPeerTrustPin() after rotate: %v", err)
	}
	if !ok || !bytes.Equal(got, rotated) {
		t.Errorf("rotated pin = (%x,%v), want %x", got, ok, rotated)
	}
}
