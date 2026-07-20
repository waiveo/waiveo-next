package manifest

import (
	"path/filepath"
	"testing"
)

// man030File is the frozen oracle for a bare-wildcard egress manifest.
var man030File = filepath.Join("..", "..", "conformance", "corpora", "manifest-1", "MAN-030-invalid-egress-wildcard.json")

// TestValidateEgressBareWildcard drives the MAN-030 corpus case: a bare `*`
// egress entry is refused with a typed EGRESS_ENTRY_INVALID error naming it —
// the exact code, field, and message the frozen fixture pins.
func TestValidateEgressBareWildcard(t *testing.T) {
	m := loadManifest(t, man030File)
	errs := Validate(m, testHost())
	if len(errs) != 1 {
		t.Fatalf("expected exactly one error on MAN-030, got %d: %+v", len(errs), errs)
	}
	got := errs[0]
	if got.Code != "EGRESS_ENTRY_INVALID" {
		t.Errorf("code: got %q, want EGRESS_ENTRY_INVALID", got.Code)
	}
	if got.Field != "egress[0]" {
		t.Errorf("field: got %q, want egress[0]", got.Field)
	}
	const wantMsg = `"*" is a bare wildcard, which is not an allowed egress entry`
	if got.Message != wantMsg {
		t.Errorf("message: got %q, want %q", got.Message, wantMsg)
	}
}

// TestValidateEgressAccepted: a bare hostname, a wildcard-leftmost hostname, and
// an IP-literal are all valid egress entries (MAN-030) — https is the implied
// scheme (MAN-031), so no per-entry scheme/port is carried.
func TestValidateEgressAccepted(t *testing.T) {
	m := loadManifest(t, man001File)
	m.Egress = []string{"example.com", "api.weather.example", "*.example.com", "192.0.2.10"}
	errs := Validate(m, testHost())
	if hasCode(errs, "EGRESS_ENTRY_INVALID") {
		t.Fatalf("bare/wildcard-leftmost/IP-literal egress entries must be accepted, got %+v", errs)
	}
	if len(errs) != 0 {
		t.Fatalf("expected a clean validation, got %+v", errs)
	}
}

// TestValidateEgressCIDRRejected: a CIDR block is wider than a single host and
// MUST fail (MAN-030), named at its index.
func TestValidateEgressCIDRRejected(t *testing.T) {
	m := loadManifest(t, man001File)
	m.Egress = []string{"192.0.2.0/24"}
	errs := Validate(m, testHost())
	if !hasCode(errs, "EGRESS_ENTRY_INVALID") {
		t.Fatalf("a CIDR egress entry MUST be rejected, got %+v", errs)
	}
	if !hasField(errs, "egress[0]") {
		t.Fatalf("the CIDR error MUST name egress[0], got %+v", errs)
	}
}
