package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// deviceinventory_test.go covers the store's `device_inventory` derivation
// (relay/1 REL-063/064) through the store's OWN write path: rows are authored
// with store.Create / packs with store.InstallPack, and the section is read back
// through Store.DeviceInventory — never assembled by the test.

const (
	invDeviceLobbyID   = "01J8Z9DEVCE1NV0BBYAAAAAAA1"
	invDeviceAtriumID  = "01J8ZADEVCE1NVATR1VMBBBBB2"
	invEntityPlayerID  = "01J8Z9ENTTY1NVP1AYERAAAAA1"
	invEntityDiagID    = "01J8Z9ENTTY1NVD1AGN0SEBB22"
	invEntityAtriumID  = "01J8ZAENTTY1NVATR1VMCCCCC3"
	invLobbyNativeID   = "10.0.0.41"
	invAtriumNativeID  = "10.0.0.42"
	invRokuPackID      = "acme/roku"
	invPrinterPackID   = "acme/printer"
	invPatternRoku     = `{"deviceClass":"media-player","match":[{"ssdp":"urn:roku-com:device:player:1"}]}`
	invPatternPrinter  = `{"deviceClass":"printer","match":[{"mdns":"_ipp._tcp"},{"macOui":"AABBCC"}]}`
	invPollCadenceSecs = 10
)

// invDeviceRow is a fully-authored adopted-device row: the five REL-063 members
// PLUS the api/1 resource baseline (name, scope_node, external_id, labels) the
// projection must NOT carry to the relay, and a two-entity policy exercising
// both categories and both hidden states.
func invDeviceRow(id, nativeID string, cadence *int, entityIDs ...string) datamodel.Device {
	if len(entityIDs) == 0 {
		entityIDs = []string{invEntityPlayerID, invEntityDiagID}
	}
	return datamodel.Device{
		ID:                 id,
		ScopeNode:          screenNodeID,
		Name:               "Lobby Roku (operator label)",
		Driver:             "roku-ecp",
		NativeID:           nativeID,
		PollCadenceSeconds: cadence,
		ExternalID:         "inventory-fixture-" + nativeID,
		Labels:             map[string]string{"floor": "1"},
		Entities: []datamodel.DeviceEntity{
			{
				EntityID:    entityIDs[0],
				DeviceClass: "media-player",
				Enabled:     true,
				Hidden:      false,
				DisplayName: "Lobby TV",
				Category:    "primary",
			},
			{
				EntityID:    entityIDs[1],
				DeviceClass: "sensor",
				Enabled:     false,
				Hidden:      true,
				DisplayName: "Lobby TV signal strength",
				Category:    "diagnostic",
			},
		},
	}
}

// invSecondDeviceRow is invDeviceRow for the atrium device, with its own entity
// ids so two adopted rows never claim one entity.
func invSecondDeviceRow() datamodel.Device {
	d := invDeviceRow(invDeviceAtriumID, invAtriumNativeID, nil, invEntityAtriumID, "01J8ZAENTTY1NVD1AGN0SECC33")
	d.Name = "Atrium Roku"
	return d
}

// invPackWithDevices is a pack whose manifest declares a `devices` contribution
// (manifest/1 MAN-070/071). `capabilities` is present in the manifest and must
// NOT appear in the emitted pattern: REL-064 asks for the discovery-match
// patterns, and a requested capability grant is not one.
func invPackWithDevices(id, devicesJSON string) store.PackInstall {
	return store.PackInstall{
		ID:               id,
		Version:          "1.0.0",
		DataModelVersion: 1,
		Manifest:         json.RawMessage(`{"id":"` + id + `","version":"1.0.0","devices":` + devicesJSON + `}`),
		// Every install carries its provenance record (MKT-094a); this fixture
		// uses the direct-upload shape (see packSpec).
		Record: store.PackInstallRecord{
			Source:        store.SourceDirect,
			ContentDigest: "sha256:" + fixtureDigestHex,
			KeyID:         "ed25519:fixture",
			VerifyingKey:  "sha256:0000000000000000000000000000000000000000000000000000000000000001",
		},
	}
}

const (
	invRokuDevicesJSON = `[{"deviceClass":"media-player","match":[{"ssdp":"urn:roku-com:device:player:1"}],"capabilities":["power","launch"]}]`
	// A second class, two match forms, to prove the whole declared pattern list
	// travels rather than just its first entry.
	invPrinterDevicesJSON = `[{"deviceClass":"printer","match":[{"mdns":"_ipp._tcp"},{"macOui":"AABBCC"}],"capabilities":["status"]}]`
)

// readInventory reads the section through the store's public accessor.
func readInventory(t *testing.T, s *store.Store) (devices []string, patterns []string) {
	t.Helper()
	inv, err := s.DeviceInventory(context.Background())
	if err != nil {
		t.Fatalf("DeviceInventory: %v", err)
	}
	for _, d := range inv.Devices {
		devices = append(devices, string(d))
	}
	for _, p := range inv.PackMatchPatterns {
		patterns = append(patterns, string(p))
	}
	return devices, patterns
}

// TestDeviceInventoryProjectsExactlyTheContractMembers pins REL-063: an adopted
// row reaches the section as the five contract members and nothing else, with
// each entity's authored enabled/hidden/display_name/category policy intact —
// and with the api/1 resource baseline the row also carries (name, scope_node,
// external_id, labels, revision, timestamps) deliberately left behind.
func TestDeviceInventoryProjectsExactlyTheContractMembers(t *testing.T) {
	s := openMem(t)
	seedPlacementNode(t, s, screenNodeID)
	cadence := invPollCadenceSecs
	if _, err := s.Create(context.Background(), store.KindAdoptedDevice,
		mustJSON(t, invDeviceRow(invDeviceLobbyID, invLobbyNativeID, &cadence))); err != nil {
		t.Fatalf("create adopted device: %v", err)
	}

	devices, _ := readInventory(t, s)
	if len(devices) != 1 {
		t.Fatalf("device_inventory.devices = %v, want exactly the one adopted row", devices)
	}

	// The whole entry, spelled out: the contract's five members in the
	// contract's order, each entity carrying its six.
	want := `{"device_id":"` + invDeviceLobbyID + `",` +
		`"driver":"roku-ecp","native_id":"` + invLobbyNativeID + `",` +
		`"poll_cadence_seconds":10,` +
		`"entities":[` +
		`{"entity_id":"` + invEntityPlayerID + `","device_class":"media-player","enabled":true,"hidden":false,"display_name":"Lobby TV","category":"primary"},` +
		`{"entity_id":"` + invEntityDiagID + `","device_class":"sensor","enabled":false,"hidden":true,"display_name":"Lobby TV signal strength","category":"diagnostic"}` +
		`]}`
	if devices[0] != want {
		t.Errorf("device_inventory entry =\n  %s\nwant\n  %s", devices[0], want)
	}
}

// TestDeviceInventoryPollCadenceNullWhenUnstated pins REL-063's "no default":
// a row that states no cadence carries an explicit null, never a fabricated 0
// (which a relay would read as "poll continuously") and never an absent key.
func TestDeviceInventoryPollCadenceNullWhenUnstated(t *testing.T) {
	s := openMem(t)
	seedPlacementNode(t, s, screenNodeID)
	if _, err := s.Create(context.Background(), store.KindAdoptedDevice,
		mustJSON(t, invDeviceRow(invDeviceLobbyID, invLobbyNativeID, nil))); err != nil {
		t.Fatalf("create adopted device: %v", err)
	}

	devices, _ := readInventory(t, s)
	if len(devices) != 1 {
		t.Fatalf("device_inventory.devices = %v, want one entry", devices)
	}
	var entry map[string]json.RawMessage
	if err := json.Unmarshal([]byte(devices[0]), &entry); err != nil {
		t.Fatalf("entry is not an object: %v", err)
	}
	got, present := entry["poll_cadence_seconds"]
	if !present {
		t.Fatalf("entry omits poll_cadence_seconds entirely: %s", devices[0])
	}
	if string(got) != "null" {
		t.Errorf("poll_cadence_seconds = %s, want null (REL-063 fixes no default)", got)
	}
}

// TestDeviceInventoryEmptyStoreCarriesEmptyArrays pins REL-060's no-null rule for
// this section: a store with nothing adopted and nothing installed still carries
// both arrays, each `[]`.
func TestDeviceInventoryEmptyStoreCarriesEmptyArrays(t *testing.T) {
	s := openMem(t)
	inv, err := s.DeviceInventory(context.Background())
	if err != nil {
		t.Fatalf("DeviceInventory: %v", err)
	}
	raw, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal section: %v", err)
	}
	if string(raw) != `{"devices":[],"pack_match_patterns":[]}` {
		t.Errorf("empty device_inventory = %s, want both arrays present and empty", raw)
	}
}

// TestPackMatchPatternsComeFromInstalledPacks pins REL-064's source: the
// patterns are every INSTALLED PACK's declared device contribution, reduced to
// {deviceClass, match} — the pack's requested `capabilities` is not a discovery
// pattern and does not travel. Uninstalling the pack removes its patterns.
func TestPackMatchPatternsComeFromInstalledPacks(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if _, patterns := readInventory(t, s); len(patterns) != 0 {
		t.Fatalf("pack_match_patterns = %v before any pack is installed, want none", patterns)
	}

	pack, _, err := s.InstallPack(ctx, invPackWithDevices(invRokuPackID, invRokuDevicesJSON))
	if err != nil {
		t.Fatalf("InstallPack: %v", err)
	}
	_, patterns := readInventory(t, s)
	if len(patterns) != 1 || patterns[0] != invPatternRoku {
		t.Fatalf("pack_match_patterns = %v, want exactly [%s]", patterns, invPatternRoku)
	}

	// A second pack contributes a second class, and both of its declared match
	// forms travel.
	if _, _, err := s.InstallPack(ctx, invPackWithDevices(invPrinterPackID, invPrinterDevicesJSON)); err != nil {
		t.Fatalf("InstallPack #2: %v", err)
	}
	_, patterns = readInventory(t, s)
	// Pack ids order as acme/printer < acme/roku.
	if len(patterns) != 2 || patterns[0] != invPatternPrinter || patterns[1] != invPatternRoku {
		t.Fatalf("pack_match_patterns = %v, want [%s %s]", patterns, invPatternPrinter, invPatternRoku)
	}

	if err := s.UninstallPack(ctx, pack.ID, pack.Revision); err != nil {
		t.Fatalf("UninstallPack: %v", err)
	}
	_, patterns = readInventory(t, s)
	if len(patterns) != 1 || patterns[0] != invPatternPrinter {
		t.Fatalf("after uninstalling %s, pack_match_patterns = %v, want only [%s]", invRokuPackID, patterns, invPatternPrinter)
	}
}

// TestPackMatchPatternsAreIndependentOfTheAdoptedSet pins the clause REL-064
// spends its second half on: the patterns are the ones a relay watches for
// "independent of what is already adopted".
//
// Both directions are checked, because each failure mode is real and neither is
// visible from the other side:
//
//   - Adopting a device of a class no installed pack declares MUST NOT invent a
//     pattern (patterns are a pack's declaration, not an inference from a row).
//   - Removing the last adopted device of a declared class MUST NOT withdraw
//     that class's pattern — otherwise a relay would go blind to the very class
//     it needs to discover the replacement, and no first device of any class
//     could ever be found.
func TestPackMatchPatternsAreIndependentOfTheAdoptedSet(t *testing.T) {
	s := openMem(t)
	seedPlacementNode(t, s, screenNodeID)
	ctx := context.Background()

	// A device adopted with NO pack installed: the section carries the device
	// and still declares no pattern.
	dev, err := s.Create(ctx, store.KindAdoptedDevice,
		mustJSON(t, invDeviceRow(invDeviceLobbyID, invLobbyNativeID, nil)))
	if err != nil {
		t.Fatalf("create adopted device: %v", err)
	}
	devices, patterns := readInventory(t, s)
	if len(devices) != 1 {
		t.Fatalf("devices = %v, want the adopted row", devices)
	}
	if len(patterns) != 0 {
		t.Fatalf("adopting a media-player with no pack installed produced patterns %v; "+
			"a pattern is a pack's declaration, never inferred from an adopted row (REL-064)", patterns)
	}

	// Install the pack that declares media-player: NOW there is a pattern, and
	// it came from the manifest, not from the row.
	if _, _, err := s.InstallPack(ctx, invPackWithDevices(invRokuPackID, invRokuDevicesJSON)); err != nil {
		t.Fatalf("InstallPack: %v", err)
	}
	if _, patterns = readInventory(t, s); len(patterns) != 1 || patterns[0] != invPatternRoku {
		t.Fatalf("pack_match_patterns = %v, want [%s]", patterns, invPatternRoku)
	}

	// Adopting a SECOND device of the same class must not duplicate the pattern.
	if _, err := s.Create(ctx, store.KindAdoptedDevice, mustJSON(t, invSecondDeviceRow())); err != nil {
		t.Fatalf("create second adopted device: %v", err)
	}
	devices, patterns = readInventory(t, s)
	if len(devices) != 2 {
		t.Fatalf("devices = %d, want 2", len(devices))
	}
	if len(patterns) != 1 || patterns[0] != invPatternRoku {
		t.Fatalf("a second adopted device changed pack_match_patterns to %v, want [%s]", patterns, invPatternRoku)
	}

	// Un-adopt every device of that class: the pattern MUST survive.
	cur, found, err := s.Get(ctx, store.KindAdoptedDevice, invDeviceAtriumID)
	if err != nil || !found {
		t.Fatalf("get second device: found=%v err=%v", found, err)
	}
	if err := s.Delete(ctx, store.KindAdoptedDevice, cur.ID, cur.Revision); err != nil {
		t.Fatalf("delete second device: %v", err)
	}
	if err := s.Delete(ctx, store.KindAdoptedDevice, dev.ID, dev.Revision); err != nil {
		t.Fatalf("delete first device: %v", err)
	}
	devices, patterns = readInventory(t, s)
	if len(devices) != 0 {
		t.Fatalf("devices = %v after un-adopting both, want none", devices)
	}
	if len(patterns) != 1 || patterns[0] != invPatternRoku {
		t.Fatalf("un-adopting the last media-player withdrew its pattern (%v); REL-064's patterns are "+
			"watched independent of what is adopted, or the class could never be discovered again", patterns)
	}
}

// TestPackMatchPatternsCollapseIdenticalContributions: two packs declaring the
// byte-identical contribution give a relay one thing to watch for, not two — so
// the section (and therefore the snapshot hash) does not depend on how many packs
// happen to say the same thing.
func TestPackMatchPatternsCollapseIdenticalContributions(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	for _, id := range []string{invRokuPackID, "zeta/roku-too"} {
		if _, _, err := s.InstallPack(ctx, invPackWithDevices(id, invRokuDevicesJSON)); err != nil {
			t.Fatalf("InstallPack %s: %v", id, err)
		}
	}
	if _, patterns := readInventory(t, s); len(patterns) != 1 || patterns[0] != invPatternRoku {
		t.Fatalf("pack_match_patterns = %v, want the single collapsed [%s]", patterns, invPatternRoku)
	}
}

// TestPackContributionWithNoMatchPatternsContributesNothing: a contribution that
// names a class but declares no way to ever match one is not a pattern a relay
// can watch for, so it is not emitted.
func TestPackContributionWithNoMatchPatternsContributesNothing(t *testing.T) {
	s := openMem(t)
	if _, _, err := s.InstallPack(context.Background(),
		invPackWithDevices(invRokuPackID, `[{"deviceClass":"media-player","match":[],"capabilities":["power"]}]`)); err != nil {
		t.Fatalf("InstallPack: %v", err)
	}
	if _, patterns := readInventory(t, s); len(patterns) != 0 {
		t.Fatalf("pack_match_patterns = %v, want none from a contribution declaring no match pattern", patterns)
	}
}

// TestDeviceInventoryIsStableAcrossReads: with nothing written between them, two
// reads produce byte-identical section bytes. That stability is what keeps an
// unchanged adopted set from churning the snapshot hash on every rebuild
// (REL-053) — a derivation with a nondeterministic row order would re-hash
// differently each time even though nothing had been authored.
func TestDeviceInventoryIsStableAcrossReads(t *testing.T) {
	s := openMem(t)
	seedPlacementNode(t, s, screenNodeID)
	ctx := context.Background()

	// The atrium row is written FIRST but sorts SECOND by id, so a derivation
	// that leaked insertion order would order these the other way round.
	for _, row := range []datamodel.Device{
		invSecondDeviceRow(),
		invDeviceRow(invDeviceLobbyID, invLobbyNativeID, nil),
	} {
		if _, err := s.Create(ctx, store.KindAdoptedDevice, mustJSON(t, row)); err != nil {
			t.Fatalf("create %s: %v", row.ID, err)
		}
	}
	if _, _, err := s.InstallPack(ctx, invPackWithDevices(invRokuPackID, invRokuDevicesJSON)); err != nil {
		t.Fatalf("InstallPack: %v", err)
	}

	first, err := s.DeviceInventory(ctx)
	if err != nil {
		t.Fatalf("DeviceInventory #1: %v", err)
	}
	second, err := s.DeviceInventory(ctx)
	if err != nil {
		t.Fatalf("DeviceInventory #2: %v", err)
	}
	a, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal #1: %v", err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal #2: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("two reads of an unchanged store differ:\n  %s\n  %s", a, b)
	}

	// Row-id order, not insertion order: the lobby row was written second but
	// sorts first, so a derivation that leaked insertion order would fail here.
	var sec struct {
		Devices []struct {
			DeviceID string `json:"device_id"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(a, &sec); err != nil {
		t.Fatalf("decode section: %v", err)
	}
	if len(sec.Devices) != 2 || sec.Devices[0].DeviceID != invDeviceLobbyID || sec.Devices[1].DeviceID != invDeviceAtriumID {
		t.Fatalf("devices order = %+v, want %s then %s (row-id order)", sec.Devices, invDeviceLobbyID, invDeviceAtriumID)
	}
}
