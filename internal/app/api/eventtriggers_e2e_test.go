package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/events"
)

// eventtriggers_e2e_test.go is the END-TO-END proof of the last hop in the
// interactive-slide chain: a durable `screen.interaction` event delivered to the
// app peer actually RUNS an automation, and what that automation does actually
// changes what a screen shows.
//
// The chain in full is: a viewer presses a focused layer on the TV -> the player
// POSTs /player/v1/interaction -> the relay records an events/1
// `screen.interaction` in its telemetry buffer -> the telemetry channel pushes
// it -> the app's ingest validates and appends it -> THIS -> the rule's actions
// run for real. The relay half is proved in internal/relay/playerserver's own
// interaction tests; this file proves the app half, over the real HTTP handler
// and the real projection a screen is served from.
//
// It exists because the `event` trigger was, until now, the purest instance of
// this codebase's recurring defect: a member of rules/1's closed vocabulary that
// an author could write, the compiler accepted, the store persisted — and
// nothing anywhere ever fired.

// mintEventAutomation creates an automation whose trigger is an `event` trigger
// on screen.interaction, optionally constrained by `match`, with one signage
// action.
func mintEventAutomation(t *testing.T, e *testEnv, node string, enabled bool, match map[string]any, action map[string]any) string {
	t.Helper()
	trigger := map[string]any{"type": "event", "event": "screen.interaction"}
	if match != nil {
		trigger["match"] = match
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", mustJSON(t, map[string]any{
		"name":       "Interaction Automation",
		"scope_node": node,
		"enabled":    enabled,
		"mode":       "single",
		"triggers":   []any{trigger},
		"actions":    []any{action},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create event automation: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// interactionEnvelope is one delivered durable event carrying a press.
func interactionEnvelope(t *testing.T, screenID, interaction string) events.Envelope {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"screen_id":   screenID,
		"interaction": interaction,
		"lease_id":    "lease-1",
		"at":          fixedNowMs,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return events.Envelope{
		ID:             "01J8Z3K4N5P6Q7R8S9T0V1W2Z5",
		Schema:         events.SchemaScreenInteraction,
		TS:             fixedNowMs,
		ScopeNode:      "01J8Z3K4N5P6Q7R8S9T0V1W2Z3",
		TraceID:        "01J8Z3K4N5P6Q7R8S9T0V1W2Z4",
		Origin:         "relay",
		CostClass:      "telemetry",
		RetentionClass: "telemetry-standard",
		Payload:        payload,
	}
}

// TestScreenInteractionEventFiresAnAutomationThatChangesAScreen is the whole
// point of the ping layer: somebody presses a button on a wall panel and a
// screen changes.
//
// The assertion is made against the real projection (screenProgramOf), not
// against the rule's own report — a report saying "ran" while the screen showed
// the same thing is exactly the half-proof this repo keeps shipping.
func TestScreenInteractionEventFiresAnAutomationThatChangesAScreen(t *testing.T) {
	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher))
	node := e.placementNode(t)

	screenID := mintSignageScreen(t, e, node, "Lobby A")
	castID := mintSignageCast(t, e, node, "Service Requested")

	if before := screenProgramOf(t, e, screenID, fixedNowMs); before.Display != "blank" {
		t.Fatalf("screen starts at %q, want the terminal blank", before.Display)
	}

	mintEventAutomation(t, e, node, true,
		map[string]any{"interaction": "call_service"},
		map[string]any{"type": "play_cast", "screen_id": screenID, "cast_id": castID})

	dispatcher.Deliver(context.Background(), interactionEnvelope(t, screenID, "call_service"))

	after := screenProgramOf(t, e, screenID, fixedNowMs)
	if after.Display != "content" {
		t.Fatalf("after the press the screen shows %q, want content — the automation did not act", after.Display)
	}
	if len(after.Content) != 1 || after.Content[0].ContentType != "slide" {
		t.Fatalf("after the press: content = %+v, want the cast's one slide", after.Content)
	}
	if !after.Pinned {
		t.Error("the override is not pinned, so a relay re-resolving its schedule would revert it within 30s")
	}
}

// TestEventTriggerHonoursTheMatchConstraint: RUL-081 evaluates `match` against
// the event's payload, so a rule watching for one button must not fire on
// another. Without it every interactive layer on the site fires every
// interaction rule — which is worse than nothing firing, because it is the kind
// of wrong that only shows up once a second button exists.
func TestEventTriggerHonoursTheMatchConstraint(t *testing.T) {
	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher))
	node := e.placementNode(t)

	screenID := mintSignageScreen(t, e, node, "Lobby A")
	castID := mintSignageCast(t, e, node, "Service Requested")

	mintEventAutomation(t, e, node, true,
		map[string]any{"interaction": "call_service"},
		map[string]any{"type": "play_cast", "screen_id": screenID, "cast_id": castID})

	// A DIFFERENT button on the same screen.
	dispatcher.Deliver(context.Background(), interactionEnvelope(t, screenID, "other_button"))

	if after := screenProgramOf(t, e, screenID, fixedNowMs); after.Display != "blank" {
		t.Fatalf("a press of a different button changed the screen to %q; the match constraint is not evaluated", after.Display)
	}
}

// TestEventTriggerIgnoresADisabledAutomation is the single worst bug this path
// could have: an operator disables a rule precisely to stop it acting, and a
// firing path that ignored the flag would act anyway, with the console still
// showing the rule as disabled.
func TestEventTriggerIgnoresADisabledAutomation(t *testing.T) {
	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher))
	node := e.placementNode(t)

	screenID := mintSignageScreen(t, e, node, "Lobby A")
	castID := mintSignageCast(t, e, node, "Service Requested")

	mintEventAutomation(t, e, node, false,
		map[string]any{"interaction": "call_service"},
		map[string]any{"type": "play_cast", "screen_id": screenID, "cast_id": castID})

	dispatcher.Deliver(context.Background(), interactionEnvelope(t, screenID, "call_service"))

	if after := screenProgramOf(t, e, screenID, fixedNowMs); after.Display != "blank" {
		t.Fatalf("a DISABLED automation acted (screen now %q)", after.Display)
	}
}

// TestEventTriggerIgnoresAnotherSchema: a rule watching screen.interaction must
// not fire on every event that happens to reach the log.
func TestEventTriggerIgnoresAnotherSchema(t *testing.T) {
	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher))
	node := e.placementNode(t)

	screenID := mintSignageScreen(t, e, node, "Lobby A")
	castID := mintSignageCast(t, e, node, "Service Requested")

	mintEventAutomation(t, e, node, true, nil,
		map[string]any{"type": "play_cast", "screen_id": screenID, "cast_id": castID})

	other := interactionEnvelope(t, screenID, "call_service")
	other.Schema = events.SchemaDeviceHeartbeat
	dispatcher.Deliver(context.Background(), other)

	if after := screenProgramOf(t, e, screenID, fixedNowMs); after.Display != "blank" {
		t.Fatalf("a device.heartbeat fired a screen.interaction rule (screen now %q)", after.Display)
	}
}

// TestEventTriggerMatchOnAnAbsentFieldDoesNotFire: an absent field is not a
// satisfied constraint. Ignoring it would make a typo'd field name silently
// widen a rule to EVERY event of that schema — the opposite of what the author
// wrote, and undetectable until two buttons exist.
func TestEventTriggerMatchOnAnAbsentFieldDoesNotFire(t *testing.T) {
	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher))
	node := e.placementNode(t)

	screenID := mintSignageScreen(t, e, node, "Lobby A")
	castID := mintSignageCast(t, e, node, "Service Requested")

	mintEventAutomation(t, e, node, true,
		map[string]any{"interation": "call_service"}, // note the typo
		map[string]any{"type": "play_cast", "screen_id": screenID, "cast_id": castID})

	dispatcher.Deliver(context.Background(), interactionEnvelope(t, screenID, "call_service"))

	if after := screenProgramOf(t, e, screenID, fixedNowMs); after.Display != "blank" {
		t.Fatalf("a match on a field the payload does not carry fired anyway (screen now %q)", after.Display)
	}
}

// TestUnboundDispatcherIsInert: a dispatcher nothing bound must do nothing
// rather than panic. Every test that builds a bare handler is in that state, and
// so is any deployment that forgets the option — the honest degrade is silence,
// and the wiring makes it transient (cmd/waiveo-feeder binds it during startup).
func TestUnboundDispatcherIsInert(t *testing.T) {
	var d api.EventTriggerDispatcher
	d.Deliver(context.Background(), events.Envelope{Schema: events.SchemaScreenInteraction})
}
