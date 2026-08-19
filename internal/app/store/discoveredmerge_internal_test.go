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
// ranked facts: the address's port and the device class, whose quality this side
// re-derives from the value itself, and the NAME, whose quality is not in the
// string and therefore had to travel — REL-110c's `name_rank`. The name was the
// one this file used to say it deliberately could not rank; the test that said
// so has been replaced, because it pinned the absence of the fix.
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
