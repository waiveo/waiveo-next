// Package relayconn is the app-peer (feeder) side of the relay/1 persistent
// connection SPIKE: one WebSocket endpoint at the contract's own stable path
// (/relay/v1, REL-001 — the claim-credential example's
// wss://…/relay/v1), upgraded from the feeder's existing HTTPS listener,
// exchanging exactly one UTF-8 JSON message per WS message (REL-002,
// wire.Frame).
//
// Per-connection sequence:
//
//	TLS (mutual: the relay presents its enrollment-issued leaf; the listener
//	verifies it against the enrollment CA, enroll.Server.ClientCAPool)
//	→ server sends `challenge` — nonce DERIVED from this connection's TLS
//	  exporter keying material (REL-040, hello.ExporterChallengeNonce)
//	→ relay sends `hello` (channel-binding signature over that nonce)
//	→ server verifies against the enrollment key looked up by the mTLS
//	  client-cert identity — the certificate's CommonName, never the
//	  self-asserted relay_id (REL-041; a mismatched hello.relay_id is
//	  refused RELAY_IDENTITY_MISMATCH) — negotiates, answers `hello-ack`
//	→ steady state: `state.pull` answered with `state.snapshot` or
//	  `state.unchanged` (REL-050/051), and generation-advance notifications
//	  pushed server→relay (NotifyGenerationAdvance).
//
// Any pre-hello frame that is not `hello` is refused with a top-level error
// frame (PROTOCOL_VIOLATION) and the connection is closed (REL-039 posture);
// every post-enrollment frame the server sends carries relay_id (REL-005)
// and echoes its request's id + trace_id (REL-006/007).
//
// SPIKE CORNERS (deliberate, recorded): no read deadlines, no ping/pong
// heartbeat, no write flow control (a stalled relay blocks
// NotifyGenerationAdvance on that conn's write mutex), no per-connection
// limit, unknown frame types are silently ignored (REL-004 forward
// tolerance without the contract's ack story).
package relayconn

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"golang.org/x/net/websocket"

	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// SnapshotProvider returns the feeder's CURRENT signed desired-state
// snapshot — the same func the HTTP pull endpoint serves
// (enroll.Server.SetSnapshotProvider's provider, e.g. cmd/waiveo-feeder's
// generation-cached desiredStateSource.current).
type SnapshotProvider func() (wire.StateSnapshotBody, error)

// Server serves /relay/v1. Safe for concurrent connections; its live-conn
// set is mutex-guarded.
type Server struct {
	provider           SnapshotProvider
	lookup             hello.RelayKeyLookup
	site               hello.SiteBinding
	implementedMinors  []string
	recognizedFeatures []string

	// pushSnapshots switches NotifyGenerationAdvance from the REL-050-
	// conformant `state.changed` nudge to pushing the full `state.snapshot`
	// unsolicited — which VIOLATES REL-050 as written ("the app peer never
	// sends an unsolicited snapshot"). Spike-only, to measure the
	// zero-round-trip variant; adopting it requires relaxing REL-050.
	pushSnapshots bool

	mu    sync.Mutex
	conns map[*serverConn]struct{}
}

// Option configures a Server.
type Option func(*Server)

// WithDirectSnapshotPush makes NotifyGenerationAdvance push the full
// state.snapshot frame instead of a state.changed nudge. NOT REL-050
// conformant — see Server.pushSnapshots.
func WithDirectSnapshotPush() Option {
	return func(s *Server) { s.pushSnapshots = true }
}

// New builds the app peer's /relay/v1 server. provider serves state.pull;
// lookup resolves a relay's enrollment public key by its mTLS-authenticated
// identity (REL-041 — enroll.Server.RelayEnrollmentKey); site/minors/
// features feed hello negotiation exactly as hello.NewAppPeerServer's do.
func New(
	provider SnapshotProvider,
	lookup hello.RelayKeyLookup,
	site hello.SiteBinding,
	implementedMinors []string,
	recognizedFeatures []string,
	opts ...Option,
) *Server {
	s := &Server{
		provider:           provider,
		lookup:             lookup,
		site:               site,
		implementedMinors:  implementedMinors,
		recognizedFeatures: recognizedFeatures,
		conns:              map[*serverConn]struct{}{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Handler returns the http.Handler to mount at /relay/v1. The WS handshake
// refuses a client that does not offer the relay/1 subprotocol
// (wire.Subprotocol) before any frame is exchanged.
func (s *Server) Handler() http.Handler {
	return websocket.Server{
		Handshake: func(config *websocket.Config, req *http.Request) error {
			if !slices.Contains(config.Protocol, wire.Subprotocol) {
				return fmt.Errorf("relayconn: client did not offer subprotocol %q", wire.Subprotocol)
			}
			config.Protocol = []string{wire.Subprotocol}
			return nil
		},
		Handler: s.serve,
	}
}

// serverConn is one live, authenticated relay connection. writeMu
// serializes frame writes (request replies vs. NotifyGenerationAdvance
// pushes race otherwise — x/net/websocket permits one concurrent writer).
type serverConn struct {
	ws      *websocket.Conn
	relayID string
	writeMu sync.Mutex
}

func (c *serverConn) send(f wire.Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return websocket.JSON.Send(c.ws, f)
}

// serve runs one connection's whole life: identity, challenge, hello,
// steady state. It returns (closing the conn) on any refusal or transport
// error.
func (s *Server) serve(ws *websocket.Conn) {
	defer ws.Close()

	req := ws.Request()
	tlsState := req.TLS

	// REL-003/041: the connection MUST be mutually authenticated, and the
	// relay's identity is the mTLS client certificate's — never the
	// self-asserted relay_id. The listener already verified the chain
	// against the enrollment CA (ClientCAs); no cert at all means this
	// connection cannot be authenticated. No relay_id is known to stamp on
	// this refusal (REL-005's stamp needs an authenticated identity first).
	if tlsState == nil || len(tlsState.PeerCertificates) == 0 {
		_ = websocket.JSON.Send(ws, wire.NewErrorFrame("", "", "",
			"CHANNEL_BINDING_INVALID", "client certificate required (REL-041)"))
		return
	}
	relayID := tlsState.PeerCertificates[0].Subject.CommonName

	pub, ok := s.lookup(relayID)
	if !ok {
		_ = websocket.JSON.Send(ws, wire.NewErrorFrame("", "", relayID,
			"CHANNEL_BINDING_INVALID", "Channel Binding Invalid"))
		return
	}

	// REL-030/040: the challenge nonce derives from THIS connection's TLS
	// exporter keying material — per-connection by construction, no
	// outstanding-nonce bookkeeping to race (the one-global-slot hazard of
	// the two-request HTTP handshake disappears entirely).
	nonce, err := hello.ExporterChallengeNonce(tlsState)
	if err != nil {
		_ = websocket.JSON.Send(ws, wire.NewErrorFrame("", "", relayID,
			"INTERNAL", "challenge derivation failed"))
		return
	}
	challenge, err := wire.NewFrame(wire.FrameTypeChallenge, ulid.New(), relayID,
		hello.ChallengeBody{Nonce: nonce})
	if err != nil {
		return
	}
	conn := &serverConn{ws: ws, relayID: relayID}
	if err := conn.send(challenge); err != nil {
		return
	}

	// First relay frame MUST be hello (REL-031/039): anything else is a
	// protocol violation, refused with a top-level error frame and a close.
	var f wire.Frame
	if err := websocket.JSON.Receive(ws, &f); err != nil {
		return
	}
	if f.Type != wire.FrameTypeHello {
		_ = conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
			"PROTOCOL_VIOLATION", fmt.Sprintf("first frame must be hello, got %q", f.Type)))
		return
	}
	// REL-041: the self-asserted relay_id is CHECKED against the mTLS
	// identity, never trusted. A mismatch is its own typed refusal.
	if f.RelayID != relayID {
		_ = conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
			"RELAY_IDENTITY_MISMATCH", "hello relay_id does not match the authenticated client-certificate identity"))
		return
	}
	var hb hello.HelloBody
	if err := f.DecodeBody(&hb); err != nil {
		_ = conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
			"PROTOCOL_VIOLATION", "hello body did not decode"))
		return
	}

	ack, err := hello.BuildHelloAck(
		hello.Hello{Type: f.Type, RelayID: f.RelayID, Body: hb},
		nonce, pub, s.implementedMinors, s.recognizedFeatures, s.site, nil,
	)
	if err != nil {
		code := "INTERNAL"
		switch {
		case errors.Is(err, hello.ErrChannelBindingInvalid):
			code = "CHANNEL_BINDING_INVALID"
		case errors.Is(err, hello.ErrProtocolVersionUnsupported):
			code = "PROTOCOL_VERSION_UNSUPPORTED"
		}
		_ = conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID, code, err.Error()))
		return
	}
	ackFrame, err := wire.NewFrame(wire.FrameTypeHelloAck, f.ID, relayID, ack.Body)
	if err != nil {
		return
	}
	ackFrame.TraceID = f.TraceID
	if err := conn.send(ackFrame); err != nil {
		return
	}

	// Authenticated steady state: register for server-initiated pushes,
	// serve pulls until the connection drops.
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()

	for {
		var f wire.Frame
		if err := websocket.JSON.Receive(ws, &f); err != nil {
			return // relay went away (or sent a non-frame); connection over
		}
		switch f.Type {
		case wire.FrameTypeStatePull:
			if err := s.handleStatePull(conn, f); err != nil {
				return
			}
		default:
			// Unknown/unhandled verb: tolerated silently (REL-004 forward
			// tolerance) — a spike corner; production wants the contract's
			// story for unrecognized types.
		}
	}
}

// handleStatePull answers one state.pull (REL-050/051): state.unchanged
// when since_generation already names the provider's current generation,
// else the full state.snapshot. The reply echoes the request's id and
// trace_id (REL-006) and carries relay_id (REL-005).
func (s *Server) handleStatePull(conn *serverConn, req wire.Frame) error {
	var body wire.StatePullBody
	if len(req.Body) > 0 { // an absent body is a pull with no since_generation claim
		if err := req.DecodeBody(&body); err != nil {
			return conn.send(wire.NewErrorFrame(req.ID, req.TraceID, conn.relayID,
				"PROTOCOL_VIOLATION", "state.pull body did not decode"))
		}
	}

	snap, err := s.provider()
	if err != nil {
		// A transient derive failure is non-fatal to the connection: the
		// relay keeps serving its last-applied generation (REL-055).
		return conn.send(wire.NewErrorFrame(req.ID, req.TraceID, conn.relayID,
			"INTERNAL", "desired state unavailable"))
	}

	var reply wire.Frame
	if body.SinceGeneration != nil && *body.SinceGeneration == snap.Generation {
		reply, err = wire.NewFrame(wire.FrameTypeStateUnchanged, req.ID, conn.relayID,
			wire.StateUnchangedBody{Generation: snap.Generation})
	} else {
		reply, err = wire.NewFrame(wire.FrameTypeStateSnapshot, req.ID, conn.relayID, snap)
	}
	if err != nil {
		return err
	}
	reply.TraceID = req.TraceID
	return conn.send(reply)
}

// CloseAll force-closes every live authenticated connection. Necessary
// shutdown surface, not a convenience: a hijacked WS connection is invisible
// to BOTH net/http.Server.Shutdown and httptest's connection tracking (they
// drop hijacked conns from their registries), so without this the app peer
// can stop listening while every relay connection stays open — a spike
// finding worth keeping.
func (s *Server) CloseAll() {
	s.mu.Lock()
	conns := make([]*serverConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.ws.Close()
	}
}

// NotifyGenerationAdvance tells every authenticated connection the feeder's
// desired-state generation advanced. Default (REL-050-conformant) form: a
// `state.changed` nudge carrying the new generation, to which a relay
// responds with its own state.pull. With WithDirectSnapshotPush, the full
// state.snapshot is pushed instead (REL-050-violating spike variant). A
// per-connection send failure is that connection's problem (its read loop
// will reap it); Notify keeps going.
func (s *Server) NotifyGenerationAdvance() {
	snap, err := s.provider()
	if err != nil {
		return // nothing coherent to announce; the next pull will surface it
	}

	s.mu.Lock()
	conns := make([]*serverConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		var f wire.Frame
		var err error
		if s.pushSnapshots {
			f, err = wire.NewFrame(wire.FrameTypeStateSnapshot, ulid.New(), c.relayID, snap)
		} else {
			f, err = wire.NewFrame(wire.FrameTypeStateChanged, ulid.New(), c.relayID,
				wire.StateChangedBody{Generation: snap.Generation})
		}
		if err != nil {
			continue
		}
		_ = c.send(f)
	}
}
