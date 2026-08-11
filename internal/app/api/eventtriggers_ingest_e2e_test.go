package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/eventingest"
	"github.com/maaxton/waiveo-next/internal/app/eventingest/ingesttest"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// eventtriggers_ingest_e2e_test.go joins the two halves of the app peer's side
// of the interactive chain that were only ever tested apart: the TELEMETRY
// INGEST that receives a relay's push, and the `event`-trigger EVALUATOR that
// runs a rule from it.
//
// Everything either side of this seam was covered. internal/app/eventingest
// proves a pushed record becomes a durable envelope; eventtriggers_e2e_test.go
// proves a delivered envelope runs a rule and changes a screen — but it calls
// dispatcher.Deliver itself. So the hop that a real box depends on, the ingest
// handing an appended envelope to the dispatcher, was exercised by nothing. That
// hop lives in one argument of one constructor call in cmd/waiveo-feeder, and
// deleting it left the whole suite green.
//
// This drives the REAL ingest handler, over the real relay-mTLS identity check,
// with the deliverer wired exactly as the feeder wires it (the dispatcher's own
// Deliver method), and asserts on the projection a screen is actually served
// from. cmd/waiveo-feeder/eventtriggerwiring_test.go asserts main builds this
// same shape; between them, both halves of "does a press reach a rule?" have an
// answer.

// ingestSiteScope is the site scope-node ULID the ingest stamps onto every
// envelope it reconstructs. It is deliberately NOT one of the placement nodes
// these tests author rows at: an ingested event's scope_node is the site's, and
// nothing about rule matching may depend on it lining up with anything.
const ingestSiteScope = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"

// pushInteraction drives one `screen.interaction` telemetry record through the
// live ingest handler and returns the telemetry ack.
func pushInteraction(t *testing.T, h http.Handler, relay *ingesttest.Relay, seq int64, screenID, interaction string) telemetry.Ack {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"screen_id":   screenID,
		"interaction": interaction,
		"lease_id":    "lease-1",
		"at":          fixedNowMs,
	})
	if err != nil {
		t.Fatalf("marshal interaction payload: %v", err)
	}
	body, err := json.Marshal(telemetry.PushBatch{
		Entries:     []telemetry.Entry{{Seq: seq, Schema: events.SchemaScreenInteraction, Payload: payload}},
		LossMarkers: []telemetry.LossMarker{},
	})
	if err != nil {
		t.Fatalf("marshal push batch: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/telemetry/v1/push", bytes.NewReader(body))
	relay.Present(req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("telemetry push status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var ack telemetry.Ack
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatalf("decode ack: %v (body %s)", err, rec.Body.String())
	}
	return ack
}

// TestATelemetryPushOfAPressRunsTheAutomationAndChangesTheScreen is the whole
// app-side chain in one test: a relay pushes the press it recorded, the ingest
// validates and appends it, the dispatcher fires the rule, and the screen the
// rule names is serving different content afterwards.
func TestATelemetryPushOfAPressRunsTheAutomationAndChangesTheScreen(t *testing.T) {
	relay, err := ingesttest.NewRelay("01J8Z3K4N5P6Q7R8S9T0V1W2ZR")
	if err != nil {
		t.Fatalf("mint relay identity: %v", err)
	}

	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher))
	node := e.placementNode(t)

	screenID := mintSignageScreen(t, e, node, "Lobby A")
	castID := mintSignageCast(t, e, node, "Service Requested")
	mintEventAutomation(t, e, node, true,
		map[string]any{"interaction": "call_service"},
		map[string]any{"type": "play_cast", "screen_id": screenID, "cast_id": castID})

	if before := screenProgramOf(t, e, screenID, fixedNowMs); before.Display != "blank" {
		t.Fatalf("the screen starts at %q, want the terminal blank", before.Display)
	}

	// The ingest, wired the way cmd/waiveo-feeder wires it: the dispatcher is
	// the EventDeliverer, not part of the EventSink.
	log := events.NewEventLog(0)
	ingest := eventingest.New(log, ingestSiteScope, ulid.Monotonic(),
		func() int64 { return fixedNowMs }, relay.Authorizer(), dispatcher.Deliver)

	// The push returns before the rule run finishes — delivery is asynchronous
	// and the ack cursor trails it, so the push that STARTS a run acks nothing
	// (eventingest's durability contract). Drain is the point the run concluded;
	// the ack that follows is what the relay acts on.
	if ack := pushInteraction(t, ingest, relay, 1, screenID, "call_service"); ack.AckThroughSeq != 0 {
		t.Fatalf("the push that started the rule run acked through %d, want 0 — nothing may be acked before its delivery concludes", ack.AckThroughSeq)
	}
	if err := ingest.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if ack := pushInteraction(t, ingest, relay, 1, screenID, "call_service"); ack.AckThroughSeq != 1 {
		t.Fatalf("after the run concluded, ack_through_seq = %d, want 1", ack.AckThroughSeq)
	}

	// The durable log holds it (the record half) AND the screen changed (the act
	// half). Either alone is the familiar half-proof.
	if got := log.After(""); len(got) != 1 || got[0].Schema != events.SchemaScreenInteraction {
		t.Fatalf("the durable event log holds %+v, want one screen.interaction", got)
	}
	after := screenProgramOf(t, e, screenID, fixedNowMs)
	if after.Display != "content" {
		t.Fatalf("after the pushed press the screen shows %q, want content — the ingest is not reaching the event-trigger evaluator", after.Display)
	}
	if len(after.Content) != 1 || after.Content[0].ContentType != "slide" {
		t.Fatalf("after the pushed press: content = %+v, want the cast's one slide", after.Content)
	}
	if !after.Pinned {
		t.Error("the override is not pinned, so a relay re-resolving its schedule would revert it within 30s")
	}
}

// TestATelemetryPushOfADroppedRecordRunsNothing: an EVT-013 drop must reach no
// rule. It is the same append-then-deliver ordering asserted from the app's own
// side — a rule that ran on a record the durable log does not hold is a run
// nobody can later explain.
func TestATelemetryPushOfADroppedRecordRunsNothing(t *testing.T) {
	relay, err := ingesttest.NewRelay("01J8Z3K4N5P6Q7R8S9T0V1W2ZR")
	if err != nil {
		t.Fatalf("mint relay identity: %v", err)
	}

	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher))
	node := e.placementNode(t)

	screenID := mintSignageScreen(t, e, node, "Lobby A")
	castID := mintSignageCast(t, e, node, "Service Requested")
	// No `match`: this rule fires on ANY screen.interaction, so nothing but the
	// EVT-013 drop can be what stops it.
	mintEventAutomation(t, e, node, true, nil,
		map[string]any{"type": "play_cast", "screen_id": screenID, "cast_id": castID})

	log := events.NewEventLog(0)
	ingest := eventingest.New(log, ingestSiteScope, ulid.Monotonic(),
		func() int64 { return fixedNowMs }, relay.Authorizer(), dispatcher.Deliver)

	// `interaction` missing: the payload fails events.Validate, so the record is
	// dropped and logged rather than appended (EVT-013/055).
	payload, err := json.Marshal(map[string]any{"screen_id": screenID, "lease_id": "lease-1", "at": fixedNowMs})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	body, err := json.Marshal(telemetry.PushBatch{
		Entries:     []telemetry.Entry{{Seq: 1, Schema: events.SchemaScreenInteraction, Payload: payload}},
		LossMarkers: []telemetry.LossMarker{},
	})
	if err != nil {
		t.Fatalf("marshal push batch: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/telemetry/v1/push", bytes.NewReader(body))
	relay.Present(req)
	rec := httptest.NewRecorder()
	ingest.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("telemetry push status = %d, want 200 (a dropped record still acks, REL-092)", rec.Code)
	}

	if got := log.After(""); len(got) != 0 {
		t.Fatalf("an invalid record was appended anyway: %+v", got)
	}
	if after := screenProgramOf(t, e, screenID, fixedNowMs); after.Display != "blank" {
		t.Fatalf("a record the event log refused fired an automation anyway (screen now %q)", after.Display)
	}
}
