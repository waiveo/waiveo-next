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
	sink := newCandidateMirror(liveRegistry, first, store.WallClockMs)
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
	if n.Restored != 2 {
		t.Errorf("restored count = %d, want 2", n.Restored)
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
	sink := newCandidateMirror(registry, s, store.WallClockMs)

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
	sink := newCandidateMirror(registry, s, store.WallClockMs)

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
	sink := newCandidateMirror(registry, s, store.WallClockMs)

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
	sink := newCandidateMirror(registry, first, store.WallClockMs)
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

// TestRestoreDropsARevokedRelaysDevices is the offline half of revocation, and
// it is the half the durable mirror broke. candidateMirror.Forget is driven by
// the per-connection revocation watcher, so it fires only while the relay is
// CONNECTED — and a relay is usually revoked precisely because it is not
// (stolen, decommissioned, compromised). Before this check the rows survived
// every restart and the revoked relay's devices came back in GET /devices,
// still adoptable, forever.
//
// The rows are also DELETED, not merely skipped, so the next boot has nothing
// left to re-decide.
func TestRestoreDropsARevokedRelaysDevices(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")

	first := cmOpenStore(t, path)
	registry := devices.New(cmSite, func() int64 { return 10_000 })
	sink := newCandidateMirror(registry, first, store.WallClockMs)
	if err := sink.ApplyCandidates(cmRelayA, []wire.DeviceCandidate{cmCandidate(cmNativeA, "Lobby TV", "192.168.50.31:8060")}); err != nil {
		t.Fatalf("seed relay A's report: %v", err)
	}
	if err := sink.ApplyCandidates(cmRelayB, []wire.DeviceCandidate{cmCandidate(cmNativeB, "Cafe TV", "192.168.50.32:8060")}); err != nil {
		t.Fatalf("seed relay B's report: %v", err)
	}
	// Relay A is revoked while DISCONNECTED — the connection watcher never runs,
	// so this is the only record of the decision anywhere.
	if err := first.RevokeSubject(ctx, store.RevocationSubjectRelay, cmRelayA, "operator"); err != nil {
		t.Fatalf("RevokeSubject: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := cmOpenStore(t, path)
	t.Cleanup(func() { _ = second.Close() })
	restoredRegistry := devices.New(cmSite, func() int64 { return 20_000 })
	n, err := restoreDeviceRegistry(ctx, second, restoredRegistry)
	if err != nil {
		t.Fatalf("restoreDeviceRegistry: %v", err)
	}

	revokedDeviceID := deviceid.Device(cmSite, cmDriver, cmNativeA)
	if _, ok := restoredRegistry.Device(revokedDeviceID); ok {
		t.Error("a revoked relay's device came back after a restart — revocation ends a relay's authority to describe " +
			"the site, and a restart must not hand it back")
	}
	// The un-revoked relay is untouched: a revocation is about one relay.
	if _, ok := restoredRegistry.Device(deviceid.Device(cmSite, cmDriver, cmNativeB)); !ok {
		t.Errorf("the un-revoked relay's device was dropped too (registry has %v)", ids(restoredRegistry.Devices()))
	}
	if n.Restored != 1 {
		t.Errorf("restoreDeviceRegistry reported %d restored device(s), want 1 — the count must not include rows it refused", n.Restored)
	}

	// And the rows are gone from the mirror, so this is decided once rather than
	// on every boot for the life of the deployment.
	mirrored, err := second.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	for _, d := range mirrored {
		if d.RelayID == cmRelayA {
			t.Errorf("the revoked relay's row %s is still in the mirror; the restore must purge it, not just skip it", d.DeviceID)
		}
	}
}

// TestDeviceAgeSurvivesARelayRestartEndToEnd drives defect #196 down the path a
// real report actually takes — the connection layer's CandidateSink, the live
// registry, the durable mirror, and back out through the rows `GET /devices`
// serves — rather than against the store alone.
//
// That end matters as much as the storage does. The relay's own first_seen is
// its process uptime and nothing more, and it used to be copied through the
// mirror unchanged; the value an operator saw was therefore reset by every relay
// restart even though the feeder had held the right one a second earlier.
func TestDeviceAgeSurvivesARelayRestartEndToEnd(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	appClock := &cmClock{ms: 1_800_000_000_000}

	first := cmOpenStoreAt(t, path, appClock.now)
	registry := devices.New(cmSite, func() int64 { return 10_000 })
	sink := newCandidateMirror(registry, first, appClock.now)

	// The relay claims it has been watching this TV for two hours. Its wall clock
	// is its own and nothing on this platform attests it, so what reaches the row
	// is this site's own reading — see internal/app/store devicefirstseen.go.
	const twoHours = 2 * 60 * 60 * 1000
	const relayNow = 1_791_360_000_000
	aged := cmCandidate(cmNativeA, "Lobby TV", "192.168.50.31:8060")
	aged.FirstSeen, aged.LastSeen = relayNow-twoHours, relayNow
	if err := sink.ApplyCandidates(cmRelayA, []wire.DeviceCandidate{aged}); err != nil {
		t.Fatalf("first report: %v", err)
	}

	born := cmDevice(t, registry, deviceid.Device(cmSite, cmDriver, cmNativeA))
	if born.FirstSeen != appClock.now() {
		t.Fatalf("served first_seen = %d, want this site's own clock %d", born.FirstSeen, appClock.now())
	}
	if born.LastSeen != appClock.now() {
		t.Fatalf("served last_seen = %d, want %d", born.LastSeen, appClock.now())
	}

	// The relay process restarts an hour later. Its candidate store is empty, so
	// it re-mints first_seen at the moment it re-discovers the device — the exact
	// report that used to overwrite two hours of history with "brand new".
	appClock.advance(60 * 60 * 1000)
	remitted := cmCandidate(cmNativeA, "Lobby TV", "192.168.50.31:8060")
	remitted.FirstSeen, remitted.LastSeen = 2_500, 2_600
	if err := sink.ApplyCandidates(cmRelayA, []wire.DeviceCandidate{remitted}); err != nil {
		t.Fatalf("report after the relay restart: %v", err)
	}

	after := cmDevice(t, registry, born.ID)
	if after.FirstSeen != born.FirstSeen {
		t.Errorf("served first_seen = %d after a relay restart, want the durable %d — "+
			"this is #196: a device that has been on the LAN for hours reading as new", after.FirstSeen, born.FirstSeen)
	}
	if after.LastSeen != appClock.now() {
		t.Errorf("served last_seen = %d, want %d — it must still advance", after.LastSeen, appClock.now())
	}

	// The device is unplugged. The relay does not expire candidates, so it keeps
	// re-sending this one unchanged; the served last_seen must stop moving, or
	// the console reports a dead TV as live.
	wentDark := appClock.now()
	for day := 1; day <= 3; day++ {
		appClock.advance(24 * 60 * 60 * 1000)
		if err := sink.ApplyCandidates(cmRelayA, []wire.DeviceCandidate{remitted}); err != nil {
			t.Fatalf("day %d report: %v", day, err)
		}
		if got := cmDevice(t, registry, born.ID).LastSeen; got != wentDark {
			t.Fatalf("day %d: served last_seen = %d, want it frozen at %d — the relay replayed a candidate it has not re-observed",
				day, got, wentDark)
		}
	}

	// And it survives the OTHER restart too: the feeder's. The boot restore is
	// the only thing that answers the device page before a relay reconnects, so
	// the age has to come back with the rows.
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second := cmOpenStoreAt(t, path, appClock.now)
	t.Cleanup(func() { _ = second.Close() })
	restored := devices.New(cmSite, func() int64 { return 20_000 })
	if _, err := restoreDeviceRegistry(ctx, second, restored); err != nil {
		t.Fatalf("restoreDeviceRegistry: %v", err)
	}
	if got := cmDevice(t, restored, born.ID); got.FirstSeen != born.FirstSeen {
		t.Errorf("restored first_seen = %d, want %d — the boot projection must carry the age, not just the row",
			got.FirstSeen, born.FirstSeen)
	}
}

// cmClock is a hand-driven app clock, so a case about "an hour later" moves the
// clock instead of sleeping.
type cmClock struct{ ms int64 }

func (c *cmClock) now() int64      { return c.ms }
func (c *cmClock) advance(d int64) { c.ms += d }

// cmOpenStoreAt is cmOpenStore with the clock named, for the cases whose subject
// IS the clock.
func cmOpenStoreAt(t *testing.T, path string, nowMs func() int64) *store.Store {
	t.Helper()
	s, err := store.Open(path, nowMs)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", path, err)
	}
	return s
}

// cmDevice reads one device out of the registry as `GET /devices` would, failing
// the case when it is absent.
func cmDevice(t *testing.T, r *devices.Registry, id string) devices.Device {
	t.Helper()
	for _, d := range r.Devices() {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("device %s is not in the registry; ids = %v", id, ids(r.Devices()))
	return devices.Device{}
}

// TestTheRestoreCountsWhatTheBootLogHasToSayApart is the observability half of
// the boot restore, pinned at the counts rather than at the log lines, because
// the counts are what makes the three outcomes distinguishable at all.
//
// A bare "restored N" went silent at N=0, so a box whose durable mirror had been
// UNWRITABLE for a week — which is what box .12 spent seven days doing (#194) —
// booted byte-identically to a fresh install. And the first-seen projection, the
// fact #196 exists to put in front of an operator, was never counted anywhere, so
// "every device in the console shows an em dash" had no correlate in the log.
func TestTheRestoreCountsWhatTheBootLogHasToSayApart(t *testing.T) {
	ctx := context.Background()

	// An empty mirror: nothing was restored, and the reason is that there is
	// nothing there — which must not read the same as "the restore ran and put
	// devices back".
	empty := cmOpenStore(t, filepath.Join(t.TempDir(), "empty.db"))
	t.Cleanup(func() { _ = empty.Close() })
	counts, err := restoreDeviceRegistry(ctx, empty, devices.New(cmSite, func() int64 { return 20_000 }))
	if err != nil {
		t.Fatalf("restoreDeviceRegistry over an empty store: %v", err)
	}
	if counts.Mirrored != 0 || counts.Restored != 0 || counts.Aged != 0 {
		t.Fatalf("an empty mirror restored %+v, want zeros", counts)
	}

	// A populated one, with the ages the ledger holds.
	path := filepath.Join(t.TempDir(), "app.db")
	first := cmOpenStore(t, path)
	sink := newCandidateMirror(devices.New(cmSite, func() int64 { return 10_000 }), first, store.WallClockMs)
	if err := sink.ApplyCandidates(cmRelayA, []wire.DeviceCandidate{
		cmCandidate(cmNativeA, "Lobby TV", "192.168.50.31:8060"),
		cmCandidate(cmNativeB, "Cafe TV", "192.168.50.32:8060"),
	}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := cmOpenStore(t, path)
	t.Cleanup(func() { _ = second.Close() })
	counts, err = restoreDeviceRegistry(ctx, second, devices.New(cmSite, func() int64 { return 20_000 }))
	if err != nil {
		t.Fatalf("restoreDeviceRegistry: %v", err)
	}
	if counts.Mirrored != 2 || counts.Restored != 2 {
		t.Errorf("restore counts = %+v, want 2 mirrored and 2 restored", counts)
	}
	if counts.Aged != 2 {
		t.Errorf("restore reported %d device(s) with a stored first_seen, want 2 — the age is the one fact the "+
			"console cannot answer for before a relay reconnects, and it was never counted", counts.Aged)
	}

	// And a mirror whose ages have all been retired restores the devices while
	// answering for none of their ages — the third outcome, which used to be
	// indistinguishable from the second.
	third := cmOpenStore(t, path)
	for _, native := range []string{cmNativeA, cmNativeB} {
		if _, err := third.RetireDeviceFirstSeen(ctx, deviceid.Device(cmSite, cmDriver, native)); err != nil {
			t.Fatalf("RetireDeviceFirstSeen: %v", err)
		}
	}
	retiredRegistry := devices.New(cmSite, func() int64 { return 20_000 })
	counts, err = restoreDeviceRegistry(ctx, third, retiredRegistry)
	if err != nil {
		t.Fatalf("restoreDeviceRegistry after retiring every age: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if counts.Restored != 2 || counts.Aged != 0 {
		t.Errorf("restore counts = %+v, want 2 restored and 0 with a stored first_seen", counts)
	}
	// The counts alone are not the claim, and asserting only them is how the
	// defect below survived a test that retires two devices and restarts.
	//
	// A retire clears the AGE and deliberately keeps the SIGHTING — the store's
	// own docstring says a retire that blanked last_seen "would tell an operator a
	// device that reported a minute ago has never been heard from", and
	// devices.Registry.ForgetFirstSeen honours the same split in memory. Across a
	// restart it did not hold: the restore's projection dropped any row whose
	// first_seen was zero, so both instants went and the console reported a device
	// that had never been heard from while the store still held the sighting.
	// A powered-off device is exactly the population retire exists for, so
	// "until it reports again" can be never.
	for _, native := range []string{cmNativeA, cmNativeB} {
		id := deviceid.Device(cmSite, cmDriver, native)
		got := cmDevice(t, retiredRegistry, id)
		if got.FirstSeen != 0 {
			t.Errorf("%s still reports first_seen %d after a retire and a restart", id, got.FirstSeen)
		}
		if got.LastSeen == 0 {
			t.Errorf("%s lost its last_seen across the restart: the store still holds the sighting, and a "+
				"console that reports a device as never heard from is the thing the retire promises not to do",
				id)
		}
	}
}
