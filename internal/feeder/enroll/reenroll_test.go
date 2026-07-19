package enroll

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
)

// enrollRelay runs a full loopback enrollment against ts and returns the fresh
// relay's relay_id, its issued certificate PEM (whose serial is on the feeder's
// issuance record as most-recently-issued), and the private key of the keypair
// that certificate was issued over — the "expired certificate's own private
// key" a later re-enrollment proves possession of (REL-027).
func enrollRelay(t *testing.T, client *http.Client, baseURL string) (relayID, certPEM string, certPriv ed25519.PrivateKey) {
	t.Helper()
	claimToken := fetchClaimToken(t, client, baseURL)
	_, priv, csrPEM := generateCSR(t, "reenroll-relay")
	resp, body := postEnroll(t, client, baseURL, claimToken, csrPEM)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /enroll status = %d, want 200", resp.StatusCode)
	}
	if body.RelayID == "" || body.Cert == "" {
		t.Fatalf("enroll response missing relay_id/cert: %+v", body)
	}
	return body.RelayID, body.Cert, priv
}

// fetchReEnrollChallenge performs GET /reenroll/challenge (REL-026) and returns
// its fresh single-use nonce.
func fetchReEnrollChallenge(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	resp, err := client.Get(baseURL + "/reenroll/challenge")
	if err != nil {
		t.Fatalf("GET /reenroll/challenge: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /reenroll/challenge status = %d, want 200", resp.StatusCode)
	}
	var c struct {
		Type string `json:"type"`
		Body struct {
			Nonce string `json:"nonce"`
		} `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	if c.Type != "challenge" || c.Body.Nonce == "" {
		t.Fatalf("challenge response = %+v, want type=challenge with a non-empty nonce", c)
	}
	return c.Body.Nonce
}

func postReEnrollRenew(t *testing.T, client *http.Client, baseURL string, req reenroll.RenewRequest) (*http.Response, []byte) {
	t.Helper()
	reqBody, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal renew request: %v", err)
	}
	resp, err := client.Post(baseURL+"/reenroll/renew", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /reenroll/renew: %v", err)
	}
	defer resp.Body.Close()
	raw := new(bytes.Buffer)
	_, _ = raw.ReadFrom(resp.Body)
	return resp, raw.Bytes()
}

func mustCSRDER(t *testing.T, csrPEM string) []byte {
	t.Helper()
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		t.Fatalf("csr did not PEM-decode: %q", csrPEM)
	}
	return block.Bytes
}

func mustCertSerial(t *testing.T, certPEM string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatalf("cert did not PEM-decode: %q", certPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate: %v", err)
	}
	return cert.SerialNumber.Text(16)
}

// TestReEnrollValidPathIssuesFreshCertSameRelayID drives the valid
// Expired-certificate re-enrollment path end to end (REL-020/024, corpus
// REL-020): a relay eligible on its most-recently-issued serial, proving
// possession of that certificate's OWN private key over the challenge nonce
// bound to a FRESH csr, re-enrolls — receiving a fresh certificate under the
// SAME relay_id and the re-anchored desired_state_verification_key, with the
// issuance record advanced to the new serial.
func TestReEnrollValidPathIssuesFreshCertSameRelayID(t *testing.T) {
	srv, ts, id, _ := newTestServer(t)
	client := ts.Client()

	relayID, cert1PEM, priv1 := enrollRelay(t, client, ts.URL)
	oldSerial := mustCertSerial(t, cert1PEM)

	nonce := fetchReEnrollChallenge(t, client, ts.URL)

	// A fresh keypair + csr (never the expired cert's own) — the PoP is signed
	// with the EXPIRED cert's private key over the nonce bound to THIS csr.
	newPub, _, csr2PEM := generateCSR(t, relayID)
	popSig := reenroll.SignPoP(priv1, nonce, mustCSRDER(t, csr2PEM))

	resp, raw := postReEnrollRenew(t, client, ts.URL, reenroll.RenewRequest{
		Type:    "renew",
		ID:      "01J8Z4K4N5P6Q7R8S9T0V1W3C1",
		RelayID: relayID,
		Body: reenroll.RenewBody{
			PresentedCert: cert1PEM,
			CSR:           csr2PEM,
			PoPSignature:  popSig,
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renew status = %d, want 200; body=%s", resp.StatusCode, raw)
	}

	var ack reenroll.RenewAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		t.Fatalf("decode renew-ack: %v (body=%s)", err, raw)
	}
	if ack.Type != "renew-ack" {
		t.Errorf("ack type = %q, want renew-ack", ack.Type)
	}
	if ack.ID != "01J8Z4K4N5P6Q7R8S9T0V1W3C1" {
		t.Errorf("ack id = %q, want the request id echoed back", ack.ID)
	}
	// Same relay_id (REL-024).
	if ack.RelayID != relayID {
		t.Errorf("ack relay_id = %q, want the unchanged %q", ack.RelayID, relayID)
	}
	// A FRESH serial (a new certificate, not the presented one).
	if ack.Body.Serial == "" || ack.Body.Serial == oldSerial {
		t.Errorf("ack serial = %q, want a fresh serial distinct from the presented %q", ack.Body.Serial, oldSerial)
	}
	if ack.Body.NotAfter <= ack.Body.NotBefore {
		t.Errorf("not_before/not_after = %d/%d, want a valid forward-dated window", ack.Body.NotBefore, ack.Body.NotAfter)
	}
	// The issued cert is over the NEW csr's public key.
	block, _ := pem.Decode([]byte(ack.Body.Cert))
	if block == nil {
		t.Fatalf("ack cert did not PEM-decode: %q", ack.Body.Cert)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate(re-issued cert): %v", err)
	}
	certPub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok || !certPub.Equal(newPub) {
		t.Error("re-issued cert public key is not the fresh csr's own public key")
	}
	if cert.Subject.CommonName != relayID {
		t.Errorf("re-issued cert CN = %q, want the same relay_id %q", cert.Subject.CommonName, relayID)
	}
	// Re-anchored desired-state verification key == the feeder's own signing pub (REL-017/024).
	if ack.Body.DesiredStateVerificationKey != encodeVerificationKey(id.SigningPub()) {
		t.Errorf("re-anchored key = %q, want the feeder signing pub %q", ack.Body.DesiredStateVerificationKey, encodeVerificationKey(id.SigningPub()))
	}

	// The issuance record advanced: the new serial is now most-recent, over the new key.
	gotSerial, gotPub, ok := srv.MostRecentSerial(relayID)
	if !ok {
		t.Fatalf("MostRecentSerial(%q) ok=false after re-enroll", relayID)
	}
	if gotSerial != ack.Body.Serial {
		t.Errorf("most-recent serial = %q, want the re-issued %q", gotSerial, ack.Body.Serial)
	}
	if !gotPub.Equal(newPub) {
		t.Error("most-recent recorded key != the fresh csr's public key")
	}
	// The now-superseded old serial is refused if presented again (REL-022).
	if err := reenroll.Eligible(srv, relayID, oldSerial); err == nil {
		t.Error("presenting the now-superseded old serial is eligible, want CERT_EXPIRED_INELIGIBLE")
	}
}

// TestReEnrollSupersededCertRefused asserts REL-022 (corpus REL-022): a
// presented certificate that is NOT the most-recently-issued for its relay_id
// (a newer one exists) is refused CERT_EXPIRED_INELIGIBLE and issues nothing.
func TestReEnrollSupersededCertRefused(t *testing.T) {
	srv, ts, _, _ := newTestServer(t)
	client := ts.Client()

	relayID, cert1PEM, priv1 := enrollRelay(t, client, ts.URL)
	oldSerial := mustCertSerial(t, cert1PEM)

	// A newer certificate is issued for the SAME relay_id, so the presented one
	// is superseded (a credential-integrity signal, REL-022).
	newerPub, _, _ := ed25519.GenerateKey(rand.Reader)
	srv.mu.Lock()
	srv.recordIssuance(relayID, "deadbeefdeadbeef", newerPub, 1_713_398_400_000)
	srv.mu.Unlock()

	nonce := fetchReEnrollChallenge(t, client, ts.URL)
	_, _, csr2PEM := generateCSR(t, relayID)
	popSig := reenroll.SignPoP(priv1, nonce, mustCSRDER(t, csr2PEM))

	resp, raw := postReEnrollRenew(t, client, ts.URL, reenroll.RenewRequest{
		Type:    "renew",
		ID:      "01J8Z4K4N5P6Q7R8S9T0V1W3B1",
		RelayID: relayID,
		Body:    reenroll.RenewBody{PresentedCert: cert1PEM, CSR: csr2PEM, PoPSignature: popSig},
	})
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("superseded renew returned 200, want a refusal; body=%s", raw)
	}
	var ef reenroll.ErrorFrame
	if err := json.Unmarshal(raw, &ef); err != nil {
		t.Fatalf("decode error frame: %v (body=%s)", err, raw)
	}
	if ef.Type != "error" {
		t.Errorf("frame type = %q, want error", ef.Type)
	}
	if ef.Code != reenroll.CodeExpiredIneligible {
		t.Errorf("code = %q, want %q", ef.Code, reenroll.CodeExpiredIneligible)
	}
	if !strings.Contains(ef.Message, "is superseded by") {
		t.Errorf("message = %q, want it to name the superseding serial", ef.Message)
	}

	// Nothing was issued: most-recent is still the injected newer serial, not a new one.
	gotSerial, _, _ := srv.MostRecentSerial(relayID)
	if gotSerial == oldSerial {
		t.Errorf("most-recent serial = %q (the presented one), the record is corrupt", gotSerial)
	}
	if gotSerial != "deadbeefdeadbeef" {
		t.Errorf("most-recent serial = %q, want the un-advanced newer serial (no cert issued)", gotSerial)
	}
}

// TestReEnrollMissingPoPRefused asserts REL-027/029 (corpus REL-027, first
// request): a renew whose pop_signature is absent is refused
// RE_ENROLL_POP_INVALID even though its serial matches the issuance record, and
// issues nothing.
func TestReEnrollMissingPoPRefused(t *testing.T) {
	srv, ts, _, _ := newTestServer(t)
	client := ts.Client()

	relayID, cert1PEM, _ := enrollRelay(t, client, ts.URL)
	oldSerial := mustCertSerial(t, cert1PEM)

	_ = fetchReEnrollChallenge(t, client, ts.URL)
	_, _, csr2PEM := generateCSR(t, relayID)

	resp, raw := postReEnrollRenew(t, client, ts.URL, reenroll.RenewRequest{
		Type:    "renew",
		ID:      "01J8Z4K4N5P6Q7R8S9T0V1W3D1",
		RelayID: relayID,
		Body:    reenroll.RenewBody{PresentedCert: cert1PEM, CSR: csr2PEM, PoPSignature: ""},
	})
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("missing-PoP renew returned 200, want a refusal; body=%s", raw)
	}
	var ef reenroll.ErrorFrame
	if err := json.Unmarshal(raw, &ef); err != nil {
		t.Fatalf("decode error frame: %v (body=%s)", err, raw)
	}
	if ef.ID != "01J8Z4K4N5P6Q7R8S9T0V1W3D1" {
		t.Errorf("error id = %q, want the request id echoed back", ef.ID)
	}
	if ef.Code != reenroll.CodePopInvalid {
		t.Errorf("code = %q, want %q", ef.Code, reenroll.CodePopInvalid)
	}
	if ef.Message != "proof-of-possession signature missing" {
		t.Errorf("message = %q, want %q", ef.Message, "proof-of-possession signature missing")
	}

	// Nothing issued: the record is unchanged.
	gotSerial, _, _ := srv.MostRecentSerial(relayID)
	if gotSerial != oldSerial {
		t.Errorf("most-recent serial = %q, want the unchanged %q (no cert issued)", gotSerial, oldSerial)
	}
}

// TestReEnrollWrongKeyPoPRefused asserts REL-027/028/029 (corpus REL-027,
// second request): a renew whose pop_signature was made with an unrelated key —
// not the presented certificate's own — is refused RE_ENROLL_POP_INVALID even
// though its serial matches, and issues nothing.
func TestReEnrollWrongKeyPoPRefused(t *testing.T) {
	srv, ts, _, _ := newTestServer(t)
	client := ts.Client()

	relayID, cert1PEM, _ := enrollRelay(t, client, ts.URL)
	oldSerial := mustCertSerial(t, cert1PEM)

	nonce := fetchReEnrollChallenge(t, client, ts.URL)
	_, _, csr2PEM := generateCSR(t, relayID)

	// PoP signed with an UNRELATED key, not the presented cert's own (REL-027).
	_, unrelatedPriv, _ := ed25519.GenerateKey(rand.Reader)
	popSig := reenroll.SignPoP(unrelatedPriv, nonce, mustCSRDER(t, csr2PEM))

	resp, raw := postReEnrollRenew(t, client, ts.URL, reenroll.RenewRequest{
		Type:    "renew",
		ID:      "01J8Z4K4N5P6Q7R8S9T0V1W3D2",
		RelayID: relayID,
		Body:    reenroll.RenewBody{PresentedCert: cert1PEM, CSR: csr2PEM, PoPSignature: popSig},
	})
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("wrong-key-PoP renew returned 200, want a refusal; body=%s", raw)
	}
	var ef reenroll.ErrorFrame
	if err := json.Unmarshal(raw, &ef); err != nil {
		t.Fatalf("decode error frame: %v (body=%s)", err, raw)
	}
	if ef.Code != reenroll.CodePopInvalid {
		t.Errorf("code = %q, want %q", ef.Code, reenroll.CodePopInvalid)
	}
	if ef.Message != "proof-of-possession signature did not verify against the certificate on record for this serial" {
		t.Errorf("message = %q, want the verify-failed message", ef.Message)
	}

	gotSerial, _, _ := srv.MostRecentSerial(relayID)
	if gotSerial != oldSerial {
		t.Errorf("most-recent serial = %q, want the unchanged %q (no cert issued)", gotSerial, oldSerial)
	}
}

// TestReEnrollRateLimited asserts REL-025: once a relay_id exceeds its
// per-window attempt bound, further attempts are refused RE_ENROLL_RATE_LIMITED
// — the rate-limit gate fires ahead of the eligibility/PoP checks so it cannot
// be used to force identity churn.
func TestReEnrollRateLimited(t *testing.T) {
	srv, ts, _, _ := newTestServer(t)
	client := ts.Client()

	// A tight, deterministic limiter for the test: 2 attempts per window.
	srv.reLimiter = reenroll.NewRateLimiter(2, 60_000)

	relayID, cert1PEM, _ := enrollRelay(t, client, ts.URL)
	_, _, csr2PEM := generateCSR(t, relayID)

	renew := func() reenroll.ErrorFrame {
		resp, raw := postReEnrollRenew(t, client, ts.URL, reenroll.RenewRequest{
			Type:    "renew",
			RelayID: relayID,
			Body:    reenroll.RenewBody{PresentedCert: cert1PEM, CSR: csr2PEM, PoPSignature: ""},
		})
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("renew returned 200, want a refusal; body=%s", raw)
		}
		var ef reenroll.ErrorFrame
		if err := json.Unmarshal(raw, &ef); err != nil {
			t.Fatalf("decode error frame: %v (body=%s)", err, raw)
		}
		return ef
	}

	// The first two attempts pass the rate gate (and fail on the missing PoP).
	for i := 1; i <= 2; i++ {
		if ef := renew(); ef.Code != reenroll.CodePopInvalid {
			t.Fatalf("attempt %d code = %q, want %q (within the rate window)", i, ef.Code, reenroll.CodePopInvalid)
		}
	}
	// The third is refused by the rate limiter itself.
	if ef := renew(); ef.Code != reenroll.CodeRateLimited {
		t.Errorf("attempt 3 code = %q, want %q", ef.Code, reenroll.CodeRateLimited)
	}
}
