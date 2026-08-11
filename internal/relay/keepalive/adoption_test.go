package keepalive

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// inventoryOf marshals typed REL-063 device entries into the raw-JSON section
// shape a real snapshot carries, so these tests decode exactly what the feeder
// produces rather than a hand-written approximation of it.
func inventoryOf(t *testing.T, entries ...wire.DeviceEntry) wire.DeviceInventory {
	t.Helper()
	inv := wire.DeviceInventory{Devices: []json.RawMessage{}, PackMatchPatterns: []json.RawMessage{}}
	for _, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal device entry: %v", err)
		}
		inv.Devices = append(inv.Devices, raw)
	}
	return inv
}

// mediaPlayer builds an adopted media-player device carrying one entity.
func mediaPlayer(deviceID, entityID string, enabled bool) wire.DeviceEntry {
	return wire.DeviceEntry{
		DeviceID: deviceID,
		Driver:   "roku",
		NativeID: deviceID,
		Entities: []wire.DeviceEntity{{
			EntityID:    entityID,
			DeviceClass: mediaPlayerClass,
			Enabled:     enabled,
			DisplayName: entityID,
			Category:    "primary",
		}},
	}
}

// A relay that has applied nothing knows which screens are its own only by
// guessing, and the guess this capability would make ("everything I can
// reach") is the coexistence failure the gate exists to prevent.
func TestAdoptionSetIsEmptyBeforeAnyGeneration(t *testing.T) {
	set := NewAdoptionSet()
	if set.IsAdopted("screen.hanger") {
		t.Fatal("a set with no applied generation adopted a screen; the gate must be fail-closed")
	}
	var nilSet *AdoptionSet
	if nilSet.IsAdopted("screen.hanger") {
		t.Fatal("a nil AdoptionSet adopted a screen")
	}
}

func TestAdoptionSetAdoptsEnabledMediaPlayers(t *testing.T) {
	set := NewAdoptionSet()
	set.Apply(7, inventoryOf(t,
		mediaPlayer("dev.hanger", "screen.hanger", true),
		mediaPlayer("dev.lobby", "screen.lobby", false),
		wire.DeviceEntry{
			DeviceID: "dev.thermostat",
			Driver:   "zwave",
			NativeID: "t1",
			Entities: []wire.DeviceEntity{{EntityID: "climate.office", DeviceClass: "thermostat", Enabled: true}},
		},
	))

	if !set.IsAdopted("screen.hanger") {
		t.Error("an adopted, enabled media-player was not adopted")
	}
	// `enabled:false` is the operator saying "do not operate this" — the same
	// statement, in the same section, as adoption itself.
	if set.IsAdopted("screen.lobby") {
		t.Error("a disabled entity was adopted")
	}
	// A thermostat has no channel to re-launch; REG-066's `launch` belongs to
	// the media-player vocabulary alone.
	if set.IsAdopted("climate.office") {
		t.Error("a non-media-player entity was adopted as a keep-alive target")
	}
	if set.IsAdopted("screen.unknown") {
		t.Error("an entity absent from the inventory was adopted")
	}
}

// REL-063's section is a COMPLETE statement of the adopted set, so an entity
// that disappears from a later generation has been un-adopted. Merging would
// make handing a screen back to the legacy stack impossible to express.
func TestAdoptionSetReplacesRatherThanMerges(t *testing.T) {
	set := NewAdoptionSet()
	set.Apply(1, inventoryOf(t,
		mediaPlayer("dev.hanger", "screen.hanger", true),
		mediaPlayer("dev.lobby", "screen.lobby", true),
	))
	set.Apply(2, inventoryOf(t, mediaPlayer("dev.hanger", "screen.hanger", true)))

	if !set.IsAdopted("screen.hanger") {
		t.Error("the surviving screen lost its adoption")
	}
	if set.IsAdopted("screen.lobby") {
		t.Fatal("a screen dropped from the new generation is still adopted; un-adoption cannot be expressed")
	}
}

// A late apply from a superseded generation must not resurrect an old set over
// a newer one — the same monotonicity every other apply path enforces.
func TestAdoptionSetIgnoresAStaleGeneration(t *testing.T) {
	set := NewAdoptionSet()
	set.Apply(5, inventoryOf(t, mediaPlayer("dev.hanger", "screen.hanger", true)))
	set.Apply(4, inventoryOf(t, mediaPlayer("dev.lobby", "screen.lobby", true)))

	if !set.IsAdopted("screen.hanger") {
		t.Error("a stale apply replaced the newer adoption set")
	}
	if set.IsAdopted("screen.lobby") {
		t.Error("a stale generation's adoption took effect")
	}
}

// One malformed row must cost that device its adoption, not the whole fleet's.
func TestAdoptionSetSkipsUnreadableEntries(t *testing.T) {
	inv := inventoryOf(t, mediaPlayer("dev.hanger", "screen.hanger", true))
	inv.Devices = append(inv.Devices, json.RawMessage(`{"device_id": 12345}`))

	set := NewAdoptionSet()
	set.Apply(1, inv)
	if !set.IsAdopted("screen.hanger") {
		t.Fatal("one unreadable entry disabled keep-alive for every other screen")
	}
}

// The gate in the state machine itself: a screen doing everything that would
// otherwise earn a recovery launch gets none while un-adopted, and gets one
// only after adoption arrives.
func TestUnadoptedScreenIsNeverLaunched(t *testing.T) {
	fc := &fakeController{}
	adopted := map[string]bool{}
	k := New(Config{
		LaunchDelay: time.Millisecond,
		Controller:  fc,
		Adopted:     func(id string) bool { return adopted[id] },
	})
	t0 := time.Unix(1700000000, 0)

	// Textbook recovery conditions, at length: powered on, long past the
	// settle delay, motionless at Home.
	for i := 0; i < 10; i++ {
		k.recordObservation(obsFor("screen.legacy", "PowerOn", "home"), t0.Add(time.Duration(i)*time.Second))
	}
	if got := fc.callCount(); got != 0 {
		t.Fatalf("an un-adopted screen was re-launched %d time(s) — this is the two-controller flapping failure (%+v)", got, fc.calls)
	}

	// Adoption arrives (an operator adopts the screen; a new generation lands).
	adopted["screen.legacy"] = true

	// The streak restarts from scratch: progress accumulated while the screen
	// belonged to somebody else must not fire the instant it becomes ours.
	k.recordObservation(obsFor("screen.legacy", "PowerOn", "home"), t0.Add(20*time.Second))
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch fired on the FIRST poll after adoption (%d call(s)); the confirmation must start fresh", got)
	}
	k.recordObservation(obsFor("screen.legacy", "PowerOn", "home"), t0.Add(21*time.Second))
	k.recordObservation(obsFor("screen.legacy", "PowerOn", "home"), t0.Add(22*time.Second))
	if got := fc.callCount(); got == 0 {
		t.Fatal("an adopted screen stuck at Home was never re-launched")
	}
}

// The gate is fail-closed at the Config level too: this capability is ON by
// default in the relay, so a wiring mistake that leaves Adopted nil must mean
// "drive nothing", never "drive every reachable Roku".
func TestNilAdoptedGateDrivesNothing(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{LaunchDelay: time.Millisecond, Controller: fc}) // Adopted left nil
	t0 := time.Unix(1700000000, 0)

	for i := 0; i < 10; i++ {
		k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(time.Duration(i)*time.Second))
	}
	if got := fc.callCount(); got != 0 {
		t.Fatalf("a nil Adopted gate dispatched %d launch(es); it must adopt nothing (%+v)", got, fc.calls)
	}
}

// A screen un-adopted mid-streak stops being driven immediately, and its
// primed streak does not survive to fire the moment it is re-adopted.
func TestUnadoptingMidStreakResetsProgress(t *testing.T) {
	fc := &fakeController{}
	adopted := map[string]bool{"scr1": true}
	k := New(Config{
		LaunchDelay: time.Millisecond,
		Controller:  fc,
		Adopted:     func(id string) bool { return adopted[id] },
	})
	t0 := time.Unix(1700000000, 0)

	// One qualifying Home poll: streak primed, one short of firing.
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0)
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(time.Second))
	if got := fc.callCount(); got != 0 {
		t.Fatalf("fired early: %d call(s)", got)
	}

	adopted["scr1"] = false
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(2*time.Second))

	adopted["scr1"] = true
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(3*time.Second))
	if got := fc.callCount(); got != 0 {
		t.Fatalf("a streak primed before un-adoption survived it and fired %d launch(es)", got)
	}
}
