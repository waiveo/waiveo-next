package playerserver

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// testRelaySigningIdentity builds a fresh self-signed relay cert (PEM+DER)
// alongside the ed25519.PrivateKey of that same keypair — the key SetProgram
// signs a Lease with, and the cert's own public key (returned to callers) is
// what PLY-090 requires a Lease's signature to verify against.
//
// This mints an ed25519 identity directly rather than reusing
// tlsboot.GenSelfSigned: GenSelfSigned now serves the browser-facing ECDSA
// P-256 leaf, but the relay's enrollment/lease-signing identity — the cert
// presented here, whose public key a player verifies Leases against — is
// ed25519 (leases are ed25519-signed via signhash). The two are distinct keys;
// this helper models the ed25519 signing identity, not the TLS serving leaf.
func testRelaySigningIdentity(t *testing.T) (certPEM, certDER []byte, priv ed25519.PrivateKey, pub ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "waiveo-relay"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certDER, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	return certPEM, certDER, priv, pub
}

// testImageContent is the one-item Wave-1 first-photon program content
// array: a single `image` item whose URL is a feeder direct-fetch
// content-origin URL (never a relay-hosted one, PLY-084/REL-140).
func testImageContent() []wire.LeaseContent {
	return []wire.LeaseContent{{
		Type:      "image",
		AssetRef:  "sha256:" + strings.Repeat("ab", 32),
		URL:       "https://198.51.100.20/cas/photon",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}}
}

// programTestServer builds a Server with one redeemable grant and one
// configured program (priority scheduled, display content, one image
// item), returning it alongside its signing pub key (for signature
// verification) and a freshly redeemed channel token.
func programTestServer(t *testing.T) (srv *Server, pub ed25519.PublicKey, token string) {
	t.Helper()

	certPEM, _, priv, pub := testRelaySigningIdentity(t)
	grant := testGrant()

	srv, err := NewServer(certPEM, []wire.PairingGrant{grant})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetProgram(1, "rev-17", "scheduled", "content", testImageContent(), priv)

	_, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})
	var pairResp PairingResponse
	remarshal(t, raw, &pairResp)
	if pairResp.ChannelToken == "" {
		t.Fatalf("pairing did not yield a channel_token: %+v", pairResp)
	}

	return srv, pub, pairResp.ChannelToken
}

func doProgram(t *testing.T, srv *Server, token string, contentTypes []string) (*http.Response, map[string]json.RawMessage) {
	t.Helper()

	ts := newPairingTestServer(t, srv)

	body, err := json.Marshal(ProgramPullRequest{
		Capabilities: Capabilities{ContentTypes: contentTypes, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/player/v1/program", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /player/v1/program: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	return resp, raw
}

// TestProgramReturnsSignedLeaseWithImage is Task 10's core assertion: a
// /program request with a valid channel token and content_types declaring
// image returns a Lease with display: content, priority: scheduled, one
// image content item whose url is the feeder content-origin URL, and a
// signature that verifies against the relay's own cert public key (PLY-090)
// — and does NOT verify against a different key.
func TestProgramReturnsSignedLeaseWithImage(t *testing.T) {
	srv, pub, token := programTestServer(t)

	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, raw)
	}

	var lease LeaseResponse
	remarshal(t, raw, &lease)

	if !validULIDForTest(lease.LeaseID) {
		t.Errorf("lease_id = %q, want a valid ULID (26 chars, Crockford base32) per PLY-097", lease.LeaseID)
	}
	if lease.ScreenID == "" {
		t.Error("screen_id is empty, want the token's own screen_id")
	}
	if lease.ProgramRevision != "rev-17" {
		t.Errorf("program_revision = %q, want %q", lease.ProgramRevision, "rev-17")
	}
	if lease.Priority != "scheduled" {
		t.Errorf("priority = %q, want %q (PLY-108, unmodified from the screen-program)", lease.Priority, "scheduled")
	}
	if lease.Display != "content" {
		t.Errorf("display = %q, want %q (PLY-109, unmodified from the screen-program)", lease.Display, "content")
	}
	if len(lease.Content) != 1 {
		t.Fatalf("content has %d items, want 1", len(lease.Content))
	}
	if lease.Content[0].Type != "image" {
		t.Errorf("content[0].type = %q, want %q", lease.Content[0].Type, "image")
	}
	if lease.Content[0].URL != testImageContent()[0].URL {
		t.Errorf("content[0].url = %q, want the feeder content-origin URL %q", lease.Content[0].URL, testImageContent()[0].URL)
	}
	if lease.IssuedAt == 0 {
		t.Error("issued_at is zero, want a real timestamp")
	}
	if lease.ValidUntil <= lease.IssuedAt {
		t.Errorf("valid_until = %d, want > issued_at = %d", lease.ValidUntil, lease.IssuedAt)
	}
	if lease.Signature == "" {
		t.Fatal("signature is empty, want a base64-encoded ed25519 signature")
	}

	sigBytes, err := wire.DecodeSignature(lease.Signature)
	if err != nil {
		t.Fatalf("wire.DecodeSignature: %v", err)
	}
	canon, err := wire.LeaseSignedBytes(lease.Lease)
	if err != nil {
		t.Fatalf("wire.LeaseSignedBytes: %v", err)
	}
	if !signhash.Verify(pub, canon, sigBytes) {
		t.Error("signature does not verify against the relay's own cert public key, want true (PLY-090)")
	}

	otherPub, _ := signhash.GenerateKey()
	if signhash.Verify(otherPub, canon, sigBytes) {
		t.Error("signature verifies against an UNRELATED public key, want false")
	}
}

// ulidCrockfordAlphabet mirrors internal/shared/ulid's own (unexported)
// encoding alphabet, duplicated here rather than imported so this test
// checks the wire-level shape a real ULID must have, independent of
// whichever internal constant the mint site happens to use.
const ulidCrockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// validULIDForTest reports whether id has the syntactic shape PLY-097
// requires of a Lease's lease_id: exactly 26 characters, every one drawn
// from the Crockford base32 alphabet (uppercase; I, L, O, U excluded).
func validULIDForTest(id string) bool {
	if len(id) != 26 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !strings.ContainsRune(ulidCrockfordAlphabet, rune(id[i])) {
			return false
		}
	}
	return true
}

// TestProgramContentTypeGateExcludesUndeclaredType confirms PLY-013/096: a
// request whose content_types omits image gets NO image content item.
func TestProgramContentTypeGateExcludesUndeclaredType(t *testing.T) {
	srv, _, token := programTestServer(t)

	resp, raw := doProgram(t, srv, token, []string{"video"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, raw)
	}

	var lease LeaseResponse
	remarshal(t, raw, &lease)

	if len(lease.Content) != 0 {
		t.Errorf("content has %d items, want 0 (image excluded, PLY-013/096); got %+v", len(lease.Content), lease.Content)
	}
}

// TestClampContentDurationsFloorsNonZeroSubFloorValues proves
// clampContentDurations raises a nonsensical near-zero or negative
// duration_ms up to leaseContentMinDurationMS, in place, while leaving 0 (the
// PLY-083b "no override" sentinel) and an already-sane value untouched.
func TestClampContentDurationsFloorsNonZeroSubFloorValues(t *testing.T) {
	content := []wire.LeaseContent{
		{Type: "image", DurationMS: 1},                         // nonsensical near-zero -> floored
		{Type: "image", DurationMS: -5},                        // negative -> floored
		{Type: "image", DurationMS: 0},                         // PLY-083b sentinel -> untouched
		{Type: "image", DurationMS: leaseContentMinDurationMS}, // exactly the floor -> untouched
		{Type: "image", DurationMS: 8000},                      // already sane -> untouched
	}

	got := clampContentDurations(content)

	want := []int64{leaseContentMinDurationMS, leaseContentMinDurationMS, 0, leaseContentMinDurationMS, 8000}
	for i, w := range want {
		if got[i].DurationMS != w {
			t.Errorf("content[%d].DurationMS = %d, want %d", i, got[i].DurationMS, w)
		}
	}
}

// TestProgramHandlerClampsUnsafeDurationEndToEnd proves the clamp is really
// wired into the served Lease, not just unit-tested in isolation: a program
// configured with a 1ms duration_ms content item (the exact render-thread-
// freeze-hazard shape a buggy/malicious producer could emit) serves a Lease
// whose corresponding item carries the floored value instead.
func TestProgramHandlerClampsUnsafeDurationEndToEnd(t *testing.T) {
	srv, _, token := programTestServer(t)

	srv.mu.Lock()
	srv.program.Content = []wire.LeaseContent{
		{Type: "image", AssetRef: "sha256:aaaa", URL: "https://example/content/aaaa", DurationMS: 1},
	}
	srv.mu.Unlock()

	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, raw)
	}

	var lease LeaseResponse
	remarshal(t, raw, &lease)

	if len(lease.Content) != 1 {
		t.Fatalf("content has %d items, want 1; got %+v", len(lease.Content), lease.Content)
	}
	if lease.Content[0].DurationMS != leaseContentMinDurationMS {
		t.Errorf("served DurationMS = %d, want floored to %d (unclamped wire-supplied duration_ms reached the player)", lease.Content[0].DurationMS, leaseContentMinDurationMS)
	}
}

// TestProgramRejectsMissingToken confirms a request with no Authorization
// header is refused with a typed error, never a Lease.
func TestProgramRejectsMissingToken(t *testing.T) {
	srv, _, _ := programTestServer(t)

	resp, raw := doProgram(t, srv, "", []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_INVALID")
}

// TestProgramRejectsUnknownToken confirms a well-formed but never-issued
// token is refused with CHANNEL_TOKEN_INVALID.
func TestProgramRejectsUnknownToken(t *testing.T) {
	srv, _, _ := programTestServer(t)

	resp, raw := doProgram(t, srv, "ct-does-not-exist", []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_INVALID")
}

// TestProgramRejectsExpiredToken confirms a token past its own expires_at
// is refused with CHANNEL_TOKEN_EXPIRED (PLY-072) — reaching directly into
// Server's own token map (same package) to fabricate an already-expired
// record, since a real 24h-TTL token cannot practically expire within a
// test.
func TestProgramRejectsExpiredToken(t *testing.T) {
	srv, _, token := programTestServer(t)

	srv.mu.Lock()
	rec := srv.tokens[token]
	rec.ExpiresAt = time.Now().Add(-time.Minute).UnixMilli()
	srv.tokens[token] = rec
	srv.mu.Unlock()

	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_EXPIRED")
}

// TestLeaseAckRecordsAck confirms POST /player/v1/lease/ack (PLY-091)
// records the acknowledgement, retrievable via Server.LeaseAck.
func TestLeaseAckRecordsAck(t *testing.T) {
	srv, _, token := programTestServer(t)

	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, raw)
	}
	var lease LeaseResponse
	remarshal(t, raw, &lease)

	ts := newPairingTestServer(t, srv)

	ackBody, err := json.Marshal(LeaseAckRequest{LeaseID: lease.LeaseID, Accepted: true})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	ackResp, err := http.Post(ts.URL+"/player/v1/lease/ack", "application/json", bytes.NewReader(ackBody))
	if err != nil {
		t.Fatalf("POST /player/v1/lease/ack: %v", err)
	}
	defer ackResp.Body.Close()
	if ackResp.StatusCode != http.StatusOK {
		t.Fatalf("lease/ack status = %d, want 200", ackResp.StatusCode)
	}

	rec, ok := srv.LeaseAck(lease.LeaseID)
	if !ok {
		t.Fatal("LeaseAck: not recorded")
	}
	if !rec.Accepted {
		t.Error("LeaseAck.Accepted = false, want true")
	}
	if rec.LeaseID != lease.LeaseID {
		t.Errorf("LeaseAck.LeaseID = %q, want %q", rec.LeaseID, lease.LeaseID)
	}
}

// TestSetProgramFencesStaleGenerationWrite is the regression guard for the
// live re-pull race (REL-052/056). When a higher desired-state generation is
// applied, the superseded generation's per-screen resolver goroutines are
// cancelled but one may still be mid-resolve and call SetProgram late. Since
// every resolver stamps its own generation onto SetProgram, the server MUST
// drop a strictly-older generation's write so the served program never reverts
// to the pre-edit generation — the "an API edit MUST change the resolved
// program" oracle across the atomic-swap window. A same-or-higher generation
// still wins, so a same-generation schedule resolver can replace the boot
// baseline. Without the fence this is plain last-write-wins and the stale
// generation-7 write below reverts the served program.
func TestSetProgramFencesStaleGenerationWrite(t *testing.T) {
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	srv, err := NewServer(certPEM, []wire.PairingGrant{testGrant()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Generation 8 is applied and served (an admin's schedule edit, live).
	srv.SetProgram(8, "gen-8", "scheduled", "content", testImageContent(), priv)
	// A superseded generation-7 resolver, still mid-resolve when 8 was applied,
	// writes late — the concrete re-pull race this fence closes.
	srv.SetProgram(7, "gen-7", "blank", "blank", nil, priv)

	srv.mu.Lock()
	gotRev, gotDisplay, gotGen := srv.program.ProgramRevision, srv.program.Display, srv.programGen
	srv.mu.Unlock()

	if gotRev != "gen-8" {
		t.Errorf("program_revision = %q, want gen-8 — a stale generation-7 write reverted the served program past generation 8 (REL-052/056)", gotRev)
	}
	if gotDisplay != "content" {
		t.Errorf("display = %q, want content — the stale generation-7 write leaked through the generation fence", gotDisplay)
	}
	if gotGen != 8 {
		t.Errorf("programGen = %d, want 8 — the fence must not lower the applied generation", gotGen)
	}

	// A same-generation write still wins: the fence must not freeze the program,
	// so a same-generation schedule resolver replaces the boot baseline (the
	// additive serving policy) exactly as before.
	srv.SetProgram(8, "gen-8-schedule", "scheduled", "content", testImageContent(), priv)
	srv.mu.Lock()
	gotRev = srv.program.ProgramRevision
	srv.mu.Unlock()
	if gotRev != "gen-8-schedule" {
		t.Errorf("program_revision = %q, want gen-8-schedule — a same-generation write must still win (last-write-wins within a generation)", gotRev)
	}
}

// TestProgramHandlerSourceNeverFetchesAssetBytes is the CI-style static
// assertion the Wave-1 plan calls for (Task 10 Step 4): grep this
// package's program.go for any outbound content-fetch call. The relay is
// never in the asset-bytes data path (PLY-084, REL-140, `#52`) — this
// handler only ever hands a URL BACK to the player, never dereferences one
// itself.
func TestProgramHandlerSourceNeverFetchesAssetBytes(t *testing.T) {
	src, err := os.ReadFile("program.go")
	if err != nil {
		t.Fatalf("read program.go: %v", err)
	}
	forbidden := []string{"http.Get(", "http.Post(", "http.Client{", "http.DefaultClient", ".Do(req"}
	for _, f := range forbidden {
		if strings.Contains(string(src), f) {
			t.Errorf("program.go contains %q — the relay must never fetch content-origin bytes itself (PLY-084/REL-140)", f)
		}
	}
}
