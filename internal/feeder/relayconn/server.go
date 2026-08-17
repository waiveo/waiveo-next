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
//	  `state.unchanged` (REL-050/051), generation-advance nudges
//	  (`state.changed`, REL-057) pushed server→relay
//	  (NotifyGenerationAdvance), and app-initiated `device.command`
//	  requests correlated to their `device.command_result` replies
//	  (REL-112, SendDeviceCommand).
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
	"sort"
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

// CandidateSink receives one relay's full `device.candidates` report
// (REL-110/111) and replaces that relay's view with it.
// internal/app/devices.Registry satisfies it directly via ApplyCandidates — the
// identical method signature, no adapter — so this package depends on the
// INTAKE capability rather than on the read model's concrete type, and a
// deployment with no device read model simply wires none.
//
// relayID is the connection's AUTHENTICATED identity (the mTLS
// client-certificate identity, REL-041/150), never the `relay_id` the frame
// asserts. That is the whole point of the sink taking it as an argument: the
// contract requires the app peer to disambiguate a relay by its enrolled
// cryptographic identity, and a report is a full-set REPLACE — a relay able to
// name another relay here could wipe that relay's entire device view with one
// frame.
//
// An error means the report was refused; the connection layer answers the relay
// with a typed refusal and the sink's prior view is expected to be intact.
type CandidateSink interface {
	ApplyCandidates(relayID string, candidates []wire.DeviceCandidate) error

	// Forget drops everything a relay reported. It is called when that relay's
	// enrollment is REVOKED, not when its connection merely drops: a relay that
	// reconnects a second later would otherwise have its devices blank out in
	// between, and a device flickering out of the read model is worse than one
	// described a moment too long. Revocation is different in kind — the point
	// of revoking a relay is that it no longer speaks for the site, and leaving
	// its last full report serving `/devices` indefinitely would leave it
	// speaking for the site anyway.
	//
	// Forgetting a relay that reported nothing is a no-op.
	Forget(relayID string)
}

// ErrGrantBoundElsewhere is what a RedemptionSink returns for a report naming a
// pairing grant bound (REL-121b) to a relay OTHER than the one reporting it —
// the one refusal REL-124b names. The connection layer answers it with the
// taxonomy's `PAIRING_REPORT_UNAUTHORIZED` and keeps the connection up: a relay
// reaching for another relay's grant is refused, not disconnected, exactly as a
// refused `device.candidates` report is.
var ErrGrantBoundElsewhere = errors.New("relayconn: pairing grant is bound to a different relay")

// RedemptionSink receives one relay's `pairing.redeemed` report (REL-124), the
// upstream record that a pairing grant was consumed — including one consumed
// while the relay was disconnected (REL-122/REL-124a). The deployment adapts
// its own record to this interface (cmd/waiveo-feeder wires the authoring
// store's RetirePairingGrant), so this package depends on the INTAKE capability
// rather than on any store type.
//
// relayID is the connection's AUTHENTICATED identity (the mTLS
// client-certificate identity, REL-041/150), never a value the frame asserts —
// REL-124b requires exactly that, and the sink takes it as an argument so there
// is no other value available to attribute the report to. The sink MUST refuse
// (ErrGrantBoundElsewhere) a grant bound to a different relay, MUST treat a
// grant it does not hold as an idempotent no-op rather than a refusal (a relay
// re-sending an owed report, or reporting one already retired, is doing what
// REL-124a requires of it), and MUST NOT create, revive, or un-consume anything.
//
// This report is an audit and retirement mechanism, never the enforcement of
// one-time redemption: that is REL-121b/REL-121c's binding, which holds with no
// report ever delivered (REL-124c). Nothing downstream of this sink may treat
// the absence of a report as evidence a grant was not redeemed.
type RedemptionSink interface {
	ApplyRedemption(relayID string, body wire.PairingRedeemedBody) error
}

// WithRedemptionSink wires the intake a relay's `pairing.redeemed` reports are
// applied to. Optional: without it the report is accepted off the wire and
// dropped — REL-124's obligation is on the RELAY (to report), and the audit
// record's own shape is explicitly out of relay/1's scope, so a deployment that
// keeps no such record is conformant and gets the same REL-004 additive
// tolerance an unknown verb gets.
func WithRedemptionSink(sink RedemptionSink) Option {
	return func(s *Server) { s.redemptions = sink }
}

// WithCandidateSink wires the intake a relay's `device.candidates` reports are
// applied to. Optional: without it the report is accepted off the wire and
// dropped — the honest behaviour for a deployment that runs no device read
// model, and the same REL-004 additive tolerance an unknown verb gets.
func WithCandidateSink(sink CandidateSink) Option {
	return func(s *Server) { s.candidates = sink }
}

// ScreenStatusSink receives one relay's full `screen.status` report — what each
// screen behind that relay has been observed doing (parity row 5.8,
// wire/screenstatus.go) — and takes it as REPLACING that relay's prior view.
// internal/app/screens.Registry satisfies it directly via ApplyScreenStatus,
// the identical method signature with no adapter, so this package depends on the
// intake CAPABILITY rather than on the read model's concrete type.
//
// relayID is the connection's AUTHENTICATED identity (the mTLS
// client-certificate identity, REL-041/150), never the `relay_id` the frame
// asserts. It is the same rule CandidateSink states, for the same reason and
// with the same stakes: a report is a full-set REPLACE, so a relay able to name
// another relay here could blank that relay's whole screen view — making a live
// wall read as never-checked-in, which is precisely the alarm this surface
// exists to raise honestly.
//
// takenAtMs is the app peer's OWN clock reading at the instant the report was
// received. It is supplied by the caller rather than read inside the sink so the
// whole report is anchored to one instant, and so the sink can be driven from a
// test with a fixed clock. Every age in the body is relative to the relay's own
// clock at report time (wire/screenstatus.go); this is the anchor the read model
// adds its own elapsed time to, which is what keeps the answer skew-free.
//
// An error means the report was refused; the connection layer answers the relay
// with a typed refusal and the sink's prior view is expected to be intact.
type ScreenStatusSink interface {
	ApplyScreenStatus(relayID string, takenAtMs int64, screens []wire.ScreenStatusEntry) error

	// ForgetScreens drops everything a relay reported, called when that relay's
	// enrollment is REVOKED — not when its connection merely drops. Exactly
	// CandidateSink.Forget's distinction and for exactly its reason: a relay that
	// reconnects a second later must not have its screens blink out in between,
	// while a REVOKED relay no longer speaks for the site at all and leaving its
	// last report serving the console would leave it speaking for the site anyway.
	ForgetScreens(relayID string)
}

// WithScreenStatusSink wires the intake a relay's `screen.status` reports are
// applied to. Optional, with the same accepted-and-dropped degrade
// WithCandidateSink documents: a deployment running no screen read model is
// conformant, and gets REL-004's additive tolerance.
//
// nowMs is the app peer's clock, read once per report to anchor it (see
// ScreenStatusSink). It is required alongside the sink rather than defaulted to
// time.Now inside this package, because a package that silently reads the wall
// clock cannot be tested for the one property this whole surface rests on —
// that an age reported later is larger.
func WithScreenStatusSink(sink ScreenStatusSink, nowMs func() int64) Option {
	return func(s *Server) {
		s.screenStatus = sink
		s.screenStatusNow = nowMs
	}
}

// DiscoveryScanStatusSink receives one relay's `discovery.scan_status` report —
// what its scan engine is doing right now (wire.DiscoveryScanStatusBody).
//
// It is a plain latest-wins set rather than a merge: the report IS the relay's
// whole scan state, so a later one replaces an earlier one outright. There is
// deliberately no Forget counterpart the way ScreenStatusSink has one — scan
// state is not a claim about the SITE (a screen a revoked relay reported would
// still be speaking for the site), it is a fact about the relay's own engine,
// and it becomes irrelevant on its own the moment that relay stops appearing in
// the connected set the console reads beside it.
type DiscoveryScanStatusSink interface {
	ApplyDiscoveryScanStatus(relayID string, status wire.DiscoveryScanStatusBody)
}

// WithDiscoveryScanStatusSink wires the intake for those reports. Optional, with
// the same accepted-and-dropped degrade the other sinks document: a deployment
// that wires none is conformant and simply shows no scan state.
func WithDiscoveryScanStatusSink(sink DiscoveryScanStatusSink) Option {
	return func(s *Server) {
		s.scanStatus = sink
	}
}

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
	candidates         CandidateSink
	redemptions        RedemptionSink
	screenStatus       ScreenStatusSink
	scanStatus         DiscoveryScanStatusSink
	screenStatusNow    func() int64
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

// ErrRelayNotConnected is returned by SendDeviceCommand when the named
// relay has no live authenticated connection to carry the command — the
// relay is offline, mid-reconnect, or was never enrolled.
//
// relay/1 defines no store-and-forward for `device.command`: the device
// plane's command surface is a live request/response exchange (REL-112),
// and the contract's own offline continuity (Negotiation, Offline
// continuity) covers a relay evaluating its ALREADY-PULLED desired state
// while disconnected, never an app peer queuing operator commands for
// later delivery. Silently dropping the command is therefore not an option
// this contract leaves open: the caller is refused explicitly and decides
// whether to retry.
var ErrRelayNotConnected = errors.New("relayconn: relay has no live connection")

// RefusalError is a REL-007 top-level error frame received in place of an
// expected reply — a typed refusal of an app-initiated request, carrying
// the Error-taxonomy code verbatim. It is distinct from a
// `device.command_result` whose own `error` field is populated: that is a
// well-formed answer the relay produced (REL-112/113) and is returned as a
// result body, not as this error.
type RefusalError struct {
	Code    string
	Message string
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("relayconn: relay refused the request: %s: %s", e.Code, e.Message)
}

// SendDeviceCommand dispatches one `device.command` to relayID over its
// EXISTING authenticated connection and returns the correlated
// `device.command_result` body (REL-112). traceID is the originating
// operation's own trace id, carried so one identifier correlates the
// operation across the app peer, the relay, and any record it produces
// (REL-006); pass "" when the command traces to no such operation.
//
// The exchange is synchronous by contract shape: REL-112 pairs the command
// with a single result frame on the same connection, so the caller's own
// request/response cycle can carry the outcome and api/1's 202-plus-Job
// rule for not-synchronously-completable work (API-111) does not apply.
// ctx bounds the wait; a caller MUST supply a deadline, since a relay that
// accepts the frame and never answers would otherwise block forever.
//
// Errors: ErrRelayNotConnected when no live connection exists (never a
// silent drop); *RefusalError when the relay answered with a REL-007 typed
// error frame; ctx.Err() when the wait ended first; and a plain error when
// the connection died mid-exchange. A returned result body with
// `ok:false` is NOT an error — it is the relay's own typed answer
// (COMMAND_UNRESOLVED, COMMAND_TARGET_UNREACHABLE) and is returned intact.
//
// body.Params MAY carry credential material scoped to this one dispatch
// (REL-114). This method never logs it and never writes it anywhere: it is
// marshaled into the outbound frame and dropped.
func (s *Server) SendDeviceCommand(ctx context.Context, relayID, traceID string, body wire.DeviceCommandBody) (wire.DeviceCommandResultBody, error) {
	conn, ok := s.connFor(relayID)
	if !ok {
		return wire.DeviceCommandResultBody{}, ErrRelayNotConnected
	}
	// REL-016: a command is a steady-state frame like any other — a revoked
	// relay is refused and disconnected rather than handed an operator's
	// command to execute.
	if s.isRevoked(conn.relayID, conn.serial) {
		go s.closeRevoked(conn)
		return wire.DeviceCommandResultBody{}, ErrRelayNotConnected
	}

	id := ulid.New()
	req, err := wire.NewFrame(wire.FrameTypeDeviceCommand, id, relayID, body)
	if err != nil {
		return wire.DeviceCommandResultBody{}, err
	}
	req.TraceID = traceID

	ch, err := conn.register(id)
	if err != nil {
		return wire.DeviceCommandResultBody{}, err
	}
	if err := conn.send(req); err != nil {
		conn.reap(id)
		return wire.DeviceCommandResultBody{}, fmt.Errorf("relayconn: SendDeviceCommand: send: %w", err)
	}

	select {
	case reply, open := <-ch:
		if !open {
			// closePending: the connection died with this request in flight.
			return wire.DeviceCommandResultBody{}, ErrRelayNotConnected
		}
		if reply.Type == wire.FrameTypeError {
			return wire.DeviceCommandResultBody{}, &RefusalError{Code: reply.Code, Message: reply.Message}
		}
		if reply.Type != wire.FrameTypeDeviceCommandResult {
			return wire.DeviceCommandResultBody{}, fmt.Errorf("relayconn: SendDeviceCommand: reply is %q, want %s", reply.Type, wire.FrameTypeDeviceCommandResult)
		}
		var out wire.DeviceCommandResultBody
		if err := reply.DecodeBody(&out); err != nil {
			return wire.DeviceCommandResultBody{}, fmt.Errorf("relayconn: SendDeviceCommand: %w", err)
		}
		return out, nil
	case <-ctx.Done():
		// The caller gave up first: drop the correlation entry so an answer
		// that arrives later is treated as uncorrelated (and ignored) rather
		// than accumulating in the map forever.
		conn.reap(id)
		return wire.DeviceCommandResultBody{}, ctx.Err()
	}
}

// SendDiscoveryScan asks one relay to run a single ACTIVE scan of its own
// network and returns the relay's acceptance (wire `discovery.scan`).
//
// It mirrors SendDeviceCommand's transport contract exactly — not-connected and
// revoked relays are refused the same way, the exchange is correlated by id, and
// a caller that gives up first reaps its correlation entry — with one deliberate
// difference in MEANING: the reply is an ACCEPTANCE, not a set of findings. A
// scan runs far longer than a request/response exchange should be held open, so
// the relay answers "started, under this scan id" and the sightings arrive
// afterwards through the ordinary `device.candidates` report, exactly as passive
// sightings do.
//
// `ok:false` is NOT a transport error — it is the relay's own typed refusal (a
// subnet its policy does not permit, no scan capability wired) and is returned
// intact for the caller to render.
func (s *Server) SendDiscoveryScan(ctx context.Context, relayID, traceID string, body wire.DiscoveryScanBody) (wire.DiscoveryScanResultBody, error) {
	conn, ok := s.connFor(relayID)
	if !ok {
		return wire.DiscoveryScanResultBody{}, ErrRelayNotConnected
	}
	// REL-016: like a command, a scan is a steady-state frame — a revoked relay
	// is refused and disconnected rather than asked to probe a network.
	if s.isRevoked(conn.relayID, conn.serial) {
		go s.closeRevoked(conn)
		return wire.DiscoveryScanResultBody{}, ErrRelayNotConnected
	}

	id := ulid.New()
	req, err := wire.NewFrame(wire.FrameTypeDiscoveryScan, id, relayID, body)
	if err != nil {
		return wire.DiscoveryScanResultBody{}, err
	}
	req.TraceID = traceID

	ch, err := conn.register(id)
	if err != nil {
		return wire.DiscoveryScanResultBody{}, err
	}
	if err := conn.send(req); err != nil {
		conn.reap(id)
		return wire.DiscoveryScanResultBody{}, fmt.Errorf("relayconn: SendDiscoveryScan: send: %w", err)
	}

	select {
	case reply, open := <-ch:
		if !open {
			return wire.DiscoveryScanResultBody{}, ErrRelayNotConnected
		}
		if reply.Type == wire.FrameTypeError {
			return wire.DiscoveryScanResultBody{}, &RefusalError{Code: reply.Code, Message: reply.Message}
		}
		if reply.Type != wire.FrameTypeDiscoveryScanResult {
			return wire.DiscoveryScanResultBody{}, fmt.Errorf("relayconn: SendDiscoveryScan: reply is %q, want %s", reply.Type, wire.FrameTypeDiscoveryScanResult)
		}
		var out wire.DiscoveryScanResultBody
		if err := reply.DecodeBody(&out); err != nil {
			return wire.DiscoveryScanResultBody{}, fmt.Errorf("relayconn: SendDiscoveryScan: %w", err)
		}
		return out, nil
	case <-ctx.Done():
		conn.reap(id)
		return wire.DiscoveryScanResultBody{}, ctx.Err()
	}
}

// connFor returns the live connection authenticated as relayID. A relay
// holds at most one connection at a time in practice; when a reconnect has
// briefly left two registered, the most recently registered one is
// indistinguishable from the other here and either is a correct carrier —
// both are authenticated as the same enrolled identity.
func (s *Server) connFor(relayID string) (*serverConn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		if c.relayID == relayID {
			return c, true
		}
	}
	return nil, false
}

// PendingCommandCount reports how many app-initiated requests are awaiting
// a reply across every live connection — observability for tests and
// operators. A correctly reaped exchange leaves this at zero; a leaked
// correlation entry would not.
func (s *Server) PendingCommandCount() int {
	s.mu.Lock()
	conns := make([]*serverConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	n := 0
	for _, c := range conns {
		n += c.pendingCount()
	}
	return n
}

// ConnCount reports the number of live authenticated connections —
// observability for tests and operators (a reaped connection leaves this
// count; a leaked one would not).
func (s *Server) ConnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// ConnectedRelay is one live authenticated relay connection's identity and
// the canonical advertised address its hello's subnet_metadata carried
// (REL-037) — per that requirement, the same {host, port} the relay itself
// encodes into a pairing code it displays (REL-126).
type ConnectedRelay struct {
	RelayID           string
	AdvertisedAddress string
}

// ConnectedRelays reports every live authenticated relay connection, in
// stable relay_id order. It is what the app's pairing-code issuance reads to
// learn WHERE a freshly minted grant's code should dial: the app holds the
// grant_selector (it minted the grant) and the relay's trust-anchor key (it
// issued the relay's certificate at enrollment); the dial address is the one
// REL-126 component only the relay knows, and its hello (REL-037) is exactly
// where the relay states it.
func (s *Server) ConnectedRelays() []ConnectedRelay {
	s.mu.Lock()
	out := make([]ConnectedRelay, 0, len(s.conns))
	for c := range s.conns {
		out = append(out, ConnectedRelay{RelayID: c.relayID, AdvertisedAddress: c.advertisedAddress})
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].RelayID < out[j].RelayID })
	return out
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
//
// pending is this side's request/reply correlation map — the mirror image
// of the relay client's own, for the ONE exchange the app peer originates
// (`device.command` → `device.command_result`, REL-112/006). An entry lives
// exactly as long as its request is outstanding: the read loop removes it
// when the correlated reply (or a REL-007 error frame carrying the same id)
// arrives, the requester removes it when its context ends first, and
// closePending drains the whole map when the connection dies — so a peer
// that never answers, or dies mid-exchange, leaks no entry and blocks no
// caller.
type serverConn struct {
	ws      *websocket.Conn
	relayID string
	serial  string // presented client-cert serial, for REL-016 re-checks
	// advertisedAddress is this relay's own canonical advertised address from
	// its hello's subnet_metadata (REL-037) — by that requirement's own text,
	// "the same address it advertises in its own discovery/pairing
	// responses", i.e. the {host, port} a pairing code for this relay dials.
	// Captured at handshake so the app peer can form a pairing code for a
	// grant it mints (REL-126's three components) without asking the relay.
	advertisedAddress string
	writeTimeout      time.Duration

	pendingMu sync.Mutex
	pending   map[string]chan wire.Frame
	closed    bool
}

// register claims id in the correlation map and returns the channel its
// reply will be delivered on, or an error if the connection is already
// gone (so a request is never armed against a dead conn).
func (c *serverConn) register(id string) (chan wire.Frame, error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if c.closed {
		return nil, ErrRelayNotConnected
	}
	ch := make(chan wire.Frame, 1)
	c.pending[id] = ch
	return ch, nil
}

// reap removes id from the correlation map — the requester's own cleanup
// when its context ends before a reply arrives. Idempotent: the read loop
// may already have resolved and removed the entry.
func (c *serverConn) reap(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

// resolve delivers f to the waiter registered under f.ID, reporting whether
// one existed. The entry is removed under the same lock the lookup takes,
// so exactly one delivery can ever happen for a given correlation id — two
// replies bearing the same id cannot both complete a request.
func (c *serverConn) resolve(f wire.Frame) bool {
	c.pendingMu.Lock()
	ch, ok := c.pending[f.ID]
	if ok {
		delete(c.pending, f.ID)
	}
	c.pendingMu.Unlock()
	if !ok {
		return false
	}
	ch <- f // buffered(1) and single-delivery: never blocks the read loop
	return true
}

// closePending marks the connection dead and closes every outstanding
// waiter's channel, so a caller blocked on a reply that will now never
// arrive unblocks with the connection-closed error instead of hanging
// until its own deadline.
func (c *serverConn) closePending() {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = map[string]chan wire.Frame{}
	c.closed = true
	c.pendingMu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
}

// pendingCount is how many app-initiated requests are outstanding on this
// connection.
func (c *serverConn) pendingCount() int {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return len(c.pending)
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
	conn := &serverConn{
		ws: ws, relayID: relayID, serial: serial, writeTimeout: s.writeTimeout,
		pending: map[string]chan wire.Frame{},
	}
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

	// The hello verified: keep its subnet_metadata's canonical advertised
	// address (REL-037) for ConnectedRelays — the pairing-code dial address
	// this relay itself would encode (REL-126). Recorded only after
	// BuildHelloAck's channel-binding verification succeeded, never off an
	// unauthenticated frame.
	conn.advertisedAddress = hb.SubnetMetadata.AdvertisedAddress

	// Authenticated steady state: register for server-initiated pushes,
	// serve pulls until the connection drops.
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		// Every app-initiated request still awaiting a reply on this
		// connection is now unanswerable: unblock its caller and clear the
		// correlation map rather than leaving entries (and goroutines)
		// behind on a dead conn.
		conn.closePending()
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
			s.forgetCandidates(relayID)
			return
		}
		// A frame whose id matches an outstanding app-initiated request
		// completes it (REL-006: the pair shares one correlation id). This
		// runs BEFORE the verb switch so it covers both the
		// `device.command_result` reply and a REL-007 top-level error frame
		// refusing the same request — the requester classifies which it got.
		if f.ID != "" && conn.resolve(f) {
			continue
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
		case wire.FrameTypeDeviceCandidates:
			if err := s.handleDeviceCandidates(conn, f, relayID); err != nil {
				return
			}
		case wire.FrameTypeScreenStatus:
			if err := s.handleScreenStatus(conn, f, relayID); err != nil {
				return
			}
		case wire.FrameTypeDiscoveryScanStatus:
			if err := s.handleDiscoveryScanStatus(conn, f, relayID); err != nil {
				return
			}
		case wire.FrameTypePairingRedeemed:
			if err := s.handlePairingRedeemed(conn, f, relayID); err != nil {
				return
			}
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
	s.forgetCandidates(conn.relayID)
}

// forgetCandidates drops a revoked relay's reported devices, if an intake is
// wired. Revocation means the relay no longer speaks for this site; without
// this its last full report kept describing the site from `/devices` and
// `/entities` forever, which is the state revocation exists to end.
func (s *Server) forgetCandidates(relayID string) {
	if s.candidates != nil {
		s.candidates.Forget(relayID)
	}
	// The screen view goes with it, and for the identical reason: a revoked
	// relay's last screen report would otherwise keep a console reporting live
	// screens on behalf of a relay this site has disowned. Both are dropped from
	// the same call site so neither can be forgotten when the other is.
	if s.screenStatus != nil {
		s.screenStatus.ForgetScreens(relayID)
	}
}

// handleScreenStatus applies one `screen.status` report to the wired intake
// (parity row 5.8). It returns a non-nil error only when the connection itself
// must end (a failed write); a refused report is answered with a typed error
// frame and the connection continues — the same posture handleDeviceCandidates
// takes, and for its reason: a bad report is not a protocol-ordering violation.
//
// relayID is the connection's AUTHENTICATED identity, never f.RelayID. See
// ScreenStatusSink's own doc for why that is load-bearing here specifically.
// handleDiscoveryScanStatus records one relay's scan-engine state. Unlike the
// screen report there is nothing to validate beyond decoding: every member is
// descriptive, none of it authorizes anything or names another relay's
// resources, and the relayID it is filed under is the AUTHENTICATED connection's
// identity rather than anything the frame asserts.
func (s *Server) handleDiscoveryScanStatus(conn *serverConn, f wire.Frame, relayID string) error {
	var body wire.DiscoveryScanStatusBody
	if err := f.DecodeBody(&body); err != nil {
		return conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
			"MALFORMED_MESSAGE", "discovery.scan_status body did not decode"))
	}
	if s.scanStatus == nil {
		return nil // no read model wired: accepted and dropped (REL-004)
	}
	s.scanStatus.ApplyDiscoveryScanStatus(relayID, body)
	return nil
}

func (s *Server) handleScreenStatus(conn *serverConn, f wire.Frame, relayID string) error {
	var body wire.ScreenStatusBody
	if err := f.DecodeBody(&body); err != nil {
		return conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
			"MALFORMED_MESSAGE", "screen.status body did not decode"))
	}
	if s.screenStatus == nil || s.screenStatusNow == nil {
		return nil // no read model wired: accepted and dropped (REL-004)
	}
	if err := s.screenStatus.ApplyScreenStatus(relayID, s.screenStatusNow(), body.Screens); err != nil {
		return conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
			"MALFORMED_MESSAGE", "screen.status report refused: "+err.Error()))
	}
	return nil
}

// handleDeviceCandidates applies one `device.candidates` report to the wired
// intake (REL-110/111). It returns a non-nil error only when the connection
// itself must end (a failed write); a refused report is answered with a typed
// error frame and the connection continues, since a bad report is not a
// protocol-ordering violation.
//
// relayID is the connection's AUTHENTICATED identity, taken from the mTLS
// client certificate at handshake — never f.RelayID. REL-150 requires an app
// peer to disambiguate a relay by its enrolled cryptographic identity, and this
// verb is the one where getting that wrong is catastrophic rather than merely
// wrong: the report REPLACES the named relay's whole device view, so honouring
// a self-asserted relay_id would let any enrolled relay delete every other
// relay's devices, and re-point their entities' commands at itself.
func (s *Server) handleDeviceCandidates(conn *serverConn, f wire.Frame, relayID string) error {
	var body wire.DeviceCandidatesBody
	if err := f.DecodeBody(&body); err != nil {
		// An undecodable body fails the verb's minimum shape — the taxonomy's
		// MALFORMED_MESSAGE (REL-002), as state.ack's own decode failure is.
		return conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
			"MALFORMED_MESSAGE", "device.candidates body did not decode"))
	}
	if s.candidates == nil {
		return nil // no read model wired: accepted and dropped (REL-004)
	}
	if err := s.candidates.ApplyCandidates(relayID, body.Candidates); err != nil {
		// The report was refused whole (the intake applies all or nothing), so
		// the app peer still holds this relay's PRIOR view. Telling the relay
		// is what lets it correct itself rather than believing a view the app
		// peer never took.
		return conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
			"MALFORMED_MESSAGE", "device.candidates report refused: "+err.Error()))
	}
	return nil
}

// handlePairingRedeemed applies one `pairing.redeemed` report to the wired
// intake (REL-124/REL-124a). It returns a non-nil error only when the
// connection itself must end (a failed write); a refused report is answered
// with a typed error frame and the connection continues, since a bad report is
// not a protocol-ordering violation — the same posture handleDeviceCandidates
// takes.
//
// relayID is the connection's AUTHENTICATED identity, taken from the mTLS
// client certificate at handshake — never f.RelayID. REL-124b requires exactly
// that, and it is what keeps this verb from being a lever one enrolled relay
// can pull against another: the report retires a grant, so honouring a
// self-asserted relay_id would let any relay cancel a pairing in progress at a
// sibling by naming its grant. The body carries no relay field at all
// (wire.PairingRedeemedBody), so there is nothing here to be tempted by.
func (s *Server) handlePairingRedeemed(conn *serverConn, f wire.Frame, relayID string) error {
	var body wire.PairingRedeemedBody
	if err := f.DecodeBody(&body); err != nil {
		// An undecodable body fails the verb's minimum shape — the taxonomy's
		// MALFORMED_MESSAGE (REL-002), as state.ack's own decode failure is.
		return conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
			"MALFORMED_MESSAGE", "pairing.redeemed body did not decode"))
	}
	if body.GrantID == "" {
		return conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
			"MALFORMED_MESSAGE", "pairing.redeemed carries no grant_id"))
	}
	if s.redemptions != nil {
		if err := s.redemptions.ApplyRedemption(relayID, body); err != nil {
			if errors.Is(err, ErrGrantBoundElsewhere) {
				return conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
					"PAIRING_REPORT_UNAUTHORIZED", "the reported pairing grant is bound to a different relay (REL-124b)"))
			}
			// Anything else is this side's own failure to record the report,
			// not the relay's fault. Answering an error rather than an ack is
			// what makes REL-124d's re-send rule work: the relay keeps the
			// redemption owed and tries again at its next connection
			// opportunity, rather than retiring a report nothing recorded.
			return conn.send(wire.NewErrorFrame(f.ID, f.TraceID, relayID,
				"INTERNAL", "the redemption report could not be recorded"))
		}
	}
	// Acknowledged (REL-124a): the report is recorded, or this deployment keeps
	// no record to fail at keeping (WithRedemptionSink's doc — REL-124's
	// obligation is the relay's, and the audit record's shape is out of
	// relay/1's scope). Either way the relay may retire it, and MUST NOT keep
	// re-sending a report the app peer has taken.
	ack, err := wire.NewFrame(wire.FrameTypePairingRedeemedAck, f.ID, relayID,
		wire.PairingRedeemedAckBody{GrantID: body.GrantID})
	if err != nil {
		return err
	}
	ack.TraceID = f.TraceID
	return conn.send(ack)
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
