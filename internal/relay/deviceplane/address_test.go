package deviceplane

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
)

// address_test.go covers the two relay-LOCAL facts the candidate store learned
// to carry: where a discovered device answered from (Address, the join that
// makes an adoption dialable) and what a driver has observed it doing
// (SetEntityState, REL-110a's per-entity `state`).
//
// Both are held to one hard rule the wire cases below pin: neither may change
// what a `device.candidates` report puts on the wire. REL-110a fixes the
// candidate's member set, and the frozen corpus is the oracle for it.

const (
	testDriver   = "roku-ecp"
	testNativeID = "uuid:roku:ecp:AA11"
	testSite     = "01J8Z3K4N5P6Q7R8S9T0V1SITE"
)

// rokuSighting is one SSDP sighting of a Roku exposing a single "main" entity.
func rokuSighting(address string) Observation {
	return Observation{
		Match:       Match{SSDP: "roku:ecp"},
		Provenance:  ProvenanceDiscovered,
		Driver:      testDriver,
		NativeID:    testNativeID,
		DeviceClass: "media-player",
		Name:        "Hanger TV",
		Entities:    []CandidateEntity{{Key: "main", DeviceClass: "media-player"}},
		Address:     address,
	}
}

// TestAddressForReturnsTheObservedLocation is the join the adoption gate needs:
// the app peer adopts by (driver, native_id) and nothing in that identity is
// dialable, so the relay's own sighting is the only source of an address.
func TestAddressForReturnsTheObservedLocation(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(rokuSighting("http://192.168.50.31:8060/"), 1000)

	addr, ok := s.AddressFor(testDriver, testNativeID)
	if !ok || addr != "http://192.168.50.31:8060/" {
		t.Fatalf("AddressFor = (%q, %v), want the observed LOCATION", addr, ok)
	}
}

// TestAddressForUnknownAndAddressless: an identity this relay never saw, and
// one it saw through a lane that reports no address, both fail closed — there
// is nothing to dial, and the caller must refuse rather than guess.
func TestAddressForUnknownAndAddressless(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(rokuSighting(""), 1000)

	if addr, ok := s.AddressFor(testDriver, testNativeID); ok {
		t.Fatalf("AddressFor for an addressless sighting = (%q, true), want not ok", addr)
	}
	if addr, ok := s.AddressFor(testDriver, "uuid:never-seen"); ok {
		t.Fatalf("AddressFor for an unknown identity = (%q, true), want not ok", addr)
	}
}

// TestAddressForRefusesAnIgnoredCandidate: suppression is an instruction not to
// act on a device. Handing out its address would let a caller dial it anyway,
// making the suppression cosmetic — the same reason ResolveEntity refuses one.
func TestAddressForRefusesAnIgnoredCandidate(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(rokuSighting("192.168.50.31:8060"), 1000)
	s.Ignore(Key(testDriver, testNativeID), nil)

	if addr, ok := s.AddressFor(testDriver, testNativeID); ok {
		t.Fatalf("AddressFor for an ignored candidate = (%q, true), want not ok", addr)
	}
}

// TestReObservationUpdatesTheAddress: a device that took a new DHCP lease is
// re-sighted at its new address, and the store must follow — a stored address
// that never moved would dispatch at whatever took over the old lease.
func TestReObservationUpdatesTheAddress(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(rokuSighting("http://192.168.50.31:8060/"), 1000)
	s.Observe(rokuSighting("http://192.168.50.77:8060/"), 2000)

	addr, _ := s.AddressFor(testDriver, testNativeID)
	if addr != "http://192.168.50.77:8060/" {
		t.Fatalf("AddressFor after a re-sighting = %q, want the new address", addr)
	}
}

// TestAddresslessReObservationKeepsTheAddress: two lanes can see one device and
// only one of them reads a LOCATION, and a NOTIFY can arrive without the
// header. Clearing on the empty case would make control flicker off and on with
// whichever lane spoke last.
func TestAddresslessReObservationKeepsTheAddress(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(rokuSighting("http://192.168.50.31:8060/"), 1000)
	s.Observe(rokuSighting(""), 2000)

	addr, ok := s.AddressFor(testDriver, testNativeID)
	if !ok || addr != "http://192.168.50.31:8060/" {
		t.Fatalf("AddressFor after an addressless re-sighting = (%q, %v), want the last known address", addr, ok)
	}
}

// TestOverlongAddressDropsTheSighting: an address arrives off unauthenticated
// multicast like every other identity field, so it is bounded at the same
// place and by the same rule — a sighting that fails is dropped rather than
// stored, so poison never reaches a report.
func TestOverlongAddressDropsTheSighting(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(rokuSighting("http://"+strings.Repeat("a", maxObservationFieldBytes)+"/"), 1000)

	if got := len(s.Report().Body.Candidates); got != 0 {
		t.Fatalf("store holds %d candidate(s) after an over-long address, want 0", got)
	}
}

// TestAnUndialableAddressIsBlankedNotStored: the address is the only observed
// field this relay ACTS on, so a sighting naming somewhere it must not dial
// keeps the device (it really was seen) and loses the address. Storing it would
// put the host into AddressFor, out of the adoption gate, and into an HTTP
// request — see internal/relay/lanaddr.
func TestAnUndialableAddressIsBlankedNotStored(t *testing.T) {
	for _, address := range []string{
		"http://attacker.example:8060/", // a name, resolved by whoever owns it
		"http://93.184.216.34:8060/",    // off this LAN entirely
		"http://169.254.169.254/",       // the cloud metadata endpoint
		"239.255.255.250:8060",          // multicast
	} {
		s := NewStore("relay-1")
		s.Observe(rokuSighting(address), 1000)

		cands := s.Report().Body.Candidates
		if len(cands) != 1 {
			t.Fatalf("store holds %d candidate(s) after a sighting with address %q, want 1 — the device was seen", len(cands), address)
		}
		if cands[0].Address != "" {
			t.Errorf("candidate address = %q for %q, want blank — this relay must never dial it", cands[0].Address, address)
		}
		if addr, ok := s.AddressFor(testDriver, testNativeID); ok {
			t.Errorf("AddressFor = (%q, true) for %q, want not ok", addr, address)
		}
	}
}

// TestAHostileSightingCannotStealAnAdoptedDevicesAddress is the attack the
// blanking rule closes, in the shape that actually costs something: the device
// is already known at a good address, and one spoofed packet reusing its USN
// tries to move it. Blanking (rather than storing) means orKeep leaves the real
// address in place, so the screen keeps being driven.
func TestAHostileSightingCannotStealAnAdoptedDevicesAddress(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(rokuSighting("http://192.168.50.31:8060/"), 1000)
	s.Observe(rokuSighting("http://attacker.example:80/"), 2000)

	addr, ok := s.AddressFor(testDriver, testNativeID)
	if !ok || addr != "http://192.168.50.31:8060/" {
		t.Fatalf("AddressFor = (%q, %v), want the real device's address retained", addr, ok)
	}
}

// TestSetEntityStateRidesTheReport is the state surface end to end on the relay
// side: a driver's observation lands on the candidate whose derived entity id
// it names (REL-110b), and the next full-set report carries it as REL-110a's
// per-entity `state`.
func TestSetEntityStateRidesTheReport(t *testing.T) {
	s := NewStore("relay-1")
	s.SetSite(testSite)
	s.Observe(rokuSighting("192.168.50.31"), 1000)

	entityID := deviceid.Entity(testSite, testDriver, testNativeID, "main")
	if !s.SetEntityState(entityID, "idle") {
		t.Fatal("SetEntityState = false for an entity this relay's own candidate derives to")
	}

	cands := s.Report().Body.Candidates
	if len(cands) != 1 || len(cands[0].Entities) != 1 {
		t.Fatalf("report = %+v, want one candidate with one entity", cands)
	}
	if got := cands[0].Entities[0].State; got != "idle" {
		t.Fatalf("reported entity state = %q, want %q", got, "idle")
	}
}

// TestSetEntityStateRefusesUnknownAndUnsited: an id no candidate derives to,
// and a store with no adopted site to derive against, both report false rather
// than writing state onto some other entity.
func TestSetEntityStateRefusesUnknownAndUnsited(t *testing.T) {
	unsited := NewStore("relay-1")
	unsited.Observe(rokuSighting("192.168.50.31"), 1000)
	if unsited.SetEntityState("01J8Z3K4N5P6Q7R8S9T0V1ENTX", "on") {
		t.Fatal("SetEntityState = true with no site adopted; nothing derives yet")
	}

	s := NewStore("relay-1")
	s.SetSite(testSite)
	s.Observe(rokuSighting("192.168.50.31"), 1000)
	if s.SetEntityState("01J8Z3K4N5P6Q7R8S9T0V1ENTX", "on") {
		t.Fatal("SetEntityState = true for an id no candidate derives to")
	}
}

// TestSetEntityStateSurvivesReObservation is the bug this merge exists to
// prevent: a discovery lane never observes state, so its Entities always carry
// State "". A sweep that replaced the fan-out wholesale would blank the polled
// state every 60 seconds, and REL-110a's "present once observed" would go
// true→false→true on the sweep cycle.
func TestSetEntityStateSurvivesReObservation(t *testing.T) {
	s := NewStore("relay-1")
	s.SetSite(testSite)
	s.Observe(rokuSighting("192.168.50.31"), 1000)

	entityID := deviceid.Entity(testSite, testDriver, testNativeID, "main")
	s.SetEntityState(entityID, "on")

	s.Observe(rokuSighting("192.168.50.31"), 2000)

	cands := s.Report().Body.Candidates
	if got := cands[0].Entities[0].State; got != "on" {
		t.Fatalf("reported entity state after a re-sighting = %q, want %q", got, "on")
	}
}

// TestReObservationDropsAVanishedEntitysState: the fresh fan-out is
// authoritative for WHICH entities exist. Only the survivors' state is carried
// across — a key that disappeared takes its state with it rather than lingering
// on a report as an entity the device no longer exposes.
func TestReObservationDropsAVanishedEntitysState(t *testing.T) {
	s := NewStore("relay-1")
	s.SetSite(testSite)

	two := rokuSighting("192.168.50.31")
	two.Entities = []CandidateEntity{
		{Key: "main", DeviceClass: "media-player"},
		{Key: "tuner", DeviceClass: "media-player"},
	}
	s.Observe(two, 1000)
	s.SetEntityState(deviceid.Entity(testSite, testDriver, testNativeID, "tuner"), "off")

	s.Observe(rokuSighting("192.168.50.31"), 2000) // "main" only

	ents := s.Report().Body.Candidates[0].Entities
	if len(ents) != 1 || ents[0].Key != "main" {
		t.Fatalf("entities after the narrower sighting = %+v, want just main", ents)
	}
}

// TestAddressIsReportedToTheAppPeer pins a DELIBERATE integration decision.
//
// Two parallel tracks disagreed here: one kept the discovered LAN address
// strictly relay-local (the relay dials, so the app peer never needs it), the
// other reported it upward. Reporting it wins, because the operator-facing
// device list has to SHOW an operator which box on their LAN a candidate is —
// the legacy product listed device IPs, and "adopt this one" is not answerable
// from an opaque USN alone.
//
// It is safe against the frozen corpus because the member is `omitempty`: a
// candidate observed without an address marshals byte-identically to before the
// field existed, so no REL-110a corpus case moves. FOLLOW-UP: REL-110a
// enumerates the candidate member set and does not yet name `address`/`model`/
// `serial`; the contract should be amended to permit them explicitly rather
// than leaving three members the text does not mention.
func TestAddressIsReportedToTheAppPeer(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(rokuSighting("http://192.168.50.31:8060/"), 1000)

	raw, err := json.Marshal(s.Report().Body.Candidates[0])
	if err != nil {
		t.Fatalf("marshaling candidate: %v", err)
	}
	if !strings.Contains(string(raw), "192.168.50.31") {
		t.Fatalf("candidate does not carry the discovered address, so no device list can show it: %s", raw)
	}

	// A candidate with no observed address carries no `address` key at all, so
	// the member is additive against the frozen corpus rather than a new
	// always-present field.
	bare := NewStore("relay-1")
	bare.Observe(rokuSighting(""), 1000)
	bareRaw, err := json.Marshal(bare.Report().Body.Candidates[0])
	if err != nil {
		t.Fatalf("marshaling addressless candidate: %v", err)
	}
	if strings.Contains(string(bareRaw), "address") {
		t.Fatalf("an addressless candidate emitted an `address` key: %s", bareRaw)
	}
}
