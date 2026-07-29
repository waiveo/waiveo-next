// Package events1 is the executable events/1 conformance driver: the §10
// differential oracle for the durable-event envelope, the registered-schema
// catalog's bare-vs-pack naming rule, and the EVT-013 validation gate. It
// replays the conformance/corpora/events-1 cases against the LIVE,
// HTTP-mounted internal/app/eventingest (POST /telemetry/v1/push) and
// internal/app/eventsse (GET /events/v1) handlers wherever a real producer/
// transport path exists, and diffs the actual behavior against each case's
// own declared `expected` block.
//
// A 2026-07-26 audit found that no conformance driver in the repo imported a
// single internal/app/ package — this driver previously called
// internal/events functions (Envelope, Validate, AckHello, Resolve, ...)
// directly rather than driving the app-side HTTP handlers built around them.
// This version mounts eventingest.New / eventsse.New for every case whose
// own req_ids name a schema the relay telemetry channel actually carries
// (REL-090/095: automation.run, content.played, entity.state_changed,
// device.heartbeat, box.vitals) or that the SSE binding actually implements
// (resume_from / Last-Event-ID, the gap classes). Two classes of case remain
// driven directly against internal/events, each with an explicit reason:
//
//   - audit.event (EVT-080) and the internal-origin automation.run case
//     (EVT-041-skipped-internal): no producer or HTTP endpoint anywhere in
//     the tree constructs either today — eventingest is the ONLY place that
//     builds a deliverable events.Envelope, and it is relay-telemetry-scoped
//     (always origin: relay). This is a real, confirmed gap (D3/D8 in the
//     2026-07-26 reconciliation), not a driver shortcut.
//
// The webhook-signing case (EVT-151) is driven in two legs. Its corpus
// `event` is the contract's own abbreviated envelope, so the FORMULA leg pins
// the literal signed_material and its hex HMAC-SHA256 against an independent
// crypto/hmac+sha256 reference computation; the TRANSPORT leg then registers a
// real endpoint, appends a real envelope to the durable log, runs the shipping
// delivery loop (internal/app/webhookdeliver) against an in-process receiving
// server, and verifies the signature the RECEIVER got over the bytes it
// actually received. A signer correct in isolation but mis-wired at the call
// site passes the first leg and fails the second.
//
// The two WS-shaped cases (EVT-091's hello/hello-ack exchange and EVT-140's
// hello-ack-then-gap sequence) are now driven over a REAL WebSocket connection
// to the same live handler — internal/app/eventsse serves both events/1
// bindings at one path (EVT-001), so a corpus case written in WS frames is
// answered by the shipping transport rather than by library calls standing in
// for one. EVT-140 is additionally driven over SSE, since its gap has a
// distinct SSE framing (EVT-104) the same case pins.
package events1

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/eventingest"
	"github.com/maaxton/waiveo-next/internal/app/eventingest/ingesttest"
	"github.com/maaxton/waiveo-next/internal/app/eventsse"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/app/webhookdeliver"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/shared/secretseal"
)

const contract = "events/1"

// siteScope is the fixture site scope-node ULID the ingest stamps onto every
// ingested event's scope_node — the same value internal/app/eventingest's own
// tests use.
const siteScope = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"

// Run loads the frozen events-1 corpus from disk and drives every case
// against the live implementation.
func Run() report.Report {
	rep := report.Report{Driver: "events1", Target: "internal/app/eventingest, internal/app/eventsse, internal/events"}

	cases, err := LoadCorpus()
	if err != nil {
		rep.Fail("corpus", contract, fmt.Sprintf("load events-1 corpus: %v", err))
		return rep
	}
	driveCases(&rep, cases)
	return rep
}

// RunCases drives the identical per-case logic Run uses against an explicit,
// caller-supplied case set — the seam the teeth-tests use.
func RunCases(cases map[string]corpus.Case) report.Report {
	rep := report.Report{Driver: "events1", Target: "internal/app/eventingest, internal/app/eventsse, internal/events"}
	driveCases(&rep, cases)
	return rep
}

func driveCases(rep *report.Report, cases map[string]corpus.Case) {
	driveEntityStateChanged(rep, cases)
	driveMalformedRegisteredPayload(rep, cases)
	driveUnregisteredSchema(rep, cases)
	driveAutomationRunRelay(rep, cases, "EVT-040-valid-automation-run")
	driveAutomationRunRelay(rep, cases, "EVT-041-valid-automation-run-misfire-caught")
	driveAutomationRunRelay(rep, cases, "EVT-041-valid-automation-run-restarted")
	driveAutomationRunInternal(rep, cases, "EVT-041-valid-automation-run-skipped-internal")
	driveContentPlayed(rep, cases)
	driveDeviceHeartbeat(rep, cases)
	driveBoxVitals(rep, cases)
	driveAuditEvent(rep, cases)
	driveHelloFreshSubscribe(rep, cases)
	driveMalformedResumeFrom(rep, cases)
	driveResumeWithGap(rep, cases)
	driveWebhookDeliverySigned(rep, cases)
	driveScopeFilteredSubscription(rep, cases)
	driveOutOfScopeResumeFrom(rep, cases)
	driveSelectorAndSchemasParameters(rep, cases)
	driveMidStreamBufferExceeded(rep, cases)
}

// --- the live ingest harness ------------------------------------------------

// ingestHarness mounts the REAL POST /telemetry/v1/push handler
// (eventingest.New) over a bare *events.EventLog sink — a legitimate
// production wiring (eventingest.EventSink is satisfied by both a bare
// EventLog and the eventsse.Hub; the Hub adds only the SSE fan-out
// synchronization boundary, irrelevant when nothing reads the log
// concurrently with the ingest write, exactly as internal/app/eventingest's
// own tests wire it).
type ingestHarness struct {
	log     *events.EventLog
	handler http.Handler
	relay   *ingesttest.Relay
}

// pushRelay is the one minted relay identity every ingest case in this driver
// pushes as. The ingest route is mutually authenticated (relay/1 REL-003/041):
// it identifies the pusher by its enrollment-issued client certificate and
// checks the enrollment registry's revocation record (REL-016), so there is no
// anonymous push path left to drive. The fixture mints a real CA and a real
// leaf rather than switching the check off, so this driver exercises exactly
// the identity extraction and authorization decision that ships.
var pushRelay = sync.OnceValue(func() *ingesttest.Relay {
	r, err := ingesttest.NewRelay("01J8Z3K4N5P6Q7R8S9T0V1RELY")
	if err != nil {
		panic("events1 driver: mint relay identity: " + err.Error())
	}
	return r
})

func newIngestHarness() *ingestHarness {
	log := events.NewEventLog(0)
	relay := pushRelay()
	return &ingestHarness{
		log:     log,
		handler: eventingest.New(log, siteScope, monotonicIDs(), relay.Authorizer()),
		relay:   relay,
	}
}

// push drives one telemetry.push batch through the live handler and returns
// the parsed ack.
func (h *ingestHarness) push(batch telemetry.PushBatch) (telemetry.Ack, int) {
	body, _ := json.Marshal(batch)
	req := httptest.NewRequest(http.MethodPost, "/telemetry/v1/push", strings.NewReader(string(body)))
	// The connection state a verifying listener would have populated for this
	// relay's client certificate.
	h.relay.Present(req)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var ack telemetry.Ack
	_ = json.Unmarshal(rec.Body.Bytes(), &ack)
	return ack, rec.Code
}

// monotonicIDs mints deterministic, ascending, valid 26-char ULIDs — the
// shape the ingest assigns (EVT-011) and the same generator style
// internal/app/eventingest's own tests use — so a reconstructed envelope
// carrying one validates.
func monotonicIDs() func() string {
	const prefix = "01J8Z3K4N5P6Q7R8S9T0V1W2"
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	n := 0
	return func() string {
		hi := alphabet[(n/32)%32]
		lo := alphabet[n%32]
		n++
		return prefix + string([]byte{hi, lo})
	}
}

// --- EVT-010 entity.state_changed -------------------------------------------

// driveEntityStateChanged drives EVT-010 through the live POST
// /telemetry/v1/push handler. internal/events has no exported constructor for
// entity.state_changed (unlike automation.run's AutomationRunEnvelope) — no
// producer anywhere derives change_emission filtering from a
// recorded_transition — so this driver still derives the payload from the
// case's own producer-shaped input (filtering changed_attributes down to its
// change_emission: significant members, EVT-031/REG-044), but now PUSHES that
// derived payload through the real ingest handler rather than hand-building
// an Envelope and calling events.Validate directly: the real handler's own
// classification (events.ClassFor) and validation gate (EVT-013) are what
// decide delivery, not a second, driver-side call to the same function.
func driveEntityStateChanged(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-010-valid-entity-state-changed"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		RecordedTransition struct {
			EntityID          string `json:"entity_id"`
			DeviceID          string `json:"device_id"`
			OldState          string `json:"old_state"`
			NewState          string `json:"new_state"`
			ChangedAttributes []struct {
				Name           string `json:"name"`
				ChangeEmission string `json:"change_emission"`
				Old            any    `json:"old"`
				New            any    `json:"new"`
			} `json:"changed_attributes"`
		} `json:"recorded_transition"`
	}
	if err := decodeInto(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input.recorded_transition: %v", err))
		return
	}
	var want caseExpectation
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	delta := map[string]map[string]any{}
	for _, ca := range input.RecordedTransition.ChangedAttributes {
		if ca.ChangeEmission == "significant" {
			delta[ca.Name] = map[string]any{"old": ca.Old, "new": ca.New}
		}
	}
	attributeChange := len(delta) > 0
	payload := map[string]any{
		"entity_id":        input.RecordedTransition.EntityID,
		"device_id":        input.RecordedTransition.DeviceID,
		"old_state":        input.RecordedTransition.OldState,
		"new_state":        input.RecordedTransition.NewState,
		"attribute_change": attributeChange,
	}
	if attributeChange {
		payload["attributes_delta"] = delta
	}
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal derived payload: %v", err))
		return
	}

	h := newIngestHarness()
	h.push(pushBatch(telemetry.Entry{Seq: 1, Schema: events.SchemaEntityStateChanged, Payload: payloadRaw}))
	delivered, env := deliveredEnvelope(h)

	var diffs []report.Diff
	if delivered && env.Schema != events.SchemaEntityStateChanged {
		diffs = append(diffs, report.Diff{Field: "envelope.schema", Expected: events.SchemaEntityStateChanged, Actual: env.Schema})
	}
	if delivered {
		if eq, cmpErr := payloadsEqual(env.Payload, want.Envelope.Payload); cmpErr != nil {
			diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: "<a parseable JSON payload>", Actual: cmpErr.Error()})
		} else if !eq {
			diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: string(want.Envelope.Payload), Actual: string(env.Payload)})
		}
	}

	recordDeliveredBool(rep, c, delivered, want.Delivered, diffs)
}

// deliveredEnvelope reports whether the ingest harness's log holds exactly
// one appended envelope (the ingest either appends-if-valid or
// drops-if-invalid, EVT-013 — never both, never a partial write) and returns
// it when so.
func deliveredEnvelope(h *ingestHarness) (bool, events.Envelope) {
	got := h.log.After("")
	if len(got) != 1 {
		return false, events.Envelope{}
	}
	return true, got[0]
}

// --- EVT-013 the delivery gate ----------------------------------------------

// driveMalformedRegisteredPayload drives the EVT-013 malformed-payload half of
// the delivery gate. It keeps the structural, library-level diagnostics
// (schema_recognized / payload_valid / violation — these name internal
// classification facts the corpus itself asserts, not something the ingest's
// HTTP response surfaces) and ADDS the live-handler-level fact the old,
// library-only driver could not observe: pushed through the real
// POST /telemetry/v1/push handler, the malformed record is dropped — never
// appended to the log the /events/v1 SSE server reads.
func driveMalformedRegisteredPayload(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-013-invalid-registered-schema-malformed-payload"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		AttemptedSchema  string          `json:"attempted_schema"`
		AttemptedPayload json.RawMessage `json:"attempted_payload"`
	}
	if err := decodeInto(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	wantDelivered := c.ExpectBool("delivered")
	wantSchemaRecognized := c.ExpectBool("schema_recognized")
	wantPayloadValid := c.ExpectBool("payload_valid")
	wantViolation := c.ExpectString("violation")

	var diffs []report.Diff
	gotSchemaRecognized := events.IsRegisteredSchema(input.AttemptedSchema)
	if gotSchemaRecognized != wantSchemaRecognized {
		diffs = append(diffs, report.Diff{Field: "schema_recognized", Expected: wantSchemaRecognized, Actual: gotSchemaRecognized})
	}

	h := newIngestHarness()
	h.push(pushBatch(telemetry.Entry{Seq: 1, Schema: input.AttemptedSchema, Payload: input.AttemptedPayload}))
	delivered, _ := deliveredEnvelope(h)

	gotPayloadValid := gotSchemaRecognized && delivered
	if gotPayloadValid != wantPayloadValid {
		diffs = append(diffs, report.Diff{Field: "payload_valid", Expected: wantPayloadValid, Actual: gotPayloadValid})
	}

	// The violation field names the failing member the corpus's own
	// events.ValidationError would carry; the live ingest logs-and-drops
	// rather than returning the error to the caller, so this is checked
	// against the SAME events.Validate call the ingest itself makes on an
	// equivalent envelope, not a re-implementation.
	env := events.Envelope{
		ID: "01J8Z3K4N5P6Q7R8S9T0V1W2X4", Schema: input.AttemptedSchema, TS: 1, ScopeNode: siteScope,
		TraceID: "01J8Z3K4N5P6Q7R8S9T0V1W2X4", CostClass: "telemetry", RetentionClass: "telemetry-standard",
		Origin: "relay", Payload: input.AttemptedPayload,
	}
	verr := events.Validate(env)
	var ve events.ValidationError
	if !errors.As(verr, &ve) {
		diffs = append(diffs, report.Diff{Field: "violation", Expected: wantViolation, Actual: fmt.Sprintf("%v (not an events.ValidationError)", verr)})
	} else if !strings.Contains(wantViolation, ve.Field) {
		diffs = append(diffs, report.Diff{Field: "violation", Expected: wantViolation, Actual: ve.Field})
	}

	if delivered != wantDelivered {
		diffs = append(diffs, report.Diff{Field: "delivered", Expected: wantDelivered, Actual: delivered})
	}

	finishCase(rep, c, diffs)
}

// driveUnregisteredSchema drives the EVT-013/021/022 unregistered-schema half
// of the delivery gate the same way: the structural catalog checks kept, plus
// the live ingest's drop confirmed.
func driveUnregisteredSchema(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-013-invalid-unregistered-schema-payload"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		AttemptedSchema  string          `json:"attempted_schema"`
		AttemptedPayload json.RawMessage `json:"attempted_payload"`
	}
	if err := decodeInto(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	wantDelivered := c.ExpectBool("delivered")
	wantRegistered := c.ExpectBool("matches_registered_catalog")
	wantPackForm := c.ExpectBool("matches_pack_namespaced_form")

	var diffs []report.Diff
	if got := events.IsRegisteredSchema(input.AttemptedSchema); got != wantRegistered {
		diffs = append(diffs, report.Diff{Field: "matches_registered_catalog", Expected: wantRegistered, Actual: got})
	}
	if got := events.IsPackSchemaName(input.AttemptedSchema); got != wantPackForm {
		diffs = append(diffs, report.Diff{Field: "matches_pack_namespaced_form", Expected: wantPackForm, Actual: got})
	}

	h := newIngestHarness()
	h.push(pushBatch(telemetry.Entry{Seq: 1, Schema: input.AttemptedSchema, Payload: input.AttemptedPayload}))
	delivered, _ := deliveredEnvelope(h)
	if delivered != wantDelivered {
		diffs = append(diffs, report.Diff{Field: "delivered", Expected: wantDelivered, Actual: delivered})
	}

	finishCase(rep, c, diffs)
}

// --- automation.run (EVT-040/041) -------------------------------------------

// driveAutomationRunRelay drives an edge (relay-origin) automation.run case
// through the live POST /telemetry/v1/push handler: the corpus's own
// trigger/conditions/actions/disposition/misfire_caught fields are assembled
// into the EVT-040 payload shape (the same field mapping
// events.AutomationRunEnvelope performs) and pushed as a relay telemetry
// record — the ingest's own hard-coded origin: relay (eventingest.go's
// buildEnvelope) is exactly what a real edge-evaluated rule's event takes,
// so this is not a driver assumption but the live handler's actual behavior.
func driveAutomationRunRelay(rep *report.Report, cases map[string]corpus.Case, caseID string) {
	c, ok := corpus.ByID(cases, caseID)
	if !ok {
		rep.Fail(caseID, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		RuleID        string          `json:"rule_id"`
		RuleRevision  int             `json:"rule_revision"`
		Trigger       json.RawMessage `json:"trigger"`
		Conditions    json.RawMessage `json:"conditions"`
		Actions       json.RawMessage `json:"actions"`
		Disposition   string          `json:"disposition"`
		MisfireCaught bool            `json:"misfire_caught"`
	}
	if err := decodeInto(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var want caseExpectation
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	payloadRaw, err := json.Marshal(map[string]any{
		"rule_id":           input.RuleID,
		"rule_revision":     input.RuleRevision,
		"trigger_snapshot":  input.Trigger,
		"condition_results": input.Conditions,
		"action_outcomes":   input.Actions,
		"mode_disposition":  input.Disposition,
		"misfire_caught":    input.MisfireCaught,
	})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal automation.run payload: %v", err))
		return
	}

	h := newIngestHarness()
	h.push(pushBatch(telemetry.Entry{Seq: 1, Schema: events.SchemaAutomationRun, Payload: payloadRaw}))
	delivered, env := deliveredEnvelope(h)

	var diffs []report.Diff
	if delivered {
		if env.Schema != want.Envelope.Schema {
			diffs = append(diffs, report.Diff{Field: "envelope.schema", Expected: want.Envelope.Schema, Actual: env.Schema})
		}
		if env.Origin != want.Envelope.Origin {
			diffs = append(diffs, report.Diff{Field: "envelope.origin", Expected: want.Envelope.Origin, Actual: env.Origin})
		}
		if eq, cmpErr := payloadsEqual(env.Payload, want.Envelope.Payload); cmpErr != nil {
			diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: "<a parseable JSON payload>", Actual: cmpErr.Error()})
		} else if !eq {
			diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: string(want.Envelope.Payload), Actual: string(env.Payload)})
		}
	}
	recordDeliveredBool(rep, c, delivered, want.Delivered, diffs)
}

// driveAutomationRunInternal drives EVT-041-skipped-internal (origin:
// internal) directly through events.AutomationRunEnvelope — the real
// constructor, not a stub — because no producer or HTTP endpoint in the tree
// constructs an internal-origin automation.run event: eventingest always
// stamps origin: relay (it is the relay telemetry channel's own ingest), and
// no app-side rule-evaluation path emits its own event yet (D3/D8 in the
// 2026-07-26 reconciliation: "eventingest is the ONLY place in the tree that
// constructs an events.Envelope for delivery"). This is a documented,
// confirmed gap, not a driver shortcut.
func driveAutomationRunInternal(rep *report.Report, cases map[string]corpus.Case, caseID string) {
	c, ok := corpus.ByID(cases, caseID)
	if !ok {
		rep.Fail(caseID, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		RuleID        string          `json:"rule_id"`
		RuleRevision  int             `json:"rule_revision"`
		Evaluator     string          `json:"evaluator"`
		Trigger       json.RawMessage `json:"trigger"`
		Conditions    json.RawMessage `json:"conditions"`
		Actions       json.RawMessage `json:"actions"`
		Disposition   string          `json:"disposition"`
		MisfireCaught bool            `json:"misfire_caught"`
	}
	if err := decodeInto(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var want caseExpectation
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	const fixtureID = "01J8Z3K4N5P6Q7R8S9T0V1W2Y7"
	env := events.AutomationRunEnvelope(
		siteScope, fixtureID,
		input.RuleID, input.RuleRevision,
		input.Trigger, input.Conditions, input.Actions,
		input.Disposition, input.MisfireCaught, input.Evaluator,
		fixtureID, 1752537600000,
	)

	var diffs []report.Diff
	if env.Schema != want.Envelope.Schema {
		diffs = append(diffs, report.Diff{Field: "envelope.schema", Expected: want.Envelope.Schema, Actual: env.Schema})
	}
	if env.Origin != want.Envelope.Origin {
		diffs = append(diffs, report.Diff{Field: "envelope.origin", Expected: want.Envelope.Origin, Actual: env.Origin})
	}
	if eq, cmpErr := payloadsEqual(env.Payload, want.Envelope.Payload); cmpErr != nil {
		diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: "<a parseable JSON payload>", Actual: cmpErr.Error()})
	} else if !eq {
		diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: string(want.Envelope.Payload), Actual: string(env.Payload)})
	}

	verr := events.Validate(env)
	recordDelivery(rep, c, verr, want.Delivered, diffs,
		"no producer or HTTP endpoint constructs an internal-origin automation.run event anywhere in the tree "+
			"(eventingest always stamps origin: relay) — driven directly against the real AutomationRunEnvelope "+
			"constructor + Validate gate, not routable through any live handler yet (D3/D8)")
}

// --- content.played (EVT-050) ------------------------------------------------

// driveContentPlayed drives EVT-050 through the live ingest handler:
// content.played's producer input is already payload-shaped field-for-field,
// so it is pushed as-is as a relay telemetry record.
func driveContentPlayed(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-050-valid-content-played"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var want caseExpectation
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}
	payloadRaw, err := json.Marshal(c.Input)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal input as payload: %v", err))
		return
	}

	h := newIngestHarness()
	h.push(pushBatch(telemetry.Entry{Seq: 1, Schema: events.SchemaContentPlayed, Payload: payloadRaw}))
	delivered, env := deliveredEnvelope(h)

	var diffs []report.Diff
	if delivered {
		if env.Schema != want.Envelope.Schema {
			diffs = append(diffs, report.Diff{Field: "envelope.schema", Expected: want.Envelope.Schema, Actual: env.Schema})
		}
		if eq, cmpErr := payloadsEqual(env.Payload, want.Envelope.Payload); cmpErr != nil {
			diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: "<a parseable JSON payload>", Actual: cmpErr.Error()})
		} else if !eq {
			diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: string(want.Envelope.Payload), Actual: string(env.Payload)})
		}
	}
	recordDeliveredBool(rep, c, delivered, want.Delivered, diffs,
		"expected.delivered_before_t_end_known (EVT-051) is a producer-timing property this driver has no temporal model to observe; not asserted")
}

// --- device.heartbeat (EVT-060) ---------------------------------------------

// driveDeviceHeartbeat drives EVT-060 through the live ingest handler.
// EVT-061's raw-driver-value -> canonical power_state/app_state
// classification is a producer-side guarantee internal/events/
// device_heartbeat.go's own doc comment defers (a producer consults the
// device's own class, internal/deviceclass, before emitting a heartbeat) —
// no such producer exists in the tree, so this driver still builds the
// canonical payload from the corpus's own declared values rather than
// deriving it from the case's raw_power_state/raw_app_state input, but now
// pushes that canonical payload through the real ingest handler (the
// EVT-060 structural gate) rather than hand-validating an Envelope directly.
func driveDeviceHeartbeat(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-060-valid-device-heartbeat"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var want struct {
		Envelope struct {
			Schema  string `json:"schema"`
			Payload struct {
				DeviceID            string  `json:"device_id"`
				PowerState          string  `json:"power_state"`
				AppState            string  `json:"app_state"`
				NowPlayingContentID *string `json:"now_playing_content_id"`
			} `json:"payload"`
		} `json:"envelope"`
		Delivered bool `json:"delivered"`
	}
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	payloadRaw, err := json.Marshal(map[string]any{
		"device_id":              want.Envelope.Payload.DeviceID,
		"power_state":            want.Envelope.Payload.PowerState,
		"app_state":              want.Envelope.Payload.AppState,
		"now_playing_content_id": want.Envelope.Payload.NowPlayingContentID,
	})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal payload: %v", err))
		return
	}

	h := newIngestHarness()
	h.push(pushBatch(telemetry.Entry{Seq: 1, Schema: events.SchemaDeviceHeartbeat, Payload: payloadRaw}))
	delivered, env := deliveredEnvelope(h)

	var diffs []report.Diff
	if delivered && env.Schema != want.Envelope.Schema {
		diffs = append(diffs, report.Diff{Field: "envelope.schema", Expected: want.Envelope.Schema, Actual: env.Schema})
	}
	recordDeliveredBool(rep, c, delivered, want.Delivered, diffs,
		"EVT-061's device-class-membership cross-check (raw-driver-value classification) is a deferred producer-side "+
			"concern no producer implements yet; this driver pushes the corpus's own declared canonical payload rather "+
			"than deriving it from raw_power_state/raw_app_state")
}

// --- box.vitals (EVT-070) ---------------------------------------------------

// driveBoxVitals drives EVT-070 through the live ingest handler. EVT-071's
// throttled -> throttled_flags bool-to-array reshaping is likewise a
// producer-side transform no producer in the tree performs (internal/events/
// box_vitals.go's own validator requires throttled_flags as an already-shaped
// array; it does not accept or reshape a bare throttled bool) — pushing the
// case's raw `input.throttled` bool through the live ingest as-is would be
// EVT-013-dropped for missing throttled_flags, which would test nothing
// about EVT-070/071's own payload shape. This driver therefore still
// supplies the reshaped canonical payload, but — like device.heartbeat —
// pushes it through the real ingest handler rather than a hand-built
// Envelope + direct Validate call.
func driveBoxVitals(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-070-valid-box-vitals"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		RelayID           string  `json:"relay_id"`
		CPUTempC          float64 `json:"cpu_temp_c"`
		Throttled         bool    `json:"throttled"`
		Undervoltage      bool    `json:"undervoltage"`
		DiskHeadroomBytes int64   `json:"disk_headroom_bytes"`
	}
	if err := decodeInto(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var want caseExpectation
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	flags := []string{}
	if input.Throttled {
		flags = []string{"throttled"}
	}
	payloadRaw, err := json.Marshal(map[string]any{
		"relay_id":        input.RelayID,
		"cpu_temp":        input.CPUTempC,
		"throttled_flags": flags,
		"undervoltage":    input.Undervoltage,
		"disk_headroom":   input.DiskHeadroomBytes,
	})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal derived payload: %v", err))
		return
	}

	h := newIngestHarness()
	h.push(pushBatch(telemetry.Entry{Seq: 1, Schema: events.SchemaBoxVitals, Payload: payloadRaw}))
	delivered, env := deliveredEnvelope(h)

	var diffs []report.Diff
	if delivered {
		if env.Schema != want.Envelope.Schema {
			diffs = append(diffs, report.Diff{Field: "envelope.schema", Expected: want.Envelope.Schema, Actual: env.Schema})
		}
		if eq, cmpErr := payloadsEqual(env.Payload, want.Envelope.Payload); cmpErr != nil {
			diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: "<a parseable JSON payload>", Actual: cmpErr.Error()})
		} else if !eq {
			diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: string(want.Envelope.Payload), Actual: string(env.Payload)})
		}
	}
	recordDeliveredBool(rep, c, delivered, want.Delivered, diffs,
		"no producer in the tree reshapes a raw throttled bool into throttled_flags (box_vitals.go's validator requires "+
			"the array already-shaped) — this driver supplies the reshaped canonical payload directly")
}

// --- audit.event (EVT-080) ---------------------------------------------------

const auditRetentionClass = "audit-long"

// driveAuditEvent drives EVT-080 directly against events.Validate — no
// producer or HTTP endpoint anywhere in the tree constructs an audit.event
// envelope today (grep confirms only envelope/validate/class.go reference
// the schema; eventingest's relay telemetry channel does not carry it, REL-
// 095's schema list is automation.run/content.played/entity.state_changed/
// device.heartbeat/box.vitals only). This is a real, confirmed gap (D3/D8),
// not a driver shortcut around a real handler.
func driveAuditEvent(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-080-valid-audit-login-failure"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		ActorPrincipal string `json:"actor_principal"`
		Action         string `json:"action"`
		Result         string `json:"result"`
	}
	if err := decodeInto(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var want struct {
		Envelope struct {
			Schema         string          `json:"schema"`
			RetentionClass string          `json:"retention_class"`
			Payload        json.RawMessage `json:"payload"`
		} `json:"envelope"`
		Delivered bool `json:"delivered"`
	}
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	payload := map[string]any{
		"actor_principal": input.ActorPrincipal,
		"on_behalf_of":    nil,
		"action":          input.Action,
		"target":          "principal:" + input.ActorPrincipal,
		"result":          input.Result,
	}
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal derived payload: %v", err))
		return
	}

	env := events.Envelope{
		ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", Schema: events.SchemaAuditEvent, TS: 1752537600000,
		ScopeNode: siteScope, TraceID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", CostClass: "telemetry",
		RetentionClass: auditRetentionClass, Origin: "internal", OriginPrincipal: "01J8Z3K4N5P6Q7R8S9T0V1W2Y8",
		Payload: payloadRaw,
	}

	var diffs []report.Diff
	if env.Schema != want.Envelope.Schema {
		diffs = append(diffs, report.Diff{Field: "envelope.schema", Expected: want.Envelope.Schema, Actual: env.Schema})
	}
	if env.RetentionClass != want.Envelope.RetentionClass {
		diffs = append(diffs, report.Diff{Field: "envelope.retention_class", Expected: want.Envelope.RetentionClass, Actual: env.RetentionClass})
	}
	if eq, cmpErr := payloadsEqual(payloadRaw, want.Envelope.Payload); cmpErr != nil {
		diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: "<a parseable JSON payload>", Actual: cmpErr.Error()})
	} else if !eq {
		diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: string(want.Envelope.Payload), Actual: string(payloadRaw)})
	}

	verr := events.Validate(env)
	recordDelivery(rep, c, verr, want.Delivered, diffs,
		"no producer or HTTP endpoint constructs an audit.event envelope anywhere in the tree today (D3/D8) — driven "+
			"directly against the real Validate gate, not routable through any live handler yet")
}

// --- the live SSE transport (EVT-091/134/140) -------------------------------

// driveHelloFreshSubscribe drives EVT-091 as the corpus writes it: over a REAL
// WebSocket connection to the live /events/v1 handler. The case's own input is a
// WS handshake (subprotocol events.v1+json) plus a hello frame carrying a
// scope-node selector; its expectation is the server's hello-ack naming
// resume_result: fresh. Every one of those is now answered by the shipping
// transport — the negotiated subprotocol comes off the real handshake
// (EVT-090), the hello goes on the wire and the hello-ack comes back off it
// (EVT-091/092).
//
// It then drives what `fresh` MEANS (EVT-132) on both bindings: against a log
// that already holds a backlog event, neither a WS subscriber nor an SSE
// subscriber replays it — only the live append arrives.
func driveHelloFreshSubscribe(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-091-valid-hello-fresh-subscribe"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		Subprotocol string            `json:"subprotocol"`
		Frame       events.HelloFrame `json:"frame"`
	}
	if err := decodeInto(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var want struct {
		Frame events.HelloAckFrame `json:"frame"`
	}
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	const backlogID = "01J8Z3K4N5P6Q7R8S9T0V1W2ZC"
	const liveID = "01J8Z3K4N5P6Q7R8S9T0V1W2ZD"

	var diffs []report.Diff

	log := events.NewEventLog(0)
	log.Append(fixtureAutomationRunEnvelope(backlogID))
	hub := eventsse.NewHub(log)
	srv := httptest.NewServer(newEventsHandler(hub, nil))
	defer srv.Close()

	// EVT-090: the case's own offered subprotocol, negotiated by a real
	// handshake against the real handler.
	ws, err := dialWS(srv, input.Subprotocol)
	if err != nil {
		diffs = append(diffs, report.Diff{Field: "ws.handshake", Expected: "an upgrade negotiating " + input.Subprotocol, Actual: err.Error()})
		finishCase(rep, c, diffs)
		return
	}
	defer ws.close()
	if got := ws.conn.Subprotocol(); got != events.Subprotocol {
		diffs = append(diffs, report.Diff{Field: "ws.subprotocol", Expected: events.Subprotocol, Actual: got})
	}

	// EVT-091/092: the case's own hello frame goes on the wire; the server's
	// answer is read back off it.
	if err := ws.send(input.Frame); err != nil {
		diffs = append(diffs, report.Diff{Field: "ws.hello", Expected: "the hello frame is accepted", Actual: err.Error()})
		finishCase(rep, c, diffs)
		return
	}
	ack, err := ws.next(2 * time.Second)
	if err != nil {
		diffs = append(diffs, report.Diff{Field: "ws.hello_ack", Expected: "a hello-ack within 2s", Actual: err.Error()})
		finishCase(rep, c, diffs)
		return
	}
	if ack.Type != want.Frame.Type {
		diffs = append(diffs, report.Diff{Field: "ws.hello_ack.type", Expected: want.Frame.Type, Actual: ack.Type})
	}
	if ack.ResumeResult != want.Frame.ResumeResult {
		diffs = append(diffs, report.Diff{Field: "ws.hello_ack.resume_result", Expected: want.Frame.ResumeResult, Actual: ack.ResumeResult})
	}

	// What `fresh` means (EVT-132), on the binding the case is written for: the
	// pre-existing backlog is NOT replayed, and the live append arrives.
	hub.Append(fixtureAutomationRunEnvelope(liveID))
	wsEvent, err := ws.next(2 * time.Second)
	if err != nil {
		diffs = append(diffs, report.Diff{Field: "ws.fresh_connect_live_frame", Expected: "one event frame within 2s", Actual: err.Error()})
	} else if wsEvent.Type != events.FrameTypeEvent || wsEvent.Event.ID != liveID {
		diffs = append(diffs, report.Diff{
			Field:    "ws.fresh_connect_live_frame",
			Expected: liveID + " as a type:event frame (the live append; a fresh subscribe must not replay the pre-existing backlog)",
			Actual:   fmt.Sprintf("type=%s id=%s", wsEvent.Type, wsEvent.Event.ID),
		})
	}

	// And the same case's `fresh` semantics on the OTHER binding, which carries
	// the same subscription on its initial request instead of a hello (EVT-105).
	br, cancel := dialSSE(srv, "", nil)
	defer cancel()
	sseLiveID := "01J8Z3K4N5P6Q7R8S9T0V1W2ZE"
	hub.Append(fixtureAutomationRunEnvelope(sseLiveID))
	frame, err := readFrame(br, 2*time.Second)
	if err != nil {
		diffs = append(diffs, report.Diff{Field: "sse.fresh_connect_live_frame", Expected: "one SSE event frame within 2s", Actual: err.Error()})
	} else if frame.id != sseLiveID {
		diffs = append(diffs, report.Diff{Field: "sse.fresh_connect_live_frame.id", Expected: sseLiveID + " (the live append; a fresh subscribe must not replay the pre-existing backlog)", Actual: frame.id})
	}

	finishCase(rep, c, diffs,
		"the hello's own selector term is carried on the wire and accepted by the live handler, but this case's "+
			"expectation asserts only the hello-ack — the selector's FILTERING effect (EVT-121/122) is the subject of "+
			"EVT-101-valid-sse-selector-and-schemas, driven separately")
}

// driveMalformedResumeFrom drives EVT-134 through the live GET /events/v1
// handler: a malformed resume_from is rejected 400/RESUME_FROM_INVALID before
// any SSE stream starts.
func driveMalformedResumeFrom(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-134-invalid-resume-from-malformed"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		Frame struct {
			ResumeFrom string `json:"resume_from"`
		} `json:"frame"`
	}
	if err := decodeInto(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var want struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		EventsDelivered int  `json:"events_delivered"`
		TreatedAsFresh  bool `json:"treated_as_fresh"`
	}
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	log := events.NewEventLog(0)
	log.Append(fixtureAutomationRunEnvelope("01J8Z3K4N5P6Q7R8S9T0V1W2ZE"))
	hub := eventsse.NewHub(log)

	req := httptest.NewRequest(http.MethodGet, "/events/v1?resume_from="+input.Frame.ResumeFrom, nil)
	req.Header.Set("Accept", "text/event-stream")
	sseAuth().Authorize(req)
	rec := httptest.NewRecorder()
	newEventsHandler(hub, nil).ServeHTTP(rec, req)

	var diffs []report.Diff
	if rec.Code != http.StatusBadRequest {
		diffs = append(diffs, report.Diff{Field: "status", Expected: http.StatusBadRequest, Actual: rec.Code})
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Code != want.Error.Code {
		diffs = append(diffs, report.Diff{Field: "error.code", Expected: want.Error.Code, Actual: body.Code})
	}
	treatedAsFresh := strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream")
	if treatedAsFresh != want.TreatedAsFresh {
		diffs = append(diffs, report.Diff{Field: "treated_as_fresh", Expected: want.TreatedAsFresh, Actual: treatedAsFresh})
	}
	// A rejection never opens the SSE stream, so zero events are delivered —
	// the live analogue of the corpus's events_delivered:0.
	if want.EventsDelivered != 0 && !treatedAsFresh {
		diffs = append(diffs, report.Diff{Field: "events_delivered", Expected: want.EventsDelivered, Actual: 0})
	}

	finishCase(rep, c, diffs)
}

// driveResumeWithGap drives EVT-140 through the live /events/v1 handler over a
// bounded log, on BOTH bindings: resume_from names an id that has aged out of
// retention.
//
// On WS the case maps to the wire one-for-one — its `expected.frames` array IS
// the WS exchange: a hello-ack naming resume_result: gap (frames[0]), then the
// explicit gap frame that must immediately follow it (frames[1], EVT-094), then
// delivery resuming AT to_id inclusive. On SSE the same discontinuity is framed
// as an `event: gap` whose id is the gap's to_id (EVT-104), which the same case
// also pins. Both are read from real streams, not from a driver-modeled Resolve
// call.
func driveResumeWithGap(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-140-valid-resume-with-gap"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		Frame struct {
			ResumeFrom string `json:"resume_from"`
		} `json:"frame"`
		OldestRetainedID string `json:"oldest_retained_id"`
	}
	if err := decodeInto(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var want struct {
		Frames []struct {
			Type         string  `json:"type"`
			ResumeResult string  `json:"resume_result"`
			FromID       *string `json:"from_id"`
			ToID         string  `json:"to_id"`
			Reason       string  `json:"reason"`
		} `json:"frames"`
		DeliveryResumesAt string `json:"delivery_resumes_at"`
		SilentLoss        bool   `json:"silent_loss"`
	}
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}
	if len(want.Frames) != 2 {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("corpus expected.frames has %d entries; want 2 (hello-ack + gap)", len(want.Frames)))
		return
	}
	ackWant, gapWant := want.Frames[0], want.Frames[1]

	// Bounded log (retention 1): appending resume_from then oldest_retained_id
	// evicts resume_from, so oldest_retained_id is the sole retained event —
	// exactly the corpus's own "resume_from predates the retention window"
	// shape.
	log := events.NewEventLog(1)
	log.Append(fixtureAutomationRunEnvelope(input.Frame.ResumeFrom))
	log.Append(fixtureAutomationRunEnvelope(input.OldestRetainedID))
	hub := eventsse.NewHub(log)
	srv := httptest.NewServer(newEventsHandler(hub, nil))
	defer srv.Close()

	var diffs []report.Diff
	diffs = append(diffs, driveResumeWithGapOverWS(srv, input.Frame.ResumeFrom, ackWant, gapWant, want.DeliveryResumesAt)...)

	br, cancel := dialSSE(srv, "resume_from="+input.Frame.ResumeFrom, nil)
	defer cancel()

	gapFrame, err := readFrame(br, 2*time.Second)
	if err != nil {
		diffs = append(diffs, report.Diff{Field: "sse.gap_frame", Expected: "an event: gap frame within 2s", Actual: err.Error()})
		finishCase(rep, c, diffs)
		return
	}
	if gapFrame.event != "gap" {
		diffs = append(diffs, report.Diff{Field: "sse.gap_frame.event", Expected: "gap", Actual: gapFrame.event})
	}
	if gapFrame.id != gapWant.ToID {
		diffs = append(diffs, report.Diff{Field: "sse.gap_frame.id", Expected: gapWant.ToID, Actual: gapFrame.id})
	}
	var marker struct {
		FromID string `json:"from_id"`
		ToID   string `json:"to_id"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(gapFrame.data), &marker)
	var wantFromID string
	if gapWant.FromID != nil {
		wantFromID = *gapWant.FromID
	}
	if marker.FromID != wantFromID || marker.ToID != gapWant.ToID || marker.Reason != gapWant.Reason {
		diffs = append(diffs, report.Diff{Field: "sse.gap_frame.marker", Expected: fmt.Sprintf("{from_id:%s,to_id:%s,reason:%s}", wantFromID, gapWant.ToID, gapWant.Reason), Actual: fmt.Sprintf("%+v", marker)})
	}

	eventFrame, err := readFrame(br, 2*time.Second)
	if err != nil {
		diffs = append(diffs, report.Diff{Field: "sse.resumed_event_frame", Expected: "an event: event frame within 2s", Actual: err.Error()})
	} else {
		if eventFrame.event != "event" {
			diffs = append(diffs, report.Diff{Field: "sse.resumed_event_frame.event", Expected: "event", Actual: eventFrame.event})
		}
		if eventFrame.id != want.DeliveryResumesAt {
			diffs = append(diffs, report.Diff{Field: "sse.resumed_event_frame.id (delivery_resumes_at)", Expected: want.DeliveryResumesAt, Actual: eventFrame.id})
		}
		// no silent loss: the first delivered event IS the oldest retained id.
		gotSilentLoss := eventFrame.id != want.DeliveryResumesAt
		if gotSilentLoss != want.SilentLoss {
			diffs = append(diffs, report.Diff{Field: "silent_loss", Expected: want.SilentLoss, Actual: gotSilentLoss})
		}
	}

	finishCase(rep, c, diffs,
		"the corpus's frames[0] (a hello-ack naming resume_result: gap) is checked on the WS binding, where it exists; "+
			"on SSE it has no separate wire representation (EVT-105) and the gap frame itself is the first thing "+
			"streamed, so only frames[1] and the resumed delivery are checked there")
}

// driveResumeWithGapOverWS is EVT-140's WS half: the corpus's expected.frames
// array read straight off a real socket — the hello-ack naming resume_result:
// gap, the explicit gap frame that must IMMEDIATELY follow it (EVT-094), then
// delivery resuming AT to_id inclusive with no silent loss (EVT-143).
func driveResumeWithGapOverWS(srv *httptest.Server, resumeFrom string, ackWant, gapWant struct {
	Type         string  `json:"type"`
	ResumeResult string  `json:"resume_result"`
	FromID       *string `json:"from_id"`
	ToID         string  `json:"to_id"`
	Reason       string  `json:"reason"`
}, resumesAt string) []report.Diff {
	var diffs []report.Diff

	ws, err := dialWS(srv, events.Subprotocol)
	if err != nil {
		return append(diffs, report.Diff{Field: "ws.handshake", Expected: "an upgrade negotiating " + events.Subprotocol, Actual: err.Error()})
	}
	defer ws.close()

	if err := ws.send(events.HelloFrame{Type: events.FrameTypeHello, ResumeFrom: resumeFrom}); err != nil {
		return append(diffs, report.Diff{Field: "ws.hello", Expected: "the hello frame is accepted", Actual: err.Error()})
	}

	ack, err := ws.next(2 * time.Second)
	if err != nil {
		return append(diffs, report.Diff{Field: "ws.frames[0]", Expected: "a hello-ack within 2s", Actual: err.Error()})
	}
	if ack.Type != ackWant.Type || ack.ResumeResult != ackWant.ResumeResult {
		diffs = append(diffs, report.Diff{
			Field:    "ws.frames[0]",
			Expected: fmt.Sprintf("{type:%s,resume_result:%s}", ackWant.Type, ackWant.ResumeResult),
			Actual:   fmt.Sprintf("{type:%s,resume_result:%s}", ack.Type, ack.ResumeResult),
		})
	}

	gap, err := ws.next(2 * time.Second)
	if err != nil {
		return append(diffs, report.Diff{Field: "ws.frames[1]", Expected: "a gap frame immediately after the hello-ack (EVT-094)", Actual: err.Error()})
	}
	var wantFromID string
	if gapWant.FromID != nil {
		wantFromID = *gapWant.FromID
	}
	var gotFromID string
	if gap.FromID != nil {
		gotFromID = *gap.FromID
	}
	if gap.Type != gapWant.Type || gotFromID != wantFromID || gap.ToID != gapWant.ToID || gap.Reason != gapWant.Reason {
		diffs = append(diffs, report.Diff{
			Field:    "ws.frames[1]",
			Expected: fmt.Sprintf("{type:%s,from_id:%s,to_id:%s,reason:%s}", gapWant.Type, wantFromID, gapWant.ToID, gapWant.Reason),
			Actual:   fmt.Sprintf("{type:%s,from_id:%s,to_id:%s,reason:%s}", gap.Type, gotFromID, gap.ToID, gap.Reason),
		})
	}

	resumed, err := ws.next(2 * time.Second)
	if err != nil {
		return append(diffs, report.Diff{Field: "ws.delivery_resumes_at", Expected: "an event frame within 2s", Actual: err.Error()})
	}
	if resumed.Type != events.FrameTypeEvent || resumed.Event.ID != resumesAt {
		diffs = append(diffs, report.Diff{
			Field:    "ws.delivery_resumes_at",
			Expected: resumesAt + " as a type:event frame (delivery resumes AT to_id inclusive)",
			Actual:   fmt.Sprintf("type=%s id=%s", resumed.Type, resumed.Event.ID),
		})
	}
	return diffs
}

// --- webhook delivery signing (EVT-151) -------------------------------------

// driveWebhookDeliverySigned drives EVT-151 in two legs, because the corpus
// case pins two different things and one leg cannot check both.
//
// The FORMULA leg checks the corpus's literal signed_material and its hex
// HMAC-SHA256 against an INDEPENDENT crypto/hmac+sha256 reference computation —
// never a call into the code under test, so a broken signer is actually caught.
// The fixture's `event` is the contract's own abbreviated envelope (its wire
// shape elides the rest with "..."), so those exact bytes are what the formula
// is pinned over.
//
// The TRANSPORT leg registers a real endpoint, appends a real envelope to the
// durable log, and runs the shipping delivery loop against an in-process
// receiving server — then verifies the signature the RECEIVER got, over the
// bytes the receiver actually received, under the endpoint's own secret. That
// is what says the shipping transport applies the formula the first leg pinned,
// rather than some other one; a signer that was correct in isolation and
// mis-wired at the call site passes the first leg and fails this one.
func driveWebhookDeliverySigned(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-151-valid-webhook-delivery-signed"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		EndpointSigningSecret string          `json:"endpoint_signing_secret"`
		DeliveryID            string          `json:"delivery_id"`
		Timestamp             int64           `json:"timestamp"`
		Event                 json.RawMessage `json:"event"`
	}
	if err := decodeInto(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var want struct {
		Request struct {
			Headers    map[string]string `json:"headers"`
			BodySchema string            `json:"body_schema"`
		} `json:"request"`
		SignedMaterial string `json:"signed_material"`
	}
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	body, err := json.Marshal(input.Event)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal fixture event: %v", err))
		return
	}
	timestamp := strconv.FormatInt(input.Timestamp, 10)
	signedMaterial := timestamp + "." + string(body)

	var diffs []report.Diff
	if signedMaterial != want.SignedMaterial {
		diffs = append(diffs, report.Diff{Field: "signed_material", Expected: want.SignedMaterial, Actual: signedMaterial})
	}

	mac := hmac.New(sha256.New, []byte(input.EndpointSigningSecret))
	mac.Write([]byte(signedMaterial))
	wantHex := hex.EncodeToString(mac.Sum(nil))
	if got := events.WebhookSignature(input.EndpointSigningSecret, timestamp, body); got != wantHex {
		diffs = append(diffs, report.Diff{Field: "X-Waiveo-Signature", Expected: wantHex, Actual: got})
	}
	if gotID := want.Request.Headers[events.HeaderDeliveryID]; gotID != input.DeliveryID {
		diffs = append(diffs, report.Diff{Field: "X-Waiveo-Delivery-Id", Expected: input.DeliveryID, Actual: gotID})
	}
	if gotTS := want.Request.Headers[events.HeaderTimestamp]; gotTS != timestamp {
		diffs = append(diffs, report.Diff{Field: "X-Waiveo-Timestamp", Expected: timestamp, Actual: gotTS})
	}
	var env events.Envelope
	if err := json.Unmarshal(input.Event, &env); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("unmarshal fixture event: %v", err))
		return
	}
	if env.Schema != want.Request.BodySchema {
		diffs = append(diffs, report.Diff{Field: "body_schema", Expected: want.Request.BodySchema, Actual: env.Schema})
	}

	diffs = append(diffs, driveWebhookDeliveryOverHTTP(rep, c, input.EndpointSigningSecret, input.DeliveryID, input.Timestamp, env.Schema)...)

	finishCase(rep, c, diffs, "")
}

// driveWebhookDeliveryOverHTTP is the transport leg: it stands the shipping
// delivery loop up over a real store, a real durable event log and an
// in-process receiving server, registers one endpoint, and checks the POST the
// receiver observes.
//
// Every assertion is made from the RECEIVER's side, against the bytes on the
// wire: the method, the three EVT-151 headers, and a signature recomputed
// independently over `<timestamp>.<body>` with the endpoint's own secret. It
// deliberately does not reuse the corpus's literal signed_material — the
// envelope delivered here is a full one, not the fixture's abbreviation — so
// what this leg pins is that the shipping loop signs the bytes it actually
// sent, under the secret that endpoint was registered with.
func driveWebhookDeliveryOverHTTP(rep *report.Report, c corpus.Case, secret, deliveryID string, timestampSec int64, schema string) []report.Diff {
	const endpointID = "01J8Z3K4N5P6Q7R8S9T0V1WHK1"
	const envelopeID = "01J8Z3K4N5P6Q7R8S9T0V1W2YB"

	fail := func(what string, err error) []report.Diff {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("%s: %v", what, err))
		return nil
	}

	type observed struct {
		method  string
		headers http.Header
		body    []byte
	}
	seen := make(chan observed, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- observed{method: r.Method, headers: r.Header.Clone(), body: body}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st, err := store.Open(":memory:")
	if err != nil {
		return fail("open store", err)
	}
	defer st.Close()

	// The clock is injected and fixed at the corpus's own timestamp, so the
	// X-Waiveo-Timestamp the receiver observes is a value this case states
	// rather than whatever second the run happened to land in.
	nowMs := timestampSec * 1000
	log, err := store.OpenEventLog(st, events.DefaultRetentionPolicy(), func() int64 { return nowMs }, func(error) {})
	if err != nil {
		return fail("open event log", err)
	}

	endpointBody, err := json.Marshal(map[string]any{
		"id": endpointID, "name": "Conformance Endpoint",
		"scope_node": siteScope, "url": srv.URL, "schemas": []string{schema},
	})
	if err != nil {
		return fail("marshal endpoint", err)
	}
	if _, err := st.Create(context.Background(), store.KindWebhookEndpoint, endpointBody); err != nil {
		return fail("register endpoint", err)
	}

	key := make([]byte, secretseal.KeySize)
	for i := range key {
		key[i] = byte(i*13 + 5)
	}
	sealer, err := secretseal.New(key)
	if err != nil {
		return fail("build sealer", err)
	}
	secrets := webhookdeliver.NewSecrets(sealer)
	sealed, err := secrets.Seal(endpointID, []byte(secret))
	if err != nil {
		return fail("seal signing secret", err)
	}
	if err := st.RotateWebhookSecret(context.Background(), endpointID, sealed, "", nowMs); err != nil {
		return fail("install signing secret", err)
	}

	log.Append(events.Envelope{
		ID: envelopeID, Schema: schema, TS: nowMs, ScopeNode: siteScope,
		TraceID: "01J8Z3K4N5P6Q7R8S9T0V1W2TR", CostClass: "cheap",
		RetentionClass: "operational", Origin: "internal",
		Payload: json.RawMessage(`{}`),
	})

	d, err := webhookdeliver.New(webhookdeliver.Config{
		Store: st, Log: log, HTTP: srv.Client(),
		NowMs: func() int64 { return nowMs },
		// The corpus's own delivery id, so the header the receiver observes is
		// the value this case names rather than a freshly minted one.
		NewID:   func() string { return deliveryID },
		Secrets: secrets,
	})
	if err != nil {
		return fail("build deliverer", err)
	}
	if err := d.Tick(context.Background()); err != nil {
		return fail("delivery pass", err)
	}

	var got observed
	select {
	case got = <-seen:
	default:
		rep.Fail(c.CaseID, contract, "the delivery loop made no HTTP request to the registered endpoint")
		return nil
	}

	var diffs []report.Diff
	if got.method != http.MethodPost {
		diffs = append(diffs, report.Diff{Field: "live.method", Expected: http.MethodPost, Actual: got.method})
	}
	if id := got.headers.Get(events.HeaderDeliveryID); id != deliveryID {
		diffs = append(diffs, report.Diff{Field: "live." + events.HeaderDeliveryID, Expected: deliveryID, Actual: id})
	}
	wantTS := strconv.FormatInt(timestampSec, 10)
	if ts := got.headers.Get(events.HeaderTimestamp); ts != wantTS {
		diffs = append(diffs, report.Diff{Field: "live." + events.HeaderTimestamp, Expected: wantTS, Actual: ts})
	}
	// The independent reference computation, over the bytes the receiver got.
	liveMac := hmac.New(sha256.New, []byte(secret))
	liveMac.Write([]byte(got.headers.Get(events.HeaderTimestamp) + "." + string(got.body)))
	wantLive := hex.EncodeToString(liveMac.Sum(nil))
	if sig := got.headers.Get(events.HeaderSignature); sig != wantLive {
		diffs = append(diffs, report.Diff{Field: "live." + events.HeaderSignature, Expected: wantLive, Actual: sig})
	}
	// No rotation has happened, so no overlap window is open and the additive
	// prior-signature header must be absent.
	if prior := got.headers.Get(events.HeaderPriorSignature); prior != "" {
		diffs = append(diffs, report.Diff{Field: "live." + events.HeaderPriorSignature, Expected: "", Actual: prior})
	}
	var liveEnv events.Envelope
	if err := json.Unmarshal(got.body, &liveEnv); err != nil {
		return fail("the delivered body is not a durable-event envelope", err)
	}
	if liveEnv.ID != envelopeID {
		diffs = append(diffs, report.Diff{Field: "live.body.id", Expected: envelopeID, Actual: liveEnv.ID})
	}
	if liveEnv.Schema != schema {
		diffs = append(diffs, report.Diff{Field: "live.body.schema", Expected: schema, Actual: liveEnv.Schema})
	}
	return diffs
}

// --- SSE dial/frame helpers --------------------------------------------------

type sseFrame struct {
	event string
	id    string
	data  string
}

// dialSSE opens a streaming GET /events/v1 against srv and returns a buffered
// reader over the live body plus a cancel func the caller MUST call.
// sseAuth is the one seeded auth fixture every SSE case in this driver connects
// as. events/1 EVT-110/111 require the binding to authenticate every connection
// and EVT-113 requires it refuse before any stream begins, so eventsse.New now
// takes an authenticator and there is no unauthenticated path to drive — a
// driver exercising the STREAM semantics has to hold a real credential first.
// One shared fixture is enough: no case here asserts anything about WHICH
// principal is connected, only that the stream behaves.
//
// sync.OnceValue rather than a package var so a fixture-construction failure
// surfaces as a panic at first use with a real message, instead of a nil
// authenticator dereferenced deep inside a handler.
var sseAuth = sync.OnceValue(func() *authtest.Fixture {
	f, err := authtest.New(authtest.Config{})
	if err != nil {
		panic("events1 driver: seed auth fixture: " + err.Error())
	}
	return f
})

// newEventsHandler mounts the live /events/v1 handler over hub, authenticated as
// the shared fixture, with the scope tree the fixture's own visible set
// (EVT-120) resolves against. One handler serves BOTH bindings (EVT-001), which
// is why the WS-shaped cases and the SSE-shaped cases mount the same thing.
func newEventsHandler(hub *eventsse.Hub, nodes []datamodel.ScopeNode) http.Handler {
	return eventsse.New(hub, sseAuth().Auth, func(context.Context) ([]datamodel.ScopeNode, error) {
		return nodes, nil
	})
}

// wsFrame is any server→client WS frame, decoded into the union of the fields
// the frame types this driver observes carry (EVT-092/093/094).
type wsFrame struct {
	Type         string          `json:"type"`
	ResumeResult string          `json:"resume_result"`
	Event        events.Envelope `json:"event"`
	FromID       *string         `json:"from_id"`
	ToID         string          `json:"to_id"`
	Reason       string          `json:"reason"`
}

// wsSubscriber is a driven events/1 WS client.
type wsSubscriber struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
}

// dialWS opens a WS connection to srv's /events/v1, offering subprotocol
// (empty = offer none, the EVT-090 refusal case) and authenticating with the
// shared fixture's session cookie — never a query-string credential (EVT-112).
func dialWS(srv *httptest.Server, subprotocol string) (*wsSubscriber, error) {
	ctx, cancel := context.WithCancel(context.Background())
	header := http.Header{}
	for k, v := range sseAuth().AuthorizeHeaders() {
		header.Set(k, v)
	}
	var offered []string
	if subprotocol != "" {
		offered = []string{subprotocol}
	}
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/events/v1", &websocket.DialOptions{
		Subprotocols: offered,
		HTTPHeader:   header,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	return &wsSubscriber{conn: conn, ctx: ctx, cancel: cancel}, nil
}

// send writes one client→server frame (EVT-002: one UTF-8 JSON message).
func (w *wsSubscriber) send(frame any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(w.ctx, 2*time.Second)
	defer cancel()
	return w.conn.Write(ctx, websocket.MessageText, data)
}

// next reads one server frame, erroring if none arrives within d.
func (w *wsSubscriber) next(d time.Duration) (wsFrame, error) {
	ctx, cancel := context.WithTimeout(w.ctx, d)
	defer cancel()
	_, data, err := w.conn.Read(ctx)
	if err != nil {
		return wsFrame{}, err
	}
	var f wsFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return wsFrame{}, fmt.Errorf("decode WS frame %s: %w", data, err)
	}
	return f, nil
}

func (w *wsSubscriber) close() {
	w.cancel()
	_ = w.conn.CloseNow()
}

func dialSSE(srv *httptest.Server, query string, header http.Header) (*bufio.Reader, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	url := srv.URL + "/events/v1"
	if query != "" {
		url += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return bufio.NewReader(strings.NewReader("")), func() {}
	}
	req.Header.Set("Accept", "text/event-stream")
	// A browser's native EventSource cannot set custom headers, so the SSE
	// binding authenticates by the session cookie (EVT-111) — which is exactly
	// what this dial presents.
	sseAuth().Authorize(req)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return bufio.NewReader(strings.NewReader("")), func() {}
	}
	return bufio.NewReader(resp.Body), func() { cancel(); resp.Body.Close() }
}

// readFrame reads one SSE frame (lines up to the blank-line terminator),
// erroring if none arrives within d.
func readFrame(br *bufio.Reader, d time.Duration) (sseFrame, error) {
	type result struct {
		f   sseFrame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var f sseFrame
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				ch <- result{f, err}
				return
			}
			if line == "\n" {
				ch <- result{f, nil}
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	select {
	case r := <-ch:
		return r.f, r.err
	case <-time.After(d):
		return sseFrame{}, fmt.Errorf("timed out after %s waiting for an SSE frame", d)
	}
}

// fixtureAutomationRunEnvelope builds a valid automation.run envelope with
// the given id — a real, serializable event body an SSE frame can carry
// (EVT-010/103); used only to seed a log's pre-existing backlog for the
// SSE-transport cases, never as the case-under-test's own assertion target.
func fixtureAutomationRunEnvelope(id string) events.Envelope {
	return events.Envelope{
		ID: id, Schema: events.SchemaAutomationRun, TS: 1752537000000, ScopeNode: siteScope,
		TraceID: "01J8Z3K4N5P6Q7R8S9T0V1W2ZZ", CostClass: "telemetry", RetentionClass: "telemetry-standard",
		Origin: "relay",
		Payload: json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2YC","rule_revision":4,` +
			`"trigger_snapshot":{"kind":"state"},"condition_results":[{"passed":true}],` +
			`"action_outcomes":[{"status":"ok"}],"mode_disposition":"ran","misfire_caught":false}`),
	}
}

// --- shared helpers ----------------------------------------------------------

func pushBatch(entries ...telemetry.Entry) telemetry.PushBatch {
	return telemetry.PushBatch{Entries: entries, LossMarkers: []telemetry.LossMarker{}}
}

type envelopeFixture struct {
	Schema  string          `json:"schema"`
	Origin  string          `json:"origin"`
	Payload json.RawMessage `json:"payload"`
}

type caseExpectation struct {
	Envelope  envelopeFixture `json:"envelope"`
	Delivered bool            `json:"delivered"`
}

func finishCase(rep *report.Report, c corpus.Case, diffs []report.Diff, notes ...string) {
	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "events/1 driver diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract, notes...)
}

// recordDeliveredBool is finishCase's helper for the ingest-driven cases,
// where "delivered" is observed directly (was exactly one envelope
// appended?) rather than derived from a driver-side Validate call.
func recordDeliveredBool(rep *report.Report, c corpus.Case, gotDelivered, wantDelivered bool, diffs []report.Diff, notes ...string) {
	if gotDelivered != wantDelivered {
		diffs = append(diffs, report.Diff{Field: "delivered", Expected: wantDelivered, Actual: gotDelivered})
	}
	finishCase(rep, c, diffs, notes...)
}

// recordDelivery is the shared EVT-013 delivery-gate assertion the two
// directly-Validate-driven cases (audit.event, internal-origin
// automation.run) use.
func recordDelivery(rep *report.Report, c corpus.Case, verr error, wantDelivered bool, diffs []report.Diff, notes ...string) {
	gotDelivered := verr == nil
	if gotDelivered != wantDelivered {
		detail := "nil"
		if verr != nil {
			detail = verr.Error()
		}
		diffs = append(diffs, report.Diff{Field: "delivered", Expected: wantDelivered, Actual: fmt.Sprintf("%v (Validate: %s)", gotDelivered, detail)})
	}
	finishCase(rep, c, diffs, notes...)
}

func payloadsEqual(a, b json.RawMessage) (bool, error) {
	var ga, gb any
	if err := json.Unmarshal(a, &ga); err != nil {
		return false, fmt.Errorf("decode actual payload: %w", err)
	}
	if err := json.Unmarshal(b, &gb); err != nil {
		return false, fmt.Errorf("decode expected payload: %w", err)
	}
	return reflect.DeepEqual(ga, gb), nil
}

func decodeInto(m map[string]any, v any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal corpus field: %w", err)
	}
	return json.Unmarshal(b, v)
}

func corpusDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "corpora", "events-1")
}

// LoadCorpus loads every frozen events-1 corpus case, keyed by case_id.
func LoadCorpus() (map[string]corpus.Case, error) {
	return corpus.LoadDir(corpusDir())
}

// --- EVT-101/120-124 scope-node filtering over the live SSE binding ---------

// scopeCase is the shape both scope-filtering corpus cases share on their input
// side: the scope-node tree the subscriber's visible set resolves against, the
// binding its principal holds, the recorded events, and the resume anchor.
type scopeCase struct {
	ScopeNodes        []datamodel.ScopeNode `json:"scope_nodes"`
	SubscriberBinding struct {
		ScopeNode string `json:"scope_node"`
		Role      string `json:"role"`
	} `json:"subscriber_binding"`
	ResumeFrom     string `json:"resume_from"`
	RecordedEvents []struct {
		ID        string `json:"id"`
		ScopeNode string `json:"scope_node"`
		Schema    string `json:"schema"`
	} `json:"recorded_events"`
}

// scopeHarness is a live /events/v1 handler whose subscriber holds a REAL
// principal bound at one scope node, over a REAL scope tree, reading a log
// already holding the case's recorded events.
//
// The Hub is CLOSED before any request is driven. That is what makes the
// delivered set observable without a timer: the handler writes and flushes the
// whole resolved backlog before it ever reaches its live select, and that select
// then returns at once on the closed done channel — so one ServeHTTP call
// against a ResponseRecorder yields exactly "the filtered backlog", complete,
// synchronously. The filtering under test is the same code path either way
// (EVT-123 applies it per event at delivery time, backlog and tail alike).
type scopeHarness struct {
	handler http.Handler
	cred    authtest.Credential
	fixture *authtest.Fixture
}

func newScopeHarness(in scopeCase) (*scopeHarness, error) {
	role := auth.Role(in.SubscriberBinding.Role)
	fixture, err := authtest.New(authtest.Config{Role: role, ScopeNode: in.SubscriberBinding.ScopeNode})
	if err != nil {
		return nil, fmt.Errorf("seed the subscriber's principal: %w", err)
	}

	log := events.NewEventLog(0)
	for _, e := range in.RecordedEvents {
		log.Append(scopedFixtureEnvelope(e.ID, e.ScopeNode, e.Schema))
	}
	hub := eventsse.NewHub(log)
	hub.Close()

	nodes := in.ScopeNodes
	handler := eventsse.New(hub, fixture.Auth, func(context.Context) ([]datamodel.ScopeNode, error) {
		return nodes, nil
	})
	return &scopeHarness{handler: handler, cred: fixture.Credential(), fixture: fixture}, nil
}

func (h *scopeHarness) close() { h.fixture.Close() }

// get drives one GET /events/v1 with the case's resume anchor plus query, and
// returns the status and every envelope id the response actually carried.
func (h *scopeHarness) get(resumeFrom, query string) (int, []string, string) {
	target := "/events/v1?resume_from=" + resumeFrom
	if query != "" {
		target += "&" + query
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Accept", "text/event-stream")
	h.cred.Authorize(req)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	ids := []string{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "id: ") {
			ids = append(ids, strings.TrimPrefix(line, "id: "))
		}
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, ids, body.Code
}

// probe drives one GET /events/v1 carrying resume_from and returns everything a
// caller could observe about the answer: the status, the Problem document member
// by member, and how many events the response actually carried.
//
// trace_id is dropped from the Problem for the reason api/1's own anti-probing
// assertions drop it: it is per-request by construction (API-010), so it differs
// between ANY two requests and says nothing about the resource. Every other
// member is part of what "indistinguishable" has to mean.
func (h *scopeHarness) probe(resumeFrom string) (int, map[string]any, int) {
	req := httptest.NewRequest(http.MethodGet, "/events/v1?resume_from="+resumeFrom, nil)
	req.Header.Set("Accept", "text/event-stream")
	h.cred.Authorize(req)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	delivered := 0
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "id: ") {
			delivered++
		}
	}
	var problem map[string]any
	if rec.Code != http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &problem)
		delete(problem, "trace_id")
	}
	return rec.Code, problem, delivered
}

// scopedFixtureEnvelope builds a valid envelope placed at scopeNode. schema
// defaults to automation.run; content.played carries its own corpus-shaped
// payload so the envelope is genuinely that schema rather than an automation.run
// body wearing a different name.
func scopedFixtureEnvelope(id, scopeNode, schema string) events.Envelope {
	env := fixtureAutomationRunEnvelope(id)
	env.ScopeNode = scopeNode
	if schema != "" {
		env.Schema = schema
	}
	if env.Schema == events.SchemaContentPlayed {
		env.Payload = json.RawMessage(`{"asset_ref":"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85",` +
			`"screen_id":"01J8Z3K4N5P6Q7R8S9T0V1W2ZE","program_revision":"rev-00042","t_start":1752537000000,` +
			`"t_end":1752537030000,"cause":"scheduled","completion":"completed"}`)
	}
	return env
}

// driveScopeFilteredSubscription drives EVT-120/123 through the live GET
// /events/v1 handler: a subscriber bound at one site receives exactly the events
// whose envelope scope_node (EVT-012) falls in its own subtree.
func driveScopeFilteredSubscription(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-120-valid-scope-filtered-subscription"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var in scopeCase
	if err := decodeInto(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var want struct {
		Status       int      `json:"status"`
		DeliveredIDs []string `json:"delivered_ids"`
	}
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	h, err := newScopeHarness(in)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	defer h.close()

	status, ids, _ := h.get(in.ResumeFrom, "")

	var diffs []report.Diff
	if status != want.Status {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want.Status, Actual: status})
	}
	if !reflect.DeepEqual(ids, want.DeliveredIDs) {
		diffs = append(diffs, report.Diff{Field: "delivered_ids", Expected: want.DeliveredIDs, Actual: ids})
	}
	finishCase(rep, c, diffs)
}

// driveSelectorAndSchemasParameters drives EVT-101/121/122/124 through the live
// GET /events/v1 handler: each of the case's query parameterizations is a real
// request, and the delivered set is read off the real response.
func driveSelectorAndSchemasParameters(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-101-valid-sse-selector-and-schemas"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var in struct {
		scopeCase
		Requests []struct {
			Name  string `json:"name"`
			Query string `json:"query"`
		} `json:"requests"`
	}
	if err := decodeInto(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var want struct {
		Responses []struct {
			Name         string   `json:"name"`
			Status       int      `json:"status"`
			ErrorCode    string   `json:"error_code"`
			DeliveredIDs []string `json:"delivered_ids"`
		} `json:"responses"`
	}
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}
	if len(want.Responses) != len(in.Requests) {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("corpus declares %d requests but %d expected responses", len(in.Requests), len(want.Responses)))
		return
	}

	h, err := newScopeHarness(in.scopeCase)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	defer h.close()

	var diffs []report.Diff
	for i, req := range in.Requests {
		exp := want.Responses[i]
		if exp.Name != req.Name {
			diffs = append(diffs, report.Diff{Field: "requests[" + strconv.Itoa(i) + "].name", Expected: req.Name, Actual: exp.Name})
			continue
		}
		status, ids, code := h.get(in.ResumeFrom, req.Query)
		if status != exp.Status {
			diffs = append(diffs, report.Diff{Field: req.Name + ".status", Expected: exp.Status, Actual: status})
		}
		if exp.ErrorCode != "" && code != exp.ErrorCode {
			diffs = append(diffs, report.Diff{Field: req.Name + ".error_code", Expected: exp.ErrorCode, Actual: code})
		}
		// A 400 body is a Problem document, whose own `instance` member is not an
		// SSE id: line, so ids is empty for it by construction.
		if !reflect.DeepEqual(ids, exp.DeliveredIDs) {
			diffs = append(diffs, report.Diff{Field: req.Name + ".delivered_ids", Expected: exp.DeliveredIDs, Actual: ids})
		}
	}
	finishCase(rep, c, diffs)
}

// --- EVT-134a: an out-of-scope resume_from reads as a never-recorded one ------

// driveOutOfScopeResumeFrom drives EVT-134a through the live GET /events/v1
// handler with a real scope tree and a really-bound principal: a resume_from
// naming a recorded event outside the subscriber's visible set is refused
// exactly as a never-recorded id is, and the subscriber's own recorded id still
// resumes. The two refusals are compared member for member — a 400 whose detail
// differed would be the same existence oracle in a politer wrapper.
func driveOutOfScopeResumeFrom(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-134-invalid-resume-from-out-of-scope"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var in struct {
		scopeCase
		NeverRecordedResumeFrom string `json:"never_recorded_resume_from"`
		VisibleResumeFrom       string `json:"visible_resume_from"`
	}
	if err := decodeInto(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var want struct {
		Status int `json:"status"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
		EventsDelivered                    int  `json:"events_delivered"`
		IndistinguishableFromNeverRecorded bool `json:"indistinguishable_from_never_recorded"`
		VisibleResumeStatus                int  `json:"visible_resume_status"`
	}
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	h, err := newScopeHarness(in.scopeCase)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	defer h.close()

	outStatus, outProblem, outDelivered := h.probe(in.ResumeFrom)
	nrStatus, nrProblem, _ := h.probe(in.NeverRecordedResumeFrom)
	visibleStatus, _, _ := h.probe(in.VisibleResumeFrom)

	var diffs []report.Diff
	if outStatus != want.Status {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want.Status, Actual: outStatus})
	}
	if code, _ := outProblem["code"].(string); code != want.Error.Code {
		diffs = append(diffs, report.Diff{Field: "error.code", Expected: want.Error.Code, Actual: code})
	}
	if outDelivered != want.EventsDelivered {
		diffs = append(diffs, report.Diff{Field: "events_delivered", Expected: want.EventsDelivered, Actual: outDelivered})
	}
	indistinguishable := outStatus == nrStatus && reflect.DeepEqual(outProblem, nrProblem)
	if indistinguishable != want.IndistinguishableFromNeverRecorded {
		diffs = append(diffs, report.Diff{
			Field:    "indistinguishable_from_never_recorded",
			Expected: want.IndistinguishableFromNeverRecorded,
			Actual:   fmt.Sprintf("out-of-scope %d %v vs never-recorded %d %v", outStatus, outProblem, nrStatus, nrProblem),
		})
	}
	// The control: without it, a handler that refused every resume_from would
	// satisfy every assertion above.
	if visibleStatus != want.VisibleResumeStatus {
		diffs = append(diffs, report.Diff{Field: "visible_resume_status", Expected: want.VisibleResumeStatus, Actual: visibleStatus})
	}

	finishCase(rep, c, diffs)
}

// --- EVT-142/142a mid-stream buffer_exceeded over a live SSE subscriber ------

// midStreamCase is the corpus shape for the mid-stream loss case: the scope tree
// and binding a real principal is seeded from, the bounded log's horizon, and
// the recorded events tagged with WHEN each is appended relative to the
// subscriber's own draining.
type midStreamCase struct {
	Retention         int                   `json:"retention"`
	ScopeNodes        []datamodel.ScopeNode `json:"scope_nodes"`
	SubscriberBinding struct {
		ScopeNode string `json:"scope_node"`
		Role      string `json:"role"`
	} `json:"subscriber_binding"`
	ResumeFrom     string `json:"resume_from"`
	RecordedEvents []struct {
		ID        string `json:"id"`
		ScopeNode string `json:"scope_node"`
		Phase     string `json:"phase"`
	} `json:"recorded_events"`
}

// flushGate parks the handler inside its own Flush so a corpus case can say
// "and THEN, before this subscriber drains, the log evicted".
//
// A mid-stream drop is by definition a race the subscriber loses, and racing it
// by timing would make this case flaky in exactly the direction that hides the
// bug (a subscriber that drains in time never gaps at all). The gate flushes
// THROUGH to the real client first — so the response headers and everything
// written so far genuinely reach the wire — and only then blocks, which leaves
// the connection live and the handler parked at a known point.
type flushGate struct {
	reached chan struct{}
	release chan struct{}
	abandon chan struct{}
}

func newFlushGate() *flushGate {
	return &flushGate{
		reached: make(chan struct{}, 64),
		release: make(chan struct{}, 64),
		abandon: make(chan struct{}),
	}
}

// wait blocks until the handler parks at its next flush.
func (g *flushGate) wait(d time.Duration) error {
	select {
	case <-g.reached:
		return nil
	case <-time.After(d):
		return fmt.Errorf("the SSE handler never reached its next flush within %s", d)
	}
}

func (g *flushGate) let()      { g.release <- struct{}{} }
func (g *flushGate) shutdown() { close(g.abandon) }

type gatedResponseWriter struct {
	http.ResponseWriter
	gate *flushGate
}

func (w *gatedResponseWriter) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
	select {
	case w.gate.reached <- struct{}{}:
	case <-w.gate.abandon:
		return
	}
	select {
	case <-w.gate.release:
	case <-w.gate.abandon:
	}
}

func gatedHandler(h http.Handler, g *flushGate) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&gatedResponseWriter{ResponseWriter: w, gate: g}, r)
	})
}

// driveMidStreamBufferExceeded drives EVT-142's mid-stream loss marker end to
// end: a REAL principal bound at one site, a REAL scope tree, a REAL bounded
// events.EventLog that really evicts, and a REAL SSE connection over HTTP.
//
// Both ends of the marker are checked against the corpus, and both are
// properties of the SUBSCRIBER rather than of the log: from_id is the last id
// this connection actually DELIVERED (EVT-140), which is not the highest id its
// stream considered — the watermark advances over events EVT-120/123 suppressed
// too — and to_id is the first retained id this subscriber may READ (EVT-134a).
// The case's own marker_must_not_name list carries the ids a whole-log
// resolution would have produced instead, so the anti-disclosure property is
// pinned by the corpus rather than by this driver's reading of it.
func driveMidStreamBufferExceeded(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-142-valid-mid-stream-buffer-exceeded-gap"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var in midStreamCase
	if err := decodeInto(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var want struct {
		DeliveredBeforeGap []string `json:"delivered_before_gap"`
		Frames             []struct {
			Type   string  `json:"type"`
			FromID *string `json:"from_id"`
			ToID   string  `json:"to_id"`
			Reason string  `json:"reason"`
		} `json:"frames"`
		DeliveryResumesAt string   `json:"delivery_resumes_at"`
		SilentLoss        bool     `json:"silent_loss"`
		MarkerMustNotName []string `json:"marker_must_not_name"`
	}
	if err := decodeInto(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}
	if len(want.Frames) != 1 {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("corpus expected.frames has %d entries; want 1 (the gap)", len(want.Frames)))
		return
	}
	gapWant := want.Frames[0]

	fixture, err := authtest.New(authtest.Config{
		Role:      auth.Role(in.SubscriberBinding.Role),
		ScopeNode: in.SubscriberBinding.ScopeNode,
	})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("seed the subscriber's principal: %v", err))
		return
	}
	defer fixture.Close()

	hub := eventsse.NewHub(events.NewEventLog(in.Retention))
	appendPhase := func(phase string) {
		for _, e := range in.RecordedEvents {
			if e.Phase == phase {
				hub.Append(scopedFixtureEnvelope(e.ID, e.ScopeNode, ""))
			}
		}
	}
	appendPhase("before_connect")

	// ORDER MATTERS, and getting it wrong turns this case's most important
	// failure into a CI timeout. Deferred calls run last-in-first-out, so
	// srv.Close() must be registered BEFORE gate.shutdown() in order to run
	// AFTER it: Close blocks until in-flight handlers return, and the handler
	// this case parks inside the flush gate cannot return until the gate is
	// released. Registered the other way round, an implementation that never
	// emits the marker — the silent loss this case exists to catch — deadlocks
	// on cleanup and is reported as a hung job rather than as a diff.
	gate := newFlushGate()
	nodes := in.ScopeNodes
	srv := httptest.NewServer(gatedHandler(eventsse.New(hub, fixture.Auth, func(context.Context) ([]datamodel.ScopeNode, error) {
		return nodes, nil
	}), gate))
	defer srv.Close()
	defer gate.shutdown()

	cred := fixture.Credential()
	br, cancel := dialSSEAs(srv, cred, "resume_from="+in.ResumeFrom)
	defer cancel()

	var diffs []report.Diff
	fail := func(field string, expected, actual any) {
		diffs = append(diffs, report.Diff{Field: field, Expected: expected, Actual: actual})
	}

	// The backlog: everything this subscriber may read above its resume point.
	for _, wantID := range want.DeliveredBeforeGap {
		f, err := readFrame(br, 2*time.Second)
		if err != nil {
			fail("sse.backlog", wantID+" as an event frame", err.Error())
			finishCase(rep, c, diffs)
			return
		}
		if f.event != "event" || f.id != wantID {
			fail("sse.backlog", wantID, fmt.Sprintf("event=%s id=%s", f.event, f.id))
		}
	}
	if err := gate.wait(3 * time.Second); err != nil {
		fail("sse.backlog_flush", "the handler parks after flushing its backlog", err.Error())
		finishCase(rep, c, diffs)
		return
	}

	// One live wake this subscriber CONSIDERS but may not be sent — what moves
	// its watermark off its last-delivered id, which is the whole difference
	// between EVT-140's from_id and the watermark.
	appendPhase("live_before_loss")
	gate.let()
	if err := gate.wait(3 * time.Second); err != nil {
		fail("sse.live_drain", "the handler parks after draining the out-of-scope wake", err.Error())
		finishCase(rep, c, diffs)
		return
	}

	// The burst the parked subscriber loses the race to, then one event it may
	// read to wake it again.
	appendPhase("undrained")
	appendPhase("live")
	gate.let()

	f, err := readFrame(br, 3*time.Second)
	if err != nil {
		fail("sse.gap_frame", "an event: gap frame within 3s (EVT-142/143 — silent loss is forbidden)", err.Error())
		finishCase(rep, c, diffs)
		return
	}
	if f.event != gapWant.Type {
		fail("sse.gap_frame.event", gapWant.Type, f.event)
	}
	if f.id != gapWant.ToID {
		// EVT-104: the gap's SSE id: field is its to_id, so a native reconnect's
		// Last-Event-ID lands exactly at the resumed point.
		fail("sse.gap_frame.id", gapWant.ToID, f.id)
	}
	var marker struct {
		FromID *string `json:"from_id"`
		ToID   string  `json:"to_id"`
		Reason string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(f.data), &marker); err != nil {
		fail("sse.gap_frame.marker", "a {from_id,to_id,reason} loss marker", f.data)
		finishCase(rep, c, diffs)
		return
	}
	show := func(p *string) string {
		if p == nil {
			return "null"
		}
		return *p
	}
	if show(marker.FromID) != show(gapWant.FromID) {
		fail("sse.gap_frame.from_id (EVT-140: the last id SUCCESSFULLY DELIVERED)", show(gapWant.FromID), show(marker.FromID))
	}
	if marker.ToID != gapWant.ToID {
		fail("sse.gap_frame.to_id (EVT-134a: an id inside the subscriber's visible set)", gapWant.ToID, marker.ToID)
	}
	if marker.Reason != gapWant.Reason {
		fail("sse.gap_frame.reason", gapWant.Reason, marker.Reason)
	}
	for _, forbidden := range want.MarkerMustNotName {
		if marker.ToID == forbidden || show(marker.FromID) == forbidden {
			fail("sse.gap_frame.marker_must_not_name",
				"neither end of the marker names an id outside the subscriber's visible set (EVT-120/134a)", forbidden)
		}
	}

	resumed, err := readFrame(br, 3*time.Second)
	if err != nil {
		fail("sse.delivery_resumes_at", want.DeliveryResumesAt+" as an event frame", err.Error())
		finishCase(rep, c, diffs)
		return
	}
	if resumed.event != "event" || resumed.id != want.DeliveryResumesAt {
		fail("sse.delivery_resumes_at", want.DeliveryResumesAt, fmt.Sprintf("event=%s id=%s", resumed.event, resumed.id))
	}
	// No silent loss: the marker PRECEDES the event delivery resumes at, so the
	// discontinuity is covered rather than shown as a bare id jump.
	gotSilentLoss := resumed.id != want.DeliveryResumesAt
	if gotSilentLoss != want.SilentLoss {
		fail("silent_loss", want.SilentLoss, gotSilentLoss)
	}

	gate.let()
	finishCase(rep, c, diffs,
		"driven over a real SSE connection to the live handler with a real scope tree and a real bounded "+
			"events.EventLog; the eviction is real, and the subscriber's loss of the race to it is made "+
			"deterministic by parking the handler in its own flush rather than by timing")
}

// dialSSEAs is dialSSE authenticating as an explicit credential rather than as
// the shared fixture — the scope-sensitive cases need their OWN principal, since
// what the marker may name is a property of that principal's bindings.
func dialSSEAs(srv *httptest.Server, cred authtest.Credential, query string) (*bufio.Reader, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	url := srv.URL + "/events/v1"
	if query != "" {
		url += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return bufio.NewReader(strings.NewReader("")), func() {}
	}
	req.Header.Set("Accept", "text/event-stream")
	cred.Authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return bufio.NewReader(strings.NewReader("")), func() {}
	}
	return bufio.NewReader(resp.Body), func() { cancel(); resp.Body.Close() }
}
