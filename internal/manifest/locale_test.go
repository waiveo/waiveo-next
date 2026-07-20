package manifest

import "testing"

// TestValidateLocaleMissingEnJSON: a host bundle file set with no
// messages/en.json violates MAN-111.
func TestValidateLocaleMissingEnJSON(t *testing.T) {
	m := loadManifest(t, man020File)
	host := testHost()
	host.BundleFiles = map[string]bool{}
	errs := Validate(m, host)
	if !hasField(errs, "messages") {
		t.Fatalf("expected a messages error for a missing messages/en.json, got %+v", errs)
	}
}

// TestValidateLocalePresentEnJSON: a host bundle file set carrying
// messages/en.json validates clean (MAN-111).
func TestValidateLocalePresentEnJSON(t *testing.T) {
	m := loadManifest(t, man020File)
	host := testHost()
	host.BundleFiles = map[string]bool{"messages/en.json": true}
	errs := Validate(m, host)
	if hasField(errs, "messages") {
		t.Fatalf("expected a bundled messages/en.json to validate clean, got %+v", errs)
	}
}

// TestValidateReservedSectionsAbsentOK: absent drivers/sources/diagnostics
// sections MUST NOT themselves be a validation failure (MAN-110) — their
// content shape is reserved for a future manifest/1 minor.
func TestValidateReservedSectionsAbsentOK(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Drivers, m.Sources, m.Diagnostics = nil, nil, nil
	errs := Validate(m, testHost())
	if len(errs) != 0 {
		t.Fatalf("expected absent drivers/sources/diagnostics to validate clean, got %+v", errs)
	}
}
