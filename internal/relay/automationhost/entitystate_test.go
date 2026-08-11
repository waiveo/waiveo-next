package automationhost

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
	"github.com/maaxton/waiveo-next/internal/slidelive"
)

// EntityState is the relay's live device-plane readout — the source a native
// slide's `entity` widget is resolved from at Lease issuance. These cases pin
// what a wall actually depends on: that a state shows up at all without any
// rule subjecting the entity, that the FIRST reading after boot counts, and
// that "never observed" stays distinguishable from "observed".

func TestEntityStateIsUnknownBeforeAnyObservation(t *testing.T) {
	host, _ := newTestHost(t, filepath.Join(t.TempDir(), "relay.db"), &recordController{})

	if st, ok := host.EntityState(testScreenEntity); ok {
		t.Fatalf("an entity nothing has reported must be unknown, got %q", st)
	}
}

func TestEntityStateRecordsTheFirstObservation(t *testing.T) {
	// The poller's very first snapshot per entity arrives as a SELF-transition
	// seed: StateChanged is false and no rule fires. It is nonetheless the
	// reading that first fills a slide's entity widget after a relay boot, so
	// recording only on change would leave the widget blank until the device
	// happened to do something.
	host, _ := newTestHost(t, filepath.Join(t.TempDir(), "relay.db"), &recordController{})
	reg := registry.FixtureRegistry{}

	seed := state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, "off"))
	if seed.StateChanged {
		t.Fatalf("setup: the seed observation must be a self-transition")
	}
	if _, err := host.Observe(seed); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	st, ok := host.EntityState(testScreenEntity)
	if !ok || st != "off" {
		t.Fatalf("EntityState = (%q,%v), want (\"off\",true)", st, ok)
	}
}

func TestEntityStateTracksTransitionsWithNoRulesLoaded(t *testing.T) {
	// No ApplyEdgeRules at all. The engine subjects nothing, so an
	// engine-snapshot-derived readout would know nothing — yet a real device is
	// reporting and a slide may well be displaying it. This is exactly why the
	// host keeps its own map.
	host, _ := newTestHost(t, filepath.Join(t.TempDir(), "relay.db"), &recordController{})
	reg := registry.FixtureRegistry{}

	for _, st := range []string{"off", "on", "playing"} {
		if _, err := host.Observe(state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, st))); err != nil {
			t.Fatalf("Observe(%s): %v", st, err)
		}
	}

	got, ok := host.EntityState(testScreenEntity)
	if !ok || got != "playing" {
		t.Fatalf("EntityState = (%q,%v), want (\"playing\",true) — the LAST observed state", got, ok)
	}
}

func TestHostSatisfiesTheSlideEntitySource(t *testing.T) {
	// The wiring the relay binary depends on, in the SAME form the binary uses:
	// the bound EntityState method adapted through slidelive.EntitySourceFunc
	// rather than the whole Host handed to the interface (see that adapter's own
	// doc for why the binary passes one method and not the object). A signature
	// drift here would otherwise only surface as a compile error in
	// cmd/waiveo-relay, which is a worse place to find it.
	host, _ := newTestHost(t, filepath.Join(t.TempDir(), "relay.db"), &recordController{})
	var src slidelive.EntitySource = slidelive.EntitySourceFunc(host.EntityState)

	reg := registry.FixtureRegistry{}
	if _, err := host.Observe(state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, "on"))); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	widget := []wire.Layer{{
		Kind: wire.LayerKindEntity, X: 0, Y: 0, W: 400, H: 80,
		Text: "Screen: {state}", EntityID: testScreenEntity,
	}}
	layers := slidelive.ResolveLayers(widget, slidelive.Sources{Entity: src})
	if got, want := layers[0].Value, "Screen: on"; got != want {
		t.Fatalf("resolved widget = %q, want %q", got, want)
	}
}

// blockingController is a device adapter stuck mid-request — a TV that accepted
// the TCP connection and is answering nothing, which internal/relay/ecp bounds
// at three seconds per command and a firing rule can do several times.
type blockingController struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingController) Dispatch(string, string, map[string]any) error {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return nil
}

// TestEntityStateDoesNotBlockOnDeviceIO is the contract slidelive.Sources
// states for this source — "Like Current it must not block" — asserted against
// the thing that would break it.
//
// The reader here is not a test convenience: EntityState is called from the
// Lease-issuance path (playerserver -> slidelive.ResolveContent ->
// EntitySourceFunc). If it waits on the engine lock, then every time a rule
// fires a device_command at a wedged device, the poll of every screen served by
// this relay stalls behind that device's HTTP timeout — including screens with
// no entity widget and no relationship to the device being commanded.
//
// Without the separate stateMu this test hangs until the test timeout: Observe
// holds mu for its whole body, Dispatch inside it never returns, and the read
// queues behind it.
func TestEntityStateDoesNotBlockOnDeviceIO(t *testing.T) {
	ctrl := &blockingController{entered: make(chan struct{}), release: make(chan struct{})}
	host, _ := newTestHost(t, filepath.Join(t.TempDir(), "relay.db"), ctrl)
	t.Cleanup(func() { close(ctrl.release) })

	if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(demoEdgeRule)}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}

	reg := registry.FixtureRegistry{}
	// The poller's first snapshot per entity is a self-transition seed that
	// sets the trigger baseline and fires nothing (RUL-300/304).
	if _, err := host.Observe(state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, "off"))); err != nil {
		t.Fatalf("Observe (baseline): %v", err)
	}
	go func() {
		// off -> on fires the demo rule's launch, which reaches the controller
		// and stops there.
		_, _ = host.Observe(state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, "on")))
	}()

	select {
	case <-ctrl.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("setup: the rule never dispatched, so nothing is holding the engine lock")
	}

	read := make(chan string, 1)
	go func() {
		st, _ := host.EntityState(testScreenEntity)
		read <- st
	}()
	select {
	case st := <-read:
		// The state is recorded BEFORE the engine advances, so the reading
		// visible during the dispatch is the one that caused it.
		if st != "on" {
			t.Fatalf("EntityState during a device dispatch = %q, want \"on\"", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EntityState blocked while a device command was in flight — issuing a Lease must never wait on an ECP round trip")
	}
}
