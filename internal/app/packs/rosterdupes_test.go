package packs_test

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// TestLoadRosterRefusesADuplicatedMemberName closes the gap the sweep in issue
// #178 found in this file's own coverage.
//
// TestLoadRosterUnresolvableNeverDegradesToEmpty drives sixteen malformed
// documents and none of them declares a member NAME twice — its "duplicated pack
// id" case is two ENTRIES naming the same pack, which a different check refuses.
// So the scanner that walks the token stream looking for a repeated name could be
// deleted with the whole tree green.
//
// WHAT MAKES THIS DIFFERENT FROM AN ORDINARY MISSING CASE is that Go's decoder
// cannot see the fault at all. A duplicate is invisible to Decode, to
// DisallowUnknownFields, and to the trailing-content check: the decoder silently
// keeps the LAST of two same-named members and reports success. So without the
// scanner these documents do not merely slip through — they load, resolve, and
// report themselves as a valid roster.
//
// For a document whose whole job is to declare restrictions, last-wins runs the
// wrong way. The later value is the one an appender controls, and it is enough to
// append: no rewrite, no reordering, nothing that changes a byte already present.
//
//   - A second `required` lifts EVERY floor, because the empty later value wins
//     and the roster resolves to "nothing is required" while still reading, to a
//     human, as the roster that named the packs above.
//   - A second `pack_id` inside an entry makes the visible id differ from the one
//     that binds: the file names waiveo/system and the deployment pins something
//     else at that floor.
//   - A second `floor_version` is the same trick on the other field — a floor a
//     reader sees as 9.9.9 and the platform enforces at 0.0.1.
//
// Each is asserted to leave the roster UNRESOLVED as well as erroring, because
// this file's central property is that an unreadable roster refuses everything
// rather than degrading to empty — and "empty" is exactly what the first case
// would produce.
func TestLoadRosterRefusesADuplicatedMemberName(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{
			"a second required list, empty, lifting every floor",
			`{"format":"required-packs/1",
			  "required":[{"pack_id":"waiveo/system","floor_version":"1.4.0"}],
			  "required":[]}`,
		},
		{
			"a second required list naming a different pack",
			`{"format":"required-packs/1",
			  "required":[{"pack_id":"waiveo/system","floor_version":"1.4.0"}],
			  "required":[{"pack_id":"acme/menu-board","floor_version":"0.0.1"}]}`,
		},
		{
			"a second pack_id inside an entry",
			`{"format":"required-packs/1",
			  "required":[{"pack_id":"waiveo/system","pack_id":"acme/menu-board","floor_version":"1.4.0"}]}`,
		},
		{
			"a second floor_version inside an entry",
			`{"format":"required-packs/1",
			  "required":[{"pack_id":"waiveo/system","floor_version":"9.9.9","floor_version":"0.0.1"}]}`,
		},
		{
			"a second format discriminant",
			`{"format":"required-packs/1","format":"pack-trust-anchors/1",
			  "required":[{"pack_id":"waiveo/system","floor_version":"1.4.0"}]}`,
		},
		{
			"the duplicate in the second of two entries",
			`{"format":"required-packs/1","required":[
			   {"pack_id":"waiveo/system","floor_version":"1.4.0"},
			   {"pack_id":"acme/menu-board","floor_version":"2.0.0","floor_version":"0.0.1"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := packs.LoadRoster(writeRoster(t, tc.body))
			if err == nil {
				t.Fatalf("LoadRoster ACCEPTED a roster with %s — Go's decoder keeps the last of two same-named "+
					"members and reports success, so this document does not slip through a check, it loads and "+
					"resolves as a valid roster: %s", tc.name, tc.body)
			}
			assertRefusesEverything(t, r, tc.name)
		})
	}
}

// TestLoadRosterAcceptsRepeatedNamesInDifferentObjects is the control, and it is
// the case the scanner is most likely to get wrong in the other direction.
//
// Every entry in a roster declares `pack_id` and `floor_version`, so the same
// names recur throughout the document — a scanner tracking names per DOCUMENT
// rather than per OBJECT would refuse every roster with more than one entry. And
// a scanner that did not distinguish a member name from a string VALUE would
// refuse any document where a value repeats, which two entries sharing a floor
// do.
//
// Without this control, the refusals above are satisfied by a check that refuses
// every roster on the platform.
func TestLoadRosterAcceptsRepeatedNamesInDifferentObjects(t *testing.T) {
	r, err := packs.LoadRoster(writeRoster(t, `{
	  "format": "required-packs/1",
	  "required": [
	    { "pack_id": "waiveo/system",   "floor_version": "1.4.0" },
	    { "pack_id": "acme/menu-board", "floor_version": "1.4.0" },
	    { "pack_id": "acme/signage",    "floor_version": "2.0.0" }
	  ]
	}`))
	if err != nil {
		t.Fatalf("a roster whose entries all declare pack_id and floor_version was refused: %v — those names recur "+
			"once per entry by construction, so refusing them refuses every multi-entry roster", err)
	}
	if !r.Resolved() {
		t.Fatal("a valid three-entry roster resolved to the unresolved value")
	}
	// The two entries sharing floor 1.4.0 are the repeated-VALUE half: a scanner
	// that mistook a value for a name would have refused above.
	for _, want := range []struct{ id, floor string }{
		{"waiveo/system", "1.4.0"},
		{"acme/menu-board", "1.4.0"},
		{"acme/signage", "2.0.0"},
	} {
		floor, required := r.RequiredFloor(want.id)
		if !required || floor != want.floor {
			t.Errorf("%s = (%q, %v), want (%q, true)", want.id, floor, required, want.floor)
		}
	}
}
