package eventsse

// ws.go is events/1's WebSocket binding (EVT-090–096): the other framing of the
// same stream eventsse.go serves over SSE, on the same path, over the same Hub,
// behind the same authentication and the same per-subscriber filter.
//
// Per-connection sequence:
//
//	authenticate + watch for revocation (ServeHTTP, EVT-110-114 — BEFORE the
//	  upgrade, so a refusal is an HTTP Problem and never a frame, EVT-113)
//	→ negotiate the `events.v1+json` subprotocol (EVT-090) — checked BEFORE
//	  websocket.Accept, so a client offering none is refused at the handshake
//	  rather than negotiated down to the empty subprotocol
//	→ client sends `hello` as frame zero (EVT-091); any other first frame
//	  closes the connection
//	→ server resolves the subscription (`open`: selector, filter, resume) and
//	  answers `hello-ack` with resume_result fresh | resumed | gap (EVT-092)
//	→ a `gap` frame immediately follows a gap ack (EVT-094), then the resolved
//	  backlog, then every live append as an `event` frame (EVT-093)
//	→ steady state: `ping`/`pong` keepalive both directions (EVT-095), and a
//	  server-initiated close always names an error-taxonomy code (EVT-096).
//
// The transport is github.com/coder/websocket — the same one the relay/1
// persistent connection (internal/feeder/relayconn) runs on, and this binding is
// deliberately its sibling: context-governed reads and writes with per-frame
// deadlines, a read limit, a heartbeat that reaps a half-open connection, and a
// proper RFC 6455 close handshake.
//
// # Why the frames a subscriber sends are read on their own goroutine
//
// A subscriber has almost nothing to say — a `hello`, then at most a `ping` or a
// `pong` — but the server MUST answer a client ping within 10 seconds (EVT-095)
// while simultaneously pushing events that arrive at their own pace. A single
// goroutine alternating "read a frame" with "wait for a wake" cannot do both.
// The reader therefore blocks in ws.Read on its own goroutine and hands frames
// to the connection's one writer, which selects over them alongside the Hub
// wake, the revocation channel and the two heartbeat timers.
//
// Reads there are NOT deadline-bounded, deliberately: a subscriber that sends
// nothing for an hour is the normal case for a watch channel, not a fault. What
// bounds the connection is the EVT-095 keepalive — an idle connection is pinged,
// and an unanswered ping closes it IDLE_TIMEOUT — and that is also what reaps a
// half-open socket (a NAT drop, a powered-off client) that would otherwise leak
// this goroutine pair and its Hub registration forever.
//
// # Why every close goes through events.CloseReason
//
// EVT-096 closes the vocabulary of a server-initiated close to the contract's
// own Error taxonomy, so that "a client's reconnect logic can distinguish an
// authentication failure from a slow-consumer disconnect from an ordinary idle
// timeout". Routing every close through CloseReason makes that structural: a
// code outside the taxonomy cannot reach the wire, it becomes the unclassified
// entry instead.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// The WS binding's three time bounds. All three are overridable per handler
// (WithHeartbeat / WithWriteTimeout) so a test drives the behavior on an
// injected cadence rather than waiting out the production one.
const (
	// defaultPingInterval and defaultPongTimeout are EVT-095's own numbers:
	// "Either peer MAY send a ping frame on a connection otherwise idle for 30
	// seconds; the receiving peer MUST respond with pong within 10 seconds or
	// the sender MUST treat the connection as dead and close it."
	defaultPingInterval = 30 * time.Second
	defaultPongTimeout  = 10 * time.Second
	// defaultWriteTimeout bounds any single frame write to a subscriber. It is
	// EVT-142's line between the two slow-consumer responses: while a write
	// still completes, a subscriber that falls behind is GAPPED (Hub.drain's
	// buffer_exceeded marker) and stays connected; once writes themselves stay
	// backed up past this bound, it is disconnected with
	// SLOW_CONSUMER_DISCONNECTED. The contract's own draft-note proposes 30s for
	// that bound; 10s is chosen here to match the sibling relay/1 connection's
	// per-frame write deadline, and is inside the proposal either way.
	defaultWriteTimeout = 10 * time.Second
	// defaultGapDeferral is EVT-142a's bound: how long a mid-stream
	// buffer_exceeded marker may wait for a resume point inside the
	// subscriber's own visible set before the connection is ended rather than
	// left holding a marker it will never be able to name. It is EVT-142's own
	// draft-note number, because it bounds the same condition that
	// requirement's other branch does — a subscriber that has fallen behind
	// live delivery — and one number for one condition is easier to reason
	// about than two.
	defaultGapDeferral = 30 * time.Second
)

// maxInboundFrameBytes bounds one inbound frame. A subscriber sends only
// `hello`, `ping` and `pong` — a `hello` carrying a selector and a schema list
// is the largest of them by orders of magnitude and is still tiny. A frame past
// this bound closes the connection, so a client cannot make the server buffer
// arbitrary memory on a channel it is only supposed to WATCH.
const maxInboundFrameBytes = 64 << 10

// serveWS serves one WS subscriber (EVT-090–096). principal and revoked come
// from ServeHTTP, which authenticated the upgrade request BEFORE this is
// reached: EVT-113 requires a failed authentication to be "rejected before any
// upgrade or stream begins ... never with a WS/SSE-level frame, since no session
// has been established yet to frame one over", and the only way to guarantee
// that is for the upgrade to happen after the check, which is what this
// signature enforces.
func (s *server) serveWS(w http.ResponseWriter, r *http.Request, traceID string, principal auth.Principal, revoked <-chan struct{}) {
	// EVT-090: the subprotocol is checked BEFORE the upgrade. websocket.Accept
	// would otherwise negotiate the empty subprotocol with a client offering
	// none and hand back a live connection, which is precisely the "refused at
	// the WS handshake, before any application-level frame is exchanged" this
	// requirement rules out.
	if !offersSubprotocol(r) {
		apihttp.WriteProblemExt(w, r, traceID, http.StatusBadRequest, codeRequestInvalid, "Bad Request",
			"A WebSocket connection to events/1 must offer the "+events.Subprotocol+" subprotocol.", nil)
		return
	}

	// The default origin check is left ON. It is load-bearing for a
	// cookie-authenticated binding: a WS handshake is not a fetch, so a
	// cross-site page could otherwise open this stream with the user's ambient
	// session cookie attached and read the whole platform's event feed —
	// SEC-024's concern, arriving over a transport the CSRF double-submit token
	// does not cover because there is no mutating request to attach it to.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{events.Subprotocol},
	})
	if err != nil {
		return // Accept already wrote the HTTP-level refusal
	}
	defer ws.CloseNow()
	ws.SetReadLimit(maxInboundFrameBytes)

	s.runWS(r.Context(), ws, principal, revoked)
}

// offersSubprotocol reports whether the upgrade request offers events/1's own
// subprotocol (EVT-090). The offer list is split here; whether it CONTAINS the
// right value is answered by events.NegotiateSubprotocol, which is the
// contract-level rule.
func offersSubprotocol(r *http.Request) bool {
	var offered []string
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(header, ",") {
			offered = append(offered, strings.TrimSpace(p))
		}
	}
	_, ok := events.NegotiateSubprotocol(offered)
	return ok
}

// runWS runs one authenticated connection's whole life. It returns (closing the
// socket) on any refusal, on the client going away, on revocation, or on a
// graceful Hub shutdown.
func (s *server) runWS(reqCtx context.Context, ws *websocket.Conn, principal auth.Principal, revoked <-chan struct{}) {
	ctx, cancel := context.WithCancel(reqCtx)
	defer cancel()

	conn := &wsConn{ws: ws, writeTimeout: s.writeTimeout}
	frames := readFrames(ctx, ws)

	// EVT-091: frame zero MUST be a `hello`. A connection that goes quiet
	// without sending one is not left holding a goroutine pair forever — it
	// draws the same idle close a silent steady-state connection does.
	helloDeadline := time.NewTimer(s.pingInterval + s.pongTimeout)
	defer helloDeadline.Stop()

	// A graceful shutdown (Hub.Close) is deliberately NOT one of the cases here,
	// nor around the backlog below. It is observed only once the steady-state
	// loop is reached, which is exactly where the SSE binding observes it — so a
	// connection that arrives during shutdown still receives the backlog it
	// resolved rather than being cut off at a point that depends on which of two
	// ready channels a select happened to pick.
	var raw []byte
	select {
	case <-ctx.Done():
		return
	case <-revoked:
		conn.closeWith(events.CloseAuthRequired)
		return
	case <-helloDeadline.C:
		conn.closeWith(events.CloseIdleTimeout)
		return
	case f, ok := <-frames:
		if !ok {
			return // the client went away before saying hello
		}
		raw = f
	}

	var hello events.HelloFrame
	if err := json.Unmarshal(raw, &hello); err != nil {
		// Not decodable as a frame at all. INTERNAL is the taxonomy's
		// unclassified entry and the only close code available for a client
		// protocol violation — see events.CloseInternal's own doc.
		conn.closeWith(events.CloseInternal)
		return
	}
	if err := events.ValidateHelloFirst(hello.Type); err != nil {
		conn.closeWith(events.CloseInternal)
		return
	}

	// The subscription: identical resolution to SSE's, from the `hello`'s own
	// fields instead of the query string (EVT-091 vs EVT-101/102).
	sub, outcome, filter, oerr := s.open(ctx, principal, subscribeRequest{
		selector:   hello.Selector,
		schemas:    hello.Schemas,
		resumeFrom: hello.ResumeFrom,
	})
	if oerr != nil {
		// The upgrade already happened, so there is no HTTP response left to
		// carry the Problem SSE would answer with — the refusal is a close
		// naming the same taxonomy code (EVT-096). A malformed or unrecorded
		// resume_from therefore closes RESUME_FROM_INVALID rather than starting
		// a fresh subscription, which is EVT-134's "MUST NOT be treated as
		// equivalent to an omitted resume_from" on this binding.
		conn.closeWith(oerr.code)
		return
	}
	defer sub.close()

	// EVT-092: the hello-ack reports which of the three outcomes the resume
	// resolved to. It is sent BEFORE the backlog, and a gap ack is followed
	// immediately by the gap frame itself (EVT-094).
	if err := conn.write(ctx, events.HelloAck(outcome.Result)); err != nil {
		s.endWS(conn, err)
		return
	}

	sk := &wsSink{conn: conn, ctx: ctx, logf: s.logf}
	st := newStream(hello.ResumeFrom, s.gapDeferral)
	defer st.stop()
	if err := s.deliverBacklog(sk, sub, outcome, st, filter); err != nil {
		s.endWS(conn, err)
		return
	}

	// Steady state. idle fires when the connection has been quiet for the
	// keepalive interval; pong bounds the answer to the ping that fires sends
	// (EVT-095). Both are Timers rather than a Ticker so "idle" means idle
	// SINCE THE LAST TRAFFIC, in either direction, rather than every N seconds
	// regardless.
	idle := time.NewTimer(s.pingInterval)
	defer idle.Stop()
	pong := time.NewTimer(s.pongTimeout)
	pong.Stop()
	defer pong.Stop()
	awaitingPong := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.hub.done:
			// A graceful server shutdown ends every open connection. UNAVAILABLE
			// is the taxonomy's "temporarily unable to serve, retry with
			// backoff", which is exactly what a restarting server is.
			conn.closeWith(events.CloseUnavailable)
			return
		case <-revoked:
			// EVT-114: the credential that authenticated this stream was
			// revoked. Ending the stream is the whole requirement — refusing the
			// NEXT connect would leave this already-open socket delivering
			// platform state to a revoked credential indefinitely. The close
			// names AUTH_REQUIRED so the client re-authenticates rather than
			// reconnecting on the dead credential.
			conn.closeWith(events.CloseAuthRequired)
			return

		case raw, ok := <-frames:
			if !ok {
				return // read error: the client is gone
			}
			var f struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(raw, &f)
			switch f.Type {
			case events.FrameTypePing:
				// EVT-095: "the receiving peer MUST respond with pong".
				if err := conn.write(ctx, events.Pong()); err != nil {
					s.endWS(conn, err)
					return
				}
			case events.FrameTypePong:
				awaitingPong = false
				pong.Stop()
			default:
				// A frame this version does not know is ignored rather than
				// refused: EVT-003 makes minor-version evolution additive, and a
				// subscriber "never mutates state over this contract", so there
				// is nothing an unknown frame could ask for that dropping it
				// gets wrong.
			}
			idle.Reset(s.pingInterval)

		case <-st.expiry():
			// EVT-142a: a mid-stream loss marker whose to_id this subscriber's
			// visible set never supplied. The condition is EVT-142's own — this
			// subscriber fell behind live delivery and undelivered events were
			// dropped — and so is the disposition: the server can no longer
			// serve it conformingly, and the alternatives are naming an id it
			// may not read or leaving it on a stream that has lost something
			// and will never say so.
			conn.closeWith(events.CloseSlowConsumer)
			return

		case <-sub.wake():
			// Reset only on a drain that actually WROTE. A wake means the log
			// grew, not that this subscriber was sent anything — and for one
			// watching a quiet scope beside a busy sibling it usually means the
			// opposite, which would silence the EVT-095 ping exactly when a
			// half-open socket most needs reaping.
			wrote, err := s.drainOnce(sk, st, filter)
			if err != nil {
				s.endWS(conn, err)
				return
			}
			if wrote {
				idle.Reset(s.pingInterval)
			}

		case <-idle.C:
			// EVT-095: the connection has been idle for the interval — ping, and
			// require the pong within the round-trip bound. idle is deliberately
			// NOT reset here: it re-arms on the next traffic in either
			// direction, so one unanswered ping cannot become a stream of them.
			if err := conn.write(ctx, events.Ping()); err != nil {
				s.endWS(conn, err)
				return
			}
			awaitingPong = true
			pong.Reset(s.pongTimeout)

		case <-pong.C:
			if awaitingPong {
				// EVT-095: "the sender MUST treat the connection as dead and
				// close it ... MUST use the IDLE_TIMEOUT code".
				conn.closeWith(events.CloseIdleTimeout)
				return
			}
		}
	}
}

// endWS classifies a failed frame write and closes accordingly (EVT-142).
//
// A write that ran out its own deadline is the slow-consumer case the contract
// names: the subscriber stopped draining its socket, the server's writes backed
// up past the bounded timeout, and the answer is a disconnect with
// SLOW_CONSUMER_DISCONNECTED rather than unbounded buffering. (The other half of
// EVT-142 — gapping rather than disconnecting a subscriber the server can still
// write to — is Hub.drain's buffer_exceeded marker, which happens before this is
// ever reached.)
//
// Any other write error means the connection is ALREADY gone, so there is
// nothing to close and no reason to name: attempting a close handshake on a dead
// socket would just block out the write timeout.
func (s *server) endWS(conn *wsConn, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		conn.closeWith(events.CloseSlowConsumer)
	}
}

// readFrames pumps inbound frames onto a channel until the connection errors or
// ctx ends, closing the channel on the way out so a receiver can tell "the
// client went away" from "no frame yet". See this file's header for why reads
// live on their own goroutine and carry no deadline of their own.
func readFrames(ctx context.Context, ws *websocket.Conn) <-chan []byte {
	out := make(chan []byte)
	go func() {
		defer close(out)
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			select {
			case out <- data:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// wsConn is one live subscriber connection: the socket plus the per-frame write
// deadline every write is bounded by.
//
// Every write goes through this type on the connection's single writer
// goroutine, so frames cannot interleave and the hello-ack → gap → event
// ordering the contract fixes (EVT-092/094/093) is the order they reach the
// wire.
type wsConn struct {
	ws           *websocket.Conn
	writeTimeout time.Duration
}

// write marshals frame and writes it as one UTF-8 JSON text message (EVT-002),
// bounded by the write deadline (EVT-142). ctx is the connection's, so a client
// disconnect aborts an in-flight write rather than waiting the deadline out.
func (c *wsConn) write(ctx context.Context, frame any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, c.writeTimeout)
	defer cancel()
	return c.ws.Write(wctx, websocket.MessageText, data)
}

// closeWith performs the close handshake naming code (EVT-096). The reason
// carried is EXACTLY the error-taxonomy code — nothing else — so a client reads
// it as the machine-readable discriminant the requirement describes rather than
// parsing prose out of it. events.CloseReason is what guarantees the value is a
// published code: anything outside the taxonomy becomes its unclassified entry.
//
// Best-effort by nature: a connection torn down under the server (a killed
// client, a stalled write) cannot complete a close handshake, and the error is
// discarded because there is no further recovery available at that point.
func (c *wsConn) closeWith(code string) {
	reason := events.CloseReason(code)
	_ = c.ws.Close(wsStatusFor(reason), reason)
}

// wsStatusFor maps an error-taxonomy code to the RFC 6455 status code the close
// frame carries alongside it. The taxonomy code in the REASON is what EVT-096
// makes normative and what a client branches on; the numeric status is the
// transport-level shape a generic WS client (or an intermediary) sees, so it is
// chosen to agree rather than to add a second vocabulary.
func wsStatusFor(reason string) websocket.StatusCode {
	switch reason {
	case events.CloseIdleTimeout, events.CloseUnavailable:
		// The server is ending a connection it has no complaint about.
		return websocket.StatusGoingAway
	case events.CloseAuthRequired, events.CloseSlowConsumer,
		events.CloseResumeFromInvalid, events.CloseSelectorInvalid:
		return websocket.StatusPolicyViolation
	default:
		return websocket.StatusInternalError
	}
}

// wsSink is the WS binding's frame writer (EVT-093/094) — the only part of
// delivery that differs from SSE's.
//
// Unlike the SSE sink its writes CAN fail meaningfully, and it reports them:
// a write that exhausts its deadline is EVT-142's slow consumer, which the
// caller turns into a SLOW_CONSUMER_DISCONNECTED close. An envelope that fails
// to serialize is logged and SKIPPED rather than written as a corrupt frame,
// exactly as the SSE sink does — one bad record cannot poison either stream.
type wsSink struct {
	conn *wsConn
	ctx  context.Context
	logf func(format string, args ...any)
}

func (s *wsSink) event(env events.Envelope) error {
	frame := events.Event(env)
	if _, err := json.Marshal(frame); err != nil {
		s.logf("eventsse: dropping unserializable event id %q: %v (EVT-093)", env.ID, err)
		return nil
	}
	return s.conn.write(s.ctx, frame)
}

func (s *wsSink) gap(g events.GapFrame) error {
	return s.conn.write(s.ctx, g)
}

// flush is a no-op: a WS write is already a discrete frame on the wire, with no
// buffering layer between it and the socket for a flush to push through.
func (s *wsSink) flush() error { return nil }
