// Package relayconn is the relay side of the relay/1 persistent connection:
// the outbound-only dialer (REL-001 — a relay is always the connecting
// party) that opens ONE mutually authenticated WS connection to the app
// peer's /relay/v1 and runs the whole exchange over it: challenge → hello →
// hello-ack, then correlated request/response (state.pull) plus
// server-initiated frames (state.changed nudges, REL-057).
//
// Transport trust is exactly the enrollment-anchored posture the HTTP paths
// already use, now on one connection:
//   - The app peer's identity is the enrollment-captured leaf-SPKI pin
//     verified by internal/relay/clocktrust (REL-136/137 — never chain
//     validation).
//   - The relay presents its enrollment-issued leaf as the TLS client
//     certificate (REL-003), so the app peer authenticates it by mTLS
//     identity (REL-041).
//   - The challenge nonce is INDEPENDENTLY derived from the same TLS
//     session's exporter keying material on this side and compared to the
//     received challenge (REL-040): a challenge minted on any other TLS
//     channel cannot match, so the REL-032 signature binds the enrollment
//     key to THIS channel. The dialer pins TLS 1.3 (REL-040's exporter
//     requirement — recorded beside the contract's label pin) even though
//     the HTTP-era paths still accept 1.2.
//
// The transport is github.com/coder/websocket: context-governed dial, read,
// write (per-operation deadlines), concurrent-safe writes, an initiable
// Ping that round-trips a pong (protocol-level dead-peer detection), and a
// proper RFC 6455 close handshake — none of which the frozen
// x/net/websocket exposes.
//
// Dial does not auto-reconnect: a dropped connection surfaces via Done/Err
// and the owner re-Dials (the reconnect supervisor lives above this type).
package relayconn

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/maaxton/waiveo-next/internal/relay/clocktrust"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/heartbeat"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// dialTimeout bounds the whole TCP+TLS+WS+challenge+hello handshake;
// requestTimeout bounds one correlated request/response exchange;
// writeTimeout bounds any single frame write (a write that cannot complete
// within it tears the connection down — the transport's slow-peer
// posture). Loopback deployment values — generous, not tuned.
const (
	dialTimeout    = 10 * time.Second
	requestTimeout = 10 * time.Second
	writeTimeout   = 10 * time.Second
)

// defaultPingInterval/-Timeout are the heartbeat cadence (Config.PingInterval/
// PingTimeout override): a ping every 20s whose pong must arrive within 10s,
// so a dead peer is detected within ~30s — the loop that reaps half-open
// NAT-dropped connections instead of leaking them.
const (
	defaultPingInterval = 20 * time.Second
	defaultPingTimeout  = 10 * time.Second
)

// maxInboundFrameBytes bounds one inbound frame on this side: the client
// receives full state.snapshot bodies (every desired-state section for a
// site), so the bound is generous — but present, so a misbehaving peer
// cannot balloon memory with one frame.
const maxInboundFrameBytes = 16 << 20

// Config configures Dial.
type Config struct {
	// URL is the app peer's connection endpoint: wss://host:port/relay/v1
	// (an https:// scheme is accepted and treated as wss).
	URL string

	// Store is the relay's operational store: Dial reads its enrolled
	// identity (relay_id + certificate + enrollment private key) and the
	// enrollment-captured app-peer trust pin from it.
	Store *identity.Store

	// Declaration is everything the relay declares in its hello except the
	// channel-binding signature (hello.Declaration).
	Declaration hello.Declaration

	// ClockTrusted and Now feed the REL-136 temporal decision on the app
	// peer's certificate (clocktrust). A zero Now means time.Now.
	ClockTrusted bool
	Now          func() time.Time

	// OnGenerationAdvance, when non-nil, is invoked with the generation a
	// state.changed nudge (REL-057) announced, on one dedicated dispatcher
	// goroutine; the callback may itself call Pull. Delivery is a
	// single-slot latest-wins cell, exactly REL-057's coalescible
	// best-effort semantics: while a callback runs, newer nudges REPLACE
	// the one waiting slot (the highest generation wins) rather than queue
	// behind it or drop silently — the latest announced generation is
	// never lost, and a stale intermediate one is subsumed by design, not
	// by backpressure accident. Any other server-initiated frame type is
	// ignored as REL-004 additive tolerance.
	OnGenerationAdvance func(generation int64)

	// PingInterval / PingTimeout govern the protocol-level heartbeat: a
	// ping every PingInterval, its pong bounded by PingTimeout; a failed
	// round-trip closes the connection (dead-peer detection for half-open
	// sockets — Done closes and the owner re-dials). Zero values take the
	// defaults (defaultPingInterval / defaultPingTimeout).
	PingInterval time.Duration
	PingTimeout  time.Duration

	// ObserveFrame, when non-nil, is invoked synchronously with every frame
	// this client sends (sent=true) and receives (sent=false), challenge
	// included, in wire order per direction. It is a TEST-ONLY observation
	// seam — the e2e proof's wire assertions record into their own bounded
	// buffers through it. Production configs leave it nil: the client
	// itself retains no frame history. The hook runs on the sending
	// goroutine / the read loop, so it must be fast and concurrent-safe.
	ObserveFrame func(sent bool, f wire.Frame)
}

// Refusal is a typed refusal frame received in place of an expected reply —
// the connection-frame form of REL-007 (code carries the Error-taxonomy
// value, e.g. CHANNEL_BINDING_INVALID).
type Refusal struct {
	Code    string
	Message string
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("relayconn: refused: %s: %s", r.Code, r.Message)
}

// Client is one live connection. Methods are safe for concurrent use.
type Client struct {
	ws      *websocket.Conn
	relayID string
	ackBody hello.HelloAckBody
	observe func(sent bool, f wire.Frame)
	mu      sync.Mutex
	pending map[string]chan wire.Frame
	done    chan struct{}
	readErr error

	// The single-slot latest-wins nudge cell (Config.OnGenerationAdvance's
	// doc): nudgeGen holds the highest generation announced and not yet
	// dispatched; nudgeSet says the slot is occupied; nudgeCh (buffer 1)
	// wakes the dispatcher.
	nudgeMu  sync.Mutex
	nudgeGen int64
	nudgeSet bool
	nudgeCh  chan struct{}
}

// Dial opens, authenticates, and hands over one connection. On a typed
// refusal in place of hello-ack it returns a *Refusal carrying the
// taxonomy code.
func Dial(cfg Config) (*Client, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("relayconn: Dial: Store must not be nil")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	id, enrolled, err := cfg.Store.Identity()
	if err != nil {
		return nil, fmt.Errorf("relayconn: Dial: read persisted identity: %w", err)
	}
	if !enrolled {
		return nil, fmt.Errorf("relayconn: Dial: relay is not enrolled — no identity persisted")
	}
	pin, havePin, err := cfg.Store.AppPeerTrustPin()
	if err != nil {
		return nil, fmt.Errorf("relayconn: Dial: read app-peer trust pin: %w", err)
	}
	if !havePin {
		return nil, fmt.Errorf("relayconn: Dial: no app-peer trust pin persisted (REL-137) — enroll first")
	}

	leaf, err := clientCertificate(id)
	if err != nil {
		return nil, fmt.Errorf("relayconn: Dial: %w", err)
	}

	wsURL, hostPort, err := connectionURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("relayconn: Dial: %w", err)
	}

	// One context over the whole handshake: TCP+TLS dial, WS upgrade,
	// challenge → hello → hello-ack.
	hsCtx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	// REL-136/137 posture verbatim from the HTTP-era verifier: SPKI pin, no
	// chain validation, temporal check deferred on an untrusted clock —
	// plus the client certificate that makes the connection mutual
	// (REL-003), and a TLS 1.3 floor: REL-040's exporter derivation needs
	// an exporter-capable session, and on this transport the relay refuses
	// to negotiate anything older rather than discover mid-handshake that
	// the session cannot bind the channel.
	tlsCfg := clocktrust.AppPeerTLSConfig(pin, cfg.ClockTrusted, now(), time.Duration(clocktrust.DefaultBoundedGraceMs)*time.Millisecond)
	tlsCfg.Certificates = []tls.Certificate{leaf}
	tlsCfg.MinVersion = tls.VersionTLS13

	// Pre-dial the TLS session ourselves so its ConnectionState (the
	// exporter keying material, REL-040) stays in hand, then feed the
	// established conn to the WS dialer through a single-use transport.
	dialer := &tls.Dialer{Config: tlsCfg}
	rawConn, err := dialer.DialContext(hsCtx, "tcp", hostPort)
	if err != nil {
		return nil, fmt.Errorf("relayconn: Dial: TLS dial %s: %w", hostPort, err)
	}
	tlsConn, ok := rawConn.(*tls.Conn)
	if !ok {
		rawConn.Close()
		return nil, fmt.Errorf("relayconn: Dial: dialer returned a %T, not *tls.Conn", rawConn)
	}

	ws, _, err := websocket.Dial(hsCtx, wsURL, &websocket.DialOptions{
		HTTPClient:   singleUseClient(tlsConn),
		Subprotocols: []string{wire.Subprotocol},
	})
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("relayconn: Dial: ws upgrade: %w", err)
	}
	if ws.Subprotocol() != wire.Subprotocol {
		ws.Close(websocket.StatusPolicyViolation, "subprotocol not negotiated")
		return nil, fmt.Errorf("relayconn: Dial: server negotiated subprotocol %q, want %q", ws.Subprotocol(), wire.Subprotocol)
	}
	ws.SetReadLimit(maxInboundFrameBytes)

	c := &Client{
		ws:      ws,
		relayID: id.RelayID,
		observe: cfg.ObserveFrame,
		pending: map[string]chan wire.Frame{},
		done:    make(chan struct{}),
		nudgeCh: make(chan struct{}, 1),
	}

	if err := c.handshake(hsCtx, tlsConn, id, cfg.Declaration); err != nil {
		ws.CloseNow()
		return nil, err
	}

	go c.readLoop()
	if cfg.OnGenerationAdvance != nil {
		go c.dispatchGenerations(cfg.OnGenerationAdvance)
	}

	// Heartbeat: dead-peer detection for half-open connections. A failed
	// ping round-trip hard-closes the connection; the read loop then
	// surfaces the death via Done/Err and the owner re-dials.
	pingInterval, pingTimeout := cfg.PingInterval, cfg.PingTimeout
	if pingInterval == 0 {
		pingInterval = defaultPingInterval
	}
	if pingTimeout == 0 {
		pingTimeout = defaultPingTimeout
	}
	go func() {
		hbCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { <-c.done; cancel() }()
		if err := heartbeat.Run(hbCtx, c.ws, pingInterval, pingTimeout); err != nil {
			_ = c.ws.CloseNow()
		}
	}()
	return c, nil
}

// noteGeneration folds one state.changed announcement into the nudge cell:
// latest-wins (the highest generation replaces an undispatched older one —
// REL-057's coalescible delivery), then wakes the dispatcher.
func (c *Client) noteGeneration(gen int64) {
	c.nudgeMu.Lock()
	if !c.nudgeSet || gen > c.nudgeGen {
		c.nudgeGen = gen
	}
	c.nudgeSet = true
	c.nudgeMu.Unlock()
	select {
	case c.nudgeCh <- struct{}{}:
	default: // dispatcher already has a wakeup pending
	}
}

// dispatchGenerations delivers coalesced nudges to cb, one at a time, until
// the connection dies. The cell is drained atomically per delivery, so a
// nudge arriving DURING a callback is delivered next, never lost.
func (c *Client) dispatchGenerations(cb func(int64)) {
	for {
		select {
		case <-c.done:
			return
		case <-c.nudgeCh:
			c.nudgeMu.Lock()
			gen, set := c.nudgeGen, c.nudgeSet
			c.nudgeSet = false
			c.nudgeMu.Unlock()
			if set {
				cb(gen)
			}
		}
	}
}

// singleUseClient wraps an already-established TLS connection in an
// http.Client whose transport hands it out exactly once — the seam that
// lets the WS dialer ride the exact TLS session whose exporter keying
// material the handshake's REL-040 check derives from.
func singleUseClient(conn *tls.Conn) *http.Client {
	var mu sync.Mutex
	used := false
	return &http.Client{
		Transport: &http.Transport{
			DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
				mu.Lock()
				defer mu.Unlock()
				if used {
					return nil, fmt.Errorf("relayconn: single-use TLS connection already consumed")
				}
				used = true
				return conn, nil
			},
		},
	}
}

// handshake runs challenge → hello → hello-ack on the fresh connection,
// bounded by ctx.
func (c *Client) handshake(ctx context.Context, tlsConn *tls.Conn, id identity.RelayIdentity, decl hello.Declaration) error {
	// The server's challenge MUST be the first frame (REL-030).
	var challenge wire.Frame
	if err := c.receive(ctx, &challenge); err != nil {
		return fmt.Errorf("relayconn: handshake: read challenge: %w", err)
	}
	if challenge.Type == wire.FrameTypeError {
		return &Refusal{Code: challenge.Code, Message: challenge.Message}
	}
	if challenge.Type != wire.FrameTypeChallenge {
		return fmt.Errorf("relayconn: handshake: first frame is %q, want challenge", challenge.Type)
	}
	var cb hello.ChallengeBody
	if err := challenge.DecodeBody(&cb); err != nil {
		return fmt.Errorf("relayconn: handshake: %w", err)
	}

	// REL-040 from THIS side: derive the exporter nonce independently and
	// require the server's challenge to equal it — proof the challenge was
	// minted on this exact TLS channel, not relayed from another.
	cs := tlsConn.ConnectionState()
	expected, err := hello.ExporterChallengeNonce(&cs)
	if err != nil {
		return fmt.Errorf("relayconn: handshake: derive exporter nonce: %w", err)
	}
	if cb.Nonce != expected {
		return fmt.Errorf("relayconn: handshake: challenge nonce is not this connection's exporter derivation (REL-040)")
	}

	sig, err := hello.SignChannelBinding(id.PrivateKey, cb.Nonce)
	if err != nil {
		return fmt.Errorf("relayconn: handshake: sign channel binding: %w", err)
	}

	helloID := ulid.New()
	helloFrame, err := wire.NewFrame(wire.FrameTypeHello, helloID, id.RelayID, hello.HelloBody{
		ProtocolVersion:         decl.ProtocolVersion,
		Features:                decl.Features,
		SiteBinding:             decl.SiteBinding,
		SubnetMetadata:          decl.SubnetMetadata,
		ClockState:              decl.ClockState,
		ChannelBindingSignature: sig,
	})
	if err != nil {
		return fmt.Errorf("relayconn: handshake: %w", err)
	}
	if err := c.sendCtx(ctx, helloFrame); err != nil {
		return fmt.Errorf("relayconn: handshake: send hello: %w", err)
	}

	var reply wire.Frame
	if err := c.receive(ctx, &reply); err != nil {
		return fmt.Errorf("relayconn: handshake: read hello-ack: %w", err)
	}
	if reply.Type == wire.FrameTypeError {
		return &Refusal{Code: reply.Code, Message: reply.Message}
	}
	if reply.Type != wire.FrameTypeHelloAck {
		return fmt.Errorf("relayconn: handshake: reply is %q, want hello-ack", reply.Type)
	}
	if reply.ID != helloID {
		return fmt.Errorf("relayconn: handshake: hello-ack id %q does not correlate with hello id %q (REL-006)", reply.ID, helloID)
	}
	if err := reply.DecodeBody(&c.ackBody); err != nil {
		return fmt.Errorf("relayconn: handshake: %w", err)
	}
	return nil
}

// readLoop dispatches every incoming frame: a frame whose id matches an
// in-flight request completes it; a state.changed nudge folds into the
// coalescing cell (never on this goroutine — the callback may Pull, which
// needs this loop alive to complete); any other server-initiated frame
// type is REL-004 additive tolerance, ignored.
func (c *Client) readLoop() {
	for {
		var f wire.Frame
		if err := c.readFrame(context.Background(), &f); err != nil {
			c.mu.Lock()
			c.readErr = err
			pending := c.pending
			c.pending = map[string]chan wire.Frame{}
			c.mu.Unlock()
			for _, ch := range pending {
				close(ch)
			}
			close(c.done)
			return
		}
		c.record(false, f)

		c.mu.Lock()
		ch, isReply := c.pending[f.ID]
		if isReply {
			delete(c.pending, f.ID)
		}
		c.mu.Unlock()

		if isReply {
			ch <- f
			continue
		}
		if f.Type == wire.FrameTypeStateChanged {
			var body wire.StateChangedBody
			if err := f.DecodeBody(&body); err != nil {
				continue // a malformed best-effort nudge; the next pull recovers
			}
			c.noteGeneration(body.Generation)
			continue
		}
		// Unknown server-initiated frame type: REL-004 additive tolerance —
		// a newer app peer's new verb is ignored, never a failure.
	}
}

// readFrame reads one frame off the connection into f.
func (c *Client) readFrame(ctx context.Context, f *wire.Frame) error {
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, f); err != nil {
		return fmt.Errorf("relayconn: decode frame: %w", err)
	}
	return nil
}

// Pull sends one state.pull (REL-050) and returns the correlated reply —
// state.snapshot or state.unchanged. traceID rides the request per REL-006
// (pass "" when the pull traces to no originating operation);
// sinceGeneration is REL-050's optional claim (nil = no claim, always a
// full snapshot).
func (c *Client) Pull(traceID string, sinceGeneration *int64) (wire.Frame, error) {
	reqID := ulid.New()
	req, err := wire.NewFrame(wire.FrameTypeStatePull, reqID, c.relayID,
		wire.StatePullBody{SinceGeneration: sinceGeneration})
	if err != nil {
		return wire.Frame{}, fmt.Errorf("relayconn: Pull: %w", err)
	}
	req.TraceID = traceID

	ch := make(chan wire.Frame, 1)
	c.mu.Lock()
	if c.readErr != nil {
		err := c.readErr
		c.mu.Unlock()
		return wire.Frame{}, fmt.Errorf("relayconn: Pull: connection is down: %w", err)
	}
	c.pending[reqID] = ch
	c.mu.Unlock()

	if err := c.send(req); err != nil {
		c.mu.Lock()
		delete(c.pending, reqID)
		c.mu.Unlock()
		return wire.Frame{}, fmt.Errorf("relayconn: Pull: send: %w", err)
	}

	select {
	case reply, ok := <-ch:
		if !ok {
			return wire.Frame{}, fmt.Errorf("relayconn: Pull: connection closed awaiting reply: %w", c.Err())
		}
		if reply.Type == wire.FrameTypeError {
			return reply, &Refusal{Code: reply.Code, Message: reply.Message}
		}
		return reply, nil
	case <-time.After(requestTimeout):
		c.mu.Lock()
		delete(c.pending, reqID)
		c.mu.Unlock()
		return wire.Frame{}, fmt.Errorf("relayconn: Pull: timed out awaiting reply to %s", reqID)
	}
}

// SendStateAck sends REL-054's state.ack acknowledging the snapshot a
// state.pull exchange delivered: the ack carries that exchange's own
// correlation id and trace id (REL-054/006) — pass the reply frame's ID
// and the pull's traceID. The owner calls this after
// desiredstate.VerifyAndApply resolves, with the apply's real outcome
// (applied_generation advanced on success; error + unadvanced generation
// on a rejected snapshot, REL-072).
func (c *Client) SendStateAck(correlationID, traceID string, body wire.StateAckBody) error {
	f, err := wire.NewFrame(wire.FrameTypeStateAck, correlationID, c.relayID, body)
	if err != nil {
		return fmt.Errorf("relayconn: SendStateAck: %w", err)
	}
	f.TraceID = traceID
	if err := c.send(f); err != nil {
		return fmt.Errorf("relayconn: SendStateAck: send: %w", err)
	}
	return nil
}

// RelayID returns the enrolled relay identity this connection authenticated as.
func (c *Client) RelayID() string { return c.relayID }

// HelloAck returns the negotiated hello-ack body (negotiated_version,
// feature subset, the app peer's authoritative site_binding).
func (c *Client) HelloAck() hello.HelloAckBody { return c.ackBody }

// Done is closed when the connection dies; Err then reports why. The
// client never reconnects itself — the owner re-Dials.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports the read-loop error that ended the connection, nil while live.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// Close tears the connection down: a proper RFC 6455 close handshake when
// the peer cooperates, a hard close otherwise.
func (c *Client) Close() error {
	err := c.ws.Close(websocket.StatusNormalClosure, "")
	if err != nil {
		return c.ws.CloseNow()
	}
	return nil
}

// record feeds the test-only observation hook (Config.ObserveFrame); a nil
// hook is the production path — nothing is retained.
func (c *Client) record(sent bool, f wire.Frame) {
	if c.observe != nil {
		c.observe(sent, f)
	}
}

// send writes one frame under the standard per-write deadline. Writes are
// concurrent-safe (the transport serializes them); a write that cannot
// complete within the deadline closes the connection.
func (c *Client) send(f wire.Frame) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return c.sendCtx(ctx, f)
}

func (c *Client) sendCtx(ctx context.Context, f wire.Frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("relayconn: encode frame: %w", err)
	}
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}
	c.record(true, f)
	return nil
}

// receive reads one frame during the handshake (before readLoop starts),
// feeding the observation hook.
func (c *Client) receive(ctx context.Context, f *wire.Frame) error {
	if err := c.readFrame(ctx, f); err != nil {
		return err
	}
	c.record(false, *f)
	return nil
}

// SnapshotFromFrame decodes a state.snapshot frame's body into the typed
// StateSnapshotBody AND re-extracts the raw `sections` bytes exactly as
// they arrived — the pair desiredstate.VerifyAndApply's REL-060 structural
// gate needs (a Go-decoded Sections cannot reveal an omitted key).
func SnapshotFromFrame(f wire.Frame) (wire.StateSnapshotBody, json.RawMessage, error) {
	if f.Type != wire.FrameTypeStateSnapshot {
		return wire.StateSnapshotBody{}, nil, fmt.Errorf("relayconn: SnapshotFromFrame: frame is %q, not state.snapshot", f.Type)
	}
	var body wire.StateSnapshotBody
	if err := f.DecodeBody(&body); err != nil {
		return wire.StateSnapshotBody{}, nil, fmt.Errorf("relayconn: SnapshotFromFrame: %w", err)
	}
	var envelope struct {
		Sections json.RawMessage `json:"sections"`
	}
	if err := json.Unmarshal(f.Body, &envelope); err != nil {
		return wire.StateSnapshotBody{}, nil, fmt.Errorf("relayconn: SnapshotFromFrame: extract raw sections: %w", err)
	}
	return body, envelope.Sections, nil
}

// clientCertificate assembles the relay's mTLS client certificate from its
// persisted identity: the enrollment-issued leaf plus the enrollment
// private key (the CSR was over that key, so leaf and key pair up).
func clientCertificate(id identity.RelayIdentity) (tls.Certificate, error) {
	block, _ := pem.Decode(id.CertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return tls.Certificate{}, fmt.Errorf("persisted certificate did not decode to a CERTIFICATE PEM block")
	}
	return tls.Certificate{
		Certificate: [][]byte{block.Bytes},
		PrivateKey:  id.PrivateKey,
	}, nil
}

// connectionURL normalizes cfg.URL: accepts wss:// or https:// (converted),
// appends the REL-001 stable path when the URL carries none, and returns
// both the ws URL and the host:port to TLS-dial.
func connectionURL(raw string) (wsURL, hostPort string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "wss":
	case "https":
		u.Scheme = "wss"
	default:
		return "", "", fmt.Errorf("URL %q must be wss:// or https://", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/relay/v1"
	}
	host := u.Host
	if u.Port() == "" {
		host += ":443"
	}
	return u.String(), host, nil
}
