package api1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/app/eventingest"
	"github.com/maaxton/waiveo-next/internal/app/eventingest/ingesttest"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// This file drives API-063 — "when a request causes work in another component
// (a relay-bound command, a durable event, a background job), the server MUST
// propagate the same Trace-Id value into that component's own record of the
// work, so one value correlates the request across every component it touched".
//
// Every other stage in this driver stays inside one component: it issues an
// /api/v1 request and reads the response. API-063 is the one api/1 requirement
// whose subject is the SEAM between components, so proving it needs both ends
// live — the relay's telemetry record (internal/relay/telemetry) and the app's
// durable-event ingest (internal/app/eventingest), which is the only place in
// the tree that constructs a deliverable events/1 envelope. Driving one end and
// asserting the other by construction would certify exactly the thing that was
// broken: the ingest used to mint a fresh trace id per event, so the field was
// always present, always well-formed, and never the request's.
//
// The trace's own type is not an api/1 invention here. events/1 EVT-010 types a
// durable event's trace_id as a ULID and events.Validate enforces it, which is
// why API-061's server-generated value must be a ULID rather than merely match
// its 20-36 char grammar: a value that is not one cannot ride an envelope at all.

// tracePropagationInput decodes exactly the corpus fields this stage drives.
type tracePropagationInput struct {
	TracePropagation struct {
		OriginatingOperation struct {
			TraceID string `json:"trace_id"`
		} `json:"originating_operation"`
		RelayRecordedEntry struct {
			Seq     int64           `json:"seq"`
			Schema  string          `json:"schema"`
			Payload json.RawMessage `json:"payload"`
		} `json:"relay_recorded_entry"`
		UnusableIncomingTraceIDs struct {
			Malformed string `json:"malformed"`
			Absent    string `json:"absent"`
		} `json:"unusable_incoming_trace_ids"`
	} `json:"trace_propagation"`
}

// tracePropagationExpected is the corpus's own expectation block.
type tracePropagationExpected struct {
	OriginatingTrace struct {
		DurableEventDelivered                bool `json:"durable_event_delivered"`
		DurableEventTraceIDEqualsOriginating bool `json:"durable_event_trace_id_equals_originating"`
	} `json:"originating_trace"`
	RelayOriginatedEntry struct {
		DurableEventDelivered                 bool `json:"durable_event_delivered"`
		DurableEventTraceIDEqualsRelayRecord  bool `json:"durable_event_trace_id_equals_relay_record"`
		DurableEventTraceIDFreshlyMintedByApp bool `json:"durable_event_trace_id_freshly_minted_by_app"`
	} `json:"relay_originated_entry"`
	UnusableIncomingTrace struct {
		DurableEventDelivered             bool `json:"durable_event_delivered"`
		DurableEventTraceIDEqualsIncoming bool `json:"durable_event_trace_id_equals_incoming"`
		DurableEventTraceIDIsULID         bool `json:"durable_event_trace_id_is_ulid"`
	} `json:"unusable_incoming_trace"`
}

// traceIngestScope is the fixture site scope-node ULID the ingest stamps onto
// every reconstructed envelope's scope_node (EVT-010).
const traceIngestScope = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"

// traceRelay is the one minted relay identity this stage pushes as. The ingest
// route is mutually authenticated (relay/1 REL-003/041/016), so the fixture
// mints a real CA and a real leaf rather than switching the check off.
var traceRelay = sync.OnceValue(func() *ingesttest.Relay {
	r, err := ingesttest.NewRelay("01J8Z3K4N5P6Q7R8S9T0V1RELY")
	if err != nil {
		panic("api1 driver: mint relay identity: " + err.Error())
	}
	return r
})

// traceIngestHarness is the live app-side ingest over a bare event log.
type traceIngestHarness struct {
	log     *events.EventLog
	handler http.Handler
}

func newTraceIngestHarness() *traceIngestHarness {
	log := events.NewEventLog(0)
	return &traceIngestHarness{
		log:     log,
		handler: eventingest.New(log, traceIngestScope, traceIngestIDs(), store.WallClockMs, traceRelay().Authorizer()),
	}
}

// push delivers one batch through the live handler and returns the envelopes it
// appended.
func (h *traceIngestHarness) push(entries ...telemetry.Entry) []events.Envelope {
	body, _ := json.Marshal(telemetry.PushBatch{Entries: entries, LossMarkers: []telemetry.LossMarker{}})
	req := httptest.NewRequest(http.MethodPost, "/telemetry/v1/push", strings.NewReader(string(body)))
	traceRelay().Present(req)
	h.handler.ServeHTTP(httptest.NewRecorder(), req)
	return h.log.After("")
}

// traceIngestIDs mints deterministic ascending ULIDs for the envelope id
// (EVT-011) — deliberately NOT the trace id, which is the thing under test.
func traceIngestIDs() func() string {
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

// driveTracePropagation drives API-063 across the relay/app seam in three
// stages, each against live code on both ends:
//
//  1. The originating operation's own trace: the corpus's Trace-Id is recorded
//     onto a real telemetry.Buffer entry (telemetry.RecordTraced — the
//     propagation seam) and must arrive on the delivered envelope UNCHANGED.
//  2. A relay-originated entry: recorded with no upstream operation, so the
//     relay assigns its own record-time trace. The delivered envelope must carry
//     THAT value — read off the buffer, never a constant this driver chose — and
//     must not be a value the app minted for itself.
//  3. A trace the app cannot use: malformed, and absent. The event must still be
//     delivered (a defect in correlation metadata is not a reason to lose a
//     durable-class event) and its trace_id must be a ULID that is not the
//     unusable incoming value.
func driveTracePropagation(rep *report.Report, c corpus.Case) {
	var in tracePropagationInput
	if err := decodeTraceCase(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus input: %v", err))
		return
	}
	var want tracePropagationExpected
	if err := decodeTraceCase(c.Expected, &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus expected: %v", err))
		return
	}

	tp := in.TracePropagation
	if !ulid.Valid(tp.OriginatingOperation.TraceID) {
		rep.Fail(c.CaseID, contract, fmt.Sprintf(
			"corpus self-check: originating_operation.trace_id %q is not a ULID, so events/1 EVT-010 could never carry it",
			tp.OriginatingOperation.TraceID))
		return
	}

	var diffs []report.Diff

	// --- 1. the originating operation's trace rides through unchanged ---------
	buf := telemetry.NewBuffer(16)
	buf.RecordTraced(tp.RelayRecordedEntry.Schema, tp.RelayRecordedEntry.Payload, "", 0, tp.OriginatingOperation.TraceID)
	pending := buf.Pending()
	if len(pending) != 1 {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("relay buffer holds %d entries after one record, want 1", len(pending)), diffs...)
		return
	}
	delivered := newTraceIngestHarness().push(pending[0])
	gotDelivered := len(delivered) == 1
	if gotDelivered != want.OriginatingTrace.DurableEventDelivered {
		diffs = append(diffs, report.Diff{Field: "originating_trace.durable_event_delivered", Expected: want.OriginatingTrace.DurableEventDelivered, Actual: gotDelivered})
	}
	if gotDelivered {
		got := delivered[0].TraceID == tp.OriginatingOperation.TraceID
		if got != want.OriginatingTrace.DurableEventTraceIDEqualsOriginating {
			diffs = append(diffs, report.Diff{
				Field:    "originating_trace.durable_event_trace_id_equals_originating (API-063)",
				Expected: tp.OriginatingOperation.TraceID,
				Actual:   delivered[0].TraceID,
			})
		}
	}

	// --- 2. a relay-originated entry carries the relay's own record-time trace
	relayBuf := telemetry.NewBuffer(16)
	recorded := relayBuf.Record(tp.RelayRecordedEntry.Schema, tp.RelayRecordedEntry.Payload, "", 0)
	delivered = newTraceIngestHarness().push(recorded)
	gotDelivered = len(delivered) == 1
	if gotDelivered != want.RelayOriginatedEntry.DurableEventDelivered {
		diffs = append(diffs, report.Diff{Field: "relay_originated_entry.durable_event_delivered", Expected: want.RelayOriginatedEntry.DurableEventDelivered, Actual: gotDelivered})
	}
	if gotDelivered {
		got := delivered[0].TraceID == recorded.TraceID
		if got != want.RelayOriginatedEntry.DurableEventTraceIDEqualsRelayRecord {
			diffs = append(diffs, report.Diff{
				Field:    "relay_originated_entry.durable_event_trace_id_equals_relay_record (API-063)",
				Expected: recorded.TraceID,
				Actual:   delivered[0].TraceID,
			})
		}
		mintedByApp := delivered[0].TraceID != recorded.TraceID
		if mintedByApp != want.RelayOriginatedEntry.DurableEventTraceIDFreshlyMintedByApp {
			diffs = append(diffs, report.Diff{
				Field:    "relay_originated_entry.durable_event_trace_id_freshly_minted_by_app",
				Expected: want.RelayOriginatedEntry.DurableEventTraceIDFreshlyMintedByApp,
				Actual:   mintedByApp,
			})
		}
	}

	// --- 3. an unusable incoming trace neither poisons nor drops the event ----
	for label, unusable := range map[string]string{
		"malformed": tp.UnusableIncomingTraceIDs.Malformed,
		"absent":    tp.UnusableIncomingTraceIDs.Absent,
	} {
		entry := telemetry.Entry{
			Seq:     tp.RelayRecordedEntry.Seq,
			Schema:  tp.RelayRecordedEntry.Schema,
			Payload: tp.RelayRecordedEntry.Payload,
			TraceID: unusable,
		}
		delivered = newTraceIngestHarness().push(entry)
		gotDelivered = len(delivered) == 1
		if gotDelivered != want.UnusableIncomingTrace.DurableEventDelivered {
			diffs = append(diffs, report.Diff{
				Field:    fmt.Sprintf("unusable_incoming_trace[%s].durable_event_delivered", label),
				Expected: want.UnusableIncomingTrace.DurableEventDelivered,
				Actual:   gotDelivered,
			})
			continue
		}
		if !gotDelivered {
			continue
		}
		gotEquals := delivered[0].TraceID == unusable
		if gotEquals != want.UnusableIncomingTrace.DurableEventTraceIDEqualsIncoming {
			diffs = append(diffs, report.Diff{
				Field:    fmt.Sprintf("unusable_incoming_trace[%s].durable_event_trace_id_equals_incoming", label),
				Expected: want.UnusableIncomingTrace.DurableEventTraceIDEqualsIncoming,
				Actual:   gotEquals,
			})
		}
		gotULID := ulid.Valid(delivered[0].TraceID)
		if gotULID != want.UnusableIncomingTrace.DurableEventTraceIDIsULID {
			diffs = append(diffs, report.Diff{
				Field:    fmt.Sprintf("unusable_incoming_trace[%s].durable_event_trace_id_is_ulid (events/1 EVT-010)", label),
				Expected: want.UnusableIncomingTrace.DurableEventTraceIDIsULID,
				Actual:   delivered[0].TraceID,
			})
		}
		if err := events.Validate(delivered[0]); err != nil {
			diffs = append(diffs, report.Diff{
				Field:    fmt.Sprintf("unusable_incoming_trace[%s]: delivered envelope passes the EVT-013 gate", label),
				Expected: "valid",
				Actual:   err.Error(),
			})
		}
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "trace-id propagation across the relay/app seam diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"driven across both live ends of the seam — a real telemetry.Buffer record pushed through the live "+
			"POST /telemetry/v1/push handler — because API-063's subject is what survives BETWEEN components; "+
			"the envelope ids this stage's ingest mints are fixture-deterministic, but the trace ids compared are "+
			"whatever the relay side actually produced.")
}

// decodeTraceCase re-decodes a corpus block into a typed struct.
func decodeTraceCase(block map[string]any, out any) error {
	raw, err := json.Marshal(block)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
