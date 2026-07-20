package registry

import (
	"reflect"
	"testing"

	"github.com/maaxton/waiveo-next/internal/deviceclass"
)

// TestFromDeviceClassReproducesFixture asserts the canonical-registry-backed
// adapter answers every method+arg the hand-maintained FixtureRegistry answered
// with the identical value: the media-player semantic groups, all nine
// attribute types (the six REG-064-pinned ones plus the three corpus-only
// overlay attributes), their change-emission classes, and all four commands.
// This is the regression oracle for retiring the hand-maintained fixture — the
// adapter is the fixture's replacement, so it must be behaviorally identical.
func TestFromDeviceClassReproducesFixture(t *testing.T) {
	adapter := FromDeviceClass(deviceclass.Builtin(), corpusOverlay)

	t.Run("SemanticGroups", func(t *testing.T) {
		cases := []struct {
			deviceClass, state string
			want               []string
		}{
			{"media-player", "on", []string{"on"}},
			{"media-player", "playing", []string{"on"}},
			{"media-player", "idle", []string{"on"}},
			{"media-player", "paused", []string{"on"}},
			{"media-player", "buffering", []string{"on"}},
			{"media-player", "off", []string{"off"}},
			{"media-player", "standby", []string{"off"}},
			{"media-player", "unavailable", []string{"off"}},
			{"media-player", "sleeping", nil},
			{"not-a-class", "on", nil},
		}
		for _, c := range cases {
			got := adapter.SemanticGroups(c.deviceClass, c.state)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("SemanticGroups(%q, %q) = %v, want %v", c.deviceClass, c.state, got, c.want)
			}
		}
	})

	t.Run("AttributeType", func(t *testing.T) {
		cases := []struct {
			deviceClass, attr string
			want              ValueType
		}{
			// REG-064-pinned attributes (built-in, base wins).
			{"media-player", "active_app", String},
			{"media-player", "active_app_id", String},
			{"media-player", "app_type", String}, // enum -> String (RUL-263)
			{"media-player", "power_mode", String},
			{"media-player", "is_screensaver", Boolean},
			{"media-player", "app_version", String},
			// Corpus-only overlay attributes (not REG-064-pinned).
			{"media-player", "volume", Number},
			{"media-player", "media_title", String},
			{"media-player", "powered_on", Boolean},
			// Absent attribute / unknown class degrade to Unknown; the overlay
			// is class-scoped and must not leak volume onto another class.
			{"media-player", "no_such_attr", Unknown},
			{"not-a-class", "volume", Unknown},
			{"not-a-class", "active_app", Unknown},
		}
		for _, c := range cases {
			if got := adapter.AttributeType(c.deviceClass, c.attr); got != c.want {
				t.Errorf("AttributeType(%q, %q) = %v, want %v", c.deviceClass, c.attr, got, c.want)
			}
		}
	})

	t.Run("ChangeEmission", func(t *testing.T) {
		cases := []struct {
			deviceClass, attr, want string
		}{
			{"media-player", "active_app", "significant"},
			{"media-player", "active_app_id", "significant"},
			{"media-player", "app_type", "significant"},
			{"media-player", "power_mode", "significant"},
			{"media-player", "is_screensaver", "significant"},
			{"media-player", "app_version", "cosmetic"},
			{"media-player", "volume", "significant"},
			{"media-player", "media_title", "cosmetic"},
			{"media-player", "powered_on", "significant"},
			{"media-player", "no_such_attr", ""},
			{"not-a-class", "volume", ""},
		}
		for _, c := range cases {
			if got := adapter.ChangeEmission(c.deviceClass, c.attr); got != c.want {
				t.Errorf("ChangeEmission(%q, %q) = %q, want %q", c.deviceClass, c.attr, got, c.want)
			}
		}
	})

	t.Run("CommandExists", func(t *testing.T) {
		for _, cmd := range []string{"launch", "home", "keypress", "power"} {
			if !adapter.CommandExists("media-player", cmd) {
				t.Errorf("CommandExists(media-player, %q) = false, want true", cmd)
			}
		}
		if adapter.CommandExists("media-player", "reboot") {
			t.Error("CommandExists(media-player, reboot) = true, want false")
		}
		if adapter.CommandExists("not-a-class", "launch") {
			t.Error("CommandExists(not-a-class, launch) = true, want false")
		}
	})
}

// TestFromDeviceClassOverlayNeverShadows asserts an overlay entry can only ADD a
// new attribute a base class lacks, never override a REG-064-pinned one. Even an
// overlay that names active_app (a built-in String/significant attribute) with a
// conflicting type/emission is ignored for that attribute: the canonical
// built-in value always wins (REG-064).
func TestFromDeviceClassOverlayNeverShadows(t *testing.T) {
	shadowing := NewOverlay(map[string]map[string]OverlayAttr{
		"media-player": {
			// Attempt to shadow a built-in attribute — must be ignored.
			"active_app": {Type: Number, ChangeEmission: "cosmetic"},
			// A genuinely new attribute the base lacks — must be added.
			"volume": {Type: Number, ChangeEmission: "significant"},
		},
	})
	adapter := FromDeviceClass(deviceclass.Builtin(), shadowing)

	if got := adapter.AttributeType("media-player", "active_app"); got != String {
		t.Errorf("AttributeType(media-player, active_app) = %v, want String (base wins, overlay must not shadow)", got)
	}
	if got := adapter.ChangeEmission("media-player", "active_app"); got != "significant" {
		t.Errorf("ChangeEmission(media-player, active_app) = %q, want significant (base wins, overlay must not shadow)", got)
	}
	if got := adapter.AttributeType("media-player", "volume"); got != Number {
		t.Errorf("AttributeType(media-player, volume) = %v, want Number (overlay adds a new attribute)", got)
	}
}

// TestFromDeviceClassEmptyOverlaySafe asserts the zero-value Overlay is usable —
// an adapter built with no overlay resolves only the canonical built-in content
// and degrades absent attributes to Unknown without panicking on the nil map.
func TestFromDeviceClassEmptyOverlaySafe(t *testing.T) {
	adapter := FromDeviceClass(deviceclass.Builtin(), Overlay{})
	if got := adapter.AttributeType("media-player", "active_app"); got != String {
		t.Errorf("AttributeType(media-player, active_app) = %v, want String", got)
	}
	if got := adapter.AttributeType("media-player", "volume"); got != Unknown {
		t.Errorf("AttributeType(media-player, volume) = %v, want Unknown (no overlay)", got)
	}
}
