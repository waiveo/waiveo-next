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
	"log"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// relayCertValidity is how long an issued relay leaf certificate is valid
// for. Wave-1 first-photon has no in-band renewal (REL-015) yet, so this
// is generous rather than tuned.
const relayCertValidity = 365 * 24 * time.Hour

// reEnrollRateLimit / reEnrollRateWindowMs bound how often a single relay_id
// may exercise the Expired-certificate re-enrollment path (REL-025), so it
// cannot be used to force repeated identity churn. Generous relative to the
// once-in-a-long-power-off legitimate use; the window is fixed and per
// relay_id (internal/relay/reenroll.RateLimiter).
const (
	reEnrollRateLimit    = 20
	reEnrollRateWindowMs = 60_000
)

// Server is the feeder's relay/1 enrollment + desired-state-pull server.
// Safe for concurrent use (its claim-token bookkeeping is mutex-guarded).
type Server struct {
	identity *signing.Identity
	snapshot wire.StateSnapshotBody

	// snapshotProvider, when set (SetSnapshotProvider), supersedes the static
	// snapshot: handleStatePull calls it to obtain the CURRENT-generation desired
	// state on each pull, so the feeder can serve a store-derived snapshot rebuilt
	// as the app store's generation advances (the authoring loop) rather than one
	// frozen snapshot. nil (the default) preserves the original behavior — serve
	// the snapshot NewServer was given, verbatim. Read/written under mu.
	snapshotProvider func() (wire.StateSnapshotBody, error)

	caCert *x509.Certificate
	caKey  ed25519.PrivateKey

	// reLimiter bounds the Expired-certificate re-enrollment path per relay_id
	// (REL-025). Not guarded by mu — it has its own internal lock.
	reLimiter *reenroll.RateLimiter

	mu            sync.Mutex
	pending       string                       // the currently unredeemed claim token, or "" if none minted yet
	redeemed      map[string]bool              // every token ever minted -> whether it has been redeemed
	relayKeys     map[string]ed25519.PublicKey // relay_id -> the enrollment public key this feeder issued a cert over
	issuances     map[string][]issuance        // relay_id -> certs this feeder has issued, most-recently-issued first
	reOutstanding string                       // the currently-issued, not-yet-consumed re-enroll challenge nonce (REL-026)

	// persistPath, when non-empty (EnablePersistence), is the file s's
	// enrollment registry (relayKeys/issuances/redeemed/pending/caCert/caKey)
	// is durably written to after every mutation, so a restarted feeder
	// process reloads exactly what it held before — see EnablePersistence's
	// own doc for why this matters (REL-032 channel-binding continuity
	// across a restart). Empty by default: NewServer alone keeps today's
	// in-memory-only, forgets-on-restart behavior unchanged for every
	// caller that never opts in (tests, conformance drivers).
	persistPath string
}

// issuance is one certificate this feeder issued under a relay_id: its serial
// (the same string form a relay presents, its cert's own SerialNumber), the
// public key it was issued over, when it was issued, and whether it has since
// been revoked. The Expired-certificate re-enrollment path's eligibility
// check (internal/relay/reenroll, REL-021/022) reads this record — never a
// revocation list.
type issuance struct {
	serial   string
	pub      ed25519.PublicKey
	issuedAt int64
	revoked  bool
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
		identity:  identity,
		snapshot:  snapshot,
		caCert:    caCert,
		caKey:     caKey,
		reLimiter: reenroll.NewRateLimiter(reEnrollRateLimit, reEnrollRateWindowMs),
		redeemed:  map[string]bool{},
		relayKeys: map[string]ed25519.PublicKey{},
		issuances: map[string][]issuance{},
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
	// Expired-certificate re-enrollment bootstrap surface (REL-020/024/026):
	// a challenge issuer and the pop-guarded renew handler, served over the
	// same server-authenticated bootstrap TLS as the rest of enrollment.
	mux.HandleFunc("/reenroll/challenge", s.handleReEnrollChallenge)
	mux.HandleFunc("/reenroll/renew", s.handleReEnrollRenew)
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
		if err := s.persistLocked(); err != nil {
			log.Printf("enroll: persist enrollment state after minting claim token: %v", err)
		}
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

	traceID := apihttp.TraceID(r)

	var req enrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "CLAIM_TOKEN_INVALID", "Claim Token Invalid")
		return
	}

	if !s.redeemToken(req.ClaimToken) {
		apihttp.WriteProblem(w, r, traceID, http.StatusForbidden, "CLAIM_TOKEN_INVALID", "Claim Token Invalid")
		return
	}

	block, _ := pem.Decode([]byte(req.CSR))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "CLAIM_TOKEN_INVALID", "Claim Token Invalid")
		return
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "CLAIM_TOKEN_INVALID", "Claim Token Invalid")
		return
	}
	if err := csr.CheckSignature(); err != nil {
		// Proof of possession: the CSR must be signed by the private key
		// matching the public key it carries.
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "CLAIM_TOKEN_INVALID", "Claim Token Invalid")
		return
	}

	relayID := newRelayID()

	certPEM, serial, notBefore, notAfter, err := s.issueRelayCert(csr, relayID)
	if err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusInternalServerError, "INTERNAL", "Internal Error")
		return
	}

	// Record the relay's enrollment public key against its relay_id — the
	// connection handshake's channel-binding verification (REL-032, via
	// internal/relay/hello) looks it up here to check hello's signature over
	// the challenge nonce. csr.PublicKey is an ed25519 key for every CSR this
	// deployment's relay client (internal/relay/enroll) generates; a non-ed25519
	// CSR simply records nothing and a later hello for it fails to bind.
	//
	// Also record the issuance (serial + public key + issued-at) as the
	// most-recently-issued certificate for this relay_id — the issuance record
	// the Expired-certificate re-enrollment path evaluates eligibility against
	// (internal/relay/reenroll, REL-021/022), never a revocation list.
	if pub, ok := csr.PublicKey.(ed25519.PublicKey); ok {
		s.mu.Lock()
		s.relayKeys[relayID] = pub
		s.recordIssuance(relayID, serial, pub, notBefore)
		if err := s.persistLocked(); err != nil {
			log.Printf("enroll: persist enrollment state after enrolling %s: %v", relayID, err)
		}
		s.mu.Unlock()
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
	if err := s.persistLocked(); err != nil {
		log.Printf("enroll: persist enrollment state after redeeming claim token: %v", err)
	}
	return true
}

// RelayEnrollmentKey returns the enrollment public key this feeder recorded
// for relayID when it issued that relay's certificate (REL-012), and whether
// one is on record. It is the RelayKeyLookup the connection handshake's
// app-peer server (internal/relay/hello) verifies a hello's
// channel_binding_signature against (REL-032) — never a key derived from the
// hello itself. A relay this feeder process never enrolled returns ok=false,
// which the handshake treats as an unverifiable binding and refuses.
func (s *Server) RelayEnrollmentKey(relayID string) (ed25519.PublicKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pub, ok := s.relayKeys[relayID]
	return pub, ok
}

// recordIssuance prepends a freshly issued certificate to relayID's issuance
// list so the head is always the most-recently-issued (REL-021). The caller
// holds s.mu.
func (s *Server) recordIssuance(relayID, serial string, pub ed25519.PublicKey, issuedAt int64) {
	s.issuances[relayID] = append([]issuance{{
		serial:   serial,
		pub:      pub,
		issuedAt: issuedAt,
	}}, s.issuances[relayID]...)
}

// MostRecentSerial returns the serial + public key of the
// most-recently-issued certificate this feeder issued for relayID, and whether
// any issuance is on record — satisfying reenroll.IssuanceRecord (REL-021).
// serial is the certificate's own SerialNumber in the exact string form a
// relay presents it (see issueRelayCert).
func (s *Server) MostRecentSerial(relayID string) (string, ed25519.PublicKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.issuances[relayID]
	if len(list) == 0 {
		return "", nil, false
	}
	head := list[0]
	return head.serial, head.pub, true
}

// IsRevoked reports whether the certificate with this serial, issued to
// relayID, is recorded as revoked — satisfying reenroll.IssuanceRecord
// (REL-022). An unknown relay_id/serial pair is reported not-revoked; the
// caller's most-recently-issued check (reenroll.Eligible) is what refuses an
// unknown serial. Wave-1 first-photon has no revocation surface yet (REL-016
// is a deferred follow-up), so no issuance is ever marked revoked today; this
// method exists so the eligibility oracle is complete and honours a revoked
// entry the moment that surface lands.
func (s *Server) IsRevoked(relayID, serial string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, iss := range s.issuances[relayID] {
		if iss.serial == serial {
			return iss.revoked
		}
	}
	return false
}

// SetSnapshotProvider installs a desired-state source that supersedes the static
// snapshot: every subsequent desired-state pull is answered by calling provider,
// so the feeder can serve a store-derived snapshot at the store's current
// generation (rebuilt as the generation advances). Passing nil reverts to serving
// the static snapshot NewServer was given. Intended to be called once at startup
// before the listener accepts pulls; guarded by mu so it is nonetheless safe
// against a concurrent pull.
func (s *Server) SetSnapshotProvider(provider func() (wire.StateSnapshotBody, error)) {
	s.mu.Lock()
	s.snapshotProvider = provider
	s.mu.Unlock()
}

// currentSnapshot returns the desired state to serve: the provider's
// current-generation snapshot when one is installed, else the static snapshot.
func (s *Server) currentSnapshot() (wire.StateSnapshotBody, error) {
	s.mu.Lock()
	provider := s.snapshotProvider
	static := s.snapshot
	s.mu.Unlock()
	if provider != nil {
		return provider()
	}
	return static, nil
}

// handleStatePull implements relay/1's desired-state pull (REL-051), serving the
// feeder's current signed generation — the provider-derived one when a snapshot
// provider is installed (the store-driven authoring loop), else the static
// snapshot verbatim. A provider error is a 500: the relay treats a failed pull as
// non-fatal and keeps serving its last-applied generation (REL-055), so a
// transient derive failure never corrupts the served program.
func (s *Server) handleStatePull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap, err := s.currentSnapshot()
	if err != nil {
		http.Error(w, "desired state unavailable", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// issueRelayCert issues a per-relay leaf certificate under s's CA, over
// csr's own public key (proof of possession already checked by the
// caller), returning it PEM-encoded plus its validity window as epoch
// milliseconds.
func (s *Server) issueRelayCert(csr *x509.CertificateRequest, relayID string) (certPEM []byte, serialStr string, notBefore, notAfter int64, err error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("enroll: issueRelayCert: generate serial: %w", err)
	}

	now := time.Now()
	nb := now.Add(-time.Hour) // small backdate, tolerating minor clock skew
	na := now.Add(relayCertValidity)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: relayID},
		NotBefore:    nb,
		NotAfter:     na,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// The relay presents this one leaf in BOTH roles: as a TLS client to
		// the feeder (mTLS re-enrollment) and as the player/1 TLS SERVER to
		// screens. A client-auth-only EKU made a PLY-060 ordinary-verification
		// client (the Roku player, firmware 15.2.4) reject the program poll
		// with "unsuitable certificate purpose" — a failure the virtualplayer's
		// skip-verify+SPKI-pin posture could never surface.
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, s.caCert, csr.PublicKey, s.caKey)
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("enroll: issueRelayCert: create certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// serialStr is the certificate's own SerialNumber rendered exactly as a
	// relay presenting that certificate later renders it (big.Int.Text(16),
	// lowercase hex, no leading zeros) — so the issuance record and a
	// presented serial compare as equal strings in reenroll.Eligible (REL-021).
	return certPEM, serial.Text(16), nb.UnixMilli(), na.UnixMilli(), nil
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
