// Package relayconn is the relay side of the relay/1 persistent connection
// SPIKE: the outbound-only dialer (REL-001 — a relay is always the
// connecting party) that opens ONE mutually authenticated WS connection to
// the app peer's /relay/v1 and runs the whole exchange over it: challenge →
// hello → hello-ack, then correlated request/response (state.pull) plus
// server-initiated frames (state.changed nudges / pushed snapshots).
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
//     key to THIS channel.
//
// SPIKE CORNERS (deliberate, recorded): Dial does not auto-reconnect (a
// dropped connection surfaces via Done/Err and the owner re-Dials), there
// is no heartbeat, per-request timeout is a fixed constant, and
// instrumentation (SentFrames/ReceivedFrames) grows without bound — it
// exists for the e2e proof, not production.
package relayconn

import (
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/maaxton/waiveo-next/internal/relay/clocktrust"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// dialTimeout bounds the whole TCP+TLS+WS+challenge+hello handshake;
// requestTimeout bounds one correlated request/response exchange. Loopback
// deployment values — generous, not tuned.
const (
	dialTimeout    = 10 * time.Second
	requestTimeout = 10 * time.Second
)

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
	// channel-binding signature, exactly as hello.PerformHello takes it.
	Declaration hello.Declaration

	// ClockTrusted and Now feed the REL-136 temporal decision on the app
	// peer's certificate (clocktrust). A zero Now means time.Now.
	ClockTrusted bool
	Now          func() time.Time

	// OnServerFrame, when non-nil, receives every server-initiated frame
	// (one whose id matches no in-flight request) — state.changed nudges,
	// pushed snapshots. Frames are delivered IN ORDER on one dedicated
	// goroutine; the callback may itself call Pull.
	OnServerFrame func(wire.Frame)
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
	ws       *websocket.Conn
	relayID  string
	ackBody  hello.HelloAckBody
	writeMu  sync.Mutex
	mu       sync.Mutex
	pending  map[string]chan wire.Frame
	done     chan struct{}
	readErr  error
	serverCh chan wire.Frame

	instMu   sync.Mutex
	sent     []wire.Frame
	received []wire.Frame
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

	// REL-136/137 posture verbatim from the HTTP-era verifier: SPKI pin, no
	// chain validation, temporal check deferred on an untrusted clock —
	// plus the client certificate that makes the connection mutual (REL-003).
	tlsCfg := clocktrust.AppPeerTLSConfig(pin, cfg.ClockTrusted, now(), time.Duration(clocktrust.DefaultBoundedGraceMs)*time.Millisecond)
	tlsCfg.Certificates = []tls.Certificate{leaf}

	tlsConn, err := tls.Dial("tcp", hostPort, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("relayconn: Dial: TLS dial %s: %w", hostPort, err)
	}
	// One deadline over the whole handshake; cleared before steady state.
	_ = tlsConn.SetDeadline(now().Add(dialTimeout))

	wsCfg, err := websocket.NewConfig(wsURL, "https://"+hostPort)
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("relayconn: Dial: ws config: %w", err)
	}
	wsCfg.Protocol = []string{wire.Subprotocol}

	ws, err := websocket.NewClient(wsCfg, tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("relayconn: Dial: ws upgrade: %w", err)
	}

	c := &Client{
		ws:       ws,
		relayID:  id.RelayID,
		pending:  map[string]chan wire.Frame{},
		done:     make(chan struct{}),
		serverCh: make(chan wire.Frame, 16),
	}

	if err := c.handshake(tlsConn, id, cfg.Declaration); err != nil {
		ws.Close()
		return nil, err
	}
	_ = tlsConn.SetDeadline(time.Time{})

	go c.readLoop()
	if cfg.OnServerFrame != nil {
		go func() {
			for f := range c.serverCh {
				cfg.OnServerFrame(f)
			}
		}()
	}
	return c, nil
}

// handshake runs challenge → hello → hello-ack on the fresh connection.
func (c *Client) handshake(tlsConn *tls.Conn, id identity.RelayIdentity, decl hello.Declaration) error {
	// The server's challenge MUST be the first frame (REL-030).
	var challenge wire.Frame
	if err := c.receive(&challenge); err != nil {
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
	if err := c.send(helloFrame); err != nil {
		return fmt.Errorf("relayconn: handshake: send hello: %w", err)
	}

	var reply wire.Frame
	if err := c.receive(&reply); err != nil {
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
// in-flight request completes it; everything else is server-initiated and
// goes to the OnServerFrame dispatcher (in order, never on this goroutine —
// the callback may Pull, which needs this loop alive to complete).
func (c *Client) readLoop() {
	for {
		var f wire.Frame
		if err := websocket.JSON.Receive(c.ws, &f); err != nil {
			c.mu.Lock()
			c.readErr = err
			pending := c.pending
			c.pending = map[string]chan wire.Frame{}
			c.mu.Unlock()
			for _, ch := range pending {
				close(ch)
			}
			close(c.done)
			close(c.serverCh)
			return
		}
		c.record(&c.received, f)

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
		select {
		case c.serverCh <- f:
		default:
			// Dispatcher backlogged beyond the buffer: drop (spike corner —
			// production wants flow control, not silent loss).
		}
	}
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

// RelayID returns the enrolled relay identity this connection authenticated as.
func (c *Client) RelayID() string { return c.relayID }

// HelloAck returns the negotiated hello-ack body (negotiated_version,
// feature subset, the app peer's authoritative site_binding).
func (c *Client) HelloAck() hello.HelloAckBody { return c.ackBody }

// Done is closed when the connection dies; Err then reports why. The spike
// client never reconnects itself — the owner re-Dials.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports the read-loop error that ended the connection, nil while live.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// Close tears the connection down.
func (c *Client) Close() error { return c.ws.Close() }

// SentFrames / ReceivedFrames return copies of every frame this client sent
// or received (challenge included), in order — e2e-proof instrumentation.
func (c *Client) SentFrames() []wire.Frame     { return c.snapshotLog(&c.sent) }
func (c *Client) ReceivedFrames() []wire.Frame { return c.snapshotLog(&c.received) }

func (c *Client) snapshotLog(log *[]wire.Frame) []wire.Frame {
	c.instMu.Lock()
	defer c.instMu.Unlock()
	out := make([]wire.Frame, len(*log))
	copy(out, *log)
	return out
}

func (c *Client) record(log *[]wire.Frame, f wire.Frame) {
	c.instMu.Lock()
	*log = append(*log, f)
	c.instMu.Unlock()
}

func (c *Client) send(f wire.Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := websocket.JSON.Send(c.ws, f); err != nil {
		return err
	}
	c.record(&c.sent, f)
	return nil
}

// receive reads one frame during the handshake (before readLoop starts),
// recording it in the instrumentation log.
func (c *Client) receive(f *wire.Frame) error {
	if err := websocket.JSON.Receive(c.ws, f); err != nil {
		return err
	}
	c.record(&c.received, *f)
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
