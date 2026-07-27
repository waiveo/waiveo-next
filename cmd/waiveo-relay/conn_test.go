package main

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	feederenroll "github.com/maaxton/waiveo-next/internal/feeder/enroll"
	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// connTestHarness is one fully-wired in-process app peer for the binary's
// pullOverFrames seam: the real enrollment server + the real /relay/v1 WS
// server on one mTLS httptest listener, serving a swappable snapshot —
// the same harness shape internal/feeder/relayconn's own e2e proof uses.
type connTestHarness struct {
	enrollSrv *feederenroll.Server
	connSrv   *feederrelayconn.Server
	feederID  *signing.Identity
	ts        *httptest.Server

	mu   sync.Mutex
	snap wire.StateSnapshotBody
}

func (h *connTestHarness) current() (wire.StateSnapshotBody, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snap, nil
}

func (h *connTestHarness) stage(s wire.StateSnapshotBody) {
	h.mu.Lock()
	h.snap = s
	h.mu.Unlock()
}

func newConnTestHarness(t *testing.T) *connTestHarness {
	t.Helper()
	feederID, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	snap, err := snapshot.Build([]byte("conn-test-image-bytes"), "https://origin.example", feederID, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	enrollSrv, err := feederenroll.NewServer(feederID, snap)
	if err != nil {
		t.Fatalf("feederenroll.NewServer: %v", err)
	}

	h := &connTestHarness{enrollSrv: enrollSrv, feederID: feederID, snap: snap}
	h.connSrv = feederrelayconn.New(
		h.current,
		enrollSrv.RelayEnrollmentKey,
		enrollSrv.IsRevoked,
		hello.SiteBinding{ScopeNode: "site-conn-test", TZ: "UTC"},
		hello.AppPeerImplementedMinors(1, 1),
		[]string{"telemetry.latest_only_v1"},
	)

	mux := http.NewServeMux()
	enrollSrv.Register(mux)
	mux.Handle("/relay/v1", h.connSrv.Handler())

	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = &tls.Config{
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  enrollSrv.ClientCAPool(),
		MinVersion: tls.VersionTLS13,
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	h.ts = ts
	return h
}

// stageGeneration installs a snapshot at gen, validly signed by the harness
// feeder's own key (foreign=false) or a freshly-minted impostor key
// (foreign=true) — the REL-071 staging.
func (h *connTestHarness) stageGeneration(t *testing.T, gen int64, foreign bool) {
	t.Helper()
	h.mu.Lock()
	next := h.snap
	h.mu.Unlock()

	next.Generation = gen
	priv := h.feederID.SigningPriv()
	if foreign {
		_, priv = signhash.GenerateKey()
		next.SignedWithKey = "ed25519:impostor"
	}
	scope, err := wire.SignedScopeBytes(gen, next.Hash)
	if err != nil {
		t.Fatalf("SignedScopeBytes: %v", err)
	}
	next.Signature = wire.EncodeSignature(signhash.Sign(priv, scope))
	h.stage(next)
}

func dialConnTest(t *testing.T, h *connTestHarness, store *identity.Store) *relayconn.Client {
	t.Helper()
	client, err := relayconn.Dial(relayconn.Config{
		URL:         h.ts.URL,
		Store:       store,
		Declaration: relayHelloDeclaration(config{listen: "127.0.0.1:0"}),
	})
	if err != nil {
		t.Fatalf("relayconn.Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestPullOverFrames drives the binary's whole live-pull seam against a real
// app peer on one authenticated connection:
//
//  1. a full pull (no since claim) verifies + applies + persists, and the
//     wire carries a success state.ack correlated to the pull (REL-054);
//  2. a same-generation pull answers state.unchanged, surfaced as an Applied
//     at exactly the claimed generation — the caller's monotonic no-op;
//  3. a staged impostor-signed snapshot is rejected (REL-071/072): nothing
//     applied, and the wire state.ack reports the SNAPSHOT_SIGNATURE_INVALID
//     error with the UNADVANCED prior generation.
func TestPullOverFrames(t *testing.T) {
	h := newConnTestHarness(t)

	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := relayenroll.Run(h.ts.URL, store); err != nil {
		t.Fatalf("relayenroll.Run: %v", err)
	}
	client := dialConnTest(t, h, store)
	relayID := client.RelayID()

	// (1) full pull applies + persists + acks on the wire.
	applied, err := pullOverFrames(client, store, 0)
	if err != nil {
		t.Fatalf("pullOverFrames (full): %v", err)
	}
	if applied.Generation != 1 {
		t.Fatalf("applied generation = %d, want 1", applied.Generation)
	}
	if gen, _, ok, err := store.LastAppliedGeneration(); err != nil || !ok || gen != 1 {
		t.Fatalf("persisted last-applied = (%d,%v,%v), want (1,true,nil)", gen, ok, err)
	}
	waitForConnTest(t, func() bool {
		f, ok := h.connSrv.LastStateAck(relayID)
		if !ok {
			return false
		}
		var body wire.StateAckBody
		return f.DecodeBody(&body) == nil && body.AppliedGeneration == 1 && body.Error == nil
	}, "no success state.ack for generation 1 arrived at the app peer (REL-054)")

	// (2) same-generation pull → state.unchanged → no-op Applied at the claim.
	unchanged, err := pullOverFrames(client, store, 1)
	if err != nil {
		t.Fatalf("pullOverFrames (since=1): %v", err)
	}
	if unchanged.Generation != 1 || unchanged.ScreenID != "" {
		t.Fatalf("unchanged pull returned %+v, want a bare Applied at generation 1", unchanged)
	}

	// (3) impostor-signed snapshot at a higher generation: rejected, nothing
	// applied, and the wire ack carries the taxonomy error + unadvanced
	// generation (REL-072).
	h.stageGeneration(t, 2, true)
	if _, err := pullOverFrames(client, store, 1); !errors.Is(err, desiredstate.ErrSnapshotSignatureInvalid) {
		t.Fatalf("impostor pull error = %v, want ErrSnapshotSignatureInvalid (REL-071)", err)
	}
	if gen, _, ok, err := store.LastAppliedGeneration(); err != nil || !ok || gen != 1 {
		t.Fatalf("after rejected snapshot, persisted last-applied = (%d,%v,%v), want unchanged (1,true,nil)", gen, ok, err)
	}
	waitForConnTest(t, func() bool {
		f, ok := h.connSrv.LastStateAck(relayID)
		if !ok {
			return false
		}
		var body wire.StateAckBody
		return f.DecodeBody(&body) == nil &&
			body.Error != nil && body.Error.Code == "SNAPSHOT_SIGNATURE_INVALID" &&
			body.AppliedGeneration == 1
	}, "no error state.ack (SNAPSHOT_SIGNATURE_INVALID, applied_generation=1) arrived at the app peer (REL-054/072)")

	// A valid staged generation then converges through the same seam.
	h.stageGeneration(t, 2, false)
	recovered, err := pullOverFrames(client, store, 1)
	if err != nil {
		t.Fatalf("pullOverFrames (recovered gen 2): %v", err)
	}
	if recovered.Generation != 2 {
		t.Fatalf("recovered generation = %d, want 2", recovered.Generation)
	}
}

func waitForConnTest(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}
