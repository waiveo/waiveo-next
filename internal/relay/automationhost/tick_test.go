package automationhost

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// syncClock is a clock.FakeClock made safe for the one test that reads it from a
// TickLoop goroutine while the test goroutine advances it. FakeClock itself has
// no locking (it is built for single-goroutine engine tests), and `go test -race`
// is a gate here.
type syncClock struct {
	mu sync.Mutex
	c  *clock.FakeClock
}

func newSyncClock() *syncClock { return &syncClock{c: clock.NewFakeClock()} }

func (s *syncClock) WallMillis() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.WallMillis()
}

func (s *syncClock) Mono() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.Mono()
}

func (s *syncClock) Advance(ms int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.Advance(ms)
}

func (s *syncClock) SetWall(ms int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.SetWall(ms)
}

// delayedRule is the shape of the defect this file exists for, and the shape the
// automations builder offers an operator first: "when the screen turns on, launch
// the dev channel, wait 30 seconds, then send it home" (RUL-190). Its head runs
// from the triggering observation; its TAIL exists only as the rule's in-flight
// run until something advances the clock.
const delayedRule = `{
	"id": "01J8Z3K4N5P6Q7R8S9T0V1DLY1",
	"mode": "single",
	"triggers": [{"type":"state","entity_id":"` + testScreenEntity + `","to":["on"]}],
	"conditions": [],
	"actions": [
		{"type":"device_command","entity_id":"` + testScreenEntity + `","command":"launch","params":{"channel":"dev"}},
		{"type":"delay","duration_seconds":30},
		{"type":"device_command","entity_id":"` + testScreenEntity + `","command":"home"}
	]
}`

// heldRule is a bounded `for`-hold (RUL-024): the screen must have been ON
// continuously for five seconds before the rule fires. Nothing observes the
// entity again during those five seconds — which is the whole point, and exactly
// what Observe alone cannot advance.
const heldRule = `{
	"id": "01J8Z3K4N5P6Q7R8S9T0V1HLD1",
	"mode": "single",
	"triggers": [{"type":"state","entity_id":"` + testScreenEntity + `","to":["on"],"for":5}],
	"conditions": [],
	"actions": [{"type":"device_command","entity_id":"` + testScreenEntity + `","command":"home"}]
}`

// newFakeClockHost builds a Host whose engine reads a steppable clock, so a
// 30-second delay costs a test no seconds at all.
func newFakeClockHost(t *testing.T, controller deviceplane.DeviceController) (*Host, *syncClock, *identity.Store) {
	t.Helper()
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	clk := newSyncClock()
	host, err := newWithClock(store, deviceclass.Builtin(), controller, testResolver, testRelayID, clk)
	if err != nil {
		t.Fatalf("newWithClock: %v", err)
	}
	return host, clk, store
}

// observeScreenOn drives the baseline-then-transition pair that fires a
// to:["on"] state trigger.
func observeScreenOn(t *testing.T, host *Host) {
	t.Helper()
	reg := registry.FixtureRegistry{}
	if _, err := host.Observe(state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, "off"))); err != nil {
		t.Fatalf("Observe (baseline): %v", err)
	}
	if _, err := host.Observe(state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, "on"))); err != nil {
		t.Fatalf("Observe (screen-on): %v", err)
	}
}

// TestTickResumesADelayedRuleTail is the direct regression for the P1 this file
// closes: a triggered rule with a `delay` ran its head and never its tail,
// because engine.resumeDelay is reachable only from engine.Tick and nothing in
// production called Tick. An operator authoring "turn on, wait 30s, turn off" —
// the delay the automations builder offers — got a device that turned on and
// stayed on for the life of the relay process.
//
// The assertions are ordered so a partial fix cannot pass: the tail must be
// absent before the delay elapses (a Tick that resumed unconditionally would be
// just as wrong as no Tick at all) and present after.
func TestTickResumesADelayedRuleTail(t *testing.T) {
	controller := &recordController{}
	host, clk, _ := newFakeClockHost(t, controller)
	if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(delayedRule)}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}

	observeScreenOn(t, host)

	// Head only: the launch dispatched, the delay parked the tail.
	if got := controller.commands(); len(got) != 1 || got[0] != testScreenEntity+"/launch" {
		t.Fatalf("after the firing observation the device saw %v, want exactly the head command (launch)", got)
	}

	// 29 seconds in, ticking hard: the tail must NOT resume early. RUL-190 tracks
	// the remaining time on MONOTONIC time, and a tick is a question, not a
	// release.
	clk.Advance(29_000)
	for i := 0; i < 4; i++ {
		if _, err := host.Tick(); err != nil {
			t.Fatalf("Tick (pre-elapse): %v", err)
		}
	}
	if got := controller.commands(); len(got) != 1 {
		t.Fatalf("at t+29s the device saw %v, want the tail still pending — a delay must not resume early", got)
	}

	// Past 30 seconds: the next tick resumes the tail and the `home` command
	// reaches the device.
	clk.Advance(1_500)
	if _, err := host.Tick(); err != nil {
		t.Fatalf("Tick (post-elapse): %v", err)
	}
	got := controller.commands()
	if len(got) != 2 || got[1] != testScreenEntity+"/home" {
		t.Fatalf("after the delay elapsed the device saw %v, want [%s/launch %s/home] — the delayed tail never resumed",
			got, testScreenEntity, testScreenEntity)
	}

	// And it resumes ONCE: further ticks must not re-run a completed tail.
	for i := 0; i < 4; i++ {
		if _, err := host.Tick(); err != nil {
			t.Fatalf("Tick (post-resume): %v", err)
		}
	}
	if got := controller.commands(); len(got) != 2 {
		t.Fatalf("after four further ticks the device saw %v, want the tail dispatched exactly once", got)
	}
}

// TestTickResumesADelayedTailWithNoFurtherObservation pins the reason Observe
// cannot substitute for Tick on this path. It is the same rule as above, driven
// with the entity re-observed at its unchanged state throughout the delay — the
// ordinary thing a 5-second ECP poll does. Observation traffic alone must not
// resume the tail, and after it a single Tick must.
//
// Without this, a repair that resumed delays from Observe would pass the test
// above (whose 29s window sees no observations) while still leaving a delayed
// rule stranded on any entity nothing happens to poll.
func TestTickResumesADelayedTailWithNoFurtherObservation(t *testing.T) {
	controller := &recordController{}
	host, clk, _ := newFakeClockHost(t, controller)
	if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(delayedRule)}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}
	observeScreenOn(t, host)

	// Poll the entity six times across a minute of monotonic time, reporting the
	// same "on" it already reported. The delay's 30 seconds elapse during this.
	reg := registry.FixtureRegistry{}
	for i := 0; i < 6; i++ {
		clk.Advance(10_000)
		if _, err := host.Observe(state.NewObservation(reg, ent(testScreenEntity, "on"), ent(testScreenEntity, "on"))); err != nil {
			t.Fatalf("Observe (unchanged poll %d): %v", i, err)
		}
	}
	if got := controller.commands(); len(got) != 1 {
		t.Fatalf("after a minute of unchanged observations the device saw %v, want only the head — observations do not resume a delay", got)
	}

	if _, err := host.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := controller.commands(); len(got) != 2 || got[1] != testScreenEntity+"/home" {
		t.Fatalf("device saw %v after one Tick, want the delayed tail dispatched", got)
	}
}

// TestTickElapsesAForHold covers the second Tick-only mechanism (RUL-024):
// engine.stepTriggerTick runs only from Tick, and Observe steps only the
// triggers whose subject is the entity just observed. A hold that needs time to
// pass with no further observation of that entity therefore never elapses
// without a tick — "the screen has been on for five minutes" is exactly that
// shape.
func TestTickElapsesAForHold(t *testing.T) {
	controller := &recordController{}
	host, clk, _ := newFakeClockHost(t, controller)
	if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(heldRule)}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}

	observeScreenOn(t, host)
	if got := controller.commands(); len(got) != 0 {
		t.Fatalf("the arming observation dispatched %v, want nothing — a `for` hold must not fire on the edge itself", got)
	}

	// Four seconds of the five-second hold, with no further observation.
	clk.Advance(4_000)
	if _, err := host.Tick(); err != nil {
		t.Fatalf("Tick (pre-elapse): %v", err)
	}
	if got := controller.commands(); len(got) != 0 {
		t.Fatalf("at t+4s of a for:5 hold the device saw %v, want nothing", got)
	}

	clk.Advance(1_500)
	disps, err := host.Tick()
	if err != nil {
		t.Fatalf("Tick (post-elapse): %v", err)
	}
	if len(disps) != 1 {
		t.Fatalf("the elapsing tick produced %d disposition(s), want 1", len(disps))
	}
	if got := controller.commands(); len(got) != 1 || got[0] != testScreenEntity+"/home" {
		t.Fatalf("device saw %v after the hold elapsed, want one home — a `for` hold never elapses without a tick", got)
	}
}

// TestTickRecordsAutomationRunTelemetry pins the disposition disposal question.
// Observe's dispositions become durable automation.run entries (REL-090/093,
// EVT-040/041) and Tick's must go to the same place: a firing is a firing
// whatever advanced the engine, and an operator reading the event log has no way
// to know a rule fired on a timer rather than on an observation unless it is
// recorded. A disposition dropped on the floor is the same class of defect as an
// engine nothing drives.
//
// The `for`-hold rule is used because it produces a disposition from a TICK
// alone — the observation that arms it dispatches nothing and records nothing —
// so exactly one durable entry proves the tick recorded it.
func TestTickRecordsAutomationRunTelemetry(t *testing.T) {
	controller := &recordController{}
	host, clk, store := newFakeClockHost(t, controller)
	if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(heldRule)}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}
	observeScreenOn(t, host)

	entries, _, err := store.LoadTelemetry()
	if err != nil {
		t.Fatalf("store.LoadTelemetry (before the hold elapses): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("durable telemetry has %d entries before the hold elapsed, want 0", len(entries))
	}

	clk.Advance(6_000)
	if _, err := host.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	assertOneDurableAutomationRun(t, store)
}

// TestTickDoesNotRecordASecondRunForAResumedDelay is the other half of the
// disposition question, and the reason "record everything Tick returns" is not
// automatically right. A resumed `delay` tail yields NO disposition by design —
// the firing that scheduled it already recorded `ran` — so the completed run
// must appear in the event log exactly once, not twice.
func TestTickDoesNotRecordASecondRunForAResumedDelay(t *testing.T) {
	controller := &recordController{}
	host, clk, store := newFakeClockHost(t, controller)
	if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(delayedRule)}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}
	observeScreenOn(t, host)
	assertOneDurableAutomationRun(t, store) // the firing itself

	clk.Advance(31_000)
	if _, err := host.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := controller.commands(); len(got) != 2 {
		t.Fatalf("device saw %v, want the tail resumed", got)
	}
	assertOneDurableAutomationRun(t, store) // still exactly one — no double count
}

// TestTickFiresAWallClockScheduleTrigger covers the mechanism the trace of this
// defect did not name but that Tick is equally the sole driver of: `time`,
// `time_pattern` and `sun` triggers carry no EntityRef and fire only through
// engine.dispatchSchedule, which runs only from Tick (RUL-040/041/340). Until
// this loop existed, "turn the sign on at 08:00" never fired on a relay at all.
//
// The first tick after a (re)start is a RESUME, not a live tick — it catches the
// downtime span up through each trigger's misfire policy, and the default is
// `skip` (RUL-350/354), so it fires nothing and simply establishes the cursor.
// The second tick, with the wall clock stepped past the scheduled instant, is
// live.
func TestTickFiresAWallClockScheduleTrigger(t *testing.T) {
	controller := &recordController{}
	host, clk, _ := newFakeClockHost(t, controller)
	if err := host.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}

	// 2026-08-12T07:59:30Z, half a minute before the scheduled 08:00:00.
	const beforeMs = 1786521570_000
	clk.SetWall(beforeMs)

	scheduledRule := `{
		"id": "01J8Z3K4N5P6Q7R8S9T0V1SCH1",
		"mode": "single",
		"triggers": [{"type":"time","at":"08:00:00"}],
		"conditions": [],
		"actions": [{"type":"device_command","entity_id":"` + testScreenEntity + `","command":"launch"}]
	}`
	if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(scheduledRule)}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}

	// One poll of the screen, as the ECP poller makes at boot. A schedule trigger
	// has no entity subject, so this fires nothing — but the rule's action
	// dispatches to an entity, and a command is only allowed against a device
	// class the engine's snapshot knows (RUL-160). Without an observation the
	// firing is real and the dispatch is refused, which is what the relay does
	// for a device it has never seen.
	if _, err := host.Observe(state.NewObservation(registry.FixtureRegistry{}, ent(testScreenEntity, "off"), ent(testScreenEntity, "off"))); err != nil {
		t.Fatalf("Observe (poller seed): %v", err)
	}

	// Resume tick: establishes the wall cursor, fires nothing under the default
	// `skip` misfire policy.
	if _, err := host.Tick(); err != nil {
		t.Fatalf("Tick (resume): %v", err)
	}
	if got := controller.commands(); len(got) != 0 {
		t.Fatalf("the resume tick dispatched %v, want nothing (default misfire is skip, RUL-354)", got)
	}

	// Step the wall clock past 08:00:00 and tick live.
	clk.SetWall(beforeMs + 60_000)
	clk.Advance(60_000)
	if _, err := host.Tick(); err != nil {
		t.Fatalf("Tick (live): %v", err)
	}
	if got := controller.commands(); len(got) != 1 || got[0] != testScreenEntity+"/launch" {
		t.Fatalf("device saw %v after the scheduled instant passed, want one launch — a `time` trigger fires only from Tick", got)
	}
}

// TestSeedScheduleFloorBoundsTheFirstTickResumeSpan is the guard on the hazard
// wiring the tick loop ACTIVATES. The first Tick after a start treats the span
// from the schedule floor up to now as downtime and catches every occurrence in
// it up through the trigger's misfire policy. With the floor unseeded that span
// starts at the Unix epoch, and under `fire_each` the relay dispatches one
// firing per day since 1970 the instant it boots.
//
// engine.SeedScheduleFloor existed for precisely this and had no production
// caller, which cost nothing for as long as nothing called Tick.
func TestSeedScheduleFloorBoundsTheFirstTickResumeSpan(t *testing.T) {
	// 2026-08-12T07:59:30Z.
	const nowMs = 1786521570_000
	scheduledRule := `{
		"id": "01J8Z3K4N5P6Q7R8S9T0V1SCH2",
		"mode": "single",
		"triggers": [{"type":"time","at":"08:00:00","misfire":"fire_each"}],
		"conditions": [],
		"actions": [{"type":"device_command","entity_id":"` + testScreenEntity + `","command":"launch"}]
	}`

	newHost := func(t *testing.T) (*Host, *syncClock) {
		t.Helper()
		host, clk, _ := newFakeClockHost(t, &recordController{})
		if err := host.SetLocation("UTC", 0, 0); err != nil {
			t.Fatalf("SetLocation: %v", err)
		}
		clk.SetWall(nowMs)
		if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(scheduledRule)}, 1); err != nil {
			t.Fatalf("ApplyEdgeRules: %v", err)
		}
		return host, clk
	}

	// Unseeded: the resume span is (epoch, now] and `fire_each` catches up one
	// occurrence per day since 1970. This asserts the hazard is real rather than
	// theoretical — if the engine ever grows its own bound, this is the case that
	// says so and the seed can be retired.
	//
	// It goes through the engine rather than Host.Tick only to keep the case
	// fast: Host.Tick durably RECORDS every disposition, and twenty thousand
	// SQLite write-throughs take eight seconds. That is itself worth knowing —
	// an unseeded boot does not merely fire, it also floods a telemetry queue
	// whose capacity is 1024 — but the claim under test here is the size of the
	// enumerated span.
	unseeded, uclk := newHost(t)
	epochDisps := unseeded.engine.Tick(uclk)
	if len(epochDisps) < 10_000 {
		t.Fatalf("an unseeded first Tick produced %d catch-up firing(s); this case exists because that number is enormous", len(epochDisps))
	}

	// Seeded a day back, which is what a persisted clock floor means: only the
	// occurrences inside the actual downtime are caught up.
	seeded, _ := newHost(t)
	seeded.SeedScheduleFloor(nowMs - 24*60*60*1000)
	disps, err := seeded.Tick()
	if err != nil {
		t.Fatalf("Tick (seeded): %v", err)
	}
	if len(disps) != 1 {
		t.Fatalf("a first Tick seeded one day back produced %d catch-up firing(s), want exactly the one 08:00 occurrence inside that day", len(disps))
	}
}

// TestTickLoopDrivesTickOnEveryDeliveredTick proves the LOOP, not just the step:
// TickLoop must call Tick once per tick delivered on its injected channel, and
// must stop when its context is cancelled. The cadence is injected, so this test
// sleeps on nothing.
//
// A loop whose body did not call Tick — or that read the channel and dropped it
// — would leave the delayed tail unfired here, which is the production failure
// one level down from the one above.
func TestTickLoopDrivesTickOnEveryDeliveredTick(t *testing.T) {
	controller := &recordController{}
	host, clk, _ := newFakeClockHost(t, controller)
	if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(delayedRule)}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}
	observeScreenOn(t, host)
	clk.Advance(31_000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		host.TickLoop(ctx, ticks)
	}()

	// One tick delivered; the loop's Tick must resume the tail. Sending a second
	// tick is what proves the first was CONSUMED and processed rather than
	// buffered: this channel is unbuffered, so the second send cannot complete
	// until the loop comes back round for it.
	ticks <- time.Now()
	ticks <- time.Now()

	if got := controller.commands(); len(got) != 2 || got[1] != testScreenEntity+"/home" {
		t.Fatalf("device saw %v after two delivered ticks, want the delayed tail resumed by the loop", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("TickLoop did not return within 5s of its context being cancelled; the relay's time drive must stop cleanly with the binary")
	}
}

// TestTickLoopReturnsWhenItsTickChannelCloses covers the loop's other exit. The
// binary owns the ticker and stops it on the way out, and a closed channel must
// end the loop rather than spin it — a `for range` over a closed channel that
// was written as a bare receive is a busy loop that pins a core on an appliance.
func TestTickLoopReturnsWhenItsTickChannelCloses(t *testing.T) {
	host, _, _ := newFakeClockHost(t, &recordController{})

	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		host.TickLoop(context.Background(), ticks)
	}()

	close(ticks)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("TickLoop did not return when its tick channel closed")
	}
}

// TestTickRacesObserveAndApply is the concurrency guard for the new caller of
// h.mu, in the shape TestApplyEdgeRulesRacesClockTrust already established for
// the last one. The running binary now advances the SAME unsynchronized
// engine.Engine from a ticker goroutine, the ECP poll loop and the desired-state
// re-pull loop at once; all three must serialize on Host's lock. The assertion
// is the race detector.
//
// A clean -race report proves memory safety and nothing about ordering, so this
// case is deliberately not the only guard here — the ordering claims are the
// deterministic single-goroutine cases above.
func TestTickRacesObserveAndApply(t *testing.T) {
	host, clk, _ := newFakeClockHost(t, &recordController{})
	rules := []json.RawMessage{json.RawMessage(delayedRule)}
	if err := host.ApplyEdgeRules(rules, 1); err != nil {
		t.Fatalf("ApplyEdgeRules (seed): %v", err)
	}

	const iterations = 300
	reg := registry.FixtureRegistry{}

	var wg sync.WaitGroup
	wg.Add(3)

	go func() { // the ticker goroutine
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			clk.Advance(10)
			if _, err := host.Tick(); err != nil {
				t.Errorf("Tick: %v", err)
				return
			}
		}
	}()
	go func() { // the ECP poll loop
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := host.Observe(state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, "on"))); err != nil {
				t.Errorf("Observe: %v", err)
				return
			}
			// EntityState is the non-blocking Lease-issuance read; it must stay
			// concurrent with all of this, on stateMu rather than mu.
			host.EntityState(testScreenEntity)
		}
	}()
	go func() { // the desired-state re-pull loop
		defer wg.Done()
		for gen := 2; gen < 2+iterations; gen++ {
			if err := host.ApplyEdgeRules(rules, gen); err != nil {
				t.Errorf("ApplyEdgeRules(gen=%d): %v", gen, err)
				return
			}
		}
	}()

	wg.Wait()
}
