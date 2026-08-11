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
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/paircode"
	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
	"github.com/maaxton/waiveo-next/internal/slidelive"
)

// channelTokenTTL bounds a minted channel token's lifetime at issuance —
// the banked PLY-071 value (no more than 24h after issuance).
const channelTokenTTL = 24 * time.Hour

// Pairing attempt-budget defaults (security-model/1 SEC-033): 10 redemption
// attempts per source address per 15 minutes.
//
// They are the app's own grant-budget numbers verbatim
// (internal/app/auth.DefaultGrantAttemptLimit/DefaultGrantAttemptWindowMs), not
// a second policy: SEC-033 is one requirement over one kind of secret, and a
// relay that bounded pairing-code guesses differently from the app bounding
// setup-code guesses would be two policies nobody could reason about together.
// Against a grant_id carrying SEC-032's 128-bit floor this is astronomically
// more than an attacker needs to be hopeless, and far more than a human
// fumbling a hand-typed pairing code on a TV remote needs.
const (
	pairAttemptLimit          = 10
	pairAttemptWindowMs int64 = 15 * 60_000
)

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
// screen_id it authorizes, its own bounded expiry, and whether the relay has
// since DROPPED the session — the record a later /player/v1/program task
// validates a presented token against.
//
// Terminated is a tombstone, not a deletion, and the record stays resolvable
// after it is set: a dropped session's token must still resolve to its
// screen_id, because PLY-072 requires a token naming a currently-revoked screen
// be refused `CHANNEL_TOKEN_REVOKED` specifically, and a relay that had
// forgotten which screen the token named could only answer
// `CHANNEL_TOKEN_INVALID`. See SetRevokedScreens for what sets it.
type channelTokenRecord struct {
	ScreenID   string
	ExpiresAt  int64
	Terminated bool
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
// signingKey — installed by SetSigningKey) and lease/ack records: it is one
// player/1 server surface (pairing + program delivery + lease
// acknowledgement), not two, since PLY-070's channel token issued here is
// exactly the credential program.go's handlers validate.
type Server struct {
	relayCertPEM []byte

	// relayID is this relay's OWN enrolled identity, read from the subject
	// CommonName of relayCertPEM at construction — the same value the
	// enrollment issuer put there (internal/feeder/enroll issues every relay
	// leaf with `Subject: pkix.Name{CommonName: relayID}`) and the same value
	// the app peer authenticates the relay by on its own connection, which
	// takes the identity from the mTLS client certificate and never from a
	// self-asserted field (REL-041/150).
	//
	// It is derived rather than passed in on purpose: REL-121b's binding check
	// compares a grant's `relay_id` against "the relay's OWN enrolled identity
	// — the identity its enrollment-issued certificate carries", so reading it
	// from the very certificate this server presents as its trust anchor makes
	// the two impossible to desync. A caller-supplied string could drift from
	// the certificate and would silently decide which grants this relay may
	// consume.
	relayID string

	// nowMs is the clock every time-dependent decision this server makes reads
	// — a pairing grant's ttl at redemption (REL-121), a minted channel token's
	// issued_at/expires_at (PLY-071), a presented token's expiry (PLY-072), an
	// issued Lease's issued_at/valid_until (PLY-092), and the attempt budget's
	// own window (SEC-033). NewServer REQUIRES it; see NewServer's own doc for
	// why there is no wall-clock default.
	nowMs func() int64

	// pairAttempts is SEC-033's attempt budget over POST /player/v1/pair,
	// keyed by the attempt's source address (apihttp.RequestSource). It is
	// internal/relay/reenroll.RateLimiter — a per-key counting window driven by
	// an injected clock — reused rather than reimplemented, the same primitive
	// internal/app/auth.GrantAttemptBudget wraps for the app-side redemption
	// endpoints. Its own state is guarded internally, so it is not under mu.
	pairAttempts *reenroll.RateLimiter

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

	// programs is the served screen-program PER SCREEN (REL-061's own
	// `screen_programs` array), keyed by the screen identity row's id
	// (data-model/1 DAT-004a) — the same id a channel token resolves to, so
	// handleProgram selects a Lease by the credential the player actually
	// presented. programGens fences each screen's writes independently
	// (SetProgram's own doc): a slow resolver for one screen must not be able
	// to refuse another screen's legitimate write.
	programs    map[string]program         // screen_id -> SetProgram's own configured state for that screen
	programGens map[string]int64           // screen_id -> desired-state generation that screen's served program was applied for (REL-052/056)
	signingKey  ed25519.PrivateKey         // relay's own key, signs every issued Lease (PLY-090) — one per relay, never per screen, installed by SetSigningKey independently of any program
	leaseAcks   map[string]LeaseAckRequest // lease_id -> most recent LeaseAck (PLY-091)
	// ackOrder is leaseAcks' arrival order, so the map can be trimmed oldest-first.
	// A map alone has no order to evict by, and "delete an arbitrary one" would drop
	// the ack a caller was about to read as readily as the oldest.
	ackOrder []string

	// issuedLeases is which screen each recently-issued lease_id was handed to,
	// so the acknowledgement and telemetry routes can distinguish a player's own
	// lease from a sibling screen's — the cross-screen presentation PLY-070
	// forbids — and an invented id from a real one (LEASE_UNKNOWN, PLY-114).
	//
	// Keyed BY SCREEN and bounded per screen rather than keyed by lease_id, and
	// that is the whole reason for the shape: every program pull mints a fresh
	// lease (PLY-097), so a lease_id-keyed map grows without bound for as long as
	// a relay runs, and a relay is the component with no operator watching its
	// memory. Bounded by screen count instead, which desired state bounds.
	//
	// It keeps a few per screen rather than only the newest because PLY-114's own
	// wording is "currently or most-recently active": a player that pulled again
	// before acknowledging the previous Lease is behaving conformantly, and
	// refusing that ack would break a legitimate flow to enforce a rule about
	// invented ids. Oldest-first eviction, so the id under pressure is always the
	// one least likely to still be in play.
	issuedLeases map[string][]string // screen_id -> recently issued lease_ids, oldest first

	// revokedScreens is the relay's own last-synced view of the screen_ids in
	// relay/1 REL-066's revocation_and_site.revoked (PLY-072): a channel token
	// naming a screen_id present here is rejected CHANNEL_TOKEN_REVOKED, and no
	// channel token is ever MINTED for one (redeem, REL-123). Both checks read
	// this local copy even while disconnected from the app peer, exactly as
	// REL-123 requires.
	//
	// revokedGen fences a strictly-older generation's late write
	// (SetRevokedScreens), mirroring grantsGen and programGens above.
	revokedScreens map[string]bool
	revokedGen     int64

	// pendingReports is the REL-124/REL-124a ledger of redemptions this relay
	// performed and has not yet reported upstream, used ONLY when no durable
	// session store is wired (sessionStore == nil). With one wired, the
	// durable ledger is the single source of truth — a report owed at "the
	// next connection opportunity" has to survive a restart that happens
	// before one arrives (REL-142a), which an in-process slice cannot do.
	//
	// nextReportSeq is a strictly monotonic counter, NOT len(pendingReports):
	// a length-derived seq is reused as soon as an earlier report is
	// acknowledged and removed, and MarkRedemptionReported would then retire
	// two distinct owed reports on one acknowledgement — the silent loss
	// REL-124d forbids.
	pendingReports []RedemptionReport
	nextReportSeq  int64

	// Playback telemetry a player posts (PLY-110/111): recorded in order of
	// arrival. Wave-1 records them in memory only — REL-090/093's durable
	// upstream forward of each as an events/1 content.played (PLY-113) is a
	// later task's scope; this record is what lets the interrupt-now swap's
	// own render reports be observed end to end today.
	renderStarts []RenderStartRequest
	renderEnds   []RenderEndRequest

	// slideLive is the live data a native slide's server-resolved widgets are
	// filled from as a Lease is issued (internal/slidelive) — installed once by
	// the deployment via SetSlideLive, never per screen, exactly like the
	// signing key above and for the same reason: it is a property of the RELAY
	// (its site coordinates, its device-plane view), identical for every screen
	// it serves.
	//
	// Its zero value is fully valid and is what every test and conformance
	// construction leaves it at: a Sources with no weather and no entity source
	// resolves every live widget to its unavailable placeholder, so a slide
	// still draws and no construction has to know this field exists.
	slideLive slidelive.Sources

	// liveness is the per-screen observation record screenstatus.go maintains:
	// when each screen last pulled a program, last acknowledged a Lease, and last
	// reported a render start, plus what it was last handed. Keyed by screen_id
	// and bounded by the screen count, like programs above — see
	// screenstatus.go's own header for what it is for and what it deliberately
	// does not claim.
	liveness map[string]screenLiveness
}

// SetSlideLive installs the live data source a native slide's `weather` and
// `entity` layers are resolved against as this relay issues Leases
// (internal/slidelive — see that package's doc for why resolution happens at
// issuance rather than in either upstream projection).
//
// A deployment calls it at boot and AGAIN on every site adoption, because the
// coordinates it carries come from the app peer's authoritative site_binding
// (REL-036) and a relay that booted into offline-serve (REL-055/061) has not
// been told them yet. Re-calling is the supported way to correct that: this is
// a wholesale install under the lock, so the newest call wins and the next
// Lease resolves against it. Until a site with real coordinates is adopted —
// and forever, on a relay with neither source configured — every live widget
// shows slidelive.Unavailable, which is the correct thing for a relay that
// genuinely cannot answer (slidelive.Sources.HasGeo: coordinates it does not
// have are not a location it may guess at).
func (s *Server) SetSlideLive(src slidelive.Sources) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slideLive = src
}

// WallClockMs reads the host wall clock in epoch milliseconds.
//
// It is exported for the callers that genuinely want the HOST's reading: tests,
// conformance harnesses, and the virtual player — none of which have a relay
// clock-trust runtime to read. Naming it makes that a deliberate choice at the
// call site rather than an unremarkable inline closure nobody reads twice.
//
// A DEPLOYMENT does not pass this. cmd/waiveo-relay passes its floor-aware
// reading — the latest of the host clock, the persisted clock floor (REL-130)
// and the hint-adjusted runtime clock (REL-133) — so a rolled-back host clock
// cannot walk this server's notion of now behind time the relay has already
// verified.
func WallClockMs() int64 { return time.Now().UnixMilli() }

// NewServer builds a pairing Server that redeems against grants (the
// relay's own applied pairing_grants, REL-067) and presents relayCertPEM as
// this relay's sole trust_anchors entry (PLY-042) on every redemption —
// the same certificate relayCertPEM's own commitment
// (tlsboot.CommitmentForCertDER) is computed over at pairing-code formation
// time (FormPairingCode), so a player's local PLY-052 comparison is always
// checking the cert this server actually hands back.
//
// nowMs is the clock every time-dependent decision this server makes reads: a
// grant's ttl at redemption, a channel token's issuance and expiry, an issued
// Lease's own window, and the SEC-033 attempt budget's window. It is REQUIRED
// and positional, matching internal/app/store.Open's, and for the same reason:
// this package used to call time.Now directly with no seam at all, so a grant's
// ttl was enforced against the bare host clock while the relay maintained a
// persisted, advance-only clock floor (REL-130) precisely so that a rolled-back
// host clock could not re-open a time window that had closed. A deliberately
// NOT-optional argument with no wall-clock default: a default that silently
// reads the host clock is the defect the argument exists to remove, and a seam
// nobody is forced to fill is a seam that stays unfilled. A caller that
// genuinely wants the host's reading passes the named WallClockMs and says so.
//
// It is read per decision, never captured once: a clock floor that advances
// mid-process must move the next check with it.
func NewServer(relayCertPEM []byte, grants []wire.PairingGrant, nowMs func() int64) (*Server, error) {
	if nowMs == nil {
		return nil, fmt.Errorf("playerserver: NewServer: nowMs must not be nil — a server that can be built without naming its clock will eventually be built without one; pass WallClockMs to choose the host's reading deliberately")
	}
	block, _ := pem.Decode(relayCertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("playerserver: NewServer: relayCertPEM did not PEM-decode to a CERTIFICATE block")
	}
	// The relay's own enrolled identity, for REL-121b's binding check (see
	// Server.relayID). A certificate that will not parse cannot be served over
	// either, so this is a construction failure rather than a degrade — a
	// server that silently ran with an empty relayID would refuse every bound
	// grant, which reads on the wire exactly like an invalid pairing code.
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("playerserver: NewServer: parse relayCertPEM: %w", err)
	}

	grantIndex := make(map[string]wire.PairingGrant, len(grants))
	for _, g := range grants {
		grantIndex[g.GrantID] = g
	}

	return &Server{
		relayCertPEM:   relayCertPEM,
		relayID:        leaf.Subject.CommonName,
		nowMs:          nowMs,
		pairAttempts:   reenroll.NewRateLimiter(pairAttemptLimit, pairAttemptWindowMs),
		grants:         grantIndex,
		redeemedGrants: map[string]bool{},
		tokens:         map[string]channelTokenRecord{},
		pollResults:    map[string]redemption{},
		programs:       map[string]program{},
		programGens:    map[string]int64{},
		leaseAcks:      map[string]LeaseAckRequest{},
		issuedLeases:   map[string][]string{},
		revokedScreens: map[string]bool{},
		liveness:       map[string]screenLiveness{},
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
// minted channel token resolves to, and whether it still AUTHORIZES anything —
// the accessor a later /player/v1/program task uses to validate a presented
// Authorization: Bearer channel token (PLY-076, Channel tokens).
//
// A session the relay has dropped (SetRevokedScreens' own session termination)
// reports ok=false here, since it authorizes nothing. The request path does not
// use this accessor for exactly that reason — it needs the dropped session's
// screen_id to tell PLY-072's `CHANNEL_TOKEN_REVOKED` from its
// `CHANNEL_TOKEN_INVALID` — and calls lookupSession instead.
func (s *Server) LookupChannelToken(token string) (screenID string, expiresAt int64, ok bool) {
	rec, known := s.lookupSession(token)
	if !known || rec.Terminated {
		return "", 0, false
	}
	return rec.ScreenID, rec.ExpiresAt, true
}

// lookupSession resolves token to its session record, whether or not that
// session still authorizes anything — the raw read LookupChannelToken and
// handleProgram both sit on. ok reports only that a record EXISTS; a caller
// MUST consult rec.Terminated before treating it as a credential.
func (s *Server) lookupSession(token string) (channelTokenRecord, bool) {
	s.mu.Lock()
	rec, known := s.tokens[token]
	store := s.sessionStore
	s.mu.Unlock()
	if known {
		return rec, true
	}
	if store == nil {
		return channelTokenRecord{}, false
	}

	// A cache miss with persistence enabled does not by itself mean the
	// token is unknown: this process's own s.tokens map starts empty on
	// every restart (NewServer), so it alone cannot distinguish "never
	// issued" from "issued and durably persisted in an EARLIER process
	// lifetime". Consult the durable store, keyed by the token's own hash
	// (identity.HashToken — EnablePersistence's own doc on why a hash
	// rather than the raw token) rather than the unrecoverable raw value.
	//
	// A durably-terminated session is loaded back AS terminated, not dropped
	// on the floor: that is what makes a session the relay dropped before a
	// restart stay dropped after one, which is the whole point of terminating
	// it durably rather than only in this process's map.
	durable, found, err := store.PlayerSession(identity.HashToken(token))
	if err != nil || !found {
		return channelTokenRecord{}, false
	}
	rec = channelTokenRecord{ScreenID: durable.ScreenID, ExpiresAt: durable.ExpiresAt, Terminated: durable.Terminated()}

	// Backfill the in-memory cache so this SAME token resolves from memory
	// on a screen's every later poll this process lifetime, without
	// repeating the durable lookup on every single one.
	s.mu.Lock()
	// Re-checked under the lock, against the revocation view as it stands NOW.
	// The durable read above ran with s.mu released, so a SetRevokedScreens
	// could have completed in between: its in-memory sweep would not have found
	// this token (it was not cached yet) and its durable UPDATE may have landed
	// after this read returned, so a naive backfill would install a LIVE cache
	// entry for a session the relay had just dropped — outliving the revocation
	// itself, since the entry is only consulted once the screen is un-revoked.
	//
	// A currently-revoked screen has no live session by construction: installing
	// the set drops the ones that exist, and redeem mints none while it stands
	// (REL-123). So "revoked now" implies "this session is dropped", and that is
	// the invariant restated here rather than a heuristic.
	if s.revokedScreens[rec.ScreenID] {
		rec.Terminated = true
	}
	s.tokens[token] = rec
	s.mu.Unlock()
	return rec, true
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

// SetRevokedScreens replaces s's revocation view wholesale with screenIDs —
// relay/1 REL-066's revocation_and_site.revoked as the verified snapshot at
// generation carried it (internal/relay/desiredstate.Applied.Revoked). Every
// channel token naming a listed screen_id is thereafter rejected
// CHANNEL_TOKEN_REVOKED, driving that player to re-enter Pairing redemption
// (PLY-073) rather than to a futile renewal, and no channel token is minted for
// one at all (redeem, REL-123). Safe for concurrent use.
//
// It returns an error only when the DURABLE half of the session drop below
// failed. The in-memory revocation view is installed either way and before
// anything else can fail — enforcement is never withheld on account of a
// storage fault — so a caller that ignores the error still enforces the
// revocation for this process's lifetime; what it loses is the guarantee that
// the dropped sessions stay dropped across a restart, and it loses that only
// until the next snapshot restates the set (see the retry rule below). Callers
// log it (cmd/waiveo-relay's own apply seam).
//
// # Dropping the sessions a revocation voids
//
// Installing a set does not merely gate future decisions: every screen the set
// names that the PRIOR view did not is dropped from this server's live and
// durable session state (identity.Store.TerminatePlayerSessionsForScreen).
// Without that, a revocation is undone by its own withdrawal in the one way it
// must not be — a credential minted BEFORE the revocation authorizes again the
// moment the app peer restates the set without that screen, never re-issued and
// never re-paired. Withdrawal restores the ability to PAIR (the one-time grant
// was refused, not consumed — see redeem); it must not resurrect an
// already-minted token.
//
// The drop is a tombstone, not a delete, so a dropped session's token still
// resolves to its screen_id and PLY-072's `CHANNEL_TOKEN_REVOKED` remains
// answerable for as long as the screen is revoked (channelTokenRecord's own
// doc). Only after the revocation is withdrawn does that token fall through to
// `CHANNEL_TOKEN_INVALID` — which PLY-136 makes a player clear its token and
// re-pair on, the same terminal path.
//
// EVERY screen the set names is dropped on EVERY install, not only the ones the
// prior view did not already hold. The difference is what happens after the
// durable half fails. A drop is two halves — this process's token map and the
// store — and only the second can fail; installing a screen into the view is
// what a diff would then read as "already handled", so a set restated on every
// later snapshot (REL-066) would compute nothing newly revoked and never retry
// the write that failed. The screen would stay revoked in memory and durably
// LIVE, which is precisely the state a restart resolves the wrong way: the
// pre-revocation token resolves from the store, un-terminated, and serves. So
// the drop is written to be REPEATABLE and cheap instead of conditional and
// unrepairable — a failure is simply retried by the next apply.
//
// Nothing is re-run by that repetition. A repeated install is free of
// apply-time side effects (REL-070, see the generation fence below) because
// both halves of the drop skip what is already dropped — the sweep by
// `rec.Terminated`, the durable half by `WHERE terminated_at = 0`, which also
// pins the termination stamp to the FIRST drop. Idempotence lives at the row,
// where it can be enforced regardless of what this process believes, rather
// than in a caller-side diff that has to be right about history to be safe.
//
// The cost of repeating it is one SQL statement per apply — not one per revoked
// screen — and in the steady state that statement matches no rows and commits
// without an fsync (dropSessionsForLocked, and
// identity.Store.TerminatePlayerSessionsForScreens' own doc).
//
// # Why a set-replace and not a one-at-a-time mark
//
// `revoked` is not an event stream of revocations; it is a SET the app peer
// restates in full on every snapshot (REL-060: the section key is present on
// every one, empty when the site revokes nothing). So the whole set, including
// what it OMITS, is the message. A screen dropped from a newer generation's
// list is thereby un-revoked, and a mutator that only ever added would leave
// the relay enforcing a revocation its app peer had withdrawn — with no verb
// able to withdraw it, since REL-066 defines no negative entry. Replacing is
// the only shape that can express both directions of a set the relay does not
// author.
//
// This is the deliberate OPPOSITE of the redeemedGrants rule SetPairingGrants
// documents, and the contrast is the point: a consumed one-time grant is a
// record of something the relay ITSELF irreversibly did (REL-121's count never
// exceeds one), so no later generation may revive it. A revocation is a
// statement the APP PEER owns and may restate differently, so the relay's copy
// tracks it in both directions and holds no memory of its own.
//
// # The generation fence
//
// A strictly-older generation's write is dropped (REL-052/056), exactly as
// SetPairingGrants and SetProgram fence theirs. It has to be, and MORE than for
// those: a late write from a superseded generation would not merely serve a
// stale program, it would silently REINSTATE a revocation the current
// generation withdrew, or WITHDRAW one the current generation added — either
// direction a credential decision, and the second one a security regression
// that no subsequent snapshot would correct until the app peer happened to
// change the set again.
//
// A same-generation write is admitted — only a strictly older one is dropped —
// because this fence's job is ORDERING, not idempotence. REL-070's rule is the
// opposite of an idempotence licence: a relay applying a snapshot whose hash
// equals its persisted last-applied hash "MUST NOT re-run any apply-time side
// effect", and that holds "regardless of whether generation itself advanced".
//
// Nothing here relies on that rule being caught upstream. cmd/waiveo-relay's
// rePuller.tick does return before any apply for a generation not strictly
// greater than the last-applied one, which covers the same-generation repeat —
// but it fences on the GENERATION, and REL-070's condition is the HASH, so an
// advancing generation carrying byte-identical sections reaches an apply. This
// method is therefore written to have no apply-time side effect to re-run at
// all when the set it is handed is one it already holds: the view is REPLACED
// with an equal set (not merged, so no accumulation), and the session drop runs
// only for screens the prior view did not already name. A repeated install
// drops nothing a second time and changes no credential decision, whether it
// arrives under the same generation number or a higher one.
func (s *Server) SetRevokedScreens(generation int64, screenIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation < s.revokedGen {
		return nil // stale generation's late write — never revert a newer generation's revocation view (REL-052/056)
	}
	s.revokedGen = generation
	revoked := make(map[string]bool, len(screenIDs))
	for _, id := range screenIDs {
		if id == "" {
			continue // an empty id names no screen; indexing it would revoke the "" a caller-side bug produces
		}
		revoked[id] = true
	}

	// Enforcement is installed before any session work, so a failure below never
	// leaves the screen un-revoked. The drop then runs over the WHOLE set rather
	// than the part of it this call newly added — see the doc above on why the
	// diff that would compute here cannot be repaired if it fails.
	s.revokedScreens = revoked

	return s.dropSessionsForLocked(revoked)
}

// dropSessionsForLocked terminates every live channel-token session issued to
// any screen in screens, in this process's own map and in the durable store when
// one is wired. The caller holds s.mu.
//
// The in-memory sweep marks rather than deletes, for channelTokenRecord's own
// documented reason: a dropped session's token has to keep resolving to its
// screen_id or the relay cannot answer PLY-072's `CHANNEL_TOKEN_REVOKED` for
// it. It also keeps the cache honest against the durable row, so a later
// lookup does not read a terminated row and then overwrite it with a live
// cache entry.
//
// Both halves skip what is already dropped — the sweep by `rec.Terminated`, the
// durable half by the store's own `terminated_at = 0` — so this is safe to run
// on every apply, which is exactly what its caller does with it.
//
// The durable half is ONE statement over the whole set
// (identity.Store.TerminatePlayerSessionsForScreens), and that matters here
// rather than only there: this runs under s.mu, which every pairing redemption
// and every program request also take, so the length of the durable half is
// how long the player-facing surface stalls. The per-screen loop this replaced
// held that lock across one fsync'd write for every screen it dropped — a site
// revoking fifty screens at once stalled every screen it did NOT revoke for the
// duration of fifty serialized fsyncs. One statement makes that one fsync
// regardless of the size of the set, and none at all on the repeats, where the
// set is one the relay has already acted on and the statement matches no rows.
func (s *Server) dropSessionsForLocked(screens map[string]bool) error {
	if len(screens) == 0 {
		return nil
	}
	for token, rec := range s.tokens {
		if rec.Terminated || !screens[rec.ScreenID] {
			continue
		}
		rec.Terminated = true
		s.tokens[token] = rec
	}

	if s.sessionStore == nil {
		return nil
	}
	// The relay's own clock, never the host's: this stamp is a durable record
	// of when the relay acted, and every other time-dependent decision this
	// server makes reads the same floor-aware source (NewServer's nowMs).
	atMs := s.nowMs()
	if atMs == 0 {
		// 0 is the store's not-terminated sentinel, so a clock reading of
		// exactly the epoch would write tombstones that read back as live.
		atMs = 1
	}
	// Sorted so two applies of the same set issue an identical statement rather
	// than one differing only in map-iteration order — the drop now runs on
	// every apply, and a non-deterministic statement makes what the store did
	// (and what any failure of it reports) depend on a hash seed.
	ids := slices.Sorted(maps.Keys(screens))
	if _, err := s.sessionStore.TerminatePlayerSessionsForScreens(ids, atMs); err != nil {
		return fmt.Errorf("playerserver: drop durable sessions for %d revoked screen(s): %w", len(ids), err)
	}
	return nil
}

// isScreenRevoked reports whether screenID is present in the relay's own
// last-synced revocation view (PLY-072) — the check handleProgram runs on a
// presented token's screen_id before serving a Lease.
func (s *Server) isScreenRevoked(screenID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isScreenRevokedLocked(screenID)
}

// isScreenRevokedLocked is isScreenRevoked's body for a caller that already
// holds s.mu — redeem, whose whole check-and-mint sequence runs under one
// acquisition so a revocation cannot land between the check and the mint.
func (s *Server) isScreenRevokedLocked(screenID string) bool {
	return s.revokedScreens[screenID]
}

// handlePair implements POST /player/v1/pair (PLY-030–033): decodes a
// PairingRequest, redeems its grant_selector, and responds either with a
// redeemed PairingResponse or a typed PLY-036 error — never a pending
// status that can never resolve, since Wave-1 first-photon's redemption is
// always synchronous.
//
// This route is reachable with no credential at all — a screen that has never
// paired holds none, which is the entire premise of pairing — so what stands in
// for one is the grant_selector itself, and three things bound what that
// admits: a grant_id carrying at least 128 bits of entropy (SEC-032), an atomic
// check-and-consume for a one-time grant (SEC-036, redeem below), and SEC-033's
// attempt budget enforced BEFORE the selector is looked up.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	traceID := apihttp.TraceID(r)

	// SEC-033, enforced BEFORE the body is even read and long before the
	// selector is resolved: "guessing a code that already exists is the attack
	// this bounds", so the budget must refuse without checking the guess. A
	// budget spent only on lookups that reached the grant index would be
	// counting the attacker's successes rather than their attempts.
	//
	// The key is the attempt's SOURCE ALLOCATION (apihttp.RequestSource, which
	// owns why the address for IPv4, its /64 for IPv6, and not a coarser class).
	// A `pairing` purpose is the only purpose this endpoint redeems, so unlike
	// the app's budget there is no purpose component to separate one sweep from
	// another here.
	//
	// The refusal is UNAVAILABLE: player/1's error taxonomy has no rate-limit
	// code of its own, and PLY-007 forbids reaching into api/1's registry for
	// one. UNAVAILABLE is that taxonomy's own "the relay is temporarily unable
	// to serve the request, retry with backoff", which is exactly true and,
	// unlike PAIRING_CODE_INVALID, does not tell an operator holding a
	// perfectly good code to throw it away and fetch another. The HTTP status
	// is the accurate 429.
	if !s.pairAttempts.Allow(apihttp.RequestSource(r), s.nowMs()) {
		// A refused attempt is the only evidence a sweep against this route
		// leaves. The app-side twin emits an audit record for exactly this
		// reason; the relay has no auditor and no durable event stream of its
		// own, so a log line is the honest analogue rather than a lesser
		// version of one.
		//
		// The SOURCE is named and the code is not: a pairing code is a
		// credential, and a log that recorded the value an attacker guessed
		// would put a working code in the journal every time one came close.
		// What an operator needs is that someone is sweeping and from where.
		log.Printf("playerserver: refused a pairing attempt from %s: the per-source attempt budget is exhausted (SEC-033). "+
			"A pairing endpoint is reachable with no credential, so a burst here is what a sweep against this relay looks like",
			apihttp.RequestSource(r))
		apihttp.WriteProblem(w, r, traceID, http.StatusTooManyRequests, "UNAVAILABLE", "Too Many Requests")
		return
	}

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

	// Budgeted on the same terms as /pair, and BEFORE the token is read.
	//
	// A poll_token is guessable and, once asynchronous redemption exists, is a
	// live credential — an unbudgeted route handing one out to whoever enumerates
	// fastest. It is harmless today only because pollResults is never populated,
	// which makes this the cheapest possible moment to add the bound: after
	// async redemption lands, the same line is a security fix under time
	// pressure rather than a precaution.
	//
	// It shares /pair's budget deliberately. The two are one attack surface —
	// guess a code, or guess the token a redemption would have produced — and a
	// separate allowance would let an attacker refused at one route continue at
	// the other.
	if !s.pairAttempts.Allow(apihttp.RequestSource(r), s.nowMs()) {
		log.Printf("playerserver: refused a pairing-status poll from %s: the per-source attempt budget is exhausted (SEC-033)",
			apihttp.RequestSource(r))
		apihttp.WriteProblem(w, r, apihttp.TraceID(r), http.StatusTooManyRequests, "UNAVAILABLE", "Too Many Requests")
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
//
// That atomicity is per-RELAY, and on its own it is not the site-wide
// at-most-once REL-121 requires: `pairing_grants` is a section of the ONE
// signed snapshot every relay of the site applies, and REL-122 makes a grant
// redeemable for its whole ttl with no app peer reachable, so nothing here can
// ask anyone else whether a sibling relay already consumed the grant. The
// REL-121b binding check below is what closes that: a grant naming another
// relay is refused outright, so at most one relay can ever reach the
// check-and-mark at all and its own count IS the site's.
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
	// REL-121b, checked BEFORE ttl and before the consumption check, and
	// answering with the identical error an unresolvable selector draws: a
	// relay that may not consume this grant must not report anything about it
	// — not that it exists, not that it expired, not that it was already
	// taken. Distinguishing those would make every relay of a site an oracle
	// for the grants held at its siblings. Nothing below this line runs for a
	// grant bound elsewhere, so no credential is minted and no consumption is
	// recorded either.
	if grant.RelayID != "" && grant.RelayID != s.relayID {
		return redemption{}, errPairingCodeInvalid
	}
	if grant.RedemptionMode == "one-time" && s.grantAlreadyRedeemedLocked(grant.GrantID) {
		return redemption{}, errPairingCodeInvalid
	}

	// The grant's ttl is enforced against the SERVER'S clock (s.nowMs), never
	// the bare host clock. On a relay that is the floor-aware reading — the
	// latest of the host clock, the persisted advance-only clock floor
	// (REL-130) and the hint-adjusted runtime clock (REL-133) — so a host clock
	// rolled back below a time the relay has already verified cannot re-open a
	// grant whose ttl has elapsed. Reading time.Now here made REL-130's whole
	// point ("on restart it MUST NOT adopt a wall-clock reading earlier than
	// this persisted floor") true of everything except the one check on this
	// path that a time window actually gates.
	nowMs := s.nowMs()
	if nowMs > grant.IssuedAt+grant.TTL*1000 {
		return redemption{}, errPairingExpired
	}

	// REL-123: `revoked` is enforced against EVERY channel-token issuance, not
	// only against a token later presented. Without this, a revoked screen
	// pairs successfully, is handed a fresh credential, and is refused only at
	// its first program pull — the relay minting a credential it has already
	// been told is void. The screen a redemption credentials is the grant's own
	// (REL-121a), so the revocation check has a screen_id to run against before
	// anything is minted. PLY-075's "never issue a token for an unrecognized
	// screen_id" lands here too, in the one form a relay can actually evaluate:
	// the app peer's own statement that this screen is no longer to be
	// credentialed. (Absence from `screen_programs` is NOT that statement — the
	// feeder legitimately omits an entry for a screen whose effective timezone
	// will not resolve, data-model/1 DAT-034, so a relay inferring revocation
	// from absence would terminally revoke a live screen over a placement
	// mistake.)
	//
	// Placed AFTER the ttl check and BEFORE consumption, and both halves of
	// that placement are load-bearing:
	//
	//   - AFTER the ttl check, because the two draw DIFFERENT codes
	//     (PAIRING_CODE_INVALID vs PAIRING_EXPIRED) and an earlier revocation
	//     check made that difference an oracle: for one and the same expired
	//     grant, a not-revoked screen drew PAIRING_EXPIRED and a revoked one
	//     drew PAIRING_CODE_INVALID, so anyone holding a pairing code learned
	//     the screen's revocation state simply by waiting for expiry. Checking
	//     the ttl first collapses that: revoked-and-expired now draws
	//     PAIRING_EXPIRED like any other expired grant, and revoked-inside-ttl
	//     draws PAIRING_CODE_INVALID like an unresolvable selector. Neither
	//     answer distinguishes a revoked screen from an unrevoked one.
	//   - BEFORE consumption, so nothing below this line runs and a one-time
	//     grant is NOT consumed. Consuming it would make the revocation
	//     destructive: an app peer that removes the screen from `revoked` again
	//     (SetRevokedScreens' own both-directions rule) would find the grant
	//     already spent by an attempt that was refused and minted nothing.
	if grant.ScreenID != "" && s.isScreenRevokedLocked(grant.ScreenID) {
		// The wire answer is deliberately indistinguishable; the operator's is
		// not. With no authoring surface for revocation anywhere yet, a field
		// tech at the screen sees a bare "Pairing Code Invalid" and nothing in
		// the system says why — so the reason is recorded HERE, server-side,
		// where the relay's own log is the only place it can be read. This does
		// not touch the response.
		//
		// Unbounded log growth from an unauthenticated endpoint is already
		// bounded upstream: handlePair spends the SEC-033 attempt budget (10
		// per source per 15 minutes) before redeem is ever called.
		log.Printf("playerserver: refused pairing redemption for screen %s: the relay's last-synced revocation view (generation %d) names it revoked, so no channel token may be issued for it (REL-066/REL-123); answering PAIRING_CODE_INVALID, the same code an unresolvable selector draws",
			grant.ScreenID, s.revokedGen)
		return redemption{}, errPairingCodeInvalid
	}

	// The consumption mark (REL-121c, one-time only) and the owed upstream
	// report (REL-124, EVERY redemption) are recorded TOGETHER, before anything
	// is minted, and the in-memory mark follows only once that succeeded.
	//
	// They used to be two writes with the consumption first, and a failure in
	// the second BURNED THE GRANT: recorded as spent, no token minted, 500
	// returned, and no retry able to recover because the grant the player needed
	// was consumed by an attempt that gave it nothing.
	//
	// Reordering alone does not fix it — recording the report first turns the
	// same failure into a report of a redemption that never happened, and the
	// report ledger has no idempotency key, so the retry enqueues a second one.
	// One transaction is the only ordering with no losing side.
	oneTime := grant.RedemptionMode == "one-time"
	if err := s.recordRedemptionLocked(grant.GrantID, nowMs, oneTime); err != nil {
		return redemption{}, err
	}
	// In-memory last, because it cannot fail. Setting it earlier is what made a
	// store failure consume the grant for the life of the process even when
	// nothing durable had been written.
	if oneTime {
		s.redeemedGrants[grant.GrantID] = true
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
		IssuedAt:     nowMs,
		ExpiresAt:    nowMs + channelTokenTTL.Milliseconds(),
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

// RedemptionReport is one pairing-grant redemption this relay performed and
// owes its app peer (REL-124), as the connection's own `pairing.redeemed` frame
// carries it (REL-124a's `{grant_id, redeemed_at}`) plus the ledger position
// that identifies WHICH owed redemption it is. Seq is opaque to a caller except
// that MarkRedemptionReported clears the ledger by it — a value that must be
// carried back, never re-derived from grant_id, so a `multi` grant's other
// outstanding redemptions are not retired by one delivered report.
type RedemptionReport struct {
	Seq        int64
	GrantID    string
	RedeemedAt int64
}

// recordRedemptionOwedLocked enqueues one redemption for upstream report. The
// durable ledger is the sole source of truth whenever one is wired
// (EnablePersistence): a report owed at "the next connection opportunity"
// (REL-124a) has to survive a restart that happens before one arrives, which an
// in-process slice cannot do. Without persistence the in-memory slice keeps
// today's non-durable behavior for tests and harnesses byte-for-byte. The
// caller holds s.mu.
func (s *Server) recordRedemptionLocked(grantID string, redeemedAtMs int64, oneTime bool) error {
	if s.sessionStore == nil {
		// No durable store: the in-memory queue is the whole record, and an
		// append cannot fail, so there is no partial state to guard against.
		s.nextReportSeq++
		s.pendingReports = append(s.pendingReports, RedemptionReport{
			Seq:        s.nextReportSeq,
			GrantID:    grantID,
			RedeemedAt: redeemedAtMs,
		})
		return nil
	}
	if err := s.sessionStore.RecordRedemption(grantID, redeemedAtMs, oneTime); err != nil {
		return fmt.Errorf("%w: %v", errSessionPersistFailed, err)
	}
	return nil
}

// PendingRedemptionReports returns every redemption this relay owes its app
// peer, oldest first (REL-124/REL-124a) — what the connection owner drains onto
// the wire on connect and while connected. Safe for concurrent use.
func (s *Server) PendingRedemptionReports() ([]RedemptionReport, error) {
	s.mu.Lock()
	store := s.sessionStore
	if store == nil {
		out := make([]RedemptionReport, len(s.pendingReports))
		copy(out, s.pendingReports)
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()

	rows, err := store.PendingRedemptionReports()
	if err != nil {
		return nil, err
	}
	out := make([]RedemptionReport, 0, len(rows))
	for _, r := range rows {
		out = append(out, RedemptionReport{Seq: r.Seq, GrantID: r.GrantID, RedeemedAt: r.RedeemedAt})
	}
	return out, nil
}

// MarkRedemptionReported retires the owed redemption seq names, once its
// `pairing.redeemed` frame has actually been written to the connection. A
// caller MUST NOT call it before the write succeeds: REL-124a requires an
// unreported redemption be re-sent at the next connection opportunity, and
// retiring one that never crossed the wire is precisely the loss that rule
// forbids.
func (s *Server) MarkRedemptionReported(seq int64) error {
	s.mu.Lock()
	store := s.sessionStore
	if store == nil {
		kept := s.pendingReports[:0]
		for _, r := range s.pendingReports {
			if r.Seq != seq {
				kept = append(kept, r)
			}
		}
		s.pendingReports = kept
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	return store.MarkRedemptionReported(seq)
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
