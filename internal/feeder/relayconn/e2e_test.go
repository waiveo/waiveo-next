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
//	      body carries the generation and NO sections key (byte-asserted);
//	      since_generation GREATER than current → the FULL state.snapshot,
//	      never a lying unchanged (REL-051's ahead-of-current clause).
//	(iv)  server-initiated push over the SAME connection: the REL-057
//	      state.changed nudge (relay reacts with an immediate pull, applied
//	      well under the legacy 3s ticker).
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
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
// generation-cached desiredStateSource.current. setStall installs a channel
// every subsequent get blocks on — simulating an app peer wedged off its
// read loop, for the heartbeat dead-peer tests.
type snapshotHolder struct {
	mu    sync.Mutex
	snap  wire.StateSnapshotBody
	stall chan struct{}
}

func (h *snapshotHolder) get() (wire.StateSnapshotBody, error) {
	h.mu.Lock()
	stall := h.stall
	snap := h.snap
	h.mu.Unlock()
	if stall != nil {
		<-stall
	}
	return snap, nil
}

func (h *snapshotHolder) set(s wire.StateSnapshotBody) {
	h.mu.Lock()
	h.snap = s
	h.mu.Unlock()
}

func (h *snapshotHolder) setStall(stall chan struct{}) {
	h.mu.Lock()
	h.stall = stall
	h.mu.Unlock()
}

// harness is one fully-wired app peer: real enrollment server + WS server
// on one mTLS httptest listener. lookupOverride, when set, replaces the
// enrollment registry's key lookup (the unknown-identity refusal test).
type harness struct {
	enrollSrv *feederenroll.Server
	connSrv   *feederrelayconn.Server
	holder    *snapshotHolder
	feederID  *signing.Identity
	mux       *http.ServeMux
	ts        *httptest.Server

	lookupMu       sync.Mutex
	lookupOverride hello.RelayKeyLookup
}

func (h *harness) lookup(relayID string) (ed25519.PublicKey, bool) {
	h.lookupMu.Lock()
	override := h.lookupOverride
	h.lookupMu.Unlock()
	if override != nil {
		return override(relayID)
	}
	return h.enrollSrv.RelayEnrollmentKey(relayID)
}

func (h *harness) setLookupOverride(l hello.RelayKeyLookup) {
	h.lookupMu.Lock()
	h.lookupOverride = l
	h.lookupMu.Unlock()
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

	enrollSrv, err := feederenroll.NewServer(feederID)
	if err != nil {
		t.Fatalf("feederenroll.NewServer: %v", err)
	}

	holder := &snapshotHolder{snap: snap1}
	h := &harness{
		enrollSrv: enrollSrv, holder: holder, feederID: feederID,
	}
	h.connSrv = feederrelayconn.New(
		holder.get,
		h.lookup,
		enrollSrv.IsRevoked,
		testSite,
		hello.AppPeerImplementedMinors(1, 1),
		[]string{"telemetry.latest_only_v1"},
		opts...,
	)

	mux := http.NewServeMux()
	enrollSrv.Register(mux)
	mux.Handle("/relay/v1", h.connSrv.Handler())
	h.mux = mux
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
	return h.advanceGenerationRevision(t, gen, "rev-e2e-gen-"+strconv.FormatInt(gen, 10))
}

// advanceGenerationRevision is advanceGeneration with a caller-chosen
// program revision — the slow-consumer test uses a large one so each
// snapshot reply is big enough to fill socket buffers deterministically.
func (h *harness) advanceGenerationRevision(t *testing.T, gen int64, revision string) wire.StateSnapshotBody {
	t.Helper()
	h.holder.mu.Lock()
	next := h.holder.snap
	h.holder.mu.Unlock()

	programs := make([]wire.ScreenProgram, len(next.Sections.ScreenPrograms))
	copy(programs, next.Sections.ScreenPrograms)
	if len(programs) == 0 {
		t.Fatal("harness snapshot carries no screen_programs")
	}
	programs[0].ProgramRevision = revision
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

// frameLog is the test's own record of the wire exchange, fed by the
// client's test-only ObserveFrame hook — the production client retains no
// frame history (the spike's unbounded SentFrames/ReceivedFrames
// instrumentation is gone).
type frameLog struct {
	mu       sync.Mutex
	sent     []wire.Frame
	received []wire.Frame
}

func (l *frameLog) observe(sent bool, f wire.Frame) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if sent {
		l.sent = append(l.sent, f)
	} else {
		l.received = append(l.received, f)
	}
}

func (l *frameLog) Sent() []wire.Frame     { return l.snapshot(&l.sent) }
func (l *frameLog) Received() []wire.Frame { return l.snapshot(&l.received) }

func (l *frameLog) snapshot(log *[]wire.Frame) []wire.Frame {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]wire.Frame, len(*log))
	copy(out, *log)
	return out
}

func dialClient(t *testing.T, h *harness, store *identity.Store, onGenerationAdvance func(int64)) (*relayclient.Client, *frameLog) {
	t.Helper()
	log := &frameLog{}
	client, err := relayclient.Dial(relayclient.Config{
		URL:                 h.ts.URL,
		Store:               store,
		Declaration:         testDeclaration,
		OnGenerationAdvance: onGenerationAdvance,
		ObserveFrame:        log.observe,
	})
	if err != nil {
		t.Fatalf("relayconn.Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, log
}

// TestPersistentConnectionEndToEnd is behaviors (i)-(iii) plus the
// conformant half of (iv) on ONE connection.
func TestPersistentConnectionEndToEnd(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)

	// The nudge handler: on a state.changed generation advance, immediately
	// pull + verify + apply (the client delivers nudges on a dedicated
	// dispatcher goroutine, so Pull from here is safe).
	type pushResult struct {
		applied desiredstate.Applied
		at      time.Time
		err     error
	}
	pushCh := make(chan pushResult, 1)
	var clientRef *relayclient.Client
	var clientMu sync.Mutex

	onGeneration := func(int64) {
		clientMu.Lock()
		c := clientRef
		clientMu.Unlock()
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
		if err == nil {
			// REL-054: acknowledge the applied snapshot, correlated to the
			// pull exchange that delivered it.
			err = c.SendStateAck(reply.ID, reply.TraceID,
				wire.StateAckBody{AppliedGeneration: applied.Generation})
		}
		pushCh <- pushResult{applied: applied, at: time.Now(), err: err}
	}

	client, frames := dialClient(t, h, store, onGeneration)
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
	sent := frames.Sent()
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

	// REL-054: the relay acknowledges the applied snapshot with state.ack
	// carrying the PULL exchange's correlation id; the app peer records it.
	if err := client.SendStateAck(reply.ID, reply.TraceID,
		wire.StateAckBody{AppliedGeneration: applied.Generation}); err != nil {
		t.Fatalf("SendStateAck: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		f, ok := h.connSrv.LastStateAck(relayID)
		return ok && f.ID == reply.ID
	}, "state.ack never arrived at the app peer with the pull's correlation id (REL-054)")
	ackFrame, _ := h.connSrv.LastStateAck(relayID)
	if ackFrame.TraceID != "trace-op-1" || ackFrame.RelayID != relayID {
		t.Fatalf("state.ack envelope = trace %q relay %q, want trace-op-1/%s (REL-005/006)", ackFrame.TraceID, ackFrame.RelayID, relayID)
	}
	var ackKeys map[string]json.RawMessage
	if err := json.Unmarshal(ackFrame.Body, &ackKeys); err != nil {
		t.Fatalf("decode state.ack body: %v", err)
	}
	if _, hasErr := ackKeys["error"]; hasErr {
		t.Fatalf("successful state.ack carries error: %s (REL-054: present only on failure)", ackFrame.Body)
	}
	if _, hasDiv := ackKeys["divergence_reason"]; hasDiv {
		t.Fatalf("successful state.ack carries divergence_reason: %s (REL-054)", ackFrame.Body)
	}
	var ackBody wire.StateAckBody
	if err := ackFrame.DecodeBody(&ackBody); err != nil {
		t.Fatalf("decode state.ack: %v", err)
	}
	if ackBody.AppliedGeneration != 1 {
		t.Fatalf("state.ack applied_generation = %d, want 1", ackBody.AppliedGeneration)
	}

	// REL-005: every post-enrollment frame received so far — challenge,
	// hello-ack, state.snapshot — carries relay_id.
	for _, f := range frames.Received() {
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

	// REL-051's ahead-of-current clause: since_generation naming a
	// generation GREATER than the app peer's current one draws the FULL
	// state.snapshot — never a state.unchanged asserting agreement that
	// does not exist. (Current generation here is 1; claim 6.) This is the
	// executing pin on the >-vs-== distinction at handleStatePull's guard:
	// only exact equality may answer unchanged.
	ahead := applied.Generation + 5
	replyAhead, err := client.Pull("trace-op-ahead", &ahead)
	if err != nil {
		t.Fatalf("Pull(since=%d, ahead of current): %v", ahead, err)
	}
	if replyAhead.Type != wire.FrameTypeStateSnapshot {
		t.Fatalf("ahead-of-current pull reply = %q, want state.snapshot (REL-051: a since_generation GREATER than current MUST draw the full snapshot)", replyAhead.Type)
	}
	var aheadKeys map[string]json.RawMessage
	if err := json.Unmarshal(replyAhead.Body, &aheadKeys); err != nil {
		t.Fatalf("decode ahead-of-current snapshot body: %v", err)
	}
	if _, hasSections := aheadKeys["sections"]; !hasSections {
		t.Fatalf("ahead-of-current snapshot carries no sections key — not the FULL snapshot REL-051 requires: %s", replyAhead.Body)
	}
	aheadBody, _, err := relayclient.SnapshotFromFrame(replyAhead)
	if err != nil {
		t.Fatalf("SnapshotFromFrame(ahead-of-current): %v", err)
	}
	if aheadBody.Generation != applied.Generation {
		t.Fatalf("ahead-of-current snapshot generation = %d, want the app peer's own current %d (the app peer stays stateless about relay history)", aheadBody.Generation, applied.Generation)
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
	// REL-054 again for the nudge-applied generation: the wire ack advanced.
	waitFor(t, 5*time.Second, func() bool {
		f, ok := h.connSrv.LastStateAck(relayID)
		if !ok {
			return false
		}
		var b wire.StateAckBody
		return f.DecodeBody(&b) == nil && b.AppliedGeneration == 2
	}, "no state.ack for the nudge-applied generation 2 (REL-054)")
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

// TestProtocolVersionUnsupportedDrawsTypedRefusal pins REL-033 on the wire:
// a relay declaring a major the app peer implements no minor of is refused
// PROTOCOL_VERSION_UNSUPPORTED in place of hello-ack — the channel binding is
// checked first (the declaration is otherwise genuine), so this exercises the
// negotiation refusal itself, the coverage the HTTP-era handshake tests
// carried before that transport was deleted.
func TestProtocolVersionUnsupportedDrawsTypedRefusal(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)

	decl := testDeclaration
	decl.ProtocolVersion = "2.0" // the app peer implements only major 1
	_, err := relayclient.Dial(relayclient.Config{
		URL: h.ts.URL, Store: store, Declaration: decl,
	})
	var refusal *relayclient.Refusal
	if !errors.As(err, &refusal) || refusal.Code != "PROTOCOL_VERSION_UNSUPPORTED" {
		t.Fatalf("Dial declaring major 2 = %v, want a PROTOCOL_VERSION_UNSUPPORTED refusal (REL-033)", err)
	}
}

// TestReconnectAfterAppPeerRestart is the reconnect measurement: the spike
// client does NOT auto-reconnect (Done closes, in-flight pulls fail); a
// fresh Dial against the restarted app peer — same enrollment registry,
// same serving leaf, new listener — succeeds and pulling resumes.
func TestReconnectAfterAppPeerRestart(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	client, _ := dialClient(t, h, store, nil)

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

	client2, _ := dialClient(t, h, store, nil)
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

// rawHandshake completes a VALID challenge → hello → hello-ack on a raw
// test connection, signing the challenge nonce with the store's enrollment
// key — the test acting as a well-behaved relay up to steady state, without
// the production client's read loop.
func rawHandshake(t *testing.T, ws *websocket.Conn, store *identity.Store) {
	t.Helper()
	id, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("store.Identity: ok=%v err=%v", ok, err)
	}

	var challenge wire.Frame
	if err := wsRecv(t, ws, &challenge); err != nil {
		t.Fatalf("read challenge: %v", err)
	}
	var cb hello.ChallengeBody
	if err := challenge.DecodeBody(&cb); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	sig, err := hello.SignChannelBinding(id.PrivateKey, cb.Nonce)
	if err != nil {
		t.Fatalf("SignChannelBinding: %v", err)
	}
	helloFrame, err := wire.NewFrame(wire.FrameTypeHello, "raw-hello-1", id.RelayID, hello.HelloBody{
		ProtocolVersion:         testDeclaration.ProtocolVersion,
		Features:                testDeclaration.Features,
		SubnetMetadata:          testDeclaration.SubnetMetadata,
		ClockState:              testDeclaration.ClockState,
		ChannelBindingSignature: sig,
	})
	if err != nil {
		t.Fatalf("NewFrame(hello): %v", err)
	}
	if err := wsSend(t, ws, helloFrame); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	var ack wire.Frame
	if err := wsRecv(t, ws, &ack); err != nil {
		t.Fatalf("read hello-ack: %v", err)
	}
	if ack.Type != wire.FrameTypeHelloAck {
		t.Fatalf("handshake reply = %+v, want hello-ack", ack)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestSlowConsumerIsDisconnected is the slow-consumer policy's proof: a
// relay that authenticates and then stops draining its socket (a stalled
// reader) is torn down by the per-write deadline — the feeder's conns set
// drops it rather than wedging behind its buffer. Heartbeats are pushed out
// of the test window so the WRITE deadline is provably the reaper.
func TestSlowConsumerIsDisconnected(t *testing.T) {
	h := newHarness(t,
		feederrelayconn.WithWriteTimeout(200*time.Millisecond),
		feederrelayconn.WithHeartbeat(time.Hour, time.Second),
	)
	store := enrolledRelay(t, h)

	// Stage a large current snapshot (~256 KiB) so a handful of pull
	// replies deterministically outgrow the socket buffers between the
	// peers.
	h.advanceGenerationRevision(t, 2, strings.Repeat("x", 256<<10))

	ws := rawWS(t, h, store)
	rawHandshake(t, ws, store)
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() == 1 },
		"authenticated connection never registered")

	// The stall: send pulls, never read a reply. Each reply is ~256 KiB;
	// once the buffers fill, the server's next reply write cannot complete
	// within its 200ms bound and the connection is torn down.
	id, _, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	for i := 0; i < 40; i++ {
		pull, err := wire.NewFrame(wire.FrameTypeStatePull, fmt.Sprintf("stall-pull-%d", i), id.RelayID, wire.StatePullBody{})
		if err != nil {
			t.Fatalf("NewFrame: %v", err)
		}
		if wsSend(t, ws, pull) != nil {
			break // the server already tore the connection down mid-loop
		}
	}

	waitFor(t, 15*time.Second, func() bool { return h.connSrv.ConnCount() == 0 },
		"stalled consumer was never disconnected (write deadline did not reap it)")
}

// TestClientHeartbeatReapsUnresponsiveServer is the relay-side dead-peer
// proof: when the app peer stops servicing the connection entirely (wedged
// off its read loop — here, a snapshot provider that never returns), the
// client's ping round-trip fails and the client tears the connection down,
// surfacing the death via Done instead of hanging forever on a half-open
// socket.
func TestClientHeartbeatReapsUnresponsiveServer(t *testing.T) {
	h := newHarness(t, feederrelayconn.WithHeartbeat(time.Hour, time.Second))
	store := enrolledRelay(t, h)

	client, err := relayclient.Dial(relayclient.Config{
		URL:          h.ts.URL,
		Store:        store,
		Declaration:  testDeclaration,
		PingInterval: 100 * time.Millisecond,
		PingTimeout:  300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("relayconn.Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// Healthy first: a pull round-trips while the server services reads.
	if _, err := client.Pull("trace-healthy", nil); err != nil {
		t.Fatalf("healthy Pull: %v", err)
	}

	// Wedge the server: its next provider call blocks forever, so its read
	// loop stops servicing the connection — pings included.
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	h.holder.setStall(stall)
	go func() { _, _ = client.Pull("trace-wedged", nil) }() // parks the server in the provider

	select {
	case <-client.Done():
		// the heartbeat detected the dead peer and tore the connection down
	case <-time.After(10 * time.Second):
		t.Fatal("client never detected the unresponsive app peer (heartbeat did not reap)")
	}
}

// TestSupervisorThrottlesShortLivedConnections is the reconnect-storm
// regression proof: against a peer that ACCEPTS the handshake and then loses
// every connection immediately (staged here from the owner side — OnConnected
// closes the fresh client at once, so every connection dies well under
// MinStableUptime), the supervisor's backoff ladder MUST engage exactly as it
// does for refused dials. Before the min-uptime guard, this loop re-dialed at
// full rate (hundreds of TLS handshakes + channel-binding signatures per
// second — measured ~265 dials/s); throttled, a 1.5s window fits only the
// ladder's worth of attempts.
func TestSupervisorThrottlesShortLivedConnections(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)

	var dials atomic.Int64
	sup := relayclient.StartSupervisor(relayclient.SupervisorConfig{
		Connect: func() (*relayclient.Client, error) {
			dials.Add(1)
			return relayclient.Dial(relayclient.Config{
				URL: h.ts.URL, Store: store, Declaration: testDeclaration,
			})
		},
		// The accept-then-drop peer stand-in: the connection authenticates
		// fully, then dies instantly.
		OnConnected:    func(c *relayclient.Client) { _ = c.Close() },
		InitialBackoff: 50 * time.Millisecond,
		MaxBackoff:     400 * time.Millisecond,
		// MinStableUptime defaults to MaxBackoff — every one of these
		// instant deaths counts as a failed attempt.
	})
	defer func() {
		sup.Stop()
		select {
		case <-sup.Done():
		case <-time.After(5 * time.Second):
			t.Error("supervisor never stopped")
		}
	}()

	time.Sleep(1500 * time.Millisecond)
	n := dials.Load()
	// Ladder floor (all-minimum jitter): waits of 25, 50, 100, 200, 200…ms
	// between attempts allow at most ~11 dials in 1.5s even before dial
	// latency; an unthrottled loop measured hundreds.
	if n > 15 {
		t.Fatalf("%d dials in 1.5s against an accept-then-drop peer — the backoff ladder is not engaging (reconnect storm)", n)
	}
	if n < 2 {
		t.Fatalf("dials = %d — the supervisor stopped re-dialing entirely", n)
	}
}

// TestSupervisorStableConnectionRedialsImmediately proves the other half of
// the min-uptime guard (and that the guard is what throttles the storm test
// above): a connection whose uptime exceeds MinStableUptime is an isolated
// drop — its death re-dials immediately and resets the ladder, so a long
// window fits MANY more attempts than the throttled ladder ever would.
func TestSupervisorStableConnectionRedialsImmediately(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)

	var dials atomic.Int64
	sup := relayclient.StartSupervisor(relayclient.SupervisorConfig{
		Connect: func() (*relayclient.Client, error) {
			dials.Add(1)
			return relayclient.Dial(relayclient.Config{
				URL: h.ts.URL, Store: store, Declaration: testDeclaration,
			})
		},
		OnConnected: func(c *relayclient.Client) {
			time.Sleep(10 * time.Millisecond) // outlive MinStableUptime
			_ = c.Close()
		},
		// Backoff rungs of 2s: if these deaths were (wrongly) ladder-paced,
		// every re-dial would wait >= 1s (the jitter floor); the immediate
		// path is bounded by handshake latency only. The two behaviors are
		// orders of magnitude apart, so the assertion below cannot pass by
		// jitter luck.
		InitialBackoff:  2 * time.Second,
		MaxBackoff:      2 * time.Second,
		MinStableUptime: time.Millisecond,
	})
	defer func() {
		sup.Stop()
		select {
		case <-sup.Done():
		case <-time.After(5 * time.Second):
			t.Error("supervisor never stopped")
		}
	}()

	start := time.Now()
	deadline := start.Add(3 * time.Second)
	for dials.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// 5 attempts under the 2s ladder would need >= 4 waits x 1s = 4s of
	// pure waiting; the immediate path needs only 5 handshakes + 4x10ms.
	if n := dials.Load(); n < 5 {
		t.Fatalf("only %d dials in 3s — stable connections are being back-off-paced instead of re-dialed immediately", n)
	}
}

// certSerial reads the enrolled identity's relay_id and presented
// certificate serial, rendered exactly as the issuance record keys it
// (big.Int.Text(16) — enroll.issueRelayCert's own form).
func certSerial(t *testing.T, store *identity.Store) (relayID, serial string) {
	t.Helper()
	id, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("store.Identity: ok=%v err=%v", ok, err)
	}
	der, _ := pemDecode(id.CertPEM)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse enrolled certificate: %v", err)
	}
	return id.RelayID, cert.SerialNumber.Text(16)
}

// TestRevokedCertRefusedAtConnect is REL-016's connection-time half: a
// revoked certificate is refused with typed CERT_REVOKED at the very next
// connection attempt, even though it remains within its validity window.
func TestRevokedCertRefusedAtConnect(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	relayID, serial := certSerial(t, store)
	if !h.enrollSrv.Revoke(relayID, serial) {
		t.Fatalf("Revoke(%s, %s) found no issuance on record", relayID, serial)
	}

	_, err := relayclient.Dial(relayclient.Config{
		URL: h.ts.URL, Store: store, Declaration: testDeclaration,
	})
	var refusal *relayclient.Refusal
	if !errors.As(err, &refusal) || refusal.Code != "CERT_REVOKED" {
		t.Fatalf("Dial with a revoked certificate = %v, want a CERT_REVOKED refusal (REL-016)", err)
	}
}

// TestRevocationLandsMidSession is REL-016's mid-session half: a
// revocation recorded while the connection is live takes effect on the
// very next frame — the frame draws typed CERT_REVOKED and the connection
// closes; the relay does not get to ride out its session.
func TestRevocationLandsMidSession(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	client, _ := dialClient(t, h, store, nil)

	if _, err := client.Pull("trace-pre-revoke", nil); err != nil {
		t.Fatalf("pre-revocation Pull: %v", err)
	}

	relayID, serial := certSerial(t, store)
	if !h.enrollSrv.Revoke(relayID, serial) {
		t.Fatalf("Revoke(%s, %s) found no issuance on record", relayID, serial)
	}

	_, err := client.Pull("trace-post-revoke", nil)
	var refusal *relayclient.Refusal
	if !errors.As(err, &refusal) || refusal.Code != "CERT_REVOKED" {
		t.Fatalf("post-revocation Pull = %v, want a CERT_REVOKED refusal (REL-016 mid-session)", err)
	}
	select {
	case <-client.Done():
		// the app peer closed the connection after the refusal
	case <-time.After(5 * time.Second):
		t.Fatal("connection stayed open after a mid-session CERT_REVOKED refusal")
	}
}

// TestRevocationReapsSilentSession is REL-016's silent-relay half: a relay
// whose certificate is revoked mid-session and which then sends NOTHING
// further (the stolen-credential holder's posture — keep the authenticated
// TLS session open, never trip the inbound-frame re-check) is proactively
// refused and disconnected on the heartbeat cadence, its conn slot
// reclaimed — it does not get to ride out the session.
func TestRevocationReapsSilentSession(t *testing.T) {
	h := newHarness(t, feederrelayconn.WithHeartbeat(50*time.Millisecond, time.Second))
	store := enrolledRelay(t, h)

	log := &frameLog{}
	client, err := relayclient.Dial(relayclient.Config{
		URL: h.ts.URL, Store: store, Declaration: testDeclaration,
		ObserveFrame: log.observe,
	})
	if err != nil {
		t.Fatalf("relayconn.Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() == 1 },
		"authenticated connection never registered")

	relayID, serial := certSerial(t, store)
	if !h.enrollSrv.Revoke(relayID, serial) {
		t.Fatalf("Revoke(%s, %s) found no issuance on record", relayID, serial)
	}

	// The relay sends nothing after the revocation. The sweep must still
	// tear the session down within a few heartbeat ticks.
	select {
	case <-client.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("silent revoked session was never torn down (REL-016 sweep did not reap it)")
	}
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() == 0 },
		"revoked connection still holds its conn slot after teardown")

	// The teardown was the typed refusal, not a bare socket close: the
	// relay received the same CERT_REVOKED error frame a fresh connection
	// attempt draws.
	var sawRefusal bool
	for _, f := range log.Received() {
		if f.Type == wire.FrameTypeError && f.Code == "CERT_REVOKED" {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Fatalf("no CERT_REVOKED error frame preceded the close; received: %+v", log.Received())
	}
}

// TestNotifyGenerationAdvanceGatesRevoked: server-initiated pushes are
// revocation-gated (REL-016) — a revoked relay never receives fresh
// generation metadata via state.changed; the push path closes its
// connection instead. The heartbeat cadence is pushed out of the test
// window so the push-path gate is provably what enforces it.
func TestNotifyGenerationAdvanceGatesRevoked(t *testing.T) {
	h := newHarness(t, feederrelayconn.WithHeartbeat(time.Hour, time.Second))
	store := enrolledRelay(t, h)

	nudges := make(chan int64, 4)
	client, _ := dialClient(t, h, store, func(gen int64) { nudges <- gen })

	// Sanity: an un-revoked connection receives the nudge.
	h.advanceGeneration(t, 2)
	h.connSrv.NotifyGenerationAdvance()
	select {
	case gen := <-nudges:
		if gen != 2 {
			t.Fatalf("nudge carried generation %d, want 2", gen)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no state.changed nudge before revocation (harness broken)")
	}

	relayID, serial := certSerial(t, store)
	if !h.enrollSrv.Revoke(relayID, serial) {
		t.Fatalf("Revoke(%s, %s) found no issuance on record", relayID, serial)
	}

	h.advanceGeneration(t, 3)
	h.connSrv.NotifyGenerationAdvance()

	// The push path must close the revoked connection...
	select {
	case <-client.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("revoked connection stayed open through NotifyGenerationAdvance (REL-016)")
	}
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() == 0 },
		"revoked connection still holds its conn slot after the gated push")

	// ...and no generation-3 nudge may have reached the revoked relay.
	select {
	case gen := <-nudges:
		t.Fatalf("revoked relay received state.changed generation %d (REL-016: pushes must be revocation-gated)", gen)
	default:
	}
}

// TestMissingClientCertDrawsMalformedMessage: a connection presenting no
// client certificate cannot satisfy REL-003's mutual authentication; it is
// refused with MALFORMED_MESSAGE — the taxonomy's REL-003 entry — and the
// pre-authentication refusal omits relay_id (REL-005: no identity was ever
// established to stamp).
func TestMissingClientCertDrawsMalformedMessage(t *testing.T) {
	h := newHarness(t)

	wsURL := "wss" + strings.TrimPrefix(h.ts.URL, "https") + "/relay/v1"
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test dials its own httptest listener
		MinVersion:         tls.VersionTLS13,
	}}}
	ws, _, err := websocket.Dial(t.Context(), wsURL, &websocket.DialOptions{
		HTTPClient:   hc,
		Subprotocols: []string{wire.Subprotocol},
	})
	if err != nil {
		t.Fatalf("certless dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })

	var refusal wire.Frame
	if err := wsRecv(t, ws, &refusal); err != nil {
		t.Fatalf("read refusal: %v", err)
	}
	if refusal.Type != wire.FrameTypeError || refusal.Code != "MALFORMED_MESSAGE" {
		t.Fatalf("refusal = %+v, want type=error code=MALFORMED_MESSAGE (the taxonomy's REL-003 row)", refusal)
	}
	if refusal.RelayID != "" {
		t.Fatalf("pre-authentication refusal carries relay_id %q, want none (REL-005's pre-auth exception)", refusal.RelayID)
	}
}

// TestUnknownIdentityRefusedAtHello: an mTLS identity with no enrollment
// record is NOT pre-refused before the handshake (that would hand an
// unauthenticated prober an enrollment oracle); the challenge is issued
// and hello's channel-binding verification fails against the absent
// enrollment key — CHANNEL_BINDING_INVALID exactly where REL-032 defines
// it.
func TestUnknownIdentityRefusedAtHello(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	h.setLookupOverride(func(string) (ed25519.PublicKey, bool) { return nil, false })

	log := &frameLog{}
	_, err := relayclient.Dial(relayclient.Config{
		URL: h.ts.URL, Store: store, Declaration: testDeclaration,
		ObserveFrame: log.observe,
	})
	var refusal *relayclient.Refusal
	if !errors.As(err, &refusal) || refusal.Code != "CHANNEL_BINDING_INVALID" {
		t.Fatalf("Dial with an unknown identity = %v, want a CHANNEL_BINDING_INVALID refusal (REL-032)", err)
	}
	received := log.Received()
	if len(received) == 0 || received[0].Type != wire.FrameTypeChallenge {
		t.Fatalf("first server frame = %+v, want challenge — an unknown identity must not be refused pre-hello (enrollment oracle)", received)
	}
}

// TestMalformedHelloBodyDrawsMalformedMessage: an undecodable hello body
// fails its type's minimum shape — MALFORMED_MESSAGE (REL-002), never the
// message-ordering PROTOCOL_VIOLATION the spike misassigned.
func TestMalformedHelloBodyDrawsMalformedMessage(t *testing.T) {
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

	badHello := wire.Frame{
		Type:    wire.FrameTypeHello,
		ID:      "bad-body-1",
		RelayID: id.RelayID,
		Body:    json.RawMessage(`42`), // not a hello body shape
	}
	if err := wsSend(t, ws, badHello); err != nil {
		t.Fatalf("send malformed hello: %v", err)
	}

	var refusal wire.Frame
	if err := wsRecv(t, ws, &refusal); err != nil {
		t.Fatalf("read refusal: %v", err)
	}
	if refusal.Type != wire.FrameTypeError || refusal.Code != "MALFORMED_MESSAGE" || refusal.ID != "bad-body-1" {
		t.Fatalf("refusal = %+v, want type=error code=MALFORMED_MESSAGE id=bad-body-1 (REL-002)", refusal)
	}
}

// TestSupervisorChaosReconnect is the reconnect supervisor's end-to-end
// chaos proof: the app peer is killed MID-FLIGHT (live connection torn
// down, listener gone), a generation is authored while the relay is
// offline, the app peer comes back (same enrollment registry, same
// serving leaf, new listener) — and the supervisor re-establishes, its
// pull-on-reconnect converges on the missed generation, and stopping it
// leaks no goroutines (before/after counts).
func TestSupervisorChaosReconnect(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)

	// The app peer's URL moves across the restart (a fresh httptest
	// listener binds a fresh port); Connect reads it under the lock the
	// test's restart also takes.
	var urlMu sync.Mutex
	currentURL := func() string {
		urlMu.Lock()
		defer urlMu.Unlock()
		return h.ts.URL
	}

	baseline := runtime.NumGoroutine()

	applied := make(chan int64, 16)
	sup := relayclient.StartSupervisor(relayclient.SupervisorConfig{
		Connect: func() (*relayclient.Client, error) {
			return relayclient.Dial(relayclient.Config{
				URL: currentURL(), Store: store, Declaration: testDeclaration,
			})
		},
		OnConnected: func(c *relayclient.Client) {
			// Pull-on-reconnect: claim the persisted last-applied
			// generation, apply whatever is newer, ack it (REL-054).
			var since *int64
			if gen, _, ok, err := store.LastAppliedGeneration(); err == nil && ok {
				since = &gen
			}
			reply, err := c.Pull("trace-supervisor", since)
			if err != nil || reply.Type != wire.FrameTypeStateSnapshot {
				return // unchanged (already converged) or a dead conn: nothing to apply
			}
			body, raw, err := relayclient.SnapshotFromFrame(reply)
			if err != nil {
				return
			}
			a, err := desiredstate.VerifyAndApply(store, body, raw)
			if err != nil {
				return
			}
			_ = c.SendStateAck(reply.ID, reply.TraceID, wire.StateAckBody{AppliedGeneration: a.Generation})
			applied <- a.Generation
		},
		InitialBackoff: 20 * time.Millisecond,
		MaxBackoff:     200 * time.Millisecond,
	})

	waitApplied := func(want int64) {
		t.Helper()
		deadline := time.After(15 * time.Second)
		for {
			select {
			case gen := <-applied:
				if gen == want {
					return
				}
			case <-deadline:
				t.Fatalf("generation %d was never applied through the supervisor", want)
			}
		}
	}

	// First connection converges on generation 1.
	waitApplied(1)

	// CHAOS: kill the app peer mid-flight — live conns torn down, listener
	// closed — and author generation 2 while the relay is down.
	h.connSrv.CloseAll()
	h.ts.Close()
	h.advanceGeneration(t, 2)
	time.Sleep(100 * time.Millisecond) // let the supervisor eat a few failed re-dials

	// Restart: same mux, same enrollment registry, same serving leaf
	// (httptest reuses one fixed leaf, so the SPKI pin still matches).
	urlMu.Lock()
	h.ts = h.listen(t)
	urlMu.Unlock()

	// The supervisor re-establishes on its own and converges on the
	// generation authored while it was offline.
	waitApplied(2)
	if gen, _, ok, err := store.LastAppliedGeneration(); err != nil || !ok || gen != 2 {
		t.Fatalf("after chaos reconnect, persisted last-applied = (%d,%v,%v), want (2,true,nil)", gen, ok, err)
	}

	// Orderly end + goroutine hygiene: everything the supervisor and its
	// connections spawned (read loops, heartbeats, dispatchers) winds down.
	sup.Stop()
	select {
	case <-sup.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor never stopped")
	}
	waitFor(t, 10*time.Second, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baseline+5
	}, fmt.Sprintf("goroutine leak after supervisor stop: baseline %d, now %d", baseline, runtime.NumGoroutine()))
}
