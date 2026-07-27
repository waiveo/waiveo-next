// e2e_test.go is the framed-transport spike's proof: BOTH sides real (the
// feeder's enroll.Server + this package's WS server on one httptest mTLS
// listener; the relay's real enrollment client, identity store, dialer, and
// verify chain), one persistent connection, no simulated peers.
//
// Proven behaviors:
//
//	(i)   enroll over the existing bootstrap, then ONE WS connection runs
//	      challenge → hello → hello-ack; every post-enrollment frame carries
//	      relay_id (REL-005) and the challenge nonce is the TLS exporter
//	      derivation, independently recomputed by the relay (REL-040).
//	(ii)  state.pull → state.snapshot with shared correlation id + trace_id
//	      (REL-006); desiredstate.VerifyAndApply accepts and persists.
//	(iii) state.pull with since_generation=current → state.unchanged whose
//	      body carries the generation and NO sections key (byte-asserted).
//	(iv)  server-initiated push over the SAME connection: the conformant
//	      state.changed nudge (relay reacts with an immediate pull, applied
//	      well under the legacy 3s ticker) AND the REL-050-violating direct
//	      snapshot push (relay sends ZERO frames between snapshot N and
//	      N+1, byte-asserted from the client's sent-frame log).
//	(v)   typed refusals: a hello signed with the wrong key draws
//	      {type:"error", id, code:CHANNEL_BINDING_INVALID} then close; a
//	      state.pull before hello draws PROTOCOL_VIOLATION then close.
//
// Plus the reconnect measurement: the spike client does NOT auto-reconnect;
// after the app peer restarts (new listener, same persisted enrollment
// registry + same TLS leaf), a fresh Dial succeeds and pulling resumes.
package relayconn_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	feederenroll "github.com/maaxton/waiveo-next/internal/feeder/enroll"
	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	relayclient "github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

const testImagePath = "../origin/testdata/photon.png"

var testSite = hello.SiteBinding{ScopeNode: "site-e2e", TZ: "UTC", Lat: 51.5, Long: -0.1}

var testDeclaration = hello.Declaration{
	ProtocolVersion: "1.0",
	Features:        []string{"telemetry.latest_only_v1", "spike.only_flag"},
	SubnetMetadata:  hello.SubnetMetadata{AdvertisedAddress: "192.0.2.10:8443"},
	ClockState:      hello.ClockState{State: "untrusted", Source: "none"},
}

// snapshotHolder is the mutable SnapshotProvider the test advances
// generations through — standing in for cmd/waiveo-feeder's
// generation-cached desiredStateSource.current.
type snapshotHolder struct {
	mu   sync.Mutex
	snap wire.StateSnapshotBody
}

func (h *snapshotHolder) get() (wire.StateSnapshotBody, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snap, nil
}

func (h *snapshotHolder) set(s wire.StateSnapshotBody) {
	h.mu.Lock()
	h.snap = s
	h.mu.Unlock()
}

// harness is one fully-wired app peer: real enrollment server + WS server
// on one mTLS httptest listener.
type harness struct {
	enrollSrv *feederenroll.Server
	connSrv   *feederrelayconn.Server
	holder    *snapshotHolder
	feederID  *signing.Identity
	mux       *http.ServeMux
	ts        *httptest.Server
}

func newHarness(t *testing.T, opts ...feederrelayconn.Option) *harness {
	t.Helper()

	feederID, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	img, err := os.ReadFile(testImagePath)
	if err != nil {
		t.Fatalf("read fixture image: %v", err)
	}
	snap1, err := snapshot.Build(img, "https://origin.example", feederID, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	enrollSrv, err := feederenroll.NewServer(feederID, snap1)
	if err != nil {
		t.Fatalf("feederenroll.NewServer: %v", err)
	}

	holder := &snapshotHolder{snap: snap1}
	connSrv := feederrelayconn.New(
		holder.get,
		enrollSrv.RelayEnrollmentKey,
		testSite,
		hello.AppPeerImplementedMinors(1, 1),
		[]string{"telemetry.latest_only_v1"},
		opts...,
	)

	mux := http.NewServeMux()
	enrollSrv.Register(mux)
	mux.Handle("/relay/v1", connSrv.Handler())

	h := &harness{
		enrollSrv: enrollSrv, connSrv: connSrv, holder: holder,
		feederID: feederID, mux: mux,
	}
	h.ts = h.listen(t)
	return h
}

// listen starts an mTLS listener over h.mux: httptest's fixed server leaf
// (identical across restarts, exactly like a feeder that keeps its serving
// identity) + VerifyClientCertIfGiven against the enrollment CA — the
// listener change the spike proposes for the real feeder.
func (h *harness) listen(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(h.mux)
	ts.TLS = &tls.Config{
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  h.enrollSrv.ClientCAPool(),
		// The TLS 1.3 floor the production feeder listener pins (REL-040:
		// the challenge nonce derives from the session's exporter keying
		// material, so the session must be exporter-capable).
		MinVersion: tls.VersionTLS13,
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

// advanceGeneration builds + installs generation gen: same feeder signing
// identity, a changed program revision (so sections genuinely differ),
// rehashed and re-signed through the SAME shared canonicalization the
// production builder uses.
func (h *harness) advanceGeneration(t *testing.T, gen int64) wire.StateSnapshotBody {
	t.Helper()
	h.holder.mu.Lock()
	next := h.holder.snap
	h.holder.mu.Unlock()

	programs := make([]wire.ScreenProgram, len(next.Sections.ScreenPrograms))
	copy(programs, next.Sections.ScreenPrograms)
	if len(programs) == 0 {
		t.Fatal("harness snapshot carries no screen_programs")
	}
	programs[0].ProgramRevision = "rev-e2e-gen-" + strconv.FormatInt(gen, 10)
	next.Sections.ScreenPrograms = programs

	hash, err := wire.HashSections(next.Sections)
	if err != nil {
		t.Fatalf("HashSections: %v", err)
	}
	scope, err := wire.SignedScopeBytes(gen, hash)
	if err != nil {
		t.Fatalf("SignedScopeBytes: %v", err)
	}
	next.Generation = gen
	next.Hash = hash
	next.Signature = wire.EncodeSignature(signhash.Sign(h.feederID.SigningPriv(), scope))

	h.holder.set(next)
	return next
}

// enrolledRelay opens a relay identity store in a temp dir and enrolls it
// against the harness over the real HTTP bootstrap (claim-token + enroll +
// SPKI-pin capture) — the exact production flow.
func enrolledRelay(t *testing.T, h *harness) *identity.Store {
	t.Helper()
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := relayenroll.Run(h.ts.URL, store); err != nil {
		t.Fatalf("relayenroll.Run: %v", err)
	}
	return store
}

func dialClient(t *testing.T, h *harness, store *identity.Store, onServerFrame func(wire.Frame)) *relayclient.Client {
	t.Helper()
	client, err := relayclient.Dial(relayclient.Config{
		URL:           h.ts.URL,
		Store:         store,
		Declaration:   testDeclaration,
		OnServerFrame: onServerFrame,
	})
	if err != nil {
		t.Fatalf("relayconn.Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestPersistentConnectionEndToEnd is behaviors (i)-(iii) plus the
// conformant half of (iv) on ONE connection.
func TestPersistentConnectionEndToEnd(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)

	// The nudge handler: on state.changed, immediately pull + verify +
	// apply on a fresh goroutine-free path (the client delivers server
	// frames on a dedicated dispatcher goroutine, so Pull from here is safe).
	type pushResult struct {
		applied desiredstate.Applied
		at      time.Time
		err     error
	}
	pushCh := make(chan pushResult, 1)
	var clientRef *relayclient.Client
	var clientMu sync.Mutex

	onServer := func(f wire.Frame) {
		if f.Type != wire.FrameTypeStateChanged {
			return
		}
		clientMu.Lock()
		c := clientRef
		clientMu.Unlock()
		var body wire.StateChangedBody
		if err := f.DecodeBody(&body); err != nil {
			pushCh <- pushResult{err: err}
			return
		}
		reply, err := c.Pull("trace-nudge-1", nil)
		if err != nil {
			pushCh <- pushResult{err: err}
			return
		}
		snapBody, rawSections, err := relayclient.SnapshotFromFrame(reply)
		if err != nil {
			pushCh <- pushResult{err: err}
			return
		}
		applied, err := desiredstate.VerifyAndApply(store, snapBody, rawSections)
		pushCh <- pushResult{applied: applied, at: time.Now(), err: err}
	}

	client := dialClient(t, h, store, onServer)
	clientMu.Lock()
	clientRef = client
	clientMu.Unlock()

	relayID := client.RelayID()
	if !strings.HasPrefix(relayID, "relay-") {
		t.Fatalf("enrolled relay_id = %q, want relay-… (feeder grammar)", relayID)
	}

	// (i) Handshake completed on ONE connection (Dial fails otherwise).
	// hello-ack negotiated 1.0 (app peer implements 1.0/1.1, relay declared
	// 1.0) and the feature subset excludes the app-peer-unknown flag.
	ack := client.HelloAck()
	if ack.NegotiatedVersion != "1.0" {
		t.Fatalf("negotiated_version = %q, want 1.0", ack.NegotiatedVersion)
	}
	if len(ack.Features) != 1 || ack.Features[0] != "telemetry.latest_only_v1" {
		t.Fatalf("hello-ack features = %v, want the shared subset only", ack.Features)
	}
	if ack.SiteBinding != testSite {
		t.Fatalf("hello-ack site_binding = %+v, want the app peer's authoritative %+v", ack.SiteBinding, testSite)
	}

	// (ii) state.pull → state.snapshot; correlation id + trace_id shared;
	// verify chain accepts and persists.
	reply, err := client.Pull("trace-op-1", nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if reply.Type != wire.FrameTypeStateSnapshot {
		t.Fatalf("pull reply type = %q, want state.snapshot", reply.Type)
	}
	if reply.TraceID != "trace-op-1" {
		t.Fatalf("snapshot trace_id = %q, want the pull's own trace-op-1 (REL-006)", reply.TraceID)
	}
	sent := client.SentFrames()
	pullReq := sent[len(sent)-1]
	if pullReq.Type != wire.FrameTypeStatePull || pullReq.ID != reply.ID {
		t.Fatalf("correlation broken: request %q id %q vs reply id %q (REL-006)", pullReq.Type, pullReq.ID, reply.ID)
	}

	snapBody, rawSections, err := relayclient.SnapshotFromFrame(reply)
	if err != nil {
		t.Fatalf("SnapshotFromFrame: %v", err)
	}
	applied, err := desiredstate.VerifyAndApply(store, snapBody, rawSections)
	if err != nil {
		t.Fatalf("VerifyAndApply rejected a genuine feeder snapshot: %v", err)
	}
	if applied.Generation != 1 {
		t.Fatalf("applied generation = %d, want 1", applied.Generation)
	}
	if gen, _, ok, err := store.LastAppliedGeneration(); err != nil || !ok || gen != 1 {
		t.Fatalf("persisted last-applied = (%d,%v,%v), want (1,true,nil)", gen, ok, err)
	}

	// REL-005: every post-enrollment frame received so far — challenge,
	// hello-ack, state.snapshot — carries relay_id.
	for _, f := range client.ReceivedFrames() {
		if f.RelayID != relayID {
			t.Fatalf("received %s frame carries relay_id %q, want %q (REL-005)", f.Type, f.RelayID, relayID)
		}
	}

	// (iii) Same-generation pull → state.unchanged, byte-asserted to carry
	// generation only — no sections, no hash.
	since := applied.Generation
	reply2, err := client.Pull("trace-op-2", &since)
	if err != nil {
		t.Fatalf("Pull(since=%d): %v", since, err)
	}
	if reply2.Type != wire.FrameTypeStateUnchanged {
		t.Fatalf("same-generation pull reply = %q, want state.unchanged (REL-051)", reply2.Type)
	}
	if reply2.TraceID != "trace-op-2" || reply2.RelayID != relayID {
		t.Fatalf("state.unchanged envelope = trace %q relay %q, want trace-op-2/%s", reply2.TraceID, reply2.RelayID, relayID)
	}
	var unchangedKeys map[string]json.RawMessage
	if err := json.Unmarshal(reply2.Body, &unchangedKeys); err != nil {
		t.Fatalf("decode state.unchanged body: %v", err)
	}
	if _, hasSections := unchangedKeys["sections"]; hasSections {
		t.Fatalf("state.unchanged body carries sections: %s", reply2.Body)
	}
	if len(unchangedKeys) != 1 {
		t.Fatalf("state.unchanged body keys = %v, want exactly {generation}", unchangedKeys)
	}
	var unchanged wire.StateUnchangedBody
	if err := reply2.DecodeBody(&unchanged); err != nil {
		t.Fatalf("decode state.unchanged: %v", err)
	}
	if unchanged.Generation != 1 {
		t.Fatalf("state.unchanged generation = %d, want 1", unchanged.Generation)
	}

	// (iv, conformant half) generation advances server-side; the app peer
	// nudges over the SAME connection; the relay's own pull applies the new
	// generation well under the legacy 3s poll interval.
	h.advanceGeneration(t, 2)
	notified := time.Now()
	h.connSrv.NotifyGenerationAdvance()

	select {
	case res := <-pushCh:
		if res.err != nil {
			t.Fatalf("nudge-triggered pull/apply failed: %v", res.err)
		}
		if res.applied.Generation != 2 {
			t.Fatalf("nudge applied generation %d, want 2", res.applied.Generation)
		}
		if elapsed := res.at.Sub(notified); elapsed >= 3*time.Second {
			t.Fatalf("nudge→applied took %v — not faster than the 3s poll it replaces", elapsed)
		} else {
			t.Logf("nudge→verified+persisted latency: %v (legacy poll worst case: 3s)", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no state.changed nudge arrived within 5s of NotifyGenerationAdvance")
	}
	if gen, _, ok, err := store.LastAppliedGeneration(); err != nil || !ok || gen != 2 {
		t.Fatalf("after nudge, persisted last-applied = (%d,%v,%v), want (2,true,nil)", gen, ok, err)
	}
}

// TestDirectSnapshotPushZeroClientFrames is the strict half of (iv): with
// the REL-050-VIOLATING direct-push option, a new generation arrives with
// ZERO frames sent by the relay between snapshot N and snapshot N+1 —
// asserted from the client's complete sent-frame log.
func TestDirectSnapshotPushZeroClientFrames(t *testing.T) {
	h := newHarness(t, feederrelayconn.WithDirectSnapshotPush())
	store := enrolledRelay(t, h)

	pushed := make(chan wire.Frame, 1)
	client := dialClient(t, h, store, func(f wire.Frame) {
		if f.Type == wire.FrameTypeStateSnapshot {
			pushed <- f
		}
	})

	// Snapshot N via an ordinary pull.
	reply, err := client.Pull("trace-direct-1", nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	body, raw, err := relayclient.SnapshotFromFrame(reply)
	if err != nil {
		t.Fatalf("SnapshotFromFrame: %v", err)
	}
	if _, err := desiredstate.VerifyAndApply(store, body, raw); err != nil {
		t.Fatalf("VerifyAndApply(gen 1): %v", err)
	}
	sentBefore := len(client.SentFrames())

	// Snapshot N+1 arrives with no solicitation whatsoever.
	h.advanceGeneration(t, 2)
	h.connSrv.NotifyGenerationAdvance()

	var pushedFrame wire.Frame
	select {
	case pushedFrame = <-pushed:
	case <-time.After(5 * time.Second):
		t.Fatal("no pushed state.snapshot within 5s")
	}

	if sentAfter := len(client.SentFrames()); sentAfter != sentBefore {
		t.Fatalf("relay sent %d frame(s) between snapshot N and N+1, want 0: %+v",
			sentAfter-sentBefore, client.SentFrames()[sentBefore:])
	}

	pushedBody, pushedRaw, err := relayclient.SnapshotFromFrame(pushedFrame)
	if err != nil {
		t.Fatalf("SnapshotFromFrame(pushed): %v", err)
	}
	applied, err := desiredstate.VerifyAndApply(store, pushedBody, pushedRaw)
	if err != nil {
		t.Fatalf("VerifyAndApply(pushed gen 2): %v", err)
	}
	if applied.Generation != 2 {
		t.Fatalf("pushed generation = %d, want 2", applied.Generation)
	}
}

// rawWS opens a bare authenticated WS connection (mTLS client cert from the
// enrolled store, correct subprotocol) WITHOUT the client's handshake logic
// — the test acting as a misbehaving relay for the refusal cases.
func rawWS(t *testing.T, h *harness, store *identity.Store) *websocket.Conn {
	t.Helper()
	id, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("store.Identity: ok=%v err=%v", ok, err)
	}
	ws, err := rawDial(t, h, store, id.CertPEM, []string{wire.Subprotocol})
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws
}

// rawDial dials the harness's /relay/v1 with the store's client certificate
// and the given subprotocol offer, returning the transport error verbatim.
func rawDial(t *testing.T, h *harness, store *identity.Store, certPEM []byte, subprotocols []string) (*websocket.Conn, error) {
	t.Helper()
	id, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("store.Identity: ok=%v err=%v", ok, err)
	}
	block, _ := pemDecode(certPEM)

	wsURL := "wss" + strings.TrimPrefix(h.ts.URL, "https") + "/relay/v1"
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test dials its own httptest listener
		MinVersion:         tls.VersionTLS13,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{block},
			PrivateKey:  id.PrivateKey,
		}},
	}}}
	ws, _, err := websocket.Dial(t.Context(), wsURL, &websocket.DialOptions{
		HTTPClient:   hc,
		Subprotocols: subprotocols,
	})
	return ws, err
}

// wsSend / wsRecv exchange one wire.Frame on a raw test connection.
func wsSend(t *testing.T, ws *websocket.Conn, f wire.Frame) error {
	t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	return ws.Write(ctx, websocket.MessageText, data)
}

func wsRecv(t *testing.T, ws *websocket.Conn, f *wire.Frame) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, data, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, f)
}

// pemDecode returns the first PEM block's DER bytes.
func pemDecode(certPEM []byte) (der []byte, rest []byte) {
	block, rest := pem.Decode(certPEM)
	if block == nil {
		return nil, rest
	}
	return block.Bytes, rest
}

// newWrongKey mints a fresh ed25519 private key that is NOT any enrolled
// relay's enrollment key.
func newWrongKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return priv
}

// TestHelloWrongKeyDrawsTypedRefusal is (v-a): a hello whose channel
// binding is signed with the WRONG key draws {type:"error", id,
// code:CHANNEL_BINDING_INVALID} and the connection closes.
func TestHelloWrongKeyDrawsTypedRefusal(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	id, _, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}

	ws := rawWS(t, h, store)

	var challenge wire.Frame
	if err := wsRecv(t, ws, &challenge); err != nil {
		t.Fatalf("read challenge: %v", err)
	}
	if challenge.Type != wire.FrameTypeChallenge || challenge.RelayID != id.RelayID {
		t.Fatalf("challenge = %+v, want type=challenge relay_id=%s", challenge, id.RelayID)
	}
	var cb hello.ChallengeBody
	if err := challenge.DecodeBody(&cb); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}

	// Sign with a fresh key that is NOT the enrollment key.
	wrongPriv := newWrongKey(t)
	sig, err := hello.SignChannelBinding(wrongPriv, cb.Nonce)
	if err != nil {
		t.Fatalf("SignChannelBinding: %v", err)
	}
	helloFrame, err := wire.NewFrame(wire.FrameTypeHello, "bad-hello-1", id.RelayID, hello.HelloBody{
		ProtocolVersion:         "1.0",
		Features:                []string{},
		ClockState:              hello.ClockState{State: "untrusted", Source: "none"},
		ChannelBindingSignature: sig,
	})
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if err := wsSend(t, ws, helloFrame); err != nil {
		t.Fatalf("send hello: %v", err)
	}

	var refusal wire.Frame
	if err := wsRecv(t, ws, &refusal); err != nil {
		t.Fatalf("read refusal: %v", err)
	}
	if refusal.Type != wire.FrameTypeError || refusal.Code != "CHANNEL_BINDING_INVALID" {
		t.Fatalf("refusal = %+v, want type=error code=CHANNEL_BINDING_INVALID", refusal)
	}
	if refusal.ID != "bad-hello-1" {
		t.Fatalf("refusal id = %q, want the hello's own id (REL-007)", refusal.ID)
	}

	var next wire.Frame
	if err := wsRecv(t, ws, &next); err == nil {
		t.Fatalf("connection stayed open after refusal; received %+v", next)
	}
}

// TestPullBeforeHelloIsProtocolViolation is (v-b): any pre-hello frame
// draws PROTOCOL_VIOLATION and the connection closes (REL-039 posture).
func TestPullBeforeHelloIsProtocolViolation(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	id, _, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}

	ws := rawWS(t, h, store)

	var challenge wire.Frame
	if err := wsRecv(t, ws, &challenge); err != nil {
		t.Fatalf("read challenge: %v", err)
	}

	pull, err := wire.NewFrame(wire.FrameTypeStatePull, "eager-pull-1", id.RelayID, wire.StatePullBody{})
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if err := wsSend(t, ws, pull); err != nil {
		t.Fatalf("send pull: %v", err)
	}

	var refusal wire.Frame
	if err := wsRecv(t, ws, &refusal); err != nil {
		t.Fatalf("read refusal: %v", err)
	}
	if refusal.Type != wire.FrameTypeError || refusal.Code != "PROTOCOL_VIOLATION" || refusal.ID != "eager-pull-1" {
		t.Fatalf("refusal = %+v, want type=error code=PROTOCOL_VIOLATION id=eager-pull-1", refusal)
	}

	var next wire.Frame
	if err := wsRecv(t, ws, &next); err == nil {
		t.Fatalf("connection stayed open after protocol violation; received %+v", next)
	}
}

// TestReconnectAfterAppPeerRestart is the reconnect measurement: the spike
// client does NOT auto-reconnect (Done closes, in-flight pulls fail); a
// fresh Dial against the restarted app peer — same enrollment registry,
// same serving leaf, new listener — succeeds and pulling resumes.
func TestReconnectAfterAppPeerRestart(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	client := dialClient(t, h, store, nil)

	if _, err := client.Pull("trace-pre-restart", nil); err != nil {
		t.Fatalf("pre-restart Pull: %v", err)
	}

	// "Restart" the app peer: kill the listener, start a new one over the
	// SAME mux/servers (httptest reuses one fixed leaf, so the enrollment-
	// captured SPKI pin still matches — exactly a feeder that persists its
	// serving identity, as signing.LoadOrCreate does).
	//
	// SPIKE FINDING: neither Close() nor CloseClientConnections() tears
	// down a hijacked WS connection (httptest drops hijacked conns from its
	// registry, exactly as net/http.Server.Shutdown does in production) —
	// the app peer MUST hold its own live-conn registry to die cleanly.
	// Server.CloseAll is that registry; together with Close() this is
	// process death.
	h.connSrv.CloseAll()
	h.ts.Close()

	select {
	case <-client.Done():
		// the client observed the drop
	case <-time.After(5 * time.Second):
		t.Fatal("client never observed the app-peer restart")
	}
	if _, err := client.Pull("trace-during-down", nil); err == nil {
		t.Fatal("Pull on a dead connection succeeded; want an error")
	}

	h.ts = h.listen(t)

	client2 := dialClient(t, h, store, nil)
	reply, err := client2.Pull("trace-post-restart", nil)
	if err != nil {
		t.Fatalf("post-restart Pull: %v", err)
	}
	body, raw, err := relayclient.SnapshotFromFrame(reply)
	if err != nil {
		t.Fatalf("SnapshotFromFrame: %v", err)
	}
	if _, err := desiredstate.VerifyAndApply(store, body, raw); err != nil {
		t.Fatalf("post-restart VerifyAndApply: %v", err)
	}
}

// TestSubprotocolRequired pins the handshake gate: a client not offering
// relay.v1+json is refused at the WS upgrade, before any frame.
func TestSubprotocolRequired(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	id, _, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}

	// No subprotocol offered.
	if ws, err := rawDial(t, h, store, id.CertPEM, nil); err == nil {
		ws.CloseNow()
		t.Fatal("upgrade without the relay.v1+json subprotocol succeeded; want refusal")
	}
}
