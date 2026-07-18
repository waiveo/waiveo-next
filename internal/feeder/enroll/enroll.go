// Package enroll implements the feeder's relay/1 loopback enrollment
// server (relay/1 Enrollment, REL-010–014): a claim-token endpoint (a
// co-located/loopback claim credential, REL-011), an enrollment endpoint
// that issues the relay a certificate and hands it the feeder's own
// desired-state signing public key — the trust anchor the relay persists
// and verifies every subsequent snapshot against (REL-012, REL-071,
// enrollment-anchored trust) — and a desired-state pull endpoint serving
// the feeder's one signed generation (Task 5's snapshot.Build output,
// relay/1 REL-051).
//
// The feeder acts as a minimal certificate authority for this loopback
// deployment: NewServer generates a fresh, in-memory, self-signed CA
// keypair distinct from both the feeder's own TLS-listener identity
// (internal/feeder/signing, which this HTTPS server is served over) and
// its desired-state signing key (also feeder/signing, handed to the relay
// as `desired_state_verification_key`) — three separate ed25519 keys, each
// with one distinct job. A relay's issued cert authenticates it in a later
// wave's mTLS-protected relay/1 connection (REL-003); nothing in this
// package's own endpoints requires a client certificate, per REL-010's
// bootstrap exception.
package enroll

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// relayCertValidity is how long an issued relay leaf certificate is valid
// for. Wave-1 first-photon has no in-band renewal (REL-015) yet, so this
// is generous rather than tuned.
const relayCertValidity = 365 * 24 * time.Hour

// Server is the feeder's relay/1 enrollment + desired-state-pull server.
// Safe for concurrent use (its claim-token bookkeeping is mutex-guarded).
type Server struct {
	identity *signing.Identity
	snapshot wire.StateSnapshotBody

	caCert *x509.Certificate
	caKey  ed25519.PrivateKey

	mu       sync.Mutex
	pending  string          // the currently unredeemed claim token, or "" if none minted yet
	redeemed map[string]bool // every token ever minted -> whether it has been redeemed
}

// NewServer builds an enroll.Server that issues relay certificates under a
// fresh, in-memory feeder CA, hands out identity's own desired-state
// signing public key as `desired_state_verification_key`, and serves
// snapshot verbatim from its pull endpoint. snapshot is expected to
// already be identity-signed (snapshot.Build's output) — NewServer does
// not sign or otherwise modify it.
func NewServer(identity *signing.Identity, snapshot wire.StateSnapshotBody) (*Server, error) {
	if identity == nil {
		return nil, fmt.Errorf("enroll: NewServer: identity must not be nil")
	}

	caCert, caKey, err := generateCA()
	if err != nil {
		return nil, fmt.Errorf("enroll: NewServer: generate CA: %w", err)
	}

	return &Server{
		identity: identity,
		snapshot: snapshot,
		caCert:   caCert,
		caKey:    caKey,
		redeemed: map[string]bool{},
	}, nil
}

// Register mounts the server's three routes (`/claim-token`, `/enroll`,
// `/state/pull`) onto mux. Callers serve mux over the feeder's own HTTPS
// listener (signing.Identity's TLS cert/key) — REL-010's server-authenticated,
// no-client-cert bootstrap TLS.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/claim-token", s.handleClaimToken)
	mux.HandleFunc("/enroll", s.handleEnroll)
	mux.HandleFunc("/state/pull", s.handleStatePull)
}

// claimTokenResponse is this package's loopback claim-token endpoint
// response — a co-located claim credential (relay/1 REL-011) MAY leave
// `app_endpoint`/`trust_pin` implicit, so only `claim_token` is carried.
type claimTokenResponse struct {
	ClaimToken string `json:"claim_token"`
}

// handleClaimToken mints (or, while one is still unredeemed, re-returns)
// a loopback claim credential. A fresh token is minted whenever none is
// currently pending — at server start, and again after the pending one is
// redeemed by a successful enrollment — so a later loopback enrollment
// (e.g. a re-provisioned relay) can obtain a new one without restarting
// the feeder.
func (s *Server) handleClaimToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	if s.pending == "" {
		token := newToken()
		s.pending = token
		s.redeemed[token] = false
	}
	token := s.pending
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, claimTokenResponse{ClaimToken: token})
}

// enrollRequest is REL-012's enrollment request body: `{claim_token, csr}`.
type enrollRequest struct {
	ClaimToken string `json:"claim_token"`
	CSR        string `json:"csr"`
}

// enrollResponse is REL-012's enrollment response body on success:
// `{relay_id, cert, not_before, not_after, desired_state_verification_key}`.
// not_before/not_after are epoch milliseconds (relay/1's Timestamp
// grammar, matching the contract's own worked example).
type enrollResponse struct {
	RelayID                     string `json:"relay_id"`
	Cert                        string `json:"cert"`
	NotBefore                   int64  `json:"not_before"`
	NotAfter                    int64  `json:"not_after"`
	DesiredStateVerificationKey string `json:"desired_state_verification_key"`
}

// errorBody is this server's typed-refusal shape (REL-013's
// `CLAIM_TOKEN_INVALID` and REL-007's `{code, message}` core, adapted to
// this package's plain-HTTP framing rather than the full relay/1
// `{type,id,trace_id,code,message}` error frame — this endpoint predates
// the enrolled, correlated connection REL-007's envelope presumes).
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// handleEnroll implements REL-012: on a valid, not-yet-redeemed
// claim_token and a well-formed CSR, issues the relay a certificate under
// the feeder's in-memory CA and hands back this feeder's own desired-state
// signing public key. A malformed, unknown, or already-redeemed
// claim_token is refused with a typed CLAIM_TOKEN_INVALID error (REL-013)
// — never a silent second enrollment.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req enrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "CLAIM_TOKEN_INVALID", "malformed enrollment request body")
		return
	}

	if !s.redeemToken(req.ClaimToken) {
		writeError(w, http.StatusForbidden, "CLAIM_TOKEN_INVALID", "claim_token is unknown or already redeemed")
		return
	}

	block, _ := pem.Decode([]byte(req.CSR))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		writeError(w, http.StatusBadRequest, "CLAIM_TOKEN_INVALID", "csr did not PEM-decode to a CERTIFICATE REQUEST block")
		return
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CLAIM_TOKEN_INVALID", "csr did not parse: "+err.Error())
		return
	}
	if err := csr.CheckSignature(); err != nil {
		// Proof of possession: the CSR must be signed by the private key
		// matching the public key it carries.
		writeError(w, http.StatusBadRequest, "CLAIM_TOKEN_INVALID", "csr signature did not verify: "+err.Error())
		return
	}

	relayID := newRelayID()

	certPEM, notBefore, notAfter, err := s.issueRelayCert(csr, relayID)
	if err != nil {
		http.Error(w, "internal error issuing certificate", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, enrollResponse{
		RelayID:                     relayID,
		Cert:                        string(certPEM),
		NotBefore:                   notBefore,
		NotAfter:                    notAfter,
		DesiredStateVerificationKey: encodeVerificationKey(s.identity.SigningPub()),
	})
}

// redeemToken reports whether token was a currently-pending, unredeemed
// claim token, atomically marking it redeemed and clearing the pending
// slot if so. Redeeming an unknown token, or re-presenting an
// already-redeemed one, returns false (REL-013) — the redeemed map
// remembers every token ever minted, so a repeat presentation is
// recognized as "already redeemed" rather than merely "unknown".
func (s *Server) redeemToken(token string) bool {
	if token == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	already, known := s.redeemed[token]
	if !known || already {
		return false
	}

	s.redeemed[token] = true
	if s.pending == token {
		s.pending = ""
	}
	return true
}

// handleStatePull implements relay/1's desired-state pull (REL-051),
// serving this server's one signed generation verbatim.
func (s *Server) handleStatePull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.snapshot)
}

// issueRelayCert issues a per-relay leaf certificate under s's CA, over
// csr's own public key (proof of possession already checked by the
// caller), returning it PEM-encoded plus its validity window as epoch
// milliseconds.
func (s *Server) issueRelayCert(csr *x509.CertificateRequest, relayID string) (certPEM []byte, notBefore, notAfter int64, err error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("enroll: issueRelayCert: generate serial: %w", err)
	}

	now := time.Now()
	nb := now.Add(-time.Hour) // small backdate, tolerating minor clock skew
	na := now.Add(relayCertValidity)

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: relayID},
		NotBefore:             nb,
		NotAfter:              na,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, s.caCert, csr.PublicKey, s.caKey)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("enroll: issueRelayCert: create certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return certPEM, nb.UnixMilli(), na.UnixMilli(), nil
}

// generateCA generates a fresh, in-memory, self-signed ed25519 CA
// certificate + key — this feeder instance's own minimal certificate
// authority, good for a loopback deployment (package doc). Never
// persisted: a fresh feeder process mints a fresh CA and, per REL-017,
// re-enrollment under a restarted feeder is treated as a fresh enrollment
// relationship — out of Wave-1 first-photon's scope (no in-band renewal or
// re-enrollment path is implemented by this package yet).
func generateCA() (*x509.Certificate, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "waiveo-feeder-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	return cert, priv, nil
}

// newToken returns a fresh, crypto-random claim token (REL-011's
// `claim_token` — the contract gives it no specific grammar beyond "a
// token"; this package's own choice is hex-encoded random bytes).
func newToken() string {
	return randomHex(16)
}

// newRelayID returns a fresh, permanent relay identity (REL-012's
// `relay_id`). REL-014 requires this never be derived or recomputed from a
// certificate's own serial number — it is generated independently, here,
// before the certificate itself is issued.
func newRelayID() string {
	return "relay-" + randomHex(16)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Reader failing is a fatal environment problem
		// (entropy source unavailable); there is no meaningful error to
		// propagate through these value-returning helpers.
		panic("enroll: randomHex: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// encodeVerificationKey formats an ed25519 public key as REL-012's
// `desired_state_verification_key` wire value — the contract's own worked
// example is `"ed25519:<hex>"`, so that is this codec's grammar.
func encodeVerificationKey(pub ed25519.PublicKey) string {
	return "ed25519:" + hex.EncodeToString(pub)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Code: code, Message: message})
}
