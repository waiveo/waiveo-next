package engine

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// --- Task 9: single / restart modes -> RunDisposition ----------------------

// fireInto drives a genuine off->motion transition so the `to:["motion"]` state
// trigger fires; it first leaves motion (motion->off) so the re-entry is a real
// transition, not a same-state no-op (RUL-020/300). It returns the dispositions
// produced by the firing observation.
func fireIntoMotion(t *testing.T, e *Engine, reg registry.Registry) []RunDisposition {
	t.Helper()
	// Leave motion (no fire — the trigger only fires on entry into motion).
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "motion"), ent(subjectEntity, "off")))
	// Re-enter motion: a genuine transition that fires the trigger.
	return e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "motion")))
}

// TestModeSingleDropsConcurrentFire is the RUL-241 corpus case
// (RUL-241-mode-single-drops-concurrent-fire): under the default single mode, a
// firing while a run is in progress is dropped and recorded `skipped`; the
// original in-progress run is unaffected (RUL-241/246).
func TestModeSingleDropsConcurrentFire(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	entry, rule := compileRule(t, `{
		"id":"01J8Z3K4N5P6Q7R8S9T0V1RULD",
		"mode":"single",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["motion"]}],
		"actions":[
			{"type":"delay","duration_seconds":20},
			{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}
		]
	}`)
	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Prime baseline off so the first entry into motion is a genuine transition.
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))

	// seq 1, t=0: fire run-1 -> ran, delay pending.
	seq1 := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "motion")))
	if len(seq1) != 1 || seq1[0].Disposition != Ran {
		t.Fatalf("seq1: want one ran, got %+v", seq1)
	}
	if seq1[0].Mode != "single" {
		t.Fatalf("seq1: mode = %q, want single", seq1[0].Mode)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("seq1: command dispatched before delay elapsed: %+v", sink.calls)
	}

	// seq 2, t=5000: fire again while run-1 is mid-delay -> single drops -> skipped.
	clk.Advance(5_000)
	seq2 := fireIntoMotion(t, e, reg)
	if len(seq2) != 1 || seq2[0].Disposition != Skipped {
		t.Fatalf("seq2: want one skipped, got %+v", seq2)
	}
	if seq2[0].Mode != "single" || seq2[0].RuleID != "01J8Z3K4N5P6Q7R8S9T0V1RULD" {
		t.Fatalf("seq2: unexpected disposition fields %+v", seq2[0])
	}

	// run-1 completes uninterrupted: its 20s delay resolves at mono=20000 and
	// dispatches exactly once (the skipped firing started no second run).
	clk.Advance(15_000) // mono now 20_000
	e.Tick(clk)
	if len(sink.calls) != 1 {
		t.Fatalf("want exactly one dispatch from the uninterrupted run-1, got %d (%+v)", len(sink.calls), sink.calls)
	}
	if sink.calls[0].EntityID != subjectEntity || sink.calls[0].Command != "power" {
		t.Fatalf("run-1 dispatched %+v", sink.calls[0])
	}
}

// TestModeRestartCancelsInFlightDelay is the RUL-242 corpus case
// (RUL-242-mode-restart-cancels-in-flight-delay): under restart mode, a firing
// while a run is mid-delay cancels that run (recorded `restarted`), discards its
// pending delay, and starts a fresh run from the top whose delay timer starts at
// zero rather than resuming the canceled run's elapsed time (RUL-242/246).
func TestModeRestartCancelsInFlightDelay(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	entry, rule := compileRule(t, `{
		"id":"01J8Z3K4N5P6Q7R8S9T0V1RULE",
		"mode":"restart",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["motion"]}],
		"actions":[
			{"type":"delay","duration_seconds":10},
			{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}
		]
	}`)
	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))

	// seq 1, t=0: fire run-1 -> ran, delay pending resume at mono=10000.
	seq1 := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "motion")))
	if len(seq1) != 1 || seq1[0].Disposition != Ran {
		t.Fatalf("seq1: want one ran, got %+v", seq1)
	}

	// seq 2, t=4000: fire again while run-1 is mid-delay (4000/10000 elapsed).
	// restart cancels run-1 (-> restarted) and starts run-2 (-> ran) from zero.
	clk.Advance(4_000)
	seq2 := fireIntoMotion(t, e, reg)
	if len(seq2) != 2 {
		t.Fatalf("seq2: want two dispositions (restarted + ran), got %+v", seq2)
	}
	if seq2[0].Disposition != Restarted {
		t.Fatalf("seq2[0]: want restarted (the canceled run), got %q", seq2[0].Disposition)
	}
	if seq2[1].Disposition != Ran {
		t.Fatalf("seq2[1]: want ran (the fresh run), got %q", seq2[1].Disposition)
	}
	if seq2[0].Mode != "restart" || seq2[1].Mode != "restart" {
		t.Fatalf("seq2: mode fields %+v", seq2)
	}

	// The canceled run-1's original resume (mono=10000) MUST NOT fire: at mono=10000
	// nothing dispatches, because run-2's fresh 10s delay resumes at mono=14000.
	clk.Advance(6_000) // mono now 10_000
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("tick at mono=10000 produced dispositions: %+v", d)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("canceled run-1's delay dispatched at mono=10000: %+v", sink.calls)
	}

	// run-2's delay completes at mono=14000: exactly one dispatch, the fresh run's.
	clk.Advance(4_000) // mono now 14_000
	e.Tick(clk)
	if len(sink.calls) != 1 {
		t.Fatalf("want exactly one dispatch from run-2 at mono=14000, got %d (%+v)", len(sink.calls), sink.calls)
	}
	if sink.calls[0].Command != "power" {
		t.Fatalf("run-2 dispatched %+v", sink.calls[0])
	}
}

// TestModeSingleNoDedupAfterCompletion exercises RUL-245: rules/1 defines no
// refire-suppression beyond `for` and the mode rules. Once a run has completed,
// a fresh identical firing starts a new run and records another `ran` — it is
// NOT silently deduped as a repeat.
func TestModeSingleNoDedupAfterCompletion(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	// No delay: each firing's run completes synchronously within Observe, so the
	// next firing is never "while a run is in progress".
	entry, rule := compileRule(t, `{
		"id":"RULENODEDUP",
		"mode":"single",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["motion"]}],
		"actions":[{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}]
	}`)
	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))

	first := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "motion")))
	if len(first) != 1 || first[0].Disposition != Ran {
		t.Fatalf("first firing: want ran, got %+v", first)
	}
	second := fireIntoMotion(t, e, reg)
	if len(second) != 1 || second[0].Disposition != Ran {
		t.Fatalf("second identical firing: want ran (no dedup, RUL-245), got %+v", second)
	}
	if len(sink.calls) != 2 {
		t.Fatalf("want two dispatches (one per completed run), got %d (%+v)", len(sink.calls), sink.calls)
	}
}

// TestModeRestartFromTick verifies the mode logic also applies to a firing that
// arrives via a `for`-hold elapsing in Tick, not only via Observe: a held
// restart-mode trigger firing while a prior run is mid-delay cancels it
// (RUL-242) — proving the mode branch is shared by Observe and Tick.
func TestModeRestartFromTick(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	// A short-`for` trigger so a hold can elapse inside Tick, plus a long delay so
	// the first run is still in flight when the second firing lands.
	entry, rule := compileRule(t, `{
		"id":"RULERESTARTTICK",
		"mode":"restart",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["motion"],"for":5}],
		"actions":[
			{"type":"delay","duration_seconds":100},
			{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}
		]
	}`)
	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))

	// First firing: enter motion, hold 5s, fires via Tick -> run-1 (delay 100s).
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "motion")))
	clk.Advance(5_000)
	d1 := e.Tick(clk)
	if len(d1) != 1 || d1[0].Disposition != Ran {
		t.Fatalf("first hold fire: want ran, got %+v", d1)
	}

	// Second firing while run-1 is mid-delay: leave & re-enter motion, hold 5s,
	// then the hold elapses in Tick -> restart cancels run-1 -> [restarted, ran].
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "motion"), ent(subjectEntity, "off")))
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "motion")))
	clk.Advance(5_000)
	d2 := e.Tick(clk)
	if len(d2) != 2 || d2[0].Disposition != Restarted || d2[1].Disposition != Ran {
		t.Fatalf("second hold fire under restart: want [restarted, ran], got %+v", d2)
	}
}
