package reenroll

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

// fakeRecord is a hand-built IssuanceRecord for exercising Eligible's pure
// serial-record logic (REL-021/022) without a live feeder: it reports one
// most-recently-issued serial per relay_id and a per-serial revoked set.
type fakeRecord struct {
	present    bool
	mostRecent string
	pub        ed25519.PublicKey
	revoked    map[string]bool
}

func (f fakeRecord) MostRecentSerial(relayID string) (string, ed25519.PublicKey, bool) {
	if !f.present {
		return "", nil, false
	}
	return f.mostRecent, f.pub, true
}

func (f fakeRecord) IsRevoked(relayID, serial string) bool {
	return f.revoked[serial]
}

func testPub(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub
}

// TestEligibleMostRecentSerial: the presented serial is the
// most-recently-issued on record for that relay_id and is not revoked — the
// one path REL-021 lets through (Eligible returns nil).
func TestEligibleMostRecentSerial(t *testing.T) {
	rec := fakeRecord{present: true, mostRecent: "7a1b2c3d", pub: testPub(t)}
	if err := Eligible(rec, "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "7a1b2c3d"); err != nil {
		t.Fatalf("Eligible = %v, want nil (most-recently-issued, not revoked)", err)
	}
}

// TestEligibleSupersededRefused wires REL-022's corpus case
// (REL-022-invalid-re-enroll-superseded-cert): serial 5c6d7e8f is presented
// but 7a1b2c3d is the most-recently-issued for that relay_id, so the
// presented cert is superseded and MUST be refused CERT_EXPIRED_INELIGIBLE
// with the corpus's exact operator-facing message.
func TestEligibleSupersededRefused(t *testing.T) {
	rec := fakeRecord{present: true, mostRecent: "7a1b2c3d", pub: testPub(t)}

	err := Eligible(rec, "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "5c6d7e8f")
	if err == nil {
		t.Fatal("Eligible = nil for a superseded serial, want CERT_EXPIRED_INELIGIBLE (REL-022)")
	}
	if err.Code != CodeExpiredIneligible {
		t.Errorf("Code = %q, want %q", err.Code, CodeExpiredIneligible)
	}
	const wantMsg = "presented certificate serial 5c6d7e8f is superseded by 7a1b2c3d for this relay_id"
	if err.Message != wantMsg {
		t.Errorf("Message = %q, want the REL-022 corpus message %q", err.Message, wantMsg)
	}
}

// TestEligibleRevokedRefused: the presented serial IS the most-recently-issued
// on record, but it was revoked — REL-022 refuses a revoked cert through this
// path exactly like a superseded one (CERT_EXPIRED_INELIGIBLE).
func TestEligibleRevokedRefused(t *testing.T) {
	rec := fakeRecord{
		present:    true,
		mostRecent: "7a1b2c3d",
		pub:        testPub(t),
		revoked:    map[string]bool{"7a1b2c3d": true},
	}
	err := Eligible(rec, "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "7a1b2c3d")
	if err == nil {
		t.Fatal("Eligible = nil for a revoked most-recent serial, want CERT_EXPIRED_INELIGIBLE (REL-022)")
	}
	if err.Code != CodeExpiredIneligible {
		t.Errorf("Code = %q, want %q", err.Code, CodeExpiredIneligible)
	}
}

// TestEligibleUnknownRelayRefused: a relay_id with no issuance on record is
// ineligible — eligibility is decided from the record alone (REL-021), and an
// absent record is a refusal, not a grant.
func TestEligibleUnknownRelayRefused(t *testing.T) {
	rec := fakeRecord{present: false}
	err := Eligible(rec, "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "7a1b2c3d")
	if err == nil {
		t.Fatal("Eligible = nil for an unknown relay_id, want CERT_EXPIRED_INELIGIBLE")
	}
	if err.Code != CodeExpiredIneligible {
		t.Errorf("Code = %q, want %q", err.Code, CodeExpiredIneligible)
	}
}

// TestReEnrollErrorImplementsError: ReEnrollError is a Go error whose string
// surfaces both code and message (Task 4 maps it onto an apihttp Problem).
func TestReEnrollErrorImplementsError(t *testing.T) {
	var err error = &ReEnrollError{Code: CodeExpiredIneligible, Message: "x"}
	if err.Error() == "" {
		t.Error("ReEnrollError.Error() is empty")
	}
}
