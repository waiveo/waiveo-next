package automationhost

import (
	"path/filepath"
	"testing"

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
