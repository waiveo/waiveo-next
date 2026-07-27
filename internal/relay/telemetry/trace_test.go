package telemetry

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// trace_test.go covers the relay half of the trace_id correlation chain: the
// buffer assigns every entry a record-time correlation id (relay/1 REL-006), it
// is a canonical ULID because that is what the events/1 envelope the app peer
// reconstructs types it as (EVT-010), and it rides the REL-090 wire shape so the
// app peer has something to propagate instead of fabricating a fresh value
// (api/1 API-063).

func tracePayload() json.RawMessage {
	return json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2YC","mode_disposition":"ran"}`)
}

// TestRecordAssignsAULIDTraceID: an entry recorded with no originating
// operation still leaves the relay carrying a real correlation id — minted where
// REL-091's seq is minted, so no producer can forget to set one.
func TestRecordAssignsAULIDTraceID(t *testing.T) {
	buf := NewBuffer(10)
	e := buf.Record(SchemaAutomationRun, tracePayload(), "subject", 0)

	if !ulid.Valid(e.TraceID) {
		t.Fatalf("Record assigned trace_id %q, which is not a canonical ULID — events/1 EVT-010 types the envelope field it becomes as a ULID", e.TraceID)
	}
	if pending := buf.Pending(); len(pending) != 1 || pending[0].TraceID != e.TraceID {
		t.Fatalf("the buffered entry must carry the same trace_id Record returned; returned %q, buffered %+v", e.TraceID, pending)
	}
}

// TestRecordAssignsADistinctTraceIDPerEntry: two independently recorded entries
// are two independent operations, so they must not share a correlation id — a
// shared value would assert a causal link that does not exist.
func TestRecordAssignsADistinctTraceIDPerEntry(t *testing.T) {
	buf := NewBuffer(10)
	first := buf.Record(SchemaAutomationRun, tracePayload(), "a", 0)
	second := buf.Record(SchemaAutomationRun, tracePayload(), "b", 0)

	if first.TraceID == second.TraceID {
		t.Fatalf("two independently recorded entries share trace_id %q; each records its own operation (REL-006)", first.TraceID)
	}
}

// TestRecordTracedPropagatesAnOriginatingTrace: when the recorded work DOES
// trace to an operation elsewhere in the platform, that operation's own trace id
// is what the entry carries — REL-006's "one identifier correlates the operation
// across the app peer, the relay, and any durable record the operation
// eventually produces".
func TestRecordTracedPropagatesAnOriginatingTrace(t *testing.T) {
	origin := ulid.New()

	buf := NewBuffer(10)
	e := buf.RecordTraced(SchemaAutomationRun, tracePayload(), "subject", 0, origin)

	if e.TraceID != origin {
		t.Fatalf("RecordTraced(trace=%q) recorded trace_id %q — an originating trace must be carried through, never replaced", origin, e.TraceID)
	}
}

// TestRecordTracedNormalizesANonULIDTrace: the relay never puts a value the app
// peer's EVT-013 gate would reject onto the wire. A caller handing over
// something that is not a ULID gets a freshly minted one rather than an entry
// that would be dropped downstream.
func TestRecordTracedNormalizesANonULIDTrace(t *testing.T) {
	for _, bad := range []string{
		"",
		"not-a-ulid",
		"7f3c1a9d4e2b6c8a0f5d3e1b7a9c2d4e", // 32 hex chars: the old newTraceID shape
		"01J8Z3K4N5P6Q7R8S9T0V1W2YCEXTRA",  // too long
		"01j8z3k4n5p6q7r8s9t0v1w2yc",       // lowercase is not canonical Crockford
	} {
		buf := NewBuffer(10)
		e := buf.RecordTraced(SchemaAutomationRun, tracePayload(), "subject", 0, bad)

		if e.TraceID == bad {
			t.Errorf("RecordTraced(trace=%q) carried it through unchanged; a non-ULID must be replaced before it reaches the wire", bad)
		}
		if !ulid.Valid(e.TraceID) {
			t.Errorf("RecordTraced(trace=%q) produced trace_id %q, still not a canonical ULID", bad, e.TraceID)
		}
	}
}

// TestTraceIDRidesTheREL090WireShape: the correlation id must actually serialize
// onto the telemetry.push entry, or the app peer has nothing to propagate. An
// entry with no trace omits the field entirely (the optional-field shape an
// older relay produces), which is what lets the app peer tell "absent" from
// "malformed".
func TestTraceIDRidesTheREL090WireShape(t *testing.T) {
	buf := NewBuffer(10)
	e := buf.Record(SchemaAutomationRun, tracePayload(), "subject", 0)

	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode entry wire fields: %v", err)
	}
	var gotTrace string
	if err := json.Unmarshal(wire["trace_id"], &gotTrace); err != nil {
		t.Fatalf("telemetry.push entry carries no trace_id field (%s) — the app peer would have nothing to propagate", raw)
	}
	if gotTrace != e.TraceID {
		t.Errorf("wire trace_id = %q, want the recorded %q", gotTrace, e.TraceID)
	}

	// Subject stays off the wire (REL-090): adding trace_id must not have
	// widened the entry shape into buffer bookkeeping.
	if _, leaked := wire["subject"]; leaked {
		t.Errorf("subject leaked onto the REL-090 wire shape: %s", raw)
	}

	bare, err := json.Marshal(Entry{Seq: 1, Schema: SchemaAutomationRun, Payload: tracePayload()})
	if err != nil {
		t.Fatalf("marshal trace-less entry: %v", err)
	}
	var bareWire map[string]json.RawMessage
	if err := json.Unmarshal(bare, &bareWire); err != nil {
		t.Fatalf("decode trace-less entry wire fields: %v", err)
	}
	if _, present := bareWire["trace_id"]; present {
		t.Errorf("an entry with no trace_id must omit the field, not send an empty one: %s", bare)
	}
}
