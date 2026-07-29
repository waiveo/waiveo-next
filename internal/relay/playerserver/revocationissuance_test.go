package playerserver

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// revocationissuance_test.go covers relay/1 REL-123's channel-token-ISSUANCE
// half, which is separate from — and was missing alongside — the presented-token
// check in program.go.

// TestRedeemMintsNoTokenForRevokedScreen is REL-123's channel-token-ISSUANCE
// half. Enforcing revocation only on a presented token leaves a revoked screen
// able to pair successfully and walk away holding a fresh credential the relay
// has already been told is void; it is refused only at the next program pull.
// The refusal is the same typed code an unresolvable selector draws, so the
// pairing endpoint does not become an oracle for which screens are revoked, and
// the one-time grant is NOT consumed — an app peer that later withdraws the
// revocation must find the grant still redeemable.
func TestRedeemMintsNoTokenForRevokedScreen(t *testing.T) {
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grant := testGrantForScreen(testScreenIDA)

	srv, err := NewServer(certPEM, []wire.PairingGrant{grant}, WallClockMs)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetSigningKey(priv)
	srv.SetRevokedScreens(1, []string{testScreenIDA})

	resp, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-revoked",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if resp.StatusCode < 400 {
		t.Fatalf("pairing against a revoked screen returned %d, want a 4xx refusal (REL-123: enforced at every channel-token issuance)", resp.StatusCode)
	}
	if got := decodeString(t, raw["code"]); got != "PAIRING_CODE_INVALID" {
		t.Errorf("refusal code = %q, want PAIRING_CODE_INVALID — a distinguishable code makes /pair an oracle for which screens are revoked", got)
	}
	if _, minted := raw["channel_token"]; minted {
		t.Error("a channel token was minted for a revoked screen (REL-123)")
	}
	if _, disclosed := raw["screen_id"]; disclosed {
		t.Error("the refusal disclosed a screen_id")
	}

	// The grant was refused, not consumed: withdrawing the revocation leaves it
	// redeemable. A revocation that silently spent one-time grants would make
	// itself irreversible in a way REL-066 never says it is.
	srv.SetRevokedScreens(2, nil)
	resp, raw = doPair(t, srv, PairingRequest{
		HardwareID:    "hw-revoked",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("pairing after the revocation was withdrawn = %d (body %v), want 200 — the refused attempt must not have consumed the one-time grant", resp.StatusCode, raw)
	}
	if _, minted := raw["channel_token"]; !minted {
		t.Error("no channel token minted after the revocation was withdrawn")
	}
}

// decodeString unquotes a raw JSON string field, returning "" for an absent or
// non-string value.
func decodeString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
