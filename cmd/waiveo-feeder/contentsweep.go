package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

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
func newContentSweeper(st *store.Store, contentStore *origin.Store, fleet contentgc.FleetConverged, nowMs func() int64) (*contentgc.Sweeper, error) {
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
func runContentSweep(ctx context.Context, sweeper *contentgc.Sweeper, reporter *sweepReporter) {
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
	// Reclamation is permanent, and a reference set derived from ZERO
	// asset-bearing rows (playlists and casts) is the one shape where "this
	// workspace schedules nothing" and "the read returned nothing it should
	// have" are indistinguishable to the sweeper.
	// Neither the store nor the pass can tell them apart — both are legitimate
	// states, and a guard on the count would refuse the ordinary case (assets
	// uploaded and not yet scheduled) to defend against the rare one.
	//
	// So it is said HERE, at the moment it stops being recoverable, to the one
	// party who can tell: an operator who knows whether that workspace had
	// playlists or casts. Nothing is withheld and nothing is retried — the
	// deletion has already happened, and this is the record that it happened on
	// an empty read.
	if res.Reclaimed > 0 && res.SourceRows == 0 {
		log.Printf("waiveo-feeder: NOTE — those %d asset(s) were reclaimed from a reference set derived from zero playlist or cast rows, at generation %d. If this workspace has playlists or casts, that read was wrong and the content is gone; if it schedules nothing, this is the sweep working as intended",
			res.Reclaimed, res.Generation)
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
	reporter.report(res)
}

// sweepReporter says what a sweep DID on the passes where it did nothing.
//
// Before this, a sweep that reclaimed nothing logged nothing at all — and one
// retention reason (fleet-not-converged) was the sole exception. Seven days of
// silence on a real box was therefore indistinguishable between three very
// different states: the sweep correctly retaining, the sweep wrongly retaining,
// and the sweep not running. Every fact needed to tell them apart is already in
// Result and was being discarded.
//
// REPORTED ON CHANGE, NOT ON CADENCE. The sweep runs hourly and a steady box has
// the same answer every hour, so logging each pass would add twenty-four
// identical lines a day and bury the pass where something moved — which is how a
// log stops being read. A summary that repeats its predecessor is dropped, so
// the record reads as "this is the state, and here is each moment it changed".
//
// The first pass after start always reports, because "unchanged since a value
// nobody has seen" is not a thing an operator can act on.
type sweepReporter struct {
	last string
	seen bool
	// logf is the sink, injectable ONLY so a test can observe that a line was
	// emitted. An earlier version of this test inferred "was it logged" from the
	// reporter's own fields, which meant a build with the change detection
	// deleted still passed — the test was describing its own closure rather than
	// this type's behaviour. Nil takes log.Printf, so production has one path.
	logf func(string, ...any)
}

// report logs one line when this pass's summary differs from the last.
//
// Scanned == 0 is deliberately still reported once: an origin holding no assets
// at all is a legitimate steady state, and saying it once is what distinguishes
// it from a sweep that is not running.
func (r *sweepReporter) report(res contentgc.Result) {
	summary := summarizeSweep(res)
	if r.seen && summary == r.last {
		return
	}
	r.last, r.seen = summary, true
	emit := r.logf
	if emit == nil {
		emit = log.Printf
	}
	emit("waiveo-feeder: content retention sweep: %s", summary)
}

// summarizeSweep renders one pass in the terms an operator needs: what it looked
// at, what it took, and — the part that was missing — WHY it kept the rest.
//
// The retention reasons are sorted so two passes with the same outcome produce
// byte-identical summaries; an unordered map range would defeat the change
// detection above by making every pass look new.
func summarizeSweep(res contentgc.Result) string {
	reasons := make([]string, 0, len(res.Retained))
	for reason, n := range res.Retained {
		if n > 0 {
			reasons = append(reasons, fmt.Sprintf("%s=%d", reason, n))
		}
	}
	sort.Strings(reasons)
	held := "nothing held"
	if len(reasons) > 0 {
		held = "held " + strings.Join(reasons, " ")
	}
	fleet := "fleet at generation " + strconv.FormatInt(res.Generation, 10)
	if !res.FleetKnown {
		fleet = "fleet generation UNKNOWN"
	} else if !res.FleetConverged {
		fleet = "fleet not converged on generation " + strconv.FormatInt(res.Generation, 10)
	}
	return fmt.Sprintf("scanned %d, reclaimed %d (%d bytes), %s, %s, from %d asset-bearing row(s)",
		res.Scanned, res.Reclaimed, res.ReclaimedBytes, held, fleet, res.SourceRows)
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
) contentgc.FleetConverged {
	return func(target int64) (converged, known bool) {
		active := activeRelays()
		if len(active) == 0 {
			// No relay is entitled to connect, so no screen is being served and
			// no older program exists to break. Converged for ANY target — a
			// relay-less box must still reclaim, and expressing this as a
			// generation would make it fail the equality test forever while
			// reporting the fleet as known, so nothing would ever say why.
			return true, true
		}
		connected := map[string]bool{}
		for _, c := range connectedRelays() {
			connected[c.RelayID] = true
		}
		for _, relayID := range active {
			if !connected[relayID] {
				return false, false
			}
			frame, ok := lastStateAck(relayID)
			if !ok {
				return false, false
			}
			var ack wire.StateAckBody
			if err := json.Unmarshal(frame.Body, &ack); err != nil {
				// An ack this process cannot read is not an ack. Treating a
				// malformed body as generation 0 would report a floor of 0 and
				// merely stall the sweep, but it would do so by ASSERTING a fact
				// about the fleet from bytes that did not parse.
				return false, false
			}
			// EXACT equality per relay, not a running minimum. A relay ahead of
			// the target is serving a program built from rows the caller has not
			// read; a minimum would hide it behind whichever relay is furthest
			// behind.
			if ack.AppliedGeneration != target {
				return false, true
			}
		}
		return true, true
	}
}
