// deviceplanesync.go is the periodic JOIN between the three things the relay's
// device plane keeps separately: what it may drive, what it has observed, and
// what it keeps alive.
//
// It is a file, and a named type with a tick() of its own, because of how it
// got here. It was an anonymous goroutine inside run() — a ticker, three lines,
// no name — and it was the sole mechanism behind two user-facing claims: that
// GET /api/v1/entities finally reports what a screen is doing, and that a
// device adopted BEFORE discovery found it becomes drivable without waiting for
// the next authoring change. Deleting its body left `go build`, `go test
// ./cmd/... ./internal/relay/...` and validate-deadcode all green, because
// every half was tested on its own and the join was tested by nothing. That is
// this project's recurring defect shape exactly: the halves proven, the seam
// unproven. A named unit with an injectable collaborator set is the smallest
// thing that makes the seam assertable.
package main

import (
	"context"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/devicetargets"
	"github.com/maaxton/waiveo-next/internal/relay/ecppoll"
	"github.com/maaxton/waiveo-next/internal/relay/keepalive"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// statePoller is the half of *ecppoll.Poller this sync uses: re-point the
// polled set, and read the state it has derived. An interface rather than the
// concrete type so a test can drive the join with a two-method fake and assert
// what reached the other side.
type statePoller interface {
	SetTargets(map[string]ecppoll.Target)
	Snapshot() map[string]state.Entity
}

// entityStateSink is the candidate store's REL-110a per-entity `state` writer
// (*deviceplane.Store.SetEntityState).
type entityStateSink interface {
	SetEntityState(entityID, entityState string) bool
}

// keepaliveTargets is the screen keep-alive's watched set
// (*keepalive.Keepalive.SetTargets). It is nil when the capability is off,
// which is the shape of a deployment that opted out.
type keepaliveTargets interface {
	SetTargets(map[string]keepalive.Target)
}

// devicePlaneSync carries one tick's worth of work across the device plane.
type devicePlaneSync struct {
	// gate is the adoption gate: the intersection of what the app peer adopted
	// and what this relay can currently locate (internal/relay/devicetargets).
	gate *devicetargets.Registry
	// poller is the relay's ECP state poller, the one automationhost.Run
	// consumes observations from.
	poller statePoller
	// states is the candidate store the device.candidates report is built from.
	states entityStateSink
	// keepalive is the screen keep-alive, or nil when it is switched off.
	keepalive keepaliveTargets
}

// tick performs one synchronization pass.
//
// # Why the target set is RE-DERIVED every tick
//
// Adoption arrives on a generation apply (installInventory), but the gate has
// two halves and only one of them has a generation attached. Whether this relay
// can LOCATE an adopted device changes on discovery's own clock: a device
// adopted before it was ever swept for becomes drivable when the sweep finds
// it, and a device that took a new DHCP lease becomes drivable again at its new
// address. Neither event carries a generation, so without this refresh an
// adopted screen stays uncontrollable until the next unrelated authoring
// change.
//
// # Why the keep-alive set comes from the SAME derivation
//
// Because "what this relay drives", "what this relay observes" and "what this
// relay keeps alive" being three separately-maintained lists is how they end up
// disagreeing. One derivation, three consumers.
//
// # Why the state copy is here at all
//
// REL-110a gives each reported entity a `state` "present only once the relay
// has observed one", and the whole chain behind it — candidate store,
// device.candidates, the app's entity registry, GET /api/v1/entities — existed
// and was permanently empty, because nothing wrote the relay's own observations
// into the store the report is built from. This is that write.
func (s devicePlaneSync) tick() {
	resolved := s.gate.Targets()

	pollTargets := make(map[string]ecppoll.Target, len(resolved))
	kaTargets := make(map[string]keepalive.Target, len(resolved))
	for entityID, ep := range resolved {
		pollTargets[entityID] = ecppoll.Target{Host: ep.Host, Port: ep.Port}
		kaTargets[entityID] = keepalive.Target{Host: ep.Host, Port: ep.Port}
	}

	s.poller.SetTargets(pollTargets)
	if s.keepalive != nil {
		s.keepalive.SetTargets(kaTargets)
	}

	for entityID, entity := range s.poller.Snapshot() {
		s.states.SetEntityState(entityID, entity.State)
	}
}

// startDevicePlaneSync builds the join from the relay's real collaborators and
// starts it ticking for the life of ctx. It is the ONE place the running binary
// turns this file's mechanism into a running loop.
//
// It exists as a named function, rather than as a composite literal plus a `go`
// statement inline in main, for the reason the rest of this file exists. Naming
// the TYPE made the seam testable; it did not make the WIRING testable, and the
// wiring is the half that was missing. Deleting the two lines from main left
// every gate green — the extracted unit still had its own passing tests, and
// nothing anywhere asked whether the binary starts it. The same shape, one level
// up: the mechanism proven, its use unproven.
//
// A named call is the smallest thing a test can pin (deviceplanesync_test.go's
// TestMainStartsTheDevicePlaneSync, following cmd/waiveo-feeder's
// TestMainStartsTheConsoleBinding). Its collaborators are positional and
// explicit for the same reason: passing nil for one of them is how a join
// silently stops joining, and a positional argument is a thing that test can
// name.
func startDevicePlaneSync(
	ctx context.Context,
	gate *devicetargets.Registry,
	poller statePoller,
	states entityStateSink,
	ka keepaliveTargets,
	interval time.Duration,
) {
	sync := devicePlaneSync{gate: gate, poller: poller, states: states, keepalive: ka}
	go sync.run(ctx, interval)
}

// run ticks every interval until ctx is done. It is separate from tick so a
// test can assert the WORK without waiting on a clock, and the loop itself
// stays the two lines there is nothing to get wrong in.
func (s devicePlaneSync) run(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.tick()
		}
	}
}
