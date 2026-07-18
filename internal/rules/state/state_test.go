package state

import (
	"reflect"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/registry"
)

const testEntityID = "01J8Z3K4N5P6Q7R8S9T0V1W2Z2"

func TestObserveStateChange(t *testing.T) {
	reg := registry.FixtureRegistry{}
	prev := Entity{ID: testEntityID, DeviceClass: "media-player", State: "playing"}
	curr := Entity{ID: testEntityID, DeviceClass: "media-player", State: "paused"}

	obs := NewObservation(reg, prev, curr)
	if !obs.StateChanged {
		t.Error("StateChanged = false, want true (playing -> paused)")
	}
	if len(obs.ChangedAttrs) != 0 {
		t.Errorf("ChangedAttrs = %v, want empty (no attribute moved)", obs.ChangedAttrs)
	}
	if obs.Entity.State != "paused" {
		t.Errorf("Entity.State = %q, want the current snapshot's state", obs.Entity.State)
	}
}

func TestObserveSignificantAttrOnlyChange(t *testing.T) {
	reg := registry.FixtureRegistry{}
	prev := Entity{
		ID: testEntityID, DeviceClass: "media-player", State: "playing",
		Attributes: map[string]any{"volume": 20.0},
	}
	curr := Entity{
		ID: testEntityID, DeviceClass: "media-player", State: "playing",
		Attributes: map[string]any{"volume": 25.0},
	}

	obs := NewObservation(reg, prev, curr)
	if obs.StateChanged {
		t.Error("StateChanged = true, want false (state string unchanged)")
	}
	if want := []string{"volume"}; !reflect.DeepEqual(obs.ChangedAttrs, want) {
		t.Errorf("ChangedAttrs = %v, want %v (volume is significant, RUL-330)", obs.ChangedAttrs, want)
	}
}

func TestObserveCosmeticAttrChangeExcluded(t *testing.T) {
	reg := registry.FixtureRegistry{}
	prev := Entity{
		ID: testEntityID, DeviceClass: "media-player", State: "playing",
		Attributes: map[string]any{"app_version": "1.0", "volume": 20.0},
	}
	curr := Entity{
		ID: testEntityID, DeviceClass: "media-player", State: "playing",
		Attributes: map[string]any{"app_version": "1.1", "volume": 20.0},
	}

	obs := NewObservation(reg, prev, curr)
	if obs.StateChanged {
		t.Error("StateChanged = true, want false")
	}
	if len(obs.ChangedAttrs) != 0 {
		t.Errorf("ChangedAttrs = %v, want empty (app_version is cosmetic, RUL-330/REG-044)", obs.ChangedAttrs)
	}
}

func TestObserveNoChange(t *testing.T) {
	reg := registry.FixtureRegistry{}
	e := Entity{
		ID: testEntityID, DeviceClass: "media-player", State: "playing",
		Attributes: map[string]any{"volume": 20.0},
	}

	obs := NewObservation(reg, e, e)
	if obs.StateChanged {
		t.Error("StateChanged = true, want false (identical snapshot)")
	}
	if len(obs.ChangedAttrs) != 0 {
		t.Errorf("ChangedAttrs = %v, want empty (identical snapshot)", obs.ChangedAttrs)
	}
}

func TestObserveNilAttributesTreatedAsEmpty(t *testing.T) {
	reg := registry.FixtureRegistry{}
	prev := Entity{ID: testEntityID, DeviceClass: "media-player", State: "playing"}
	curr := Entity{
		ID: testEntityID, DeviceClass: "media-player", State: "playing",
		Attributes: map[string]any{"volume": 20.0},
	}

	obs := NewObservation(reg, prev, curr)
	if want := []string{"volume"}; !reflect.DeepEqual(obs.ChangedAttrs, want) {
		t.Errorf("ChangedAttrs = %v, want %v (attribute newly present counts as changed)", obs.ChangedAttrs, want)
	}
}
