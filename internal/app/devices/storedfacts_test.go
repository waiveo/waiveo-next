package devices

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// storedfacts_test.go covers the fourth thing held BESIDE a relay's view: what
// the durable mirror committed for a device's learned facts.
//
// It exists because the mirror's merge learned to refuse part of a report — the
// generic class a restarted relay mints, the bare address a lane that cannot see
// ports reports — and a refusal only the mirror knows about is a refusal the API
// does not honour. The projection is MarkSeen's shape and these are MarkSeen's
// rules, one layer over.

func storedTestDevice(t *testing.T, r *Registry, relayID, nativeID string) Device {
	t.Helper()
	d, ok := r.Device(deviceid.Device(testSite, "roku-ecp", nativeID))
	if !ok {
		t.Fatalf("device %s/%s is not in the registry", relayID, nativeID)
	}
	return d
}

// The point of the whole map: a report that is correct but IGNORANT — a relay
// whose in-memory store is empty after a restart — must not undo what the mirror
// remembers.
func TestTheMirrorsCommittedFactsOverrideAnIgnorantReport(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	c := candidate("roku-ecp", "X1")
	c.Name, c.Address, c.Model = "Lobby TV", "192.168.50.31:8060", "Roku Ultra"
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	id := deviceid.Device(testSite, "roku-ecp", "X1")

	// The mirror commits the merged answer.
	r.MarkStored(map[string]Stored{id: {
		RelayID: relayA, DeviceClass: classMP, Name: "Lobby TV",
		Address: "192.168.50.31:8060", Model: "Roku Ultra",
	}})

	// The relay restarts and reports what its neighbour lane can see.
	ignorant := candidate("roku-ecp", "X1")
	ignorant.DeviceClass, ignorant.Name, ignorant.Address = "unclassified", "", "192.168.50.31"
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{ignorant}); err != nil {
		t.Fatalf("ApplyCandidates (ignorant): %v", err)
	}

	d := storedTestDevice(t, r, relayA, "X1")
	if d.DeviceClass != classMP || d.Address != "192.168.50.31:8060" || d.Name != "Lobby TV" || d.Model != "Roku Ultra" {
		t.Fatalf("row = {class %q address %q name %q model %q}, want the mirror's committed answer — "+
			"a refusal only the durable layer knows about is one the API does not honour",
			d.DeviceClass, d.Address, d.Name, d.Model)
	}

	// The entity label is composed from the device's name at intake, so it has to
	// follow an overlaid name or one API response names one device two ways.
	e, ok := r.Entity(deviceid.Entity(testSite, "roku-ecp", "X1", entityKey))
	if !ok || e.Name != "Lobby TV "+entityKey {
		t.Fatalf("entity name = %q (found %v), want %q", e.Name, ok, "Lobby TV "+entityKey)
	}
}

// ...and it is an overlay, not a freeze. A report that genuinely LEARNS
// something newer still lands, because the mirror will commit that newer answer
// on the same round trip.
func TestARealChangeStillLandsThroughTheOverlay(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	c := candidate("roku-ecp", "X1")
	c.Name, c.Address = "Lobby TV", "192.168.50.31:8060"
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	id := deviceid.Device(testSite, "roku-ecp", "X1")
	r.MarkStored(map[string]Stored{id: {RelayID: relayA, DeviceClass: classMP, Name: "Lobby TV", Address: "192.168.50.31:8060"}})

	// The device is renamed and its DHCP lease moves; the mirror commits both.
	moved := candidate("roku-ecp", "X1")
	moved.Name, moved.Address = "Hangar Bay", "192.168.50.99:8060"
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{moved}); err != nil {
		t.Fatalf("ApplyCandidates (moved): %v", err)
	}
	r.MarkStored(map[string]Stored{id: {RelayID: relayA, DeviceClass: classMP, Name: "Hangar Bay", Address: "192.168.50.99:8060"}})

	d := storedTestDevice(t, r, relayA, "X1")
	if d.Name != "Hangar Bay" || d.Address != "192.168.50.99:8060" {
		t.Fatalf("row = {name %q address %q}, want the newly committed answer — an overlay that could not change would be a permanently stale device list", d.Name, d.Address)
	}
}

// A device id is derived from (site, driver, native_id) and is therefore
// guessable by any enrolled relay; REL-153 incumbency decides which relay speaks
// for it, and it can hand a device over. An overlay taken under one relay must
// never describe another relay's row.
func TestAnOverlayDoesNotFollowADeviceToAnotherRelay(t *testing.T) {
	nowMs := int64(0)
	r := New(testSite, func() int64 { return nowMs })
	c := candidate("roku-ecp", "X1")
	c.Name, c.Address = "Lobby TV", "192.168.50.31:8060"
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	id := deviceid.Device(testSite, "roku-ecp", "X1")
	r.MarkStored(map[string]Stored{id: {RelayID: relayA, Name: "Lobby TV", Address: "192.168.50.31:8060"}})

	// A relays goes silent long enough to lose incumbency, and relay B takes the
	// device over with its own reading of it.
	nowMs += IncumbencyWindowMs + 1
	takeover := candidate("roku-ecp", "X1")
	takeover.Name, takeover.Address = "Cafe TV", "10.0.0.5:8060"
	if err := r.ApplyCandidates(relayB, []wire.DeviceCandidate{takeover}); err != nil {
		t.Fatalf("ApplyCandidates (relay B): %v", err)
	}

	d := storedTestDevice(t, r, relayB, "X1")
	if d.RelayID != relayB {
		t.Fatalf("device is still routed by %s, want %s — the fixture did not exercise a handover", d.RelayID, relayB)
	}
	if d.Name != "Cafe TV" || d.Address != "10.0.0.5:8060" {
		t.Fatalf("row = {name %q address %q}, want relay B's own reading — an overlay committed under relay A must not describe a device relay A no longer routes", d.Name, d.Address)
	}
}

// Forget is revocation's read-model half, and the overlay goes with the view: it
// is the revoked relay's description of the site, and the caller is deleting its
// durable rows in the same breath.
func TestForgetDropsTheOverlayWithTheView(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	c := candidate("roku-ecp", "X1")
	c.Name = "Lobby TV"
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	id := deviceid.Device(testSite, "roku-ecp", "X1")
	r.MarkStored(map[string]Stored{id: {RelayID: relayA, Name: "Lobby TV", Address: "192.168.50.31:8060"}})

	r.Forget(relayA)
	if len(r.Devices()) != 0 {
		t.Fatalf("Forget left %d device(s)", len(r.Devices()))
	}

	// The same relay id enrolls again and reports the device with nothing learned
	// about it yet. The revoked description must not come back.
	fresh := candidate("roku-ecp", "X1")
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{fresh}); err != nil {
		t.Fatalf("ApplyCandidates (after forget): %v", err)
	}
	if d := storedTestDevice(t, r, relayA, "X1"); d.Address == "192.168.50.31:8060" {
		t.Fatalf("address = %q after the relay was forgotten and re-enrolled — a revoked relay's remembered description must not be stamped back on", d.Address)
	}
}

// The mirror commits THREE answers about ports, and the projection must carry
// all three. This guard read `len(s.OpenPorts) > 0`, which folds the third into
// the first: a mirror that committed "a scan looked and nothing is open" could
// not say so, and the API reported "nobody has looked" instead.
//
// It only bites after a restart, which is exactly this file's subject — a relay
// whose in-memory candidate store is empty re-reports passively with no ports at
// all, so every port an operator sees comes from the mirror. Measured on the dev
// box: 24 of 63 devices silently reverted to unscanned, one restart after the
// scan that had cleared them.

func TestTheMirrorsEMPTYPortListReachesTheAPI(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	// An ignorant report: the restarted relay carries no ports at all.
	c := candidate("roku-ecp", "X1")
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	id := deviceid.Device(testSite, "roku-ecp", "X1")

	// The mirror knows a scan looked and found nothing open.
	r.MarkStored(map[string]Stored{id: {RelayID: relayA, OpenPorts: []int{}}})

	d := storedTestDevice(t, r, relayA, "X1")
	if d.OpenPorts == nil {
		t.Fatal("the mirror's committed empty list did not reach the device; the API now says nobody has looked")
	}
	if len(d.OpenPorts) != 0 {
		t.Fatalf("want an empty list, got %#v", d.OpenPorts)
	}
}

func TestAMirrorThatKNOWSNothingLeavesThePortsAbsent(t *testing.T) {
	// The other side of the same nil check: a mirror holding nothing about a
	// device must not be turned into a claim that a scan found nothing.
	r := New(testSite, func() int64 { return 0 })
	c := candidate("roku-ecp", "X1")
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	id := deviceid.Device(testSite, "roku-ecp", "X1")
	r.MarkStored(map[string]Stored{id: {RelayID: relayA}})

	if d := storedTestDevice(t, r, relayA, "X1"); d.OpenPorts != nil {
		t.Fatalf("a mirror that knows nothing produced %#v, want absent", d.OpenPorts)
	}
}

func TestTheMirrorsFINDINGSStillOverrideAnIgnorantReport(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	c := candidate("roku-ecp", "X1")
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	id := deviceid.Device(testSite, "roku-ecp", "X1")
	r.MarkStored(map[string]Stored{id: {RelayID: relayA, OpenPorts: []int{22, 8060}}})

	if d := storedTestDevice(t, r, relayA, "X1"); len(d.OpenPorts) != 2 {
		t.Fatalf("the mirror's findings did not survive an ignorant report: %#v", d.OpenPorts)
	}
}

// MAC and vendor are published as FACTS, not as a name fragment. Both were
// already known: candidateName has been reading the vendor out of the MAC all
// along and spending it on a fallback name, which means it reached an operator
// only for a device that could not name itself — 12 of 63 on the box, including
// both machines called "NAS", had a vendor the platform knew and no surface
// that said it.

func TestAVendorIsPublishedEvenWhenTheDeviceNamedItself(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	c := candidate("net", "bc:24:11:3f:b9:4d")
	c.Name = "NAS" // self-named, so the fallback never runs
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	d, ok := r.Device(deviceid.Device(testSite, "net", "bc:24:11:3f:b9:4d"))
	if !ok {
		t.Fatal("device missing")
	}
	if d.Name != "NAS" {
		t.Fatalf("the device's own name was overwritten: %q", d.Name)
	}
	if d.Vendor != "Proxmox" {
		t.Errorf("Vendor = %q, want Proxmox — the fact was known and reached nobody", d.Vendor)
	}
	if d.MAC != "bc:24:11:3f:b9:4d" {
		t.Errorf("MAC = %q", d.MAC)
	}
}

func TestANativeIDThatIsNotAnAddressPublishesNoMAC(t *testing.T) {
	// `native_id` is driver-specific. A device a protocol lane named carries
	// that protocol's id, and inventing a MAC from it would be a fabricated
	// identifier for hardware nobody saw at layer 2.
	r := New(testSite, func() int64 { return 0 })
	c := candidate("roku-ecp", "X01500ABCDEF")
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	d, ok := r.Device(deviceid.Device(testSite, "roku-ecp", "X01500ABCDEF"))
	if !ok {
		t.Fatal("device missing")
	}
	if d.MAC != "" {
		t.Errorf("MAC = %q, want empty for a non-address native_id", d.MAC)
	}
	if d.Vendor != "" {
		t.Errorf("Vendor = %q, want empty", d.Vendor)
	}
}

func TestTheMACIsPublishedInOneSpelling(t *testing.T) {
	// Two rows spelling one address differently read as two devices.
	r := New(testSite, func() int64 { return 0 })
	c := candidate("net", "BC-24-11-3F-B9-4D")
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	d, ok := r.Device(deviceid.Device(testSite, "net", "BC-24-11-3F-B9-4D"))
	if !ok {
		t.Fatal("device missing")
	}
	if d.MAC != "bc:24:11:3f:b9:4d" {
		t.Errorf("MAC = %q, want the canonical spelling", d.MAC)
	}
}
