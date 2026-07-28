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

	"github.com/maaxton/waiveo-next/internal/relay/identity"
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

// errSessionPersistFailed wraps a durable-store write failure encountered
// while redeeming a pairing grant (EnablePersistence, below) — a distinct,
// non-taxonomy failure from PLY-036's two typed pairing-rejection codes: the
// grant selector itself was valid, only the durable write of its redemption
// result failed. handlePair maps this to a 500 INTERNAL rather than any
// PAIRING_* code, since attributing a storage failure to "your pairing code
// was invalid" would be actively misleading to a retrying player.
var errSessionPersistFailed = errors.New("playerserver: failed to persist pairing/session state durably")

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
	grantsGen      int64                        // desired-state generation the currently-redeemable grants set was applied for; SetPairingGrants fences a strictly-older write (REL-052/056), mirroring programGen below
	redeemedGrants map[string]bool              // grant_id -> redeemed (enforced only for one-time grants) — an in-process cache; grantAlreadyRedeemedLocked also consults sessionStore
	tokens         map[string]channelTokenRecord
	pollResults    map[string]redemption // poll_token -> completed result (PLY-034; see handlePairStatus doc)

	// sessionStore, when non-nil (EnablePersistence), is the relay's durable
	// operational store (internal/relay/identity) this Server ALSO persists
	// every minted channel token (hashed, never raw) and every one-time
	// pairing grant's redemption into, so both survive a relay process
	// restart (REL-120's sole-issuer/sole-verifier role; PLY-091/PLY-105's
	// own precedent for extending this tier — see EnablePersistence's doc).
	// Nil in every test and conformance-driver construction that never
	// calls EnablePersistence — those keep today's in-memory-only,
	// non-durable behavior byte-for-byte.
	sessionStore *identity.Store

	program    program                    // Task 10: SetProgram's own configured state
	programGen int64                      // desired-state generation the served program was applied for; SetProgram fences a strictly-older write (REL-052/056)
	signingKey ed25519.PrivateKey         // Task 10: relay's own key, signs every issued Lease (PLY-090)
	leaseAcks  map[string]LeaseAckRequest // Task 10: lease_id -> most recent LeaseAck (PLY-091)

	// revokedScreens is the relay's own last-synced view of the screen_ids in
	// relay/1 REL-066's revocation_and_site.revoked (PLY-072): a channel token
	// naming a screen_id present here is rejected CHANNEL_TOKEN_REVOKED, checked
	// against this local copy even while disconnected from the app peer, exactly
	// as REL-123 requires. A screen record the relay no longer recognizes at all
	// (PLY-075) is modeled the same way — RevokeScreen marks it revoked.
	revokedScreens map[string]bool

	// Playback telemetry a player posts (PLY-110/111): recorded in order of
	// arrival. Wave-1 records them in memory only — REL-090/093's durable
	// upstream forward of each as an events/1 content.played (PLY-113) is a
	// later task's scope; this record is what lets the interrupt-now swap's
	// own render reports be observed end to end today.
	renderStarts []RenderStartRequest
	renderEnds   []RenderEndRequest
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
		revokedScreens: map[string]bool{},
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
	mux.HandleFunc("/player/v1/render/start", s.handleRenderStart)
	mux.HandleFunc("/player/v1/render/end", s.handleRenderEnd)
}

// LookupChannelToken reports the screen_id and expires_at a previously
// minted channel token resolves to, and whether it is currently known at
// all — the accessor a later /player/v1/program task uses to validate a
// presented Authorization: Bearer channel token (PLY-076, Channel tokens).
func (s *Server) LookupChannelToken(token string) (screenID string, expiresAt int64, ok bool) {
	s.mu.Lock()
	rec, known := s.tokens[token]
	store := s.sessionStore
	s.mu.Unlock()
	if known {
		return rec.ScreenID, rec.ExpiresAt, true
	}
	if store == nil {
		return "", 0, false
	}

	// A cache miss with persistence enabled does not by itself mean the
	// token is unknown: this process's own s.tokens map starts empty on
	// every restart (NewServer), so it alone cannot distinguish "never
	// issued" from "issued and durably persisted in an EARLIER process
	// lifetime". Consult the durable store, keyed by the token's own hash
	// (identity.HashToken — EnablePersistence's own doc on why a hash
	// rather than the raw token) rather than the unrecoverable raw value.
	screenID, expiresAt, found, err := store.PlayerSession(identity.HashToken(token))
	if err != nil || !found {
		return "", 0, false
	}

	// Backfill the in-memory cache so this SAME token resolves from memory
	// on a screen's every later poll this process lifetime, without
	// repeating the durable lookup on every single one.
	s.mu.Lock()
	s.tokens[token] = channelTokenRecord{ScreenID: screenID, ExpiresAt: expiresAt}
	s.mu.Unlock()
	return screenID, expiresAt, true
}

// EnablePersistence points s at store — the relay's own durable operational
// SQLite store (internal/relay/identity, the SAME store SetLastAppliedGeneration
// and AppendTelemetry already write into) — as its durable session tier: every
// channel token this Server mints from here on is ALSO persisted there
// (hashed, never raw — identity.HashToken's own doc), and every one-time
// pairing grant's redemption is ALSO marked durably there, so a relay
// restart no longer strands an already-paired screen behind a channel token
// only this process's own memory ever knew about.
//
// This closes the amnesia relay/1's own sole-issuer/sole-verifier role
// (REL-120) otherwise leaves open: NewServer always builds s.tokens and
// s.redeemedGrants empty, so — absent this call — a screen whose channel
// token was minted before a relay restart gets CHANNEL_TOKEN_INVALID on its
// very next /player/v1/program poll, purely because the relay process
// restarted, not because anything about its credential actually changed.
// player/1 PLY-071's 24-hour bounded channel-token expiry, and PLY-091's/
// PLY-105's own precedent of extending this SAME durable tier — "relay/1's
// own operational storage, mirroring the persistence relay/1 REL-142
// already requires of it" — to Lease acknowledgement and preempt-grant
// state, both presuppose a token's (or a one-time grant's consumption)
// validity survives exactly this kind of restart.
//
// This is additive, not a load-back: unlike
// internal/feeder/enroll.Server.EnablePersistence (which replaces an
// in-memory registry wholesale from a prior JSON snapshot at call time), a
// hashed channel token cannot be recovered back into a plaintext-keyed
// in-memory map, so there is nothing to eagerly reload here. Instead,
// LookupChannelToken and redeem's own one-time-grant check
// (grantAlreadyRedeemedLocked) each fall through to store on their own
// in-memory cache miss — see their own docs — which is what actually makes
// a token or a grant's redemption state minted/marked in an EARLIER process
// lifetime resolve correctly in this one. Must be called before
// Register/serving traffic, to close the race between an incoming request
// and this field's assignment.
func (s *Server) EnablePersistence(store *identity.Store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionStore = store
}

// SetPairingGrants replaces s's redeemable pairing-grant set wholesale with
// grants, applying the SAME generation-fencing discipline SetProgram
// already applies to served program state (REL-052/056): a write whose
// generation is strictly older than the last one applied here is dropped,
// so a re-pull racing a newer generation's own SetPairingGrants call can
// never revert a screen's redeemable grant set to a superseded generation's.
//
// This is the setter relay/1 REL-122 requires and which NewServer alone
// never provided: "a pairing grant delivered via pairing_grants MUST remain
// redeemable ... until a newer generation supersedes it" presupposes a live
// re-pull CAN refresh what's redeemable; before this method existed, the
// grant set NewServer built at BOOT was frozen for the rest of the
// process's life, so even a fully-recovered live desired-state pull could
// never hand a screen the grant it actually needs. Callers apply this on
// every generation a re-pull applies (cmd/waiveo-relay's rePuller.tick),
// the same call site that already re-drives the served program and edge
// rules.
//
// redeemedGrants (REL-121's one-time-redemption bookkeeping) is left
// untouched by this call: a grant_id already marked redeemed stays marked
// redeemed regardless of whether the newer generation's set still carries
// that grant_id — a consumed one-time grant never becomes redeemable again
// merely because the surrounding grant set was superseded.
func (s *Server) SetPairingGrants(generation int64, grants []wire.PairingGrant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation < s.grantsGen {
		return // stale generation's late write — never revert a newer generation's grant set (REL-052/056)
	}
	s.grantsGen = generation
	grantIndex := make(map[string]wire.PairingGrant, len(grants))
	for _, g := range grants {
		grantIndex[g.GrantID] = g
	}
	s.grants = grantIndex
}

// RevokeScreen marks screenID revoked in the relay's own last-synced view of
// relay/1 REL-066's revocation_and_site.revoked (PLY-072): every channel token
// naming this screen_id is thereafter rejected CHANNEL_TOKEN_REVOKED, and the
// player it belongs to is driven to re-enter Pairing redemption (PLY-073),
// never to a futile renewal. This is the relay-side revocation surface a
// screen-record deletion or an explicit revocation entry drives; it also models
// PLY-075's "screen_id the relay no longer recognizes at all". Safe for
// concurrent use.
func (s *Server) RevokeScreen(screenID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokedScreens[screenID] = true
}

// isScreenRevoked reports whether screenID is present in the relay's own
// last-synced revocation view (PLY-072) — the check handleProgram runs on a
// presented token's screen_id before serving a Lease.
func (s *Server) isScreenRevoked(screenID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revokedScreens[screenID]
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
		if errors.Is(err, errSessionPersistFailed) {
			// The grant selector itself resolved fine — only the durable
			// write of its redemption result failed (disk full, store
			// unavailable). PLY-036's two typed codes both mean "your
			// pairing code itself is no good"; neither is accurate here, so
			// this is a 500 rather than either.
			apihttp.WriteProblem(w, r, traceID, http.StatusInternalServerError, "INTERNAL", "Internal Error")
			return
		}
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
	if grant.RedemptionMode == "one-time" && s.grantAlreadyRedeemedLocked(grant.GrantID) {
		return redemption{}, errPairingCodeInvalid
	}

	issuedAt := time.UnixMilli(grant.IssuedAt)
	if time.Now().After(issuedAt.Add(time.Duration(grant.TTL) * time.Second)) {
		return redemption{}, errPairingExpired
	}

	now := time.Now()

	if grant.RedemptionMode == "one-time" {
		s.redeemedGrants[grant.GrantID] = true
		if s.sessionStore != nil {
			if err := s.sessionStore.MarkPairingGrantRedeemed(grant.GrantID, now.UnixMilli()); err != nil {
				return redemption{}, fmt.Errorf("%w: %v", errSessionPersistFailed, err)
			}
		}
	}

	// REL-121a: a screen-bound grant's redemption results in exactly the
	// screen identity row the grant names — the screen_id a player learns
	// (PLY-035) IS the app's own screen row id (data-model/1 DAT-004a), so
	// the paired credential and the screen_programs entry that drives this
	// screen resolve to the SAME row. Only a grant that carries no binding
	// (the REL-121 baseline shape) still mints an opaque placeholder id.
	screenID := grant.ScreenID
	if screenID == "" {
		screenID = newOpaqueToken("screen")
	}

	rec := redemption{
		ChannelToken: newOpaqueToken("ct"),
		ScreenID:     screenID,
		IssuedAt:     now.UnixMilli(),
		ExpiresAt:    now.Add(channelTokenTTL).UnixMilli(),
	}
	s.tokens[rec.ChannelToken] = channelTokenRecord{ScreenID: rec.ScreenID, ExpiresAt: rec.ExpiresAt}
	if s.sessionStore != nil {
		if err := s.sessionStore.SetPlayerSession(identity.HashToken(rec.ChannelToken), rec.ScreenID, rec.ExpiresAt); err != nil {
			return redemption{}, fmt.Errorf("%w: %v", errSessionPersistFailed, err)
		}
	}

	return rec, nil
}

// grantAlreadyRedeemedLocked reports whether grantID's one-time grant has
// already been redeemed — checking the in-memory cache first, then (on a
// cache miss) the durable store, so a one-time grant redeemed in an earlier
// process lifetime stays consumed across a relay restart. Without this,
// NewServer's fresh, empty redeemedGrants map would let a one-time grant
// that was already fully redeemed before a restart be redeemed a SECOND
// time after one — relay/1 REL-121's own redemption-count-never-exceeds-one
// guarantee holding only within a single process lifetime, not across a
// restart, which is exactly the amnesia PLY-091/PLY-105 already name and
// fix for Lease-acknowledgement state (see EnablePersistence's own doc). The
// caller holds s.mu.
func (s *Server) grantAlreadyRedeemedLocked(grantID string) bool {
	if s.redeemedGrants[grantID] {
		return true
	}
	if s.sessionStore == nil {
		return false
	}
	redeemed, err := s.sessionStore.PairingGrantRedeemed(grantID)
	if err != nil || !redeemed {
		return false
	}
	s.redeemedGrants[grantID] = true // backfill the in-memory cache
	return true
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
