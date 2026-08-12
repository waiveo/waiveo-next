package ctxproto

import (
	"bytes"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

// A request round-trips through the frame layer with its envelope intact, and
// the wire form uses the KEY NAMES CTX-003 specifies. The key names are asserted
// against a raw msgpack decode rather than through this package's own decoder:
// a codec that renames `trace_id` and reads it back is self-consistent and talks
// to nothing.
func TestARequestRoundTripsUnderTheSpecifiedKeyNames(t *testing.T) {
	m := Message{
		Type: TypeRequest, ID: "01J8Z2Q1A0000000000000000A", TraceID: "01J8Z2Q1B0000000000000000B",
		Verb: "data.read", Body: map[string]any{"collection": "menu_items"},
	}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	// Peek at the raw payload: strip the 4-byte frame prefix and decode as a
	// bare map, so the assertion is about the WIRE, not about our struct tags
	// agreeing with themselves.
	var raw map[string]any
	if err := msgpack.Unmarshal(buf.Bytes()[prefixBytes:], &raw); err != nil {
		t.Fatalf("payload is not a msgpack map: %v", err)
	}
	for _, key := range []string{"type", "id", "trace_id", "verb", "body"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("payload has no %q key; got keys %v (CTX-003)", key, keysOf(raw))
		}
	}

	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got.Type != m.Type || got.ID != m.ID || got.TraceID != m.TraceID || got.Verb != m.Verb {
		t.Fatalf("envelope round-tripped as %+v, want %+v", got, m)
	}
	if got.Body["collection"] != "menu_items" {
		t.Fatalf("body = %v, want collection=menu_items", got.Body)
	}
}

// An error frame carries code+message IN PLACE OF body (CTX-003) — it must not
// be forced to invent an empty body, and its code must survive the trip.
func TestAnErrorFrameCarriesCodeAndMessageInPlaceOfBody(t *testing.T) {
	var buf bytes.Buffer
	err := WriteMessage(&buf, Message{
		Type: TypeError, ID: "01J8Z2Q1A0000000000000000A", TraceID: "01J8Z2Q1B0000000000000000B",
		Code: CodeIncompatibleRange, Text: "no host version satisfies the range",
	})
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got.Code != CodeIncompatibleRange || got.Text == "" {
		t.Fatalf("error frame = %+v, want the code and message preserved", got)
	}
	if got.Body != nil {
		t.Fatalf("error frame carries a body %v; CTX-003 puts code/message in its place", got.Body)
	}
}

// CTX-003's minimum envelope, one violation at a time. Every one is
// MALFORMED_FRAME — a decoder that returned a zero value for any of these would
// hand a verb dispatch an untyped, unattributed message from another process.
//
// The fixtures are explicit maps, NOT this package's wireMessage struct. Built
// through the struct they were worthless: `body,omitempty` silently dropped the
// empty body from every case, so the "no trace_id" case was actually being
// refused for a missing BODY and would have passed with the trace_id check
// deleted. A mutation caught that. A fixture must violate exactly the one rule
// its name claims.
func TestTheEnvelopeMinimumIsEnforcedFieldByField(t *testing.T) {
	const id, trace = "01J8Z2Q1A0000000000000000A", "01J8Z2Q1B0000000000000000B"
	full := func(over map[string]any, drop ...string) map[string]any {
		m := map[string]any{"type": TypeRequest, "id": id, "trace_id": trace,
			"verb": "data.read", "body": map[string]any{}}
		for k, v := range over {
			m[k] = v
		}
		for _, k := range drop {
			delete(m, k)
		}
		return m
	}
	cases := []struct {
		name string
		wire map[string]any
	}{
		{"type outside the closed set", full(map[string]any{"type": "teleport"})},
		{"no type at all", full(nil, "type")},
		{"no correlation id", full(nil, "id")},
		{"no trace id", full(nil, "trace_id")},
		{"request without a verb", full(nil, "verb")},
		{"request without a body", full(nil, "body")},
		{"event without a verb", full(map[string]any{"type": TypeEvent}, "verb")},
		{"event without a body", full(map[string]any{"type": TypeEvent}, "body")},
		{"response without a body", full(map[string]any{"type": TypeResponse}, "verb", "body")},
		{"error without a code", full(map[string]any{"type": TypeError, "message": "something"}, "verb", "body")},
		{"error without a message", full(map[string]any{"type": TypeError, "code": "SOME_CODE"}, "verb", "body")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := msgpack.Marshal(tc.wire)
			if err != nil {
				t.Fatalf("marshal fixture: %v", err)
			}
			_, err = DecodeMessage(payload)
			assertFrameCode(t, err, CodeMalformed)
		})
	}
}

// The fixture builder itself produces a VALID envelope, so every case above is
// proven to fail because of the one thing it changed rather than because the
// baseline was broken all along.
func TestTheEnvelopeFixtureBaselineIsValid(t *testing.T) {
	payload, err := msgpack.Marshal(map[string]any{
		"type": TypeRequest, "id": "01J8Z2Q1A0000000000000000A",
		"trace_id": "01J8Z2Q1B0000000000000000B", "verb": "data.read", "body": map[string]any{},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := DecodeMessage(payload); err != nil {
		t.Fatalf("the unmodified fixture must decode cleanly, else every case above proves nothing: %v", err)
	}
}

// A payload that is not msgpack at all, and one that is msgpack but not a map,
// are both malformed rather than a panic or a zero-valued Message.
func TestNonMsgpackAndNonMapPayloadsAreMalformed(t *testing.T) {
	_, err := DecodeMessage([]byte("this is not msgpack at all, it is plain text"))
	assertFrameCode(t, err, CodeMalformed)

	arr, _ := msgpack.Marshal([]string{"a", "map", "this", "is", "not"})
	_, err = DecodeMessage(arr)
	assertFrameCode(t, err, CodeMalformed)
}

// The encoder validates on the way OUT too. A host that framed a malformed
// message would be asking its peer to close the connection (CTX-004), and
// learning that from the peer's disconnect is far worse than learning it here.
func TestEncodingRefusesAMalformedEnvelopeBeforeItReachesTheWire(t *testing.T) {
	var buf bytes.Buffer
	err := WriteMessage(&buf, Message{Type: TypeRequest, ID: "", TraceID: "t", Verb: "v", Body: map[string]any{}})
	assertFrameCode(t, err, CodeMalformed)
	if buf.Len() != 0 {
		t.Fatalf("a refused encode put %d bytes on the wire", buf.Len())
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Regression: an EMPTY body is a legal body, and must survive the round trip.
//
// The first encoder marshalled a struct with `body,omitempty`, which dropped
// `body: {}` from the wire; the receiver then refused the frame as malformed.
// That is a message a peer can send and nothing can read — and every verb that
// takes no arguments sends exactly it.
func TestAnEmptyBodyIsALegalBodyAndSurvivesTheWire(t *testing.T) {
	for _, typ := range []string{TypeRequest, TypeEvent, TypeResponse} {
		t.Run(typ, func(t *testing.T) {
			m := Message{
				Type: typ, ID: "01J8Z2Q1A0000000000000000A", TraceID: "01J8Z2Q1B0000000000000000B",
				Body: map[string]any{},
			}
			if typ != TypeResponse {
				m.Verb = "health.report"
			}
			var buf bytes.Buffer
			if err := WriteMessage(&buf, m); err != nil {
				t.Fatalf("WriteMessage: %v", err)
			}
			got, err := ReadMessage(&buf)
			if err != nil {
				t.Fatalf("a %s with an empty body must read back: %v", typ, err)
			}
			if got.Body == nil {
				t.Fatalf("empty body read back as nil — absent and empty must stay distinguishable")
			}
			if len(got.Body) != 0 {
				t.Fatalf("body = %v, want empty", got.Body)
			}
		})
	}
}

// An error frame must NOT carry a body key even when one was set on the struct:
// CTX-003 puts code/message in its place, and a body would claim a payload shape
// the contract says the frame does not have.
func TestAnErrorFrameNeverPutsABodyOnTheWire(t *testing.T) {
	payload, err := EncodeMessage(Message{
		Type: TypeError, ID: "01J8Z2Q1A0000000000000000A", TraceID: "01J8Z2Q1B0000000000000000B",
		Code: "SOME_CODE", Text: "something went wrong", Body: map[string]any{"stray": true},
	})
	if err != nil {
		t.Fatalf("EncodeMessage: %v", err)
	}
	var raw map[string]any
	if err := msgpack.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := raw["body"]; present {
		t.Fatalf("error frame put a body on the wire: keys %v", keysOf(raw))
	}
}
