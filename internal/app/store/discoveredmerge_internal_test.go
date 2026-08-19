package store

import "testing"

// discoveredmerge_internal_test.go covers mergeDiscovered's rules directly,
// because they are the DURABLE half of the same defect the relay's in-memory
// merge has: a report that learned something WORSE is not the same as a report
// that learned nothing, and presence alone cannot tell them apart.
//
// A relay-side fix the mirror then undoes is not a fix. These pin the two rules
// this side can enforce without a contract change — the address's port and the
// device class — and the one it deliberately cannot (the name, whose quality
// lives in a record REL-110a does not carry).
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

// THE LIMIT, stated as a test so it is a decision rather than an oversight. The
// name is NOT ranked here: telling a display name from a machine-generated mDNS
// instance label needs to know which record authored it, and REL-110a's
// candidate carries no such member. The relay ranks it where that knowledge
// exists; this side stores what the relay reports. Presence is still enforced.
func TestTheMirrorTakesTheRelaysNameButStillRefusesAnEmptyOne(t *testing.T) {
	renamed := mergeDiscovered(
		DiscoveredDevice{Name: "onn. 4K Streaming Box"},
		DiscoveredDevice{Name: "Living Room"},
	).Name
	if renamed != "Living Room" {
		t.Errorf("name = %q, want the relay's newer answer — the relay owns this ranking, and a mirror that second-guessed it could not be renamed", renamed)
	}

	silent := mergeDiscovered(
		DiscoveredDevice{Name: "onn. 4K Streaming Box"},
		DiscoveredDevice{Name: ""},
	).Name
	if silent != "onn. 4K Streaming Box" {
		t.Errorf("name = %q, want the held name kept — a report that carries no name did not learn one", silent)
	}
}
