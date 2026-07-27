package wire

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestErrorFrameShapeIsREL007 pins the error frame's on-wire field set to
// REL-007's `{type:"error", id, trace_id, code, message}` (+ relay_id per
// REL-005) — no body key, no extras.
func TestErrorFrameShapeIsREL007(t *testing.T) {
	f := NewErrorFrame("req-1", "trace-1", "relay-abc", "CHANNEL_BINDING_INVALID", "Channel Binding Invalid")

	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal error frame: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal error frame: %v", err)
	}

	want := map[string]any{
		"type":     "error",
		"id":       "req-1",
		"trace_id": "trace-1",
		"relay_id": "relay-abc",
		"code":     "CHANNEL_BINDING_INVALID",
		"message":  "Channel Binding Invalid",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("error frame wire shape = %v, want exactly %v", got, want)
	}
}

// TestNewFrameRoundTrip proves NewFrame + DecodeBody round-trip a typed
// body, and that a bodiless frame marshals without a `body` key.
func TestNewFrameRoundTrip(t *testing.T) {
	since := int64(41)
	f, err := NewFrame(FrameTypeStatePull, "req-2", "relay-abc", StatePullBody{SinceGeneration: &since})
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	f.TraceID = "trace-2"

	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Frame
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Type != FrameTypeStatePull || back.ID != "req-2" || back.RelayID != "relay-abc" || back.TraceID != "trace-2" {
		t.Fatalf("envelope did not round-trip: %+v", back)
	}

	var body StatePullBody
	if err := back.DecodeBody(&body); err != nil {
		t.Fatalf("DecodeBody: %v", err)
	}
	if body.SinceGeneration == nil || *body.SinceGeneration != 41 {
		t.Fatalf("since_generation did not round-trip: %+v", body)
	}
}

// TestBodilessFrameOmitsBodyKey pins that a nil body marshals with NO
// `body` key at all — and that DecodeBody on such a frame fails rather than
// silently zero-filling.
func TestBodilessFrameOmitsBodyKey(t *testing.T) {
	f, err := NewFrame(FrameTypeChallenge, "c-1", "relay-abc", nil)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, has := got["body"]; has {
		t.Fatalf("bodiless frame marshaled a body key: %s", b)
	}

	var v struct{}
	if err := f.DecodeBody(&v); err == nil {
		t.Fatal("DecodeBody on a bodiless frame should fail, got nil")
	}
}

// TestStatePullBodyOmitsAbsentSinceGeneration pins REL-050's MAY: a pull
// with no since_generation claim marshals with the key absent, not null/0.
func TestStatePullBodyOmitsAbsentSinceGeneration(t *testing.T) {
	b, err := json.Marshal(StatePullBody{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "{}" {
		t.Fatalf("empty StatePullBody = %s, want {}", b)
	}
}

// TestStateAckBodyMarshalShape pins REL-054's conditional fields: a
// successful ack marshals applied_generation ONLY (error and
// divergence_reason absent, not null), and a rejected ack carries the
// {code, message} error object plus the implementation-defined
// divergence_reason.
func TestStateAckBodyMarshalShape(t *testing.T) {
	okBytes, err := json.Marshal(StateAckBody{AppliedGeneration: 42})
	if err != nil {
		t.Fatalf("marshal success ack: %v", err)
	}
	if string(okBytes) != `{"applied_generation":42}` {
		t.Fatalf("success ack = %s, want applied_generation only (REL-054)", okBytes)
	}

	rejected := StateAckBody{
		AppliedGeneration: 41,
		Error:             &AckErrorBody{Code: "SNAPSHOT_SIGNATURE_INVALID", Message: "did not verify"},
		DivergenceReason:  "signature-rejected",
	}
	rejBytes, err := json.Marshal(rejected)
	if err != nil {
		t.Fatalf("marshal rejected ack: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(rejBytes, &keys); err != nil {
		t.Fatalf("re-decode rejected ack: %v", err)
	}
	for _, key := range []string{"applied_generation", "error", "divergence_reason"} {
		if _, ok := keys[key]; !ok {
			t.Fatalf("rejected ack %s lacks %q", rejBytes, key)
		}
	}
	var round StateAckBody
	if err := json.Unmarshal(rejBytes, &round); err != nil {
		t.Fatalf("round-trip rejected ack: %v", err)
	}
	if round.Error == nil || round.Error.Code != "SNAPSHOT_SIGNATURE_INVALID" {
		t.Fatalf("round-tripped error = %+v, want SNAPSHOT_SIGNATURE_INVALID", round.Error)
	}
}
