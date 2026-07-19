package telemetry

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// This file (Task 2) gates the durable Buffer's seq assignment (REL-091),
// durable-class retention (REL-093), and latest-only supersession by
// schema+subject (REL-094/REL-104). The latest-only case is wired to
// REL-094-valid-telemetry-latest-only-heartbeat-superseded.json — the two
// same-device heartbeat payloads and the app peer's expected final view come
// from that corpus document, not invented.

// rel094Heartbeats extracts the two same-device device.heartbeat payloads (the
// older superseded seq-2001 entry and the newer surviving seq-2002 entry) plus
// the app peer's expected final view of the device from the REL-094 corpus.
func rel094Heartbeats(t *testing.T) (older, newer json.RawMessage, finalView json.RawMessage) {
	t.Helper()
	raw, err := os.ReadFile(rel094Corpus)
	if err != nil {
		t.Fatalf("reading REL-094 corpus: %v", err)
	}
	var doc struct {
		Input struct {
			BufferedEntriesSameDevice []struct {
				Seq     int64           `json:"seq"`
				Schema  string          `json:"schema"`
				Payload json.RawMessage `json:"payload"`
			} `json:"buffered_entries_same_device"`
		} `json:"input"`
		Expected struct {
			AppPeerFinalView json.RawMessage `json:"app_peer_final_view_of_device"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshaling REL-094 corpus: %v", err)
	}
	ents := doc.Input.BufferedEntriesSameDevice
	if len(ents) != 2 {
		t.Fatalf("REL-094 corpus: want 2 buffered_entries_same_device, got %d", len(ents))
	}
	for _, e := range ents {
		if e.Schema != SchemaDeviceHeartbeat {
			t.Fatalf("REL-094 corpus entry schema = %q, want %q", e.Schema, SchemaDeviceHeartbeat)
		}
	}
	return ents[0].Payload, ents[1].Payload, doc.Expected.AppPeerFinalView
}

// deviceID pulls the device_id field (the latest-only supersession subject for
// device.heartbeat) out of a heartbeat payload.
func deviceID(t *testing.T, payload json.RawMessage) string {
	t.Helper()
	var p struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("unmarshaling heartbeat payload: %v", err)
	}
	return p.DeviceID
}

// TestRecordAssignsMonotonicSeq confirms REL-091: seq is a per-relay
// monotonically increasing integer assigned at record time, distinct per
// recorded entry regardless of schema.
func TestRecordAssignsMonotonicSeq(t *testing.T) {
	b := NewBuffer(500)
	e1 := b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:aa","screen_id":"s1"}`), "s1", 1_000)
	e2 := b.Record(SchemaAutomationRun, json.RawMessage(`{"rule_id":"r1","mode_disposition":"ran"}`), "r1", 1_001)
	e3 := b.Record(SchemaEntityStateChanged, json.RawMessage(`{"entity_id":"e1"}`), "e1", 1_002)
	if !(e1.Seq < e2.Seq && e2.Seq < e3.Seq) {
		t.Fatalf("seq not monotonic: %d, %d, %d", e1.Seq, e2.Seq, e3.Seq)
	}
	if e1.Seq == 0 {
		t.Fatalf("first assigned seq = 0, want a positive monotonic seq")
	}
}

// TestRecordDurableRetainsAll confirms REL-093: every durable-class entry
// (content.played, automation.run, entity.state_changed) is retained in
// Pending — none is coalesced or superseded.
func TestRecordDurableRetainsAll(t *testing.T) {
	b := NewBuffer(500)
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:aa","screen_id":"s1"}`), "s1", 1_000)
	b.Record(SchemaAutomationRun, json.RawMessage(`{"rule_id":"r1","mode_disposition":"ran"}`), "r1", 1_001)
	b.Record(SchemaEntityStateChanged, json.RawMessage(`{"entity_id":"e1"}`), "e1", 1_002)
	if got := len(b.Pending()); got != 3 {
		t.Fatalf("Pending len = %d, want 3 (durable never coalesced, REL-093)", got)
	}
}

// TestRecordDurableSameSubjectNeverCoalesced confirms REL-093's core
// guarantee: two automation.run entries for the SAME subject both remain —
// durable-class entries are discrete occurrences, never superseded like a
// latest-only snapshot.
func TestRecordDurableSameSubjectNeverCoalesced(t *testing.T) {
	b := NewBuffer(500)
	b.Record(SchemaAutomationRun, json.RawMessage(`{"rule_id":"r1","mode_disposition":"skipped"}`), "r1", 1_000)
	b.Record(SchemaAutomationRun, json.RawMessage(`{"rule_id":"r1","mode_disposition":"ran"}`), "r1", 1_001)
	pending := b.Pending()
	if len(pending) != 2 {
		t.Fatalf("Pending len = %d, want 2 (durable same-subject never coalesced, REL-093)", len(pending))
	}
	if pending[0].Seq >= pending[1].Seq {
		t.Fatalf("Pending not in seq order: %d, %d", pending[0].Seq, pending[1].Seq)
	}
}

// TestRecordLatestOnlySupersedesSameSubject is the REL-094/REL-104 oracle,
// wired to REL-094-valid-telemetry-latest-only-heartbeat-superseded.json: two
// device.heartbeat entries for the same device queue while disconnected; the
// older is discarded when the newer is recorded, leaving only the newer in
// Pending — and that supersession is NOT loss (no marker, corpus asserts
// loss_marker_generated:false). Task 2 has no delivery/loss-marker surface
// yet; here we assert the buffer holds exactly the newer entry and drops the
// older, matching seq_2001_discarded / seq_2001_pushed:false /
// app_peer_final_view_of_device.
func TestRecordLatestOnlySupersedesSameSubject(t *testing.T) {
	older, newer, finalView := rel094Heartbeats(t)
	subject := deviceID(t, newer)
	if subject == "" || subject != deviceID(t, older) {
		t.Fatalf("REL-094 corpus: the two heartbeats must share a device_id subject")
	}

	b := NewBuffer(500)
	eOld := b.Record(SchemaDeviceHeartbeat, older, subject, 2_001)
	eNew := b.Record(SchemaDeviceHeartbeat, newer, subject, 2_002)

	if eNew.Seq <= eOld.Seq {
		t.Fatalf("newer heartbeat seq %d not > older %d", eNew.Seq, eOld.Seq)
	}

	pending := b.Pending()
	if len(pending) != 1 {
		t.Fatalf("Pending len = %d, want 1 (older heartbeat superseded, REL-094)", len(pending))
	}
	survivor := pending[0]
	if survivor.Seq != eNew.Seq {
		t.Fatalf("survivor seq = %d, want the newer entry's seq %d", survivor.Seq, eNew.Seq)
	}
	// The surviving entry MUST carry the newer payload unmodified (REL-090).
	assertJSONEqual(t, survivor.Payload, newer)
	// ...and the app peer's resulting view of the device (REL-094
	// expected.app_peer_final_view_of_device, a status projection of the
	// newer heartbeat) MUST be exactly what the survivor reports.
	assertJSONSubset(t, finalView, survivor.Payload)
}

// assertJSONSubset asserts every key/value in sub appears with an equal value
// in sup (sup may carry additional keys). Used where a corpus expectation is a
// projection of a fuller payload.
func assertJSONSubset(t *testing.T, sub, sup json.RawMessage) {
	t.Helper()
	var subM, supM map[string]any
	if err := json.Unmarshal(sub, &subM); err != nil {
		t.Fatalf("unmarshaling subset JSON: %v", err)
	}
	if err := json.Unmarshal(sup, &supM); err != nil {
		t.Fatalf("unmarshaling superset JSON: %v", err)
	}
	for k, v := range subM {
		gv, ok := supM[k]
		if !ok {
			t.Fatalf("superset missing key %q\n sub: %s\n sup: %s", k, sub, sup)
		}
		if !reflect.DeepEqual(v, gv) {
			t.Fatalf("key %q mismatch: sub=%v sup=%v", k, v, gv)
		}
	}
}

// TestRecordLatestOnlyDifferentSubjectsBothRetained confirms REL-094's
// supersession is keyed on schema AND subject: two device.heartbeat entries
// for DIFFERENT devices both remain — one device's newer status never
// supersedes another device's.
func TestRecordLatestOnlyDifferentSubjectsBothRetained(t *testing.T) {
	b := NewBuffer(500)
	b.Record(SchemaDeviceHeartbeat, json.RawMessage(`{"device_id":"devA","power_state":"on"}`), "devA", 2_001)
	b.Record(SchemaDeviceHeartbeat, json.RawMessage(`{"device_id":"devB","power_state":"on"}`), "devB", 2_002)
	if got := len(b.Pending()); got != 2 {
		t.Fatalf("Pending len = %d, want 2 (distinct subjects not superseded, REL-094)", got)
	}
}

// TestRecordLatestOnlyDifferentSchemaSameSubjectBothRetained confirms the
// supersession key includes the schema: a box.vitals entry does not supersede
// a device.heartbeat entry even for the same subject string — they are
// distinct snapshot streams.
func TestRecordLatestOnlyDifferentSchemaSameSubjectBothRetained(t *testing.T) {
	b := NewBuffer(500)
	b.Record(SchemaDeviceHeartbeat, json.RawMessage(`{"device_id":"x","power_state":"on"}`), "x", 2_001)
	b.Record(SchemaBoxVitals, json.RawMessage(`{"relay_id":"x"}`), "x", 2_002)
	if got := len(b.Pending()); got != 2 {
		t.Fatalf("Pending len = %d, want 2 (different schema, same subject not superseded, REL-094)", got)
	}
}

// TestPendingIsSeqOrderedCopy confirms Pending returns entries in seq order and
// as a copy the caller cannot use to mutate the buffer's internal state.
func TestPendingIsSeqOrderedCopy(t *testing.T) {
	b := NewBuffer(500)
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:aa","screen_id":"s1"}`), "s1", 1_000)
	b.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:bb","screen_id":"s2"}`), "s2", 1_001)
	first := b.Pending()
	if len(first) != 2 || first[0].Seq >= first[1].Seq {
		t.Fatalf("Pending not seq-ordered: %+v", first)
	}
	// Mutating the returned slice must not affect a subsequent Pending call.
	first[0].Seq = -1
	second := b.Pending()
	if second[0].Seq == -1 {
		t.Fatalf("Pending returned an aliased slice; internal state was mutated")
	}
}
