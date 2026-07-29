// Package enroll implements the feeder's relay/1 loopback enrollment
// server (relay/1 Enrollment, REL-010–014): a claim-token endpoint (a
// co-located/loopback claim credential, REL-011), an enrollment endpoint
// that issues the relay a certificate and hands it the feeder's own
// desired-state signing public key — the trust anchor the relay persists
// and verifies every subsequent snapshot against (REL-012, REL-071,
// enrollment-anchored trust) — and the Expired-certificate re-enrollment
// surface (REL-020–027). These are the ONLY pre-mTLS HTTP routes: desired
// state itself moves exclusively over the authenticated persistent
// connection (internal/feeder/relayconn, state.pull/state.snapshot,
// REL-050/051), which this server feeds through its enrollment registry
// (RelayEnrollmentKey, IsRevoked, ClientCAPool).
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
	"sort"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// relayCertValidity is how long an issued relay leaf certificate is valid
// for. The in-band renew verb (REL-015) has not landed yet, but the relay
// now renews proactively over the bootstrap exchange (the REL-015
// draft-note's blessed transport) once a leaf enters its renewal window —
// cmd/waiveo-relay's relayRenewalWindow, 30 days ahead of this — so this
// stays generous rather than tuned.
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

// Server is the feeder's relay/1 enrollment server. Safe for concurrent use
// (its claim-token bookkeeping is mutex-guarded).
type Server struct {
	identity *signing.Identity

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

	// nowFn, when non-nil, replaces time.Now for certificate-issuance
	// validity windows (issueRelayCert) — the injectable-clock seam the
	// conformance notes require for timing-dependent behavior (renewal
	// windows), letting a test enroll a relay whose leaf is ALREADY expired
	// under this feeder's real CA without any wall-clock sleep. Production
	// never sets it.
	nowFn func() time.Time
}

// SetNowFunc installs (or, with nil, clears) the certificate-issuance clock
// override — see the nowFn field doc. Test-only by intent.
func (s *Server) SetNowFunc(fn func() time.Time) {
	s.mu.Lock()
	s.nowFn = fn
	s.mu.Unlock()
}

// now returns the issuance clock: nowFn when set, else time.Now.
func (s *Server) now() time.Time {
	s.mu.Lock()
	fn := s.nowFn
	s.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return time.Now()
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
// fresh, in-memory feeder CA and hands out identity's own desired-state
// signing public key as `desired_state_verification_key` — the trust anchor
// every snapshot the relay later pulls over the persistent connection
// verifies against (REL-012/071).
func NewServer(identity *signing.Identity) (*Server, error) {
	if identity == nil {
		return nil, fmt.Errorf("enroll: NewServer: identity must not be nil")
	}

	caCert, caKey, err := generateCA()
	if err != nil {
		return nil, fmt.Errorf("enroll: NewServer: generate CA: %w", err)
	}

	return &Server{
		identity:  identity,
		caCert:    caCert,
		caKey:     caKey,
		reLimiter: reenroll.NewRateLimiter(reEnrollRateLimit, reEnrollRateWindowMs),
		redeemed:  map[string]bool{},
		relayKeys: map[string]ed25519.PublicKey{},
		issuances: map[string][]issuance{},
	}, nil
}

// Register mounts the server's bootstrap routes (`/claim-token`, `/enroll`,
// and the re-enrollment pair below) onto mux. Callers serve mux over the
// feeder's own HTTPS listener (signing.Identity's TLS cert/key) — REL-010's
// server-authenticated, no-client-cert bootstrap TLS. These are the routes
// that CANNOT ride the authenticated persistent connection: they exist to
// bootstrap the very identity that connection authenticates with
// (REL-010/026).
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/claim-token", s.handleClaimToken)
	mux.HandleFunc("/enroll", s.handleEnroll)
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
		s.methodNotAllowed(w, r)
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
		s.methodNotAllowed(w, r)
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

// ActiveRelayIDs lists, in stable order, every relay_id this feeder has enrolled
// whose most-recently-issued certificate is NOT revoked — the relays that may
// still open a /relay/v1 connection (REL-016's check is over that same
// most-recent issuance) and therefore the relays that may still be serving
// screens from a desired-state generation this app peer handed them.
//
// It answers a question ConnectedRelays cannot: which relays EXIST. A relay that
// enrolled, applied a generation and then went offline is invisible to the
// connection registry while its screens keep fetching content from this feeder's
// origin — so anything that reasons about "what generation is the fleet on" has
// to start from the enrolled set and treat an absent member as unknown rather
// than as absent-therefore-irrelevant.
//
// A revoked relay is excluded deliberately. It has been cut off on purpose; if it
// counted, revoking one relay would hold the whole fleet's generation floor at
// whatever it last reported, forever.
func (s *Server) ActiveRelayIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.relayKeys))
	for relayID := range s.relayKeys {
		if list := s.issuances[relayID]; len(list) > 0 && list[0].revoked {
			continue
		}
		out = append(out, relayID)
	}
	sort.Strings(out)
	return out
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
// (REL-022), and consulted by the /relay/v1 connection listener at every
// connection attempt (REL-016, internal/feeder/relayconn). An unknown
// relay_id/serial pair is reported not-revoked; the caller's
// most-recently-issued check (reenroll.Eligible) is what refuses an
// unknown serial.
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

// Revoke marks the certificate with this serial, issued to relayID, as
// revoked in the issuance record — the revocation authority REL-016's
// connection-time check (IsRevoked, consumed by internal/feeder/relayconn)
// and REL-022's re-enrollment eligibility oracle both read. It reports
// whether a matching issuance was found; the update is persisted when
// persistence is enabled, so a revocation survives a feeder restart.
func (s *Server) Revoke(relayID, serial string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, iss := range s.issuances[relayID] {
		if iss.serial == serial {
			s.issuances[relayID][i].revoked = true
			if err := s.persistLocked(); err != nil {
				log.Printf("enroll: persist enrollment state after revoking %s/%s: %v", relayID, serial, err)
			}
			return true
		}
	}
	return false
}

// ClientCAPool returns a fresh x509.CertPool holding this feeder's
// enrollment CA certificate — the pool a TLS listener sets as ClientCAs so
// the leaf certificates this feeder itself issued at enrollment
// (issueRelayCert) verify as client certificates on the mutually
// authenticated relay/1 connection (REL-003/041). Call it AFTER
// EnablePersistence, which may replace the in-memory CA with the persisted
// one; the pool is a point-in-time copy and does not track a later CA
// change (none occurs within a process lifetime today).
func (s *Server) ClientCAPool() *x509.CertPool {
	s.mu.Lock()
	defer s.mu.Unlock()
	pool := x509.NewCertPool()
	pool.AddCert(s.caCert)
	return pool
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

	now := s.now()
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

// methodNotAllowed answers a bootstrap-endpoint request whose HTTP method is
// not the exchange's own with the relay/1 typed error frame over the REL-010
// bootstrap transport — MALFORMED_MESSAGE, the taxonomy's "did not satisfy its
// type's minimum shape" row (REL-002/003) — never a bare plain-text body
// (REL-007). No id: the refused request never presented a correlatable frame.
func (s *Server) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusMethodNotAllowed, reenroll.ErrorFrame{
		Type:    "error",
		TraceID: apihttp.TraceID(r),
		Code:    "MALFORMED_MESSAGE",
		Message: "this exchange does not accept " + r.Method + " requests.",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
