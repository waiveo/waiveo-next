package snapshot

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// deviceinventory_test.go drives the `device_inventory` section (relay/1
// REL-063/064) END TO END: adopted-device rows and packs are authored through
// the real store write path, and the section is read back off the SIGNED
// snapshot the feeder builds — never assembled by the test.

const (
	invScopeNodeID    = "01J8Z4DEM0SCREENF1RSTPH0TN"
	invDeviceLobbyID  = "01J8Z9DEVCE1NV0BBYAAAAAAA1"
	invDeviceAtriumID = "01J8ZADEVCE1NVATR1VMBBBBB2"
	invEntityPlayerID = "01J8Z9ENTTY1NVP1AYERAAAAA1"
	invEntityDiagID   = "01J8Z9ENTTY1NVD1AGN0SEBB22"
	invEntityAtriumID = "01J8ZAENTTY1NVATR1VMCCCCC3"
	invEntityAtrDiag  = "01J8ZAENTTY1NVD1AGN0SECC33"

	invRokuPackID    = "acme/roku"
	invPrinterPackID = "acme/printer"

	invPatternRoku    = `{"deviceClass":"media-player","match":[{"ssdp":"urn:roku-com:device:player:1"}]}`
	invPatternPrinter = `{"deviceClass":"printer","match":[{"mdns":"_ipp._tcp"}]}`

	invRokuDevicesJSON    = `[{"deviceClass":"media-player","match":[{"ssdp":"urn:roku-com:device:player:1"}],"capabilities":["power","launch"]}]`
	invPrinterDevicesJSON = `[{"deviceClass":"printer","match":[{"mdns":"_ipp._tcp"}],"capabilities":["status"]}]`
)

// invAdoptedDevice is an authored adopted-device row carrying BOTH the five
// REL-063 members and the api/1 resource baseline around them, with a two-entity
// policy that exercises every per-entity decision the section carries: enabled
// vs not, hidden vs not, a display name, and both `category` values.
func invAdoptedDevice(id, nativeID string, cadence *int, entA, entB string) datamodel.Device {
	return datamodel.Device{
		ID:                 id,
		ScopeNode:          invScopeNodeID,
		Name:               "Operator's own label",
		Driver:             "roku-ecp",
		NativeID:           nativeID,
		PollCadenceSeconds: cadence,
		ExternalID:         "inventory-fixture-" + nativeID,
		Labels:             map[string]string{"floor": "1"},
		Entities: []datamodel.DeviceEntity{
			{EntityID: entA, DeviceClass: "media-player", Enabled: true, Hidden: false, DisplayName: "Lobby TV", Category: "primary"},
			{EntityID: entB, DeviceClass: "sensor", Enabled: false, Hidden: true, DisplayName: "Lobby TV signal", Category: "diagnostic"},
		},
	}
}

// invPack is a pack whose manifest declares a MAN-070/071 `devices`
// contribution.
func invPack(id, devicesJSON string) store.PackInstall {
	return store.PackInstall{
		ID:               id,
		Version:          "1.0.0",
		DataModelVersion: 1,
		Manifest:         json.RawMessage(`{"id":"` + id + `","version":"1.0.0","devices":` + devicesJSON + `}`),
	}
}

// invAdopt writes one adopted-device row through the store's real write path.
func invAdopt(t *testing.T, s *store.Store, d datamodel.Device) store.Resource {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal device row: %v", err)
	}
	res, err := s.Create(context.Background(), store.KindAdoptedDevice, b)
	if err != nil {
		t.Fatalf("adopt device %s: %v", d.ID, err)
	}
	return res
}

// invSectionStrings renders the built section's two arrays as plain strings.
func invSectionStrings(inv wire.DeviceInventory) (devices, patterns []string) {
	for _, d := range inv.Devices {
		devices = append(devices, string(d))
	}
	for _, p := range inv.PackMatchPatterns {
		patterns = append(patterns, string(p))
	}
	return devices, patterns
}

// TestBuildFromStoreCarriesAdoptedDeviceInventory is the end-to-end proof of
// REL-063: a device adopted through the api/1 resource family reaches the signed
// desired-state snapshot as a `device_inventory.devices` entry carrying its
// per-entity settings — and rides the same `hash`/`signature` every other
// section does, so it cannot be tampered with in transit.
func TestBuildFromStoreCarriesAdoptedDeviceInventory(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)
	s := seededStore(t, signhash.ContentID(img))
	ctx := context.Background()

	cadence := 10
	invAdopt(t, s, invAdoptedDevice(invDeviceLobbyID, "10.0.0.41", &cadence, invEntityPlayerID, invEntityDiagID))
	if _, _, err := s.InstallPack(ctx, invPack(invRokuPackID, invRokuDevicesJSON)); err != nil {
		t.Fatalf("InstallPack: %v", err)
	}

	ds, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	snap, _, err := BuildFromStore(ds, "https://origin.example", id, nil, contentInstant(t))
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}

	devices, patterns := invSectionStrings(snap.Sections.DeviceInventory)
	if len(devices) != 1 {
		t.Fatalf("device_inventory.devices = %v, want the one adopted row", devices)
	}
	// The entry, spelled out: the five REL-063 members in the contract's order,
	// each entity carrying its six — and none of the row's api/1 baseline.
	wantEntry := `{"device_id":"` + invDeviceLobbyID + `","driver":"roku-ecp","native_id":"10.0.0.41",` +
		`"poll_cadence_seconds":10,"entities":[` +
		`{"entity_id":"` + invEntityPlayerID + `","device_class":"media-player","enabled":true,"hidden":false,"display_name":"Lobby TV","category":"primary"},` +
		`{"entity_id":"` + invEntityDiagID + `","device_class":"sensor","enabled":false,"hidden":true,"display_name":"Lobby TV signal","category":"diagnostic"}` +
		`]}`
	if devices[0] != wantEntry {
		t.Errorf("device_inventory entry =\n  %s\nwant\n  %s", devices[0], wantEntry)
	}
	if len(patterns) != 1 || patterns[0] != invPatternRoku {
		t.Errorf("pack_match_patterns = %v, want [%s]", patterns, invPatternRoku)
	}

	// The section is inside the signed scope: recomputing `hash` over the
	// sections and verifying `signature` over {generation, hash} must both hold
	// (REL-053/075).
	recomputed, err := hashSections(snap.Sections)
	if err != nil {
		t.Fatalf("hashSections: %v", err)
	}
	if recomputed != snap.Hash {
		t.Errorf("recomputed hash %q != snapshot hash %q (REL-053)", recomputed, snap.Hash)
	}
	canon, err := generationHashCanonBytes(snap.Generation, snap.Hash)
	if err != nil {
		t.Fatalf("generationHashCanonBytes: %v", err)
	}
	sig, err := wire.DecodeSignature(snap.Signature)
	if err != nil {
		t.Fatalf("DecodeSignature: %v", err)
	}
	if !signhash.Verify(id.SigningPub(), canon, sig) {
		t.Error("signature did not verify over a snapshot carrying a real device inventory (REL-075)")
	}
}

// TestPackMatchPatternsTrackTheInstalledPacks pins REL-064's source through the
// built snapshot: what changes the patterns is installing or uninstalling a pack
// that declares a device contribution — never the adopted set, which REL-064
// requires the patterns be watched independent of.
//
// The independence direction is the one worth pinning hardest. If a pattern
// appeared only once a device of its class had been adopted, the FIRST device of
// every class could never be discovered, and un-adopting the last device of a
// class would blind the relay to the very class it needs to find the
// replacement.
func TestPackMatchPatternsTrackTheInstalledPacks(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)
	s := seededStore(t, signhash.ContentID(img))
	ctx := context.Background()

	build := func(what string) wire.DeviceInventory {
		t.Helper()
		ds, err := s.DesiredState(ctx)
		if err != nil {
			t.Fatalf("DesiredState (%s): %v", what, err)
		}
		snap, _, err := BuildFromStore(ds, "https://origin.example", id, nil, contentInstant(t))
		if err != nil {
			t.Fatalf("BuildFromStore (%s): %v", what, err)
		}
		return snap.Sections.DeviceInventory
	}

	// Nothing installed, nothing adopted.
	if _, patterns := invSectionStrings(build("empty")); len(patterns) != 0 {
		t.Fatalf("pack_match_patterns = %v on a store with no packs installed, want none", patterns)
	}

	// Adopting a media-player with no pack installed must not invent its
	// pattern: a pattern is a pack's declaration, not an inference from a row.
	dev := invAdopt(t, s, invAdoptedDevice(invDeviceLobbyID, "10.0.0.41", nil, invEntityPlayerID, invEntityDiagID))
	devices, patterns := invSectionStrings(build("adopted, no pack"))
	if len(devices) != 1 {
		t.Fatalf("devices = %v, want the adopted row", devices)
	}
	if len(patterns) != 0 {
		t.Fatalf("adopting a media-player with no pack installed produced patterns %v (REL-064: "+
			"the patterns are the INSTALLED PACKS' declarations)", patterns)
	}

	// Installing a pack changes the patterns.
	pack, _, err := s.InstallPack(ctx, invPack(invRokuPackID, invRokuDevicesJSON))
	if err != nil {
		t.Fatalf("InstallPack roku: %v", err)
	}
	if _, patterns = invSectionStrings(build("roku installed")); len(patterns) != 1 || patterns[0] != invPatternRoku {
		t.Fatalf("pack_match_patterns = %v after installing %s, want [%s]", patterns, invRokuPackID, invPatternRoku)
	}

	// Installing a SECOND pack changes them again — the derivation is the
	// installed set, not a fixed list.
	if _, _, err := s.InstallPack(ctx, invPack(invPrinterPackID, invPrinterDevicesJSON)); err != nil {
		t.Fatalf("InstallPack printer: %v", err)
	}
	// Pack ids order acme/printer < acme/roku.
	if _, patterns = invSectionStrings(build("both installed")); len(patterns) != 2 ||
		patterns[0] != invPatternPrinter || patterns[1] != invPatternRoku {
		t.Fatalf("pack_match_patterns = %v, want [%s %s]", patterns, invPatternPrinter, invPatternRoku)
	}

	// Changing the ADOPTED set — adding a second device, then removing every
	// adopted device — leaves the patterns exactly as they were.
	second := invAdopt(t, s, invAdoptedDevice(invDeviceAtriumID, "10.0.0.42", nil, invEntityAtriumID, invEntityAtrDiag))
	if _, patterns = invSectionStrings(build("second device adopted")); len(patterns) != 2 ||
		patterns[0] != invPatternPrinter || patterns[1] != invPatternRoku {
		t.Fatalf("adopting a second device changed pack_match_patterns to %v", patterns)
	}
	if err := s.Delete(ctx, store.KindAdoptedDevice, second.ID, second.Revision); err != nil {
		t.Fatalf("un-adopt second device: %v", err)
	}
	if err := s.Delete(ctx, store.KindAdoptedDevice, dev.ID, dev.Revision); err != nil {
		t.Fatalf("un-adopt first device: %v", err)
	}
	devices, patterns = invSectionStrings(build("nothing adopted"))
	if len(devices) != 0 {
		t.Fatalf("devices = %v after un-adopting both, want none", devices)
	}
	if len(patterns) != 2 || patterns[0] != invPatternPrinter || patterns[1] != invPatternRoku {
		t.Fatalf("un-adopting every device withdrew a pattern (%v); REL-064's patterns are watched "+
			"independent of what is adopted, or a class could never be discovered again", patterns)
	}

	// Uninstalling the pack is what withdraws its pattern.
	if err := s.UninstallPack(ctx, pack.ID, pack.Revision); err != nil {
		t.Fatalf("UninstallPack: %v", err)
	}
	if _, patterns = invSectionStrings(build("roku uninstalled")); len(patterns) != 1 || patterns[0] != invPatternPrinter {
		t.Fatalf("pack_match_patterns = %v after uninstalling %s, want only [%s]", patterns, invRokuPackID, invPatternPrinter)
	}
}

// TestUnchangedAdoptedSetDoesNotChurnTheGeneration: with nothing authored
// between two builds, the snapshot's generation AND its hash are unchanged, and
// the device_inventory section is byte-identical — so a relay re-pulling sees
// `state.unchanged` (REL-052) rather than a fresh generation to re-apply. The
// authored write at the end is the positive control: the same comparison DOES
// move when the adopted set actually changes, so the first half is not passing
// merely because nothing was ever wired up.
func TestUnchangedAdoptedSetDoesNotChurnTheGeneration(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)
	s := seededStore(t, signhash.ContentID(img))
	ctx := context.Background()

	cadence := 10
	invAdopt(t, s, invAdoptedDevice(invDeviceLobbyID, "10.0.0.41", &cadence, invEntityPlayerID, invEntityDiagID))
	if _, _, err := s.InstallPack(ctx, invPack(invRokuPackID, invRokuDevicesJSON)); err != nil {
		t.Fatalf("InstallPack: %v", err)
	}

	buildInv := func(what string) (SignedSnapshot, []byte) {
		t.Helper()
		ds, err := s.DesiredState(ctx)
		if err != nil {
			t.Fatalf("DesiredState (%s): %v", what, err)
		}
		snap, _, err := BuildFromStore(ds, "https://origin.example", id, nil, contentInstant(t))
		if err != nil {
			t.Fatalf("BuildFromStore (%s): %v", what, err)
		}
		raw, err := json.Marshal(snap.Sections.DeviceInventory)
		if err != nil {
			t.Fatalf("marshal device_inventory (%s): %v", what, err)
		}
		return snap, raw
	}

	first, firstInv := buildInv("first")
	second, secondInv := buildInv("second, nothing authored in between")

	if second.Generation != first.Generation {
		t.Errorf("generation moved from %d to %d with nothing authored in between", first.Generation, second.Generation)
	}
	if string(secondInv) != string(firstInv) {
		t.Errorf("device_inventory re-derived differently with nothing authored in between:\n  %s\n  %s", firstInv, secondInv)
	}
	if second.Hash != first.Hash {
		t.Errorf("snapshot hash moved from %q to %q with nothing authored in between (REL-053)", first.Hash, second.Hash)
	}

	// Positive control: authoring a NEW adopted device does move all three.
	invAdopt(t, s, invAdoptedDevice(invDeviceAtriumID, "10.0.0.42", nil, invEntityAtriumID, invEntityAtrDiag))
	third, thirdInv := buildInv("after adopting a second device")

	if !(third.Generation > second.Generation) {
		t.Fatalf("generation did not advance across an adoption: %d -> %d", second.Generation, third.Generation)
	}
	if string(thirdInv) == string(secondInv) {
		t.Fatalf("device_inventory did not change when a device was adopted: %s", thirdInv)
	}
	if third.Hash == second.Hash {
		t.Fatal("snapshot hash did not change when the adopted set changed")
	}
	if got := len(third.Sections.DeviceInventory.Devices); got != 2 {
		t.Fatalf("devices = %d after adopting a second device, want 2", got)
	}
}
