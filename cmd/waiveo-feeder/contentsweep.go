package main

import (
	"context"
	"encoding/json"
	"log"
	"math"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/contentgc"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// newContentSweeper builds this deployment's content retention sweep: the pass
// that reclaims content bytes no live generation references any more
// (internal/feeder/contentgc).
//
// It is a function rather than inline in main for the reason startWebhookDelivery
// and startConsoleBinding are: so a test can drive the IDENTICAL wiring — the
// same origin, the same store, the same fleet oracle, the same clock. A test that
// reassembled it would pass while main's copy of it was missing, and "the
// component exists but nothing a deployment runs reaches it" is exactly the state
// the required-pack floor was in before its own option was wired.
//
// The windows are the package defaults. This deployment has no reason to be more
// aggressive than the shipping policy, and every reason not to be: it is the
// dev-lab box with real screens on it.
func newContentSweeper(st *store.Store, contentStore *origin.Store, fleet contentgc.FleetFloor, nowMs func() int64) (*contentgc.Sweeper, error) {
	return contentgc.New(contentgc.Config{
		Origin:     contentStore,
		References: st,
		Fleet:      fleet,
		NowMs:      nowMs,
	})
}

// runContentSweep runs one pass and reports it, in the shape the other two
// retention arms on this cadence use.
//
// Failure is a WARNING and never fatal, and the direction of that failure is the
// point: a sweep that cannot read the reference set deletes NOTHING (see
// contentgc.Sweep), so the worst a broken sweep can do to a running box is let
// disk usage keep growing — which is the state the box was already in before this
// existed. There is no failure mode of this loop that removes content it should
// not have; the guards that prevent that live inside the pass, not here.
func runContentSweep(ctx context.Context, sweeper *contentgc.Sweeper) {
	res, err := sweeper.Sweep(ctx)
	if err != nil {
		log.Printf("waiveo-feeder: WARNING — the content retention sweep failed: %v (nothing was reclaimed)", err)
		return
	}
	for _, rerr := range res.RemoveErrors {
		log.Printf("waiveo-feeder: WARNING — the content retention sweep could not reclaim an asset: %v (it is still served; the next sweep retries)", rerr)
	}
	if res.Reclaimed > 0 {
		log.Printf("waiveo-feeder: reclaimed %d unreferenced content asset(s), %d byte(s), at generation %d",
			res.Reclaimed, res.ReclaimedBytes, res.Generation)
	}
	// A fleet this process cannot account for is the one retention outcome an
	// operator has to be able to see WITHOUT a reclamation having happened: it
	// means content will accumulate indefinitely, and the cause (a relay that is
	// enrolled but not connecting) is a fixable deployment fact rather than a
	// property of the content.
	if !res.FleetKnown && res.Retained[contentgc.ReasonFleetNotConverged] > 0 {
		log.Printf("waiveo-feeder: the content retention sweep reclaimed nothing: %d unreferenced asset(s) are held because an enrolled relay is not connected, so this feeder cannot tell which desired-state generation the fleet is serving",
			res.Retained[contentgc.ReasonFleetNotConverged])
	}
}

// contentSweepFleetFloor answers, for the content retention sweep, "what is the
// oldest desired-state generation anything out there could still be serving?"
//
// The three inputs are the three facts that question needs, and they come from
// the two registries that actually hold them — the enrollment registry (which
// relays EXIST) and the connection server (which are connected, and what each
// last said it applied). Nothing here is derived from a timer or a guess.
//
// The rules, in the order they matter:
//
//   - NO ACTIVE RELAY AT ALL ⇒ known, and vacuously so. Content reaches a screen
//     only through a relay's snapshot; with no relay entitled to connect, there
//     is no program in play anywhere and no older generation to protect.
//   - AN ACTIVE RELAY THAT IS NOT CONNECTED ⇒ UNKNOWN. This is the case the whole
//     function exists for. Its screens go on fetching content from this origin
//     directly (relay/1 REL-140) using whatever program it applied before it went
//     quiet, and this process cannot ask what that was. Reclaiming against the
//     remaining fleet's generation would delete content those screens are still
//     playing.
//   - A CONNECTED RELAY THAT HAS NEVER ACKNOWLEDGED ⇒ UNKNOWN. It is mid-pull, or
//     mid-handshake; it has not yet said what it is serving.
//   - OTHERWISE the floor is the minimum applied_generation across the fleet.
//     REL-054 defines that field as the relay's ACTUAL last-applied generation —
//     on a rejected snapshot it is the unadvanced prior one — so a relay that
//     refused the current generation lowers the floor and, correctly, stops the
//     sweep rather than being counted as converged.
//
// A stale ack — one recorded before the relay's current connection — is used
// rather than discarded. The relay persists its last-applied generation across
// restarts, so an old ack under-reports at worst (the relay may since have
// applied a later one), and under-reporting only ever makes the sweep more
// conservative.
func contentSweepFleetFloor(
	activeRelays func() []string,
	connectedRelays func() []relayconn.ConnectedRelay,
	lastStateAck func(string) (wire.Frame, bool),
) contentgc.FleetFloor {
	return func() (int64, bool) {
		active := activeRelays()
		if len(active) == 0 {
			return 0, true
		}
		connected := map[string]bool{}
		for _, c := range connectedRelays() {
			connected[c.RelayID] = true
		}
		floor := int64(math.MaxInt64)
		for _, relayID := range active {
			if !connected[relayID] {
				return 0, false
			}
			frame, ok := lastStateAck(relayID)
			if !ok {
				return 0, false
			}
			var ack wire.StateAckBody
			if err := json.Unmarshal(frame.Body, &ack); err != nil {
				// An ack this process cannot read is not an ack. Treating a
				// malformed body as generation 0 would report a floor of 0 and
				// merely stall the sweep, but it would do so by ASSERTING a fact
				// about the fleet from bytes that did not parse.
				return 0, false
			}
			if ack.AppliedGeneration < floor {
				floor = ack.AppliedGeneration
			}
		}
		return floor, true
	}
}
