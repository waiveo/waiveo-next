package deviceplane

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/rules/registry"
)

// blockingController lets a test control exactly when each Dispatch enters and
// returns, so it can observe whether two dispatches to a device overlap
// (REL-115 per-device serialization).
type blockingController struct {
	onDispatch func(entityID string)
}

func (c *blockingController) Dispatch(entityID, command string, params map[string]any) error {
	if c.onDispatch != nil {
		c.onDispatch(entityID)
	}
	return nil
}

// deviceMapSurface builds a media-player CommandSurface whose entity→device_id
// resolution is driven by entityToDevice: every entity is a media-player, and
// its owning physical device_id is the map value (an unmapped entity falls back
// to a device_id equal to its own entity_id — a lone entity that is its own
// device). This is what lets a test model one physical device exposing several
// entities (data-model/1) and assert REL-115 serializes per device_id.
func deviceMapSurface(controller DeviceController, entityToDevice map[string]string) *CommandSurface {
	return NewCommandSurface(controller, registry.FixtureRegistry{},
		func(entityID string) (deviceID, deviceClass string, ok bool) {
			if d, mapped := entityToDevice[entityID]; mapped {
				return d, "media-player", true
			}
			return entityID, "media-player", true
		})
}

// TestSameDeviceCommandsAreSerialized asserts REL-115: while one command to a
// device is outstanding in the controller, a second command to that SAME
// physical device — dispatched through a DIFFERENT entity_id of that device —
// MUST NOT enter the controller. This is data-model/1's fan-out case: device D
// exposes entities E1 and E2; concurrent commands to E1 and E2 both target the
// one physical D and MUST NOT interleave delivery to it.
func TestSameDeviceCommandsAreSerialized(t *testing.T) {
	var callNum int32
	firstIn := make(chan struct{})
	proceed := make(chan struct{})
	controller := &blockingController{onDispatch: func(string) {
		if atomic.AddInt32(&callNum, 1) == 1 {
			close(firstIn) // the first dispatch has entered
			<-proceed      // ...and stays outstanding until released
		}
	}}
	// E1 and E2 are two entities of the ONE physical device "device-D".
	surface := deviceMapSurface(controller, map[string]string{
		"E1": "device-D",
		"E2": "device-D",
	})

	cmd := func(id, entityID string) DeviceCommand {
		return DeviceCommand{ID: id, RelayID: "r",
			Body: CommandBody{EntityID: entityID, Command: "home"}}
	}

	done := make(chan struct{}, 2)
	go func() { surface.Execute(cmd("c1", "E1")); done <- struct{}{} }()
	<-firstIn // c1 (via E1) is now in-flight in the controller and parked
	go func() { surface.Execute(cmd("c2", "E2")); done <- struct{}{} }()

	// Give c2 (a DIFFERENT entity of the SAME device) a chance to (wrongly)
	// enter the controller while c1 is outstanding.
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(&callNum); n != 1 {
		t.Fatalf("a second command to a different entity of the same device entered the controller (%d dispatches) while the first was outstanding — REL-115 forbids interleaving one physical device", n)
	}

	close(proceed) // release c1; c2 may now proceed
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for serialized commands to complete")
		}
	}
	if n := atomic.LoadInt32(&callNum); n != 2 {
		t.Fatalf("both commands should have dispatched once serialized, got %d", n)
	}
}

// TestDifferentDevicesRunConcurrently asserts REL-115 serializes PER device_id,
// not globally: two commands to two DIFFERENT physical devices may be in-flight
// in the controller at the same time — even if that would be wrongly serialized
// by an entity_id key when the entities happen to share nothing.
func TestDifferentDevicesRunConcurrently(t *testing.T) {
	// Two entities, each its own distinct physical device.
	entities := []string{"entity-on-A", "entity-on-B"}
	entered := make(chan string, len(entities))
	release := make(chan struct{})
	surface := deviceMapSurface(&blockingController{onDispatch: func(entityID string) {
		entered <- entityID
		<-release // stay in-flight until released
	}}, map[string]string{
		"entity-on-A": "device-A",
		"entity-on-B": "device-B",
	})

	var wg sync.WaitGroup
	for _, e := range entities {
		wg.Add(1)
		go func(entityID string) {
			defer wg.Done()
			surface.Execute(DeviceCommand{ID: entityID, RelayID: "r",
				Body: CommandBody{EntityID: entityID, Command: "home"}})
		}(e)
	}

	// Both dispatches to DIFFERENT devices must be able to be in-flight at once.
	// If the surface serialized across devices, only one would enter the
	// controller and this would time out.
	seen := map[string]bool{}
	for range entities {
		select {
		case e := <-entered:
			seen[e] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d devices dispatched concurrently — cross-device commands must not be serialized (REL-115 is per-device_id)", len(seen), len(entities))
		}
	}
	close(release)
	wg.Wait()
	if !seen["entity-on-A"] || !seen["entity-on-B"] {
		t.Errorf("expected both devices to dispatch, saw %v", seen)
	}
}
