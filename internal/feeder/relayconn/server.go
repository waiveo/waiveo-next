// Package relayconn is the app-peer (feeder) side of the relay/1 persistent
// connection: one WebSocket endpoint at the contract's own stable path
// (/relay/v1, REL-001 — the claim-credential example's
// wss://…/relay/v1), upgraded from the feeder's existing HTTPS listener,
// exchanging exactly one UTF-8 JSON message per WS message (REL-002,
// wire.Frame).
//
// Per-connection sequence:
//
//	TLS (mutual: the relay presents its enrollment-issued leaf; the listener
//	verifies it against the enrollment CA, enroll.Server.ClientCAPool)
//	→ REL-016 revocation check on the presented certificate's serial —
//	  refused CERT_REVOKED at every connection attempt, re-checked before
//	  every steady-state frame so a mid-session revocation lands on the
//	  very next frame, and swept on the heartbeat cadence so a revoked
//	  relay that simply goes SILENT is proactively refused + disconnected
//	  (its conn slot reclaimed) rather than riding out an open session;
//	  server-initiated pushes are gated the same way
//	  (NotifyGenerationAdvance never nudges a revoked connection)
//	→ server sends `challenge` — nonce DERIVED from this connection's TLS
//	  exporter keying material (REL-040, hello.ExporterChallengeNonce)
//	→ relay sends `hello` (channel-binding signature over that nonce)
//	→ server verifies against the enrollment key looked up by the mTLS
//	  client-cert identity — the certificate's CommonName, never the
//	  self-asserted relay_id (REL-041; a mismatched hello.relay_id is
//	  refused RELAY_IDENTITY_MISMATCH) — negotiates, answers `hello-ack`
//	→ steady state: `state.pull` answered with `state.snapshot` or
//	  `state.unchanged` (REL-050/051), and generation-advance nudges
//	  (`state.changed`, REL-057) pushed server→relay
//	  (NotifyGenerationAdvance).
//
// Any pre-hello frame that is not `hello` is refused with a top-level error
// frame (PROTOCOL_VIOLATION) and the connection is closed (REL-039 posture);
// every post-enrollment frame the server sends carries relay_id (REL-005)
// and echoes its request's id + trace_id (REL-006/007).
//
// The transport is github.com/coder/websocket: context-governed reads and
// writes (per-operation deadlines), concurrent-safe writes, Ping for
// protocol-level dead-peer detection, and a proper RFC 6455 close
// handshake.
package relayconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/heartbeat"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// defaultWriteTimeout bounds any single frame write to a relay: a peer that
// cannot take a frame within it has its connection torn down (the transport
// closes the connection when a write's context expires) — the
// slow-consumer posture that keeps one stalled relay from wedging the
// feeder. WithWriteTimeout overrides it (tests).
const defaultWriteTimeout = 10 * time.Second

// defaultPingInterval/-Timeout are the heartbeat cadence (WithHeartbeat
// overrides): a ping every 20s whose pong must arrive within 10s. This is
// the loop that reaps a half-open connection (NAT drop, power-cut relay) —
// without it the connection's goroutine and conns entry leak forever,
// since a dead peer that never sends again never errors a plain read.
const (
	defaultPingInterval = 20 * time.Second
	defaultPingTimeout  = 10 * time.Second
)

// maxInboundFrameBytes bounds one inbound frame on this side: a relay sends
// small control frames (hello, state.pull, state.ack) — nothing remotely
// near this bound; a frame exceeding it closes the connection.
const maxInboundFrameBytes = 1 << 20

// SnapshotProvider returns the feeder's CURRENT signed desired-state
// snapshot — the same func the HTTP pull endpoint serves
// (enroll.Server.SetSnapshotProvider's provider, e.g. cmd/waiveo-feeder's
// generation-cached desiredStateSource.current).
type SnapshotProvider func() (wire.StateSnapshotBody, error)

// RevocationCheck reports whether the certificate with this serial, issued
// to relayID, has been revoked — enroll.Server.IsRevoked. REL-016: the
// check runs at EVERY connection attempt, not only at issuance time; again
// before every steady-state frame is served, so a revocation lands
// mid-session on the very next frame from a relay that keeps talking; on
// every server-initiated push (NotifyGenerationAdvance); and on the
// heartbeat cadence, so a revoked relay that goes silent — keeping its
// authenticated TLS session open while sending nothing — is proactively
// disconnected rather than holding its conn slot and its session forever.
// serial is the presented client certificate's own SerialNumber, lowercase
// hex (big.Int.Text(16)) — the exact string form the issuance record keys.
type RevocationCheck func(relayID, serial string) bool

// Server serves /relay/v1. Safe for concurrent connections; its live-conn
// set is mutex-guarded.
type Server struct {
	provider           SnapshotProvider
	lookup             hello.RelayKeyLookup
	revoked            RevocationCheck
	site               hello.SiteBinding
	implementedMinors  []string
	recognizedFeatures []string

	writeTimeout time.Duration
	pingInterval time.Duration
	pingTimeout  time.Duration

	mu    sync.Mutex
	conns map[*serverConn]struct{}
	acks  map[string]wire.Frame // relay_id -> last received state.ack frame (REL-054)
}

// Option configures a Server.
type Option func(*Server)

// WithWriteTimeout overrides the per-frame write deadline (the
// slow-consumer disconnect bound). Tests use a small value to prove the
// stalled-reader teardown without waiting out the production bound.
func WithWriteTimeout(d time.Duration) Option {
	return func(s *Server) { s.writeTimeout = d }
}

// WithHeartbeat overrides the heartbeat cadence: a ping every interval
// whose pong must arrive within timeout; a failed round-trip closes the
// connection.
func WithHeartbeat(interval, timeout time.Duration) Option {
	return func(s *Server) { s.pingInterval, s.pingTimeout = interval, timeout }
}

// New builds the app peer's /relay/v1 server. provider serves state.pull;
// lookup resolves a relay's enrollment public key by its mTLS-authenticated
// identity (REL-041 — enroll.Server.RelayEnrollmentKey); revoked is the
// REL-016 revocation check run at every connection attempt and before
// every steady-state frame (enroll.Server.IsRevoked; nil disables the
// check and is for tests only — production always wires it);
// site/minors/features feed hello negotiation exactly as
// the HTTP-era handshake server's did.
func New(
	provider SnapshotProvider,
	lookup hello.RelayKeyLookup,
	revoked RevocationCheck,
	site hello.SiteBinding,
	implementedMinors []string,
	recognizedFeatures []string,
	opts ...Option,
) *Server {
	s := &Server{
		provider:           provider,
		lookup:             lookup,
		revoked:            revoked,
		site:               site,
		implementedMinors:  implementedMinors,
		recognizedFeatures: recognizedFeatures,
		writeTimeout:       defaultWriteTimeout,
		pingInterval:       defaultPingInterval,
		pingTimeout:        defaultPingTimeout,
		conns:              map[*serverConn]struct{}{},
		acks:               map[string]wire.Frame{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// LastStateAck returns the most recent state.ack frame received from
// relayID (REL-054) and whether one has arrived — observability for tests
// and operators.
func (s *Server) LastStateAck(relayID string) (wire.Frame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.acks[relayID]
	return f, ok
}

// ConnCount reports the number of live authenticated connections —
// observability for tests and operators (a reaped connection leaves this
// count; a leaked one would not).
func (s *Server) ConnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// Handler returns the http.Handler to mount at /relay/v1. A client that
// does not offer the relay/1 subprotocol (wire.Subprotocol) is refused at
// the HTTP upgrade, before any frame is exchanged.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !offersSubprotocol(r) {
			// Pre-upgrade refusal: no frame exists yet, so the typed error
			// rides the HTTP response body (REL-007's frame shape; no
			// relay_id — the pre-auth exception REL-005 carves out). The
			// subprotocol carries the protocol major, so offering none is a
			// version-negotiation failure, not a malformed frame.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(wire.NewErrorFrame("", apihttp.TraceID(r), "",
				"PROTOCOL_VERSION_UNSUPPORTED",
				fmt.Sprintf("the %q subprotocol is required; no supported protocol major was offered.", wire.Subprotocol)))
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{wire.Subprotocol},
		})
		if err != nil {
			return // Accept already wrote the HTTP-level refusal
		}
		ws.SetReadLimit(maxInboundFrameBytes)
		s.serve(r, ws)
	})
}

// offersSubprotocol reports whether the upgrade request offers the relay/1
// subprotocol — checked before Accept so a client offering none (or the
// wrong one) is refused at the handshake, never negotiated down to the
// empty subprotocol.
func offersSubprotocol(r *http.Request) bool {
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(header, ",") {
			if strings.TrimSpace(p) == wire.Subprotocol {
				return true
			}
		}
	}
	return false
}

// serverConn is one live, authenticated relay connection. Writes are
// concurrent-safe (the transport serializes them) and individually bounded
// by the server's write timeout: a write that cannot complete within it
// closes the connection — the slow-consumer disconnect policy (a relay
// that stops draining its socket is torn down, never allowed to wedge the
// feeder behind its buffer).
type serverConn struct {
	ws           *websocket.Conn
	relayID      string
	serial       string // presented client-cert serial, for REL-016 re-checks
	writeTimeout time.Duration
}

func (c *serverConn) send(f wire.Frame) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.writeTimeout)
	defer cancel()
	return sendFrame(ctx, c.ws, f)
}

func sendFrame(ctx context.Context, ws *websocket.Conn, f wire.Frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("relayconn: encode frame: %w", err)
	}
	return ws.Write(ctx, websocket.MessageText, data)
}

// readFrame reads one frame off ws into f.
func readFrame(ctx context.Context, ws *websocket.Conn, f *wire.Frame) error {
	_, data, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, f); err != nil {
		return fmt.Errorf("relayconn: decode frame: %w", err)
	}
	return nil
}

// serve runs one connection's whole life: identity, challenge, hello,
// steady state. It returns (closing the conn) on any refusal or transport
// error.
func (s *Server) serve(req *http.Request, ws *websocket.Conn) {
	defer ws.CloseNow()
	ctx := context.Background()

	tlsState := req.TLS

	// REL-003/041: the connection MUST be mutually authenticated, and the
	// relay's identity is the mTLS client certificate's — never the
	// self-asserted relay_id. The listener already verified the chain
	// against the enrollment CA (ClientCAs); no cert at all means this
	// connection does not satisfy REL-003's mutual-authentication shape —
	// MALFORMED_MESSAGE is the taxonomy's REL-003 entry. No relay_id is
	// known to stamp on this refusal (REL-005's pre-authentication
	// exception).
	if tlsState == nil || len(tlsState.PeerCertificates) == 0 {
		_ = sendFrame(ctx, ws, wire.NewErrorFrame("", "", "",
			"MALFORMED_MESSAGE", "client certificate required — the connection is not mutually authenticated (REL-003)"))
		return
	}
	leafCert := tlsState.PeerCertificates[0]
	relayID := leafCert.Subject.CommonName
	// The presented serial, rendered exactly as the issuance record keys it
	// (big.Int.Text(16) — see enroll.issueRelayCert).
	serial := leafCert.SerialNumber.Text(16)

	// REL-016: revocation is checked at EVERY connection attempt, before
	// the handshake proceeds. The mTLS identity IS established here, so the
	// refusal carries relay_id (REL-005).
	if s.isRevoked(relayID, serial) {
		_ = sendFrame(ctx, ws, wire.NewErrorFrame("", "", relayID,
			"CERT_REVOKED", "the presented certificate has been revoked (REL-016)"))
		return
	}

	// An mTLS identity with no enrollment record on file is NOT refused
	// here: the handshake proceeds and hello's channel-binding verification
	// fails against the absent enrollment key (VerifyChannelBinding fails
	// closed on a nil key), drawing CHANNEL_BINDING_INVALID at the point
	// the taxonomy defines it (REL-032) — and never giving an
	// unauthenticated prober a pre-hello oracle for which identities are
	// enrolled.
	pub, _ := s.lookup(relayID)

	// REL-030/040: the challenge nonce derives from THIS connection's TLS
	// exporter keying material — per-connection by construction, no
	// outstanding-nonce bookkeeping to race (the one-global-slot hazard of
	// the two-request HTTP handshake disappears entirely).
	nonce, err := hello.ExporterChallengeNonce(tlsState)
	if err != nil {
		_ = sendFrame(ctx, ws, wire.NewErrorFrame("", "", relayID,
			"INTERNAL", "challenge derivation failed"))
		return
	}
	challenge, err := wire.NewFrame(wire.FrameTypeChallenge, ulid.New(), relayID,
		hello.ChallengeBody{Nonce: nonce})
	if err != nil {
		return
	}
	conn := &serverConn{ws: ws, relayID: relayID, serial: serial, writeTimeout: s.writeTimeout}
	if err := conn.send(challenge); err != nil {
		return
	}

	// First relay frame MUST be hello (REL-031/039): anything else is a
	// protocol violation, refused with a top-level error frame and a close.
	var f wire.Frame
	if err := readFrame(ctx, ws, &f); err != nil {
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
		// An undecodable body fails the type's minimum shape — the
		// taxonomy's MALFORMED_MESSAGE (REL-002), not a message-ordering
		// PROTOCOL_VIOLATION.
		_ = conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
			"MALFORMED_MESSAGE", "hello body did not decode"))
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

	// Heartbeat: a half-open connection (NAT drop, power-cut relay) never
	// errors a plain read, so without this the read loop below — and this
	// conns entry — would leak forever. A failed ping round-trip
	// hard-closes the connection, which errors the read loop out.
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go func() {
		if err := heartbeat.Run(hbCtx, ws, s.pingInterval, s.pingTimeout); err != nil {
			_ = ws.CloseNow()
		}
	}()

	// REL-016's silent-relay half: the inbound-loop re-check below only
	// fires when the relay SENDS something, so a revoked relay that simply
	// goes quiet would otherwise keep its authenticated session (and this
	// conns slot) open indefinitely. This sweep re-checks revocation on the
	// heartbeat cadence and tears a revoked connection down proactively.
	go s.revocationSweep(hbCtx, conn)

	for {
		var f wire.Frame
		if err := readFrame(ctx, ws, &f); err != nil {
			return // relay went away (or sent a non-frame); connection over
		}
		// REL-016 mid-session: a revocation landing while the connection is
		// up takes effect on the very next frame — refused with the same
		// typed CERT_REVOKED a fresh connection attempt draws, then closed.
		if s.isRevoked(relayID, serial) {
			_ = conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
				"CERT_REVOKED", "the presented certificate has been revoked (REL-016)"))
			return
		}
		switch f.Type {
		case wire.FrameTypeStatePull:
			if err := s.handleStatePull(conn, f); err != nil {
				return
			}
		case wire.FrameTypeStateAck:
			var ack wire.StateAckBody
			if err := f.DecodeBody(&ack); err != nil {
				if err := conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
					"MALFORMED_MESSAGE", "state.ack body did not decode")); err != nil {
					return
				}
				continue
			}
			s.mu.Lock()
			s.acks[relayID] = f
			s.mu.Unlock()
		default:
			// Unknown verb: REL-004 additive tolerance — a newer relay's
			// new message type is ignored, never a refusal, exactly as
			// REL-057 prescribes for its own nudge at an older peer.
		}
	}
}

// isRevoked applies the REL-016 check when one is wired (nil = tests only).
func (s *Server) isRevoked(relayID, serial string) bool {
	return s.revoked != nil && s.revoked(relayID, serial)
}

// revocationSweep re-checks conn's certificate against the revocation
// record on the heartbeat cadence until the connection ends, closing it as
// soon as a revocation is found (RevocationCheck's doc: the enforcement
// path for a revoked relay that goes silent).
func (s *Server) revocationSweep(ctx context.Context, conn *serverConn) {
	ticker := time.NewTicker(s.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.isRevoked(conn.relayID, conn.serial) {
				s.closeRevoked(conn)
				return
			}
		}
	}
}

// closeRevoked sends the typed CERT_REVOKED refusal (best-effort — the
// same frame a fresh connection attempt draws, REL-016) and hard-closes
// the connection; the closed socket errors the conn's read loop out, which
// unregisters it from the live-conn set.
func (s *Server) closeRevoked(conn *serverConn) {
	_ = conn.send(wire.NewErrorFrame("", "", conn.relayID,
		"CERT_REVOKED", "the presented certificate has been revoked (REL-016)"))
	_ = conn.ws.CloseNow()
}

// handleStatePull answers one state.pull (REL-050/051): state.unchanged
// when since_generation already names the provider's current generation,
// else the full state.snapshot — including when since_generation names a
// generation GREATER than the current one (REL-051: the divergence then
// surfaces at the relay's own REL-052 gate, never behind a lying
// unchanged). The reply echoes the request's id and trace_id (REL-006) and
// carries relay_id (REL-005).
func (s *Server) handleStatePull(conn *serverConn, req wire.Frame) error {
	var body wire.StatePullBody
	if len(req.Body) > 0 { // an absent body is a pull with no since_generation claim
		if err := req.DecodeBody(&body); err != nil {
			// Undecodable body: the taxonomy's MALFORMED_MESSAGE (REL-002).
			return conn.send(wire.NewErrorFrame(req.ID, req.TraceID, conn.relayID,
				"MALFORMED_MESSAGE", "state.pull body did not decode"))
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
		_ = c.ws.CloseNow()
	}
}

// NotifyGenerationAdvance tells every authenticated connection the feeder's
// desired-state generation advanced, as REL-057's `state.changed` nudge
// carrying the new generation — to which a relay responds with its own
// state.pull, keeping REL-050's pull-only snapshot movement intact.
//
// Each nudge is sent on its own goroutine: Notify returns immediately, so a
// caller hanging it off a store's post-commit hook never couples an api/1
// write's latency to the slowest (or deadest) relay connection. Each send
// is still individually bounded by the write timeout, and a failed send is
// that connection's problem (the write-timeout teardown errors its read
// loop out); nudges are best-effort by contract (REL-057) — the relay's
// next pull recovers a lost one.
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
		// REL-016: a server-initiated push is a steady-state frame like any
		// other — a revoked relay draws the typed refusal and a close, never
		// fresh generation metadata.
		if s.isRevoked(c.relayID, c.serial) {
			go s.closeRevoked(c)
			continue
		}
		f, err := wire.NewFrame(wire.FrameTypeStateChanged, ulid.New(), c.relayID,
			wire.StateChangedBody{Generation: snap.Generation})
		if err != nil {
			continue
		}
		go func(c *serverConn) { _ = c.send(f) }(c)
	}
}
