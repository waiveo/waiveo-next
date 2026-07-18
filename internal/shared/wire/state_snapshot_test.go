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
