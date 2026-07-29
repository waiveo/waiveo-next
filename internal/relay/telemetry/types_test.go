package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// This file (Task 1) gates the wire shapes — Entry/LossMarker/PushBatch/Ack —
// against the exact field names and values the frozen corpus cases use:
// REL-090's telemetry.push (entries + a non-empty loss_markers) and
// telemetry.ack, and REL-094's telemetry.push (a single surviving entry with
// an explicitly empty loss_markers array). Values are wired from the corpus
// documents themselves, not invented — the corpus is the oracle.

const (
	rel090Corpus = "../../../conformance/corpora/relay-1/REL-090-valid-telemetry-overflow-loss-marker.json"
	rel094Corpus = "../../../conformance/corpora/relay-1/REL-094-valid-telemetry-latest-only-heartbeat-superseded.json"
)

// assertJSONEqual compares two JSON documents structurally (key order and
// whitespace irrelevant), so tests assert on shape/values rather than on an
// incidental Go-encoder byte layout.
func assertJSONEqual(t *testing.T, got []byte, want json.RawMessage) {
	t.Helper()
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("unmarshaling got JSON: %v\ngot: %s", err, got)
	}
	if err := json.Unmarshal(want, &wantVal); err != nil {
		t.Fatalf("unmarshaling want JSON: %v\nwant: %s", err, want)
	}
	if !reflect.DeepEqual(gotVal, wantVal) {
		t.Fatalf("JSON mismatch:\n got:  %s\n want: %s", got, want)
	}
}

// rel090Body is the subset of REL-090's telemetry_push.body /
// telemetry_ack.body this test drives Entry/LossMarker/PushBatch/Ack against.
type rel090Fixture struct {
	Input struct {
		TelemetryPush struct {
			Body struct {
				Entries     json.RawMessage `json:"entries"`
				LossMarkers json.RawMessage `json:"loss_markers"`
			} `json:"body"`
		} `json:"telemetry_push"`
	} `json:"input"`
	Expected struct {
		TelemetryAck struct {
			Body struct {
				AckThroughSeq    int64           `json:"ack_through_seq"`
				LossMarkersAcked json.RawMessage `json:"loss_markers_acked"`
			} `json:"body"`
		} `json:"telemetry_ack"`
	} `json:"expected"`
}

func loadRel090Fixture(t *testing.T) rel090Fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(rel090Corpus))
	if err != nil {
		t.Fatalf("reading REL-090 corpus: %v", err)
	}
	var f rel090Fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decoding REL-090 corpus: %v", err)
	}
	return f
}

// TestPushBatchMarshalsRel090Shape builds a PushBatch from Go values matching
// REL-090's two entries (an automation.run and a content.played) and its one
// loss marker, then confirms it marshals to exactly the corpus's
// telemetry.push body — entries[].{seq,schema,payload,trace_id},
// loss_markers[].{from_seq,to_seq,dropped_counts_by_schema,reason}
// (REL-090/090a/100). The two entries carry DIFFERENT trace ids, so a shape
// that hoisted the correlation id to the batch (REL-090a forbids it: a push
// batches entries recorded independently) cannot satisfy this.
func TestPushBatchMarshalsRel090Shape(t *testing.T) {
	f := loadRel090Fixture(t)

	batch := PushBatch{
		Entries: []Entry{
			{
				Seq:     1001,
				Schema:  SchemaAutomationRun,
				Payload: json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z1","rule_revision":4,"mode_disposition":"ran"}`),
				TraceID: "01J8Z4K4N5P6Q7R8S9T0V1W3T1",
			},
			{
				Seq:     1002,
				Schema:  SchemaContentPlayed,
				Payload: json.RawMessage(`{"asset_ref":"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85","screen_id":"01J8Z3K4N5P6Q7R8S9T0V1W2X6"}`),
				TraceID: "01J8Z4K4N5P6Q7R8S9T0V1W3T2",
			},
		},
		LossMarkers: []LossMarker{
			{
				FromSeq: 980,
				ToSeq:   999,
				DroppedCountsBySchema: map[string]int{
					SchemaContentPlayed:      12,
					SchemaEntityStateChanged: 8,
				},
				Reason: ReasonBufferExceeded,
			},
		},
	}

	got, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshaling PushBatch: %v", err)
	}

	wantBody, err := json.Marshal(map[string]json.RawMessage{
		"entries":      f.Input.TelemetryPush.Body.Entries,
		"loss_markers": f.Input.TelemetryPush.Body.LossMarkers,
	})
	if err != nil {
		t.Fatalf("marshaling expected body: %v", err)
	}
	assertJSONEqual(t, got, wantBody)
}

// TestPushBatchLossMarkersEmptyStillPresent confirms REL-090's "loss_markers
// present (possibly empty) on every push": a PushBatch built with a non-nil
// empty slice marshals loss_markers as [] (matching REL-094's
// telemetry.push, whose single surviving device.heartbeat entry carries an
// explicitly empty loss_markers array), never a bare omitted field or null.
func TestPushBatchLossMarkersEmptyStillPresent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(rel094Corpus))
	if err != nil {
		t.Fatalf("reading REL-094 corpus: %v", err)
	}
	var f struct {
		Input struct {
			TelemetryPush struct {
				Body struct {
					Entries     json.RawMessage `json:"entries"`
					LossMarkers json.RawMessage `json:"loss_markers"`
				} `json:"body"`
			} `json:"telemetry_push"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decoding REL-094 corpus: %v", err)
	}

	batch := PushBatch{
		Entries: []Entry{
			{
				Seq:     2002,
				Schema:  SchemaDeviceHeartbeat,
				Payload: json.RawMessage(`{"device_id":"01J8Z3K4N5P6Q7R8S9T0V1W2YA","power_state":"on","app_state":"app","now_playing_content_id":"01J8Z3K4N5P6Q7R8S9T0V1W2X6"}`),
			},
		},
		LossMarkers: []LossMarker{},
	}

	got, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshaling PushBatch: %v", err)
	}

	wantBody, err := json.Marshal(map[string]json.RawMessage{
		"entries":      f.Input.TelemetryPush.Body.Entries,
		"loss_markers": f.Input.TelemetryPush.Body.LossMarkers,
	})
	if err != nil {
		t.Fatalf("marshaling expected body: %v", err)
	}
	assertJSONEqual(t, got, wantBody)
}

// TestAckMarshalsRel090Shape builds an Ack from Go values matching REL-090's
// expected telemetry.ack body and confirms it marshals to exactly that shape
// — ack_through_seq + loss_markers_acked[].{from_seq,to_seq} (REL-092).
func TestAckMarshalsRel090Shape(t *testing.T) {
	f := loadRel090Fixture(t)

	ack := Ack{
		AckThroughSeq: 1002,
		LossMarkersAcked: []SeqRange{
			{FromSeq: 980, ToSeq: 999},
		},
	}

	got, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshaling Ack: %v", err)
	}

	wantBody, err := json.Marshal(map[string]any{
		"ack_through_seq":    f.Expected.TelemetryAck.Body.AckThroughSeq,
		"loss_markers_acked": f.Expected.TelemetryAck.Body.LossMarkersAcked,
	})
	if err != nil {
		t.Fatalf("marshaling expected body: %v", err)
	}
	assertJSONEqual(t, got, wantBody)
}

// TestLossMarkerHasExactlyFourFields confirms REL-105: the loss-marker
// object carries exactly the four REL-100 fields, no more — a defense
// against an accidental extra exported field silently widening the wire
// shape in a later task.
func TestLossMarkerHasExactlyFourFields(t *testing.T) {
	m := LossMarker{
		FromSeq:               980,
		ToSeq:                 999,
		DroppedCountsBySchema: map[string]int{SchemaContentPlayed: 12},
		Reason:                ReasonBufferExceeded,
	}
	got, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshaling LossMarker: %v", err)
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(got, &asMap); err != nil {
		t.Fatalf("unmarshaling LossMarker into map: %v", err)
	}
	if len(asMap) != 4 {
		t.Fatalf("LossMarker marshaled with %d top-level fields, want 4 (got %s)", len(asMap), got)
	}
	for _, field := range []string{"from_seq", "to_seq", "dropped_counts_by_schema", "reason"} {
		if _, ok := asMap[field]; !ok {
			t.Fatalf("LossMarker JSON missing field %q (got %s)", field, got)
		}
	}
}

// TestEntrySubjectNotOnWire confirms Entry's Subject (the latest-only
// supersession key, an internal Buffer bookkeeping field per REL-094) never
// leaks onto the {seq,schema,payload} wire shape REL-090/095 define.
func TestEntrySubjectNotOnWire(t *testing.T) {
	e := Entry{
		Seq:     2002,
		Schema:  SchemaDeviceHeartbeat,
		Payload: json.RawMessage(`{"device_id":"01J8Z3K4N5P6Q7R8S9T0V1W2YA"}`),
		Subject: "01J8Z3K4N5P6Q7R8S9T0V1W2YA",
	}
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshaling Entry: %v", err)
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(got, &asMap); err != nil {
		t.Fatalf("unmarshaling Entry into map: %v", err)
	}
	if len(asMap) != 3 {
		t.Fatalf("Entry marshaled with %d top-level fields, want 3 (got %s)", len(asMap), got)
	}
	if _, ok := asMap["subject"]; ok {
		t.Fatalf("Entry JSON leaked a subject field onto the wire (got %s)", got)
	}
}
