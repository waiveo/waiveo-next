// This file implements player/1's Program delivery (`GET /player/v1/program`,
// PLY-080/081/090/096) and Leases' own acknowledgement
// (`POST /player/v1/lease/ack`, PLY-091) — the last relay-side piece
// before the conformance drivers and virtual player (Wave-1 first-photon
// Task 10).
//
// A paired player holding a channel token (pairing.go's own handlePair)
// pulls its program here and receives a signed Lease carrying the one
// image content item and its DIRECT feeder content-origin URL — this
// handler never fetches, caches, or serves the asset bytes themselves
// (PLY-084, `relay/1` REL-140, `#52`): it only ever hands a URL back to the
// player, exactly as SetProgram received it from the verified desired-state
// snapshot (internal/relay/desiredstate).
package playerserver

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// leaseValidity bounds how long a freshly issued Lease remains valid
// (PLY-092's `valid_until`). Wave-1 first-photon has no daypart/schedule
// engine to derive a tighter, content-aware bound from, so this is a fixed
// window comfortably wider than a player's ordinary poll cadence (PLY-082's
// proposed ~10s) — a player re-leases well before this ever lapses.
const leaseValidity = 5 * time.Minute

// ProgramPullRequest is player/1's ProgramPull request body (PLY-080,
// PLY-012). The contract's own worked example shows a JSON body riding a
// GET (Wire shapes' `ProgramPull request`); this package follows that
// shape literally rather than inventing a query-string encoding of its
// own. `Generation` is read (a player's currently held `program_revision`)
// but not yet acted on — see handleProgram's own doc for Wave-1
// first-photon's `program.unchanged` simplification.
type ProgramPullRequest struct {
	Capabilities Capabilities `json:"capabilities"`
	Generation   string       `json:"generation,omitempty"`
}

// LeaseResponse is player/1's Lease (PLY-090): wire.Lease's own fields, in
// their declared order, plus Signature appended last — producing exactly
// PLY-090's shape `{lease_id, screen_id, program_revision, priority,
// display, content, issued_at, valid_until, signature}` on the wire, since
// encoding/json inlines an embedded struct's fields at the embedding
// field's own position.
type LeaseResponse struct {
	wire.Lease
	Signature string `json:"signature"`
}

// LeaseAckRequest is player/1's LeaseAck body (PLY-091):
// `{lease_id, accepted, reason?}`.
type LeaseAckRequest struct {
	LeaseID  string `json:"lease_id"`
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

// RenderStartRequest is player/1's RenderStart body (PLY-110):
// `{lease_id, asset_ref, ts}` — a player reports the moment it begins
// presenting a content item on screen.
type RenderStartRequest struct {
	LeaseID  string `json:"lease_id"`
	AssetRef string `json:"asset_ref"`
	TS       int64  `json:"ts"`
}

// RenderEndRequest is player/1's RenderEnd body (PLY-111): exactly events/1's
// content.played payload shape (EVT-050), field for field —
// `{screen_id, asset_ref, program_revision, t_start, t_end, cause,
// completion, power_evidence?}`. `cause` and `completion` are PLY-112's
// enumerations. power_evidence is optional and out of this task's scope.
type RenderEndRequest struct {
	ScreenID        string `json:"screen_id"`
	AssetRef        string `json:"asset_ref"`
	ProgramRevision string `json:"program_revision"`
	TStart          int64  `json:"t_start"`
	TEnd            int64  `json:"t_end"`
	Cause           string `json:"cause"`
	Completion      string `json:"completion"`
}

// program is ONE screen's served screen-program (`relay/1` REL-061), carried
// unmodified from the verified desired-state snapshot
// (internal/relay/desiredstate.Applied) — SetProgram is main.go's own
// hand-off point from that verified value into this server.
//
// A Server holds one of these PER SCREEN, keyed by the screen_id a channel
// token resolves to (Server.programs). It is deliberately not one value for
// the whole server: `screen_programs` is an ARRAY with one entry per screen
// identity row (REL-061), and a player presents a channel token that names
// exactly which of them it is (PLY-035/076), so a single shared value would
// serve every paired player whichever screen's entry happened to be written
// last — including one screen's content to a different screen.
type program struct {
	ProgramRevision string
	Priority        string
	Display         string
	Content         []wire.LeaseContent
}

// TerminalProgramRevision is the stable `program_revision` a screen parked at
// data-model/1's terminal default carries (DAT-118). It is a fixed sentinel
// rather than a derived value so a screen sitting at the terminal blank never
// spuriously re-swaps: a player only re-fetches content when the revision
// changes, and a revision that varied would churn a screen that is showing
// nothing.
//
// It is exported because internal/relay/schedulehost's own DAT-118 projection
// must carry the identical value — a schedule that resolves to the terminal
// default and a screen with no `screen_programs` entry at all reach the SAME
// contract-defined state, and two sentinels for one state would present as a
// spurious program change to any player that crossed between them.
const TerminalProgramRevision = "terminal:blank"

// terminalDefault is the program a screen this server holds no entry for is
// served: data-model/1's terminal default (DAT-118) — `display: blank` with no
// content, "powered on, showing nothing, distinct from off".
//
// DAT-118 is explicit that resolution "never leaves N's state unresolved and
// never falls back to any box-local or player-local content"; a relay asked
// for a screen it has no program for is in exactly that position, and this is
// the empty-but-DEFINED state the contract names for it. What it must NOT do
// is hand back some other screen's program, which is what a single
// whole-server program value did for every screen but the last one written.
//
// `priority` is `scheduled` because PLY-108's other value, `preempt`, is a
// deliberately-invoked emergency takeover — the absence of any program for a
// screen is the opposite of one, and a blank Lease claiming `preempt` would
// make an unauthored screen outrank an authored preemption on any consumer
// that compares them.
func terminalDefault() program {
	return program{
		ProgramRevision: TerminalProgramRevision,
		Priority:        "scheduled",
		Display:         DisplayBlank,
	}
}

// SetProgram configures the program-delivery state GET /player/v1/program
// serves FOR ONE SCREEN: programRevision/priority/display/content carried
// UNMODIFIED onto every Lease this server issues to the player holding a
// channel token for screenID (PLY-108 priority, PLY-109 display — REL-061's
// entry for that screen reflected exactly), signed with signingKey.
//
// screenID is the screen identity row's id (data-model/1 DAT-004a) — the same
// value a `screen_programs` entry names (REL-061), a screen-bound pairing
// grant carries (REL-121a), and a redeemed channel token therefore resolves to
// (PLY-035, LookupChannelToken). It is NOT a scope node's id: DAT-004a is
// explicit that "a `screen`-kind scope node is a placement classification —
// never a screen identity in its own right", so a caller resolving scheduling
// state AT a scope node must translate to the screen row it serves before
// calling this. Installing a program under a scope-node id would key it where
// no channel token can ever reach it.
//
// An empty screenID is ignored outright rather than installed under "": no
// channel token ever resolves to an empty screen_id (redeem always mints or
// carries a non-empty one), so such an entry could only ever be dead state.
//
// signingKey MUST be the relay's own enrollment private key
// (internal/relay/identity.RelayIdentity.PrivateKey) — the SAME keypair
// the certificate passed to NewServer (relayCertPEM) certifies, so a
// player's Steady-state-pinning verification of a Lease's signature
// (PLY-090, "verifiable... against the same trust anchor its... connection
// to this relay is itself pinned to") checks against the exact cert this
// relay's player/1 listener presents. It is held once for the whole server
// rather than per screen ON PURPOSE: it is the relay's own identity, identical
// for every screen, and a re-enrollment that rotates it (REL-027–029) must take
// effect for EVERY screen's Leases at once — a per-screen copy would keep
// signing one screen's Leases with a retired key until that screen's program
// happened to be rewritten, and a player pinning the currently-presented cert
// would reject them.
//
// generation is the desired-state generation this program was resolved for
// (relay/1 REL-052/056). The live re-pull loop drives SetProgram concurrently
// across generations: when a higher generation is applied, the superseded
// generation's per-screen resolver goroutines are cancelled but one may still be
// mid-flight inside a resolve, so its late write can arrive AFTER the new
// generation's. SetProgram FENCES that hazard: a write whose generation is
// strictly older than the last one applied FOR THAT SCREEN is dropped, so a
// stale resolver can never revert the screen's served program to a superseded
// generation (upholding the "an API edit MUST change the resolved program"
// oracle across the atomic-swap window). A write at the same-or-higher
// generation wins (last-write-wins WITHIN a generation is preserved, so the
// schedule resolver's TickBoot correctly replaces the app-authored baseline
// SetServedProgram configured at the same generation at boot).
//
// The fence is PER SCREEN, and that is load-bearing rather than incidental: a
// single whole-server fence lets a screen whose resolver happens to be running
// at a higher generation silently refuse every other screen's writes at the
// generation they were legitimately resolved for. Screens do not supersede one
// another; only a later generation of the SAME screen does.
func (s *Server) SetProgram(generation int64, screenID, programRevision, priority, display string, content []wire.LeaseContent, signingKey ed25519.PrivateKey) {
	if screenID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation < s.programGens[screenID] {
		return // stale generation's late write — never revert a newer generation's served program for THIS screen (REL-052/056).
	}
	s.programGens[screenID] = generation
	s.programs[screenID] = program{
		ProgramRevision: programRevision,
		Priority:        priority,
		Display:         display,
		Content:         content,
	}
	s.signingKey = signingKey
}

// SetServedProgram configures program delivery from a PERSISTED
// screen_programs entry (relay/1 REL-061) — the wire.ScreenProgram
// desiredstate.ServedProgram returns from the relay's durable operational
// store, NOT a live desired-state pull. This is the offline-continuity serve
// path (REL-055/061): a relay whose app peer is disconnected still delivers
// its last-applied program, carrying sp's priority/display/program_revision
// and content pointers UNMODIFIED onto every issued Lease (PLY-108/109) —
// so a preempt/content (or blank) assignment reaches the screen through the
// relay's own offline continuity with no app-peer connection live.
//
// sp's content references (relay/1's own ContentRef, REL-061/061a) each carry
// their OWN `content_type`, annotated onto player/1's required content `type`
// field (PLY-083) as they become Lease content items — a multi-item `content`
// array (an ordered cast, REL-061) therefore reaches the player with each
// item's real kind, never a blanket constant. An item whose ContentType is ""
// (an older feeder that predates the field, REL-061a) defaults to `image`,
// this codebase's own historical implicit value from before the field
// existed — so a pre-existing single-image snapshot serves an identical Lease
// to before. Each item's `duration_ms` (REL-061a) is likewise carried
// unmodified onto the Lease content item's own `duration_ms` (PLY-083b) via
// wire.LeaseContent's `omitempty` tag — an item with none marshals with no
// `duration_ms` key at all. Each URL is the screen's DIRECT content-origin
// fetch target (never a relay-hosted one, REL-140) — this server never
// touches the bytes.
//
// signingKey MUST be the relay's own enrollment private key, as SetProgram
// documents — the same trust anchor a player pins its Lease-signature check
// against (PLY-090).
//
// The entry is installed for sp.ScreenID alone (REL-061's own `screen_id`),
// so a caller replays the whole persisted `screen_programs` array through this
// method — one call per entry — and each screen is served ITS entry. A screen
// the array carries no entry for is served data-model/1's terminal default
// (DAT-118, terminalDefault), never a sibling screen's program.
//
// generation is the persisted last-applied generation this served program
// belongs to (relay/1 REL-052/056), carried into SetProgram's own generation
// fence: it is the boot-time baseline a same-generation schedule resolver then
// replaces, and a strictly-older stale write can never revert.
func (s *Server) SetServedProgram(generation int64, sp wire.ScreenProgram, signingKey ed25519.PrivateKey) {
	content := make([]wire.LeaseContent, 0, len(sp.Content))
	for _, c := range sp.Content {
		contentType := c.ContentType
		if contentType == "" {
			// REL-061a back-compat default: an item carrying no content_type
			// (an older feeder) is treated as `image`, this codebase's own
			// historical implicit value from before the field existed.
			contentType = "image"
		}
		content = append(content, wire.LeaseContent{
			Type:       contentType,
			AssetRef:   c.AssetRef,
			URL:        c.URL,
			ExpiresAt:  c.ExpiresAt,
			DurationMS: c.DurationMS,
		})
	}
	s.SetProgram(generation, sp.ScreenID, sp.ProgramRevision, sp.Priority, sp.Display, content, signingKey)
}

// programFor returns the program this server currently serves screenID —
// SetProgram/SetServedProgram's own installed entry, or data-model/1's
// terminal default (DAT-118, terminalDefault) when this server holds no entry
// for that screen. It is the ONE place the served-program selection happens,
// so handleProgram and CurrentDisplay cannot disagree about what a given
// screen is currently being served. The caller holds s.mu.
func (s *Server) programForLocked(screenID string) program {
	prog, ok := s.programs[screenID]
	if !ok {
		return terminalDefault()
	}
	return prog
}

// CurrentDisplay returns the `display` value (PLY-093: DisplayContent or
// DisplayBlank) of the Lease this server is currently configured to issue TO
// screenID — SetProgram/SetServedProgram's own program.Display for that
// screen, read under the same lock those setters take, or DisplayBlank when
// this server holds no program for it (data-model/1's DAT-118 terminal
// default, which is exactly what a pull for that screen would be served).
//
// This is the real signal internal/relay/keepalive's screen-liveness recovery
// gates PLY-155's blank-Lease suppression on (Config.ActiveDisplay,
// playerserver.LivenessSignal via liveness.go's EvaluateRecovery). PLY-155
// gates on "the TARGET screen's own currently active Lease", so it takes the
// screen id rather than reporting some arbitrary screen's display: an accessor
// that answered for the whole server would suppress recovery on a live screen
// because a DIFFERENT screen is blank, and relaunch an intentionally blanked
// one because a different screen is showing content.
func (s *Server) CurrentDisplay(screenID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.programForLocked(screenID).Display
}

// SoleServedScreen reports the screen_id of the ONE screen this server holds a
// program for, and whether there is exactly one. It is false for zero screens
// and for two or more.
//
// It exists for the one caller that must ask about a screen it cannot name:
// internal/relay/keepalive polls DEVICE ENTITIES and needs the display of the
// screen each entity is attached to (PLY-155), but nothing in relay/1's
// desired state binds an adopted entity to a screen identity row —
// `device_inventory` (REL-063) carries `{device_id, driver, native_id,
// poll_cadence_seconds, entities}` and no screen reference, and the relay's ECP
// targets are keyed by entity id alone. Where this server serves exactly one
// screen the binding is not needed to answer the question — every polled entity
// belongs to that screen, because there is no other — and this reports it.
// Where it serves several, the caller has no way to attribute an entity to one
// of them and MUST NOT guess: answering with some other screen's display is the
// defect a per-screen CurrentDisplay exists to remove, so the caller degrades
// to keepalive's documented not-blank reading instead.
func (s *Server) SoleServedScreen() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.programs) != 1 {
		return "", false
	}
	for screenID := range s.programs {
		return screenID, true
	}
	return "", false
}

// LeaseAck returns a previously recorded lease/ack for leaseID, and
// whether one has been recorded — exposed for tests and any later task
// wanting to inspect ack state (a real relay/1 upstream forward of
// acceptance is out of this task's scope, PLY-091's own persistence
// obligation noted on handleLeaseAck).
func (s *Server) LeaseAck(leaseID string) (LeaseAckRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.leaseAcks[leaseID]
	return rec, ok
}

// handleProgram implements GET /player/v1/program (PLY-080/081/090/096):
// validates the presented channel token (PLY-070/076), filters the
// configured program's content by the request's declared `content_types`
// (PLY-013/096 — a relay MUST NOT hand back a content item of a type the
// player hasn't declared), and returns a freshly signed Lease.
//
// Wave-1 first-photon simplification: this handler always returns a fresh
// Lease and never PLY-081's `program.unchanged {program_revision}` branch
// — there is exactly one program system-wide in this task's scope and no
// player has any reason to hold a stale one within it. A later task
// implementing real reissuance should add that branch keyed on the
// request's own `generation`.
//
// Auth error taxonomy: an absent, malformed, or unresolvable token maps to
// `CHANNEL_TOKEN_INVALID` — the Error taxonomy's own code for "malformed or
// unknown"; a resolvable token past its own `expires_at` maps to
// `CHANNEL_TOKEN_EXPIRED` (PLY-072), distinct because PLY-073 requires a
// player treat the two differently (renew vs. re-pair).
func (s *Server) handleProgram(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	traceID := apihttp.TraceID(r)

	token := bearerToken(r)
	if token == "" {
		apihttp.WriteProblem(w, r, traceID, http.StatusUnauthorized, "CHANNEL_TOKEN_INVALID", "Channel Token Invalid")
		return
	}

	screenID, expiresAt, ok := s.LookupChannelToken(token)
	if !ok {
		apihttp.WriteProblem(w, r, traceID, http.StatusUnauthorized, "CHANNEL_TOKEN_INVALID", "Channel Token Invalid")
		return
	}
	// Revocation (PLY-072) is checked BEFORE expiry: it is terminal (PLY-073's
	// re-pair path), so a token that is both revoked and expired reports
	// CHANNEL_TOKEN_REVOKED — driving the player to re-pair rather than to a
	// renewal (PLY-074) of a credential whose screen no longer validates
	// (PLY-075). The check reads the relay's own last-synced revocation view,
	// valid even while disconnected from the app peer (REL-123).
	if s.isScreenRevoked(screenID) {
		apihttp.WriteProblem(w, r, traceID, http.StatusUnauthorized, "CHANNEL_TOKEN_REVOKED", "Channel Token Revoked")
		return
	}
	if time.Now().UnixMilli() > expiresAt {
		apihttp.WriteProblem(w, r, traceID, http.StatusUnauthorized, "CHANNEL_TOKEN_EXPIRED", "Channel Token Expired")
		return
	}

	var req ProgramPullRequest
	if r.Body != nil {
		// A malformed or absent body degrades safely to an empty
		// capabilities declaration: PLY-013/096's content-type gate then
		// excludes every content item, rather than this handler treating a
		// body-parse hiccup as a hard failure on an otherwise-authorized
		// pull.
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	// The Lease is selected by the TOKEN'S OWN screen_id — the screen identity
	// row this credential was minted for (PLY-035, REL-121a) — so a site with
	// several screens serves each paired player its own screen's program. A
	// screen this server holds no entry for is served data-model/1's terminal
	// default (DAT-118), never a sibling screen's program.
	s.mu.Lock()
	prog := s.programForLocked(screenID)
	signingKey := s.signingKey
	s.mu.Unlock()

	// No signing key configured yet — nothing has called SetProgram on this
	// server at all. Every Lease MUST carry a signature a player verifies
	// against its pinned trust anchor (PLY-090), and there is no unsigned Lease
	// in that contract, so this refuses rather than issuing one. It is a 500
	// because the fault is entirely the relay's own configuration: the player's
	// credential is valid and its request is well-formed.
	//
	// Reachable whenever a screen pairs before any program has been installed —
	// a site whose persisted screen_programs is empty (REL-060's empty
	// placeholder) is exactly that, and signhash.Sign on a nil key panics
	// rather than erroring, which on the serving goroutine takes the request's
	// whole connection down.
	if len(signingKey) != ed25519.PrivateKeySize {
		apihttp.WriteProblem(w, r, traceID, http.StatusInternalServerError, "INTERNAL", "Internal Error")
		return
	}

	lease := wire.Lease{
		// PLY-097: lease_id MUST be a ULID (unique per issuance), unlike
		// this codebase's other opaque ids (screen_id, channel_token,
		// grant_id, relay_id) whose own contracts only require
		// opaqueness — newOpaqueToken's `<prefix>-<hex>` shape doesn't
		// satisfy the ULID requirement, so this mint site alone uses the
		// shared ulid package instead.
		LeaseID:         ulid.New(),
		ScreenID:        screenID,
		ProgramRevision: prog.ProgramRevision,
		Priority:        prog.Priority,
		Display:         prog.Display,
		Content:         clampContentDurations(filterContentTypes(prog.Content, req.Capabilities.ContentTypes)),
		IssuedAt:        time.Now().UnixMilli(),
		ValidUntil:      time.Now().Add(leaseValidity).UnixMilli(),
	}

	canon, err := wire.LeaseSignedBytes(lease)
	if err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusInternalServerError, "INTERNAL", "Internal Error")
		return
	}
	signature := wire.EncodeSignature(signhash.Sign(signingKey, canon))

	writeJSON(w, http.StatusOK, LeaseResponse{Lease: lease, Signature: signature})
}

// handleLeaseAck implements POST /player/v1/lease/ack (PLY-091): records a
// player's acknowledgement of a Lease it received, independent of whether
// that Lease's own content is yet fetchable (PLY-088).
//
// PLY-091 also requires a relay persist Lease delivery/acknowledgement
// state in its own durable local storage (mirroring `relay/1` REL-142) so
// an acknowledgement survives a relay's own disconnection from its app
// peer. Wave-1 first-photon records acks in memory only — render
// telemetry's own upstream forwarding is Phase 2 scope, out of this task —
// and this in-memory record is what lets a conformant player complete the
// pairing -> program -> lease/ack flow end to end today.
func (s *Server) handleLeaseAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	traceID := apihttp.TraceID(r)

	var req LeaseAckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}
	if req.LeaseID == "" {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}

	s.mu.Lock()
	s.leaseAcks[req.LeaseID] = req
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRenderStart implements POST /player/v1/render/start (PLY-110):
// records a player's report that it has begun presenting a content item on
// screen. Wave-1 records it in memory only — REL-090/093's durable upstream
// forward is a later task (PLY-113 scope) — so this record is what makes the
// interrupt-now swap's own render/start observable end to end.
func (s *Server) handleRenderStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	traceID := apihttp.TraceID(r)

	var req RenderStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}
	if req.LeaseID == "" || req.AssetRef == "" {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}

	s.mu.Lock()
	s.renderStarts = append(s.renderStarts, req)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRenderEnd implements POST /player/v1/render/end (PLY-111): records a
// player's report that a content item's playback has completed or been
// definitively interrupted, in events/1's content.played payload shape
// (EVT-050). Recorded in memory only for now, as handleRenderStart documents.
func (s *Server) handleRenderEnd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	traceID := apihttp.TraceID(r)

	var req RenderEndRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}
	if req.AssetRef == "" || req.Cause == "" || req.Completion == "" {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}

	s.mu.Lock()
	s.renderEnds = append(s.renderEnds, req)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// RenderStarts returns a copy of the render/start reports recorded so far
// (PLY-110), in arrival order — exposed for tests and any later task
// forwarding them upstream (PLY-113).
func (s *Server) RenderStarts() []RenderStartRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RenderStartRequest(nil), s.renderStarts...)
}

// RenderEnds returns a copy of the render/end reports recorded so far
// (PLY-111), in arrival order — exposed for tests and any later task
// forwarding them upstream as content.played (PLY-113).
func (s *Server) RenderEnds() []RenderEndRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RenderEndRequest(nil), s.renderEnds...)
}

// filterContentTypes returns only the content items whose Type is present
// in declaredTypes (PLY-013/PLY-096): a relay MUST NOT hand back a content
// item of a type the player hasn't most-recently declared support for. An
// empty or nil declaredTypes excludes every item, never included by
// default.
// leaseContentMinDurationMS is the floor this relay enforces on a Lease
// content item's own `duration_ms` (PLY-083b) before it ever reaches a
// player. `duration_ms` rides this codebase's wire.LeaseContent/ContentRef
// as a raw, unvalidated int64 from every producer (schedulehost's
// duration_seconds*1000 projection, an app-authored ContentRef, a persisted
// screen_programs entry) all the way to this handler with no bounds check
// anywhere upstream. A player (PhotonScene.brs renderCastItem) feeds this
// value directly into a SceneGraph Timer's `duration` field; a near-zero or
// negative value re-arms that timer at a CPU-saturating rate (or is
// undefined behavior, for negative) on the player's render thread — the
// exact freeze signature this fleet has been bitten by before. This is
// player/1's own choke point for every content item regardless of which
// producer set duration_ms, so clamping here closes the hazard for all of
// them at once rather than requiring every producer to remember to.
//
// A duration_ms of exactly 0 is left untouched: PLY-083b defines 0 (like
// absent) as "no override — the player supplies its own default", a
// meaningful sentinel this clamp must not disturb. Only a non-zero value
// below the floor (including any negative value, which is not a meaningful
// override at all) is raised to it.
const leaseContentMinDurationMS = 1000

// clampContentDurations floors every non-zero, sub-floor duration_ms in
// content to leaseContentMinDurationMS, in place, and returns it. See
// leaseContentMinDurationMS's own doc for why this exists.
func clampContentDurations(content []wire.LeaseContent) []wire.LeaseContent {
	for i := range content {
		if content[i].DurationMS != 0 && content[i].DurationMS < leaseContentMinDurationMS {
			content[i].DurationMS = leaseContentMinDurationMS
		}
	}
	return content
}

func filterContentTypes(content []wire.LeaseContent, declaredTypes []string) []wire.LeaseContent {
	allowed := make(map[string]bool, len(declaredTypes))
	for _, t := range declaredTypes {
		allowed[t] = true
	}
	out := make([]wire.LeaseContent, 0, len(content))
	for _, c := range content {
		if allowed[c.Type] {
			out = append(out, c)
		}
	}
	return out
}

// bearerToken extracts a channel token from r's Authorization header
// (PLY-076: "Authorization: Bearer <token>"; this contract "defines no
// alternate credential placement"), returning "" if absent or malformed.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}
