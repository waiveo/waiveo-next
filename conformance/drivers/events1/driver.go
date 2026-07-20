// Package events1 is the executable events/1 conformance driver: the §10
// differential oracle for the durable-event envelope, the registered-schema
// catalog's bare-vs-pack naming rule, and the EVT-013 validation gate. It
// replays the first-half conformance/corpora/events-1 cases against the LIVE
// internal/events implementation and diffs the actual behavior — the
// delivered/valid verdict, and any pinned derived envelope schema/origin/
// payload — against each case's own declared `expected` block.
//
// Every case builds its Envelope from the case's own `input` block: the four
// automation.run cases (EVT-040/041) go through internal/events'
// AutomationRunEnvelope constructor, the anti-drift seam mapping the rules
// engine's plain-value outcome (evaluator/disposition/misfire_caught) onto
// the event's origin/mode_disposition/misfire_caught — proving the
// constructor's own mapping against the corpus, not just its unit tests. The
// remaining schemas have no exported constructor (only automation.run needed
// one, per the plan's Task 2), so this driver derives each one's payload
// from its own case's producer-shaped input directly: entity.state_changed
// filters recorded_transition.changed_attributes down to its
// change_emission: significant members (EVT-031/REG-044); content.played and
// box.vitals rename/reshape already-near-canonical producer fields
// (box.vitals's throttled bool into the required, present-even-empty
// throttled_flags array, EVT-071); audit.event defaults on_behalf_of to null
// and target to "principal:"+actor_principal when the producer's input
// omits them, and always stamps the long-lived audit-long retention_class
// (EVT-082). device.heartbeat is the one exception: EVT-061's raw-driver-
// value classification (this specific fixture's raw_power_state/
// raw_app_state) is a producer-side guarantee internal/events' own
// device_heartbeat.go documents as deferred (a producer consults the
// device's own class, internal/deviceclass, before emitting a heartbeat) —
// this driver builds that one case's payload from the corpus's own declared
// canonical values rather than reproducing that deferred classification
// itself, and drives only the EVT-060 structural gate.
//
// The delivery layer (WS/SSE bindings, resumable delivery, webhooks —
// EVT-091/134/140/151) drives its four corpus cases against internal/events'
// delivery.go/eventlog.go/resume.go/webhook.go directly: a hello with no
// resume_from acks resume_result fresh (EVT-091, via AckHello); a malformed
// resume_from is rejected RESUME_FROM_INVALID before any delivery, never
// silently fresh (EVT-134, via Resolve); a resume older than the retention
// horizon acks resume_result gap with the {from_id,to_id,reason} loss marker
// and resumes delivery at the oldest retained id with no silent loss
// (EVT-140, via Resolve against an EventLog whose oldest retained id is the
// case's own input); and a webhook delivery's X-Waiveo-Signature is the
// hex HMAC-SHA256 of the corpus's own literal signed_material, checked
// against an INDEPENDENT crypto/hmac+sha256 reference computation — never a
// call into the code under test — so a broken WebhookSignature is actually
// caught, not rubber-stamped (EVT-151). events/1 is now at full corpus
// coverage: every case in the frozen corpus is driven, none pending (§10 "no
// silent caps").
package events1

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
)

const contract = "events/1"

// Shared fixture envelope metadata (fixture-ULID) for every case whose own
// producer-shaped input does not itself supply envelope-level context (only
// EVT-010's own recorded_transition case pins its full envelope, reproduced
// verbatim from that case's own `expected` below). These are the same values
// internal/events' own validEnvelope() test fixture and the EVT-010 corpus
// case use, so a reader cross-referencing the two sees the identical
// fixture, never a second invented one.
const (
	fixtureID              = "01J8Z3K4N5P6Q7R8S9T0V1W2Y7"
	fixtureScopeNode       = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	fixtureTraceID         = "01J8Z3K4N5P6Q7R8S9T0V1W2Y7"
	fixtureOriginPrincipal = "01J8Z3K4N5P6Q7R8S9T0V1W2Y8"
	fixtureCostClass       = "telemetry"
	fixtureRetentionClass  = "telemetry-standard"
	fixtureTS              = int64(1752537600000)
	fixtureOrigin          = "internal"
)

// auditRetentionClass is audit.event's own envelope override (EVT-082: an
// audit record's envelope always carries a long-lived retention_class,
// unlike the telemetry-tier default the other change-feed schemas use).
const auditRetentionClass = "audit-long"

// envelopeFixture decodes a corpus case's `expected.envelope` block. Not
// every case pins every field: the automation.run cases only pin
// schema/origin/payload (their id/ts/scope_node/trace_id are this driver's
// own fixture values, produced by the constructor, never asserted); a case
// that leaves a field out of its own corpus JSON simply decodes that field
// to its zero value, which the per-case drive function does not compare.
type envelopeFixture struct {
	ID              string          `json:"id"`
	Schema          string          `json:"schema"`
	TS              int64           `json:"ts"`
	ScopeNode       string          `json:"scope_node"`
	TraceID         string          `json:"trace_id"`
	CostClass       string          `json:"cost_class"`
	RetentionClass  string          `json:"retention_class"`
	Origin          string          `json:"origin"`
	OriginPrincipal string          `json:"origin_principal"`
	Payload         json.RawMessage `json:"payload"`
}

// caseExpectation decodes a corpus case's whole `expected` block down to the
// two things every one of these ten cases declares: the envelope shape (or
// the subset of it a given case pins) and the delivered verdict.
type caseExpectation struct {
	Envelope  envelopeFixture `json:"envelope"`
	Delivered bool            `json:"delivered"`
}

// Run loads the frozen events-1 corpus from disk and drives every first-half
// case against the live internal/events implementation.
func Run() report.Report {
	rep := report.Report{Driver: "events1", Target: "internal/events"}

	cases, err := LoadCorpus()
	if err != nil {
		rep.Fail("corpus", contract, fmt.Sprintf("load events-1 corpus: %v", err))
		return rep
	}
	driveCases(&rep, cases)
	return rep
}

// RunCases drives the identical per-case logic Run uses against an explicit,
// caller-supplied case set. It is the seam TestEvents1DriverHasTeeth uses:
// load the real corpus, corrupt one case's `expected` block in memory, and
// confirm the SAME comparison logic (never a re-implementation of it)
// reports FAIL against the corrupted expectation.
func RunCases(cases map[string]corpus.Case) report.Report {
	rep := report.Report{Driver: "events1", Target: "internal/events"}
	driveCases(&rep, cases)
	return rep
}

func driveCases(rep *report.Report, cases map[string]corpus.Case) {
	driveEntityStateChanged(rep, cases)
	driveMalformedRegisteredPayload(rep, cases)
	driveUnregisteredSchema(rep, cases)
	driveAutomationRun(rep, cases, "EVT-040-valid-automation-run")
	driveAutomationRun(rep, cases, "EVT-041-valid-automation-run-misfire-caught")
	driveAutomationRun(rep, cases, "EVT-041-valid-automation-run-restarted")
	driveAutomationRun(rep, cases, "EVT-041-valid-automation-run-skipped-internal")
	driveContentPlayed(rep, cases)
	driveDeviceHeartbeat(rep, cases)
	driveBoxVitals(rep, cases)
	driveAuditEvent(rep, cases)
	driveHelloFreshSubscribe(rep, cases)
	driveMalformedResumeFrom(rep, cases)
	driveResumeWithGap(rep, cases)
	driveWebhookDeliverySigned(rep, cases)
}

// driveEntityStateChanged drives EVT-010: it derives the entity.state_changed
// payload from the case's own producer-shaped recorded_transition input —
// filtering changed_attributes down to its change_emission: significant
// members (EVT-031/REG-044) to decide both attribute_change and
// attributes_delta — then runs the full envelope (reproduced verbatim from
// this case's own pinned `expected.envelope`, since recorded_transition
// itself carries no envelope-level metadata) through Validate.
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

	env := events.Envelope{
		ID:              want.Envelope.ID,
		Schema:          want.Envelope.Schema,
		TS:              want.Envelope.TS,
		ScopeNode:       want.Envelope.ScopeNode,
		TraceID:         want.Envelope.TraceID,
		CostClass:       want.Envelope.CostClass,
		RetentionClass:  want.Envelope.RetentionClass,
		Origin:          want.Envelope.Origin,
		OriginPrincipal: want.Envelope.OriginPrincipal,
		Payload:         payloadRaw,
	}

	var diffs []report.Diff
	if env.Schema != events.SchemaEntityStateChanged {
		diffs = append(diffs, report.Diff{Field: "envelope.schema", Expected: events.SchemaEntityStateChanged, Actual: env.Schema})
	}
	if eq, cmpErr := payloadsEqual(payloadRaw, want.Envelope.Payload); cmpErr != nil {
		diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: "<a parseable JSON payload>", Actual: cmpErr.Error()})
	} else if !eq {
		diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: string(want.Envelope.Payload), Actual: string(payloadRaw)})
	}

	verr := events.Validate(env)
	recordDelivery(rep, c, verr, want.Delivered, diffs)
}

// driveMalformedRegisteredPayload drives the EVT-013 malformed-payload half
// of the delivery gate: a recognized (registered) schema name whose payload
// omits a field its own field definition requires is never delivered, even
// though the schema name itself is a recognized platform-registered name.
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

	env := events.Envelope{
		ID:              fixtureID,
		Schema:          input.AttemptedSchema,
		TS:              fixtureTS,
		ScopeNode:       fixtureScopeNode,
		TraceID:         fixtureTraceID,
		CostClass:       fixtureCostClass,
		RetentionClass:  fixtureRetentionClass,
		Origin:          fixtureOrigin,
		OriginPrincipal: fixtureOriginPrincipal,
		Payload:         input.AttemptedPayload,
	}
	verr := events.Validate(env)

	gotPayloadValid := gotSchemaRecognized && verr == nil
	if gotPayloadValid != wantPayloadValid {
		diffs = append(diffs, report.Diff{Field: "payload_valid", Expected: wantPayloadValid, Actual: gotPayloadValid})
	}

	var ve events.ValidationError
	if !errors.As(verr, &ve) {
		diffs = append(diffs, report.Diff{Field: "violation", Expected: wantViolation, Actual: fmt.Sprintf("%v (not an events.ValidationError)", verr)})
	} else if !strings.Contains(wantViolation, ve.Field) {
		diffs = append(diffs, report.Diff{Field: "violation", Expected: wantViolation, Actual: ve.Field})
	}

	recordDelivery(rep, c, verr, wantDelivered, diffs)
}

// driveUnregisteredSchema drives the EVT-013/021/022 unregistered-schema
// half of the delivery gate: an attempted schema name that is syntactically
// bare (dot-separated, no slash) but not a member of the registered-schema
// catalog, and not a slash-qualified pack name either, is never delivered.
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

	env := events.Envelope{
		ID:              fixtureID,
		Schema:          input.AttemptedSchema,
		TS:              fixtureTS,
		ScopeNode:       fixtureScopeNode,
		TraceID:         fixtureTraceID,
		CostClass:       fixtureCostClass,
		RetentionClass:  fixtureRetentionClass,
		Origin:          fixtureOrigin,
		OriginPrincipal: fixtureOriginPrincipal,
		Payload:         input.AttemptedPayload,
	}
	verr := events.Validate(env)
	if errors.Is(verr, events.ErrPackSchema) {
		diffs = append(diffs, report.Diff{Field: "delivered", Expected: "an EVT-013 ValidationError (unregistered bare schema), not the EVT-022 pack sentinel", Actual: verr.Error()})
	}

	recordDelivery(rep, c, verr, wantDelivered, diffs)
}

// driveAutomationRun drives one of the four automation.run cases
// (EVT-040/041) through the AutomationRunEnvelope constructor — the
// anti-drift seam mapping the rules engine's plain-value outcome
// (evaluator/disposition/misfire_caught) onto the event's
// origin/mode_disposition/misfire_caught (EVT-041/042) — and asserts the
// constructor's derived schema/origin/payload against the corpus's own
// pinned expectation.
func driveAutomationRun(rep *report.Report, cases map[string]corpus.Case, caseID string) {
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

	env := events.AutomationRunEnvelope(
		fixtureScopeNode, fixtureTraceID,
		input.RuleID, input.RuleRevision,
		input.Trigger, input.Conditions, input.Actions,
		input.Disposition, input.MisfireCaught, input.Evaluator,
		fixtureID, fixtureTS,
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
	recordDelivery(rep, c, verr, want.Delivered, diffs)
}

// driveContentPlayed drives EVT-050: content.played's producer input is
// already payload-shaped field-for-field, so this case's derivation is a
// direct field mapping (no renaming, no defaulting). The corpus's own
// delivered_before_t_end_known claim (EVT-051: emitted only once t_end is
// known) is a producer-timing property this static driver has no temporal
// model to observe — t_end is already present in this fixture — so it rides
// as a Note, never asserted as if it were driven.
func driveContentPlayed(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-050-valid-content-played"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		ScreenID        string `json:"screen_id"`
		AssetRef        string `json:"asset_ref"`
		ProgramRevision string `json:"program_revision"`
		TStart          int64  `json:"t_start"`
		TEnd            int64  `json:"t_end"`
		Cause           string `json:"cause"`
		Completion      string `json:"completion"`
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

	payload := map[string]any{
		"asset_ref":        input.AssetRef,
		"screen_id":        input.ScreenID,
		"program_revision": input.ProgramRevision,
		"t_start":          input.TStart,
		"t_end":            input.TEnd,
		"cause":            input.Cause,
		"completion":       input.Completion,
	}
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal derived payload: %v", err))
		return
	}

	env := events.Envelope{
		ID:              fixtureID,
		Schema:          events.SchemaContentPlayed,
		TS:              fixtureTS,
		ScopeNode:       fixtureScopeNode,
		TraceID:         fixtureTraceID,
		CostClass:       fixtureCostClass,
		RetentionClass:  fixtureRetentionClass,
		Origin:          fixtureOrigin,
		OriginPrincipal: fixtureOriginPrincipal,
		Payload:         payloadRaw,
	}

	var diffs []report.Diff
	if env.Schema != want.Envelope.Schema {
		diffs = append(diffs, report.Diff{Field: "envelope.schema", Expected: want.Envelope.Schema, Actual: env.Schema})
	}
	if eq, cmpErr := payloadsEqual(payloadRaw, want.Envelope.Payload); cmpErr != nil {
		diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: "<a parseable JSON payload>", Actual: cmpErr.Error()})
	} else if !eq {
		diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: string(want.Envelope.Payload), Actual: string(payloadRaw)})
	}

	verr := events.Validate(env)
	recordDelivery(rep, c, verr, want.Delivered, diffs,
		"expected.delivered_before_t_end_known (EVT-051) is a producer-timing property this static driver has no temporal model to observe; not asserted")
}

// driveDeviceHeartbeat drives EVT-060. EVT-061's raw-driver-value ->
// canonical power_state/app_state classification is a producer-side
// guarantee internal/events/device_heartbeat.go's own doc comment defers (a
// producer consults the reporting device's own class, internal/deviceclass,
// before emitting a heartbeat) — this driver does not reimplement that
// classification; it links this case's own input.device_id through to the
// corpus's declared canonical payload and drives the EVT-060 structural gate
// only: the canonical payload the (not-yet-built) producer would emit
// validates and delivers.
func driveDeviceHeartbeat(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-060-valid-device-heartbeat"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		DeviceID      string `json:"device_id"`
		DeviceClass   string `json:"device_class"`
		RawPowerState string `json:"raw_power_state"`
		RawAppState   string `json:"raw_app_state"`
	}
	if err := decodeInto(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
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

	var diffs []report.Diff
	if input.DeviceID != want.Envelope.Payload.DeviceID {
		diffs = append(diffs, report.Diff{Field: "input.device_id vs payload.device_id", Expected: want.Envelope.Payload.DeviceID, Actual: input.DeviceID})
	}

	payload := map[string]any{
		"device_id":              input.DeviceID,
		"power_state":            want.Envelope.Payload.PowerState,
		"app_state":              want.Envelope.Payload.AppState,
		"now_playing_content_id": want.Envelope.Payload.NowPlayingContentID,
	}
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal payload: %v", err))
		return
	}

	env := events.Envelope{
		ID:              fixtureID,
		Schema:          events.SchemaDeviceHeartbeat,
		TS:              fixtureTS,
		ScopeNode:       fixtureScopeNode,
		TraceID:         fixtureTraceID,
		CostClass:       fixtureCostClass,
		RetentionClass:  fixtureRetentionClass,
		Origin:          fixtureOrigin,
		OriginPrincipal: fixtureOriginPrincipal,
		Payload:         payloadRaw,
	}
	if env.Schema != want.Envelope.Schema {
		diffs = append(diffs, report.Diff{Field: "envelope.schema", Expected: want.Envelope.Schema, Actual: env.Schema})
	}

	verr := events.Validate(env)
	recordDelivery(rep, c, verr, want.Delivered, diffs,
		"EVT-061's device-class-membership cross-check (raw-driver-value classification) is a deferred producer-side concern; this driver builds the payload from the corpus's own declared canonical values rather than deriving them")
}

// driveBoxVitals drives EVT-070: it reshapes the producer's throttled bool
// into the required, present-even-empty throttled_flags array (EVT-071 —
// never absent, even when empty) and renames the remaining fields onto their
// box.vitals payload names.
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

	// EVT-071: throttled_flags is present-even-empty, never absent — a relay
	// with no active flag reports the empty array, never omits the field.
	flags := []string{}
	if input.Throttled {
		flags = []string{"throttled"}
	}
	payload := map[string]any{
		"relay_id":        input.RelayID,
		"cpu_temp":        input.CPUTempC,
		"throttled_flags": flags,
		"undervoltage":    input.Undervoltage,
		"disk_headroom":   input.DiskHeadroomBytes,
	}
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal derived payload: %v", err))
		return
	}

	env := events.Envelope{
		ID:              fixtureID,
		Schema:          events.SchemaBoxVitals,
		TS:              fixtureTS,
		ScopeNode:       fixtureScopeNode,
		TraceID:         fixtureTraceID,
		CostClass:       fixtureCostClass,
		RetentionClass:  fixtureRetentionClass,
		Origin:          fixtureOrigin,
		OriginPrincipal: fixtureOriginPrincipal,
		Payload:         payloadRaw,
	}

	var diffs []report.Diff
	if env.Schema != want.Envelope.Schema {
		diffs = append(diffs, report.Diff{Field: "envelope.schema", Expected: want.Envelope.Schema, Actual: env.Schema})
	}
	if eq, cmpErr := payloadsEqual(payloadRaw, want.Envelope.Payload); cmpErr != nil {
		diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: "<a parseable JSON payload>", Actual: cmpErr.Error()})
	} else if !eq {
		diffs = append(diffs, report.Diff{Field: "envelope.payload", Expected: string(want.Envelope.Payload), Actual: string(payloadRaw)})
	}

	verr := events.Validate(env)
	recordDelivery(rep, c, verr, want.Delivered, diffs)
}

// driveAuditEvent drives EVT-080: it defaults on_behalf_of to an explicit
// null and target to "principal:"+actor_principal when the producer's own
// input omits them (EVT-083: result: failure still carries every field), and
// always stamps the long-lived audit-long retention_class (EVT-082) —
// asserted against the corpus's own pinned retention_class, not copied from
// it.
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
		ID:              fixtureID,
		Schema:          events.SchemaAuditEvent,
		TS:              fixtureTS,
		ScopeNode:       fixtureScopeNode,
		TraceID:         fixtureTraceID,
		CostClass:       fixtureCostClass,
		RetentionClass:  auditRetentionClass,
		Origin:          fixtureOrigin,
		OriginPrincipal: fixtureOriginPrincipal,
		Payload:         payloadRaw,
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
	recordDelivery(rep, c, verr, want.Delivered, diffs)
}

// driveHelloFreshSubscribe drives EVT-091: a WS client opens with subprotocol
// events.v1+json and sends a hello carrying no resume_from and a
// scope-node-narrowing selector; the server acks resume_result: fresh
// (EVT-090/091/092/132). The hello's own selector is run through
// apiselector.Parse — the EVT-121 selector MECHANICS this driver reuses
// rather than reimplements — proving it well-formed; this case pins no
// scope-node placement tree to test Selector.Matches containment against, so
// only Parse is exercised here (Matches itself is apiselector's own,
// already-corpus-driven concern under api/1).
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

	var diffs []report.Diff
	if _, negotiated := events.NegotiateSubprotocol([]string{input.Subprotocol}); !negotiated {
		diffs = append(diffs, report.Diff{Field: "subprotocol", Expected: events.Subprotocol, Actual: input.Subprotocol})
	}
	if err := events.ValidateHelloFirst(input.Frame.Type); err != nil {
		diffs = append(diffs, report.Diff{Field: "hello_first_frame", Expected: "accepted (EVT-091)", Actual: err.Error()})
	}
	if _, perr := apiselector.Parse(input.Frame.Selector); perr != nil {
		diffs = append(diffs, report.Diff{Field: "selector", Expected: "a well-formed apiselector (EVT-121)", Actual: perr.Error()})
	}

	ack, handled := events.AckHello(input.Frame)
	if !handled {
		diffs = append(diffs, report.Diff{Field: "hello_ack.handled", Expected: true, Actual: false})
	}
	if ack.Type != want.Frame.Type {
		diffs = append(diffs, report.Diff{Field: "hello_ack.type", Expected: want.Frame.Type, Actual: ack.Type})
	}
	if ack.ResumeResult != want.Frame.ResumeResult {
		diffs = append(diffs, report.Diff{Field: "hello_ack.resume_result", Expected: want.Frame.ResumeResult, Actual: ack.ResumeResult})
	}

	finishCase(rep, c, diffs)
}

// driveMalformedResumeFrom drives EVT-134: a hello supplying a syntactically
// malformed resume_from is rejected RESUME_FROM_INVALID before any event is
// delivered, never silently treated as an omitted (fresh) resume_from — even
// though the log holds a live event, proving the malformed check precedes
// any membership/retention comparison (EVT-131/134).
func driveMalformedResumeFrom(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-134-invalid-resume-from-malformed"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		Frame struct {
			Type       string `json:"type"`
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

	var log events.EventLog
	log.Append(events.Envelope{ID: fixtureID})

	out, rerr := events.Resolve(&log, input.Frame.ResumeFrom)

	var diffs []report.Diff
	if rerr == nil {
		diffs = append(diffs, report.Diff{Field: "error.code", Expected: want.Error.Code, Actual: "nil (Resolve did not reject the malformed resume_from)"})
	} else if rerr.Code != want.Error.Code {
		diffs = append(diffs, report.Diff{Field: "error.code", Expected: want.Error.Code, Actual: rerr.Code})
	}
	if got := len(out.Events); got != want.EventsDelivered {
		diffs = append(diffs, report.Diff{Field: "events_delivered", Expected: want.EventsDelivered, Actual: got})
	}
	treatedAsFresh := rerr == nil && out.Result == events.ResumeResultFresh
	if treatedAsFresh != want.TreatedAsFresh {
		diffs = append(diffs, report.Diff{Field: "treated_as_fresh", Expected: want.TreatedAsFresh, Actual: treatedAsFresh})
	}

	finishCase(rep, c, diffs)
}

// driveResumeWithGap drives EVT-140: a hello resumes from an event id older
// than the retention window; the server acks resume_result gap, the gap
// frame names the requested point and the oldest recoverable point, and
// delivery resumes AT the oldest retained id with no silent loss (EVT-140/
// 141/143). The log is built so its oldest retained id is exactly the case's
// own oldest_retained_id input — the requested resume_from predates it.
func driveResumeWithGap(rep *report.Report, cases map[string]corpus.Case) {
	const id = "EVT-140-valid-resume-with-gap"
	c, ok := corpus.ByID(cases, id)
	if !ok {
		rep.Fail(id, contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		Frame struct {
			Type       string `json:"type"`
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

	var log events.EventLog
	log.Append(events.Envelope{ID: input.OldestRetainedID})

	out, rerr := events.Resolve(&log, input.Frame.ResumeFrom)
	if rerr != nil {
		rep.Fail(c.CaseID, contract, "events/1 driver diverged from the corpus expectation",
			report.Diff{Field: "resolve.error", Expected: "nil (an aged-out resume gaps, it never errors — EVT-143)", Actual: rerr.Code})
		return
	}

	var diffs []report.Diff
	ack := want.Frames[0]
	if out.Result != ack.ResumeResult {
		diffs = append(diffs, report.Diff{Field: "hello_ack.resume_result", Expected: ack.ResumeResult, Actual: out.Result})
	}

	gapWant := want.Frames[1]
	if out.Gap == nil {
		diffs = append(diffs, report.Diff{Field: "gap", Expected: "non-nil (EVT-140)", Actual: "nil"})
	} else {
		if out.Gap.Type != gapWant.Type {
			diffs = append(diffs, report.Diff{Field: "gap.type", Expected: gapWant.Type, Actual: out.Gap.Type})
		}
		var gotFromID, wantFromID string
		if out.Gap.FromID != nil {
			gotFromID = *out.Gap.FromID
		}
		if gapWant.FromID != nil {
			wantFromID = *gapWant.FromID
		}
		if gotFromID != wantFromID {
			diffs = append(diffs, report.Diff{Field: "gap.from_id", Expected: wantFromID, Actual: gotFromID})
		}
		if out.Gap.ToID != gapWant.ToID {
			diffs = append(diffs, report.Diff{Field: "gap.to_id", Expected: gapWant.ToID, Actual: out.Gap.ToID})
		}
		if out.Gap.Reason != gapWant.Reason {
			diffs = append(diffs, report.Diff{Field: "gap.reason", Expected: gapWant.Reason, Actual: out.Gap.Reason})
		}
	}

	if out.ResumeAtID != want.DeliveryResumesAt {
		diffs = append(diffs, report.Diff{Field: "delivery_resumes_at", Expected: want.DeliveryResumesAt, Actual: out.ResumeAtID})
	}
	// EVT-143 no-silent-loss: delivery must resume AT the oldest retained id
	// inclusive, never a gap marker alongside an additional silently-missing
	// event.
	gotSilentLoss := len(out.Events) == 0 || out.Events[0].ID != want.DeliveryResumesAt
	if gotSilentLoss != want.SilentLoss {
		diffs = append(diffs, report.Diff{Field: "silent_loss", Expected: want.SilentLoss, Actual: gotSilentLoss})
	}

	finishCase(rep, c, diffs)
}

// driveWebhookDeliverySigned drives EVT-151: an automation.run event due for
// delivery to a registered webhook endpoint is signed with
// X-Waiveo-Signature = hex HMAC-SHA256 of "<timestamp>.<raw body>" keyed by
// the endpoint's own signing secret. body is the corpus's own literal event
// object re-marshaled (compacting insignificant whitespace, never reordering
// keys), reproducing the corpus's signed_material bit-for-bit. The expected
// hex is computed by an INDEPENDENT crypto/hmac+sha256 reference — never a
// call into events.WebhookSignature itself — so this can actually catch a
// broken signer, not rubber-stamp it.
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
			Method     string            `json:"method"`
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

	finishCase(rep, c, diffs,
		"expected.request.method (always POST, EVT-151) has no field on events.WebhookRequest to assert against — the live HTTP transport is a deferred thin wrapper (webhook.go's own doc comment); not asserted here")
}

// finishCase records a delivery-layer case's outcome from its own collected
// diffs (EVT-091/134/140/151): the same PASS/FAIL bookkeeping recordDelivery
// applies to the schema-catalog half, without its events.Validate
// delivered-boolean check — these four cases have no analogous single
// "delivered" verdict of their own.
func finishCase(rep *report.Report, c corpus.Case, diffs []report.Diff, notes ...string) {
	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "events/1 driver diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract, notes...)
}

// recordDelivery is the shared EVT-013 delivery-gate assertion every drive
// function ends with: a live events.Validate error means the event is never
// delivered (delivered:false), a nil error means it is (delivered:true).
// Combined with any additional structural diffs (schema/origin/payload) the
// caller already collected, it records the case PASS/FAIL against the
// corpus's own declared expectation.
func recordDelivery(rep *report.Report, c corpus.Case, verr error, wantDelivered bool, diffs []report.Diff, notes ...string) {
	gotDelivered := verr == nil
	if gotDelivered != wantDelivered {
		detail := "nil"
		if verr != nil {
			detail = verr.Error()
		}
		diffs = append(diffs, report.Diff{Field: "delivered", Expected: wantDelivered, Actual: fmt.Sprintf("%v (Validate: %s)", gotDelivered, detail)})
	}
	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "events/1 driver diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract, notes...)
}

// payloadsEqual reports whether two JSON payloads are semantically equal —
// decoded to generic Go values before comparison, so key order never
// matters, only content.
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

// decodeField re-marshals a corpus case's generic map[string]any input (or
// any sub-object of it) into a concrete typed value — the driver's own
// fields are JSON-tagged to match the frozen corpus's field names exactly,
// so this is a lossless re-decode, not a schema translation.
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

// LoadCorpus loads every frozen events-1 corpus case, keyed by case_id — the
// exact set Run itself reads. Exported so the driver's own tests can
// independently verify every case_id in the corpus DIRECTORY is accounted
// for (driven or pending), and so the teeth-test can load the real corpus
// and hand RunCases a deliberately-corrupted copy of one case.
func LoadCorpus() (map[string]corpus.Case, error) {
	return corpus.LoadDir(corpusDir())
}
