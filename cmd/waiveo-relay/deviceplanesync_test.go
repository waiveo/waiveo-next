package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/devicetargets"
	"github.com/maaxton/waiveo-next/internal/relay/ecppoll"
	"github.com/maaxton/waiveo-next/internal/relay/keepalive"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// deviceplanesync_test.go covers the SEAM, which is the part that had nothing.
//
// Every half of this join was already tested in its own package —
// deviceplane.Store.SetEntityState rides the report, ecppoll.Poller.Snapshot
// reports the derived state, devicetargets follows the adopted set — and the
// thing that joins them was an anonymous goroutine in main() whose body could
// be deleted entirely with `go build`, `go test ./cmd/... ./internal/relay/...`
// and validate-deadcode all still green.

// fakePoller stands in for *ecppoll.Poller: it records the target set it was
// pointed at and answers Snapshot from a fixture.
type fakePoller struct {
	mu       sync.Mutex
	targets  map[string]ecppoll.Target
	snapshot map[string]state.Entity
	sets     int
}

func (f *fakePoller) SetTargets(t map[string]ecppoll.Target) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = t
	f.sets++
}

func (f *fakePoller) Snapshot() map[string]state.Entity {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]state.Entity, len(f.snapshot))
	for id, e := range f.snapshot {
		out[id] = e
	}
	return out
}

func (f *fakePoller) polled() map[string]ecppoll.Target {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.targets
}

func (f *fakePoller) setCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sets
}

// fakeKeepalive stands in for *keepalive.Keepalive's target set.
type fakeKeepalive struct {
	mu      sync.Mutex
	targets map[string]keepalive.Target
}

func (f *fakeKeepalive) SetTargets(t map[string]keepalive.Target) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = t
}

func (f *fakeKeepalive) watched() map[string]keepalive.Target {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.targets
}

// syncFixture is the real candidate store and the real adoption gate — only the
// two pollers are doubles, because they are what the assertions read.
type syncFixture struct {
	store  *deviceplane.Store
	gate   *devicetargets.Registry
	poller *fakePoller
	ka     *fakeKeepalive
	sync   devicePlaneSync
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	store := deviceplane.NewStore("relay-sync")
	store.SetSite(dcSite)
	gate := devicetargets.New(nil, store)
	poller := &fakePoller{}
	ka := &fakeKeepalive{}
	return &syncFixture{
		store: store, gate: gate, poller: poller, ka: ka,
		sync: devicePlaneSync{gate: gate, poller: poller, states: store, keepalive: ka},
	}
}

// TestSyncCarriesPolledStateOntoTheCandidateReport is the first of the two
// user-facing claims the deleted closure was the sole mechanism for: GET
// /api/v1/entities reporting what a screen is actually doing. The oracle is the
// device.candidates report, because that is what carries the reading to the app
// peer — asserting the store's internals would prove the write happened without
// proving it reaches anybody.
func TestSyncCarriesPolledStateOntoTheCandidateReport(t *testing.T) {
	f := newSyncFixture(t)
	f.store.Observe(sighting(dcNativeID, "192.168.50.31:8060"), 1000)
	f.gate.SetInventory(adoptionFor(t, dcNativeID, true).Devices)

	entityID := entityIDOf(dcNativeID)
	f.poller.snapshot = map[string]state.Entity{entityID: {ID: entityID, State: "playing"}}

	f.sync.tick()

	cands := f.store.Report().Body.Candidates
	if len(cands) != 1 || len(cands[0].Entities) != 1 {
		t.Fatalf("report = %+v, want one candidate with one entity", cands)
	}
	if got := cands[0].Entities[0].State; got != "playing" {
		t.Fatalf("reported entity state = %q, want \"playing\" — nothing else writes the relay's own observations "+
			"into the store the report is built from", got)
	}
}

// TestSyncMakesALateDiscoveredAdoptedDeviceDrivable is the second claim, and
// the reason the loop re-derives targets rather than only reacting to a
// generation apply. Adoption arrives with a generation; being LOCATABLE does
// not. A device adopted before the first sweep found it must become drivable
// when discovery finds it, with no authoring change in between.
func TestSyncMakesALateDiscoveredAdoptedDeviceDrivable(t *testing.T) {
	f := newSyncFixture(t)
	f.gate.SetInventory(adoptionFor(t, dcNativeID, true).Devices)
	entityID := entityIDOf(dcNativeID)

	// Adopted, never seen: the gate resolves nothing and the pollers watch
	// nothing.
	f.sync.tick()
	if got := f.poller.polled(); len(got) != 0 {
		t.Fatalf("poll targets = %v before discovery, want none — adoption alone is not an address", got)
	}
	if got := f.ka.watched(); len(got) != 0 {
		t.Fatalf("keep-alive targets = %v before discovery, want none", got)
	}

	// The sweep finds it. No generation applies, and nothing calls
	// installInventory — only the tick can pick this up.
	f.store.Observe(sighting(dcNativeID, "192.168.50.31:8060"), 2000)
	f.sync.tick()

	target, ok := f.poller.polled()[entityID]
	if !ok {
		t.Fatalf("poll targets = %v after discovery, want the adopted entity — an adopted screen must not stay "+
			"uncontrollable until the next unrelated authoring change", f.poller.polled())
	}
	if target.Host != "192.168.50.31" || target.Port != 8060 {
		t.Errorf("poll target = %+v, want the discovered address", target)
	}
}

// TestSyncKeepsTheKeepAliveSetEqualToTheDrivableSet is the carried fix: the
// keep-alive was switched on by default while watching cfg.ecpTargets, a map
// that is an out-of-band escape hatch and is empty in the normal deployment —
// so it was on and watching nothing in exactly the power-cut scenario it was
// turned on for. Its set now comes from the same derivation the state poller's
// does, in the same tick, so the two cannot drift.
func TestSyncKeepsTheKeepAliveSetEqualToTheDrivableSet(t *testing.T) {
	f := newSyncFixture(t)
	f.store.Observe(sighting(dcNativeID, "192.168.50.31:8060"), 1000)
	f.gate.SetInventory(adoptionFor(t, dcNativeID, true).Devices)
	entityID := entityIDOf(dcNativeID)

	f.sync.tick()

	watched, ok := f.ka.watched()[entityID]
	if !ok {
		t.Fatalf("keep-alive watches %v, want the adopted+locatable entity — a keep-alive that watches nothing "+
			"self-heals nothing", f.ka.watched())
	}
	if watched.Host != "192.168.50.31" || watched.Port != 8060 {
		t.Errorf("keep-alive target = %+v, want the discovered address", watched)
	}
	if len(f.ka.watched()) != len(f.poller.polled()) {
		t.Errorf("keep-alive watches %d target(s) and the poller %d; one derivation must feed both",
			len(f.ka.watched()), len(f.poller.polled()))
	}

	// Un-adopted by a later generation: the keep-alive must stop watching it in
	// the same tick the poller does, or it keeps re-launching a screen this
	// deployment has just been told it may not drive.
	f.gate.SetInventory(nil)
	f.sync.tick()
	if got := f.ka.watched(); len(got) != 0 {
		t.Fatalf("keep-alive still watches %v after un-adoption, want none", got)
	}
}

// TestSyncToleratesNoKeepAlive: the capability is switchable off, and the sync
// still has to do its other two jobs.
func TestSyncToleratesNoKeepAlive(t *testing.T) {
	f := newSyncFixture(t)
	f.sync.keepalive = nil
	f.store.Observe(sighting(dcNativeID, "192.168.50.31:8060"), 1000)
	f.gate.SetInventory(adoptionFor(t, dcNativeID, true).Devices)

	f.sync.tick() // must not panic
	if _, ok := f.poller.polled()[entityIDOf(dcNativeID)]; !ok {
		t.Fatal("the poller was not re-pointed with keep-alive switched off")
	}
}

// TestSyncRunTicksUntilTheContextIsDone pins the driver itself: it really is
// periodic, and it really stops. A loop that ticked once, or that outlived
// cancellation, would satisfy every case above.
func TestSyncRunTicksUntilTheContextIsDone(t *testing.T) {
	f := newSyncFixture(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		f.sync.run(ctx, time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for f.poller.setCount() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the loop ticked %d time(s) in 5s, want it running periodically", f.poller.setCount())
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after its context was canceled")
	}
}
