package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// discovereddevices_test.go drives the mirror as the thing it exists to be: a
// copy of the relays' reports that is still there after the process is not.
// Every case below therefore either reopens the file or asserts something the
// in-memory registry could not have told us.

const (
	ddDeviceA = "01J8Z8D1SC0VEREDDEV1CEAAA1"
	ddDeviceB = "01J8Z8D1SC0VEREDDEV1CEBBB2"
	ddDeviceC = "01J8Z8D1SC0VEREDDEV1CECCC3"
	ddNodeID  = "01J8Z8D1SC0VEREDN0DE000001"
)

// discovered builds one mirror row at the fixture placement node.
func discovered(id, relayID, nativeID string) store.DiscoveredDevice {
	return store.DiscoveredDevice{
		DeviceID:    id,
		RelayID:     relayID,
		ScopeNode:   ddNodeID,
		Driver:      "roku-ecp",
		NativeID:    nativeID,
		DeviceClass: "media-player",
		Name:        "Lobby TV",
		Address:     "192.168.50.31:8060",
		Model:       "Roku Ultra",
		Serial:      "X00500ABC123",
		FirstSeen:   1_000,
		LastSeen:    2_000,
		Entities:    []wire.CandidateEntity{{Key: "main", DeviceClass: "media-player"}},
	}
}

// openFileStore opens a FILE-backed store at path — the whole point of these
// cases, since ":memory:" cannot be reopened and would make a persistence test
// prove nothing.
func openFileStore(t *testing.T, path string) *store.Store {
	t.Helper()
	return openFileStoreAt(t, path, store.WallClockMs)
}

// ddAppNowMs is the app-clock reading the fixed-clock cases below run on. The
// stored seen instants are stamped from the STORE's clock rather than copied off
// the report (devicefirstseen.go), so a case that asserts an exact value has to
// pin that clock instead of racing the wall.
const ddAppNowMs int64 = 1_800_000_000_000

// openFileStoreAt is openFileStore with the clock named, for the cases whose
// subject IS the clock.
func openFileStoreAt(t *testing.T, path string, nowMs func() int64) *store.Store {
	t.Helper()
	s, err := store.Open(path, nowMs)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	return s
}

// TestDiscoveredDevicesSurviveAReopen is the requirement the mirror exists for:
// a feeder that restarts must still be able to answer "what devices are on this
// site" before any relay has reconnected.
func TestDiscoveredDevicesSurviveAReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")

	first := openFileStoreAt(t, path, func() int64 { return ddAppNowMs })
	if _, err := first.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		discovered(ddDeviceA, "relay-a", "uuid:roku:ecp:X1"),
	}); err != nil {
		t.Fatalf("ReplaceDiscoveredDevices: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := openFileStoreAt(t, path, func() int64 { return ddAppNowMs })
	t.Cleanup(func() { _ = second.Close() })

	got, err := second.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices after reopen: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("after reopen: %d device(s), want 1 — the mirror is the answer to a restart with no relay connected", len(got))
	}
	d := got[0]
	if d.DeviceID != ddDeviceA || d.RelayID != "relay-a" || d.Driver != "roku-ecp" || d.NativeID != "uuid:roku:ecp:X1" {
		t.Errorf("identity round trip = %+v, want the written one", d)
	}
	// The reachability facts are the ones a restart most needs to keep: without
	// them the restored row is a name with nowhere to send anything.
	if d.Address != "192.168.50.31:8060" || d.Model != "Roku Ultra" || d.Serial != "X00500ABC123" {
		t.Errorf("address/model/serial = %q/%q/%q, want them all round-tripped", d.Address, d.Model, d.Serial)
	}
	// Both instants are stamped on the APP's clock, never copied off the report:
	// the fixture's candidate carries 1000/2000, the relay's own unattested wall
	// readings, and neither reaches the row. A reader must be able to tell how
	// old the answer is, and it can only do that if every row's timestamps are
	// readings of ONE clock (devicefirstseen.go).
	if d.FirstSeen != ddAppNowMs || d.LastSeen != ddAppNowMs {
		t.Errorf("first/last seen = %d/%d, want %d for both — this site's own clock, not the relay's",
			d.FirstSeen, d.LastSeen, ddAppNowMs)
	}
	// The relay's raw stamp rides along, unrendered: the next report compares
	// against it to tell a genuine re-sighting from a replayed frozen candidate.
	if d.RelayLastSeen != 2_000 {
		t.Errorf("relay_last_seen = %d, want the reported 2000 kept as the change-detector", d.RelayLastSeen)
	}
	if len(d.Entities) != 1 || d.Entities[0].Key != "main" {
		t.Errorf("entities = %+v, want the reported fan-out (adoption consumes it)", d.Entities)
	}
}

// TestReplaceDiscoveredDevicesIsPerRelayFullSet pins REL-111's replace
// semantics onto the durable copy: a relay's report replaces exactly its own
// rows and cannot disturb another relay's.
func TestReplaceDiscoveredDevicesIsPerRelayFullSet(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)

	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		discovered(ddDeviceA, "relay-a", "uuid:roku:ecp:X1"),
		discovered(ddDeviceB, "relay-a", "uuid:roku:ecp:X2"),
	}); err != nil {
		t.Fatalf("seed relay-a: %v", err)
	}
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-b", []store.DiscoveredDevice{
		discovered(ddDeviceC, "relay-b", "uuid:roku:ecp:X3"),
	}); err != nil {
		t.Fatalf("seed relay-b: %v", err)
	}

	// relay-a now sees only one of its two devices — the other was unplugged.
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		discovered(ddDeviceB, "relay-a", "uuid:roku:ecp:X2"),
	}); err != nil {
		t.Fatalf("re-report relay-a: %v", err)
	}

	got, err := s.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, d := range got {
		ids = append(ids, d.DeviceID)
	}
	// Ordered by device_id, which is also the keyset order the list surface
	// pages by — asserted here so a caller can hand the result straight to it.
	want := []string{ddDeviceB, ddDeviceC}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Errorf("device ids = %v, want %v (relay-a's dropped device gone, relay-b's untouched, id order)", ids, want)
	}
}

// TestForgetDiscoveredDevicesClearsOneRelay covers the revocation path: a
// revoked relay must stop describing the site, including across restarts.
func TestForgetDiscoveredDevicesClearsOneRelay(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)

	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{discovered(ddDeviceA, "relay-a", "uuid:roku:ecp:X1")}); err != nil {
		t.Fatalf("seed relay-a: %v", err)
	}
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-b", []store.DiscoveredDevice{discovered(ddDeviceC, "relay-b", "uuid:roku:ecp:X3")}); err != nil {
		t.Fatalf("seed relay-b: %v", err)
	}
	if err := s.ForgetDiscoveredDevices(ctx, "relay-a"); err != nil {
		t.Fatalf("ForgetDiscoveredDevices: %v", err)
	}

	got, err := s.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	if len(got) != 1 || got[0].DeviceID != ddDeviceC {
		t.Errorf("remaining = %+v, want only relay-b's device", got)
	}
}

// TestDiscoveryWritesDoNotBumpTheGeneration is load-bearing rather than
// cosmetic: candidate reports arrive every minute for as long as the box is up,
// and a generation bump per report would make every relay on the site re-pull
// and re-verify a signed snapshot forever for a change that alters no desired
// state.
func TestDiscoveryWritesDoNotBumpTheGeneration(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)

	before := gen(t, s)
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{discovered(ddDeviceA, "relay-a", "uuid:roku:ecp:X1")}); err != nil {
		t.Fatalf("ReplaceDiscoveredDevices: %v", err)
	}
	if err := s.ForgetDiscoveredDevices(ctx, "relay-a"); err != nil {
		t.Fatalf("ForgetDiscoveredDevices: %v", err)
	}
	if after := gen(t, s); after != before {
		t.Errorf("generation %d -> %d over two mirror writes, want unchanged", before, after)
	}
}

// TestAdoptDiscoveredDeviceCreatesTheAdoptedRow is the whole point of the adopt
// operation: not a flag, but the durable row the signed desired-state
// `device_inventory` section compiles from (REL-063).
func TestAdoptDiscoveredDeviceCreatesTheAdoptedRow(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	seedPlacementNode(t, s, ddNodeID)

	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{discovered(ddDeviceA, "relay-a", "uuid:roku:ecp:X1")}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}

	created, err := s.AdoptDiscoveredDevice(ctx, ddDeviceA)
	if err != nil {
		t.Fatalf("AdoptDiscoveredDevice: %v", err)
	}
	if !created {
		t.Fatal("AdoptDiscoveredDevice reported created=false on the first adopt")
	}

	res, ok, err := s.Get(ctx, store.KindAdoptedDevice, ddDeviceA)
	if err != nil || !ok {
		t.Fatalf("adopted row not found (ok=%v, err=%v) — adoption must write the row, not merely a flag", ok, err)
	}
	var row datamodel.Device
	if err := json.Unmarshal(res.Body, &row); err != nil {
		t.Fatalf("decode adopted row: %v", err)
	}
	// The id is the DISCOVERED id, not a freshly minted one: that convergence is
	// what stops re-adopting one physical unit producing a second device_id.
	if row.ID != ddDeviceA {
		t.Errorf("adopted row id = %q, want the discovered device's own %q", row.ID, ddDeviceA)
	}
	if row.Driver != "roku-ecp" || row.NativeID != "uuid:roku:ecp:X1" {
		t.Errorf("adopted identity = %q/%q, want the discovered tuple", row.Driver, row.NativeID)
	}
	if row.Name != "Lobby TV" {
		t.Errorf("adopted name = %q, want the device's self-reported %q", row.Name, "Lobby TV")
	}
	if len(row.Entities) != 1 {
		t.Fatalf("adopted entities = %d, want the one the sighting reported", len(row.Entities))
	}
	e := row.Entities[0]
	if !e.Enabled || e.Category != "primary" {
		t.Errorf("adopted entity = {enabled %v, category %q}, want {true, primary} — an adoption that arrives disabled looks adopted and does nothing", e.Enabled, e.Category)
	}
	if e.EntityID == "" || e.EntityID == "main" {
		t.Errorf("entity_id = %q, want a derived platform id rather than the relay's addressing key", e.EntityID)
	}

	// The adopted set is what the desired-state section is compiled from, so
	// the real proof is that it now carries this device.
	inv, err := s.DeviceInventory(ctx)
	if err != nil {
		t.Fatalf("DeviceInventory: %v", err)
	}
	if len(inv.Devices) != 1 {
		t.Errorf("device_inventory devices = %d, want 1 — adoption that never reaches desired state controls nothing", len(inv.Devices))
	}
}

// TestAdoptDiscoveredDeviceIsIdempotent: an operator double-click, or a retry
// after a timeout, must not be an error.
func TestAdoptDiscoveredDeviceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	seedPlacementNode(t, s, ddNodeID)
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{discovered(ddDeviceA, "relay-a", "uuid:roku:ecp:X1")}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if _, err := s.AdoptDiscoveredDevice(ctx, ddDeviceA); err != nil {
		t.Fatalf("first adopt: %v", err)
	}

	created, err := s.AdoptDiscoveredDevice(ctx, ddDeviceA)
	if err != nil {
		t.Fatalf("second adopt: %v, want success", err)
	}
	if created {
		t.Error("second adopt reported created=true, want false — the row already existed")
	}

	rows, err := s.List(ctx, store.KindAdoptedDevice, store.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("adopted rows = %d after two adopts, want 1", len(rows))
	}
}

// TestAdoptUnknownDeviceIsRefused: an adoption record is signed desired state
// shipped to the LAN edge, so it may never be filed on a caller's word about a
// device nobody has reported.
func TestAdoptUnknownDeviceIsRefused(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	seedPlacementNode(t, s, ddNodeID)

	if _, err := s.AdoptDiscoveredDevice(ctx, ddDeviceA); !errors.Is(err, store.ErrDiscoveredDeviceUnknown) {
		t.Errorf("AdoptDiscoveredDevice(unseen) error = %v, want ErrDiscoveredDeviceUnknown", err)
	}
	if _, err := s.AdoptDiscoveredDevice(ctx, ""); err == nil {
		t.Error("AdoptDiscoveredDevice(\"\") = nil error, want a refusal")
	}
}

// TestAdoptDiscoveredDeviceRefusesADuplicateIdentity proves the adopt path is
// held to the SAME identity rule every authored device row is: two physical
// records claiming one (driver, native_id) is a validation failure, surfaced as
// such rather than silently creating a second device_id for one device.
func TestAdoptDiscoveredDeviceRefusesADuplicateIdentity(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	seedPlacementNode(t, s, ddNodeID)

	// An adopted row already claims the tuple, under a DIFFERENT device id.
	if _, err := s.Create(ctx, store.KindAdoptedDevice, mustJSON(t, datamodel.Device{
		ID: ddDeviceB, ScopeNode: ddNodeID, Name: "Already Adopted",
		Driver: "roku-ecp", NativeID: "uuid:roku:ecp:X1",
		Entities: []datamodel.DeviceEntity{},
	})); err != nil {
		t.Fatalf("seed the incumbent adopted row: %v", err)
	}
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{discovered(ddDeviceA, "relay-a", "uuid:roku:ecp:X1")}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}

	_, err := s.AdoptDiscoveredDevice(ctx, ddDeviceA)
	var verr *store.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("AdoptDiscoveredDevice error = %v, want a *store.ValidationError", err)
	}
	if _, ok, _ := s.Get(ctx, store.KindAdoptedDevice, ddDeviceA); ok {
		t.Error("a refused adopt still wrote a row")
	}
}
