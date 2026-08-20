package store

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// discoveredmerge_internal_test.go covers mergeDiscovered's rules directly,
// because they are the DURABLE half of the same defect the relay's in-memory
// merge has: a report that learned something WORSE is not the same as a report
// that learned nothing, and presence alone cannot tell them apart.
//
// A relay-side fix the mirror then undoes is not a fix. These pin all three
// ranked facts: the ADDRESS, whose quality this side re-derives entirely from
// the value; the NAME, whose quality is not in the string at all and therefore
// had to travel (REL-110c's `name_rank`); and the CLASS, which is HALF of each —
// how concrete a class is follows from the token and is re-derived here, how
// authoritative the record behind it was cannot be and travels as REL-110d's
// `class_rank`. The name was the one this file used to say it deliberately could
// not rank; the test that said so has been replaced, because it pinned the
// absence of the fix.
//
// EVERY TEST HERE PAIRS THE REFUSAL WITH THE ACCEPTANCE. The rule this replaced
// was `if reported != "" { return reported }`, which satisfies "the better value
// lands", "the move lands" and "the reclassification lands" on its own — so a
// test built only from those would go green on a revert and evidence nothing.

// THE ADDRESS. The relay's SSDP lane reads a LOCATION and reports host:port; its
// neighbour and host-mDNS lanes report the bare host they read out of the kernel
// table and the avahi cache. Both are non-empty, so a presence-only merge stored
// whichever arrived last — 61 of 61 rows on the lab box had lost their port.
func TestAReportWithoutAPortDoesNotEraseAStoredOneButAPortStillLands(t *testing.T) {
	// Learning the port for a known host is an improvement and lands.
	learned := mergeDiscovered(
		DiscoveredDevice{Address: "192.168.50.31"},
		DiscoveredDevice{Address: "192.168.50.31:8060"},
	).Address
	if learned != "192.168.50.31:8060" {
		t.Fatalf("address = %q, want the newly learned port to land", learned)
	}

	// ...and the next report from a lane that cannot see ports does not undo it.
	kept := mergeDiscovered(
		DiscoveredDevice{Address: "192.168.50.31:8060"},
		DiscoveredDevice{Address: "192.168.50.31"},
	).Address
	if kept != "192.168.50.31:8060" {
		t.Fatalf("address = %q, want 192.168.50.31:8060 — a report from a lane that cannot see ports must not delete one another lane read", kept)
	}

	// An empty address is still silence rather than a retraction.
	silent := mergeDiscovered(
		DiscoveredDevice{Address: "192.168.50.31:8060"},
		DiscoveredDevice{Address: ""},
	).Address
	if silent != "192.168.50.31:8060" {
		t.Fatalf("address = %q, want the held address kept — an empty report is silence, not a retraction", silent)
	}
}

// The port is the ONLY thing protected. A different host is a device whose DHCP
// lease moved, and refusing it would leave every command going to an address
// nothing answers at. The refusal of the same-host bare report first is what
// makes this a pin rather than a description of the old rule.
func TestAMovedDeviceOverwritesEvenWithoutAPort(t *testing.T) {
	held := DiscoveredDevice{Address: "192.168.50.31:8060"}

	if got := mergeDiscovered(held, DiscoveredDevice{Address: "192.168.50.31"}).Address; got != "192.168.50.31:8060" {
		t.Fatalf("address = %q, want the port kept before the move is exercised", got)
	}
	if got := mergeDiscovered(held, DiscoveredDevice{Address: "192.168.50.99"}).Address; got != "192.168.50.99" {
		t.Fatalf("address = %q, want 192.168.50.99 — the rule protects the port, never a stale host", got)
	}
}

// THE CLASS. This one had no guard here at all: the class was "taken as
// reported" on the theory that a report always states it in full. It does — but
// a relay that has just restarted states the GENERIC DEFAULT for every host its
// mDNS sweep has not reached yet, and a blind take wrote that over a learned
// class durably, which outlives the in-memory flicker the relay itself already
// fixed.
//
// All three arms together, because the take-as-reported rule satisfies two of
// them by itself.
func TestTheGenericClassFillsAGapAndReclassifiesButNeverDowngrades(t *testing.T) {
	// It still fills a gap, so a device nothing has classified is stored with a
	// class at all (REL-110a requires a non-empty one).
	if got := mergeDiscovered(DiscoveredDevice{}, DiscoveredDevice{DeviceClass: classUnclassified}).DeviceClass; got != classUnclassified {
		t.Fatalf("device_class = %q, want %q — a device nothing has recognised must still carry the generic class", got, classUnclassified)
	}

	// A genuine reclassification still lands: between two SPECIFIC classes the
	// newer report wins, exactly as deviceplane.keepClass decides it in relay
	// memory.
	if got := mergeDiscovered(DiscoveredDevice{DeviceClass: "media-player"}, DiscoveredDevice{DeviceClass: "printer"}).DeviceClass; got != "printer" {
		t.Fatalf("device_class = %q, want printer — a specific class re-stated by a later report is a reclassification and must win", got)
	}

	// But the generic default never erases a learned class.
	if got := mergeDiscovered(DiscoveredDevice{DeviceClass: "media-player"}, DiscoveredDevice{DeviceClass: classUnclassified}).DeviceClass; got != "media-player" {
		t.Fatalf("device_class = %q, want media-player — the generic default is 'not yet learned', never 'no longer true'", got)
	}
}

// THE GENERIC DEFAULT MAY NOT BUY AUTHORITY, which is the way the rank breaks
// the guard above rather than strengthening it.
//
// A rank is a statement about the RECORD BEHIND a class, and behind
// `unclassified` there is no record — it is what a lane mints for a host it did
// not recognise. Take the rank from the wire without checking what it ranks and
// the ladder inverts: the report that recognised NOTHING out-ranks every report
// that recognised something, and because nothing on a LAN mints `product` for a
// device whose records are all features, the row is pinned at the generic
// default for the life of the file.
//
// Two producers, neither hypothetical. Any enrolled relay can put the pair on
// the wire — this store's own header calls relay input untrusted, which is why
// classRankFact exists, and classRankFact clamps the token's vocabulary while
// saying nothing about the PAIRING. And a pack may register a device class whose
// id is literally `unclassified`: REG-010's grammar admits it, nothing reserves
// the sentinel, and discovery.Watch.observation stamps ClassRankProduct on a
// declared class unconditionally.
func TestARankedGenericDefaultNeitherOutranksNorPinsALearnedClass(t *testing.T) {
	// It cannot displace a learned class, however it is ranked.
	refused := mergeDiscovered(classRow("media-player", classRankFeature), classRow(classUnclassified, classRankProduct))
	if refused.DeviceClass != "media-player" || refused.ClassRank != classRankFeature {
		t.Fatalf("class/rank = %q/%q, want media-player/feature — a report that recognised NOTHING must not out-rank one that recognised something",
			refused.DeviceClass, refused.ClassRank)
	}

	// And when it legitimately fills a gap it is STORED at the floor, so the
	// next honest sighting can take the field. This is the assertion that
	// matters: stored at `product`, the row would refuse every feature-ranked
	// report the LAN can produce, forever.
	filled := mergeDiscovered(DiscoveredDevice{}, classRow(classUnclassified, classRankProduct))
	if filled.DeviceClass != classUnclassified || filled.ClassRank != classRankNone {
		t.Fatalf("class/rank = %q/%q, want %q/%q — a generic default must be recorded with no authority behind it, whatever the wire claimed",
			filled.DeviceClass, filled.ClassRank, classUnclassified, classRankNone)
	}
	recovered := mergeDiscovered(filled, classRow("media-player", classRankFeature))
	if recovered.DeviceClass != "media-player" || recovered.ClassRank != classRankFeature {
		t.Fatalf("class/rank = %q/%q, want media-player/feature — an honest sighting must be able to classify a host the generic default is holding",
			recovered.DeviceClass, recovered.ClassRank)
	}
}

// THE CLASS, RANKED (#204). The two fixtures are the two real devices the
// relay-side ladder is built on, and they disagree on purpose: one is fixed by
// comparing concreteness and the other is BROKEN by comparing concreteness
// first, so a test using either alone would pass while the ordering was wrong.
//
//   - 192.168.50.43, a Google speaker: `_matter` -> smart-home and
//     `_spotify-connect` -> media-player, BOTH at feature. This is the measured
//     instance — drop the one Spotify record from a sweep and the relay reports
//     smart-home for an unchanged device, which this merge used to write to disk.
//   - 192.168.39.241, an ecobee: `_ecobee` -> smart-home at PRODUCT beside
//     `_airplay`/`_spotify-connect` -> media-player at feature. Concreteness
//     alone calls the thermostat a media player.
func classRow(class, rank string) DiscoveredDevice {
	return DiscoveredDevice{DeviceClass: class, ClassRank: rank}
}

// THE CONCRETENESS TIEBREAK IS DELIBERATELY NOT RESTATED HERE, and this test is
// the pin on that decision.
//
// deviceplane.keepClass DOES refuse an equal-authority downgrade, and that is
// what fixes the measured #204 instance: the speaker's `_matter` and
// `_spotify-connect` records are both FEATURE, so only concreteness separates
// them. Restating that comparison at this layer looks like defence-in-depth and
// is the same bet held for a different duration — and the duration is the whole
// bet. In relay memory it expires with the process. On disk it never expires, so
// a device that PERMANENTLY stops advertising its more concrete service reports
// the honest lower class on every sweep for the rest of the file's life and
// every one of them is refused: relay restart, refused; app restart, refused; no
// operator action clears it short of revoking the relay.
//
// Both arms, because the first alone is satisfied by a mirror that simply never
// refuses anything.
func TestAnEqualAuthorityChangeLandsBecauseThisLayerCannotBoundARefusal(t *testing.T) {
	// The better record lands: same authority, more concrete.
	learned := mergeDiscovered(classRow("smart-home", classRankFeature), classRow("media-player", classRankFeature))
	if learned.DeviceClass != "media-player" || learned.ClassRank != classRankFeature {
		t.Fatalf("class/rank = %q/%q, want media-player/feature", learned.DeviceClass, learned.ClassRank)
	}

	// 192.168.50.43 with Spotify unlinked: it genuinely IS a smart-home device
	// now, it says so at the same authority on every sweep forever, and the
	// relay's own cross-sweep memory has already decided this is not a dropped
	// record. A durable refusal here would freeze the row against a device that
	// really changed.
	downgraded := mergeDiscovered(classRow("media-player", classRankFeature), classRow("smart-home", classRankFeature))
	if downgraded.DeviceClass != "smart-home" || downgraded.ClassRank != classRankFeature {
		t.Fatalf("class/rank = %q/%q, want smart-home/feature — an equal-authority restatement is the relay's settled verdict, and refusing it on disk is a freeze with no expiry and no operator escape",
			downgraded.DeviceClass, downgraded.ClassRank)
	}

	// The same rule is what lets a pack CORRECT its own declared device class —
	// the one input where the newer statement is authoritative by construction,
	// and equally ranked with the one it replaces.
	corrected := mergeDiscovered(classRow("media-player", classRankProduct), classRow("smart-home", classRankProduct))
	if corrected.DeviceClass != "smart-home" {
		t.Fatalf("device_class = %q, want smart-home — a pack correcting its own declared class must be able to land", corrected.DeviceClass)
	}
}

// AUTHORITY IS COMPARED FIRST. This is the remedy d321893 rejected on live data,
// pinned so it cannot be re-proposed: a durable class rank built on the derivable
// half ALONE fixes the speaker above and turns this thermostat into a media
// player.
func TestTheThermostatsProductServiceOutranksTheMediaFeatureDurably(t *testing.T) {
	got := mergeDiscovered(classRow("smart-home", classRankProduct), classRow("media-player", classRankFeature))
	if got.DeviceClass != "smart-home" || got.ClassRank != classRankProduct {
		t.Fatalf("class/rank = %q/%q, want smart-home/product — an ecobee advertises `_airplay` and `_spotify-connect` beside its own service, and comparing specificity before authority calls it a media player",
			got.DeviceClass, got.ClassRank)
	}
	// And a product statement still corrects a feature guess, which is what keeps
	// the refusal above from being a freeze.
	if corrected := mergeDiscovered(classRow("media-player", classRankFeature), classRow("smart-home", classRankProduct)); corrected.DeviceClass != "smart-home" {
		t.Fatalf("device_class = %q, want smart-home — a product-level statement must be able to correct a feature-level guess", corrected.DeviceClass)
	}
}

// DECISION (a), AND THE DIVERGENCE FROM THE NAME, asserted as behaviour rather
// than left in a comment.
//
// keepNameFact rule 2 lets an UNRANKED report overwrite a better-ranked name and
// resets the row to unrecorded. The class deliberately does the opposite,
// because half a class's quality is derivable HERE: an unranked class report is
// not "no information", so refusing on the derivable half asserts nothing an
// older relay did not say — both sides of the comparison are this store's own
// derivation.
func TestAnUnrankedClassReportIsNoOpinionAboveTheFloorAndDoesNotResetTheRow(t *testing.T) {
	// A pre-REL-110d relay reporting the sweep-artifact class. It must not
	// displace a class this store holds at a recorded rank...
	refused := mergeDiscovered(classRow("media-player", classRankFeature), classRow("smart-home", classRankUnrecorded))
	if refused.DeviceClass != "media-player" {
		t.Fatalf("device_class = %q, want media-player — an absent rank is no opinion, not a licence to reclassify a device this store holds at a recorded rank", refused.DeviceClass)
	}
	// ...and must not RESET the rank either, which would re-open the same hole
	// one report later.
	if refused.ClassRank != classRankFeature {
		t.Fatalf("class_rank = %q, want feature — resetting the row to unrecorded on an unranked report is keepNameFact rule 2, and the class deliberately does not copy it", refused.ClassRank)
	}

	// It still fills a gap: nothing is being protected, so refusing would lose a
	// classification for the crime of a relay being older than this build.
	filled := mergeDiscovered(classRow(classUnclassified, classRankUnrecorded), classRow("media-player", classRankUnrecorded))
	if filled.DeviceClass != "media-player" || filled.ClassRank != classRankUnrecorded {
		t.Fatalf("class/rank = %q/%q, want media-player/unrecorded — a gap-fill must land, and must record honestly that nothing ranked it",
			filled.DeviceClass, filled.ClassRank)
	}

	// THE INVARIANT that keeps both fields comprehensible: an absent rank never
	// RAISES what the store will refuse. An unranked report that DOES land lands
	// at the bottom.
	landed := mergeDiscovered(classRow("smart-home", classRankUnrecorded), classRow("media-player", classRankUnrecorded))
	if landed.ClassRank != classRankUnrecorded {
		t.Fatalf("class_rank = %q, want unrecorded — an unranked report must never be stamped with a rank nothing stated", landed.ClassRank)
	}
}

// THE MIXED-VERSION WINDOW, which is every relay in the fleet on the day this
// ships — and the one place a durable guard MUST NOT try to be clever.
//
// With no rank on either side there is no authority to reason from, only the
// class tokens. Refusing the LESS CONCRETE of two unranked reports looks like a
// free win: it holds 192.168.50.43 at media-player without any relay upgrade at
// all. It is not free, and this test is the proof, because concreteness points
// the WRONG WAY on the other real device. The ecobee at 192.168.39.241
// advertises `_ecobee` (smart-home) beside `_airplay` and `_spotify-connect`
// (media-player), so an old relay's sweeps alternate between the two classes as
// records drop in and out — and a concreteness refusal turns the FIRST sweep
// that misses `_ecobee` into a permanent verdict. The thermostat becomes a media
// player on disk and every later correct sweep is refused, forever.
//
// So with nothing ranked, this layer does exactly what the presence-shaped merge
// it replaces did — a flap that self-corrects, never a pin. #204's measured
// instance is fixed on the RELAY, by the cross-sweep memory that stops the
// flapping report from ever being sent.
//
// Driven as SWEEPS rather than as a single merge, because "does it flap" and
// "does it pin" are not distinguishable from one call.
func TestWithNoRankOnEitherSideTheMirrorFlapsButNeverPins(t *testing.T) {
	// The ecobee, whose sweeps alternate. The last three are the ones that
	// matter: each must be able to correct the one before it.
	held := DiscoveredDevice{}
	for i, sweep := range []string{"smart-home", "media-player", "smart-home", "media-player", "smart-home"} {
		held = mergeDiscovered(held, classRow(sweep, classRankUnrecorded))
		if held.DeviceClass != sweep {
			t.Fatalf("sweep %d reported %q and the row holds %q — with nothing ranked, a report can only be refused permanently, and a thermostat pinned as a media player is worse than one that flaps",
				i+1, sweep, held.DeviceClass)
		}
		if held.ClassRank != classRankUnrecorded {
			t.Fatalf("sweep %d stored class_rank %q, want unrecorded — an unranked report must never be stamped with a rank nothing stated", i+1, held.ClassRank)
		}
	}

	// The generic default is still refused, because that guard needs no
	// authority to justify it: `unclassified` is a statement of ignorance, not a
	// competing verdict.
	if got := mergeDiscovered(classRow("media-player", classRankUnrecorded), classRow(classUnclassified, classRankUnrecorded)).DeviceClass; got != "media-player" {
		t.Fatalf("device_class = %q, want media-player — the gap rule survives having no ranks to compare", got)
	}

	// And an authority-ranked report still corrects a pre-upgrade row, so the
	// unrecorded state is not a pin in the other direction either.
	if corrected := mergeDiscovered(classRow("media-player", classRankUnrecorded), classRow("smart-home", classRankProduct)); corrected.DeviceClass != "smart-home" {
		t.Fatalf("device_class = %q, want smart-home — an unrecorded rank sits at the BOTTOM and must refuse nothing an authority-ranked report says", corrected.DeviceClass)
	}
}

// An unreadable class rank — a token a NEWER relay minted, or one a hostile
// relay invented — is clamped to the bottom of this build's ladder rather than
// refused at intake. Degrade-safe, not degrade-shut, and never stored verbatim.
func TestAnUnreadableClassRankSitsAtTheBottomAndIsNotStoredVerbatim(t *testing.T) {
	const hostile = "absolute-truth"

	refused := mergeDiscovered(classRow("media-player", classRankProduct), classRow("smart-home", hostile))
	if refused.DeviceClass != "media-player" || refused.ClassRank != classRankProduct {
		t.Fatalf("class/rank = %q/%q, want media-player/product — an uninterpretable rank must never be honoured as a licence to reclassify",
			refused.DeviceClass, refused.ClassRank)
	}

	filled := mergeDiscovered(DiscoveredDevice{}, classRow("media-player", hostile))
	if filled.DeviceClass != "media-player" {
		t.Fatalf("device_class = %q, want media-player — an unknown rank must still fill a gap", filled.DeviceClass)
	}
	if filled.ClassRank != classRankNone {
		t.Fatalf("class_rank = %q, want %q — an untrusted relay's bytes must not reach a column this store reasons over, and `unrecorded` would be untrue because the relay DID state something",
			filled.ClassRank, classRankNone)
	}
}

// The app plane restates relay/1's vocabulary rather than importing the relay's
// package (the rule classUnclassified and the name ranks already follow), so the
// two spellings are pinned in agreement here.
func TestTheStoredClassRankVocabularyIsRelay1s(t *testing.T) {
	for _, tc := range []struct{ stored, onTheWire string }{
		{classRankNone, wire.CandidateClassRankNone},
		{classRankFeature, wire.CandidateClassRankFeature},
		{classRankProduct, wire.CandidateClassRankProduct},
	} {
		if tc.stored != tc.onTheWire {
			t.Errorf("stored %q but relay/1 spells it %q", tc.stored, tc.onTheWire)
		}
		if classRankFact(tc.onTheWire) != tc.onTheWire {
			t.Errorf("classRankFact(%q) = %q — a token relay/1 publishes must survive the clamp verbatim", tc.onTheWire, classRankFact(tc.onTheWire))
		}
	}
	if !(classRankOrder(classRankProduct) > classRankOrder(classRankFeature) &&
		classRankOrder(classRankFeature) > classRankOrder(classRankNone)) {
		t.Fatalf("the app-side authority ladder is not strictly ordered: none=%d feature=%d product=%d",
			classRankOrder(classRankNone), classRankOrder(classRankFeature), classRankOrder(classRankProduct))
	}
	if classRankOrder(classRankUnrecorded) > classRankOrder(classRankNone) {
		t.Fatalf("unrecorded (%d) outranks none (%d) — an upgraded row would then refuse an authority-ranked report and could never be reclassified",
			classRankOrder(classRankUnrecorded), classRankOrder(classRankNone))
	}
	// The DERIVED key must agree with the relay's, or the sweep's own pick and
	// this merge disagree about the same pair of tokens — a flap neither layer's
	// tests can see. Restated rather than imported (no relay code app-side), so
	// the agreement is asserted rather than assumed.
	for _, tc := range []struct {
		class string
		want  int
	}{{"", 0}, {classUnclassified, 0}, {"smart-home", 1}, {"media-player", 2}, {"printer", 2}} {
		if got := classConcretenessFact(tc.class); got != tc.want {
			t.Errorf("classConcretenessFact(%q) = %d, want %d — this must match deviceplane.ClassConcreteness exactly", tc.class, got, tc.want)
		}
	}
}

// THE NAME. This used to be the LIMIT — the one fact this file said it could not
// rank, because the quality of a name is not in the string and REL-110a carried
// no member saying which record authored it. REL-110c adds that member, so the
// limit is gone and the rule below is the one the address has had all along.
//
// The two real names of the onn box on the lab LAN are the fixtures, because
// they are what the defect is made of: `_androidtvremote2._tcp` announces the
// display name at Friendly, `_googlecast._tcp` announces a 20-char truncation of
// the hostname at Machine, and a presence-only merge stored whichever the
// relay's post-restart sweep happened to reach first.
const (
	onnDisplayName = "onn. 4K Streaming Box"
	onnCastName    = "onn.-4K-Streaming-Bo"
)

func TestAWorseRankedNameDoesNotDisplaceABetterOneButARenameStillLands(t *testing.T) {
	// The better source lands over the worse one.
	learned := mergeDiscovered(
		DiscoveredDevice{Name: onnCastName, NameRank: nameRankMachine},
		DiscoveredDevice{Name: onnDisplayName, NameRank: nameRankFriendly},
	)
	if learned.Name != onnDisplayName || learned.NameRank != nameRankFriendly {
		t.Fatalf("name/rank = %q/%q, want %q/%q — a better-sourced name must land", learned.Name, learned.NameRank, onnDisplayName, nameRankFriendly)
	}

	// ...and the worse-sourced one does not take it back. THIS is the arm that
	// fails without the fix; every other arm passes on the presence-only rule.
	kept := mergeDiscovered(
		DiscoveredDevice{Name: onnDisplayName, NameRank: nameRankFriendly},
		DiscoveredDevice{Name: onnCastName, NameRank: nameRankMachine},
	)
	if kept.Name != onnDisplayName {
		t.Fatalf("name = %q, want %q — a Cast instance truncation must not overwrite the display name the device announces elsewhere", kept.Name, onnDisplayName)
	}
	if kept.NameRank != nameRankFriendly {
		t.Fatalf("name_rank = %q, want %q — the held name and the held rank move as a pair; a kept name stamped with the reported rank is a corrupted ladder entry", kept.NameRank, nameRankFriendly)
	}

	// A RENAME MUST ALWAYS LAND: same rank, newer report, and the ladder gets out
	// of the way. Without this the refusal above would be a permanent pin.
	renamed := mergeDiscovered(
		DiscoveredDevice{Name: onnDisplayName, NameRank: nameRankFriendly},
		DiscoveredDevice{Name: "Living Room", NameRank: nameRankFriendly},
	)
	if renamed.Name != "Living Room" || renamed.NameRank != nameRankFriendly {
		t.Fatalf("name/rank = %q/%q, want Living Room/friendly — the device restating itself through the record it always announced is a rename and must land",
			renamed.Name, renamed.NameRank)
	}

	// An empty name is still silence rather than a retraction — and it must keep
	// the RANK too, or the next worse report walks in through the gap.
	silent := mergeDiscovered(
		DiscoveredDevice{Name: onnDisplayName, NameRank: nameRankFriendly},
		DiscoveredDevice{Name: "", NameRank: nameRankNone},
	)
	if silent.Name != onnDisplayName || silent.NameRank != nameRankFriendly {
		t.Fatalf("name/rank = %q/%q, want %q/friendly — a report carrying no name learned nothing about the name OR its source",
			silent.Name, silent.NameRank, onnDisplayName)
	}
}

// THE UPGRADE ROW, which is the half #197 got wrong twice. A row written before
// the column existed carries the empty string, and the question is what that
// empty string is allowed to REFUSE.
//
// Both arms are the decision: it refuses nothing (so a name can still change),
// and the first landing ENDS the unrecorded state (so the row becomes one the
// ladder can protect). A row that stayed unrecorded forever would be a row this
// rule can never act on, which is the silent way to ship half a fix.
func TestAnUpgradedRowRefusesNothingAndStopsBeingUnrecordedOnTheFirstRankedReport(t *testing.T) {
	upgraded := DiscoveredDevice{Name: onnDisplayName, NameRank: nameRankUnrecorded}

	// It refuses nothing — not even the weakest ranked report. The held name's
	// real quality is unknowable, and guarding it would pin every pre-upgrade
	// name against every sweeping lane, at DISK lifetime.
	landed := mergeDiscovered(upgraded, DiscoveredDevice{Name: onnCastName, NameRank: nameRankMachine})
	if landed.Name != onnCastName {
		t.Fatalf("name = %q, want %q — an unrecorded rank is not evidence of quality and must not be able to refuse", landed.Name, onnCastName)
	}
	if landed.NameRank != nameRankMachine {
		t.Fatalf("name_rank = %q, want %q — the landing must record the reported rank, or the row stays unprotectable forever", landed.NameRank, nameRankMachine)
	}

	// And once recorded, the ladder is live on that row: the same worse report
	// that just won is now refused.
	healed := mergeDiscovered(
		mergeDiscovered(landed, DiscoveredDevice{Name: onnDisplayName, NameRank: nameRankFriendly}),
		DiscoveredDevice{Name: onnCastName, NameRank: nameRankMachine},
	)
	if healed.Name != onnDisplayName || healed.NameRank != nameRankFriendly {
		t.Fatalf("name/rank = %q/%q, want %q/friendly — the row must be self-healing after one report's worth of wrong",
			healed.Name, healed.NameRank, onnDisplayName)
	}
}

// A RELAY THAT DOES NOT RANK NAMES. `name_rank` is optional (REL-004/REL-110c),
// so a peer that predates it — or one downgraded back to that build — reports a
// name with no rank. Absent is "this peer does not rank names", NOT "this name
// is unranked", and the two must not be collapsed.
//
// Both arms again: such a report merges exactly as it did before this rule
// existed (presence wins, so a downgrade cannot brick a device's name), and the
// row goes BACK to unrecorded rather than being stamped with a rank nobody
// stated.
func TestAReportWithNoRankIsMergedOnPresenceAndLeavesTheRowUnrecorded(t *testing.T) {
	held := DiscoveredDevice{Name: onnDisplayName, NameRank: nameRankFriendly}

	got := mergeDiscovered(held, DiscoveredDevice{Name: onnCastName, NameRank: nameRankUnrecorded})
	if got.Name != onnCastName {
		t.Fatalf("name = %q, want %q — a stored rank must not be able to out-rank everything an un-upgraded relay can ever say; that device could never be renamed again",
			got.Name, onnCastName)
	}
	if got.NameRank != nameRankUnrecorded {
		t.Fatalf("name_rank = %q, want the row back at unrecorded — stamping a rank the report did not carry is this store asserting a relay's opinion on its behalf (#197)", got.NameRank)
	}

	// An absent rank is still silence about the NAME only: no name, no change.
	silent := mergeDiscovered(held, DiscoveredDevice{Name: "", NameRank: nameRankUnrecorded})
	if silent.Name != onnDisplayName || silent.NameRank != nameRankFriendly {
		t.Fatalf("name/rank = %q/%q, want %q/friendly", silent.Name, silent.NameRank, onnDisplayName)
	}
}

// A TOKEN THIS BUILD CANNOT READ. The intake deliberately does not refuse one
// (refusing would let a single malformed candidate blank a site's device list),
// so the clamp is here, and it is what stops a durable rank being a durable
// licence to refuse.
//
// A relay claiming a rank nobody has heard of gains NOTHING: it fills a gap like
// any weakest source, it cannot displace a better-sourced name, and what reaches
// the column is this build's own vocabulary rather than the relay's bytes.
func TestAnUnreadableRankSitsAtTheBottomAndIsNotStoredVerbatim(t *testing.T) {
	const hostile = "absolute-truth"

	// It cannot displace a better-sourced name.
	refused := mergeDiscovered(
		DiscoveredDevice{Name: onnDisplayName, NameRank: nameRankFriendly},
		DiscoveredDevice{Name: "Attacker's Choice", NameRank: hostile},
	)
	if refused.Name != onnDisplayName || refused.NameRank != nameRankFriendly {
		t.Fatalf("name/rank = %q/%q, want %q/friendly — an uninterpretable rank must never be honoured as a licence to refuse",
			refused.Name, refused.NameRank, onnDisplayName)
	}

	// It can still fill a gap — degrade-safe, not degrade-shut.
	filled := mergeDiscovered(DiscoveredDevice{}, DiscoveredDevice{Name: "Some Host", NameRank: hostile})
	if filled.Name != "Some Host" {
		t.Fatalf("name = %q, want Some Host — an unknown rank fills a gap; refusing one would lose a name for a member the relay merely spelled newer", filled.Name)
	}
	if filled.NameRank != nameRankNone {
		t.Fatalf("name_rank = %q, want %q — an untrusted relay's bytes must not reach a column this store reasons over",
			filled.NameRank, nameRankNone)
	}
}

// The app plane restates relay/1's vocabulary rather than importing the relay's
// package (the same rule classUnclassified follows in the same file), so the two
// spellings are pinned in agreement here. A token renamed on one side and not
// the other would silently demote every name that arrived under it to the bottom
// of the ladder — no error, no log line, just worse names.
func TestTheStoredNameRankVocabularyIsRelay1s(t *testing.T) {
	for _, tc := range []struct{ stored, onTheWire string }{
		{nameRankNone, wire.CandidateNameRankNone},
		{nameRankMachine, wire.CandidateNameRankMachine},
		{nameRankModel, wire.CandidateNameRankModel},
		{nameRankFriendly, wire.CandidateNameRankFriendly},
	} {
		if tc.stored != tc.onTheWire {
			t.Errorf("stored %q but relay/1 spells it %q", tc.stored, tc.onTheWire)
		}
		if nameRankFact(tc.onTheWire) != tc.onTheWire {
			t.Errorf("nameRankFact(%q) = %q — a token relay/1 publishes must survive the clamp verbatim", tc.onTheWire, nameRankFact(tc.onTheWire))
		}
	}
	// The ladder's own order, so a re-spelled token cannot quietly flatten it.
	if !(nameRankOrder(nameRankFriendly) > nameRankOrder(nameRankModel) &&
		nameRankOrder(nameRankModel) > nameRankOrder(nameRankMachine) &&
		nameRankOrder(nameRankMachine) > nameRankOrder(nameRankNone)) {
		t.Fatalf("the app-side ladder is not strictly ordered: none=%d machine=%d model=%d friendly=%d",
			nameRankOrder(nameRankNone), nameRankOrder(nameRankMachine), nameRankOrder(nameRankModel), nameRankOrder(nameRankFriendly))
	}
	// Unrecorded is at the BOTTOM, which is what makes an upgraded row refuse
	// nothing. Stated as its own assertion because it is a decision, not a
	// consequence.
	if nameRankOrder(nameRankUnrecorded) > nameRankOrder(nameRankNone) {
		t.Fatalf("unrecorded (%d) outranks none (%d) — an upgraded row would then refuse reports, and a rename could never land on it",
			nameRankOrder(nameRankUnrecorded), nameRankOrder(nameRankNone))
	}
}
