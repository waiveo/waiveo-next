package ecppoll

import (
	"context"
	"testing"
	"time"
)

// livetargets_test.go covers the two capabilities that let the poller follow
// the relay's adoption gate instead of a map fixed at boot: SetTargets (the
// polled set tracks the commandable set) and Snapshot (the derived state is
// readable, so it can be reported upward).
//
// The set has to be live for the same reason the controller's targets do. A
// device adopted after this process started, or one that only became locatable
// when the first SSDP sweep landed, would otherwise never be polled — its
// entity would report no state forever and no edge rule could ever fire on it.

// TestSetTargetsAddsANewlyAdoptedDevice: a poller that started with nothing
// picks up a target and polls it on the next cycle, with no reconstruction.
func TestSetTargetsAddsANewlyAdoptedDevice(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()

	p := New(nil, time.Hour)
	ctx := context.Background()

	p.pollAll(ctx)
	select {
	case obs := <-p.ch:
		t.Fatalf("polled with no targets and emitted %+v", obs)
	default:
	}

	p.SetTargets(map[string]Target{"tv1": fx.target(t)})
	p.pollAll(ctx)

	obs, ok := p.Next()
	if !ok || obs.Entity.ID != "tv1" {
		t.Fatalf("Next after SetTargets = (%+v, %v), want the tv1 seed observation", obs, ok)
	}
}

// TestSetTargetsDropsAnUnadoptedDevice: a device removed from the set stops
// being polled. This is the un-adoption path — the relay must stop touching a
// device the moment the operator releases it, including with reads.
func TestSetTargetsDropsAnUnadoptedDevice(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()

	p := New(map[string]Target{"tv1": fx.target(t)}, time.Hour)
	ctx := context.Background()
	p.pollAll(ctx)
	if _, ok := p.Next(); !ok {
		t.Fatal("expected the first seed observation")
	}

	p.SetTargets(nil)
	fx.setDeviceInfo(`<device-info><power-mode>Ready</power-mode></device-info>`)
	p.pollAll(ctx)

	select {
	case obs := <-p.ch:
		t.Fatalf("emitted %+v for a target that was removed from the set", obs)
	default:
	}
	if _, ok := p.Snapshot()["tv1"]; ok {
		t.Fatal("Snapshot still holds a removed target's state")
	}
}

// TestReAdoptionSeedsAgain: because a removed target's remembered snapshot is
// dropped, a re-adoption behaves like a first sighting — a self-transition seed
// that re-seeds the engine's baselines rather than a transition classified
// against a snapshot from before the device left the set. Classifying against
// that stale snapshot would fire triggers on a change nobody was watching for.
func TestReAdoptionSeedsAgain(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()

	p := New(map[string]Target{"tv1": fx.target(t)}, time.Hour)
	ctx := context.Background()
	p.pollAll(ctx)
	if _, ok := p.Next(); !ok {
		t.Fatal("expected the first seed observation")
	}

	p.SetTargets(nil)
	// While unpolled, the device changes state.
	fx.setDeviceInfo(`<device-info><power-mode>Ready</power-mode></device-info>`)

	p.SetTargets(map[string]Target{"tv1": fx.target(t)})
	p.pollAll(ctx)

	obs, ok := p.Next()
	if !ok {
		t.Fatal("expected an observation after re-adoption")
	}
	if obs.StateChanged {
		t.Fatalf("StateChanged = true after re-adoption; want a fresh self-transition seed, not a transition against a pre-removal snapshot (%+v)", obs)
	}
	if obs.Entity.State != "standby" {
		t.Fatalf("seed state = %q, want standby (the device's state now)", obs.Entity.State)
	}
}

// TestSnapshotReportsTheDerivedState is the read side the relay's entity-state
// surface is built on: the state a poll derived, without issuing a second round
// of ECP requests to ask again.
func TestSnapshotReportsTheDerivedState(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()

	p := New(map[string]Target{"tv1": fx.target(t)}, time.Hour)
	p.pollAll(context.Background())

	snap := p.Snapshot()
	e, ok := snap["tv1"]
	if !ok {
		t.Fatalf("Snapshot = %v, want an entry for tv1", snap)
	}
	if e.State != "on" {
		t.Fatalf("Snapshot state = %q, want on", e.State)
	}
	// The attributes the state was derived FROM are carried too — this is the
	// "active app / power mode" answer, not just a bare classification.
	if got := e.Attributes["active_app"]; got != "TestApp" {
		t.Fatalf("Snapshot active_app = %v, want TestApp", got)
	}
	if got := e.Attributes["power_mode"]; got != "PowerOn" {
		t.Fatalf("Snapshot power_mode = %v, want PowerOn", got)
	}
}

// TestSnapshotOmitsANeverPolledEntity: an entity that has not completed a poll
// is ABSENT rather than present with a fabricated state. Reporting a default
// would be indistinguishable from a real observation to everything downstream.
func TestSnapshotOmitsANeverPolledEntity(t *testing.T) {
	p := New(map[string]Target{"tv1": {Host: "192.0.2.1"}}, time.Hour)
	if got := len(p.Snapshot()); got != 0 {
		t.Fatalf("Snapshot has %d entries before any poll, want 0", got)
	}
}

// TestSnapshotIsACopy: a caller must not be able to reach into the poller's own
// record. The state reporter iterates this map on its own goroutine while Run
// keeps polling, so an aliased map would be a data race as well as a leak.
func TestSnapshotIsACopy(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()

	p := New(map[string]Target{"tv1": fx.target(t)}, time.Hour)
	p.pollAll(context.Background())

	snap := p.Snapshot()
	e := snap["tv1"]
	e.Attributes["active_app"] = "tampered"
	delete(snap, "tv1")

	again := p.Snapshot()
	if again["tv1"].Attributes["active_app"] != "TestApp" {
		t.Fatalf("mutating a Snapshot changed the poller's own record: %+v", again["tv1"])
	}
}

// TestSetTargetsCopiesTheMap: the caller's map is rebuilt on every refresh, so
// the poller must not stay aliased to one it may mutate next tick.
func TestSetTargetsCopiesTheMap(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()

	targets := map[string]Target{"tv1": fx.target(t)}
	p := New(nil, time.Hour)
	p.SetTargets(targets)
	delete(targets, "tv1")

	p.pollAll(context.Background())
	if _, ok := p.Next(); !ok {
		t.Fatal("expected tv1 to still be polled; SetTargets must copy its map")
	}
}
