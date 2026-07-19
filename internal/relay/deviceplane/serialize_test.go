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

func mediaPlayerSurface(controller DeviceController) *CommandSurface {
	return NewCommandSurface(controller, registry.FixtureRegistry{},
		func(string) (string, bool) { return "media-player", true })
}

// TestSameDeviceCommandsAreSerialized asserts REL-115: while one command to a
// device is outstanding in the controller, a second command to that SAME device
// MUST NOT enter the controller — it queues behind the first rather than
// interleaving.
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
	surface := mediaPlayerSurface(controller)

	cmd := func(id string) DeviceCommand {
		return DeviceCommand{ID: id, RelayID: "r",
			Body: CommandBody{EntityID: "same-device", Command: "home"}}
	}

	done := make(chan struct{}, 2)
	go func() { surface.Execute(cmd("c1")); done <- struct{}{} }()
	<-firstIn // c1 is now in-flight in the controller and parked
	go func() { surface.Execute(cmd("c2")); done <- struct{}{} }()

	// Give c2 a chance to (wrongly) enter the controller while c1 is outstanding.
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(&callNum); n != 1 {
		t.Fatalf("a second command entered the controller (%d dispatches) while the first was outstanding — REL-115 forbids interleaving one device", n)
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

// TestDifferentDevicesRunConcurrently asserts REL-115 serializes PER device, not
// globally: two commands to two different devices may be in-flight in the
// controller at the same time.
func TestDifferentDevicesRunConcurrently(t *testing.T) {
	entities := []string{"device-A", "device-B"}
	entered := make(chan string, len(entities))
	release := make(chan struct{})
	controller := &blockingController{onDispatch: func(entityID string) {
		entered <- entityID
		<-release // stay in-flight until released
	}}
	surface := mediaPlayerSurface(controller)

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
			t.Fatalf("only %d of %d devices dispatched concurrently — cross-device commands must not be serialized (REL-115 is per-device)", len(seen), len(entities))
		}
	}
	close(release)
	wg.Wait()
	if !seen["device-A"] || !seen["device-B"] {
		t.Errorf("expected both devices to dispatch, saw %v", seen)
	}
}
