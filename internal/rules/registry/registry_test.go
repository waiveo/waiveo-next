package registry

import (
	"reflect"
	"testing"
)

func TestFixtureRegistrySemanticGroups(t *testing.T) {
	tests := []struct {
		name        string
		deviceClass string
		state       string
		want        []string
	}{
		{"playing is in the on-group", "media-player", "playing", []string{"on"}},
		{"paused is in the on-group", "media-player", "paused", []string{"on"}},
		{"standby is in the off-group", "media-player", "standby", []string{"off"}},
		{"unknown state belongs to no group", "media-player", "sleeping", nil},
		{"unknown device class belongs to no group", "not-a-class", "on", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FixtureRegistry{}.SemanticGroups(tt.deviceClass, tt.state)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SemanticGroups(%q, %q) = %v, want %v", tt.deviceClass, tt.state, got, tt.want)
			}
		})
	}
}

func TestFixtureRegistryAttributeType(t *testing.T) {
	reg := FixtureRegistry{}
	if got := reg.AttributeType("media-player", "volume"); got != Number {
		t.Errorf("AttributeType(media-player, volume) = %v, want Number", got)
	}
	if got := reg.AttributeType("media-player", "is_screensaver"); got != Boolean {
		t.Errorf("AttributeType(media-player, is_screensaver) = %v, want Boolean", got)
	}
	if got := reg.AttributeType("media-player", "active_app"); got != String {
		t.Errorf("AttributeType(media-player, active_app) = %v, want String", got)
	}
	if got := reg.AttributeType("media-player", "no_such_attr"); got != Unknown {
		t.Errorf("AttributeType(media-player, no_such_attr) = %v, want Unknown", got)
	}
	if got := reg.AttributeType("not-a-class", "volume"); got != Unknown {
		t.Errorf("AttributeType(not-a-class, volume) = %v, want Unknown", got)
	}
}

func TestFixtureRegistryChangeEmission(t *testing.T) {
	reg := FixtureRegistry{}
	if got := reg.ChangeEmission("media-player", "volume"); got != "significant" {
		t.Errorf("ChangeEmission(media-player, volume) = %q, want significant", got)
	}
	if got := reg.ChangeEmission("media-player", "app_version"); got != "cosmetic" {
		t.Errorf("ChangeEmission(media-player, app_version) = %q, want cosmetic", got)
	}
	if got := reg.ChangeEmission("media-player", "no_such_attr"); got != "" {
		t.Errorf("ChangeEmission(media-player, no_such_attr) = %q, want empty", got)
	}
}

func TestFixtureRegistryCommandExists(t *testing.T) {
	reg := FixtureRegistry{}
	for _, cmd := range []string{"launch", "home", "keypress", "power"} {
		if !reg.CommandExists("media-player", cmd) {
			t.Errorf("CommandExists(media-player, %q) = false, want true", cmd)
		}
	}
	if reg.CommandExists("media-player", "reboot") {
		t.Error("CommandExists(media-player, reboot) = true, want false")
	}
	if reg.CommandExists("not-a-class", "launch") {
		t.Error("CommandExists(not-a-class, launch) = true, want false")
	}
}

func TestFixtureRegistryImplementsRegistry(t *testing.T) {
	var _ Registry = FixtureRegistry{}
}
