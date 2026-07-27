// frame.go is relay/1's generic connection-frame envelope — the one JSON
// shape every message on the persistent app-peer connection rides in
// (REL-002: one UTF-8 JSON message per underlying connection frame).
// Like the rest of this package it is contract-derived data shape only —
// no handshake sequencing, no transport; the WS carriage lives in
// internal/feeder/relayconn (app-peer server) and internal/relay/relayconn
// (relay dialer), mirroring internal/events/delivery.go's pure-frame idiom.
//
// Envelope fields, per contracts/relay-1.md:
//   - `type` — the message verb (REL-002).
//   - `id` — request/response correlation: a response echoes its request's
//     id (REL-006).
//   - `relay_id` — REQUIRED on every message exchanged after enrollment
//     completes (REL-005); the enrollment exchange itself is the sole
//     exception.
//   - `trace_id` — carried when the message's work traces to a single
//     originating operation elsewhere in the platform (REL-006).
//   - `code`/`message` — top-level error-frame fields (REL-007:
//     `{type:"error", id, trace_id, code, message}`, aligned with ctx/1's
//     own frame envelope); empty on every non-error frame.
//   - `body` — the verb's own payload, opaque to the envelope.
package wire

import (
	"encoding/json"
	"fmt"
)

// Subprotocol is the WS subprotocol the relay/1 persistent connection
// negotiates at its REL-001 upgrade, mirroring events/1's "events.v1+json"
// (internal/events.Subprotocol). A client offering no subprotocol, or a
// different one, is refused at the handshake before any frame is exchanged.
const Subprotocol = "relay.v1+json"

// Frame type discriminators — the `type` field of every relay/1 connection
// frame. The state.* verbs are contracts/relay-1.md's own wire shapes
// (REL-050/051, REL-057); challenge/hello/hello-ack are the handshake's
// (REL-030/031/039); "error" is REL-007's top-level error frame.
//
// FrameTypeStateChanged is REL-057's server-initiated nudge announcing a new
// desired-state generation, to which a relay responds with its own
// state.pull (or nothing, when already at that generation) — keeping
// REL-050's "the app peer never sends an unsolicited snapshot" intact while
// killing the poll loop. Delivery is best-effort and coalescible; a lost
// nudge is recovered by the relay's next pull.
const (
	FrameTypeChallenge      = "challenge"
	FrameTypeHello          = "hello"
	FrameTypeHelloAck       = "hello-ack"
	FrameTypeStatePull      = "state.pull"
	FrameTypeStateSnapshot  = "state.snapshot"
	FrameTypeStateUnchanged = "state.unchanged"
	FrameTypeStateChanged   = "state.changed"
	FrameTypeStateAck       = "state.ack"
	FrameTypeError          = "error"
)

// Frame is the relay/1 connection envelope (package doc above). Zero-valued
// fields marshal as absent keys, so a frame carries exactly the envelope
// fields its verb needs — e.g. an error frame carries code/message and no
// body, and a challenge carries no trace_id.
type Frame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	RelayID string          `json:"relay_id,omitempty"`
	TraceID string          `json:"trace_id,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
}

// NewFrame builds a Frame of the given type/id/relay_id whose Body is body
// marshaled — the one place envelope assembly happens, so both roles build
// byte-compatible frames. A nil body leaves Body absent. Callers stamp
// TraceID themselves when the frame's work traces to an originating
// operation (REL-006).
func NewFrame(frameType, id, relayID string, body any) (Frame, error) {
	f := Frame{Type: frameType, ID: id, RelayID: relayID}
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return Frame{}, fmt.Errorf("wire: NewFrame: marshal %s body: %w", frameType, err)
		}
		f.Body = raw
	}
	return f, nil
}

// NewErrorFrame builds REL-007's top-level error frame:
// `{type:"error", id, trace_id, code, message}` — plus relay_id when the
// refusing peer knows the authenticated relay identity (REL-005 requires it
// on every post-enrollment message; a pre-authentication refusal may have
// none to stamp).
func NewErrorFrame(id, traceID, relayID, code, message string) Frame {
	return Frame{
		Type:    FrameTypeError,
		ID:      id,
		TraceID: traceID,
		RelayID: relayID,
		Code:    code,
		Message: message,
	}
}

// DecodeBody unmarshals f's Body into v, failing on an absent body — a
// frame whose verb requires a payload but carries none is malformed, never
// silently zero-valued.
func (f Frame) DecodeBody(v any) error {
	if len(f.Body) == 0 {
		return fmt.Errorf("wire: DecodeBody: %s frame carries no body", f.Type)
	}
	if err := json.Unmarshal(f.Body, v); err != nil {
		return fmt.Errorf("wire: DecodeBody: decode %s body: %w", f.Type, err)
	}
	return nil
}

// StatePullBody is `state.pull`'s body (REL-050): since_generation — the
// relay's own last-applied generation — is OPTIONAL (MAY carry), letting the
// app peer answer state.unchanged when nothing has changed. A nil
// SinceGeneration marshals as an absent key: "no generation claimed", which
// always draws a full snapshot.
type StatePullBody struct {
	SinceGeneration *int64 `json:"since_generation,omitempty"`
}

// StateUnchangedBody is `state.unchanged`'s body (REL-051): the current
// generation since_generation already named. Deliberately generation-only —
// no sections, no hash — the whole point of the verb.
type StateUnchangedBody struct {
	Generation int64 `json:"generation"`
}

// StateChangedBody is `state.changed`'s body (REL-057, FrameTypeStateChanged):
// the generation the app peer now holds, so a relay already at that
// generation can skip the pull entirely.
type StateChangedBody struct {
	Generation int64 `json:"generation"`
}

// StateAckBody is `state.ack`'s body (REL-054): the relay's acknowledgment
// after applying a snapshot. `error` and `divergence_reason` are present
// ONLY when the apply did not fully succeed (both marshal absent on
// success); `applied_generation` is the relay's actual last-applied
// generation — on a rejected snapshot (REL-072) that is the UNADVANCED
// prior generation, never the rejected one. The ack's envelope carries the
// correlation id of the state.pull exchange that delivered the
// acknowledged snapshot (REL-054).
type StateAckBody struct {
	AppliedGeneration int64         `json:"applied_generation"`
	Error             *AckErrorBody `json:"error,omitempty"`
	DivergenceReason  string        `json:"divergence_reason,omitempty"`
}

// AckErrorBody is the `{code, message}` object an unsuccessful apply's
// `state.ack.error` carries — `code` an Error-taxonomy value (e.g.
// SNAPSHOT_SIGNATURE_INVALID), exactly the contract's own rejected-ack
// wire shape.
type AckErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
