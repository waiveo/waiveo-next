package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// man001File is the frozen oracle for a minimal, fully-well-formed manifest.
var man001File = filepath.Join("..", "..", "conformance", "corpora", "manifest-1", "MAN-001-valid-minimal.json")

// loadManifest reads a corpus case's `input` block into a PackManifest, freshly
// unmarshaled on every call so a caller may mutate its own copy freely.
func loadManifest(t *testing.T, file string) PackManifest {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read corpus %s: %v", file, err)
	}
	var c struct {
		Input PackManifest `json:"input"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("unmarshal corpus %s: %v", file, err)
	}
	return c.Input
}

// testHost is the small fixed fixture registry the manifest corpus validates
// against — the host's recognized capability/feature/page-type/device-class/
// provider sets plus a memory floor (conformance notes: a documented fixture
// registry, not the real independently-evolving registries). BundleFiles
// carries messages/en.json by default (MAN-111) since every corpus fixture's
// pack is presumed to bundle its default-locale catalog; a test exercising the
// MAN-111 failure path overrides BundleFiles to omit it. Capabilities
// includes "notifications.send", a host-recognized capability no fixture
// manifest itself declares — used to pin that an actions[].capabilityScope
// MUST resolve against the manifest's own declared capabilities, not merely
// the host registry (MAN-100).
func testHost() HostRegistries {
	return HostRegistries{
		Capabilities:   map[string]bool{"device.read": true, "egress.http": true, "notifications.send": true},
		Features:       map[string]bool{},
		PageTypes:      map[string]bool{"dashboard": true, "list-detail": true, "settings-form": true},
		DeviceClasses:  map[string]bool{"media-player": true},
		Providers:      map[string]bool{"weather-api/api-key": true},
		BundleFiles:    map[string]bool{"messages/en.json": true},
		MemoryFloorMiB: 32,
	}
}

func hasField(errs []Error, field string) bool {
	for _, e := range errs {
		if e.Field == field {
			return true
		}
	}
	return false
}

func hasCode(errs []Error, code string) bool {
	for _, e := range errs {
		if e.Code == code {
			return true
		}
	}
	return false
}

// TestValidateMinimalClean is the happy path: the frozen minimal manifest
// validates against the fixture host with zero errors (MAN-001/002/003/010).
func TestValidateMinimalClean(t *testing.T) {
	m := loadManifest(t, man001File)
	if errs := Validate(m, testHost()); len(errs) != 0 {
		t.Fatalf("expected zero errors on the minimal manifest, got %d: %+v", len(errs), errs)
	}
}

// TestValidateBadID: an uppercase publisher/name is not a MAN-001 id.
func TestValidateBadID(t *testing.T) {
	m := loadManifest(t, man001File)
	m.ID = "Acme/Weather"
	errs := Validate(m, testHost())
	if !hasField(errs, "id") {
		t.Fatalf("expected an id error for %q, got %+v", m.ID, errs)
	}
}

// TestValidateBadVersion: a two-component version is not MAN-002.
func TestValidateBadVersion(t *testing.T) {
	m := loadManifest(t, man001File)
	m.Version = "1.2"
	errs := Validate(m, testHost())
	if !hasField(errs, "version") {
		t.Fatalf("expected a version error for %q, got %+v", m.Version, errs)
	}
}

// TestValidateRawDisplayName: a raw (non-msg:) displayName violates MAN-003.
func TestValidateRawDisplayName(t *testing.T) {
	m := loadManifest(t, man001File)
	m.DisplayName = "Weather"
	errs := Validate(m, testHost())
	if !hasField(errs, "displayName") {
		t.Fatalf("expected a displayName error for %q, got %+v", m.DisplayName, errs)
	}
}

// TestValidateUnknownRenderer: a compat.renderer page type absent from the host
// page-type registry is refused with a typed UNKNOWN_PAGE_TYPE error naming it
// (MAN-010), never a silent grant.
func TestValidateUnknownRenderer(t *testing.T) {
	m := loadManifest(t, man001File)
	m.Compat.Renderer = []string{"nope"}
	errs := Validate(m, testHost())
	if !hasCode(errs, "UNKNOWN_PAGE_TYPE") {
		t.Fatalf("expected an UNKNOWN_PAGE_TYPE error, got %+v", errs)
	}
	if !hasField(errs, "compat.renderer[0]") {
		t.Fatalf("expected the error to name compat.renderer[0], got %+v", errs)
	}
}

// TestValidateUnknownFeature: an unrecognized compat.features flag is refused
// with a typed UNKNOWN_FEATURE_FLAG error naming it (MAN-012).
func TestValidateUnknownFeature(t *testing.T) {
	m := loadManifest(t, man001File)
	m.Compat.Features = []string{"experimental.thing"}
	errs := Validate(m, testHost())
	if !hasCode(errs, "UNKNOWN_FEATURE_FLAG") {
		t.Fatalf("expected an UNKNOWN_FEATURE_FLAG error, got %+v", errs)
	}
	if !hasField(errs, "compat.features[0]") {
		t.Fatalf("expected the error to name compat.features[0], got %+v", errs)
	}
}

// TestValidateRelayWithoutDevices: compat.relay declared with no devices block
// violates MAN-011 (a pack with no devices MUST omit compat.relay).
func TestValidateRelayWithoutDevices(t *testing.T) {
	m := loadManifest(t, man001File)
	m.Compat.Relay = ">=1.0 <2.0"
	errs := Validate(m, testHost())
	if !hasField(errs, "compat.relay") {
		t.Fatalf("expected a compat.relay error (MAN-011), got %+v", errs)
	}
}

// TestErrorFieldLocationWireKey pins Error's field-location to the wire key the
// manifest/1 ValidationResult shape and every corpus fixture use — "field", not
// "path" (contracts/manifest-1.md's canonical ValidationResult example and
// MAN-021/MAN-030's expected.errors[].field). A wrong tag silently drops the
// offending-field detail from any consumer that JSON-marshals an Error per the
// documented shape, and breaks a corpus-diff driver comparing against
// expected.errors[].field. The round-trip references only the Error type (not
// its Go field name), so it exercises the JSON tag directly.
func TestErrorFieldLocationWireKey(t *testing.T) {
	// The offending-field value is taken verbatim from the MAN-021 fixture's
	// expected.errors[0].field (conformance/corpora/manifest-1).
	const wire = `{"code":"UNKNOWN_CAPABILITY","field":"capabilities[0].capability","message":"m"}`
	var e Error
	if err := json.Unmarshal([]byte(wire), &e); err != nil {
		t.Fatalf("unmarshal wire Error: %v", err)
	}
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal Error: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("re-unmarshal Error: %v", err)
	}
	if m["field"] != "capabilities[0].capability" {
		t.Fatalf("Error must round-trip the field-location under wire key %q; got %s", "field", got)
	}
	if _, ok := m["path"]; ok {
		t.Fatalf("Error must not emit a %q field-location key; got %s", "path", got)
	}
}
