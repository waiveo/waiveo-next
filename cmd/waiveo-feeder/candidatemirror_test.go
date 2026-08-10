package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// candidatemirror_test.go drives the property the mirror exists for and nothing
// else could prove: a report applied to one process is still listable by the
// NEXT one, before any relay has reconnected.
//
// The store is file-backed throughout — ":memory:" cannot be reopened, so a
// persistence test over it would pass without persisting anything.

const (
	cmSite    = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	cmRelayA  = "relay-mirror-a"
	cmRelayB  = "relay-mirror-b"
	cmDriver  = "roku-ecp"
	cmNativeA = "uuid:roku:ecp:X00500LOBBY"
	cmNativeB = "uuid:roku:ecp:X00500CAFE0"
)

// cmCandidate is one reported candidate in the shape the relay actually sends,
// address and identification included.
func cmCandidate(nativeID, name, address string) wire.DeviceCandidate {
	return wire.DeviceCandidate{
		Match:       json.RawMessage(`{"ssdp":"roku:ecp"}`),
		Provenance:  wire.CandidateProvenanceDiscovered,
		Status:      wire.CandidateStatusPending,
		FirstSeen:   1_000,
		LastSeen:    2_000,
		Driver:      cmDriver,
		NativeID:    nativeID,
		DeviceClass: "media-player",
		Name:        name,
		Address:     address,
		Model:       "Roku Ultra",
		Serial:      "X00500ABC123",
		Entities:    []wire.CandidateEntity{{Key: "main", DeviceClass: "media-player"}},
	}
}

func cmOpenStore(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path, store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", path, err)
	}
	return s
}

// TestMirroredDevicesAreListableAfterARestart is the whole feature end to end:
// one process takes a report, the next process — with no relay connected —
// still answers "what devices are on this site", addresses included.
func TestMirroredDevicesAreListableAfterARestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")

	// --- first run: a relay reports two devices ---
	first := cmOpenStore(t, path)
	liveRegistry := devices.New(cmSite, func() int64 { return 10_000 })
	sink := candidateMirror{registry: liveRegistry, st: first}
	// The sink must satisfy the connection layer's own interface, or none of
	// this is reachable from a real relay.
	var _ feederrelayconn.CandidateSink = sink

	if err := sink.ApplyCandidates(cmRelayA, []wire.DeviceCandidate{
		cmCandidate(cmNativeA, "Lobby TV", "192.168.50.31:8060"),
		cmCandidate(cmNativeB, "Cafe TV", "192.168.50.32:8060"),
	}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	if got := len(liveRegistry.Devices()); got != 2 {
		t.Fatalf("live registry holds %d device(s) after the report, want 2", got)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// --- second run: nothing has connected ---
	second := cmOpenStore(t, path)
	t.Cleanup(func() { _ = second.Close() })
	restoredRegistry := devices.New(cmSite, func() int64 { return 20_000 })

	if got := len(restoredRegistry.Devices()); got != 0 {
		t.Fatalf("a fresh registry already holds %d device(s) — the fixture proves nothing", got)
	}
	n, err := restoreDeviceRegistry(ctx, second, restoredRegistry)
	if err != nil {
		t.Fatalf("restoreDeviceRegistry: %v", err)
	}
	if n != 2 {
		t.Errorf("restored count = %d, want 2", n)
	}

	rows := restoredRegistry.Devices()
	if len(rows) != 2 {
		t.Fatalf("restored registry holds %d device(s), want 2 — this is the blank device page a restart used to serve", len(rows))
	}
	// Ids must be the SAME derived values a live report produces, or a restored
	// row is a different device than the one the relay will re-report and the
	// list would double.
	wantID := deviceid.Device(cmSite, cmDriver, cmNativeA)
	var lobby devices.Device
	for _, d := range rows {
		if d.ID == wantID {
			lobby = d
		}
	}
	if lobby.ID == "" {
		t.Fatalf("restored ids %v do not include the derived %s", ids(rows), wantID)
	}
	if lobby.Address != "192.168.50.31:8060" || lobby.Model != "Roku Ultra" || lobby.Serial != "X00500ABC123" {
		t.Errorf("restored facts = %q/%q/%q, want the reported ones — a restored row with no address is unreachable",
			lobby.Address, lobby.Model, lobby.Serial)
	}
	if lobby.RelayID != cmRelayA {
		t.Errorf("restored relay_id = %q, want %q — it is the dispatch input", lobby.RelayID, cmRelayA)
	}
}

// TestMirrorFollowsTheRegistryOnARefusedReport: REL-111 makes a report
// all-or-nothing, so a refused one must leave the durable copy exactly as it
// was rather than writing the half that parsed.
func TestMirrorFollowsTheRegistryOnARefusedReport(t *testing.T) {
	ctx := context.Background()
	s := cmOpenStore(t, filepath.Join(t.TempDir(), "app.db"))
	t.Cleanup(func() { _ = s.Close() })
	registry := devices.New(cmSite, func() int64 { return 10_000 })
	sink := candidateMirror{registry: registry, st: s}

	if err := sink.ApplyCandidates(cmRelayA, []wire.DeviceCandidate{cmCandidate(cmNativeA, "Lobby TV", "192.168.50.31:8060")}); err != nil {
		t.Fatalf("seed report: %v", err)
	}

	// One device reported twice under one identity: the registry refuses the
	// whole report (REL-153).
	bad := []wire.DeviceCandidate{
		cmCandidate(cmNativeB, "Cafe TV", "192.168.50.32:8060"),
		cmCandidate(cmNativeB, "Cafe TV again", "192.168.50.33:8060"),
	}
	if err := sink.ApplyCandidates(cmRelayA, bad); err == nil {
		t.Fatal("ApplyCandidates accepted a duplicated identity, want a refusal")
	}

	mirrored, err := s.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	if len(mirrored) != 1 || mirrored[0].NativeID != cmNativeA {
		t.Errorf("mirror = %+v after a refused report, want the prior single row untouched", mirrored)
	}
}

// TestMirrorDropsWhatIncumbencyDropped: a relay that reports another relay's
// live device does not take it in the read model (REL-153a), and must not take
// it in the durable copy either — otherwise the next restart would restore the
// capture the incumbency rule just refused.
func TestMirrorDropsWhatIncumbencyDropped(t *testing.T) {
	ctx := context.Background()
	s := cmOpenStore(t, filepath.Join(t.TempDir(), "app.db"))
	t.Cleanup(func() { _ = s.Close() })
	registry := devices.New(cmSite, func() int64 { return 10_000 })
	sink := candidateMirror{registry: registry, st: s}

	if err := sink.ApplyCandidates(cmRelayA, []wire.DeviceCandidate{cmCandidate(cmNativeA, "Lobby TV", "192.168.50.31:8060")}); err != nil {
		t.Fatalf("incumbent report: %v", err)
	}
	// A second enrolled relay names the same guessable (driver, native_id).
	if err := sink.ApplyCandidates(cmRelayB, []wire.DeviceCandidate{cmCandidate(cmNativeA, "Not Your TV", "10.0.0.9:8060")}); err != nil {
		t.Fatalf("challenger report: %v", err)
	}

	mirrored, err := s.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	if len(mirrored) != 1 {
		t.Fatalf("mirror holds %d row(s), want 1", len(mirrored))
	}
	if mirrored[0].RelayID != cmRelayA || mirrored[0].Address != "192.168.50.31:8060" {
		t.Errorf("mirrored row = {relay %q, address %q}, want the incumbent's — a challenger must not capture routing in the durable copy either",
			mirrored[0].RelayID, mirrored[0].Address)
	}
}

// TestForgetClearsBothCopies: revocation must not leave a mirror that lets a
// revoked relay keep describing the site across the next restart.
func TestForgetClearsBothCopies(t *testing.T) {
	ctx := context.Background()
	s := cmOpenStore(t, filepath.Join(t.TempDir(), "app.db"))
	t.Cleanup(func() { _ = s.Close() })
	registry := devices.New(cmSite, func() int64 { return 10_000 })
	sink := candidateMirror{registry: registry, st: s}

	if err := sink.ApplyCandidates(cmRelayA, []wire.DeviceCandidate{cmCandidate(cmNativeA, "Lobby TV", "192.168.50.31:8060")}); err != nil {
		t.Fatalf("seed report: %v", err)
	}
	sink.Forget(cmRelayA)

	if got := len(registry.Devices()); got != 0 {
		t.Errorf("live registry holds %d device(s) after Forget, want 0", got)
	}
	mirrored, err := s.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	if len(mirrored) != 0 {
		t.Errorf("mirror holds %d row(s) after Forget, want 0", len(mirrored))
	}
}

// TestRestoreCarriesAdoptionForward: the adoption decision lives in the adopted
// rows, not the mirror, and the restore has to reunite the two — otherwise
// every restart would report every adopted device as un-adopted.
func TestRestoreCarriesAdoptionForward(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")

	first := cmOpenStore(t, path)
	registry := devices.New(cmSite, func() int64 { return 10_000 })
	sink := candidateMirror{registry: registry, st: first}
	if err := sink.ApplyCandidates(cmRelayA, []wire.DeviceCandidate{cmCandidate(cmNativeA, "Lobby TV", "192.168.50.31:8060")}); err != nil {
		t.Fatalf("seed report: %v", err)
	}
	// The site node has to exist for an authored row to be placed at it.
	if _, err := first.Create(ctx, store.KindScopeNode, mustMarshal(t, datamodel.ScopeNode{
		ID: cmSite, Kind: "org", Name: "Mirror Fixture Org",
		AccountState: "active", Entitlements: json.RawMessage(`{}`),
	})); err != nil {
		t.Fatalf("seed site node: %v", err)
	}
	deviceID := deviceid.Device(cmSite, cmDriver, cmNativeA)
	if created, err := first.AdoptDiscoveredDevice(ctx, deviceID); err != nil || !created {
		t.Fatalf("AdoptDiscoveredDevice(created=%v): %v", created, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := cmOpenStore(t, path)
	t.Cleanup(func() { _ = second.Close() })
	restored := devices.New(cmSite, func() int64 { return 20_000 })
	if _, err := restoreDeviceRegistry(ctx, second, restored); err != nil {
		t.Fatalf("restoreDeviceRegistry: %v", err)
	}

	d, ok := restored.Device(deviceID)
	if !ok {
		t.Fatalf("restored registry has no device %s (has %v)", deviceID, ids(restored.Devices()))
	}
	if !d.Adopted {
		t.Error("restored device reports adopted=false, want true — the adoption record outlives the process that took it")
	}
}

// ids renders a device slice as its ids, for readable failure output.
func ids(rows []devices.Device) []string {
	out := make([]string, 0, len(rows))
	for _, d := range rows {
		out = append(out, d.ID)
	}
	return out
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}
