// Package enroll implements the relay/1 client side of enrollment
// (contracts/relay-1.md, REL-010–014): generating a keypair and CSR the
// relay retains the private half of, presenting a loopback claim credential
// to the feeder (Task 6's internal/feeder/enroll server), and persisting
// the resulting identity — relay_id, issued certificate, and the feeder's
// own desired-state signing public key (`desired_state_verification_key`)
// — into the relay's operational SQLite (internal/relay/identity), the
// enrollment-anchored trust anchor every subsequent snapshot is verified
// against (REL-071, `#28`).
//
// Run is idempotent across restarts: once a store already holds a
// persisted identity, Run reads it back and returns without contacting the
// feeder again — REL-014's relay_id, once assigned, persists across
// restarts under the same enrollment relationship, and a claim_token is
// single-use (REL-013), so re-presenting one on every boot would be both
// wrong and futile.
//
// The request/response JSON shapes below are copies of
// internal/feeder/enroll's own unexported types, field-for-field — this
// package is the wire consumer on the other side of that exact contract
// and must decode exactly what the feeder sends, never an invented shape.
package enroll

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
)

// ErrInvalidVerificationKey is returned when the feeder's enrollment
// response carries a `desired_state_verification_key` that does not decode
// to a well-formed ed25519 public key — carry-forward guard (Task 2
// finding): crypto/ed25519.Verify panics on a wrong-length key, so a
// malformed key here must be refused as a typed error, never persisted into
// the store and never handed to signhash.Verify.
var ErrInvalidVerificationKey = errors.New("enroll: desired_state_verification_key did not decode to a valid ed25519 public key")

// bootstrapHTTPTimeout bounds the loopback enrollment exchange — this is a
// co-located, same-host call (REL-010's bootstrap exception), so a short
// timeout is generous, not tight.
const bootstrapHTTPTimeout = 10 * time.Second

// claimTokenResponse mirrors internal/feeder/enroll's claimTokenResponse:
// GET /claim-token's `{claim_token}` body (REL-011).
type claimTokenResponse struct {
	ClaimToken string `json:"claim_token"`
}

// enrollRequest mirrors internal/feeder/enroll's enrollRequest: POST
// /enroll's `{claim_token, csr}` request body (REL-012).
type enrollRequest struct {
	ClaimToken string `json:"claim_token"`
	CSR        string `json:"csr"`
}

// enrollResponse mirrors internal/feeder/enroll's enrollResponse: POST
// /enroll's `{relay_id, cert, not_before, not_after,
// desired_state_verification_key}` success body (REL-012).
type enrollResponse struct {
	RelayID                     string `json:"relay_id"`
	Cert                        string `json:"cert"`
	NotBefore                   int64  `json:"not_before"`
	NotAfter                    int64  `json:"not_after"`
	DesiredStateVerificationKey string `json:"desired_state_verification_key"`
}

// errorBody mirrors internal/feeder/enroll's errorBody: this bootstrap
// exchange's `{code, message}` typed-refusal shape (e.g. REL-013's
// CLAIM_TOKEN_INVALID).
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Run enrolls the relay identified by store against the feeder at
// feederBaseURL (e.g. "https://127.0.0.1:7420"), unless store already holds
// a persisted identity — in which case Run is a no-op success, per this
// package's idempotent-across-restarts doc.
//
// On success, store holds the issued relay_id and certificate, the private
// key Run generated for the CSR, and the feeder's own
// desired_state_verification_key.
func Run(feederBaseURL string, store *identity.Store) error {
	if store == nil {
		return fmt.Errorf("enroll: Run: store must not be nil")
	}

	if _, alreadyEnrolled, err := store.Identity(); err != nil {
		return fmt.Errorf("enroll: Run: read persisted identity: %w", err)
	} else if alreadyEnrolled {
		return nil
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("enroll: Run: generate relay keypair: %w", err)
	}

	csrPEM, err := buildCSR(priv)
	if err != nil {
		return fmt.Errorf("enroll: Run: build CSR: %w", err)
	}

	client := bootstrapClient()

	claimToken, err := fetchClaimToken(client, feederBaseURL)
	if err != nil {
		return fmt.Errorf("enroll: Run: fetch claim token: %w", err)
	}

	resp, err := postEnroll(client, feederBaseURL, claimToken, csrPEM)
	if err != nil {
		return fmt.Errorf("enroll: Run: POST /enroll: %w", err)
	}

	verKey, err := decodeVerificationKey(resp.DesiredStateVerificationKey)
	if err != nil {
		return fmt.Errorf("enroll: Run: %w", err)
	}

	if err := store.SetIdentity(resp.RelayID, []byte(resp.Cert), priv); err != nil {
		return fmt.Errorf("enroll: Run: persist identity: %w", err)
	}
	if err := store.SetDesiredStateVerificationKey(verKey); err != nil {
		return fmt.Errorf("enroll: Run: persist desired_state_verification_key: %w", err)
	}

	return nil
}

// bootstrapClient returns an http.Client for the pre-enrollment bootstrap
// exchange (REL-010): server-authenticated TLS, but with no trust anchor
// yet to validate the feeder's self-signed certificate against — a relay
// holds no CA bundle and, per REL-011, a co-located (loopback) deployment
// MAY leave the out-of-band `trust_pin` implicit entirely. Skipping
// certificate verification here is that bootstrap exception made concrete
// for Wave-1 first-photon's co-located feeder+relay deployment; REL-137's
// pinned trust anchor governs every connection AFTER enrollment, once the
// relay actually has a key to pin against.
func bootstrapClient() *http.Client {
	return &http.Client{
		Timeout: bootstrapHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // REL-010/011 bootstrap exception, see doc above
		},
	}
}

// buildCSR generates a PEM-encoded PKCS#10 certificate signing request over
// priv — the relay's own generated keypair (REL-012's `csr`), proof of
// possession the feeder's own CSR.CheckSignature verifies before issuing a
// certificate.
func buildCSR(priv ed25519.PrivateKey) (string, error) {
	template := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "waiveo-relay"},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, template, priv)
	if err != nil {
		return "", fmt.Errorf("x509.CreateCertificateRequest: %w", err)
	}
	block := &pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}
	return string(pem.EncodeToMemory(block)), nil
}

// fetchClaimToken performs REL-011's `GET /claim-token` against the
// feeder's loopback enrollment server.
func fetchClaimToken(client *http.Client, feederBaseURL string) (string, error) {
	resp, err := client.Get(feederBaseURL + "/claim-token")
	if err != nil {
		return "", fmt.Errorf("GET /claim-token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /claim-token: status %d: %w", resp.StatusCode, decodeFeederError(resp.Body))
	}

	var body claimTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode claim-token response: %w", err)
	}
	if body.ClaimToken == "" {
		return "", fmt.Errorf("claim-token response carried an empty claim_token")
	}
	return body.ClaimToken, nil
}

// postEnroll performs REL-012's `POST /enroll` against the feeder's
// loopback enrollment server, returning its decoded success body.
func postEnroll(client *http.Client, feederBaseURL, claimToken, csrPEM string) (enrollResponse, error) {
	reqBody, err := json.Marshal(enrollRequest{ClaimToken: claimToken, CSR: csrPEM})
	if err != nil {
		return enrollResponse{}, fmt.Errorf("marshal enroll request: %w", err)
	}

	resp, err := client.Post(feederBaseURL+"/enroll", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return enrollResponse{}, fmt.Errorf("POST /enroll: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return enrollResponse{}, fmt.Errorf("status %d: %w", resp.StatusCode, decodeFeederError(resp.Body))
	}

	var body enrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return enrollResponse{}, fmt.Errorf("decode enroll response: %w", err)
	}
	return body, nil
}

// decodeFeederError best-effort decodes the feeder's `{code, message}`
// typed-refusal body (internal/feeder/enroll's errorBody) into a Go error,
// falling back to a generic error if the body isn't that shape.
func decodeFeederError(r io.Reader) error {
	var eb errorBody
	if err := json.NewDecoder(r).Decode(&eb); err != nil || eb.Code == "" {
		return fmt.Errorf("enrollment request refused")
	}
	return fmt.Errorf("%s: %s", eb.Code, eb.Message)
}

// decodeVerificationKey decodes a `desired_state_verification_key` value
// (internal/feeder/enroll's own grammar: `"ed25519:" + hex(pub)`, the
// contract's worked example) into an ed25519.PublicKey, returning
// ErrInvalidVerificationKey (wrapped with detail) for anything that isn't
// well-formed — including, per the Task 2 carry-forward, anything that
// doesn't decode to exactly ed25519.PublicKeySize bytes.
func decodeVerificationKey(s string) (ed25519.PublicKey, error) {
	const prefix = "ed25519:"
	if !strings.HasPrefix(s, prefix) {
		return nil, fmt.Errorf("%w: missing %q prefix: %q", ErrInvalidVerificationKey, prefix, s)
	}

	raw, err := hex.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil {
		return nil, fmt.Errorf("%w: not valid hex: %v", ErrInvalidVerificationKey, err)
	}

	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: decoded to %d bytes, want %d", ErrInvalidVerificationKey, len(raw), ed25519.PublicKeySize)
	}

	return ed25519.PublicKey(raw), nil
}
