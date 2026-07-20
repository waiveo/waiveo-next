package manifest

import (
	"path/filepath"
	"testing"
)

// man020File is the frozen oracle for a full-featured manifest exercising
// capabilities, egress, resources, a device contribution, a playable
// contribution, and automation/action contributions — the fixture Task 4's UI/
// device/playable tests extend via mutation (per the plan's Step 1).
var man020File = filepath.Join("..", "..", "conformance", "corpora", "manifest-1", "MAN-020-valid-full-featured.json")

// TestValidateFullFeaturedClean pins the full-featured fixture's own baseline:
// it validates clean against the fixture host before any of the mutation tests
// below perturb a copy of it.
func TestValidateFullFeaturedClean(t *testing.T) {
	m := loadManifest(t, man020File)
	if errs := Validate(m, testHost()); len(errs) != 0 {
		t.Fatalf("expected zero errors on the full-featured manifest, got %d: %+v", len(errs), errs)
	}
}

// TestValidatePageTypeNotInRenderer: a ui.pages[].pageType absent from
// compat.renderer is refused with a typed UNKNOWN_PAGE_TYPE error naming it
// (MAN-060), never a silent grant — even though "settings-form" may be a
// perfectly valid host page type, it is not one THIS pack declared support for.
func TestValidatePageTypeNotInRenderer(t *testing.T) {
	m := loadManifest(t, man020File)
	m.UI.Pages[0].PageType = "settings-form"
	errs := Validate(m, testHost())
	if !hasCode(errs, "UNKNOWN_PAGE_TYPE") {
		t.Fatalf("expected an UNKNOWN_PAGE_TYPE error, got %+v", errs)
	}
	if !hasField(errs, "ui.pages[0].pageType") {
		t.Fatalf("expected the error to name ui.pages[0].pageType, got %+v", errs)
	}
}

// TestValidateDuplicatePagePath: two pages sharing a path violate the MAN-060
// pack-unique-path requirement.
func TestValidateDuplicatePagePath(t *testing.T) {
	m := loadManifest(t, man020File)
	m.UI.Pages = append(m.UI.Pages, m.UI.Pages[0])
	errs := Validate(m, testHost())
	if !hasField(errs, "ui.pages[1].path") {
		t.Fatalf("expected a duplicate-path error on ui.pages[1].path, got %+v", errs)
	}
}

// TestValidateFragmentCardRequiresSizeHint: fragment: card with no sizeHint (or
// an unrecognized one) violates MAN-061.
func TestValidateFragmentCardRequiresSizeHint(t *testing.T) {
	m := loadManifest(t, man020File)
	m.UI.Pages[0].Fragment = "card"
	errs := Validate(m, testHost())
	if !hasField(errs, "ui.pages[0].sizeHint") {
		t.Fatalf("expected a sizeHint error for fragment:card with no sizeHint, got %+v", errs)
	}
}

// TestValidateFragmentCardBadSizeHint: an unrecognized sizeHint value is not one
// of small/medium/large (MAN-061).
func TestValidateFragmentCardBadSizeHint(t *testing.T) {
	m := loadManifest(t, man020File)
	m.UI.Pages[0].Fragment = "card"
	m.UI.Pages[0].SizeHint = "huge"
	errs := Validate(m, testHost())
	if !hasField(errs, "ui.pages[0].sizeHint") {
		t.Fatalf("expected a sizeHint error for an unrecognized value, got %+v", errs)
	}
}

// TestValidateFragmentCardValidSizeHint: fragment: card with a recognized
// sizeHint validates clean (MAN-061).
func TestValidateFragmentCardValidSizeHint(t *testing.T) {
	m := loadManifest(t, man020File)
	m.UI.Pages[0].Fragment = "card"
	m.UI.Pages[0].SizeHint = "small"
	errs := Validate(m, testHost())
	if hasField(errs, "ui.pages[0].sizeHint") || hasField(errs, "ui.pages[0].fragment") {
		t.Fatalf("expected fragment:card + sizeHint:small to validate clean, got %+v", errs)
	}
}

// TestValidateSurfaceEntryNotBundled: a ui.surfaces entry whose entry names no
// file in the host's bundled file set is refused (MAN-063).
func TestValidateSurfaceEntryNotBundled(t *testing.T) {
	m := loadManifest(t, man020File)
	m.UI.Surfaces = []Surface{{Name: "main", Entry: "dist/surface.js"}}
	errs := Validate(m, testHost())
	if !hasField(errs, "ui.surfaces[0].entry") {
		t.Fatalf("expected an entry error for an unbundled surface entry, got %+v", errs)
	}
}

// TestValidateSurfaceEntryBundled: a ui.surfaces entry naming a file present in
// the host's bundled file set validates clean (MAN-063).
func TestValidateSurfaceEntryBundled(t *testing.T) {
	m := loadManifest(t, man020File)
	m.UI.Surfaces = []Surface{{Name: "main", Entry: "dist/surface.js"}}
	host := testHost()
	host.BundleFiles = map[string]bool{"dist/surface.js": true}
	errs := Validate(m, host)
	if hasField(errs, "ui.surfaces[0].entry") {
		t.Fatalf("expected a bundled surface entry to validate clean, got %+v", errs)
	}
}

// TestValidateDuplicateSurfaceName: two surfaces sharing a name violate the
// MAN-063 pack-unique-identifier requirement for `name`, even when each
// surface's entry independently resolves against the host's bundled file set.
func TestValidateDuplicateSurfaceName(t *testing.T) {
	m := loadManifest(t, man020File)
	m.UI.Surfaces = []Surface{
		{Name: "main", Entry: "dist/a.js"},
		{Name: "main", Entry: "dist/b.js"},
	}
	host := testHost()
	host.BundleFiles = map[string]bool{"dist/a.js": true, "dist/b.js": true}
	errs := Validate(m, host)
	if !hasField(errs, "ui.surfaces[1].name") {
		t.Fatalf("expected a duplicate-name error on ui.surfaces[1].name, got %+v", errs)
	}
}
