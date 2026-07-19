package wire

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// TestStateSnapshotBodyFieldNames asserts a round-trip JSON marshal of a
// StateSnapshotBody carries exactly the relay/1 `state.snapshot` body's
// contract field names (REL-051), and that `sections` carries exactly the
// 7 REL-060 keys, every one of them present — never omitted, even when
// empty.
func TestStateSnapshotBodyFieldNames(t *testing.T) {
	body := StateSnapshotBody{
		Generation: 1,
		Hash:       "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		Signature:  "c2lnbmF0dXJl",
		Sections: Sections{
			ScreenPrograms: []ScreenProgram{
				{
					ScreenID:        "screen-1",
					ProgramRevision: "rev-1",
					Priority:        "scheduled",
					Display:         "content",
					Content: []ContentRef{
						{
							AssetRef:  "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85",
							URL:       "https://origin.example/content/e3b0c4",
							ExpiresAt: 1752541200000,
						},
					},
				},
			},
			EdgeRules: EdgeRules{
				RulesMinorVersion: "",
				Rules:             []json.RawMessage{},
			},
			DeviceInventory: DeviceInventory{
				Devices:           []json.RawMessage{},
				PackMatchPatterns: []json.RawMessage{},
			},
			Schedule: ScheduleSection{}.Normalized(),
			RevocationAndSite: RevocationAndSite{
				Revoked:       []string{},
				SiteEffective: SiteEffective{},
			},
			PairingGrants: []PairingGrant{},
		},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}

	for _, k := range []string{"generation", "hash", "signature", "sections"} {
		if _, ok := got[k]; !ok {
			t.Errorf("StateSnapshotBody JSON missing contract field %q; got %s", k, raw)
		}
	}
	if _, ok := got["signed_with_key"]; ok {
		t.Errorf("StateSnapshotBody JSON carries signed_with_key when unset (should be omitted); got %s", raw)
	}

	var sections map[string]json.RawMessage
	if err := json.Unmarshal(got["sections"], &sections); err != nil {
		t.Fatalf("Unmarshal sections: %v", err)
	}

	wantKeys := []string{
		"screen_programs",
		"edge_rules",
		"device_inventory",
		"schedule",
		"revocation_and_site",
		"pairing_grants",
		"workflow_generation",
	}
	if len(sections) != len(wantKeys) {
		t.Fatalf("sections marshaled to %d keys, want exactly %d (%v); got %s", len(sections), len(wantKeys), wantKeys, got["sections"])
	}
	for _, k := range wantKeys {
		raw, ok := sections[k]
		if !ok {
			t.Errorf("sections JSON missing REL-060 key %q; got %s", k, got["sections"])
			continue
		}
		if string(raw) == "" {
			t.Errorf("sections key %q is present but empty (not even null) — REL-060 requires an explicit value; got %s", k, got["sections"])
		}
	}

	// pairing_grants and screen_programs must be present as arrays
	// ("[...]"), never JSON null, even when empty — REL-060's "empty array
	// ... never an omitted key".
	if string(sections["pairing_grants"]) != "[]" {
		t.Errorf("sections.pairing_grants = %s, want [] (empty array, not null)", sections["pairing_grants"])
	}

	// schedule is the newly-typed ScheduleSection (REL-065): an object
	// carrying exactly the seven scheduling-core row arrays, each `[]` when
	// empty — never null, never an omitted array (REL-060).
	var sched map[string]json.RawMessage
	if err := json.Unmarshal(sections["schedule"], &sched); err != nil {
		t.Fatalf("Unmarshal schedule section: %v; got %s", err, sections["schedule"])
	}
	schedKeys := []string{
		"scope_nodes",
		"playlists",
		"schedules",
		"validity_windows",
		"dayparts",
		"fallbacks",
		"preset_batches",
	}
	if len(sched) != len(schedKeys) {
		t.Errorf("schedule section marshaled to %d keys, want exactly %d (%v); got %s", len(sched), len(schedKeys), schedKeys, sections["schedule"])
	}
	for _, k := range schedKeys {
		if string(sched[k]) != "[]" {
			t.Errorf("schedule.%s = %s, want [] (empty array, never null — REL-060/065)", k, sched[k])
		}
	}

	// Round-trip.
	var back StateSnapshotBody
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("round-trip Unmarshal: %v", err)
	}
}

// TestValidateSectionsComplete asserts the REL-060 structural completeness
// gate: a raw `sections` object carrying all seven keys passes, one missing
// ANY single key fails, and a key present with a JSON `null` value (as
// `workflow_generation` carries in this version, REL-068) counts as present.
func TestValidateSectionsComplete(t *testing.T) {
	full := Sections{
		ScreenPrograms:     []ScreenProgram{},
		EdgeRules:          EdgeRules{Rules: []json.RawMessage{}},
		DeviceInventory:    DeviceInventory{Devices: []json.RawMessage{}, PackMatchPatterns: []json.RawMessage{}},
		RevocationAndSite:  RevocationAndSite{Revoked: []string{}, SiteEffective: SiteEffective{}},
		PairingGrants:      []PairingGrant{},
		WorkflowGeneration: nil, // marshals as JSON null — REL-068 empty placeholder
	}
	rawFull, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("Marshal full sections: %v", err)
	}

	if err := ValidateSectionsComplete(rawFull); err != nil {
		t.Fatalf("ValidateSectionsComplete(full) = %v, want nil (all seven keys present)", err)
	}

	// workflow_generation is present but null — must still count as present.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rawFull, &m); err != nil {
		t.Fatalf("Unmarshal full sections into map: %v", err)
	}
	if string(m["workflow_generation"]) != "null" {
		t.Fatalf("precondition: workflow_generation marshaled to %q, want null", m["workflow_generation"])
	}

	// Removing ANY one of the seven keys must fail the gate.
	for _, k := range SectionKeys {
		var missing map[string]json.RawMessage
		if err := json.Unmarshal(rawFull, &missing); err != nil {
			t.Fatalf("Unmarshal for %q removal: %v", k, err)
		}
		delete(missing, k)
		rawMissing, err := json.Marshal(missing)
		if err != nil {
			t.Fatalf("Marshal sections without %q: %v", k, err)
		}
		if err := ValidateSectionsComplete(rawMissing); err == nil {
			t.Errorf("ValidateSectionsComplete(sections without %q) = nil, want an error (REL-060)", k)
		}
	}

	// A non-object sections value is refused rather than panicking.
	if err := ValidateSectionsComplete(json.RawMessage(`[]`)); err == nil {
		t.Error("ValidateSectionsComplete(non-object) = nil, want an error")
	}
}

// TestScheduleSectionNormalizesNilToEmptyArrays asserts ScheduleSection's
// Normalized() replaces every nil row slice with a non-nil empty slice, so
// each of the seven scheduling-core arrays marshals as `[]` — never `null`,
// never an omitted key (REL-060). This is what keeps a schedule-carrying
// snapshot REL-060-complete even when the feeder has nothing to populate a
// row array with (today's empty-but-typed first-photon state).
func TestScheduleSectionNormalizesNilToEmptyArrays(t *testing.T) {
	// A zero-value ScheduleSection has all seven nil slices.
	norm := ScheduleSection{}.Normalized()

	raw, err := json.Marshal(norm)
	if err != nil {
		t.Fatalf("Marshal normalized ScheduleSection: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}

	wantKeys := []string{
		"scope_nodes",
		"playlists",
		"schedules",
		"validity_windows",
		"dayparts",
		"fallbacks",
		"preset_batches",
	}
	if len(got) != len(wantKeys) {
		t.Fatalf("ScheduleSection marshaled to %d keys, want exactly %d (%v); got %s", len(got), len(wantKeys), wantKeys, raw)
	}
	for _, k := range wantKeys {
		v, ok := got[k]
		if !ok {
			t.Errorf("ScheduleSection JSON missing key %q; got %s", k, raw)
			continue
		}
		if string(v) != "[]" {
			t.Errorf("ScheduleSection.%s = %s, want [] (empty array, never null — REL-060/065)", k, v)
		}
	}
}

// TestScheduleSectionMarshalsStableAndRoundTrips asserts a Sections value
// carrying a populated ScheduleSection (REL-065) preserves the byte-identical
// marshaling → hash invariant (REL-053): HashSections is stable across
// repeated marshals, and a full wire round-trip (marshal → unmarshal →
// marshal) is byte-identical, so a relay recomputing the hash over the
// received sections reproduces the feeder's hash exactly. The schedule
// section's opaque rows round-trip unmodified.
func TestScheduleSectionMarshalsStableAndRoundTrips(t *testing.T) {
	sec := ScheduleSection{
		ScopeNodes: []json.RawMessage{
			json.RawMessage(`{"id":"01ARZ3NDEKTSV4RRFFQ69G5FA1","revision":1,"kind":"site","tz":"America/Chicago"}`),
			json.RawMessage(`{"id":"01ARZ3NDEKTSV4RRFFQ69G5FA2","revision":1,"kind":"screen","parent":"01ARZ3NDEKTSV4RRFFQ69G5FA1"}`),
		},
		Playlists: []json.RawMessage{json.RawMessage(`{"id":"01ARZ3NDEKTSV4RRFFQ69G5FA3","revision":1}`)},
		Schedules: []json.RawMessage{json.RawMessage(`{"id":"01ARZ3NDEKTSV4RRFFQ69G5FA4","revision":1,"scope_node":"01ARZ3NDEKTSV4RRFFQ69G5FA2"}`)},
		Dayparts:  []json.RawMessage{json.RawMessage(`{"id":"01ARZ3NDEKTSV4RRFFQ69G5FA5","revision":1}`)},
	}.Normalized()

	sections := Sections{
		ScreenPrograms:     []ScreenProgram{},
		EdgeRules:          EdgeRules{Rules: []json.RawMessage{}},
		DeviceInventory:    DeviceInventory{Devices: []json.RawMessage{}, PackMatchPatterns: []json.RawMessage{}},
		Schedule:           sec,
		RevocationAndSite:  RevocationAndSite{Revoked: []string{}, SiteEffective: SiteEffective{}},
		PairingGrants:      []PairingGrant{},
		WorkflowGeneration: nil,
	}

	h1, err := HashSections(sections)
	if err != nil {
		t.Fatalf("HashSections #1: %v", err)
	}
	h2, err := HashSections(sections)
	if err != nil {
		t.Fatalf("HashSections #2: %v", err)
	}
	if h1 != h2 {
		t.Errorf("HashSections not stable across marshals: %q vs %q (byte-identical marshaling → hash invariant, REL-053/060)", h1, h2)
	}

	raw1, err := json.Marshal(sections)
	if err != nil {
		t.Fatalf("Marshal sections: %v", err)
	}
	var back Sections
	if err := json.Unmarshal(raw1, &back); err != nil {
		t.Fatalf("Unmarshal sections: %v", err)
	}
	raw2, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("re-Marshal round-tripped sections: %v", err)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Errorf("Sections round-trip not byte-identical:\n first=%s\n back =%s", raw1, raw2)
	}

	hBack, err := HashSections(back)
	if err != nil {
		t.Fatalf("HashSections(round-tripped): %v", err)
	}
	if hBack != h1 {
		t.Errorf("hash changed across a wire round-trip: %q → %q (byte-identical marshaling → hash invariant broken)", h1, hBack)
	}

	if !reflect.DeepEqual(back.Schedule, sec) {
		t.Errorf("Schedule round-trip mismatch:\n got  %+v\n want %+v", back.Schedule, sec)
	}
}

// TestSectionKeysAreTheSevenRELKeys asserts SectionKeys is exactly the seven
// REL-060 keys, in the contract's declared order.
func TestSectionKeysAreTheSevenRELKeys(t *testing.T) {
	want := []string{
		"screen_programs",
		"edge_rules",
		"device_inventory",
		"schedule",
		"revocation_and_site",
		"pairing_grants",
		"workflow_generation",
	}
	if len(SectionKeys) != len(want) {
		t.Fatalf("SectionKeys = %v, want %v", SectionKeys, want)
	}
	for i, k := range want {
		if SectionKeys[i] != k {
			t.Errorf("SectionKeys[%d] = %q, want %q", i, SectionKeys[i], k)
		}
	}
}
