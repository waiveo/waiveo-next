package reenroll

// This file holds the relay (client) side of the Expired-certificate
// re-enrollment path (REL-014/017/020/027): the counterpart to the feeder's
// handler in internal/feeder/enroll. A relay whose certificate has expired
// during a long power-off — but whose private key it deliberately retained
// past expiry (REL-142) — recovers its identity without a fresh claim
// credential by proving possession of that expired certificate's OWN private
// key over a fresh challenge nonce bound to a brand-new CSR, and persists the
// fresh certificate under the SAME relay_id while re-anchoring its trust anchor.
//
// The connection is not yet mutually authenticated here (the relay's cert is
// expired), so, like the ordinary enrollment bootstrap (internal/relay/enroll,
// REL-010/011), this exchange runs over server-authenticated TLS with
// verification disabled for the co-located loopback feeder. The application-layer
// pop_signature — not the transport — is what proves the relay's identity.

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
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// bootstrapHTTPTimeout bounds the loopback re-enrollment exchange — a
// co-located, same-host call (REL-010's bootstrap exception), so a short
// timeout is generous.
const bootstrapHTTPTimeout = 10 * time.Second

// ReEnroll drives the Expired-certificate re-enrollment path for the relay
// whose identity store holds an expired-but-unrevoked certificate, against the
// feeder at feederBaseURL (e.g. "https://127.0.0.1:7420").
//
// It reads the relay's persisted identity (its relay_id, the certificate it is
// presenting for re-enrollment, and that certificate's OWN retained private
// key, REL-142), generates a FRESH keypair + CSR (never the expired one),
// fetches the bootstrap challenge nonce (REL-026), signs the proof-of-possession
// over the nonce bound to that fresh CSR with the EXPIRED certificate's private
// key (REL-027), and presents the renew request. On success it persists the
// fresh certificate + fresh private key under the SAME relay_id (REL-014) and
// re-anchors (replaces) the persisted desired_state_verification_key (REL-017).
//
// A typed refusal from the app peer (CERT_EXPIRED_INELIGIBLE, RE_ENROLL_POP_INVALID,
// RE_ENROLL_RATE_LIMITED) is returned as a *ReEnrollError carrying the frame's
// code+message, so a caller can branch on errors.As; the persisted identity is
// left untouched on any error.
func ReEnroll(feederBaseURL string, store *identity.Store) error {
	if store == nil {
		return errors.New("reenroll: ReEnroll: store must not be nil")
	}

	id, ok, err := store.Identity()
	if err != nil {
		return fmt.Errorf("reenroll: ReEnroll: read persisted identity: %w", err)
	}
	if !ok {
		return errors.New("reenroll: ReEnroll: no persisted identity to re-enroll")
	}

	// A fresh keypair + CSR — a brand-new key, never the expired certificate's
	// own. The relay proves possession of the EXPIRED key (below); this new key
	// is what the fresh certificate is issued over.
	_, newPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("reenroll: ReEnroll: generate fresh keypair: %w", err)
	}
	csrPEM, csrDER, err := buildCSR(newPriv, id.RelayID)
	if err != nil {
		return fmt.Errorf("reenroll: ReEnroll: build CSR: %w", err)
	}

	client := bootstrapClient()

	nonce, err := fetchChallenge(client, feederBaseURL)
	if err != nil {
		return fmt.Errorf("reenroll: ReEnroll: fetch challenge: %w", err)
	}

	// Proof of possession: sign the nonce bound to THIS csr with the EXPIRED
	// certificate's own private key (REL-027), never newPriv.
	popSig := SignPoP(id.PrivateKey, nonce, csrDER)

	ack, err := postRenew(client, feederBaseURL, RenewRequest{
		Type:    "renew",
		ID:      ulid.New(),
		RelayID: id.RelayID,
		Body: RenewBody{
			PresentedCert: string(id.CertPEM),
			CSR:           csrPEM,
			PoPSignature:  popSig,
		},
	})
	if err != nil {
		return err
	}

	verKey, err := decodeVerificationKey(ack.Body.DesiredStateVerificationKey)
	if err != nil {
		return fmt.Errorf("reenroll: ReEnroll: %w", err)
	}

	// Persist the fresh certificate + fresh private key under the SAME relay_id
	// (REL-014), then re-anchor the trust anchor (REL-017). Identity first: a
	// crash between the two leaves a usable cert to retry re-anchoring against,
	// never a re-anchored key with no matching certificate.
	if err := store.SetIdentity(id.RelayID, []byte(ack.Body.Cert), newPriv); err != nil {
		return fmt.Errorf("reenroll: ReEnroll: persist fresh identity: %w", err)
	}
	if err := store.SetDesiredStateVerificationKey(verKey); err != nil {
		return fmt.Errorf("reenroll: ReEnroll: re-anchor desired_state_verification_key: %w", err)
	}
	return nil
}

// bootstrapClient returns an http.Client for the re-enrollment bootstrap
// exchange: server-authenticated TLS with verification disabled, the same
// REL-010/011 bootstrap exception the ordinary enrollment client relies on —
// the relay holds no CA bundle for the feeder's self-signed certificate, and
// the pop_signature (not the transport) proves the relay's own identity here.
func bootstrapClient() *http.Client {
	return &http.Client{
		Timeout: bootstrapHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // REL-010/011 bootstrap exception
		},
	}
}

// buildCSR generates a PEM-encoded PKCS#10 certificate-signing request over
// priv (the relay's fresh keypair), returning both the PEM and the raw DER —
// the DER is the exact bytes SignPoP binds (REL-027), so it MUST be the same
// bytes the feeder recovers when it PEM-decodes this CSR.
func buildCSR(priv ed25519.PrivateKey, commonName string) (csrPEM string, der []byte, err error) {
	template := &x509.CertificateRequest{Subject: pkix.Name{CommonName: commonName}}
	der, err = x509.CreateCertificateRequest(rand.Reader, template, priv)
	if err != nil {
		return "", nil, fmt.Errorf("x509.CreateCertificateRequest: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), der, nil
}

// fetchChallenge performs the re-enroll bootstrap challenge (REL-026,
// GET /reenroll/challenge) and returns its fresh single-use nonce.
func fetchChallenge(client *http.Client, feederBaseURL string) (string, error) {
	resp, err := client.Get(feederBaseURL + "/reenroll/challenge")
	if err != nil {
		return "", fmt.Errorf("GET /reenroll/challenge: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /reenroll/challenge: status %d", resp.StatusCode)
	}
	// The challenge reuses internal/relay/hello's Challenge shape (REL-026):
	// {type:"challenge", body:{nonce}}. Decoded inline to avoid importing hello.
	var c struct {
		Body struct {
			Nonce string `json:"nonce"`
		} `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return "", fmt.Errorf("decode challenge: %w", err)
	}
	if c.Body.Nonce == "" {
		return "", errors.New("challenge response carried an empty nonce")
	}
	return c.Body.Nonce, nil
}

// postRenew presents the renew request (POST /reenroll/renew) and decodes the
// reply. A 200 decodes to a RenewAck; any other status decodes the app peer's
// ErrorFrame and is returned as a *ReEnrollError carrying its code+message
// (CERT_EXPIRED_INELIGIBLE / RE_ENROLL_POP_INVALID / RE_ENROLL_RATE_LIMITED).
func postRenew(client *http.Client, feederBaseURL string, req RenewRequest) (RenewAck, error) {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return RenewAck{}, fmt.Errorf("marshal renew request: %w", err)
	}
	resp, err := client.Post(feederBaseURL+"/reenroll/renew", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return RenewAck{}, fmt.Errorf("POST /reenroll/renew: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return RenewAck{}, fmt.Errorf("read renew response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var ef ErrorFrame
		if err := json.Unmarshal(raw, &ef); err != nil || ef.Code == "" {
			return RenewAck{}, fmt.Errorf("re-enroll renew refused: status %d", resp.StatusCode)
		}
		return RenewAck{}, &ReEnrollError{Code: ef.Code, Message: ef.Message}
	}

	var ack RenewAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		return RenewAck{}, fmt.Errorf("decode renew-ack: %w", err)
	}
	if ack.Body.Cert == "" {
		return RenewAck{}, errors.New("renew-ack carried an empty certificate")
	}
	return ack, nil
}

// decodeVerificationKey decodes a `desired_state_verification_key` value (the
// feeder's `"ed25519:" + hex(pub)` grammar) into an ed25519.PublicKey, refusing
// anything that does not decode to exactly ed25519.PublicKeySize bytes — a
// wrong-length key must never be persisted or handed to signhash.Verify (which
// would otherwise panic).
func decodeVerificationKey(s string) (ed25519.PublicKey, error) {
	const prefix = "ed25519:"
	if !strings.HasPrefix(s, prefix) {
		return nil, fmt.Errorf("desired_state_verification_key missing %q prefix: %q", prefix, s)
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil {
		return nil, fmt.Errorf("desired_state_verification_key not valid hex: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("desired_state_verification_key decoded to %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}
