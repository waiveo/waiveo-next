package ctxproto

import (
	"fmt"
	"io"

	"github.com/vmihailenco/msgpack/v5"
)

// The ctx/1 message envelope (CTX-003): the shape every frame's payload carries,
// sitting directly on the framing layer in frame.go.
//
// This layer decodes bytes a PACK PROCESS sent. Everything here treats that
// input as hostile: a field of the wrong type, a missing key, or a `type` outside
// the closed set is MALFORMED_FRAME (CTX-004), never a zero value quietly
// carried forward into a verb dispatch.

// Frame types — the closed set CTX-003 defines for the `type` key.
const (
	TypeRequest  = "request"
	TypeResponse = "response"
	TypeEvent    = "event"
	TypeError    = "error"
)

// Message is one decoded ctx/1 envelope.
//
// Body is left as a raw map rather than a typed per-verb struct: this package
// owns the ENVELOPE, and the verb families own their own bodies. Decoding a body
// here would put every family's shape in the transport layer and make adding a
// verb a change to the codec.
type Message struct {
	Type    string
	ID      string
	TraceID string
	Verb    string
	Body    map[string]any
	// Code and Text carry an `error` frame's payload, which CTX-003 puts in
	// place of `body` rather than inside it.
	Code string
	Text string
}

// wireMessage is the DECODE target. `omitempty` is deliberately absent from
// `body`: decoding must be able to tell an absent body from an empty one, and
// on this side it can — an absent key leaves the field nil, while `body: {}`
// decodes to an empty non-nil map.
//
// Encoding does NOT use this struct; see EncodeMessage for why.
type wireMessage struct {
	Type    string         `msgpack:"type"`
	ID      string         `msgpack:"id"`
	TraceID string         `msgpack:"trace_id"`
	Verb    string         `msgpack:"verb,omitempty"`
	Body    map[string]any `msgpack:"body,omitempty"`
	Code    string         `msgpack:"code,omitempty"`
	Text    string         `msgpack:"message,omitempty"`
}

// validTypes is the closed `type` set. A map rather than a slice so an unknown
// type is one lookup, and so adding a type is a change here rather than in every
// switch that reads one.
var validTypes = map[string]bool{
	TypeRequest: true, TypeResponse: true, TypeEvent: true, TypeError: true,
}

// EncodeMessage renders m as a frame payload.
//
// The envelope is validated on the way OUT as well as in. A host that emitted a
// malformed frame would be asking its peer to close the connection (CTX-004),
// and finding that out from the peer's disconnect is far worse than finding it
// out here.
//
// # Why the map is built by hand rather than marshalled from a struct
//
// `omitempty` cannot express CTX-003's rule. The keys a frame carries depend on
// its `type`, not on whether their values happen to be empty — and an EMPTY BODY
// IS A LEGAL BODY. A verb that takes no arguments sends `body: {}`, which
// `omitempty` drops; the receiver then sees a request with no body at all and
// refuses the frame as malformed. That is a message a peer can send and nothing
// can read, and it was caught here by a test doing exactly what a real
// no-argument verb will do.
//
// So the presence of each key is decided by the type, explicitly.
func EncodeMessage(m Message) ([]byte, error) {
	if err := validateEnvelope(m); err != nil {
		return nil, err
	}
	payload := map[string]any{"type": m.Type, "id": m.ID, "trace_id": m.TraceID}
	switch m.Type {
	case TypeRequest, TypeEvent:
		payload["verb"] = m.Verb
		payload["body"] = m.Body
	case TypeResponse:
		payload["body"] = m.Body
	case TypeError:
		// No `body` key at all: CTX-003 puts code/message in its place, and an
		// error frame also carrying an empty body would be claiming a payload
		// shape the contract says it does not have.
		payload["code"] = m.Code
		payload["message"] = m.Text
	}
	out, err := msgpack.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ctxproto: encode message: %w", err)
	}
	return out, nil
}

// DecodeMessage parses a frame payload into a Message, enforcing CTX-003's
// minimum envelope. Every failure is MALFORMED_FRAME, which CTX-004 tells the
// caller to answer by closing the connection rather than resynchronizing.
func DecodeMessage(payload []byte) (Message, error) {
	var w wireMessage
	if err := msgpack.Unmarshal(payload, &w); err != nil {
		return Message{}, frameErr(CodeMalformed, "payload is not a decodable msgpack map (CTX-003): %v", err)
	}
	m := Message{
		Type: w.Type, ID: w.ID, TraceID: w.TraceID,
		Verb: w.Verb, Body: w.Body, Code: w.Code, Text: w.Text,
	}
	if err := validateEnvelope(m); err != nil {
		return Message{}, err
	}
	return m, nil
}

// validateEnvelope enforces CTX-003's per-type required keys.
//
// The rules are asserted POSITIVELY (this type requires these fields) rather
// than by rejecting known-bad combinations: a negative list silently admits
// every shape nobody thought of, which for a transport reading another process's
// output is the whole risk.
func validateEnvelope(m Message) error {
	if !validTypes[m.Type] {
		return frameErr(CodeMalformed,
			"frame type %q is outside the closed set request|response|event|error (CTX-003)", m.Type)
	}
	if m.ID == "" {
		return frameErr(CodeMalformed, "frame carries no correlation id (CTX-003)")
	}
	if m.TraceID == "" {
		return frameErr(CodeMalformed, "frame carries no trace_id (CTX-003)")
	}

	switch m.Type {
	case TypeRequest, TypeEvent:
		// `verb` and `body` are both required. A verbless request is
		// undispatchable, and a bodyless one is indistinguishable from a request
		// whose body failed to encode — the receiver must not have to guess which.
		if m.Verb == "" {
			return frameErr(CodeMalformed, "a %s frame carries no verb (CTX-003)", m.Type)
		}
		if m.Body == nil {
			return frameErr(CodeMalformed, "a %s frame carries no body (CTX-003)", m.Type)
		}
	case TypeResponse:
		if m.Body == nil {
			return frameErr(CodeMalformed, "a response frame carries no body (CTX-003)")
		}
	case TypeError:
		// `code` and `message` sit in place of `body` (CTX-003). A code-less
		// error is one a caller cannot branch on, which defeats a taxonomy whose
		// entire purpose is machine-readable refusals.
		if m.Code == "" {
			return frameErr(CodeMalformed, "an error frame carries no code (CTX-003)")
		}
		if m.Text == "" {
			return frameErr(CodeMalformed, "an error frame carries no message (CTX-003)")
		}
	}
	return nil
}

// WriteMessage encodes m and writes it as one frame.
func WriteMessage(w io.Writer, m Message) error {
	payload, err := EncodeMessage(m)
	if err != nil {
		return err
	}
	return WriteFrame(w, payload)
}

// ReadMessage reads one frame and decodes its envelope.
func ReadMessage(r io.Reader) (Message, error) {
	payload, err := ReadFrame(r)
	if err != nil {
		return Message{}, err
	}
	return DecodeMessage(payload)
}
