package manifest

import (
	"path/filepath"
	"testing"
)

// man021File is the frozen oracle for an unknown-capability manifest.
var man021File = filepath.Join("..", "..", "conformance", "corpora", "manifest-1", "MAN-021-invalid-unknown-capability.json")

// TestValidateUnknownCapability drives the MAN-021 corpus case: a capability
// absent from the host capability registry is refused with a typed
// UNKNOWN_CAPABILITY error naming it — the exact code, field, and message the
// frozen fixture pins — never a silent no-op grant.
func TestValidateUnknownCapability(t *testing.T) {
	m := loadManifest(t, man021File)
	errs := Validate(m, testHost())
	if len(errs) != 1 {
		t.Fatalf("expected exactly one error on MAN-021, got %d: %+v", len(errs), errs)
	}
	got := errs[0]
	if got.Code != "UNKNOWN_CAPABILITY" {
		t.Errorf("code: got %q, want UNKNOWN_CAPABILITY", got.Code)
	}
	if got.Field != "capabilities[0].capability" {
		t.Errorf("field: got %q, want capabilities[0].capability", got.Field)
	}
	const wantMsg = `capability "nonexistent.thing" is not in the host capability registry`
	if got.Message != wantMsg {
		t.Errorf("message: got %q, want %q", got.Message, wantMsg)
	}
}

// TestValidateEmptyCapabilitiesValid: the minimal manifest's empty capabilities
// array is valid (MAN-020) — an empty array declares a pack that needs no grants.
func TestValidateEmptyCapabilitiesValid(t *testing.T) {
	m := loadManifest(t, man001File)
	if len(m.Capabilities) != 0 {
		t.Fatalf("fixture drift: MAN-001 should declare no capabilities, got %+v", m.Capabilities)
	}
	if errs := Validate(m, testHost()); len(errs) != 0 {
		t.Fatalf("empty capabilities array must validate clean, got %+v", errs)
	}
}

// TestConsentDiffWidenedCapability: adding a capability grant is a change within
// the consent-relevant subset (MAN-023) and requires re-consent.
func TestConsentDiffWidenedCapability(t *testing.T) {
	prev := loadManifest(t, man001File)
	next := loadManifest(t, man001File)
	next.Capabilities = append(next.Capabilities, Capability{
		Capability: "device.read", Scope: "*", Reason: "msg:cap.deviceRead",
	})
	c := ConsentDiff(prev, next)
	if !c.RequiresReconsent() {
		t.Fatalf("a widened capability MUST require re-consent, got %+v", c)
	}
	if !c.Capabilities {
		t.Errorf("the change MUST be attributed to the capabilities subset, got %+v", c)
	}
}

// TestConsentDiffLocaleOnlyNoChange: a change outside the four-field consent
// subset (here displayName, a locale-catalog reference) MUST NOT require
// re-consent (MAN-023 — never a whole-manifest hash).
func TestConsentDiffLocaleOnlyNoChange(t *testing.T) {
	prev := loadManifest(t, man001File)
	next := loadManifest(t, man001File)
	next.DisplayName = "msg:pack.renamedDisplayName"
	c := ConsentDiff(prev, next)
	if c.RequiresReconsent() {
		t.Fatalf("a locale-only edit MUST NOT require re-consent, got %+v", c)
	}
}
