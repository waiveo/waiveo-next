package automationhost

import (
	"context"
	"log"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/rules/engine"
)

// Tick advances the engine's TIME-driven mechanisms once, against the host's
// real clock, and emits an automation.run into the durable telemetry queue for
// every firing it produces — the same recording Observe does, because a firing
// is a firing whatever caused it (REL-090/093, EVT-040/041).
//
// Observe is the engine's other half and it is not enough on its own. Observe
// steps only the triggers whose subject is the entity just observed; everything
// whose readiness is a matter of TIME PASSING rather than of a device reporting
// advances here, in engine.Tick:
//
//   - a `delay`'s deferred tail (RUL-190). A triggered rule that reaches a
//     `delay` parks its remaining actions as the rule's in-flight run;
//     engine.resumeDelay, reachable only from Tick, is what resumes them. With
//     nothing ticking, "turn the sign on, wait 30s, turn it off" turns it on and
//     leaves it on for the life of the process.
//   - a `for`-hold (RUL-024/033/310). The hold elapses in engine.stepTriggerTick,
//     also reachable only from Tick. A hold whose span passes with no further
//     observation of that entity — the ordinary case for "has been off for five
//     minutes" — never fires without this.
//   - a stabilization-held fire (RUL-301), whose bounded fallback deadline is
//     released in Tick so a held transition dispatches even if no further
//     readiness signal arrives.
//   - every wall-clock (`time`, `time_pattern`, `sun`) trigger, live and missed
//     (RUL-041/051/340/350-355). engine.dispatchSchedule is reached only from
//     Tick: it enumerates occurrences over the half-open interval since the last
//     tick, so no occurrence is lost to a coarse cadence — but with no tick at
//     all, the interval never advances and a scheduled rule never fires.
//
// It returns the firings' dispositions and the first durable write-through error
// the records hit (telemetry.Buffer.StoreErr), mirroring Observe exactly. Like
// Observe, the dispositions it can produce are only the ran/skipped/restarted
// set (RUL-246) — every one of them arrives through the same fireRuleWith path
// Observe's do, and the generation-swap Canceled outcome arises solely from
// ApplyGeneration — so telemetry.AutomationRunEntry's EVT-041 enum is never fed
// an out-of-band value. A resumed `delay` tail yields NO disposition on purpose:
// the firing that scheduled it already recorded `ran`, and recording a second
// automation.run for the same firing would double-count it in the app's event
// log.
//
// # Locking
//
// It takes h.mu, the lock that stands in for the relay's single evaluation loop,
// because engine.Engine has no internal locking (see Host's own doc). That makes
// the ticker goroutine the fifth caller of mu, alongside Observe (the ECP poll
// loop), ApplyEdgeRules (the desired-state re-pull loop), SetClockTrust (the
// clock.hint handler) and SetLocation.
//
// It adds no new lock-ordering edge and so cannot deadlock: mu is a leaf, and
// the only nested acquisition anywhere in this package is Observe's mu -> stateMu,
// which Tick does not participate in. Tick deliberately does NOT touch
// entityStates — that map is the observed device-plane view, and a tick observes
// nothing.
//
// What it DOES add is latency coupling, and that is worth stating plainly
// because it is the hazard this package was already written around. mu is held
// across engine advancement, and advancing the engine can dispatch device
// commands: deviceplane.CommandSurface.Execute makes a real ECP HTTP request
// with a three-second timeout, and a firing rule can make several. So a tick
// that fires a rule against a wedged TV holds mu for seconds, and the poll
// loop's next Observe waits behind it. That coupling already existed between an
// observation-fired rule and the re-pull loop; a tick is a new ORIGINATOR of it,
// not a new class of it, and the delay it can impose on an observation is
// bounded by the same command timeout that already bounds Observe's own.
//
// The invariant that actually matters is preserved by construction: EntityState
// — whose contract is that it MUST NOT block on device I/O, because its caller
// is the goroutine issuing a screen's Lease — reads under stateMu, never mu. A
// tick stalled on a wedged TV therefore cannot stall the Lease of a screen that
// has nothing to do with it. That is exactly why stateMu is a second lock, and
// adding a time-driven caller of mu is precisely the case it was separated for.
func (h *Host) Tick() ([]engine.RunDisposition, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	disps := h.engine.Tick(h.clk)
	if len(disps) == 0 {
		// The overwhelmingly common tick: nothing was due. Do not read the wall
		// clock and do not touch the buffer.
		return nil, nil
	}
	atMs := h.clk.WallMillis()
	for _, d := range disps {
		schema, payload, subject := telemetry.AutomationRunEntry(d, atMs)
		h.buf.Record(schema, payload, subject, atMs)
	}
	return disps, h.buf.StoreErr()
}

// SeedScheduleFloor supplies the engine's resume cursor for schedule triggers
// (RUL-370 -> engine.SeedScheduleFloor): the wall instant below which no
// occurrence is ever enumerated. It MUST be called before the first Tick, and
// the reason is a cost rather than a nicety.
//
// The first Tick after a (re)start is a RESUME: engine.dispatchSchedule treats
// the whole span from the floor up to now as downtime and enumerates every
// occurrence in it, so each can be caught up through its trigger's misfire
// policy (RUL-350/351/354). With the floor left at its zero value that span
// begins at the Unix epoch. The enumerators walk the window day by day
// (schedule.daysInWindow) with no cap, so a `time` trigger builds ~20,000
// candidate instants and a `time_pattern` firing once a minute builds ~30
// MILLION — measured at 3.6s and ~240MB on a developer laptop, under h.mu,
// before the relay has served anything, and far worse on the appliance this
// ships to. Under a `fire_each` misfire policy it would then dispatch every one
// of them at a real device.
//
// The relay already persists exactly the value this wants: the verified clock
// floor internal/relay/clocktrust.Controller advances into the operational store
// (REL-130/132). It had simply never been handed to the engine — engine.SeedScheduleFloor
// had no production caller at all — which stayed invisible for as long as
// nothing called Tick.
//
// It takes h.mu like every other engine-advancing method.
func (h *Host) SeedScheduleFloor(wallMs int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.engine.SeedScheduleFloor(wallMs)
}

// TickLoop drives Tick once per tick delivered on ticks, until ctx is cancelled
// or ticks is closed. It is the engine's TIME drive loop, the counterpart of
// Run's observation loop — between them they are the two inputs an edge rule can
// advance on, and neither is optional.
//
// The tick CADENCE is injected: a time.Ticker's channel in the relay binary, a
// manual channel in tests, so nothing here sleeps on the wall clock and a test
// can advance time exactly. This is the same shape schedulehost.Resolver.Loop
// and cmd/waiveo-relay's telemetryFlushLoop already use.
//
// A durable write-through error is LOGGED and the loop CONTINUES, which is
// deliberately different from Run, and the difference is not an oversight. Run
// returns on the first such error because the observation stream it drives can
// be restarted; this loop is what resumes a `delay` that is already in flight,
// so stopping it strands a rule mid-sequence — the sign turns on and never turns
// off — for the life of the process. A telemetry queue that has stopped
// accepting writes is a reporting failure; refusing to finish a run that is
// already half-executed on real hardware is a control failure, and the control
// failure is the worse one.
//
// Buffer.StoreErr DRAINS on read, so be precise about what that costs: a tick
// that fires can consume an error Observe would otherwise have returned to
// Host.Run. The error is not lost — it is logged here instead of ending the
// observation loop there — but the two paths do share one error slot, and this
// one is the more forgiving reader of it.
func (h *Host) TickLoop(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			if _, err := h.Tick(); err != nil {
				log.Printf("automationhost: tick recorded automation.run but durable write-through failed (%v); engine time drive continues", err)
			}
		}
	}
}
