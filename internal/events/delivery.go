package events

// delivery.go is events/1's binding layer (EVT-090–105): the WS
// hello/hello-ack/event/gap/ping/pong frame types, the binding-selection rule
// that distinguishes a WS upgrade from an SSE request at the shared /events/v1
// path (EVT-001/100), the SSE line framing (EVT-103/104), and the
// server-initiated close reason taxonomy (EVT-096). It is pure frame logic —
// the live WS/SSE socket transport is a deferred thin net/http wrapper; the
// resume/gap RESOLUTION (fresh|resumed|gap against the durable event log) lands
// in resume.go, and binding authentication (EVT-110–114) + the roles-based
// default visible set (EVT-120) are a documented seam pending security-model.
// The selector MECHANICS (EVT-121) reuse internal/shared/apiselector.

import (
	"encoding/json"
	"errors"
	"strings"
)

// Subprotocol is the sole WS subprotocol events/1 negotiates (EVT-090). A WS
// client offering no subprotocol, or a different one, MUST be refused at the
// handshake before any application frame is exchanged.
const Subprotocol = "events.v1+json"

// Frame type discriminators (EVT-091–095) — the "type" field every WS frame
// this contract exchanges carries.
const (
	FrameTypeHello    = "hello"
	FrameTypeHelloAck = "hello-ack"
	FrameTypeEvent    = "event"
	FrameTypeGap      = "gap"
	FrameTypePing     = "ping"
	FrameTypePong     = "pong"
)

// resume_result values (EVT-092) the hello-ack reports: a fresh subscribe
// (no resume_from), a clean resume from the requested point, or a gap (an
// explicit gap frame follows).
const (
	ResumeResultFresh   = "fresh"
	ResumeResultResumed = "resumed"
	ResumeResultGap     = "gap"
)

// Binding discriminators returned by SelectBinding (EVT-001).
const (
	BindingWS  = "ws"
	BindingSSE = "sse"
)

// Server-initiated WS close codes (EVT-096) — the error-taxonomy codes a
// server-closed connection names so a client's reconnect logic can distinguish
// an auth failure from a slow-consumer disconnect from an idle timeout from an
// invalid resume cursor. AUTH_REQUIRED appears here for a mid-connection
// credential revocation (EVT-114); a pre-upgrade auth failure is an HTTP
// Problem, never a frame (EVT-113).
const (
	CloseIdleTimeout       = "IDLE_TIMEOUT"
	CloseAuthRequired      = "AUTH_REQUIRED"
	CloseSlowConsumer      = "SLOW_CONSUMER_DISCONNECTED"
	CloseResumeFromInvalid = "RESUME_FROM_INVALID"
	// closeInternal is the unclassified fallback (Error taxonomy) CloseReason
	// returns for a code outside the server-initiated-close set.
	closeInternal = "INTERNAL"
)

// ErrHelloRequired is returned by ValidateHelloFirst when the first WS frame on
// a newly opened connection is not a hello (EVT-091) — the server MUST close
// such a connection before any other exchange.
var ErrHelloRequired = errors.New("events: first WS frame must be a hello (EVT-091)")

// HelloFrame is the mandatory frame-zero a WS client sends (EVT-091):
// {type:"hello", resume_from?, selector?, schemas?}. resume_from is a
// previously observed, transparent envelope id (EVT-130/131); selector is an
// api/1 label-selector narrowing the visible set (EVT-121); schemas restricts
// delivery to the listed registered schemas (EVT-124). All three are optional.
type HelloFrame struct {
	Type       string   `json:"type"`
	ResumeFrom string   `json:"resume_from,omitempty"`
	Selector   string   `json:"selector,omitempty"`
	Schemas    []string `json:"schemas,omitempty"`
}

// HelloAckFrame is the server's response to a hello (EVT-092):
// {type:"hello-ack", resume_result} where resume_result is fresh | resumed | gap.
type HelloAckFrame struct {
	Type         string `json:"type"`
	ResumeResult string `json:"resume_result"`
}

// EventFrame wraps a delivered durable-event envelope over WS (EVT-093):
// {type:"event", event:<envelope>}.
type EventFrame struct {
	Type  string   `json:"type"`
	Event Envelope `json:"event"`
}

// GapFrame is the WS loss marker (EVT-094/140): {type:"gap", from_id, to_id,
// reason}. from_id is the subscriber's own last-known point (the requested
// resume_from, or the last id delivered before a mid-stream gap) — null only
// when no such point exists, so it is never omitted. reason is retention_expired
// or buffer_exceeded. This {from_id,to_id,reason} shape is the subscriber-stream
// loss marker; a relay's telemetry-buffer marker shares the from/to/reason spine
// but is seq-bounded and per-schema-counted (EVT-144) — not this same shape.
type GapFrame struct {
	Type   string  `json:"type"`
	FromID *string `json:"from_id"`
	ToID   string  `json:"to_id"`
	Reason string  `json:"reason"`
}

// PingFrame / PongFrame are the idle-keepalive frames (EVT-095): either peer MAY
// ping an otherwise-idle connection, and the receiver MUST pong.
type PingFrame struct {
	Type string `json:"type"`
}
type PongFrame struct {
	Type string `json:"type"`
}

// Ping / Pong build the keepalive frames (EVT-095).
func Ping() PingFrame { return PingFrame{Type: FrameTypePing} }
func Pong() PongFrame { return PongFrame{Type: FrameTypePong} }

// Gap builds a WS gap frame (EVT-094/140) with the {from_id,to_id,reason} loss
// marker. fromID is nil when the subscriber has no prior known point (rendered
// as null, never omitted); reason is retention_expired or buffer_exceeded.
func Gap(fromID *string, toID, reason string) GapFrame {
	return GapFrame{Type: FrameTypeGap, FromID: fromID, ToID: toID, Reason: reason}
}

// HelloAck builds the server's hello-ack for a given resume_result (EVT-092).
func HelloAck(resumeResult string) HelloAckFrame {
	return HelloAckFrame{Type: FrameTypeHelloAck, ResumeResult: resumeResult}
}

// NegotiateSubprotocol picks the events/1 subprotocol from a WS client's offered
// list (EVT-090). It returns (Subprotocol, true) only when events.v1+json is
// among the offers; an empty or non-matching list is refused with ("", false),
// so the handshake fails before any frame is exchanged.
func NegotiateSubprotocol(offered []string) (string, bool) {
	for _, o := range offered {
		if o == Subprotocol {
			return Subprotocol, true
		}
	}
	return "", false
}

// SelectBinding chooses the binding at the shared /events/v1 path from the
// request's own shape (EVT-001/100): a WS Upgrade header selects the WS binding
// (which wins over any Accept), an Accept: text/event-stream header with no
// Upgrade selects the SSE binding; a request that is neither selects nothing
// ("").
func SelectBinding(hasUpgrade bool, accept string) string {
	if hasUpgrade {
		return BindingWS
	}
	if strings.Contains(accept, "text/event-stream") {
		return BindingSSE
	}
	return ""
}

// ValidateHelloFirst enforces EVT-091: the first client-to-server WS frame on a
// newly opened connection MUST be a hello. Any other first frame returns
// ErrHelloRequired, and the server MUST close the connection.
func ValidateHelloFirst(frameType string) error {
	if frameType != FrameTypeHello {
		return ErrHelloRequired
	}
	return nil
}

// AckHello resolves the hello-ack for a hello that opens a FRESH subscription —
// one with no resume_from — reporting resume_result: fresh (EVT-092/132) and
// handled=true. A hello carrying a resume_from is NOT acked here: its outcome
// (resumed | gap | RESUME_FROM_INVALID) is resolved against the durable event
// log by Resolve (resume.go, EVT-133/134/140), so AckHello returns
// (HelloAckFrame{}, false) for it rather than silently acking fresh — never
// treating a resume_from as an omitted one (EVT-134).
func AckHello(h HelloFrame) (HelloAckFrame, bool) {
	if h.ResumeFrom != "" {
		return HelloAckFrame{}, false
	}
	return HelloAck(ResumeResultFresh), true
}

// SSEEventLine frames a delivered envelope as an SSE event (EVT-103): an
// event: event field, an id: field carrying the envelope's id (so a browser's
// native reconnect resends it as Last-Event-ID), and a data: field carrying the
// envelope as one JSON line, terminated by the blank line SSE requires. It
// returns an error when the envelope cannot be serialized (e.g. an invalid
// payload) rather than emitting a corrupt line.
func SSEEventLine(env Envelope) (string, error) {
	data, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return "event: " + FrameTypeEvent + "\nid: " + env.ID + "\ndata: " + string(data) + "\n\n", nil
}

// sseGapData is the SSE data: payload for a gap — the {from_id,to_id,reason}
// loss marker without the WS frame's type discriminator (the SSE event: field
// already carries "gap"). Its fields are string / *string only, so marshaling
// it cannot fail, which is why SSEGapLine needs no error return.
type sseGapData struct {
	FromID *string `json:"from_id"`
	ToID   string  `json:"to_id"`
	Reason string  `json:"reason"`
}

// SSEGapLine frames a gap as an SSE event (EVT-104): an event: gap field, an
// id: field carrying the gap's to_id (so a subsequent native reconnect's
// Last-Event-ID lands exactly at the resumed point), and a data: field carrying
// the {from_id,to_id,reason} loss marker. Marshaling the marker cannot fail, so
// this returns a string directly.
func SSEGapLine(g GapFrame) string {
	data, _ := json.Marshal(sseGapData{FromID: g.FromID, ToID: g.ToID, Reason: g.Reason})
	return "event: " + FrameTypeGap + "\nid: " + g.ToID + "\ndata: " + string(data) + "\n\n"
}

// serverCloseCodes is the closed set of error-taxonomy codes a server-initiated
// WS close MAY name (EVT-096).
var serverCloseCodes = map[string]struct{}{
	CloseIdleTimeout:       {},
	CloseAuthRequired:      {},
	CloseSlowConsumer:      {},
	CloseResumeFromInvalid: {},
}

// CloseReason returns the WS close reason for a server-initiated close (EVT-096).
// A recognized server-close code is returned verbatim so the reason NAMES the
// error-taxonomy code a client's reconnect logic branches on; any other code
// falls back to the taxonomy's unclassified INTERNAL, since a server MUST NOT
// close with a code outside the taxonomy.
func CloseReason(code string) string {
	if _, ok := serverCloseCodes[code]; ok {
		return code
	}
	return closeInternal
}
