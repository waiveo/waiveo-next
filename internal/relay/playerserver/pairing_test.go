package playerserver

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/paircode"
	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// testRelayCert generates a fresh self-signed relay bootstrap cert
// (internal/shared/tlsboot, exactly what cmd/waiveo-relay serves player/1
// over), returning both its PEM and raw DER — the two forms this package's
// callers need (PEM for trust_anchors, DER for the PLY-052/REL-126
// commitment).
func testRelayCert(t *testing.T) (certPEM, certDER []byte) {
	t.Helper()
	certPEM, _ = tlsboot.GenSelfSigned()
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("GenSelfSigned() cert did not PEM-decode to a CERTIFICATE block")
	}
	return certPEM, block.Bytes
}

// testGrant builds a fresh one-time REL-121 pairing-grant record, issued
// now with a generous TTL — the ordinary "still redeemable" fixture most
// tests start from.
func testGrant() wire.PairingGrant {
	return wire.PairingGrant{
		GrantID:                "grant-test-0123456789abcdef",
		Purpose:                "pairing",
		ResultingPrincipalKind: "screen",
		TTL:                    900,
		RedemptionMode:         "one-time",
		IssuedAt:               time.Now().UnixMilli(),
	}
}

func newTestServer(t *testing.T, grants ...wire.PairingGrant) (*Server, []byte, []byte) {
	t.Helper()
	certPEM, certDER := testRelayCert(t)
	srv, err := NewServer(certPEM, grants)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, certPEM, certDER
}

func doPair(t *testing.T, srv *Server, req PairingRequest) (*http.Response, map[string]json.RawMessage) {
	t.Helper()

	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("json.Marshal(req): %v", err)
	}

	resp, err := http.Post(ts.URL+"/player/v1/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /player/v1/pair: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	return resp, raw
}

// TestPairingRedeemsValidGrantSelector is Step 1's core assertion: a
// PairingRequest carrying the applied grant's grant_selector redeems ->
// pairing_status: redeemed, a channel_token, a fresh screen_id, an
// expires_at ~24h after issued_at (PLY-071's banked value), and
// trust_anchors carrying the relay's own cert.
func TestPairingRedeemsValidGrantSelector(t *testing.T) {
	grant := testGrant()
	srv, certPEM, _ := newTestServer(t, grant)

	resp, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, raw)
	}

	var out PairingResponse
	remarshal(t, raw, &out)

	if out.PairingStatus != "redeemed" {
		t.Fatalf("pairing_status = %q, want %q", out.PairingStatus, "redeemed")
	}
	if out.ChannelToken == "" {
		t.Error("channel_token is empty, want a minted token")
	}
	if out.ScreenID == "" {
		t.Error("screen_id is empty, want a freshly generated id")
	}
	if out.IssuedAt == 0 {
		t.Error("issued_at is zero, want a real timestamp")
	}
	wantExpiresAt := out.IssuedAt + int64(24*time.Hour/time.Millisecond)
	if out.ExpiresAt != wantExpiresAt {
		t.Errorf("expires_at = %d, want %d (issued_at + 24h, PLY-071)", out.ExpiresAt, wantExpiresAt)
	}

	if len(out.TrustAnchors) != 1 {
		t.Fatalf("trust_anchors has %d entries, want 1", len(out.TrustAnchors))
	}
	if out.TrustAnchors[0].PEM != string(certPEM) {
		t.Error("trust_anchors[0].pem does not match the relay's own cert PEM")
	}
}

// TestPairingResponseCarriesNoAuthenticatorField is the PLY-032/PLY-056
// property made concrete: a redeemed PairingResponse's JSON keys must be
// EXACTLY the allowed set the contract's own worked example shows — no
// relay-computed digest/checksum/fingerprint field riding alongside
// trust_anchors that a player could mistake for an out-of-band
// authenticator.
func TestPairingResponseCarriesNoAuthenticatorField(t *testing.T) {
	grant := testGrant()
	srv, _, _ := newTestServer(t, grant)

	resp, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	allowed := map[string]bool{
		"trust_anchors":  true,
		"pairing_status": true,
		"channel_token":  true,
		"screen_id":      true,
		"issued_at":      true,
		"expires_at":     true,
	}
	for key := range raw {
		if !allowed[key] {
			t.Errorf("PairingResponse carries unexpected key %q — suspect a self-attesting authenticator field (PLY-056 forbids this)", key)
		}
	}
	// And every allowed key that a redeemed response must carry is present.
	for key := range allowed {
		if _, ok := raw[key]; !ok {
			t.Errorf("PairingResponse missing expected key %q", key)
		}
	}
}

// TestPairingCodeCommitmentVerifiesAgainstRelayCert is the MITM property at
// this layer, end to end: a pairing code formed by the relay for a real
// grant decodes to a fingerprint_commitment that verifies against the
// relay's REAL cert DER, and does NOT verify against a different cert's DER
// (a MITM substituting its own cert during bootstrap fetch cannot pass
// PLY-052's local comparison).
func TestPairingCodeCommitmentVerifiesAgainstRelayCert(t *testing.T) {
	grant := testGrant()
	_, certDER := testRelayCert(t)

	code, err := FormPairingCode("127.0.0.1", 7421, grant, certDER)
	if err != nil {
		t.Fatalf("FormPairingCode: %v", err)
	}

	gotHost, gotPort, gotSelector, gotCommitment, err := paircode.Decode(code)
	if err != nil {
		t.Fatalf("paircode.Decode(%q): %v", code, err)
	}
	if gotHost != "127.0.0.1" || gotPort != 7421 {
		t.Errorf("decoded dial address = (%q, %d), want (127.0.0.1, 7421)", gotHost, gotPort)
	}
	if gotSelector != grant.GrantID {
		t.Errorf("decoded grant_selector = %q, want the grant's own grant_id %q", gotSelector, grant.GrantID)
	}

	ok, err := tlsboot.VerifyCommitmentForCertDER(certDER, gotCommitment)
	if err != nil {
		t.Fatalf("VerifyCommitmentForCertDER(real cert): %v", err)
	}
	if !ok {
		t.Fatal("VerifyCommitmentForCertDER(real cert, decoded commitment) = false, want true")
	}

	// A DIFFERENT cert's DER must NOT verify against the same commitment —
	// the concrete MITM-substitution property this pairing-code layer
	// exists to defend.
	_, otherDER := testRelayCert(t)
	if bytes.Equal(certDER, otherDER) {
		t.Fatal("two GenSelfSigned() certs produced identical DER; test fixture invalid")
	}
	ok, err = tlsboot.VerifyCommitmentForCertDER(otherDER, gotCommitment)
	if err != nil {
		t.Fatalf("VerifyCommitmentForCertDER(other cert): %v", err)
	}
	if ok {
		t.Fatal("VerifyCommitmentForCertDER(other cert, decoded commitment) = true, want false (MITM-substituted cert must be rejected)")
	}
}

// TestPairingRejectsAbsentGrantSelector confirms an absent grant_selector
// is refused with the typed PAIRING_CODE_INVALID error (PLY-036), never a
// never-resolving pending.
func TestPairingRejectsAbsentGrantSelector(t *testing.T) {
	grant := testGrant()
	srv, _, _ := newTestServer(t, grant)

	resp, raw := doPair(t, srv, PairingRequest{
		HardwareID:   "hw-0001",
		Capabilities: Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})

	assertTypedError(t, resp, raw, "PAIRING_CODE_INVALID")
}

// TestPairingRejectsUnknownGrantSelector confirms a grant_selector that
// resolves against no pairing grant is refused with PAIRING_CODE_INVALID.
func TestPairingRejectsUnknownGrantSelector(t *testing.T) {
	grant := testGrant()
	srv, _, _ := newTestServer(t, grant)

	resp, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-0001",
		GrantSelector: "grant-does-not-exist",
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})

	assertTypedError(t, resp, raw, "PAIRING_CODE_INVALID")
}

// TestPairingRejectsExpiredGrant confirms a grant whose ttl has elapsed
// since issued_at is refused with PAIRING_EXPIRED (PLY-036).
func TestPairingRejectsExpiredGrant(t *testing.T) {
	grant := testGrant()
	grant.TTL = 1 // 1 second
	grant.IssuedAt = time.Now().Add(-1 * time.Hour).UnixMilli()

	srv, _, _ := newTestServer(t, grant)

	resp, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})

	assertTypedError(t, resp, raw, "PAIRING_EXPIRED")
}

// TestPairingRejectsAlreadyRedeemedOneTimeGrant confirms a second
// redemption attempt against a one-time grant that has already been
// redeemed is refused with PAIRING_CODE_INVALID, never silently re-issued.
func TestPairingRejectsAlreadyRedeemedOneTimeGrant(t *testing.T) {
	grant := testGrant()
	srv, _, _ := newTestServer(t, grant)

	req := PairingRequest{
		HardwareID:    "hw-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	}

	first, _ := doPair(t, srv, req)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first redemption status = %d, want 200", first.StatusCode)
	}

	second, raw := doPair(t, srv, req)
	assertTypedError(t, second, raw, "PAIRING_CODE_INVALID")
}

// TestPairingMultiGrantRedeemableMoreThanOnce confirms a redemption_mode:
// multi grant (REL-121, PLY-037) may be redeemed by more than one
// PairingRequest, each producing its own independent screen_id and
// channel_token.
func TestPairingMultiGrantRedeemableMoreThanOnce(t *testing.T) {
	grant := testGrant()
	grant.RedemptionMode = "multi"
	srv, _, _ := newTestServer(t, grant)

	req := PairingRequest{
		HardwareID:    "hw-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	}

	firstResp, firstRaw := doPair(t, srv, req)
	if firstResp.StatusCode != http.StatusOK {
		t.Fatalf("first redemption status = %d, want 200", firstResp.StatusCode)
	}
	secondResp, secondRaw := doPair(t, srv, req)
	if secondResp.StatusCode != http.StatusOK {
		t.Fatalf("second redemption status = %d, want 200", secondResp.StatusCode)
	}

	var first, second PairingResponse
	remarshal(t, firstRaw, &first)
	remarshal(t, secondRaw, &second)

	if first.ScreenID == second.ScreenID {
		t.Error("two multi-grant redemptions produced the same screen_id, want independent ids")
	}
	if first.ChannelToken == second.ChannelToken {
		t.Error("two multi-grant redemptions produced the same channel_token, want independent tokens")
	}
}

// TestPairStatusRejectsUnknownPollToken confirms GET /player/v1/pair/status
// is present and answers a typed error for a poll_token it never issued —
// Wave-1 first-photon's /pair always redeems synchronously (PLY-033), so no
// poll_token is ever outstanding, but the endpoint's shape/path must still
// be faithful to PLY-034 rather than absent or panicking.
func TestPairStatusRejectsUnknownPollToken(t *testing.T) {
	srv, _, _ := newTestServer(t)

	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/player/v1/pair/status?poll_token=unknown-token")
	if err != nil {
		t.Fatalf("GET /player/v1/pair/status: %v", err)
	}
	defer resp.Body.Close()

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode response body: %v", err)
	}

	assertTypedError(t, resp, raw, "PAIRING_CODE_INVALID")
}

// assertTypedError confirms resp/raw is a non-2xx response carrying the
// typed error shape {code, message} with code == wantCode — never a
// pairing_status: pending shape that can never resolve (PLY-036).
func assertTypedError(t *testing.T, resp *http.Response, raw map[string]json.RawMessage, wantCode string) {
	t.Helper()

	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want a 4xx error status", resp.StatusCode)
	}
	if _, isPending := raw["pairing_status"]; isPending {
		t.Fatalf("response carries pairing_status %s, want a typed error instead (PLY-036 forbids a never-resolving pending)", raw["pairing_status"])
	}

	var eb struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	remarshal(t, raw, &eb)
	if eb.Code != wantCode {
		t.Errorf("error code = %q, want %q (message: %q)", eb.Code, wantCode, eb.Message)
	}
}

func remarshal(t *testing.T, raw map[string]json.RawMessage, out any) {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-marshal raw response: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal into %T: %v", out, err)
	}
}
