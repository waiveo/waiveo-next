package relayconn_test

// discoveryenginestate_e2e_test.go proves `discovery.engine_state` travels the
// whole way: a real relay client sends the frame over an authenticated
// connection, the app peer's intake decodes it, and the read model holds it
// under the RIGHT relay.
//
// The two things worth proving here are the two a unit test on either side
// cannot reach. First, that the frame is dispatched at all — a new frame type
// that no `case` matches is silently ignored under REL-004 additive tolerance,
// which is exactly the failure a registry test would sail past. Second, that the
// report is filed under the connection's AUTHENTICATED identity rather than the
// `relay_id` the envelope asserts, because every upward report on this wire
// shares that rule and each new one has to earn it separately.

import (
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/enginestate"
	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	relayclient "github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

const engineStateNowMs = int64(1_755_000_000_000)

// engineStateBody is a report with a distinct non-zero value on every count, so
// a projection that drops or transposes a member is caught. A body of zeroes
// would let `mdns_watches` and `mac_oui_unimplemented` swap places unnoticed.
var engineStateBody = wire.DiscoveryEngineStateBody{
	SSDPLane:            true,
	MDNSLane:            true,
	SSDPWatches:         7,
	MDNSWatches:         5,
	PackPatterns:        4,
	MDNSUndeliverable:   3,
	MacOUIUnimplemented: 2,
	Malformed:           1,
}

// engineStateStack is a harness with the engine-state read model wired and one
// authenticated relay connected.
type engineStateStack struct {
	h       *harness
	reg     *enginestate.Registry
	client  *relayclient.Client
	relayID string
}

func newEngineStateStack(t *testing.T) *engineStateStack {
	t.Helper()
	reg := enginestate.New(func() int64 { return engineStateNowMs })
	h := newHarness(t, feederrelayconn.WithDiscoveryEngineStateSink(reg))

	identStore := enrolledRelay(t, h)
	id, _, err := identStore.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	client, err := relayclient.Dial(relayclient.Config{
		URL:         h.ts.URL,
		Store:       identStore,
		Declaration: testDeclaration,
	})
	if err != nil {
		t.Fatalf("relayconn.Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() >= 1 },
		"authenticated connection never registered with the app peer")

	return &engineStateStack{h: h, reg: reg, client: client, relayID: id.RelayID}
}

func TestEngineStateReportReachesTheReadModel(t *testing.T) {
	s := newEngineStateStack(t)

	if err := s.client.SendDiscoveryEngineState(engineStateBody); err != nil {
		t.Fatalf("SendDiscoveryEngineState: %v", err)
	}

	var got enginestate.State
	waitFor(t, 5*time.Second, func() bool {
		states := s.reg.States()
		if len(states) != 1 {
			return false
		}
		got = states[0]
		return true
	}, "the engine-state report never reached the read model — check the frame is DISPATCHED, not merely sent (an unmatched frame type is silently ignored under REL-004)")

	if got.RelayID != s.relayID {
		t.Errorf("filed under %q, want the authenticated relay %q", got.RelayID, s.relayID)
	}
	// Field for field, because the failure this guards is adding a member to the
	// body and forgetting one line in the projection — which nothing else sees.
	want := enginestate.State{
		RelayID:             s.relayID,
		SSDPLane:            true,
		MDNSLane:            true,
		SSDPWatches:         7,
		MDNSWatches:         5,
		PackPatterns:        4,
		MDNSUndeliverable:   3,
		MacOUIUnimplemented: 2,
		Malformed:           1,
		WatchingNothing:     false,
		ReportedAtMs:        engineStateNowMs,
	}
	if got != want {
		t.Errorf("projection mismatch\n got %+v\nwant %+v", got, want)
	}
}

func TestEngineStateReportingZeroWatchesRaisesWatchingNothing(t *testing.T) {
	// The condition the frame exists to surface, end to end: a relay that is
	// connected, healthy, and watching for absolutely nothing. Before this frame
	// that state was visible only in the relay's own journal.
	s := newEngineStateStack(t)

	if err := s.client.SendDiscoveryEngineState(wire.DiscoveryEngineStateBody{
		SSDPLane: true, MDNSLane: false,
	}); err != nil {
		t.Fatalf("SendDiscoveryEngineState: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool { return len(s.reg.States()) == 1 },
		"the report never arrived")

	got := s.reg.States()[0]
	if !got.WatchingNothing {
		t.Error("a relay holding no watch in either lane must report watching_nothing")
	}
	// And the lane booleans must survive, because they are what make the zero
	// readable: mdns_watches:0 with the lane OFF is a deployment choice, the same
	// zero with the lane ON is a generation that declared no mDNS patterns.
	if !got.SSDPLane || got.MDNSLane {
		t.Errorf("lane booleans lost in transit: ssdp=%v mdns=%v, want true/false", got.SSDPLane, got.MDNSLane)
	}
}

func TestAForgedRelayIdOnAnEngineStateFrameIsFiledUnderTheRealReporter(t *testing.T) {
	// Every upward report on this wire is filed under the authenticated
	// connection identity, never the envelope's `relay_id`. That rule has to be
	// re-proven per frame type: it is one line in each handler, and the frame
	// still decodes and still lands somewhere if the line is wrong — it just
	// lands under whichever relay the sender names.
	s := newEngineStateStack(t)

	bIdent := enrolledRelay(t, s.h)
	bID, _, err := bIdent.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	if bID.RelayID == s.relayID {
		t.Fatal("the two relays enrolled under one identity — this case cannot separate them")
	}
	ws, err := rawDial(t, s.h, bIdent, bID.CertPEM, []string{wire.Subprotocol})
	if err != nil {
		t.Fatalf("rawDial (relay B): %v", err)
	}
	defer ws.CloseNow()
	rawHandshake(t, ws, bIdent)

	forged, err := wire.NewFrame(wire.FrameTypeDiscoveryEngineState, "forged-engine-1",
		s.relayID, // the lie: relay B stamping relay A's identity
		wire.DiscoveryEngineStateBody{SSDPWatches: 99})
	if err != nil {
		t.Fatalf("NewFrame(discovery.engine_state): %v", err)
	}
	if err := wsSend(t, ws, forged); err != nil {
		t.Fatalf("send forged report: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool { return len(s.reg.States()) == 1 },
		"the forged report never reached the intake at all")

	got := s.reg.States()[0]
	if got.RelayID != bID.RelayID {
		t.Errorf("filed under %q; a report must be attributed to the relay that SENT it (%q), not the one it names", got.RelayID, bID.RelayID)
	}
	if got.SSDPWatches != 99 {
		t.Errorf("ssdp_watches = %d, want the forged body's 99 — the report is accepted, just attributed correctly", got.SSDPWatches)
	}
}
