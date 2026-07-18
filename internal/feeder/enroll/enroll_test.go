package enroll

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

const testImagePath = "../origin/testdata/photon.png"

func loadTestImage(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(testImagePath)
	if err != nil {
		t.Fatalf("read fixture image %s: %v", testImagePath, err)
	}
	return b
}

func testIdentity(t *testing.T) *signing.Identity {
	t.Helper()
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	return id
}

// newTestServer builds an enroll.Server carrying a real signed snapshot
// (Task 5's Build) with one grant.Mint() grant riding it, and mounts it on
// an httptest TLS server — a relay client presenting the loopback claim
// token over server-authenticated TLS (REL-010), exactly the deployment
// shape this package serves in cmd/waiveo-feeder.
func newTestServer(t *testing.T) (*Server, *httptest.Server, *signing.Identity, wire.PairingGrant) {
	t.Helper()
	id := testIdentity(t)
	img := loadTestImage(t)
	g := grant.Mint()

	snap, err := snapshot.Build(img, "https://origin.example", id, []wire.PairingGrant{g})
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	srv, err := NewServer(id, snap)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)

	return srv, ts, id, g
}

// generateCSR generates a fresh ed25519 keypair (the relay's own, never
// leaving the relay) and a PEM-encoded CSR over it — REL-012's `csr`.
func generateCSR(t *testing.T, commonName string) (pub ed25519.PublicKey, priv ed25519.PrivateKey, csrPEM string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	template := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: commonName},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, template, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificateRequest: %v", err)
	}
	block := &pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}
	return pub, priv, string(pem.EncodeToMemory(block))
}

func fetchClaimToken(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	resp, err := client.Get(baseURL + "/claim-token")
	if err != nil {
		t.Fatalf("GET /claim-token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /claim-token status = %d, want 200", resp.StatusCode)
	}
	var body claimTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode claim-token response: %v", err)
	}
	if body.ClaimToken == "" {
		t.Fatal("claim-token response carried an empty claim_token")
	}
	return body.ClaimToken
}

func postEnroll(t *testing.T, client *http.Client, baseURL, claimToken, csrPEM string) (*http.Response, enrollResponse) {
	t.Helper()
	reqBody, err := json.Marshal(enrollRequest{ClaimToken: claimToken, CSR: csrPEM})
	if err != nil {
		t.Fatalf("marshal enroll request: %v", err)
	}
	resp, err := client.Post(baseURL+"/enroll", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /enroll: %v", err)
	}
	defer resp.Body.Close()
	var body enrollResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp, body
}

// TestEnrollIssuesRelayCertAndSigningKey asserts a relay client presenting
// the loopback claim token (REL-010/011) enrolls (REL-012) and receives a
// cert issued over its own CSR public key, plus `desired_state_verification_key`
// equal to the feeder's own signing.Identity.SigningPub() — the trust
// anchor a relay persists and verifies every subsequent snapshot against
// (REL-071, `#28`).
func TestEnrollIssuesRelayCertAndSigningKey(t *testing.T) {
	_, ts, id, _ := newTestServer(t)
	client := ts.Client()

	claimToken := fetchClaimToken(t, client, ts.URL)
	relayPub, _, csrPEM := generateCSR(t, "test-relay")

	resp, body := postEnroll(t, client, ts.URL, claimToken, csrPEM)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /enroll status = %d, want 200", resp.StatusCode)
	}

	if body.RelayID == "" {
		t.Error("enroll response carried an empty relay_id")
	}
	if body.NotBefore <= 0 || body.NotAfter <= body.NotBefore {
		t.Errorf("not_before/not_after = %d/%d, want a valid forward-dated window", body.NotBefore, body.NotAfter)
	}

	// The issued cert's own public key must be the relay's CSR key — proof
	// the feeder issued a cert over the CSR it was actually given, not
	// some other key.
	block, _ := pem.Decode([]byte(body.Cert))
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("enroll response cert did not PEM-decode to a CERTIFICATE block: %q", body.Cert)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate(issued cert): %v", err)
	}
	certPub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("issued cert public key is %T, want ed25519.PublicKey", cert.PublicKey)
	}
	if !certPub.Equal(relayPub) {
		t.Error("issued cert's public key does not match the relay's own CSR public key")
	}

	// desired_state_verification_key MUST equal the feeder's own signing
	// pub (REL-012, REL-071) — the exact key the relay will verify every
	// subsequent state.snapshot against.
	const wantPrefix = "ed25519:"
	if !strings.HasPrefix(body.DesiredStateVerificationKey, wantPrefix) {
		t.Fatalf("desired_state_verification_key = %q, want an %q-prefixed value", body.DesiredStateVerificationKey, wantPrefix)
	}
	gotKeyHex := strings.TrimPrefix(body.DesiredStateVerificationKey, wantPrefix)
	gotKey, err := hex.DecodeString(gotKeyHex)
	if err != nil {
		t.Fatalf("desired_state_verification_key did not hex-decode: %v", err)
	}
	if !ed25519.PublicKey(gotKey).Equal(id.SigningPub()) {
		t.Error("desired_state_verification_key != the feeder identity's own SigningPub()")
	}
}

// TestEnrollRefusesAlreadyRedeemedClaimToken asserts REL-013: a second
// enrollment presenting the same, already-redeemed claim_token is refused
// with a typed CLAIM_TOKEN_INVALID error, never silently re-enrolled as a
// second relay.
func TestEnrollRefusesAlreadyRedeemedClaimToken(t *testing.T) {
	_, ts, _, _ := newTestServer(t)
	client := ts.Client()

	claimToken := fetchClaimToken(t, client, ts.URL)
	_, _, csrPEM1 := generateCSR(t, "test-relay-1")

	resp1, body1 := postEnroll(t, client, ts.URL, claimToken, csrPEM1)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first enroll status = %d, want 200", resp1.StatusCode)
	}
	if body1.RelayID == "" {
		t.Fatal("first enroll did not return a relay_id")
	}

	_, _, csrPEM2 := generateCSR(t, "test-relay-2")
	resp2, err2 := postEnrollError(t, client, ts.URL, claimToken, csrPEM2)
	if err2 == nil {
		t.Fatal("second enroll with the same (already-redeemed) claim_token succeeded, want a refusal")
	}
	if resp2.StatusCode == http.StatusOK {
		t.Errorf("second enroll status = %d, want a non-200 refusal", resp2.StatusCode)
	}
	if err2.Code != "CLAIM_TOKEN_INVALID" {
		t.Errorf("second enroll error code = %q, want %q (REL-013)", err2.Code, "CLAIM_TOKEN_INVALID")
	}
}

func postEnrollError(t *testing.T, client *http.Client, baseURL, claimToken, csrPEM string) (*http.Response, *errorBody) {
	t.Helper()
	reqBody, err := json.Marshal(enrollRequest{ClaimToken: claimToken, CSR: csrPEM})
	if err != nil {
		t.Fatalf("marshal enroll request: %v", err)
	}
	resp, err := client.Post(baseURL+"/enroll", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return resp, &eb
}

// TestEnrollRefusesUnknownClaimToken asserts a claim_token the server never
// issued is refused the same way an already-redeemed one is (REL-013's
// "malformed, unknown, or already redeemed" all collapse to
// CLAIM_TOKEN_INVALID).
func TestEnrollRefusesUnknownClaimToken(t *testing.T) {
	_, ts, _, _ := newTestServer(t)
	client := ts.Client()

	_, _, csrPEM := generateCSR(t, "test-relay")
	resp, errBody := postEnrollError(t, client, ts.URL, "never-issued-token", csrPEM)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("enroll with an unknown claim_token succeeded, want a refusal")
	}
	if errBody.Code != "CLAIM_TOKEN_INVALID" {
		t.Errorf("error code = %q, want %q", errBody.Code, "CLAIM_TOKEN_INVALID")
	}
}

// TestStatePullReturnsVerifiableSignedSnapshot asserts the pull endpoint
// serves the exact signed snapshot the server was constructed with: its
// signature verifies under the feeder's own SigningPub() (REL-071's own
// verification recipe: wire.DecodeSignature + signhash.Verify over
// {generation, hash}), and sections.pairing_grants carries exactly the one
// REL-121 grant this test minted.
func TestStatePullReturnsVerifiableSignedSnapshot(t *testing.T) {
	_, ts, id, g := newTestServer(t)
	client := ts.Client()

	resp, err := client.Get(ts.URL + "/state/pull")
	if err != nil {
		t.Fatalf("GET /state/pull: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /state/pull status = %d, want 200", resp.StatusCode)
	}

	var pulled wire.StateSnapshotBody
	if err := json.NewDecoder(resp.Body).Decode(&pulled); err != nil {
		t.Fatalf("decode state.snapshot body: %v", err)
	}

	if pulled.Generation != 1 {
		t.Errorf("Generation = %d, want 1", pulled.Generation)
	}

	sigBytes, err := wire.DecodeSignature(pulled.Signature)
	if err != nil {
		t.Fatalf("wire.DecodeSignature: %v", err)
	}
	canon, err := json.Marshal(struct {
		Generation int64  `json:"generation"`
		Hash       string `json:"hash"`
	}{pulled.Generation, pulled.Hash})
	if err != nil {
		t.Fatalf("marshal {generation,hash} canon: %v", err)
	}
	if !signhash.Verify(id.SigningPub(), canon, sigBytes) {
		t.Error("pulled snapshot signature did not verify under the feeder's own SigningPub()")
	}

	if len(pulled.Sections.PairingGrants) != 1 {
		t.Fatalf("Sections.PairingGrants = %#v, want exactly 1 grant", pulled.Sections.PairingGrants)
	}
	if pulled.Sections.PairingGrants[0] != g {
		t.Errorf("pulled grant = %#v, want the minted grant %#v", pulled.Sections.PairingGrants[0], g)
	}
}
