package engine

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// TestParamsExpressionLiveEvaluatedAtDispatch is the Task-4 end-to-end proof that
// an action's `params` Expression is live-evaluated at the moment the action runs
// (RUL-393), reading the current snapshot through the real expr evaluator the
// engine wires — never frozen into the compiled generation (RUL-390). The rule
// fires twice on the same off->on transition with a different `volume` attribute
// each time; each dispatch carries the value live at that instant.
func TestParamsExpressionLiveEvaluatedAtDispatch(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	entry, rule := compileRule(t, `{
		"id":"01J8Z3K4N5P6Q7R8S9T0V1RULE",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"]}],
		"actions":[
			{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power",
			 "params":{"vol":{"expr":"attr('`+subjectEntity+`','volume') | int"}}}
		]
	}`)
	if entry.ExecutionClass != "edge" {
		t.Fatalf("rule should classify edge, got %q", entry.ExecutionClass)
	}

	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}

	off := func(vol float64) state.Entity { return entAttr(subjectEntity, "off", map[string]any{"volume": vol}) }
	on := func(vol float64) state.Entity { return entAttr(subjectEntity, "on", map[string]any{"volume": vol}) }

	// Prime baseline (off) — no fire.
	if d := e.Observe(state.NewObservation(reg, off(10), off(10))); len(d) != 0 {
		t.Fatalf("priming observation fired: %+v", d)
	}
	// off -> on with volume 30: fires, dispatches the live value 30.
	if d := e.Observe(state.NewObservation(reg, off(10), on(30))); len(d) != 1 || d[0].Disposition != Ran {
		t.Fatalf("first transition dispositions = %+v, want one ran", d)
	}
	// on -> off (leaves the `on` group, does not fire).
	if d := e.Observe(state.NewObservation(reg, on(30), off(40))); len(d) != 0 {
		t.Fatalf("leaving on fired: %+v", d)
	}
	// off -> on with volume 55: fires again, dispatches the NEW live value 55.
	if d := e.Observe(state.NewObservation(reg, off(40), on(55))); len(d) != 1 || d[0].Disposition != Ran {
		t.Fatalf("second transition dispositions = %+v, want one ran", d)
	}

	if len(sink.calls) != 2 {
		t.Fatalf("sink calls = %d, want 2 (%+v)", len(sink.calls), sink.calls)
	}
	if got := sink.calls[0].Params["vol"]; got != float64(30) {
		t.Errorf("first dispatch params[vol] = %v, want 30 (live at first dispatch)", got)
	}
	if got := sink.calls[1].Params["vol"]; got != float64(55) {
		t.Errorf("second dispatch params[vol] = %v, want 55 (live at second dispatch — not frozen)", got)
	}
}

// TestParamsExpressionSourcingTriggerSubjectInScope proves the engine supplies
// the firing trigger's subject entity as the edge Expression scope (RUL-282): a
// `params` Expression sourcing the rule's own trigger subject resolves and
// dispatches (it would fail closed if the engine set the wrong scope entity).
func TestParamsExpressionSourcingTriggerSubjectInScope(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	entry, rule := compileRule(t, `{
		"id":"01J8Z3K4N5P6Q7R8S9T0V1RULE",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"]}],
		"actions":[
			{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power",
			 "params":{"st":{"expr":"state('`+subjectEntity+`') | upper"}}}
		]
	}`)

	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off"))); len(d) != 0 {
		t.Fatalf("priming observation fired: %+v", d)
	}
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))

	if len(sink.calls) != 1 {
		t.Fatalf("sink calls = %d, want 1 (in-scope source must dispatch)", len(sink.calls))
	}
	if got := sink.calls[0].Params["st"]; got != "ON" {
		t.Errorf("params[st] = %v, want ON (upper of the live in-scope state)", got)
	}
}
