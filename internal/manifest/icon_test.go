package manifest

import "testing"

// The optional display icon is a lucide glyph name the console resolves to a
// pack's nav-group icon. Install validates only its SHAPE (a lowercase
// kebab-case identifier), so a malformed name — markup, a path, whitespace —
// can never reach the console's icon lookup. Membership in the host-allowed set
// and its graceful default are a render concern (an unrecognized-but-well-formed
// name degrades to the default extension glyph, never a broken icon or a failed
// install), so a well-formed unknown name is NOT refused here.

func TestValidateIcon(t *testing.T) {
	cases := []struct {
		name    string
		icon    string
		wantErr bool
	}{
		{"absent is fine", "", false},
		{"an allowed glyph name", "utensils", false},
		{"a hyphenated glyph name", "layout-dashboard", false},
		// Well-formed but not a glyph this host carries: tolerated (degrades to the
		// default at render), never a refused install.
		{"a well-formed unknown name", "totally-not-a-real-glyph", false},
		// Malformed names the shape check must refuse.
		{"uppercase", "Utensils", true},
		{"a traversal", "../../design", true},
		{"a space", "menu board", true},
		{"markup", "<svg>", true},
		{"leading hyphen", "-menu", true},
		{"leading digit", "1menu", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := loadManifest(t, man001File)
			m.Icon = tc.icon
			errs := Validate(m, testHost())
			got := hasField(errs, "icon")
			if got != tc.wantErr {
				t.Fatalf("icon %q: hasField(icon)=%v, want %v (errs=%+v)", tc.icon, got, tc.wantErr, errs)
			}
		})
	}
}

func TestIsIconName(t *testing.T) {
	for _, ok := range []string{"utensils", "menu", "layout-dashboard", "a", "a1-b2"} {
		if !IsIconName(ok) {
			t.Errorf("IsIconName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "Utensils", "menu board", "../x", "menu.", "-menu", "menu-", "1menu", "<svg>"} {
		if IsIconName(bad) {
			t.Errorf("IsIconName(%q) = true, want false", bad)
		}
	}
}
