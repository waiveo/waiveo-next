package deviceplane

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
)

// factrank_test.go covers the merge rules that rank a re-sighting's facts by
// QUALITY rather than by arrival order — keepName, keepAddress and keepMatch —
// and the declaration rules that govern the entity fan-out.
//
// EVERY TEST HERE MUST FAIL IF ITS GUARD IS REMOVED. That is not a style note:
// the pre-change merge was `if next != "" { return next }`, which satisfies
// every "the better value lands", "the rename lands" and "the move lands"
// assertion on its own. A test made only of those is a description of the old
// code wearing the new code's name. So each one below pairs the behaviour that
// must KEEP working with the refusal that is the actual fix, and the pairing is
// what makes it a pin — verified by restoring the old merge bodies and watching
// all of them go red.
//
// The fixture is a real device. On the lab box, candidate
// 0VEJA6B9AT0FFTS8X3JTWXYC7Z — an onn 4K streaming box at 48:5c:2c:31:6e:6e —
// alternated between "onn. 4K Streaming Box" and "onn.-4K-Streaming-Bo". Both
// strings came from ONE lane reading ONE avahi dump: `_androidtvremote2._tcp`
// announces the display name, `_googlecast._tcp` announces a 20-character
// truncated hostname form with a hex tail. Whichever service happened to be in a
// given sweep's browse result decided what an operator saw.

const onnNativeID = "48:5c:2c:31:6e:6e"

// The two names verbatim, as read off the box's live avahi cache.
const (
	onnDisplayName = "onn. 4K Streaming Box" // _androidtvremote2._tcp
	onnCastName    = "onn.-4K-Streaming-Bo"  // _googlecast._tcp, hex tail stripped
)

// onnSighting is one host-mDNS sighting of that box, as it arrives from the lane
// after the within-sweep pick: a name and the rank of the service that said it.
// No EntitySource — this lane reads the whole avahi cache and has no declared
// watch behind it, so it never states a fan-out.
func onnSighting(name string, rank NameRank) Observation {
	return Observation{
		Match:       Match{MacOui: "485c2c"},
		Provenance:  ProvenanceDiscovered,
		Driver:      HostDriver,
		NativeID:    onnNativeID,
		DeviceClass: "media-player",
		Name:        name,
		NameRank:    rank,
		Address:     "192.168.50.63",
	}
}

func onlyCandidate(t *testing.T, s *Store) Candidate {
	t.Helper()
	cands := s.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1 — every sighting here is the SAME device: %+v", len(cands), cands)
	}
	return cands[0]
}

// THE DEFECT (#198), and the recovery that has to come with it. A sweep whose
// browse result held the Cast record but not the remote-service record must not
// relabel a box the operator already sees named; a sweep that DOES see the
// better record must improve it on the spot. Both directions in one test on
// purpose — "the better one lands" alone is satisfied by the presence-only merge
// this replaces, so it evidences nothing by itself.
func TestAWorseSourcedNameDoesNotDisplaceABetterOneAndABetterOneLands(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(onnSighting(onnCastName, NameRankMachine), 1000)
	if got := onlyCandidate(t, s).Name; got != onnCastName {
		t.Fatalf("name = %q, want %q — a machine name must still fill an empty slot", got, onnCastName)
	}

	s.Observe(onnSighting(onnDisplayName, NameRankFriendly), 2000)
	if got := onlyCandidate(t, s).Name; got != onnDisplayName {
		t.Fatalf("name = %q, want %q — a better-ranked record must improve the name on the very next sweep", got, onnDisplayName)
	}

	// The sweep that #198 was made of: the Cast record present, the remote
	// service missing. Non-empty, later, and strictly worse.
	s.Observe(onnSighting(onnCastName, NameRankMachine), 3000)
	if got := onlyCandidate(t, s).Name; got != onnDisplayName {
		t.Fatalf("name = %q, want %q — a Cast instance name must not overwrite the display name a better-ranked record announced", got, onnDisplayName)
	}
}

// THE STICKINESS ANSWER, and the failure the rank must NOT introduce. Rank
// decides whose statement counts; recency decides what that source says. An
// owner renaming the box is the SAME record saying something new, so it lands on
// the next sweep — a name that could only ever get "better" would be a
// permanently stale name, which is a worse defect than the flapping one.
//
// The worse-ranked sightings on either side of the rename are what make this a
// pin rather than a restatement of the old behaviour.
func TestAGenuineRenameLandsWhileAWorseRecordIsRefusedAroundIt(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(onnSighting(onnDisplayName, NameRankFriendly), 1000)
	s.Observe(onnSighting(onnCastName, NameRankMachine), 2000)
	if got := onlyCandidate(t, s).Name; got != onnDisplayName {
		t.Fatalf("name = %q, want %q before the rename — the worse record must be refused", got, onnDisplayName)
	}

	// The operator renames the box. `_androidtvremote2._tcp` says so on the next
	// 30-second sweep, at the rank it has always spoken at.
	s.Observe(onnSighting("Living Room", NameRankFriendly), 3000)
	if got := onlyCandidate(t, s).Name; got != "Living Room" {
		t.Fatalf("name = %q, want %q — a rename announced by the same-ranked record must win, or the rank has traded a volatile name for a stale one", got, "Living Room")
	}

	// ...and the old name does not come back through the worse record.
	s.Observe(onnSighting(onnCastName, NameRankMachine), 4000)
	if got := onlyCandidate(t, s).Name; got != "Living Room" {
		t.Fatalf("name = %q, want %q — the rename must survive the next Cast-only sweep", got, "Living Room")
	}
}

// THE LADDER'S HARD CONSTRAINT, pinned as an inequality rather than as prose.
//
// keepName refuses a strictly worse rank and remembers the held one until this
// process exits, so a rank nothing can RESTATE is a permanent pin: the name
// freezes and the device can never be renamed. The first draft had one — a top
// rank minted only by the scan-gated ECP probe, above every rank the 30-second
// mDNS sweep can produce — and a rename became invisible until an operator ran
// a scan. The invariant that prevents it is that the ladder's top is a rank a
// continuously sweeping lane also mints, so this asserts the top IS
// NameRankFriendly and that a device can be renamed at it.
// The other half — that no such rank exists to be claimed in the first place —
// is asserted against the SOURCE, because Go has no way to enumerate a typed
// const block at runtime and a hand-written list of the ranks would just be a
// second copy of the declaration agreeing with itself. Declaring a rank above
// NameRankFriendly trips this; the seam that would then feed it is pinned
// separately (cmd/waiveo-relay TestNoECPNameOutranksWhatTheSweepingLaneCanRestate).
func TestTheTopRankIsOneASweepingLaneCanRestate(t *testing.T) {
	if last := lastDeclaredNameRank(t); last != "NameRankFriendly" {
		t.Fatalf("the highest declared NameRank is %s, want NameRankFriendly — a rank above it can only come from the "+
			"scan-gated ECP probe, and keepName remembers a held rank until this process exits, so a name that reached "+
			"it could never be corrected by the lane that sweeps every 30 seconds", last)
	}

	// And a name held at the ladder's TOP must still be displaceable, which is
	// the property the constraint exists to guarantee.
	s := NewStore("relay-1")
	s.Observe(onnSighting("The Hanger", NameRankFriendly), 1000)
	s.Observe(onnSighting("Hangar Bay", NameRankFriendly), 2000)
	if got := onlyCandidate(t, s).Name; got != "Hangar Bay" {
		t.Fatalf("name = %q, want %q — a name held at the top rank must still be renameable, or the ladder has a pin in it", got, "Hangar Bay")
	}
}

// lastDeclaredNameRank returns the name of the final constant in store.go's
// NameRank const block — the ladder's top, whatever it currently is.
func lastDeclaredNameRank(t *testing.T) string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "store.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing store.go: %v", err)
	}
	last := ""
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		block := []string{}
		isNameRank := false
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 {
				continue
			}
			if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "NameRank" {
				isNameRank = true
			}
			block = append(block, vs.Names[0].Name)
		}
		if isNameRank && len(block) > 0 {
			last = block[len(block)-1]
		}
	}
	if last == "" {
		t.Fatal("found no NameRank const block in store.go — this test can no longer see the ladder it exists to guard")
	}
	return last
}

// Silence is not a retraction, and it is not a licence either. The neighbour
// lane re-observes every host every 30s carrying no name at all; a worse-ranked
// lane carries one. Neither may change a name a better record authored.
func TestNeitherSilenceNorAWorseRecordDisplacesAName(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(onnSighting(onnDisplayName, NameRankFriendly), 1000)

	// A neighbour sweep: no name, no rank.
	s.Observe(onnSighting("", NameRankNone), 2000)
	if got := onlyCandidate(t, s).Name; got != onnDisplayName {
		t.Fatalf("name = %q, want %q — an empty name must never overwrite a known one", got, onnDisplayName)
	}

	// A REMEMBERED name replayed by the SSDP lane's identity cache arrives with
	// no rank at all (discovery.recalled), so it cannot out-shout a lane that
	// just re-read the LAN.
	s.Observe(onnSighting("onn. 4K Streaming Box (old)", NameRankNone), 3000)
	if got := onlyCandidate(t, s).Name; got != onnDisplayName {
		t.Fatalf("name = %q, want %q — an unranked replay must not displace an observed name", got, onnDisplayName)
	}
}

// The zero value has to be the SAFE one, because a lane added later will set a
// Name and forget the rank. Unranked fills a gap; it never displaces a name a
// ranked record authored.
func TestAnUnrankedNameFillsAGapButNeverDowngrades(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(onnSighting("Some Host", NameRankNone), 1000)
	if got := onlyCandidate(t, s).Name; got != "Some Host" {
		t.Fatalf("name = %q, want %q — an unranked name must still fill an empty slot", got, "Some Host")
	}

	s.Observe(onnSighting(onnDisplayName, NameRankFriendly), 2000)
	s.Observe(onnSighting("Whatever The New Lane Says", NameRankNone), 3000)
	if got := onlyCandidate(t, s).Name; got != onnDisplayName {
		t.Fatalf("name = %q, want %q — a lane that forgot to rank its name must not be able to downgrade one", got, onnDisplayName)
	}
}

// THE SAME DEFECT ON THE FIELD WITH TEETH. The SSDP lane reads a LOCATION and
// reports host:port; the neighbour and host-mDNS lanes report the bare host they
// read from the kernel table and the avahi cache. On the lab box 61 of 61 rows
// had lost their port this way, harmless only because a missing port falls
// through to the Roku driver's default.
//
// Learning the port and then keeping it are one test, for the reason the name's
// are: "the port lands" is satisfied by the presence-only merge this replaces.
func TestALearnedPortIsAdoptedAndThenNotErasedByABareSighting(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(rokuSighting("192.168.50.31"), 1000)
	s.Observe(rokuSighting("192.168.50.31:8060"), 2000)
	if addr, _ := s.AddressFor(testDriver, testNativeID); addr != "192.168.50.31:8060" {
		t.Fatalf("address = %q, want 192.168.50.31:8060 — learning the port is an improvement and must land", addr)
	}

	// A neighbour sweep of the same device: the kernel table has no ports.
	bare := rokuSighting("192.168.50.31")
	bare.Name, bare.Entities, bare.EntitySource = "", nil, ""
	s.Observe(bare, 3000)

	addr, ok := s.AddressFor(testDriver, testNativeID)
	if !ok || addr != "192.168.50.31:8060" {
		t.Fatalf("AddressFor = (%q, %v), want 192.168.50.31:8060 — a lane that cannot see ports must not delete one another lane read", addr, ok)
	}
}

// ...but the port is the only thing protected. A DIFFERENT host is a device
// whose DHCP lease moved, which is new reachability and must land immediately
// even though it arrives bare — refusing it would strand every command. The
// refusal of the same-host bare sighting first is what makes this a pin.
func TestAMovedDeviceLandsBareEvenThoughABareSameHostSightingIsRefused(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(rokuSighting("192.168.50.31:8060"), 1000)

	sameHostBare := rokuSighting("192.168.50.31")
	sameHostBare.Name, sameHostBare.Entities, sameHostBare.EntitySource = "", nil, ""
	s.Observe(sameHostBare, 2000)
	if addr, _ := s.AddressFor(testDriver, testNativeID); addr != "192.168.50.31:8060" {
		t.Fatalf("address = %q, want the port kept before the move is even exercised", addr)
	}

	moved := rokuSighting("192.168.50.99")
	moved.Name, moved.Entities, moved.EntitySource = "", nil, ""
	s.Observe(moved, 3000)

	addr, ok := s.AddressFor(testDriver, testNativeID)
	if !ok || addr != "192.168.50.99" {
		t.Fatalf("AddressFor = (%q, %v), want 192.168.50.99 — the rank protects the PORT, never a stale host", addr, ok)
	}
}

// THE UNGUARDED MERGE, and the severe one: the entity fan-out is declared by a
// WATCH, so every lane that has no watch (neighbour, port scan, host mDNS)
// builds an Observation with no declaration behind it. Replacing the list
// wholesale deleted the handles this relay addresses the device by —
// ResolveEntity and SetEntityObservation both walk it — so an inbound command
// resolved to nothing until the next SSDP announcement.
func TestALaneWithNoDeclarationDoesNotDeleteTheFanOut(t *testing.T) {
	s := NewStore("relay-1")
	s.SetSite(testSite)
	s.Observe(rokuSighting("192.168.50.31:8060"), 1000)

	entityID := deviceid.Entity(testSite, testDriver, testNativeID, "main")
	if !s.SetEntityObservation(entityID, "on", map[string]string{"active_app": "Netflix"}) {
		t.Fatalf("SetEntityObservation did not resolve %q — the fixture is wrong before the merge is even exercised", entityID)
	}

	// A host-mDNS sweep of the same device: a name, and no idea what it exposes.
	nameOnly := Observation{
		Match: Match{MacOui: "c48b66"}, Provenance: ProvenanceDiscovered,
		Driver: testDriver, NativeID: testNativeID, DeviceClass: ClassUnclassified,
		Name: "The Hanger", NameRank: NameRankFriendly, Address: "192.168.50.31",
	}
	s.Observe(nameOnly, 2000)

	c := onlyCandidate(t, s)
	if len(c.Entities) != 1 || c.Entities[0].Key != "main" {
		t.Fatalf("entities = %+v, want the declared [main] fan-out intact — a lane with no watch must not delete it", c.Entities)
	}
	if c.Entities[0].State != "on" || c.Entities[0].Attributes["active_app"] != "Netflix" {
		t.Fatalf("entity = %+v, want the polled state and attributes carried across", c.Entities[0])
	}
	if _, _, ok := s.ResolveEntity(entityID); !ok {
		t.Fatalf("ResolveEntity(%q) failed after an entity-less sighting — an operator's command would be refused COMMAND_UNRESOLVED", entityID)
	}
}

// A watch that genuinely re-declares its fan-out replaces it wholesale, in BOTH
// directions — including down to fewer entities than before. That is why the
// guard tests the DECLARATION and not the list's length: keyed on length, a pack
// narrowing its fan-out could never remove anything and the list could only ever
// grow. The entity-less sweep in the middle is what makes this fail without the
// guard.
func TestADeclaredFanOutShrinksWhileAnUndeclaredSweepChangesNothing(t *testing.T) {
	s := NewStore("relay-1")
	two := rokuSighting("192.168.50.31:8060")
	two.Entities = []CandidateEntity{
		{Key: "main", DeviceClass: "media-player"},
		{Key: "screen", DeviceClass: "media-player"},
	}
	s.Observe(two, 1000)

	// A neighbour sweep: no declaration, so it states nothing about the fan-out.
	neighbour := Observation{
		Match: Match{MacOui: "c48b66"}, Provenance: ProvenanceDiscovered,
		Driver: testDriver, NativeID: testNativeID, DeviceClass: ClassUnclassified,
		Address: "192.168.50.31",
	}
	s.Observe(neighbour, 2000)
	if got := len(onlyCandidate(t, s).Entities); got != 2 {
		t.Fatalf("entities = %d, want 2 — a sweep with no declaration behind it may not edit the fan-out", got)
	}

	// The pack narrows the watch to one entity. That IS a statement, and it lands.
	one := rokuSighting("192.168.50.31:8060")
	one.Entities = []CandidateEntity{{Key: "screen", DeviceClass: "media-player"}}
	s.Observe(one, 3000)

	c := onlyCandidate(t, s)
	if len(c.Entities) != 1 || c.Entities[0].Key != "screen" {
		t.Fatalf("entities = %+v, want exactly the re-declared [screen] — a lane that DOES declare a fan-out states it in full, shrinking included", c.Entities)
	}
}

// THE WITHDRAWAL, which no sighting can express. Remove the pack and its watch
// stops observing; the other lanes keep re-observing the same host with no
// declaration. Nothing expires a candidate, so without RetainDeclarations the
// removed pack's entities are reported — and RESOLVED against — until the relay
// restarts.
func TestARemovedDeclarationStopsBeingReportedAndResolvable(t *testing.T) {
	s := NewStore("relay-1")
	s.SetSite(testSite)
	s.Observe(rokuSighting("192.168.50.31:8060"), 1000)

	entityID := deviceid.Entity(testSite, testDriver, testNativeID, "main")
	if _, _, ok := s.ResolveEntity(entityID); !ok {
		t.Fatalf("ResolveEntity(%q) failed before the withdrawal — the fixture is wrong", entityID)
	}

	// A generation that still declares this watch changes nothing.
	if cleared := s.RetainDeclarations(map[string]bool{"ssdp:roku:ecp": true}); cleared != 0 {
		t.Fatalf("RetainDeclarations cleared %d candidate(s) while the declaration is still live, want 0", cleared)
	}
	if got := len(onlyCandidate(t, s).Entities); got != 1 {
		t.Fatalf("entities = %d, want 1 — a live declaration keeps its fan-out", got)
	}

	// The pack is removed: the next apply installs a watch set without it.
	if cleared := s.RetainDeclarations(map[string]bool{"ssdp:something-else": true}); cleared != 1 {
		t.Fatalf("RetainDeclarations cleared %d candidate(s) after the declaration was withdrawn, want 1", cleared)
	}
	if got := onlyCandidate(t, s).Entities; len(got) != 0 {
		t.Fatalf("entities = %+v, want none — a removed pack's fan-out must stop being reported", got)
	}
	if _, _, ok := s.ResolveEntity(entityID); ok {
		t.Fatalf("ResolveEntity(%q) still succeeds after the pack was removed — the relay would execute a command against a device no installed pack claims", entityID)
	}

	// A candidate a lane found with no declaration behind it has no fan-out to
	// lose and must not be disturbed by an apply.
	s.Observe(onnSighting(onnDisplayName, NameRankFriendly), 4000)
	if cleared := s.RetainDeclarations(nil); cleared != 0 {
		t.Fatalf("RetainDeclarations cleared %d candidate(s) that never had a declared fan-out, want 0", cleared)
	}
}

// `match` is the provenance of the sighting — which declared pattern found the
// device — and MACIdentity mints the macOui form mechanically for every
// MAC-resolved sighting, so it is the Match analogue of ClassUnclassified.
// Letting it overwrite the search target a device actually answered would
// replace the provenance with "whoever swept last". Both directions in one test,
// again because "the specific one lands" is what the bare overwrite already did.
//
// This exercises a path production cannot reach today — canonicalize rewrites an
// SSDP sighting's Match to the MAC form before the merge sees it, so the two
// forms never meet on one key (keepMatch says so at length). The test is honest
// about what it is: the rule, held to, so that the day canonicalize keeps the
// declared match this does not have to be rediscovered.
func TestADeclaredPatternWinsAndTheGenericMacDefaultCannotTakeItBack(t *testing.T) {
	s := NewStore("relay-1")
	macOnly := Observation{
		Match: Match{MacOui: "c48b66"}, Provenance: ProvenanceDiscovered,
		Driver: testDriver, NativeID: testNativeID, DeviceClass: ClassUnclassified,
		Address: "192.168.50.31",
	}
	s.Observe(macOnly, 1000)
	if got := onlyCandidate(t, s).Match; got.MacOui != "c48b66" {
		t.Fatalf("match = %+v, want the mac default to fill the gap", got)
	}

	s.Observe(rokuSighting("192.168.50.31:8060"), 2000) // Match{SSDP: "roku:ecp"}
	if got := onlyCandidate(t, s).Match; got.SSDP != "roku:ecp" {
		t.Fatalf("match = %+v, want the declared ssdp pattern to win — answering a search target says more than having a MAC", got)
	}

	s.Observe(macOnly, 3000)
	if got := onlyCandidate(t, s).Match; got.SSDP != "roku:ecp" {
		t.Fatalf("match = %+v, want the declared ssdp pattern kept — the mechanical MAC default must not claim to be what found the device", got)
	}
}
