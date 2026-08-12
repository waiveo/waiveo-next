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

	// OnDeviceCommand, when non-nil, handles an app-initiated
	// `device.command` (REL-112) and returns the result body the correlated
	// `device.command_result` carries. It runs on its OWN goroutine per
	// command — never the read loop — so a dispatch that waits on a physical
	// device cannot stall the connection (the read loop must stay live to
	// carry the reply itself, plus any concurrent pull). Per-device
	// serialization (REL-115) is the handler's own concern, enforced inside
	// the device plane's command surface, not by serializing this transport.
	//
	// Leaving it nil is a relay with no device plane wired: an arriving
	// command is answered {ok:false, INTERNAL} rather than dropped, since
	// REL-112 requires a result for every command.
	//
	// The body's `params` MAY carry per-dispatch credential material
	// (REL-114) — a handler must not persist or log it.
	OnDeviceCommand func(wire.DeviceCommandBody) wire.DeviceCommandResultBody

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
	command func(wire.DeviceCommandBody) wire.DeviceCommandResultBody
	mu      sync.Mutex
	pending map[string]chan wire.Frame
	done    chan struct{}
	// connErr is the FIRST cause that ended this connection, whichever half
	// noticed it, already labelled with that half (fail). It is the sole
	// death flag: nil means live, non-nil means done is closed. Set only by
	// fail, under mu, exactly once.
	connErr error

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
		command: cfg.OnDeviceCommand,
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
	c.startHeartbeat(pingInterval, pingTimeout)
	return c, nil
}

// startHeartbeat runs the dead-peer detection loop on its own goroutine
// until the connection dies (Config.PingInterval's doc).
//
// A failed round-trip goes through fail rather than a bare CloseNow, so
// Err() names the HEARTBEAT as the cause. A hard close alone would surface
// as whatever generic error the read loop then tripped over — "use of
// closed network connection", which tells an operator nothing about why the
// connection went away and actively misdirects them toward the read path.
//
// Split out of Dial so it can be driven without the authenticated
// handshake: this is the third half that can notice death, and the two that
// live in the read and write paths are exactly the pair whose asymmetry
// caused HV-22. A detector nothing can test is a detector nothing can prove
// is wired.
func (c *Client) startHeartbeat(interval, timeout time.Duration) {
	go func() {
		hbCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { <-c.done; cancel() }()
		if err := heartbeat.Run(hbCtx, c.ws, interval, timeout); err != nil {
			c.fail(sideHeartbeat, err)
		}
	}()
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
			c.fail(sideRead, err)
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
		if f.Type == wire.FrameTypeDeviceCommand {
			// Off the read loop: a command dispatch waits on a physical
			// device, and the reply it produces has to travel back through a
			// connection this loop must still be reading.
			go c.handleDeviceCommand(f)
			continue
		}
		// Unknown server-initiated frame type: REL-004 additive tolerance —
		// a newer app peer's new verb is ignored, never a failure.
	}
}

// The three halves that can notice this connection has died, named so
// Err() can say which one did. An operator reading a relay's log needs to
// tell "the peer stopped answering" (read) from "we could not get a frame
// out" (write) from "the peer stopped acknowledging pings" (heartbeat) —
// they send you to different places, and before fail existed only the read
// half could report at all.
const (
	sideRead      = "read"
	sideWrite     = "write"
	sideHeartbeat = "heartbeat"
)

// fail marks this connection dead, from ONE place, whichever half noticed
// first. It is the only writer of connErr and the only closer of done.
//
// EVERY half that can observe death routes through here — the read loop, a
// failed frame write (sendCtx), and the heartbeat's unanswered ping — which
// is the point: HV-22 was diagnosed against a client whose read loop was
// the sole detector, so a write error told its caller and told the
// connection nothing, and callers went on writing to a corpse. A detector
// wired on one path and not the others is the shape this repo keeps
// shipping; there is now one path and adding a fourth caller means calling
// this.
//
// Concurrency contract, since two halves racing is the normal case (a peer
// that exits fails a write and a read at once):
//   - The FIRST cause wins. A later half finds connErr already set and
//     returns without disturbing it, so Err() reports what actually killed
//     the connection rather than whichever goroutine was scheduled last.
//   - done closes EXACTLY once, under the same mutex that decides the
//     first-cause race, so "done is closed" and "connErr is set" are one
//     atomic fact — a caller that sees done closed can never read a nil
//     Err().
//   - In-flight requests are drained here, so a Pull blocked on a reply
//     returns immediately with the real cause instead of waiting out
//     requestTimeout.
//
// It then hard-closes the transport. That is not tidiness: without it the
// underlying socket stays open in CLOSE_WAIT for the life of the process
// (measured on a real relay after an app-peer restart — one leaked fd, plus
// the heartbeat goroutine's, per death), and a relay whose peer flaps is on
// a slow path to fd exhaustion. CloseNow is idempotent and safe to call
// while the read loop is blocked in Read — it is what unblocks it.
func (c *Client) fail(side string, cause error) {
	if cause == nil {
		return
	}
	c.mu.Lock()
	if c.connErr != nil {
		c.mu.Unlock()
		return // a half already recorded the first cause
	}
	c.connErr = fmt.Errorf("relayconn: connection died on the %s side: %w", side, cause)
	pending := c.pending
	c.pending = map[string]chan wire.Frame{}
	for _, ch := range pending {
		close(ch) // never blocks; safe to do under mu
	}
	close(c.done)
	c.mu.Unlock()

	_ = c.ws.CloseNow()
}

// handleDeviceCommand answers ONE app-initiated `device.command` (REL-112):
// it decodes the body, runs the configured handler, and sends the
// correlated `device.command_result` echoing the request's id and trace_id
// (REL-006) with this relay's own relay_id (REL-005).
//
// Every arriving command draws exactly one answer — REL-112 admits no
// silent drop:
//   - an undecodable body is refused with REL-007's top-level error frame
//     (MALFORMED_MESSAGE: the frame failed its type's minimum shape,
//     REL-002), since there is no well-formed command to produce a result
//     for;
//   - a relay with no device plane wired answers {ok:false, INTERNAL};
//   - otherwise the handler's own result body is returned verbatim,
//     including its typed refusals (COMMAND_UNRESOLVED,
//     COMMAND_TARGET_UNREACHABLE) — those ride the result's own `error`
//     field, never an additional error frame (REL-007).
//
// A send failure is dropped: the connection is already dying, and the read
// loop will surface its death through Done/Err.
func (c *Client) handleDeviceCommand(req wire.Frame) {
	var body wire.DeviceCommandBody
	if err := req.DecodeBody(&body); err != nil {
		_ = c.send(wire.NewErrorFrame(req.ID, req.TraceID, c.relayID,
			"MALFORMED_MESSAGE", "device.command body did not decode"))
		return
	}

	result := wire.NewDeviceCommandError("INTERNAL", "this relay has no device plane wired to execute commands")
	if c.command != nil {
		result = c.command(body)
	}

	reply, err := wire.NewFrame(wire.FrameTypeDeviceCommandResult, req.ID, c.relayID, result)
	if err != nil {
		return
	}
	reply.TraceID = req.TraceID
	_ = c.send(reply)
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
	if c.connErr != nil {
		err := c.connErr
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

// SendDeviceCandidates sends one `device.candidates` report: the relay's FULL
// current candidate set, which the app peer takes as replacing its prior view of
// this relay (REL-110/111).
//
// It is one-way and uncorrelated — the contract defines no reply — so it carries
// a fresh id purely as this frame's own identifier (REL-006's correlation id has
// nothing to pair with here) and no trace id, since a candidate set traces to no
// single originating operation.
//
// The body is passed as the already-built full-set report the relay's own
// candidate store produced (internal/relay/deviceplane.Store.Report), so what
// travels is exactly what that store reports and this method invents nothing.
// The envelope's relay_id is THIS connection's authenticated identity, not
// whatever the report was stamped with: the app peer authenticates the reporter
// by the connection anyway (REL-041/150), and a mismatch between the two would
// be a defect on this side, not a claim worth transmitting.
func (c *Client) SendDeviceCandidates(body wire.DeviceCandidatesBody) error {
	if body.Candidates == nil {
		// REL-110's array is always present: a relay that has found nothing
		// reports an empty set, which is a meaningful statement (it replaces
		// whatever the app peer held), not an absent one.
		body.Candidates = []wire.DeviceCandidate{}
	}
	f, err := wire.NewFrame(wire.FrameTypeDeviceCandidates, ulid.New(), c.relayID, body)
	if err != nil {
		return fmt.Errorf("relayconn: SendDeviceCandidates: %w", err)
	}
	if err := c.send(f); err != nil {
		return fmt.Errorf("relayconn: SendDeviceCandidates: send: %w", err)
	}
	return nil
}

// SendScreenStatus sends one `screen.status` report: the relay's FULL current
// view of what every screen behind it has been observed doing (parity row 5.8,
// wire/screenstatus.go).
//
// One-way and uncorrelated, exactly like SendDeviceCandidates and for the same
// reasons its doc gives — and, unlike SendPairingRedeemed, deliberately NOT
// acknowledged: a lost status report loses nothing recomputable, because the
// next one carries the whole truth again within seconds. Adding an ack would
// buy durability for data with a ten-second shelf life at the cost of a
// correlation table and a retry ledger.
//
// A nil Screens is normalized to an empty array before the frame is built. The
// empty report is the one that CLEARS the app peer's view of a relay that no
// longer knows of any screen, so dropping it to null would leave a console
// showing screens the relay has forgotten — the same reasoning
// SendDeviceCandidates applies to its own array, in the same place.
func (c *Client) SendScreenStatus(body wire.ScreenStatusBody) error {
	if body.Screens == nil {
		body.Screens = []wire.ScreenStatusEntry{}
	}
	f, err := wire.NewFrame(wire.FrameTypeScreenStatus, ulid.New(), c.relayID, body)
	if err != nil {
		return fmt.Errorf("relayconn: SendScreenStatus: %w", err)
	}
	if err := c.send(f); err != nil {
		return fmt.Errorf("relayconn: SendScreenStatus: send: %w", err)
	}
	return nil
}

// SendPairingRedeemed reports one pairing-grant redemption this relay performed
// upstream (REL-124), as REL-124a's `pairing.redeemed {grant_id, redeemed_at}`.
//
// It rides THIS connection rather than the telemetry channel because REL-095
// closes that channel's schema set to exactly the five `events/1` registered
// schemas REL-093–094 name and makes `events/1` their sole normative source:
// there is no schema there that carries a redemption, and relay/1 may not mint
// one into another contract's registry. REL-124's own "at the next telemetry or
// connection opportunity" already admits this vehicle; REL-124a fixes it as the
// vehicle.
//
// It BLOCKS until the app peer's `pairing.redeemed_ack` arrives, and returns
// nil only then. That is REL-124d's own rule: a write that left the socket is
// not a record the app peer kept, so the caller may retire its owed-report
// record on a nil return and on nothing else. A *Refusal carrying
// PAIRING_REPORT_UNAUTHORIZED (REL-124b) is the app peer stating this grant is
// not this relay's to report — the taxonomy marks it non-retryable, so a caller
// stops re-sending THAT report rather than wedging its ledger; any other error
// leaves the redemption owed for the next connection opportunity.
//
// No trace id rides the frame: a redemption traces to a player's own pairing
// request, not to an operation on this connection. The envelope's relay_id is
// this connection's authenticated identity, and the body carries no reporter
// field at all — the app peer attributes the report to the connection it
// arrived on, never to anything the frame asserts (REL-124b).
func (c *Client) SendPairingRedeemed(body wire.PairingRedeemedBody) error {
	if body.GrantID == "" {
		return fmt.Errorf("relayconn: SendPairingRedeemed: grant_id must not be empty")
	}
	reqID := ulid.New()
	req, err := wire.NewFrame(wire.FrameTypePairingRedeemed, reqID, c.relayID, body)
	if err != nil {
		return fmt.Errorf("relayconn: SendPairingRedeemed: %w", err)
	}

	ch := make(chan wire.Frame, 1)
	c.mu.Lock()
	if c.connErr != nil {
		err := c.connErr
		c.mu.Unlock()
		return fmt.Errorf("relayconn: SendPairingRedeemed: connection is down: %w", err)
	}
	c.pending[reqID] = ch
	c.mu.Unlock()

	if err := c.send(req); err != nil {
		c.mu.Lock()
		delete(c.pending, reqID)
		c.mu.Unlock()
		return fmt.Errorf("relayconn: SendPairingRedeemed: send: %w", err)
	}

	select {
	case reply, ok := <-ch:
		if !ok {
			return fmt.Errorf("relayconn: SendPairingRedeemed: connection closed awaiting ack: %w", c.Err())
		}
		if reply.Type == wire.FrameTypeError {
			return &Refusal{Code: reply.Code, Message: reply.Message}
		}
		if reply.Type != wire.FrameTypePairingRedeemedAck {
			return fmt.Errorf("relayconn: SendPairingRedeemed: reply is %q, want %s", reply.Type, wire.FrameTypePairingRedeemedAck)
		}
		return nil
	case <-time.After(requestTimeout):
		c.mu.Lock()
		delete(c.pending, reqID)
		c.mu.Unlock()
		return fmt.Errorf("relayconn: SendPairingRedeemed: timed out awaiting ack for grant %s", body.GrantID)
	}
}

// RelayID returns the enrolled relay identity this connection authenticated as.
func (c *Client) RelayID() string { return c.relayID }

// HelloAck returns the negotiated hello-ack body (negotiated_version,
// feature subset, the app peer's authoritative site_binding).
func (c *Client) HelloAck() hello.HelloAckBody { return c.ackBody }

// Done is closed when the connection dies; Err then reports why. The
// client never reconnects itself — the owner re-Dials.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports what ended the connection, nil while live. The error names
// the half that noticed — read, write, or heartbeat (fail) — because "the
// connection is down" without which half stopped working is the report
// HV-22 gave an operator for two and a half hours.
//
// A caller that has observed Done can never read nil here: fail sets the
// cause and closes done under one lock. (The load-bearing direction is that
// one — Done closed implies Err set. Closing inside the lock also makes the
// converse hold, which is stronger than any caller needs and is a choice
// rather than something a test can pin: the window it removes is a few
// nanoseconds wide.)
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connErr
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
		// An unencodable frame is this side's own bug and says nothing
		// about the connection — nothing left the socket, so the
		// connection is not dead and must not be torn down.
		return fmt.Errorf("relayconn: encode frame: %w", err)
	}
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		// A frame that could not be written means this connection can no
		// longer carry one, and that has to become the CONNECTION's fact,
		// not just this caller's return value. send's own doc already
		// declared this posture ("a write that cannot complete within the
		// deadline closes the connection") — before HV-22 nothing
		// implemented it, so the supervisor above never learned, callers
		// kept writing to the same dead socket, and the transport had a
		// death detector on the read path only.
		c.fail(sideWrite, err)
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
