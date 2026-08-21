package main

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/discovery"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// enginestate_test.go covers the relay binary's half of `discovery.engine_state`:
// what a connect-time resend would send, and the projection from an applied
// generation onto the wire body.
//
// The whole-stack proof that the report REACHES the app peer and becomes an
// api/1 row lives in internal/feeder/relayconn/discoveryenginestate_e2e_test.go;
// this file is the hold/resend decision and the projection.

func TestNothingIsResentBeforeAGenerationHasApplied(t *testing.T) {
	// The distinction the `have` flag exists for. A zeroed body would tell the
	// app peer this relay is watching nothing — an alarm — when the truth is
	// that it has not looked yet.
	r := newEngineStateReporter(nil)
	if _, have := r.pending(); have {
		t.Error("a reporter that has recorded nothing must have nothing pending")
	}
}

func TestResendCarriesTheLATESTAppliedState(t *testing.T) {
	// Reconnect must re-state what is true NOW, not what was true at the first
	// apply. A reporter that kept the first would pin the console to a watch set
	// two pack installs old, and it would look perfectly plausible.
	r := newEngineStateReporter(nil)
	r.record(wire.DiscoveryEngineStateBody{SSDPWatches: 1, PackPatterns: 1})
	r.record(wire.DiscoveryEngineStateBody{SSDPWatches: 9, PackPatterns: 4})

	got, have := r.pending()
	if !have {
		t.Fatal("a recorded state must be pending for the next connect")
	}
	if got.SSDPWatches != 9 || got.PackPatterns != 4 {
		t.Errorf("pending = %+v, want the second record", got)
	}
}

func TestStateIsHeldEvenWhenNoConnectionTookIt(t *testing.T) {
	// record stores FIRST and reports second, so a generation that applied while
	// the app peer was unreachable is still owed and still paid on reconnect.
	// A reporter that only stored on successful delivery would lose exactly the
	// state an operator most wants after an outage.
	r := newEngineStateReporter(nil) // no live connection at all
	r.record(wire.DiscoveryEngineStateBody{SSDPWatches: 3})

	got, have := r.pending()
	if !have || got.SSDPWatches != 3 {
		t.Errorf("state recorded during an outage must survive it: pending=%v %+v", have, got)
	}
}

func TestNilReporterIsInert(t *testing.T) {
	// discoveryWatchApplier takes a nil reporter in tests and in builds with
	// discovery off; neither may panic.
	var r *engineStateReporter
	r.record(wire.DiscoveryEngineStateBody{SSDPWatches: 1})
	r.resend(nil)
	if _, have := r.pending(); have {
		t.Error("a nil reporter has nothing pending")
	}
}

func TestApplierProjectsTheGenerationOntoTheWireBody(t *testing.T) {
	// The projection is the one place a number can silently diverge from the log
	// line beside it. Asserted field for field against a generation carrying a
	// deliverable pattern, an undeliverable one (mDNS lane off), an unimplemented
	// form, and a malformed entry — so every count has a distinct non-zero value
	// and a transposed pair cannot pass.
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	disc, err := discovery.New(discovery.Config{Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	rep := newEngineStateReporter(nil)

	// mdnsL is nil: the mDNS lane is OFF, which is what makes the declared mDNS
	// pattern undeliverable rather than the pack's fault.
	apply := discoveryWatchApplier(disc, nil, store, nil, nil, rep)
	apply(wire.DeviceInventory{PackMatchPatterns: rawPatterns(t,
		`{"deviceClass":"tv","match":[{"ssdp":"urn:acme:tv:1"}]}`,
		`{"deviceClass":"tv","match":[{"mdns":"_acme._tcp"}]}`,
		`{"deviceClass":"tv","match":[{"macOui":"AABBCC"}]}`,
		`{"nope":true}`,
	)})

	got, have := rep.pending()
	if !have {
		t.Fatal("applying a generation must record an engine state")
	}
	if got.MDNSLane {
		t.Error("mdns_lane must report FALSE when no listener is running — the boolean is what makes mdns_watches:0 readable")
	}
	if !got.SSDPLane {
		t.Error("ssdp_lane must report true when the discoverer is running")
	}
	if got.PackPatterns != 4 {
		t.Errorf("pack_patterns = %d, want the 4 the generation declared", got.PackPatterns)
	}
	if got.MDNSUndeliverable != 1 {
		t.Errorf("mdns_undeliverable = %d, want 1 (declared, lane off)", got.MDNSUndeliverable)
	}
	if got.MacOUIUnimplemented != 1 {
		t.Errorf("mac_oui_unimplemented = %d, want 1", got.MacOUIUnimplemented)
	}
	if got.Malformed != 1 {
		t.Errorf("malformed = %d, want 1", got.Malformed)
	}
	if got.SSDPWatches != 1 {
		t.Errorf("ssdp_watches = %d, want the 1 deliverable SSDP pattern", got.SSDPWatches)
	}
	if got.WatchingNothing() {
		t.Error("a relay holding one live SSDP watch is not watching nothing")
	}
}
