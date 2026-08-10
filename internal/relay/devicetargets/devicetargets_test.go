package devicetargets

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// fakeAddresses is a two-line AddressSource: the relay's discovered address
// book, keyed the way deviceplane.Store keys it.
type fakeAddresses map[string]string

func (f fakeAddresses) AddressFor(driver, nativeID string) (string, bool) {
	addr, ok := f[driver+"\x00"+nativeID]
	return addr, ok
}

func (f fakeAddresses) put(driver, nativeID, addr string) { f[driver+"\x00"+nativeID] = addr }

// inventory marshals a `device_inventory.devices` array the way a verified
// snapshot carries it: one raw JSON object per adopted device (REL-063).
func inventory(t *testing.T, entries ...wire.DeviceEntry) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(entries))
	for _, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshaling device entry: %v", err)
		}
		out = append(out, raw)
	}
	return out
}

// rokuEntry is one adopted Roku with a single enabled media-player entity.
func rokuEntry(deviceID, nativeID, entityID string, enabled bool) wire.DeviceEntry {
	return wire.DeviceEntry{
		DeviceID: deviceID,
		Driver:   "roku-ecp",
		NativeID: nativeID,
		Entities: []wire.DeviceEntity{{
			EntityID:    entityID,
			DeviceClass: "media-player",
			Enabled:     enabled,
			DisplayName: "Main",
			Category:    "primary",
		}},
	}
}

// TestAdoptedAndDiscoveredResolves is the happy path the whole package exists
// for: a device the app peer adopted AND this relay located is drivable, at the
// address the LOCATION header stated (port included, not defaulted).
func TestAdoptedAndDiscoveredResolves(t *testing.T) {
	addrs := fakeAddresses{}
	addrs.put("roku-ecp", "uuid:roku:ecp:AA", "http://192.168.50.31:8060/")
	r := New(nil, addrs)

	if n := r.SetInventory(inventory(t, rokuEntry("dev-1", "uuid:roku:ecp:AA", "ent-1", true))); n != 1 {
		t.Fatalf("SetInventory reported %d drivable entities, want 1", n)
	}

	ep, ok := r.Target("ent-1")
	if !ok {
		t.Fatal("Target(ent-1) = not ok; an adopted, enabled, located device must be drivable")
	}
	if ep.Host != "192.168.50.31" || ep.Port != 8060 {
		t.Fatalf("Target(ent-1) = %+v, want {192.168.50.31 8060}", ep)
	}

	deviceID, class, ok := r.ResolveEntity("ent-1")
	if !ok || deviceID != "dev-1" || class != "media-player" {
		t.Fatalf("ResolveEntity(ent-1) = (%q, %q, %v), want (dev-1, media-player, true)", deviceID, class, ok)
	}
}

// TestDiscoveredButUnadoptedIsRefused is the guardrail. A Roku the sweep found
// and nobody adopted must NOT be drivable: during coexistence the legacy stack
// still owns the screens that have not been migrated, and two controllers
// driving one Roku is a visible, screen-level failure (a launch war).
//
// This is the case a blanket "control everything discovered" default would get
// wrong, and it fails silently in production — the operator sees a screen
// flapping, not an error.
func TestDiscoveredButUnadoptedIsRefused(t *testing.T) {
	addrs := fakeAddresses{}
	addrs.put("roku-ecp", "uuid:roku:ecp:LEGACY", "http://192.168.50.99:8060/")
	r := New(nil, addrs)

	// A generation applies, and it adopts a DIFFERENT device.
	r.SetInventory(inventory(t, rokuEntry("dev-1", "uuid:roku:ecp:AA", "ent-1", true)))

	// The legacy screen's own derived entity id is not in the adopted set.
	if ep, ok := r.Target("ent-legacy"); ok {
		t.Fatalf("Target(ent-legacy) = %+v, ok; a discovered-but-unadopted device must never be drivable", ep)
	}
	if _, _, ok := r.ResolveEntity("ent-legacy"); ok {
		t.Fatal("ResolveEntity(ent-legacy) = ok; the adopted set must not resolve an unadopted entity")
	}
	if got := len(r.Targets()); got != 0 {
		t.Fatalf("Targets() has %d entries, want 0 — the adopted device is not locatable and the legacy one is not adopted", got)
	}
}

// TestAdoptedButNotLocatedIsRefused: adoption alone is not an address. Until
// this relay has actually seen the device, there is nowhere to send a command,
// and inventing one (a default host, a guess) would dispatch at whatever
// happens to answer.
func TestAdoptedButNotLocatedIsRefused(t *testing.T) {
	r := New(nil, fakeAddresses{})
	r.SetInventory(inventory(t, rokuEntry("dev-1", "uuid:roku:ecp:AA", "ent-1", true)))

	if ep, ok := r.Target("ent-1"); ok {
		t.Fatalf("Target(ent-1) = %+v, ok; an adopted device this relay cannot locate is not drivable", ep)
	}
	// It still RESOLVES, which is the correct two-stage answer: the entity is
	// real and adopted (so a command against it is not "unknown entity"), it
	// simply cannot be reached right now.
	if _, _, ok := r.ResolveEntity("ent-1"); !ok {
		t.Fatal("ResolveEntity(ent-1) = not ok; an adopted entity must resolve even while unlocatable")
	}
}

// TestDisabledEntityIsRefused: `enabled` is authored policy (REL-063) meaning
// "do not act on this". It must be enforced by ABSENCE from the drivable set,
// not by a check some later caller can forget.
func TestDisabledEntityIsRefused(t *testing.T) {
	addrs := fakeAddresses{}
	addrs.put("roku-ecp", "uuid:roku:ecp:AA", "http://192.168.50.31:8060/")
	r := New(nil, addrs)

	if n := r.SetInventory(inventory(t, rokuEntry("dev-1", "uuid:roku:ecp:AA", "ent-1", false))); n != 0 {
		t.Fatalf("SetInventory reported %d drivable entities for a disabled one, want 0", n)
	}
	if ep, ok := r.Target("ent-1"); ok {
		t.Fatalf("Target(ent-1) = %+v, ok; a disabled entity must not be drivable", ep)
	}
	if _, _, ok := r.ResolveEntity("ent-1"); ok {
		t.Fatal("ResolveEntity(ent-1) = ok; a disabled entity must not resolve")
	}
}

// TestUnadoptionTakesEffectOnTheNextGeneration: the section is a full statement
// of the adopted set, so a device absent from a newer generation has been
// released and must stop being drivable immediately. A delta fold-in could
// never express this.
func TestUnadoptionTakesEffectOnTheNextGeneration(t *testing.T) {
	addrs := fakeAddresses{}
	addrs.put("roku-ecp", "uuid:roku:ecp:AA", "192.168.50.31:8060")
	r := New(nil, addrs)

	r.SetInventory(inventory(t, rokuEntry("dev-1", "uuid:roku:ecp:AA", "ent-1", true)))
	if _, ok := r.Target("ent-1"); !ok {
		t.Fatal("Target(ent-1) not drivable after adoption")
	}

	// The next generation adopts nothing.
	if n := r.SetInventory(nil); n != 0 {
		t.Fatalf("SetInventory(nil) reported %d drivable entities, want 0", n)
	}
	if ep, ok := r.Target("ent-1"); ok {
		t.Fatalf("Target(ent-1) = %+v, ok after un-adoption; the inventory is a replace, not a delta", ep)
	}
}

// TestOverrideBypassesAdoption pins the escape hatch's semantics: a
// deployment-stated target IS an adoption decision (someone typed it into this
// relay's configuration), so it resolves with no inventory at all and no
// discovered address.
func TestOverrideBypassesAdoption(t *testing.T) {
	r := New(map[string]Endpoint{"ent-pinned": {Host: "10.0.0.5", Port: 8060}}, fakeAddresses{})

	ep, ok := r.Target("ent-pinned")
	if !ok || ep.Host != "10.0.0.5" || ep.Port != 8060 {
		t.Fatalf("Target(ent-pinned) = (%+v, %v), want ({10.0.0.5 8060}, true)", ep, ok)
	}
	if got := len(r.Targets()); got != 1 {
		t.Fatalf("Targets() has %d entries, want 1 (the override)", got)
	}
}

// TestOverrideWinsOverDiscoveredAddress: a stated fact beats an inferred one.
// The case that matters is a device whose SSDP LOCATION is wrong (a VM, a
// misconfigured responder) — the override is how an operator fixes it without
// touching adoption.
func TestOverrideWinsOverDiscoveredAddress(t *testing.T) {
	addrs := fakeAddresses{}
	addrs.put("roku-ecp", "uuid:roku:ecp:AA", "http://192.168.50.31:8060/")
	r := New(map[string]Endpoint{"ent-1": {Host: "10.0.0.5"}}, addrs)
	r.SetInventory(inventory(t, rokuEntry("dev-1", "uuid:roku:ecp:AA", "ent-1", true)))

	ep, _ := r.Target("ent-1")
	if ep.Host != "10.0.0.5" {
		t.Fatalf("Target(ent-1).Host = %q, want the override 10.0.0.5", ep.Host)
	}
	// And it appears exactly once in the whole set, not twice.
	all := r.Targets()
	if len(all) != 1 {
		t.Fatalf("Targets() = %v, want exactly one entry", all)
	}
}

// TestNewCopiesOverrides: the override map comes from the process environment
// and must not stay aliased to the caller's map.
func TestNewCopiesOverrides(t *testing.T) {
	overrides := map[string]Endpoint{"ent-1": {Host: "10.0.0.5"}}
	r := New(overrides, nil)
	delete(overrides, "ent-1")

	if _, ok := r.Target("ent-1"); !ok {
		t.Fatal("Target(ent-1) = not ok; New must copy the override map")
	}
}

// TestSetInventorySkipsUnusableEntries: the section is already
// signature-verified, so a malformed entry is a producer defect. Refusing the
// whole inventory over one bad row would un-adopt every good device on the
// site — a far worse outcome than skipping the row.
func TestSetInventorySkipsUnusableEntries(t *testing.T) {
	addrs := fakeAddresses{}
	addrs.put("roku-ecp", "uuid:roku:ecp:AA", "192.168.50.31")
	r := New(nil, addrs)

	good := inventory(t, rokuEntry("dev-1", "uuid:roku:ecp:AA", "ent-1", true))
	entries := []json.RawMessage{
		json.RawMessage(`{"device_id":`),                                    // not JSON at all
		json.RawMessage(`{"device_id":"dev-2","driver":"","native_id":""}`), // no identity to look an address up by
		good[0],
	}
	if n := r.SetInventory(entries); n != 1 {
		t.Fatalf("SetInventory reported %d drivable entities, want 1 (the good row survives)", n)
	}
	if _, ok := r.Target("ent-1"); !ok {
		t.Fatal("the well-formed adopted device must still be drivable alongside malformed rows")
	}
}

// TestNoAddressSourceResolvesOnlyOverrides is the shape of a relay with no
// discovery lane running: adoption is known, location is not, so only what the
// deployment stated outright is drivable.
func TestNoAddressSourceResolvesOnlyOverrides(t *testing.T) {
	r := New(map[string]Endpoint{"ent-pinned": {Host: "10.0.0.5"}}, nil)
	r.SetInventory(inventory(t, rokuEntry("dev-1", "uuid:roku:ecp:AA", "ent-1", true)))

	if _, ok := r.Target("ent-1"); ok {
		t.Fatal("Target(ent-1) = ok with no address source; nothing can locate it")
	}
	if _, ok := r.Target("ent-pinned"); !ok {
		t.Fatal("Target(ent-pinned) = not ok; an override needs no address source")
	}
}

// TestZeroValueRegistryRefusesEverything: before any generation applies, the
// relay has been told nothing about adoption, and the truthful answer is to
// refuse — not to fall back to driving whatever it can see.
func TestZeroValueRegistryRefusesEverything(t *testing.T) {
	addrs := fakeAddresses{}
	addrs.put("roku-ecp", "uuid:roku:ecp:AA", "192.168.50.31")
	r := New(nil, addrs)

	if _, ok := r.Target("ent-1"); ok {
		t.Fatal("Target(ent-1) = ok before any inventory applied")
	}
	if got := len(r.Targets()); got != 0 {
		t.Fatalf("Targets() has %d entries before any inventory applied, want 0", got)
	}
}

// TestParseEndpoint covers the three address shapes the relay actually sees off
// the LAN plus the ones that must fail closed. The LOCATION-URL port case is
// the load-bearing one: a Roku states its ECP port there, and defaulting to
// 8060 instead would silently never reach a device on any other port.
func TestParseEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		wantHost string
		wantPort int
		wantOK   bool
	}{
		{"roku LOCATION url", "http://192.168.50.31:8060/", "192.168.50.31", 8060, true},
		{"url on a non-standard port", "http://192.168.50.31:9999/dial/dd.xml", "192.168.50.31", 9999, true},
		{"url with no port", "http://roku.local/", "roku.local", 0, true},
		{"bare host:port", "192.168.50.31:8060", "192.168.50.31", 8060, true},
		{"bare host", "192.168.50.31", "192.168.50.31", 0, true},
		{"bracketed ipv6 with port", "[fe80::1]:8060", "fe80::1", 8060, true},
		{"ipv6 url", "http://[fe80::1]:8060/", "fe80::1", 8060, true},
		{"surrounding whitespace", "  192.168.50.31:8060  ", "192.168.50.31", 8060, true},
		{"empty", "", "", 0, false},
		{"whitespace only", "   ", "", 0, false},
		{"url with no host", "http://:8060/", "", 0, false},
		{"port only", ":8060", "", 0, false},
		{"non-numeric port", "192.168.50.31:http", "", 0, false},
		{"zero port", "192.168.50.31:0", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep, ok := ParseEndpoint(tc.addr)
			if ok != tc.wantOK {
				t.Fatalf("ParseEndpoint(%q) ok = %v, want %v", tc.addr, ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if ep.Host != tc.wantHost || ep.Port != tc.wantPort {
				t.Errorf("ParseEndpoint(%q) = %+v, want {%s %d}", tc.addr, ep, tc.wantHost, tc.wantPort)
			}
		})
	}
}

// TestUnparseableAddressRefuses: an address the relay cannot turn into a host
// must fail closed at resolution rather than producing a request to a
// half-formed authority.
func TestUnparseableAddressRefuses(t *testing.T) {
	addrs := fakeAddresses{}
	addrs.put("roku-ecp", "uuid:roku:ecp:AA", "http://:8060/")
	r := New(nil, addrs)
	r.SetInventory(inventory(t, rokuEntry("dev-1", "uuid:roku:ecp:AA", "ent-1", true)))

	if ep, ok := r.Target("ent-1"); ok {
		t.Fatalf("Target(ent-1) = %+v, ok for an address with no host", ep)
	}
}

// TestDiscoveredAddressOffTheLANIsRefused: resolution is the last thing that
// happens before a dial, so it re-asks the question the discovery lane and the
// candidate store already asked — is this a host this relay may talk to at all?
// A discovered address is the app peer's adoption decision joined to a value
// that came off unauthenticated multicast; the join must not be able to produce
// an HTTP request to a host of a spoofer's choosing (internal/relay/lanaddr).
func TestDiscoveredAddressOffTheLANIsRefused(t *testing.T) {
	for _, addr := range []string{
		"http://attacker.example:8060/",
		"http://93.184.216.34:8060/",
		"http://169.254.169.254/",
		"attacker.example:8060",
	} {
		addrs := fakeAddresses{}
		addrs.put("roku-ecp", "uuid:roku:ecp:AA", addr)
		r := New(nil, addrs)
		r.SetInventory(inventory(t, rokuEntry("dev-1", "uuid:roku:ecp:AA", "ent-1", true)))

		if ep, ok := r.Target("ent-1"); ok {
			t.Errorf("Target(ent-1) = %+v for discovered address %q, want refused", ep, addr)
		}
		if got := len(r.Targets()); got != 0 {
			t.Errorf("Targets() has %d entries for discovered address %q, want 0 — the poller must not poll it either", got, addr)
		}
	}
}

// TestAnOverrideIsNotSubjectToTheDialPolicy: the escape hatch stays an escape
// hatch. An override is a fact an operator typed into this relay's own
// configuration — the same authority that decides whether this process runs at
// all — and it exists for exactly the addresses discovery cannot serve, which
// on some sites means a name or a routed subnet.
func TestAnOverrideIsNotSubjectToTheDialPolicy(t *testing.T) {
	r := New(map[string]Endpoint{"ent-pinned": {Host: "roku-lobby.example.com", Port: 8060}}, fakeAddresses{})

	ep, ok := r.Target("ent-pinned")
	if !ok || ep.Host != "roku-lobby.example.com" {
		t.Fatalf("Target(ent-pinned) = (%+v, %v), want the operator's stated host", ep, ok)
	}
}
