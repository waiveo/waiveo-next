package deviceplane

import "testing"

// classrank_test.go covers the merge that decides what a device IS across
// sweeps — keepClass — and it is the third instance of one defect, so it is
// written to fail the way the first two were finally caught rather than the way
// they were first argued.
//
// EVERY TEST HERE MUST FAIL IF ITS GUARD IS REMOVED. The merge this replaces was
// "a specific class beats the generic default, and between two specific classes
// the newer sighting wins", which satisfies every "the classification lands",
// "the reclassification lands" and "unclassified never erases" assertion on its
// own. Each case below therefore pairs the behaviour that must KEEP working with
// the refusal that is the actual fix — verified by restoring the old merge body
// and watching the refusals go red.
//
// THE FIXTURES ARE TWO REAL DEVICES, and they disagree with each other on
// purpose: one is fixed by comparing concreteness and the other is BROKEN by
// comparing concreteness first. A test built on either alone would pass while
// the ordering was wrong.
//
//   - 192.168.50.43 (MAC 84:28:59:9f:2f:08), a Google speaker, advertises
//     `_matter._tcp` -> smart-home and `_spotify-connect._tcp` -> media-player.
//     BOTH are Feature — a Matter fabric membership and a Spotify endpoint are
//     each capabilities, not products — so authority ties and only concreteness
//     separates them. This is the MEASURED #204 instance: replaying a real box
//     .12 avahi dump, dropping the single `_spotify-connect` record flips the
//     sweep's pick to smart-home, and the old merge took that as a genuine
//     reclassification and let the app peer write it to disk.
//   - 192.168.39.241, an ecobee thermostat, advertises `_ecobee._tcp` ->
//     smart-home at PRODUCT alongside `_airplay._tcp` and
//     `_spotify-connect._tcp` -> media-player at Feature. Concreteness alone
//     calls this thermostat a media player. Only authority-first keeps it a
//     thermostat, which is why d321893 rejected ranking the class tokens outright
//     on live data.

const (
	speakerMAC = "84:28:59:9f:2f:08" // 192.168.50.43
	ecobeeMAC  = "44:61:32:1b:9e:c4" // 192.168.39.241
)

// classSighting is one sweep's report of a host: the class that sweep's best
// record implied, and the authority of that record. It is what hostmdns.sweep
// hands the store once betterClass has picked between the host's services.
func classSighting(mac, class string, rank ClassRank) Observation {
	driver, nativeID, match, ok := MACIdentity(mac)
	if !ok {
		panic("MACIdentity refused a lab MAC: " + mac)
	}
	return Observation{
		Match:       match,
		Provenance:  ProvenanceDiscovered,
		Driver:      driver,
		NativeID:    nativeID,
		DeviceClass: class,
		ClassRank:   rank,
	}
}

// THE DEFECT (#204), and the recovery that has to come with it.
//
// One sweep sees both of the speaker's records and reports media-player; the
// next sweep's browse is missing `_spotify-connect` and reports smart-home for a
// device that has not changed. Sweeps really do vary that way — three
// consecutive `-t` dumps 12s apart disagreed about one host's services — and the
// old merge, seeing two specific classes, took the newer one.
//
// Both directions in one test on purpose: "a better classification lands" is
// satisfied by the merge this replaces and evidences nothing by itself.
func TestASweepMissingAServiceDoesNotReclassifyTheDeviceAndABetterRecordStillLands(t *testing.T) {
	s := NewStore("relay-1")

	// A first sweep that saw only `_matter`: smart-home is the honest answer and
	// must land, because it is all anything knows about this host yet.
	s.Observe(classSighting(speakerMAC, "smart-home", ClassRankFeature), 1000)
	if got := onlyCandidate(t, s).DeviceClass; got != "smart-home" {
		t.Fatalf("device_class = %q, want smart-home — a lone feature signal must still classify a host nothing else recognises", got)
	}

	// A sweep that saw `_spotify-connect` too. Same authority, more concrete:
	// this is the sweep whose answer the operator should keep.
	s.Observe(classSighting(speakerMAC, "media-player", ClassRankFeature), 2000)
	if got := onlyCandidate(t, s).DeviceClass; got != "media-player" {
		t.Fatalf("device_class = %q, want media-player — a more concrete class at the same authority is a better answer and must land", got)
	}

	// THE FIX. The next browse drops the one Spotify record. Nothing about the
	// device changed; only the cache did.
	s.Observe(classSighting(speakerMAC, "smart-home", ClassRankFeature), 3000)
	if got := onlyCandidate(t, s).DeviceClass; got != "media-player" {
		t.Fatalf("device_class = %q, want media-player — one mDNS record missing from one sweep must not reclassify a device, and this class governs the command vocabulary (REG-052)", got)
	}
	if got := onlyCandidate(t, s).ClassRank; got != ClassRankFeature {
		t.Fatalf("class_rank = %d, want %d — the class and the rank that authored it must move as a pair, or the next worse sighting walks in", got, ClassRankFeature)
	}
}

// AUTHORITY IS COMPARED FIRST, and this is the device that proves the ordering
// rather than merely stating it. Concreteness alone gets this one BACKWARDS.
//
// This is the remedy d321893 already rejected on live data, pinned so it cannot
// be re-proposed: a durable class rank built on the derivable half alone fixes
// the speaker above and turns this thermostat into a media player.
func TestTheThermostatsOwnProductServiceOutranksTheMediaFeaturesBoltedOntoIt(t *testing.T) {
	s := NewStore("relay-1")

	// A sweep that read `_ecobee`: the device's own product service.
	s.Observe(classSighting(ecobeeMAC, "smart-home", ClassRankProduct), 1000)

	// A sweep that read `_airplay` — more CONCRETE, and completely wrong about
	// what the device is.
	s.Observe(classSighting(ecobeeMAC, "media-player", ClassRankFeature), 2000)
	if got := onlyCandidate(t, s).DeviceClass; got != "smart-home" {
		t.Fatalf("device_class = %q, want smart-home — an ecobee advertises `_airplay` and `_spotify-connect` beside its own service, and a thermostat is not a media player because it can play audio", got)
	}

	// And the device's own service, re-read on a later sweep, still lands: the
	// refusal above must not have frozen the field.
	s.Observe(classSighting(ecobeeMAC, "smart-home", ClassRankProduct), 3000)
	if got := onlyCandidate(t, s).DeviceClass; got != "smart-home" {
		t.Fatalf("device_class = %q, want smart-home", got)
	}
}

// The original guard, which must survive the rank being added on top of it: the
// generic default is "not yet learned", never "no longer true".
//
// The neighbour lane re-observes every host on the LAN every 30 seconds carrying
// nothing but `unclassified` and no rank at all, so this is the highest-traffic
// path through the merge.
func TestTheGenericDefaultFillsAGapAndThenNeverErasesALearnedClass(t *testing.T) {
	s := NewStore("relay-1")

	// The neighbour lane finds a host: unclassified, unranked, and it must land
	// because REL-110a requires a non-empty class.
	s.Observe(classSighting(speakerMAC, ClassUnclassified, ClassRankNone), 1000)
	if got := onlyCandidate(t, s).DeviceClass; got != ClassUnclassified {
		t.Fatalf("device_class = %q, want %q — a host nothing has recognised must still carry the generic class", got, ClassUnclassified)
	}

	s.Observe(classSighting(speakerMAC, "media-player", ClassRankFeature), 2000)

	// Every subsequent neighbour sweep.
	s.Observe(classSighting(speakerMAC, ClassUnclassified, ClassRankNone), 3000)
	if got := onlyCandidate(t, s).DeviceClass; got != "media-player" {
		t.Fatalf("device_class = %q, want media-player — the generic default must never downgrade a class another lane learned", got)
	}
}

// THE RANK MUST NOT BE ABLE TO BUY THE GENERIC DEFAULT AUTHORITY, which is how
// adding a ladder BREAKS the guard above instead of strengthening it.
//
// A rank is a statement about the RECORD BEHIND a class, and behind
// `unclassified` there is no record — it is what a lane mints for a host it did
// not recognise. Nothing stops a caller pairing the two:
// discovery.Watch.observation stamps ClassRankProduct on a declared watch's
// DeviceClass unconditionally, and `unclassified` is a legal Class identifier
// under device-class-registry/1 REG-010 (`^[a-z][a-z0-9-]*$`; nothing reserves
// the sentinel), so a pack declaring that class mints exactly this pair. Stored
// as written it would out-rank every real classification on the LAN and pin the
// candidate at the generic default for the rest of the process.
func TestARankedGenericDefaultIsStoredWithNoAuthorityAndCannotPinACandidate(t *testing.T) {
	s := NewStore("relay-1")

	// The gap-fill still lands — but at the FLOOR, whatever the caller claimed.
	s.Observe(classSighting(speakerMAC, ClassUnclassified, ClassRankProduct), 1000)
	got := onlyCandidate(t, s)
	if got.DeviceClass != ClassUnclassified {
		t.Fatalf("device_class = %q, want %q — a host nothing recognised must still carry the generic class", got.DeviceClass, ClassUnclassified)
	}
	if got.ClassRank != ClassRankNone {
		t.Fatalf("class_rank = %d, want %d (ClassRankNone) — a sighting that recognised NOTHING cannot be authoritative about nothing, and a stored Product here refuses every feature-ranked sighting the LAN can produce",
			got.ClassRank, ClassRankNone)
	}

	// So the very next honest sighting classifies the host.
	s.Observe(classSighting(speakerMAC, "media-player", ClassRankFeature), 2000)
	if got := onlyCandidate(t, s).DeviceClass; got != "media-player" {
		t.Fatalf("device_class = %q, want media-player — a generic default holding a rank it cannot justify has pinned the candidate", got)
	}

	// And it cannot displace a learned class on the way in either.
	s.Observe(classSighting(speakerMAC, ClassUnclassified, ClassRankProduct), 3000)
	if got := onlyCandidate(t, s); got.DeviceClass != "media-player" || got.ClassRank != ClassRankFeature {
		t.Fatalf("class/rank = %q/%d, want media-player/%d — a report that recognised nothing must not out-rank one that recognised something",
			got.DeviceClass, got.ClassRank, ClassRankFeature)
	}
}

// A GENUINE RECLASSIFICATION STILL LANDS. The refusals above are only safe
// because a device that really does change what it advertises is re-classified
// on the next sweep — the same property that makes keepName's refusals safe, and
// the reason every rank in this ladder is one a sweeping lane can restate.
func TestAnEquallySourcedEquallyConcreteReclassificationLands(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(classSighting(speakerMAC, "media-player", ClassRankFeature), 1000)
	s.Observe(classSighting(speakerMAC, "printer", ClassRankFeature), 2000)
	if got := onlyCandidate(t, s).DeviceClass; got != "printer" {
		t.Fatalf("device_class = %q, want printer — two equally-sourced, equally concrete classes are settled by recency, or a device that genuinely changes kind can never be corrected", got)
	}

	// And a PRODUCT statement corrects a feature guess immediately, which is the
	// path a pack's declared watch takes (discovery.Watch.observation).
	s.Observe(classSighting(speakerMAC, "smart-home", ClassRankProduct), 3000)
	if got := onlyCandidate(t, s).DeviceClass; got != "smart-home" {
		t.Fatalf("device_class = %q, want smart-home — a declared or product-service statement must be able to correct a feature-level guess", got)
	}
}

// The ZERO VALUE has to be the SAFE one, because a lane added later will set a
// DeviceClass and forget the rank — which is exactly what the two DECLARED lanes
// did until #204 taught them to state ClassRankProduct.
//
// Unranked fills a gap; it never displaces a ranked classification.
func TestAnUnrankedClassFillsAGapButNeverDisplacesARankedOne(t *testing.T) {
	s := NewStore("relay-1")
	s.Observe(classSighting(speakerMAC, "storage", ClassRankNone), 1000)
	if got := onlyCandidate(t, s).DeviceClass; got != "storage" {
		t.Fatalf("device_class = %q, want storage — an unranked class must still fill an empty slot", got)
	}

	s.Observe(classSighting(speakerMAC, "media-player", ClassRankFeature), 2000)
	s.Observe(classSighting(speakerMAC, "storage", ClassRankNone), 3000)
	if got := onlyCandidate(t, s).DeviceClass; got != "media-player" {
		t.Fatalf("device_class = %q, want media-player — a lane that forgot to rank its class must not be able to displace a ranked one", got)
	}
}

// THE WITHDRAWAL SEAM, now that a declaration authors a RANK and not only a
// fan-out.
//
// discovery.Watch.observation stamps ClassRankProduct on a declared watch's
// DeviceClass, deliberately — a human writing down "a device answering this
// pattern is a media player" is the strongest class statement this relay ever
// has — and canonicalize re-keys that sighting onto the MAC, which is the same
// store key hostmdns writes. So the declaration's class really does meet
// hostmdns's in keepClass, at Product against Product.
//
// Remove the pack and its watch simply stops observing, which is byte-for-byte
// what a quiet device looks like: no rule reading sightings can retract what it
// said. Without a retraction at the apply seam the removed pack's Product rank
// goes on out-ranking every hostmdns sighting for the rest of the process, and
// the class it authored can never be corrected by the lane that can actually see
// the device. That is the entity fan-out's own bug (#204's ledger calls it the
// KNOWN RESIDUAL one field over), and it arrives on the class the moment the
// class gains a rank.
//
// The class TOKEN is deliberately kept. Withdrawing a declaration retracts its
// AUTHORITY, not the observation — nothing on the LAN just changed, and blanking
// the class would report a known device as unrecognised until the next sweep.
func TestARemovedDeclarationStopsOutrankingTheLaneThatCanSeeTheDevice(t *testing.T) {
	s := NewStore("relay-1")
	s.SetSite(testSite)

	// A pack's declared watch: media-player at Product, canonicalized onto the
	// MAC key hostmdns also writes.
	declared := rokuSighting("192.168.50.31:8060")
	declared.ClassRank = ClassRankProduct
	s.Observe(declared, 1000)

	// hostmdns reads the device's own `_ecobee`-shaped product service and
	// disagrees. While the declaration is live it is refused, which is correct:
	// a human's declaration is at least as good as an inferred product service,
	// and at equal authority the more concrete class holds.
	hostSighting := Observation{
		Match:       Match{SSDP: "roku:ecp"},
		Provenance:  ProvenanceDiscovered,
		Driver:      testDriver,
		NativeID:    testNativeID,
		DeviceClass: "smart-home",
		ClassRank:   ClassRankProduct,
	}
	s.Observe(hostSighting, 2000)
	if got := onlyCandidate(t, s); got.DeviceClass != "media-player" || got.ClassRank != ClassRankProduct {
		t.Fatalf("class/rank = %q/%d, want media-player/%d while the declaration is live", got.DeviceClass, got.ClassRank, ClassRankProduct)
	}

	// A generation that still declares this watch retracts nothing.
	if cleared := s.RetainDeclarations(map[string]bool{"ssdp:roku:ecp": true}); cleared != 0 {
		t.Fatalf("RetainDeclarations cleared %d candidate(s) while the declaration is live, want 0", cleared)
	}
	if got := onlyCandidate(t, s).ClassRank; got != ClassRankProduct {
		t.Fatalf("class_rank = %d, want %d — a live declaration keeps its authority", got, ClassRankProduct)
	}

	// THE PACK IS REMOVED. The next apply installs a watch set without it.
	if cleared := s.RetainDeclarations(map[string]bool{"ssdp:something-else": true}); cleared != 1 {
		t.Fatalf("RetainDeclarations cleared %d candidate(s) after the declaration was withdrawn, want 1", cleared)
	}
	withdrawn := onlyCandidate(t, s)
	if withdrawn.DeviceClass != "media-player" {
		t.Fatalf("device_class = %q, want media-player — withdrawing a declaration retracts its authority, not the observation: a known device must not read as unrecognised until the next sweep",
			withdrawn.DeviceClass)
	}
	if withdrawn.ClassRank != ClassRankNone {
		t.Fatalf("class_rank = %d, want %d (ClassRankNone) — a removed pack's Product rank would out-rank every sighting for the rest of the process, and no lane could ever correct the class it authored",
			withdrawn.ClassRank, ClassRankNone)
	}

	// So the very next sweep by the lane that can actually see the device takes
	// the field, which is the point of the retraction.
	s.Observe(hostSighting, 3000)
	if got := onlyCandidate(t, s); got.DeviceClass != "smart-home" || got.ClassRank != ClassRankProduct {
		t.Fatalf("class/rank = %q/%d, want smart-home/%d — a withdrawn declaration must stop refusing the lane that re-observes the device",
			got.DeviceClass, got.ClassRank, ClassRankProduct)
	}
}

// The two keys of the comparison, asserted as a unit so a respelling cannot
// quietly flatten either. ClassConcreteness is EXPORTED because hostmdns's
// within-sweep pick and this cross-sweep merge must use one definition; two
// copies would let the sweep and the merge disagree about the same pair of
// tokens, which is a flap no single-layer test can see.
func TestTheTwoKeysOfTheClassComparisonAreOrdered(t *testing.T) {
	if !(ClassRankProduct > ClassRankFeature && ClassRankFeature > ClassRankNone) {
		t.Fatalf("the authority ladder is not strictly ordered: none=%d feature=%d product=%d", ClassRankNone, ClassRankFeature, ClassRankProduct)
	}
	if ClassRankNone != 0 {
		t.Fatalf("ClassRankNone = %d, want 0 — the zero value must be the bottom, or a lane that forgets to rank its class silently outranks one that does", ClassRankNone)
	}
	for _, tc := range []struct {
		class string
		want  int
	}{
		{"", 0},
		{ClassUnclassified, 0},
		{"smart-home", 1},
		{"media-player", 2},
		{"printer", 2},
		{"storage", 2},
	} {
		if got := ClassConcreteness(tc.class); got != tc.want {
			t.Errorf("ClassConcreteness(%q) = %d, want %d", tc.class, got, tc.want)
		}
	}
}
