package grant

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

func grantJSON(t *testing.T, g wire.PairingGrant) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("json.Marshal(grant): %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("json.Unmarshal into map: %v", err)
	}
	return m
}

// TestMintShape asserts Mint produces a well-formed relay/1 REL-121
// pairing-grant record: all 6 fields populated, redemption_mode exactly
// "one-time" (this package's Wave-1 first-photon choice — REL-121 permits
// "multi" too, but a first-photon grant is redeemed exactly once).
func TestMintShape(t *testing.T) {
	g := Mint()

	if g.GrantID == "" {
		t.Error("GrantID is empty, want a non-empty unique id")
	}
	if g.Purpose == "" {
		t.Error("Purpose is empty")
	}
	if g.ResultingPrincipalKind == "" {
		t.Error("ResultingPrincipalKind is empty")
	}
	if g.TTL <= 0 {
		t.Errorf("TTL = %d, want > 0", g.TTL)
	}
	if g.RedemptionMode != "one-time" {
		t.Errorf("RedemptionMode = %q, want %q", g.RedemptionMode, "one-time")
	}
	if g.IssuedAt <= 0 {
		t.Errorf("IssuedAt = %d, want a positive epoch-ms timestamp", g.IssuedAt)
	}
}

// TestMintUnique asserts two Mint() calls produce distinct grant_id values
// — REL-121 grants are individually redeemable records, so a grant_id must
// not collide across mints.
func TestMintUnique(t *testing.T) {
	a := Mint()
	b := Mint()

	if a.GrantID == b.GrantID {
		t.Errorf("two Mint() calls produced the same GrantID %q, want distinct ids", a.GrantID)
	}
}

// TestMintNoREL126Fields asserts Mint's record carries none of REL-126's
// pairing-code fields (grant_selector, fingerprint_commitment) — those are
// relay-side pairing-code concerns computed later against this grant's own
// grant_id and the relay's own trust anchor, never part of the REL-121
// grant record itself.
func TestMintNoREL126Fields(t *testing.T) {
	g := Mint()

	raw := grantJSON(t, g)
	for _, forbidden := range []string{"grant_selector", "fingerprint_commitment"} {
		if _, ok := raw[forbidden]; ok {
			t.Errorf("grant record carries forbidden REL-126 field %q — that's a relay-side pairing-code concern, not part of REL-121", forbidden)
		}
	}

	wantKeys := []string{"grant_id", "purpose", "resulting_principal_kind", "ttl", "redemption_mode", "issued_at"}
	if len(raw) != len(wantKeys) {
		t.Fatalf("grant record marshaled to %d keys, want exactly %d (%v); got %v", len(raw), len(wantKeys), wantKeys, raw)
	}
	for _, k := range wantKeys {
		if _, ok := raw[k]; !ok {
			t.Errorf("grant record missing REL-121 key %q", k)
		}
	}
}
