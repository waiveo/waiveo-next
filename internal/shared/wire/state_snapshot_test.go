package wire

import (
	"encoding/json"
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
