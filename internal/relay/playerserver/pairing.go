// Package playerserver implements the relay's player/1 server surface
// (contracts/player-1.md): pairing-code formation (relay/1 REL-126) and
// pairing redemption (`POST /player/v1/pair`, `GET /player/v1/pair/status`,
// PLY-030–037), turning a resolved pairing grant into a channel token a
// screen presents on every later request.
//
// This package MUST NOT leak a self-attesting authenticator (PLY-032,
// PLY-056): a PairingResponse carries trust_anchors and nothing else that
// could be mistaken for a relay-computed proof about those same
// trust_anchors. The fingerprint_commitment that DOES authenticate
// trust_anchors (out of band) is computed only at pairing-code formation
// time (FormPairingCode, REL-126), travels only inside the pairing code
// itself — never inside a PairingRequest/PairingResponse — and this
// package holds no state keyed by it: REL-126 is explicit that "the relay
// never receives fingerprint_commitment back from a player, and MUST NOT
// store it as, or treat it as part of, any redemption state."
package playerserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/paircode"
	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// channelTokenTTL bounds a minted channel token's lifetime at issuance —
// the banked PLY-071 value (no more than 24h after issuance).
const channelTokenTTL = 24 * time.Hour

// PairingRequest is player/1's PairingRequest body (PLY-030, PLY-012):
// hardware_id, capabilities, and — on the human-typed pairing-code path —
// grant_selector (the value a pairing code's paircode.Decode recovers
// alongside the fingerprint_commitment a player keeps local, PLY-053/054).
// Wave-1 first-photon implements only this pairing-code path server-side;
// the no-grant_selector trust-on-first-use path (PLY-055) is a later task.
type PairingRequest struct {
	HardwareID    string       `json:"hardware_id"`
	GrantSelector string       `json:"grant_selector,omitempty"`
	Capabilities  Capabilities `json:"capabilities"`
}

// Capabilities is player/1's capabilities object (PLY-012).
type Capabilities struct {
	ContentTypes  []string `json:"content_types"`
	PlayerVersion string   `json:"player_version"`
}

// TrustAnchor is one player/1 trust_anchors entry (PLY-042): covers names
// which purpose(s) — "player" and/or "content" — pem's issuing authority is
// scoped to.
type TrustAnchor struct {
	Covers []string `json:"covers"`
	PEM    string   `json:"pem"`
}

// PairingResponse is player/1's PairingResponse body (PLY-032/033).
//
// Field set is exactly the contract's own worked examples, no more: on a
// pending redemption, {trust_anchors, pairing_status, poll_token}; on a
// redeemed one, {trust_anchors, pairing_status, channel_token, screen_id,
// issued_at, expires_at}. PLY-056 forbids adding any relay-computed
// digest/checksum/fingerprint field here — TestPairingResponseCarriesNoAuthenticatorField
// pins this struct's marshaled key set exactly, so a future field added to
// this struct without updating that test's `allowed` set will fail loudly.
type PairingResponse struct {
	TrustAnchors  []TrustAnchor `json:"trust_anchors"`
	PairingStatus string        `json:"pairing_status"`
	PollToken     string        `json:"poll_token,omitempty"`
	ChannelToken  string        `json:"channel_token,omitempty"`
	ScreenID      string        `json:"screen_id,omitempty"`
	IssuedAt      int64         `json:"issued_at,omitempty"`
	ExpiresAt     int64         `json:"expires_at,omitempty"`
}

// Sentinel errors redeem returns — checked with errors.Is, and mapped to
// PLY-036's two typed codes by errorCode.
var (
	errPairingCodeInvalid = errors.New("grant_selector is absent, malformed, unresolvable, or already redeemed under a one-time grant")
	errPairingExpired     = errors.New("the pairing grant behind this selector has passed its ttl")
)

// channelTokenRecord is what a minted channel token resolves to: the
// screen_id it authorizes and its own bounded expiry — the record a later
// /player/v1/program task validates a presented token against.
type channelTokenRecord struct {
	ScreenID  string
	ExpiresAt int64
}

// redemption is one completed pairing-grant redemption's terminal result
// (PLY-033's redeemed shape, minus trust_anchors — a Server-wide value each
// response reuses, not a per-redemption one).
type redemption struct {
	ChannelToken string
	ScreenID     string
	IssuedAt     int64
	ExpiresAt    int64
}

// Server is the relay's player/1 pairing server: it resolves a
// PairingRequest's grant_selector against the pairing grants an already
// hash-and-signature-verified desired-state snapshot applied
// (internal/relay/desiredstate.Applied.PairingGrants), and mints a channel
// token on a valid redemption. Safe for concurrent use.
//
// This same Server also holds Task 10's program-delivery state (program,
// signingKey — configured by SetProgram) and lease/ack records: it is one
// player/1 server surface (pairing + program delivery + lease
// acknowledgement), not two, since PLY-070's channel token issued here is
// exactly the credential program.go's handlers validate.
type Server struct {
	relayCertPEM []byte

	mu             sync.Mutex
	grants         map[string]wire.PairingGrant // grant_id -> grant
	redeemedGrants map[string]bool              // grant_id -> redeemed (enforced only for one-time grants)
	tokens         map[string]channelTokenRecord
	pollResults    map[string]redemption // poll_token -> completed result (PLY-034; see handlePairStatus doc)

	program    program                    // Task 10: SetProgram's own configured state
	signingKey ed25519.PrivateKey         // Task 10: relay's own key, signs every issued Lease (PLY-090)
	leaseAcks  map[string]LeaseAckRequest // Task 10: lease_id -> most recent LeaseAck (PLY-091)
}

// NewServer builds a pairing Server that redeems against grants (the
// relay's own applied pairing_grants, REL-067) and presents relayCertPEM as
// this relay's sole trust_anchors entry (PLY-042) on every redemption —
// the same certificate relayCertPEM's own commitment
// (tlsboot.CommitmentForCertDER) is computed over at pairing-code formation
// time (FormPairingCode), so a player's local PLY-052 comparison is always
// checking the cert this server actually hands back.
func NewServer(relayCertPEM []byte, grants []wire.PairingGrant) (*Server, error) {
	block, _ := pem.Decode(relayCertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("playerserver: NewServer: relayCertPEM did not PEM-decode to a CERTIFICATE block")
	}

	grantIndex := make(map[string]wire.PairingGrant, len(grants))
	for _, g := range grants {
		grantIndex[g.GrantID] = g
	}

	return &Server{
		relayCertPEM:   relayCertPEM,
		grants:         grantIndex,
		redeemedGrants: map[string]bool{},
		tokens:         map[string]channelTokenRecord{},
		pollResults:    map[string]redemption{},
		leaseAcks:      map[string]LeaseAckRequest{},
	}, nil
}

// Register mounts the pairing AND program-delivery routes onto mux.
// Callers serve mux over the relay's own HTTPS player/1 listener, using the
// same certificate as relayCertPEM (NewServer) — PLY-001's stable
// /player/v1 path prefix, player/1's ordinary-HTTPS transport (PLY-001),
// never a persistent framed connection.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/player/v1/pair", s.handlePair)
	mux.HandleFunc("/player/v1/pair/status", s.handlePairStatus)
	mux.HandleFunc("/player/v1/program", s.handleProgram)
	mux.HandleFunc("/player/v1/lease/ack", s.handleLeaseAck)
}

// LookupChannelToken reports the screen_id and expires_at a previously
// minted channel token resolves to, and whether it is currently known at
// all — the accessor a later /player/v1/program task uses to validate a
// presented Authorization: Bearer channel token (PLY-076, Channel tokens).
func (s *Server) LookupChannelToken(token string) (screenID string, expiresAt int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, known := s.tokens[token]
	if !known {
		return "", 0, false
	}
	return rec.ScreenID, rec.ExpiresAt, true
}

// handlePair implements POST /player/v1/pair (PLY-030–033): decodes a
// PairingRequest, redeems its grant_selector, and responds either with a
// redeemed PairingResponse or a typed PLY-036 error — never a pending
// status that can never resolve, since Wave-1 first-photon's redemption is
// always synchronous.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	traceID := apihttp.TraceID(r)

	var req PairingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "PAIRING_CODE_INVALID", "Pairing Code Invalid")
		return
	}

	rec, err := s.redeem(req.GrantSelector)
	if err != nil {
		code := errorCode(err)
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, code, problemTitle(code))
		return
	}

	// PLY-032/PLY-056: trust_anchors and pairing_status only — no
	// relay-computed digest/checksum of relayCertPEM rides alongside it.
	writeJSON(w, http.StatusOK, PairingResponse{
		TrustAnchors:  []TrustAnchor{{Covers: []string{"player", "content"}, PEM: string(s.relayCertPEM)}},
		PairingStatus: "redeemed",
		ChannelToken:  rec.ChannelToken,
		ScreenID:      rec.ScreenID,
		IssuedAt:      rec.IssuedAt,
		ExpiresAt:     rec.ExpiresAt,
	})
}

// handlePairStatus implements GET /player/v1/pair/status (PLY-034):
// resolves a presented poll_token to a completed redemption result.
//
// Wave-1 first-photon's /pair (handlePair) always redeems synchronously —
// it never returns pairing_status: pending — so no poll_token is ever
// minted or outstanding for this handler to resolve. It exists so
// player/1's path and shape are faithfully present (a later minor or a
// deployment with a genuinely asynchronous redemption step can populate
// pollResults without changing this handler), and it correctly refuses any
// poll_token presented against it today, since none was ever issued.
func (s *Server) handlePairStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pollToken := r.URL.Query().Get("poll_token")

	s.mu.Lock()
	rec, known := s.pollResults[pollToken]
	s.mu.Unlock()

	if pollToken == "" || !known {
		apihttp.WriteProblem(w, r, apihttp.TraceID(r), http.StatusBadRequest, "PAIRING_CODE_INVALID", "Pairing Code Invalid")
		return
	}

	writeJSON(w, http.StatusOK, PairingResponse{
		PairingStatus: "redeemed",
		ChannelToken:  rec.ChannelToken,
		ScreenID:      rec.ScreenID,
		IssuedAt:      rec.IssuedAt,
		ExpiresAt:     rec.ExpiresAt,
	})
}

// redeem resolves selector against s.grants and, on success, atomically
// (under s.mu) marks a one-time grant redeemed and mints a fresh channel
// token + screen_id (PLY-035: a screen_id first exists only in a redeemed
// result). Every rejection path — absent, unresolvable, expired, or an
// already-redeemed one-time grant — returns a typed sentinel error
// (PLY-036) rather than ever partially minting a credential.
//
// The whole check-then-mark sequence runs under one lock acquisition so two
// concurrent PairingRequests racing the same one-time grant_selector cannot
// both observe "not yet redeemed" and both mint a credential — exactly one
// wins, and every path here would leave a one-time grant no more than
// once-redeemed even under real concurrency, not merely in the common
// sequential case.
func (s *Server) redeem(selector string) (redemption, error) {
	if selector == "" {
		return redemption{}, errPairingCodeInvalid
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	grant, known := s.grants[selector]
	if !known {
		return redemption{}, errPairingCodeInvalid
	}
	if grant.RedemptionMode == "one-time" && s.redeemedGrants[grant.GrantID] {
		return redemption{}, errPairingCodeInvalid
	}

	issuedAt := time.UnixMilli(grant.IssuedAt)
	if time.Now().After(issuedAt.Add(time.Duration(grant.TTL) * time.Second)) {
		return redemption{}, errPairingExpired
	}

	if grant.RedemptionMode == "one-time" {
		s.redeemedGrants[grant.GrantID] = true
	}

	now := time.Now()
	rec := redemption{
		ChannelToken: newOpaqueToken("ct"),
		ScreenID:     newOpaqueToken("screen"),
		IssuedAt:     now.UnixMilli(),
		ExpiresAt:    now.Add(channelTokenTTL).UnixMilli(),
	}
	s.tokens[rec.ChannelToken] = channelTokenRecord{ScreenID: rec.ScreenID, ExpiresAt: rec.ExpiresAt}

	return rec, nil
}

// errorCode maps redeem's sentinel errors to PLY-036's registry codes.
func errorCode(err error) string {
	if errors.Is(err, errPairingExpired) {
		return "PAIRING_EXPIRED"
	}
	return "PAIRING_CODE_INVALID"
}

// problemTitle maps a PLY-036 registry code to the short human-readable
// Problem `title` this package writes for it (Wire shapes' own worked
// examples are Title Case of the code's meaning, e.g. "Not Found").
func problemTitle(code string) string {
	if code == "PAIRING_EXPIRED" {
		return "Pairing Expired"
	}
	return "Pairing Code Invalid"
}

// FormPairingCode forms a relay/1 REL-126 pairing code for grant: the
// relay's own dial address (host, port), grant's own grant_id as the
// grant_selector a PairingRequest later presents, and a
// fingerprint_commitment computed over relayCertDER — the SAME certificate
// this relay serves player/1 over and hands back as trust_anchors on
// redemption (NewServer's relayCertPEM, PEM-decoded to the DER this
// function takes).
//
// Per REL-126, the commitment's role ends here: it is displayed inside the
// returned code and never received back from a player, and this function —
// like Server — holds no state keyed by it.
func FormPairingCode(host string, port int, grant wire.PairingGrant, relayCertDER []byte) (string, error) {
	commitment, err := tlsboot.CommitmentForCertDER(relayCertDER)
	if err != nil {
		return "", fmt.Errorf("playerserver: FormPairingCode: commitment: %w", err)
	}
	return paircode.Encode(host, port, grant.GrantID, commitment), nil
}

// newOpaqueToken returns a fresh, crypto-random opaque identifier, prefixed
// for readability in logs — the same random-hex convention
// internal/feeder/enroll and internal/feeder/grant already use for their
// own opaque tokens/ids (neither channel_token nor screen_id is given a
// mandated grammar by player/1 beyond "opaque"/"first comes into existence
// at redemption").
func newOpaqueToken(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Reader failing is a fatal environment problem
		// (entropy source unavailable); there is no meaningful error to
		// propagate through this value-returning helper.
		panic("playerserver: newOpaqueToken: " + err.Error())
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
