package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/events"
)

// eventtriggers_report_test.go pins the EVIDENCE half of an event-fired run.
//
// The scope bound (eventtriggers_scope_test.go) proves a run refuses a target it
// may not touch. That refusal being CORRECT and being VISIBLE are two different
// properties, and only the first was covered: the report an event-fired run
// assembles — the same one a hand-initiated run returns to its caller — was
// dropped, leaving a log line whose counts did not separate a performed target
// from a refused one. So a viewer pressed a button, the rule fired, the target
// was refused, the screen did not change, and there was nothing anywhere an
// operator could read.
//
// A silent refusal is not acceptable even with authoring tightened
// (TestAuthoringRefusesAnOutOfScopeActionTarget), because placement changes
// after authoring and the run is then the only thing that knows.

// runRecords returns every automation.run envelope the log holds, decoded.
func runRecords(t *testing.T, log *events.EventLog) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, env := range log.After("") {
		if env.Schema != events.SchemaAutomationRun {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			t.Fatalf("decode automation.run payload: %v", err)
		}
		payload["__origin"] = env.Origin
		payload["__scope_node"] = env.ScopeNode
		payload["__trace_id"] = env.TraceID
		out = append(out, payload)
	}
	return out
}

// TestAnEventFiredRunsRefusalIsRecordedWhereAnOperatorCanReadIt drives the whole
// visible chain: a rule authored in scope, its target moved out of scope, a
// press, and then the durable record that says what happened.
func TestAnEventFiredRunsRefusalIsRecordedWhereAnOperatorCanReadIt(t *testing.T) {
	log := events.NewEventLog(0)
	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher), api.WithRunEvents(log))
	nodeA := e.placementNode(t)
	e.seedPlacementNodes(t, eventScopeNodeB)

	moved := mintSignageScreen(t, e, nodeA, "Boardroom")
	castID := mintSignageCast(t, e, nodeA, "Service Requested")
	mintEventAutomation(t, e, nodeA, true,
		map[string]any{"interaction": "call_service"},
		map[string]any{"type": "play_cast", "screen_id": moved, "cast_id": castID})
	moveScreenToNode(t, e, moved, eventScopeNodeB)

	env := interactionEnvelope(t, moved, "call_service")
	dispatcher.Deliver(context.Background(), env)

	// The screen is unchanged — the refusal happened.
	if after := screenProgramOf(t, e, moved, fixedNowMs); after.Display != "blank" {
		t.Fatalf("the out-of-scope target was acted on (showing %q)", after.Display)
	}

	records := runRecords(t, log)
	if len(records) != 1 {
		t.Fatalf("the durable log holds %d automation.run record(s), want 1 — an event-fired run answers nobody, so this record is the only evidence it produced", len(records))
	}
	rec := records[0]

	if rec["__origin"] != "internal" {
		t.Errorf("automation.run origin = %v, want internal — an app-side evaluation is not the relay's (EVT-042)", rec["__origin"])
	}
	if rec["__scope_node"] != nodeA {
		t.Errorf("automation.run scope_node = %v, want the AUTOMATION's node %s — a subscriber scoped to the rule's node must see it (EVT-120)", rec["__scope_node"], nodeA)
	}
	if rec["__trace_id"] != env.TraceID {
		t.Errorf("automation.run trace_id = %v, want the triggering event's %s — the press and the run it caused correlate on it (EVT-010/API-063)", rec["__trace_id"], env.TraceID)
	}
	if rec["mode_disposition"] != "ran" {
		t.Errorf("mode_disposition = %v, want ran", rec["mode_disposition"])
	}

	outcomes, _ := rec["action_outcomes"].([]any)
	if len(outcomes) != 1 {
		t.Fatalf("action_outcomes = %+v, want exactly one entry — one target was attempted", rec["action_outcomes"])
	}
	o, _ := outcomes[0].(map[string]any)
	if o["screen_id"] != moved {
		t.Errorf("the outcome names screen %v, want the refused target %s", o["screen_id"], moved)
	}
	if ok, _ := o["ok"].(bool); ok {
		t.Fatalf("the refused target is recorded as ok — the record says the run performed work it refused: %+v", o)
	}
	msg, _ := o["error"].(string)
	if !strings.Contains(msg, "outside the scope this run acts within") {
		t.Errorf("the refusal reads %q; it must say WHY — a reason an operator can act on, not a bare failure "+
			"and not a false claim that the screen does not exist", msg)
	}

	// The envelope this produced must be one the durable log would actually
	// accept: a record that fails EVT-013 is a record nothing serves.
	for _, stored := range log.After("") {
		if stored.Schema != events.SchemaAutomationRun {
			continue
		}
		if err := events.Validate(stored); err != nil {
			t.Fatalf("the emitted automation.run does not satisfy events/1 validation: %v", err)
		}
	}
}

// TestAnEventFiredRunRecordsWhatItPerformed is the control, and it is the half a
// report that only ever recorded failures would fail: a run that did act must
// say so, with the same per-target shape, or the record is unreadable as
// evidence of anything.
func TestAnEventFiredRunRecordsWhatItPerformed(t *testing.T) {
	log := events.NewEventLog(0)
	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher), api.WithRunEvents(log))
	nodeA := e.placementNode(t)

	screenID := mintSignageScreen(t, e, nodeA, "Lobby A")
	castID := mintSignageCast(t, e, nodeA, "Service Requested")
	mintEventAutomation(t, e, nodeA, true,
		map[string]any{"interaction": "call_service"},
		map[string]any{"type": "play_cast", "screen_id": screenID, "cast_id": castID})

	dispatcher.Deliver(context.Background(), interactionEnvelope(t, screenID, "call_service"))

	records := runRecords(t, log)
	if len(records) != 1 {
		t.Fatalf("the durable log holds %d automation.run record(s), want 1", len(records))
	}
	outcomes, _ := records[0]["action_outcomes"].([]any)
	if len(outcomes) != 1 {
		t.Fatalf("action_outcomes = %+v, want one performed target", records[0]["action_outcomes"])
	}
	o, _ := outcomes[0].(map[string]any)
	if ok, _ := o["ok"].(bool); !ok {
		t.Fatalf("a target the run DID perform is recorded as failed: %+v", o)
	}
	if o["screen_id"] != screenID {
		t.Errorf("the outcome names screen %v, want %s", o["screen_id"], screenID)
	}
}

// TestAnUnwiredRunSinkIsSilentRatherThanFatal: a bare handler with no sink is a
// legitimate construction (every api test that does not care about the record
// builds one), and it must run rules exactly as it did before this record
// existed rather than nil-panicking on the first press.
func TestAnUnwiredRunSinkIsSilentRatherThanFatal(t *testing.T) {
	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher))
	nodeA := e.placementNode(t)

	screenID := mintSignageScreen(t, e, nodeA, "Lobby A")
	castID := mintSignageCast(t, e, nodeA, "Service Requested")
	mintEventAutomation(t, e, nodeA, true, nil,
		map[string]any{"type": "play_cast", "screen_id": screenID, "cast_id": castID})

	dispatcher.Deliver(context.Background(), interactionEnvelope(t, screenID, "call_service"))

	if after := screenProgramOf(t, e, screenID, fixedNowMs); after.Display != "content" {
		t.Fatalf("a handler with no run-event sink stopped running rules (screen shows %q)", after.Display)
	}
}
