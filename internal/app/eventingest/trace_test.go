package eventingest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/relay/telemetryhttp"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// trace_test.go drives the one property api/1 API-063 promises and, until the
// wire record carried a trace_id, could not deliver for a single event real
// hardware produces: ONE value correlates the work across every component it
// touched. The oracle is a comparison between two live components — the trace
// the RELAY set when it recorded the entry, and the trace on the envelope the
// APP delivered — never a constant either side was told to expect.

// --- the end-to-end correlation oracle --------------------------------------

const (
	// traceRuleID is a valid ULID because automation.run's rule_id is
	// contractually one (EVT-040): a rule id that fails events.Validate would be
	// dropped at the ingest and there would be no delivered envelope to read a
	// trace off.
	traceRuleID   = "01J8Z3K4N5P6Q7R8S9T0V1W2YC"
	traceEntityID = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"
	traceDeviceID = "01J8Z3K4N5P6Q7R8S9T0V1DEVA"
	traceRelayID  = "01J8Z3K4N5P6Q7R8S9T0V1RELY"
)

// traceEdgeRule is the authored edge rule the correlation e2e fires: a state
// trigger on the screen rising to "on" dispatching a device_command — edge-class
// (RUL-002), the same shape the feeder signs into desired state (REL-062).
var traceEdgeRule = json.RawMessage(`{"id":"` + traceRuleID + `","mode":"single",` +
	`"triggers":[{"type":"state","entity_id":"` + traceEntityID + `","to":["on"]}],` +
	`"conditions":[],` +
	`"actions":[{"type":"device_command","entity_id":"` + traceEntityID + `","command":"launch","params":{"channel":"dev"}}]}`)

// traceController is the stand-in physical device: every dispatch succeeds, so a
// fired rule always completes and records its automation.run.
type traceController struct{}

func (traceController) Dispatch(entityID, command string, params map[string]any) error { return nil }

func traceResolver(entityID string) (deviceID, deviceClass string, ok bool) {
	return traceDeviceID, "media-player", true
}

func traceEntity(st string) state.Entity {
	return state.Entity{ID: traceEntityID, DeviceClass: "media-player", State: st}
}

// newTLSIngest mounts the real POST /telemetry/v1/push handler on a real TLS
// listener that verifies a relay client certificate against the enrollment CA,
// and returns the client presenting that relay's leaf — the same shape the
// deployed feeder and relay use, so nothing about the identity path is faked out
// of the correlation drive.
func newTLSIngest(t *testing.T, sink EventSink) (*httptest.Server, *http.Client) {
	t.Helper()
	relay := testRelay()
	srv := httptest.NewUnstartedServer(New(sink, siteScope, seqIDs(), testWallMs, relay.Authorizer()))
	srv.TLS = relay.ServerTLSConfig(&tls.Config{MinVersion: tls.VersionTLS13})
	srv.StartTLS()
	t.Cleanup(srv.Close)

	serverCAs := x509.NewCertPool()
	serverCAs.AddCert(srv.Certificate())
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: relay.ClientTLSConfig(serverCAs)}}
	return srv, client
}

// TestTraceID_CorrelatesFromTheRelayProducerToTheDeliveredEnvelope drives the
// whole chain with nothing stubbed at its seams:
//
//	relay automationhost (a real edge rule fires on a real observation)
//	  -> telemetry.Buffer (assigns the record-time trace, REL-006/091)
//	    -> telemetry.Channel -> telemetryhttp (POST /telemetry/v1/push over mTLS)
//	      -> this ingest (reconstructs the events/1 envelope)
//	        -> the delivered envelope's trace_id
//
// The assertion compares the delivered envelope's trace_id against the value
// READ OFF THE RELAY'S OWN BUFFER before the push — so the expectation is
// produced by the relay, not by the test. Before the wire record carried a
// trace_id, buildEnvelope minted a fresh ULID here and these two values could
// never be equal.
func TestTraceID_CorrelatesFromTheRelayProducerToTheDeliveredEnvelope(t *testing.T) {
	// --- App: the live ingest over the shared event log, on a real mTLS listener.
	log := events.NewEventLog(0)
	srv, client := newTLSIngest(t, log)

	// --- Relay: the real automation host over a real operational store.
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	host, err := automationhost.New(store, deviceclass.Builtin(), traceController{}, traceResolver, traceRelayID)
	if err != nil {
		t.Fatalf("automationhost.New: %v", err)
	}
	if err := host.ApplyEdgeRules([]json.RawMessage{traceEdgeRule}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}

	// --- Fire the rule: a durable "off" baseline (no transition) then the
	// "off -> on" rising edge that fires it (RUL-300/330).
	reg := registry.FromDeviceClass(deviceclass.Builtin(), registry.Overlay{})
	src := automationhost.NewSyntheticSource(
		state.NewObservation(reg, traceEntity("off"), traceEntity("off")),
		state.NewObservation(reg, traceEntity("off"), traceEntity("on")),
	)
	if err := host.Run(context.Background(), src); err != nil {
		t.Fatalf("host.Run: %v", err)
	}

	// --- The relay's own record of the work, read BEFORE the push drains it.
	// This is the left-hand side of the correlation: whatever the relay actually
	// assigned, not a value this test chose.
	buffered := host.TelemetryBuffer().Pending()
	if len(buffered) != 1 {
		t.Fatalf("the fired rule must have buffered exactly one automation.run; got %d entries: %+v", len(buffered), buffered)
	}
	relayTrace := buffered[0].TraceID
	if !ulid.Valid(relayTrace) {
		t.Fatalf("the relay recorded trace_id %q, which is not a canonical ULID — events/1 EVT-010 types it as one", relayTrace)
	}

	// --- Push it upstream over the real transport.
	channel := telemetry.NewChannel(host.TelemetryBuffer(), telemetryhttp.New(srv.URL, client), nil)
	ack, err := channel.Flush()
	if err != nil {
		t.Fatalf("channel.Flush: %v", err)
	}
	if ack.AckThroughSeq != buffered[0].Seq {
		t.Fatalf("ack_through_seq = %d, want the pushed entry's seq %d", ack.AckThroughSeq, buffered[0].Seq)
	}

	// --- The oracle: the delivered envelope carries the relay's trace, not one
	// the app invented.
	delivered := log.After("")
	if len(delivered) != 1 {
		t.Fatalf("the pushed automation.run must have been delivered as exactly one envelope; got %d", len(delivered))
	}
	env := delivered[0]
	if env.Schema != events.SchemaAutomationRun || env.Origin != "relay" {
		t.Fatalf("delivered envelope is not the relay-origin automation.run: schema=%q origin=%q", env.Schema, env.Origin)
	}
	if env.TraceID != relayTrace {
		t.Fatalf("trace_id does not correlate end to end (api/1 API-063):\n  relay recorded : %q\n  app delivered  : %q",
			relayTrace, env.TraceID)
	}
}

// TestTraceID_TwoFiringsKeepTheirOwnTraces: correlation has to distinguish, not
// just exist. Two independently recorded entries must arrive as two envelopes
// carrying two different traces, each matching its own relay-side record —
// otherwise a single shared value would look like correlation while asserting a
// causal link that was never there.
func TestTraceID_TwoFiringsKeepTheirOwnTraces(t *testing.T) {
	log := events.NewEventLog(0)
	h := newTestIngest(t, log)

	buf := telemetry.NewBuffer(10)
	first := buf.Record(events.SchemaAutomationRun, validAutomationRunPayload(), "a", 0)
	second := buf.Record(events.SchemaAutomationRun, validAutomationRunPayload(), "b", 0)

	postBatch(t, h, telemetry.PushBatch{Entries: buf.Pending(), LossMarkers: []telemetry.LossMarker{}})

	delivered := log.After("")
	if len(delivered) != 2 {
		t.Fatalf("both records must be delivered; got %d envelopes", len(delivered))
	}
	if delivered[0].TraceID != first.TraceID {
		t.Errorf("first envelope trace_id = %q, want the relay's %q", delivered[0].TraceID, first.TraceID)
	}
	if delivered[1].TraceID != second.TraceID {
		t.Errorf("second envelope trace_id = %q, want the relay's %q", delivered[1].TraceID, second.TraceID)
	}
	if delivered[0].TraceID == delivered[1].TraceID {
		t.Errorf("two independent operations were delivered under one trace_id %q", delivered[0].TraceID)
	}
}

// TestTraceID_OriginatingTraceRidesThroughUnchanged: when the recorded work
// traces to an operation that already has a trace id (REL-006 — a command the
// app dispatched, say), THAT value is what reaches the envelope. This is the
// propagation API-063 actually describes; the record-time mint is the fallback
// for work that originates at the relay.
func TestTraceID_OriginatingTraceRidesThroughUnchanged(t *testing.T) {
	log := events.NewEventLog(0)
	h := newTestIngest(t, log)

	origin := ulid.New()
	buf := telemetry.NewBuffer(10)
	buf.RecordTraced(events.SchemaAutomationRun, validAutomationRunPayload(), "a", 0, origin)

	postBatch(t, h, telemetry.PushBatch{Entries: buf.Pending(), LossMarkers: []telemetry.LossMarker{}})

	delivered := log.After("")
	if len(delivered) != 1 {
		t.Fatalf("want one delivered envelope, got %d", len(delivered))
	}
	if delivered[0].TraceID != origin {
		t.Fatalf("an originating trace must reach the envelope unchanged: dispatched %q, delivered %q", origin, delivered[0].TraceID)
	}
}

// --- what the app does with a trace it cannot use ---------------------------

// TestTraceID_AbsentIsReplacedAndTheEventStillDelivers: an older relay sends no
// trace_id at all. The event must still be delivered, carrying a valid ULID —
// EVT-010's type is not negotiable — and the substitution is silent, because one
// log line per entry across an older peer's whole backlog would bury the case
// that actually needs attention.
func TestTraceID_AbsentIsReplacedAndTheEventStillDelivers(t *testing.T) {
	log := events.NewEventLog(0)
	h := New(log, siteScope, seqIDs(), testWallMs, testRelay().Authorizer())
	var logged []string
	h.(*ingest).logf = func(format string, args ...any) { logged = append(logged, format) }

	// The wire shape an older relay produces: no trace_id member at all.
	entry := telemetry.Entry{Seq: 1, Schema: events.SchemaAutomationRun, Payload: validAutomationRunPayload()}
	raw, err := json.Marshal(telemetry.PushBatch{Entries: []telemetry.Entry{entry}, LossMarkers: []telemetry.LossMarker{}})
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	if bytes.Contains(raw, []byte("trace_id")) {
		t.Fatalf("a trace-less entry must serialize without the field; got %s", raw)
	}

	postBatch(t, h, telemetry.PushBatch{Entries: []telemetry.Entry{entry}, LossMarkers: []telemetry.LossMarker{}})

	delivered := log.After("")
	if len(delivered) != 1 {
		t.Fatalf("an entry with no trace_id must still be delivered (its payload is intact); got %d envelopes", len(delivered))
	}
	if !ulid.Valid(delivered[0].TraceID) {
		t.Fatalf("substituted trace_id = %q, which does not satisfy EVT-010's ULID type", delivered[0].TraceID)
	}
	if len(logged) != 0 {
		t.Errorf("an absent trace_id is an expected older-peer shape, not a defect to log per entry; logged %v", logged)
	}
}

// TestTraceID_MalformedIsReplacedNotDropped is the decision this fix had to
// make, argued in resolveTraceID: a non-ULID trace_id is a defect in the pushing
// peer, but it is a defect in CORRELATION METADATA. Dropping the record over it
// would convert that into permanent loss of a durable-class event's actual
// content — with no loss marker, because this ingest acks a dropped-invalid seq
// as received, so the relay would discard its copy. So the event is delivered
// with a substituted ULID, and the defect is logged.
func TestTraceID_MalformedIsReplacedNotDropped(t *testing.T) {
	for _, bad := range []string{
		"not-a-ulid",
		"7f3c1a9d4e2b6c8a0f5d3e1b7a9c2d4e", // 32 hex chars — the pre-fix newTraceID shape
		"01J8Z3K4N5P6Q7R8S9T0V1W2YCEXTRA",  // too long
		"01j8z3k4n5p6q7r8s9t0v1w2yc",       // lowercase, not canonical Crockford
		"ZZZZZZZZZZZZZZZZZZZZZZZZZZ",       // right length, out-of-range leading symbol
	} {
		log := events.NewEventLog(0)
		h := New(log, siteScope, seqIDs(), testWallMs, testRelay().Authorizer())
		var logged int
		h.(*ingest).logf = func(format string, args ...any) { logged++ }

		postBatch(t, h, telemetry.PushBatch{
			Entries:     []telemetry.Entry{{Seq: 1, Schema: events.SchemaAutomationRun, Payload: validAutomationRunPayload(), TraceID: bad}},
			LossMarkers: []telemetry.LossMarker{},
		})

		delivered := log.After("")
		if len(delivered) != 1 {
			t.Errorf("trace_id %q: the event must still be delivered — a bad correlation id is not a reason to lose a durable-class event (REL-093/103); got %d envelopes", bad, len(delivered))
			continue
		}
		if delivered[0].TraceID == bad {
			t.Errorf("trace_id %q reached the envelope unchanged, violating EVT-010's ULID type", bad)
		}
		if !ulid.Valid(delivered[0].TraceID) {
			t.Errorf("trace_id %q was replaced with %q, still not a canonical ULID", bad, delivered[0].TraceID)
		}
		if logged != 1 {
			t.Errorf("trace_id %q: a malformed value is a peer defect and must be logged exactly once; logged %d times", bad, logged)
		}
	}
}

// TestTraceID_MalformedNeverPoisonsAnEnvelope closes the loop on the type
// contract: whatever a peer puts on the wire, every DELIVERED envelope passes
// events.Validate — which is what EVT-013 promises a subscriber.
func TestTraceID_MalformedNeverPoisonsAnEnvelope(t *testing.T) {
	log := events.NewEventLog(0)
	h := New(log, siteScope, seqIDs(), testWallMs, testRelay().Authorizer())
	h.(*ingest).logf = func(format string, args ...any) {}

	postBatch(t, h, telemetry.PushBatch{
		Entries: []telemetry.Entry{
			{Seq: 1, Schema: events.SchemaAutomationRun, Payload: validAutomationRunPayload(), TraceID: "not-a-ulid"},
			{Seq: 2, Schema: events.SchemaAutomationRun, Payload: validAutomationRunPayload()},
			{Seq: 3, Schema: events.SchemaAutomationRun, Payload: validAutomationRunPayload(), TraceID: ulid.New()},
		},
		LossMarkers: []telemetry.LossMarker{},
	})

	delivered := log.After("")
	if len(delivered) != 3 {
		t.Fatalf("all three records must be delivered; got %d", len(delivered))
	}
	for _, env := range delivered {
		if err := events.Validate(env); err != nil {
			t.Errorf("a delivered envelope failed the EVT-013 gate: %v (trace_id %q)", err, env.TraceID)
		}
	}
}
