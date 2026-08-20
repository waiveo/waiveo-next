package devices

import "testing"

// The two age members are one fact in two fields, and Device's own doc fixes the
// rule: FirstSeenOrigin is "absent exactly when FirstSeen is". This pins that,
// because Go lets a caller clear one and not the other and the retire handler's
// fallback path did precisely that — it served `first_seen_origin` for an age it
// had just removed.
//
// The assertion is on BOTH directions of the invariant, not just the one the
// defect hit: a body carrying an origin without an instant is the bug, and a
// method that dropped the instant while a caller expected the rest of the device
// intact would be a different one.
func TestWithoutAgeClearsBothMembersTogether(t *testing.T) {
	d := Device{
		ID:              "01J8ZDEV1CE00000000000000A",
		Name:            "Hanger TV",
		DeviceClass:     "media-player",
		FirstSeen:       1_787_098_315_675,
		LastSeen:        1_787_199_198_661,
		FirstSeenOrigin: "adopted",
	}

	got := d.WithoutAge()

	if got.FirstSeen != 0 {
		t.Errorf("FirstSeen = %d, want 0 — the age was not cleared", got.FirstSeen)
	}
	if got.FirstSeenOrigin != "" {
		t.Errorf("FirstSeenOrigin = %q, want empty — an origin without an instant "+
			"qualifies a value this deployment no longer holds", got.FirstSeenOrigin)
	}

	// LastSeen is a different fact answered by a different rule (see Device). A
	// retire that took it too would report a device that reported a minute ago as
	// never heard from.
	if got.LastSeen != d.LastSeen {
		t.Errorf("LastSeen = %d, want %d — WithoutAge took a fact it does not own", got.LastSeen, d.LastSeen)
	}
	if got.ID != d.ID || got.Name != d.Name || got.DeviceClass != d.DeviceClass {
		t.Errorf("WithoutAge altered identity or facts beyond the age pair: %+v", got)
	}

	// Value receiver: the caller's device is untouched, so a handler that still
	// holds the pre-retire body for an error path has not been quietly edited.
	if d.FirstSeen == 0 || d.FirstSeenOrigin == "" {
		t.Errorf("WithoutAge mutated its receiver; the caller's copy lost its age")
	}
}

// An already-ageless device is the common case on a box whose ledger has no
// answer for a device yet, and the operation is naturally idempotent there.
func TestWithoutAgeOnADeviceThatHasNoAgeChangesNothing(t *testing.T) {
	d := Device{ID: "01J8ZDEV1CE00000000000000B", Name: "unnamed", LastSeen: 1_787_199_198_661}
	got := d.WithoutAge()
	if got.FirstSeen != 0 || got.FirstSeenOrigin != "" {
		t.Errorf("WithoutAge invented an age on an ageless device: %+v", got)
	}
	if got.ID != d.ID || got.Name != d.Name || got.LastSeen != d.LastSeen {
		t.Errorf("WithoutAge on an ageless device changed something else: %+v", got)
	}
}
