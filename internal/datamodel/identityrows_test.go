package datamodel

import (
	"encoding/json"
	"strings"
	"testing"
)

// identityrows_test.go exercises ValidateIdentityRows: the DAT-005b baseline over
// a screen identity row and a device row, the relay/1 REL-063 device shape, the
// REL-153 (driver, native_id) identity rule, and player/1 PLY-124's optional
// screen→device reference.
//
// Every case builds its rows from literal field values and asserts the codes the
// contract names — never a code read back out of the validator's own output.

// Two canonical ULIDs, used as row ids throughout.
const (
	idScreenA = "01J8Z4SCREENR0WAAAAAAAAAA1"
	idScreenB = "01J8Z4SCREENR0WBBBBBBBBBB2"
	idDeviceA = "01J8Z4DEVCEADPTEDAAAAAAAA1"
	idDeviceB = "01J8Z4DEVCEADPTEDBBBBBBBB2"
	idNode    = "01J8Z4SCPEN0DEPACEMENTAAA1"
)

func rawOf(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

// goodDevice is a device row that satisfies every rule, so a case that alters one
// field isolates exactly that rule.
func goodDevice(id, driver, nativeID string) Device {
	cadence := 30
	return Device{
		ID:                 id,
		ScopeNode:          idNode,
		Name:               "Lobby Roku",
		Driver:             driver,
		NativeID:           nativeID,
		PollCadenceSeconds: &cadence,
		Entities: []DeviceEntity{{
			EntityID:    "01J8Z4ENTTYMEDAPAYERAAAAA1",
			DeviceClass: "media-player",
			Enabled:     true,
			DisplayName: "Lobby Roku",
			Category:    "primary",
		}},
	}
}

func goodScreen(id string, deviceID *string) Screen {
	return Screen{ID: id, ScopeNode: idNode, Name: "Lobby Screen", DeviceID: deviceID}
}

// codes flattens a validation result to its error codes, for set comparison.
func codes(errs []Error) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Code)
	}
	return out
}

func hasCode(errs []Error, code string) bool {
	for _, e := range errs {
		if e.Code == code {
			return true
		}
	}
	return false
}

// TestValidIdentityRowsAccepted pins the positive case: a device row and a screen
// row that links to it both validate, and both come back typed.
func TestValidIdentityRowsAccepted(t *testing.T) {
	dev := idDeviceA
	set, errs := ValidateIdentityRows(RawIdentityRows{
		Devices: []json.RawMessage{rawOf(t, goodDevice(idDeviceA, "roku-ecp", "10.0.0.41"))},
		Screens: []json.RawMessage{rawOf(t, goodScreen(idScreenA, &dev))},
	})
	if len(errs) != 0 {
		t.Fatalf("a conformant bundle was rejected: %v", codes(errs))
	}
	if len(set.Devices) != 1 || len(set.Screens) != 1 {
		t.Fatalf("typed set = %d device(s)/%d screen(s), want 1/1", len(set.Devices), len(set.Screens))
	}
	if set.Screens[0].DeviceID == nil || *set.Screens[0].DeviceID != idDeviceA {
		t.Fatalf("the screen's PLY-124 device link did not survive parsing: %v", set.Screens[0].DeviceID)
	}
	if set.Devices[0].Entities[0].Category != "primary" {
		t.Fatalf("entity category did not survive parsing: %q", set.Devices[0].Entities[0].Category)
	}
}

// TestScreenWithNoDeviceLinkAccepted pins that PLY-124's reference is genuinely
// optional — the common case (a screen with no adopted device bound) is not an
// error.
func TestScreenWithNoDeviceLinkAccepted(t *testing.T) {
	_, errs := ValidateIdentityRows(RawIdentityRows{
		Screens: []json.RawMessage{rawOf(t, goodScreen(idScreenA, nil))},
	})
	if len(errs) != 0 {
		t.Fatalf("an unlinked screen was rejected: %v", codes(errs))
	}
}

// TestIdentityRowIDMustBeCanonicalULID pins DAT-005a over both new kinds.
func TestIdentityRowIDMustBeCanonicalULID(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   RawIdentityRows
	}{
		{"screen", RawIdentityRows{Screens: []json.RawMessage{rawOf(t, goodScreen("screen-first-photon", nil))}}},
		{"device", RawIdentityRows{Devices: []json.RawMessage{rawOf(t, goodDevice("dev-1", "roku-ecp", "10.0.0.41"))}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := ValidateIdentityRows(tc.in)
			if !hasCode(errs, "ROW_ID_INVALID") {
				t.Fatalf("a non-ULID %s id was accepted (codes %v)", tc.name, codes(errs))
			}
		})
	}
}

// TestIdentityRowRequiresPlacementAndName pins DAT-006's scope_node and the
// name bound, on both kinds.
func TestIdentityRowRequiresPlacementAndName(t *testing.T) {
	unplaced := goodScreen(idScreenA, nil)
	unplaced.ScopeNode = ""
	unplaced.Name = "   "
	_, errs := ValidateIdentityRows(RawIdentityRows{Screens: []json.RawMessage{rawOf(t, unplaced)}})
	if !hasCode(errs, "ROW_SCOPE_NODE_MISSING") || !hasCode(errs, "ROW_NAME_INVALID") {
		t.Fatalf("an unplaced, unnamed screen was accepted (codes %v)", codes(errs))
	}

	overlong := goodDevice(idDeviceA, "roku-ecp", "10.0.0.41")
	overlong.Name = strings.Repeat("é", 201)
	_, errs = ValidateIdentityRows(RawIdentityRows{Devices: []json.RawMessage{rawOf(t, overlong)}})
	if !hasCode(errs, "ROW_NAME_INVALID") {
		t.Fatalf("a 201-character device name was accepted (codes %v)", codes(errs))
	}

	// 200 code points of a multi-byte character is 400 bytes: the bound is
	// counted in characters, so this one is accepted.
	atBound := goodDevice(idDeviceA, "roku-ecp", "10.0.0.41")
	atBound.Name = strings.Repeat("é", 200)
	if _, errs := ValidateIdentityRows(RawIdentityRows{Devices: []json.RawMessage{rawOf(t, atBound)}}); len(errs) != 0 {
		t.Fatalf("a 200-character (400-byte) device name was rejected: %v", codes(errs))
	}
}

// TestDeviceIdentityTupleMustBeComplete pins REL-063: driver and native_id are
// the tuple a device_id's identity is scoped to, so neither may be blank.
func TestDeviceIdentityTupleMustBeComplete(t *testing.T) {
	for _, tc := range []struct{ name, driver, native string }{
		{"no driver", "", "10.0.0.41"},
		{"no native_id", "roku-ecp", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := ValidateIdentityRows(RawIdentityRows{
				Devices: []json.RawMessage{rawOf(t, goodDevice(idDeviceA, tc.driver, tc.native))},
			})
			if !hasCode(errs, "DEVICE_IDENTITY_INCOMPLETE") {
				t.Fatalf("a device with %s was accepted (codes %v)", tc.name, codes(errs))
			}
		})
	}
}

// TestDeviceIdentityTupleIsUnique pins REL-153: re-adopting one physical device
// MUST resolve to one device_id, so two rows cannot claim the same
// (driver, native_id) — and the SECOND claimant is the one reported.
func TestDeviceIdentityTupleIsUnique(t *testing.T) {
	_, errs := ValidateIdentityRows(RawIdentityRows{Devices: []json.RawMessage{
		rawOf(t, goodDevice(idDeviceA, "roku-ecp", "10.0.0.41")),
		rawOf(t, goodDevice(idDeviceB, "roku-ecp", "10.0.0.41")),
	}})
	if !hasCode(errs, "DEVICE_IDENTITY_DUPLICATE") {
		t.Fatalf("two device rows claimed one (driver, native_id) and were accepted (codes %v)", codes(errs))
	}
	// The message must name the row that already held the tuple, not the one
	// being refused — otherwise an operator cannot find the existing row.
	var found bool
	for _, e := range errs {
		if e.Code == "DEVICE_IDENTITY_DUPLICATE" {
			found = strings.Contains(e.Message, idDeviceA)
		}
	}
	if !found {
		t.Fatalf("the duplicate-identity error does not name the prior claimant %s: %v", idDeviceA, errs)
	}

	// The same native_id under a DIFFERENT driver is a different physical
	// device and must be accepted.
	if _, errs := ValidateIdentityRows(RawIdentityRows{Devices: []json.RawMessage{
		rawOf(t, goodDevice(idDeviceA, "roku-ecp", "10.0.0.41")),
		rawOf(t, goodDevice(idDeviceB, "hue-bridge", "10.0.0.41")),
	}}); len(errs) != 0 {
		t.Fatalf("two drivers sharing a native_id were rejected: %v", codes(errs))
	}
}

// TestDeviceEntityPolicyValidated pins REL-063's entity members and its closed
// `category` vocabulary, plus the within-row entity_id uniqueness a command's
// policy lookup depends on.
func TestDeviceEntityPolicyValidated(t *testing.T) {
	d := goodDevice(idDeviceA, "roku-ecp", "10.0.0.41")
	d.Entities = []DeviceEntity{
		{EntityID: "", DeviceClass: "", Category: "primary"},
		{EntityID: "01J8Z4ENTTYMEDAPAYERAAAAA1", DeviceClass: "media-player", Category: "auxiliary"},
		{EntityID: "01J8Z4ENTTYMEDAPAYERAAAAA1", DeviceClass: "media-player", Category: "diagnostic"},
		{EntityID: "entity-1", DeviceClass: "media-player", Category: "primary"},
	}
	_, errs := ValidateIdentityRows(RawIdentityRows{Devices: []json.RawMessage{rawOf(t, d)}})
	for _, want := range []string{"ENTITY_ID_MISSING", "ENTITY_ID_INVALID", "DEVICE_CLASS_MISSING", "ENTITY_CATEGORY_INVALID", "ENTITY_ID_DUPLICATE"} {
		if !hasCode(errs, want) {
			t.Errorf("expected %s among the reported codes, got %v", want, codes(errs))
		}
	}
	// The field path must locate the offending entity by index, so a 422 body
	// points at one array element rather than at the whole array.
	var located bool
	for _, e := range errs {
		if e.Code == "ENTITY_CATEGORY_INVALID" && e.Field == "entities[1].category" {
			located = true
		}
	}
	if !located {
		t.Errorf("the invalid category was not located at entities[1].category: %v", errs)
	}
}

// TestPollCadenceMustBePositive pins that a stated cadence is a real interval.
func TestPollCadenceMustBePositive(t *testing.T) {
	zero := 0
	d := goodDevice(idDeviceA, "roku-ecp", "10.0.0.41")
	d.PollCadenceSeconds = &zero
	if _, errs := ValidateIdentityRows(RawIdentityRows{Devices: []json.RawMessage{rawOf(t, d)}}); !hasCode(errs, "POLL_CADENCE_INVALID") {
		t.Fatalf("a zero poll_cadence_seconds was accepted (codes %v)", codes(errs))
	}
	// An ABSENT cadence is "this deployment has not stated one" and is fine.
	d.PollCadenceSeconds = nil
	if _, errs := ValidateIdentityRows(RawIdentityRows{Devices: []json.RawMessage{rawOf(t, d)}}); len(errs) != 0 {
		t.Fatalf("an absent poll_cadence_seconds was rejected: %v", codes(errs))
	}
}

// TestScreenDeviceLinkMustResolve pins PLY-124: the reference is optional, but a
// stated one names a device row that exists.
func TestScreenDeviceLinkMustResolve(t *testing.T) {
	missing := idDeviceB
	_, errs := ValidateIdentityRows(RawIdentityRows{
		Devices: []json.RawMessage{rawOf(t, goodDevice(idDeviceA, "roku-ecp", "10.0.0.41"))},
		Screens: []json.RawMessage{rawOf(t, goodScreen(idScreenA, &missing))},
	})
	if !hasCode(errs, "REFERENCE_INVALID") {
		t.Fatalf("a screen linked to a device row that does not exist was accepted (codes %v)", codes(errs))
	}

	// A present-but-blank link is not "no link" — the absent pointer already
	// spells that — so it is refused rather than silently treated as unset.
	blank := ""
	_, errs = ValidateIdentityRows(RawIdentityRows{Screens: []json.RawMessage{rawOf(t, goodScreen(idScreenB, &blank))}})
	if !hasCode(errs, "REFERENCE_INVALID") {
		t.Fatalf("a blank device_id was accepted as an absent link (codes %v)", codes(errs))
	}
}

// TestMalformedIdentityRowReported pins that an unparseable row is reported
// rather than skipped in silence.
func TestMalformedIdentityRowReported(t *testing.T) {
	_, errs := ValidateIdentityRows(RawIdentityRows{
		Screens: []json.RawMessage{json.RawMessage(`{"id": 17}`)},
	})
	if !hasCode(errs, "ROW_MALFORMED") {
		t.Fatalf("a malformed screen row was not reported (codes %v)", codes(errs))
	}
}
