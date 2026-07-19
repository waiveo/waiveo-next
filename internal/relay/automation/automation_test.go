package automation

import (
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/rules/engine"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

const (
	screenEntity = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"
	screenDevice = "01J8Z3K4N5P6Q7R8S9T0V1DEVA"
)

// recordController records every physical dispatch it receives.
type recordController struct {
	mu    sync.Mutex
	calls []deviceplane.DeviceCommand
	seen  []string // "entity/command"
}

func (c *recordController) Dispatch(entityID, command string, params map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, entityID+"/"+command)
	return nil
}

func ent(id, st string) state.Entity {
	return state.Entity{ID: id, DeviceClass: "media-player", State: st}
}

// TestAutomationDrivesDeviceEndToEnd is the integrated proof: the edge-rules
// engine evaluates a real automation ("when the screen turns on, launch the dev
// channel"), fires a device_command, and this adapter routes it through the
// relay device-command surface (vocab-resolved, per-device serialized) to the
// physical device controller. One rule, one observation, one photon of control
// end to end across the two features.
func TestAutomationDrivesDeviceEndToEnd(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	controller := &recordController{}

	// The device plane: resolve the screen entity to its physical device +
	// class; the fixture registry is the command vocabulary (REL-113).
	resolve := func(entityID string) (deviceID, deviceClass string, ok bool) {
		if entityID == screenEntity {
			return screenDevice, "media-player", true
		}
		return "", "", false
	}
	surface := deviceplane.NewCommandSurface(controller, reg, resolve)

	// The engine, dispatching device_commands through the device plane.
	sink := NewCommandSink(surface, "01J8Z3K4N5P6Q7R8S9T0V1RELY")
	e := engine.New(reg, clk, sink, nil)

	rule := `{
		"id": "01J8Z3K4N5P6Q7R8S9T0V1AUTO",
		"mode": "single",
		"triggers": [{"type":"state","entity_id":"` + screenEntity + `","to":["on"]}],
		"conditions": [],
		"actions": [{"type":"device_command","entity_id":"` + screenEntity + `","command":"launch","params":{"channel":"dev"}}]
	}`
	entry, cerr := compile.Compile([]byte(rule))
	if cerr != nil {
		t.Fatalf("compile rule: %v", cerr)
	}
	if entry.ExecutionClass != "edge" {
		t.Fatalf("rule should be edge-class, got %s", entry.ExecutionClass)
	}
	parsed, err := model.ParseRule([]byte(rule))
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}
	if err := e.Load(entry, parsed); err != nil {
		t.Fatalf("engine.Load: %v", err)
	}

	// First observation establishes the durable baseline ("off") — never a
	// transition (rules/1 RUL-300), so it must NOT fire.
	if disps := e.Observe(state.NewObservation(reg, ent(screenEntity, "off"), ent(screenEntity, "off"))); len(disps) != 0 {
		t.Fatalf("first observation should not fire (RUL-300), got %+v", disps)
	}
	if len(controller.seen) != 0 {
		t.Fatalf("no command should have reached the device yet, got %v", controller.seen)
	}

	// The screen turns on: off -> on transition matches the trigger, the rule
	// runs, and the launch command flows engine -> adapter -> surface -> device.
	disps := e.Observe(state.NewObservation(reg, ent(screenEntity, "off"), ent(screenEntity, "on")))
	if len(disps) != 1 || disps[0].Disposition != "ran" {
		t.Fatalf("screen-on should run the rule once, got %+v", disps)
	}

	if len(controller.seen) != 1 || controller.seen[0] != screenEntity+"/launch" {
		t.Fatalf("the physical device should have received exactly one launch, got %v", controller.seen)
	}
	t.Logf("end-to-end: screen %s turned on -> rule fired -> device plane executed %q on device %s",
		screenEntity, controller.seen[0], screenDevice)
}

// TestAutomationUnknownCommandNeverReachesDevice: an automation firing a
// command outside the device class's vocabulary is rejected by the surface
// (REL-113) and never touches the physical device — the automation path gets
// the same guarantee as an app-dispatched command.
func TestAutomationUnknownCommandNeverReachesDevice(t *testing.T) {
	reg := registry.FixtureRegistry{}
	controller := &recordController{}
	resolve := func(entityID string) (string, string, bool) { return screenDevice, "media-player", true }
	surface := deviceplane.NewCommandSurface(controller, reg, resolve)
	sink := NewCommandSink(surface, "01J8Z3K4N5P6Q7R8S9T0V1RELY")

	// "blast" is not a media-player command (REL-113 / RUL-160).
	err := sink.Dispatch(screenEntity, "blast", nil)
	if err == nil {
		t.Fatal("an unknown command should surface an error to the engine")
	}
	if len(controller.seen) != 0 {
		t.Fatalf("an unresolved command must never touch the device (REL-113), got %v", controller.seen)
	}
}
