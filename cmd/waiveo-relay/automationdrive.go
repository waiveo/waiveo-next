package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
)

// automationTickInterval is the cadence the running relay advances the edge
// engine's TIME-driven mechanisms at (automationhost.Host.TickLoop ->
// engine.Tick): `delay` resumption (RUL-190), `for`-holds (RUL-024/033), the
// stabilization release (RUL-301), and wall-clock schedule triggers
// (RUL-041/051/340).
//
// # Why 250ms
//
// rules/1 expresses every duration this drives in whole SECONDS: `for` is "a
// non-negative integer number of seconds" (RUL-024) and `delay` declares
// `duration_seconds` (RUL-190). The engine does not sleep — Tick asks "has the
// resume instant passed?" — so with a tick period T the worst-case LATENESS of a
// resumption is T, and the right way to choose T is against the smallest
// duration an operator can actually author.
//
// That smallest duration is one second. At T=1s a `delay: 1` would resume
// anywhere in [1s, 2s): up to 100% late on the finest thing the contract can
// express, and visibly wrong on the kind of rule this exists for ("flash the
// screen off and back on"). At T=250ms the same resumption lands within a
// quarter of that unit — late by an amount below what an operator watching a TV
// can distinguish, on every duration the contract admits.
//
// Finer than that buys nothing a rule can observe. Nothing in rules/1 expresses
// a sub-second span, so the only effect of, say, a 10ms tick would be to take
// the engine lock a hundred times a second and contend that much harder with the
// observation path (see automationhost.Host.Tick's locking note) for latency no
// contract can see.
//
// The cost at 250ms is four lock-acquire-and-scan cycles per second. A tick with
// nothing due is a walk over the loaded rules' triggers doing integer
// comparisons against the monotonic clock, with no I/O and no allocation — next
// to the ECP state poll this same binary runs every 5s over real HTTP, it does
// not register on an appliance.
//
// Cadence bounds lateness, never correctness, and that holds for the schedule
// triggers too: engine.dispatchSchedule enumerates occurrences over the
// half-open interval (last tick, now], so a tick delayed by a slow device
// catches every instant that passed while it was blocked rather than dropping
// them.
const automationTickInterval = 250 * time.Millisecond

// clockFloorReader is the narrow slice of internal/relay/identity.Store the
// schedule-floor seed reads: the verified clock floor the clock-trust controller
// persists (REL-130/132). *identity.Store satisfies it; a fake satisfies it in
// tests. It mirrors clocktrust.FloorStore, minus the write half this has no
// business performing.
type clockFloorReader interface {
	ClockFloor() (ms int64, ok bool, err error)
}

// seedScheduleFloor hands the engine its schedule resume cursor before anything
// ticks (RUL-370), and returns the instant it seeded.
//
// This is the safety belt on the drive loop above rather than a feature of its
// own. The first Tick after a start treats everything between the floor and now
// as downtime to be caught up through each schedule trigger's misfire policy; an
// unseeded floor is zero, so that span starts at the Unix epoch and the
// enumerators walk it day by day with no cap (automationhost.Host.
// SeedScheduleFloor documents the measured cost). Nothing called Tick before, so
// nothing ever paid it. Wiring the loop without wiring this would trade one
// dead mechanism for a boot stall.
//
// The value is the relay's PERSISTED verified clock floor when it has one — the
// same one internal/relay/clocktrust.Controller advances from a verified time —
// which makes the resume span the relay's actual downtime and the catch-up its
// actual missed occurrences.
//
// With no persisted floor (a relay that has never had a verified time) it seeds
// the BOOT WALL INSTANT, and that is a deliberate reading of RUL-350 rather than
// a convenient default: a missed occurrence is one "the evaluating engine was
// unable to evaluate at its scheduled instant", and an engine that has never run
// on this box was not unable to evaluate 2019 — it did not exist. Pre-history is
// not downtime. The alternative reading, that every occurrence since the epoch is
// owed a catch-up, is the one that enumerates thirty million instants at boot.
//
// A store that cannot answer is treated as having no floor and is LOGGED, not
// fatal: a relay whose operational store is degraded should still drive its
// rules, and the cost of guessing wrong here is a missed catch-up rather than a
// wrong command.
func seedScheduleFloor(host *automationhost.Host, floors clockFloorReader, bootWallMs int64) int64 {
	floorMs, ok, err := floors.ClockFloor()
	switch {
	case err != nil:
		log.Printf("waiveo-relay: reading the persisted clock floor failed (%v); seeding the engine's schedule resume cursor at this boot instant — occurrences missed before now are not caught up (RUL-350/370)", err)
		floorMs = bootWallMs
	case !ok:
		// No verified time has ever been applied on this relay.
		floorMs = bootWallMs
	}
	host.SeedScheduleFloor(floorMs)
	return floorMs
}

// startAutomationDriveLoops starts BOTH of the edge engine's drive loops for the
// life of ctx: the OBSERVATION loop (Host.Run over src, the ECP state poller's
// stream) and the TIME loop (Host.TickLoop at automationTickInterval). It is the
// one place the running binary turns the loaded engine into a running engine.
//
// The two are started TOGETHER, by one named function, because they are two
// halves of one thing and shipping one without the other is not a degraded
// engine — it is a silently broken one. That is not hypothetical: the
// observation half was wired as an anonymous goroutine in main and the time half
// was never wired at all, so every mechanism whose readiness is a matter of time
// passing was dead in production while `go build`, the whole test suite and
// validate-deadcode stayed green. A rule with a `delay` ran its head and never
// its tail; a `for`-hold never elapsed; a `time`/`sun` trigger never fired.
// engine.Tick had unit tests proving it works and no caller, which is the
// signature defect this repo keeps rediscovering.
//
// Being a named function with positional collaborators is what lets a test pin
// the WIRING rather than only the mechanism — automationdrive_test.go's
// TestMainStartsTheAutomationDriveLoops, following
// TestMainStartsTheDevicePlaneSync and cmd/waiveo-feeder's
// TestMainStartsTheConsoleBinding. See startDevicePlaneSync's own doc for why
// that second test exists at all.
//
// Lifecycle: both goroutines belong to ctx (rootCtx in main, the context that
// governs every long-lived loop this process starts) and stop with it. The
// ticker is owned HERE and stopped on the way out of its own goroutine, so it
// releases with the loop rather than leaking for the life of the process.
func startAutomationDriveLoops(
	ctx context.Context,
	host *automationhost.Host,
	src automationhost.DeviceStateSource,
	tickEvery time.Duration,
) {
	go func() {
		if err := host.Run(ctx, src); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("waiveo-relay: device-state drive loop ended: %v", err)
		}
	}()

	go func() {
		ticker := time.NewTicker(tickEvery)
		defer ticker.Stop()
		host.TickLoop(ctx, ticker.C)
	}()
}
