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
	"slices"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
	"github.com/maaxton/waiveo-next/internal/slidelive"
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
// Serving it takes a signing key like any other Lease (PLY-090 admits no
// unsigned Lease), and that key is SetSigningKey's — the relay's own identity,
// installed once at construction time by the deployment — precisely so a relay
// holding NO program at all can still answer this. While the key was only ever
// assigned inside SetProgram, a relay that had never installed one answered
// every pull 500 INTERNAL instead, which is the one case this default exists
// for.
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
// entry for that screen reflected exactly), signed with the relay identity
// SetSigningKey installed.
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
// It takes NO signing key, and that is a security property rather than a
// convenience. The key is the RELAY's identity — one value for the whole server,
// the private half of the keypair NewServer's relayCertPEM certifies — while
// this method is a PER-SCREEN write. Letting a per-screen write carry the key
// gave one screen's program update authority over every other screen's Leases:
// a well-formed but WRONG key installed for screen B made screen A's Leases
// verify under the foreign key and not under the relay cert's, so every paired
// player on the site rejected every Lease against its pinned anchor (PLY-090),
// caused by a write that never named them. Establishing the identity is
// SetSigningKey's job alone; see its doc.
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
//
// # The priority fence: a preempt program is not overwritten within its generation
//
// WITHIN one generation, a `scheduled` write never replaces a `preempt` one.
// This is what makes a push-now override (an operator's "show this here now",
// carried as a preempt `screen_programs` entry — snapshot.overrideProgram) hold
// for longer than half a minute.
//
// The hazard is entirely internal to a single generation apply, which is why the
// generation fence above cannot see it. An apply installs the generation's
// app-authored baseline first and then lets the schedule resolvers write over
// it, deliberately, both stamped with the SAME generation (scheduleDriver.apply's
// own ordering note) — and the resolvers then keep re-resolving on their own
// 30-second ticker at that same generation for as long as the generation stands
// (schedulehost.Resolver.Loop). Without this fence, the very first tick after
// every push silently put the schedule back, and the console would have shown a
// successful push against a screen that reverted within thirty seconds.
//
// It is expressed as a priority comparison, not as an "is this an override"
// flag, because PLY-108 already gives the two classes an ordering and this IS
// that ordering: `preempt` is the deliberately-invoked takeover, `scheduled` is
// ordinary resolution, and ordinary resolution does not get to cancel a
// takeover. Every existing producer keeps working unchanged — schedulehost
// writes `scheduled` exclusively, so no schedule resolution is ever fenced by
// another schedule resolution.
//
// A NEWER generation always wins, whatever either priority is, and that is the
// clearing mechanism: an operator clearing the override produces a new
// generation whose baseline for that screen is `scheduled` again, at a
// generation strictly greater than the preempt one, so it installs and the
// resolvers resume. A preempt program can also be replaced by another preempt
// program within its own generation, so a second push to the same screen is not
// blocked by the first.
func (s *Server) SetProgram(generation int64, screenID, programRevision, priority, display string, content []wire.LeaseContent) {
	if screenID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation < s.programGens[screenID] {
		return // stale generation's late write — never revert a newer generation's served program for THIS screen (REL-052/056).
	}
	if generation == s.programGens[screenID] &&
		s.programs[screenID].Priority == PriorityPreempt && priority != PriorityPreempt {
		// The PRIORITY fence, sitting inside the generation fence (see this
		// method's own doc section below).
		return
	}
	s.programGens[screenID] = generation
	s.programs[screenID] = program{
		ProgramRevision: programRevision,
		Priority:        priority,
		Display:         display,
		Content:         content,
	}
}

// SetSigningKey installs key as the relay's own Lease-signing identity: the
// private half of the keypair the certificate passed to NewServer (relayCertPEM)
// certifies, which every Lease this server issues is signed with (PLY-090).
//
// It exists SEPARATELY from SetProgram because the key is a property of the
// RELAY, not of any screen. Assigning it only inside SetProgram meant a relay
// that had never installed a single program had no key either, and answered
// every /player/v1/program pull 500 INTERNAL — including the pull it should have
// answered with data-model/1's terminal default (DAT-118). That state is not
// exotic: a site whose `screen_programs` is REL-060's empty placeholder reaches
// it, and so does one whose entries were all derived away (a screen whose
// resolution fails on an unresolvable effective tz is omitted from the derived
// array, DAT-033/034) while its screen-bound grant and channel token stay
// perfectly valid. A relay that can pair a screen must be able to answer that
// screen.
//
// Callers install it once, at construction, BEFORE Register/serving traffic —
// the same discipline EnablePersistence documents, and for the same reason:
// an assignment racing an in-flight request is a request served without it.
//
// It is the ONLY thing that establishes or changes this server's signing
// identity once the server is built: no program write can reach s.signingKey,
// because neither SetProgram nor SetServedProgram takes a key at all. That is
// asserted, not just intended (TestOnlySetSigningKeyEstablishesTheRelayIdentity).
//
// The precise scope of that assertion, since "by construction" would overstate
// it: it rejects any ASSIGNMENT to s.signingKey outside this method. It does not
// cover the field being seeded in NewServer's own composite literal, which is a
// second establishment path with no guard on it — today NewServer sets no key
// there, and it should stay that way, but nothing fails if someone changes that. The identity is
// held once for the whole server rather than per screen ON PURPOSE: it is the
// relay's own, identical for every screen, so if it is ever rotated, every
// screen's Leases must move to the new key at once — a per-screen copy would
// keep signing one screen's Leases with a retired key until that screen's
// program happened to be rewritten, and a player pinning the currently-presented
// cert would reject them.
//
// What it is passed is what this server signs with, including nothing at all.
// Expressing "this relay currently has no signing identity" is a state a caller
// is allowed to set deliberately, and while it holds, handleProgram refuses
// every pull rather than issuing an unsigned Lease.
func (s *Server) SetSigningKey(key ed25519.PrivateKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signingKey = key
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
// It takes no signing key either, for the reason SetProgram's own doc gives:
// the Lease-signing identity belongs to the relay, is established solely by
// SetSigningKey, and a per-screen write has no business being able to change it.
//
// The entry is installed for sp.ScreenID alone (REL-061's own `screen_id`),
// so a caller replays the whole persisted `screen_programs` array through this
// method — one call per entry — and each screen is served ITS entry. A screen
// the array carries no entry for is served data-model/1's terminal default
// (DAT-118, terminalDefault), never a sibling screen's program.
//
// A `slide` content item (native slide rendering, parity milestone 2) is the
// one item kind whose layers this conversion carries through — and the one it
// can REFUSE. Its ContentRef.Layers are validated by wire.ValidateSlideLayers
// before the item becomes a Lease content item; a slide whose layers do not
// validate is DROPPED from the converted content rather than served, because a
// player has no defined behavior for a malformed layer and a slide that would
// not draw cleanly must never reach the wire. Every non-slide item is
// unaffected: its Layers is empty, so it is carried through exactly as before
// and marshals byte-identically. This method is the single production caller of
// wire.ValidateSlideLayers.
//
// generation is the persisted last-applied generation this served program
// belongs to (relay/1 REL-052/056), carried into SetProgram's own generation
// fence: it is the boot-time baseline a same-generation schedule resolver then
// replaces, and a strictly-older stale write can never revert.
func (s *Server) SetServedProgram(generation int64, sp wire.ScreenProgram) {
	content := make([]wire.LeaseContent, 0, len(sp.Content))
	for _, c := range sp.Content {
		contentType := c.ContentType
		if contentType == "" {
			// REL-061a back-compat default: an item carrying no content_type
			// (an older feeder) is treated as `image`, this codebase's own
			// historical implicit value from before the field existed.
			contentType = "image"
		}
		var layers []wire.Layer
		if contentType == leaseContentTypeSlide {
			// A slide's layers are validated before they are ever served: a
			// malformed slide is dropped, not handed to a player (native slide
			// rendering, parity milestone 2). Only a slide item carries layers
			// onto the Lease; every other kind leaves the field nil, so it
			// marshals with no `layers` key, byte-identical to before.
			if err := wire.ValidateSlideLayers(c.Layers); err != nil {
				continue
			}
			layers = c.Layers
		}
		content = append(content, wire.LeaseContent{
			Type:       contentType,
			AssetRef:   c.AssetRef,
			URL:        c.URL,
			ExpiresAt:  c.ExpiresAt,
			DurationMS: c.DurationMS,
			Layers:     layers,
		})
	}
	s.SetProgram(generation, sp.ScreenID, sp.ProgramRevision, sp.Priority, sp.Display, content)
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

	screenID, nowMs, ok := s.authorizeChannelToken(w, r, traceID)
	if !ok {
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
	live := s.slideLive
	s.mu.Unlock()

	// No signing key at all — the deployment never called SetSigningKey, so this
	// server has no relay identity to sign with. Every Lease MUST carry a
	// signature a player verifies against its pinned trust anchor (PLY-090), and
	// there is no unsigned Lease in that contract, so this refuses rather than
	// issuing one. It is a 500 because the fault is entirely the relay's own
	// configuration: the player's credential is valid and its request is
	// well-formed.
	//
	// It is NOT the "this screen has no program" case — that one is served
	// data-model/1's terminal default above (DAT-118, programForLocked), signed
	// with this same key like any other Lease. The guard remains because
	// signhash.Sign on a nil key panics rather than erroring, which on the
	// serving goroutine takes the request's whole connection down.
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
		// Live-widget resolution runs LAST, over exactly the items that survived
		// the capability filter and the duration clamp — so a slide a player
		// cannot draw costs no weather lookup, and the resolved values are the
		// freshest the relay has at the instant it signs (internal/slidelive's
		// doc explains why this issuance point, and not either projection, is
		// where a `weather`/`entity` layer gets its value). It resolves onto a
		// COPY: prog.Content is the served program's own retained state, and a
		// value written into it would outlive this Lease and overwrite the
		// authored layer permanently.
		Content:    slidelive.ResolveContent(clampContentDurations(filterContentTypes(prog.Content, req.Capabilities.ContentTypes)), live),
		IssuedAt:   nowMs,
		ValidUntil: nowMs + leaseValidity.Milliseconds(),
	}

	canon, err := wire.LeaseSignedBytes(lease)
	if err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusInternalServerError, "INTERNAL", "Internal Error")
		return
	}
	signature := wire.EncodeSignature(signhash.Sign(signingKey, canon))

	// Remember which screen this lease was issued to, so the acknowledgement and
	// telemetry routes can tell an ack for THIS player's lease from an ack for a
	// sibling screen's — the cross-screen case PLY-070 forbids outright. Recorded
	// only once the Lease is certain to be handed out: a lease that failed to
	// sign was never issued, and binding it would let an id nobody received be
	// acknowledged.
	s.mu.Lock()
	s.rememberIssuedLeaseLocked(screenID, lease.LeaseID)
	// Record the pull for the screen-status surface (screenstatus.go), under the
	// same lock and at the same instant the Lease was stamped with — so a status
	// report can never claim a screen pulled at a time no Lease was issued.
	s.noteProgramPullLocked(screenID, nowMs, lease)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, LeaseResponse{Lease: lease, Signature: signature})
}

// authorizeChannelToken performs the channel-token check every operation a
// channel token authorizes MUST perform (PLY-070: Program delivery, Leases,
// render acknowledgement and telemetry, and renewal), and writes the Problem
// itself when it refuses — so a caller is one `if !ok { return }` away from
// being correct, and no route can accidentally implement four of the five
// checks. It returns the token's own screen_id and the single clock reading the
// expiry check used.
//
// Auth error taxonomy: an absent, malformed, or unresolvable token maps to
// `CHANNEL_TOKEN_INVALID` — the Error taxonomy's own code for "malformed or
// unknown"; a resolvable token past its own `expires_at` maps to
// `CHANNEL_TOKEN_EXPIRED` (PLY-072), distinct because PLY-073 requires a player
// treat the two differently (renew vs. re-pair).
func (s *Server) authorizeChannelToken(w http.ResponseWriter, r *http.Request, traceID string) (string, int64, bool) {
	token := bearerToken(r)
	if token == "" {
		apihttp.WriteProblem(w, r, traceID, http.StatusUnauthorized, "CHANNEL_TOKEN_INVALID", "Channel Token Invalid")
		return "", 0, false
	}

	// lookupSession, not LookupChannelToken: a session the relay has DROPPED
	// (SetRevokedScreens' own session termination) must still resolve here, or
	// the revocation check below has no screen_id to run against and a revoked
	// screen's own token would draw CHANNEL_TOKEN_INVALID where PLY-072
	// requires CHANNEL_TOKEN_REVOKED.
	sess, ok := s.lookupSession(token)
	if !ok {
		apihttp.WriteProblem(w, r, traceID, http.StatusUnauthorized, "CHANNEL_TOKEN_INVALID", "Channel Token Invalid")
		return "", 0, false
	}
	screenID := sess.ScreenID
	// Revocation (PLY-072) is checked BEFORE expiry: it is terminal (PLY-073's
	// re-pair path), so a token that is both revoked and expired reports
	// CHANNEL_TOKEN_REVOKED — driving the player to re-pair rather than to a
	// renewal (PLY-074) of a credential whose screen no longer validates
	// (PLY-075). The check reads the relay's own last-synced revocation view,
	// valid even while disconnected from the app peer (REL-123).
	//
	// It is also checked before the dropped-session check below, and that order
	// is the contract's: PLY-072 names CHANNEL_TOKEN_REVOKED for a token whose
	// screen_id is present in `revoked`, whatever else is true of the token.
	if s.isScreenRevoked(screenID) {
		apihttp.WriteProblem(w, r, traceID, http.StatusUnauthorized, "CHANNEL_TOKEN_REVOKED", "Channel Token Revoked")
		return "", 0, false
	}
	// A session the relay dropped, whose screen is no longer revoked: the
	// credential itself is dead and stays dead. Withdrawing a revocation
	// restores the ability to PAIR, never a token minted before it — the only
	// party still presenting one is whoever kept a copy of a credential an
	// honest player already cleared (PLY-073). CHANNEL_TOKEN_INVALID is the
	// taxonomy's own code for a token that no longer resolves to a usable
	// session, and PLY-136 makes a player clear it and re-pair — the outcome
	// this state wants.
	if sess.Terminated {
		apihttp.WriteProblem(w, r, traceID, http.StatusUnauthorized, "CHANNEL_TOKEN_INVALID", "Channel Token Invalid")
		return "", 0, false
	}
	expiresAt := sess.ExpiresAt
	// Read ONCE, from the server's own clock (NewServer's required nowMs), and
	// reused for the Lease's own stamps below. On a relay that is the
	// floor-aware reading, so a rolled-back host clock cannot revive a channel
	// token whose expires_at has passed (PLY-072) any more than it can revive
	// an elapsed pairing grant.
	//
	// One read rather than three also keeps the Lease self-consistent: issued_at
	// and valid_until come from the same instant the expiry check ran, so a
	// token that just barely validated can never be handed a Lease stamped as
	// though it were already stale.
	nowMs := s.nowMs()
	if nowMs > expiresAt {
		apihttp.WriteProblem(w, r, traceID, http.StatusUnauthorized, "CHANNEL_TOKEN_EXPIRED", "Channel Token Expired")
		return "", 0, false
	}

	return screenID, nowMs, true
}

// leaseHistoryPerScreen is how many recently-issued lease_ids a screen's entry
// keeps. PLY-114 draws the line at "currently or most-recently active", which is
// two; the extra room absorbs a player that pulls a few times while an
// acknowledgement is in flight without ever admitting an id this relay did not
// mint. Small deliberately: the point of the record is to refuse ids, so a long
// history only widens what is accepted.
const leaseHistoryPerScreen = 4

// telemetryHistory bounds the render reports and lease acknowledgements this
// server keeps. A relay is the component with nobody watching its memory: it runs
// on an appliance for months between touches, and every one of these records
// arrives on an ordinary success path — a player reports a render start and end
// per content item, for as long as it plays.
//
// Bounding them loses nothing that is currently kept, and that is worth being
// precise about rather than hand-waving. PLY-091 does require a relay to persist
// acknowledgement state durably (mirroring REL-142), and PLY-113/REL-090 require
// render reports to reach the app peer — but neither is implemented: these are
// Wave-1 in-memory records, read only by this server's own accessors. So the
// choice here is between dropping the oldest and running out of memory, not
// between dropping the oldest and keeping them.
//
// When the durable half lands, the bound moves: an acknowledgement must survive
// long enough to be forwarded, which is a retention rule about DELIVERY rather
// than a cap on count, and this constant should be replaced rather than raised.
const telemetryHistory = 256

// rememberIssuedLeaseLocked records that leaseID was issued to screenID. Caller
// holds s.mu.
func (s *Server) rememberIssuedLeaseLocked(screenID, leaseID string) {
	h := append(s.issuedLeases[screenID], leaseID)
	if len(h) > leaseHistoryPerScreen {
		// Copy forward rather than reslicing: h[1:] keeps the whole backing array
		// alive, so a long-lived screen's entry would pin every lease_id it was
		// ever issued while showing a length of four.
		h = append([]string(nil), h[len(h)-leaseHistoryPerScreen:]...)
	}
	s.issuedLeases[screenID] = h
}

// appendBounded appends v and keeps at most telemetryHistory entries, oldest
// first out.
//
// It copies forward rather than reslicing when it trims. Reslicing keeps the
// whole backing array alive behind the window, which is the leak this bound
// exists to prevent wearing the shape of a fix — though only until the next
// append outgrows the array and Go reallocates anyway, so the copy is defensive
// rather than load-bearing.
//
// Said plainly because a mutation that switches it back to a reslice fails NO
// test: the difference is a transient the runtime erases on its own, and a test
// asserting on capacity measures Go's growth policy rather than this decision. I
// wrote such a test, watched the mutant survive it, and deleted it — a test that
// cannot see the thing it names is worse than none, because it reads as coverage.
func appendBounded[T any](history []T, v T) []T {
	history = append(history, v)
	if len(history) > telemetryHistory {
		history = append([]T(nil), history[len(history)-telemetryHistory:]...)
	}
	return history
}

// leaseIssuedTo reports whether leaseID is one this relay issued to screenID.
//
// A lease_id issued to a DIFFERENT screen answers false here exactly as an
// invented one does, and that is deliberate: the presenting player learns its
// own reference is not one it holds, not that some other screen holds it.
// Answering differently would make this route a probe for whether a given
// lease_id exists somewhere on the relay.
func (s *Server) leaseIssuedTo(screenID, leaseID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.issuedLeases[screenID], leaseID)
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

	screenID, _, ok := s.authorizeChannelToken(w, r, traceID)
	if !ok {
		return
	}

	var req LeaseAckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}
	if req.LeaseID == "" {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}
	// PLY-114/LEASE_UNKNOWN: the acknowledgement has to name a Lease this relay
	// issued to THIS screen. Without it the ack map is keyed on whatever id the
	// caller supplies, so an acknowledgement can be recorded for a Lease that was
	// never issued, or for a sibling screen's — and PLY-091 makes this record
	// something the platform reasons about.
	if !s.leaseIssuedTo(screenID, req.LeaseID) {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "LEASE_UNKNOWN", "Lease Unknown")
		return
	}

	s.mu.Lock()
	s.leaseAcks[req.LeaseID] = req
	s.ackOrder = appendBounded(s.ackOrder, req.LeaseID)
	// Evict the ack whose id fell off the order, so the map cannot outgrow it.
	if len(s.ackOrder) == telemetryHistory {
		for id := range s.leaseAcks {
			if !slices.Contains(s.ackOrder, id) {
				delete(s.leaseAcks, id)
			}
		}
	}
	s.mu.Unlock()

	// The strongest liveness evidence short of a render report: the screen
	// received, parsed and accepted what it was handed (screenstatus.go).
	s.noteLeaseAck(screenID)

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

	screenID, _, ok := s.authorizeChannelToken(w, r, traceID)
	if !ok {
		return
	}

	var req RenderStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}
	if req.LeaseID == "" || req.AssetRef == "" {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}
	// Same binding as the ack, and it matters more here: PLY-113/REL-090 forward
	// this report upstream where it becomes events/1 content.played — the record a
	// proof-of-play claim would rest on. An unbound lease_id lets a caller assert
	// that a screen displayed something it never displayed.
	if !s.leaseIssuedTo(screenID, req.LeaseID) {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "LEASE_UNKNOWN", "Lease Unknown")
		return
	}

	s.mu.Lock()
	s.renderStarts = appendBounded(s.renderStarts, req)
	s.mu.Unlock()

	// The one observation in the screen-status record that is evidence of
	// something being ON the screen rather than of the screen having been told
	// what to show (screenstatus.go).
	s.noteRenderStart(screenID, req.AssetRef)

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

	screenID, _, ok := s.authorizeChannelToken(w, r, traceID)
	if !ok {
		return
	}

	var req RenderEndRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}
	if req.AssetRef == "" || req.Cause == "" || req.Completion == "" {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}
	// This body carries a CLIENT-SUPPLIED screen_id, because PLY-111 makes it
	// events/1's content.played payload field for field (EVT-050) — and that field
	// is the subject of the record, not a hint. An authenticated player naming
	// another screen there would file playback evidence against a screen it does
	// not hold a credential for, which is the cross-screen presentation PLY-070
	// forbids in as many words.
	//
	// Refused rather than silently overwritten with the token's screen: PLY-111's
	// body IS the upstream record, so a relay quietly rewriting a field would make
	// the report disagree with what the player believes it sent, and the player
	// would never learn its own screen_id was wrong. An empty screen_id is the
	// same refusal — a report has to say which screen it is about.
	//
	// CHANNEL_TOKEN_SCOPE_INVALID, not VALIDATION_FAILED (PLY-070a). This body
	// PASSED schema validation — every field present and well typed. What failed
	// is the body against the CREDENTIAL, which is a fault of its own kind, and
	// reporting it as a validation failure tells a player its request was
	// malformed and sends it to re-serialize a body that was never the problem.
	//
	// It is deliberately not CHANNEL_TOKEN_INVALID either: PLY-073 makes that
	// code's remedy renew-or-re-pair, and a correctly-provisioned player would
	// then discard a working credential in response to a bug in its own request
	// (PLY-070b).
	if req.ScreenID != screenID {
		apihttp.WriteProblem(w, r, traceID, http.StatusForbidden, "CHANNEL_TOKEN_SCOPE_INVALID", "Forbidden")
		return
	}

	s.mu.Lock()
	s.renderEnds = appendBounded(s.renderEnds, req)
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

// leaseContentTypeSlide is the player/1 content `type` (PLY-083) of a native
// slide item (native slide rendering, parity milestone 2) — the one item kind
// whose Lease content carries positioned layers rather than a single asset.
// SetServedProgram matches on it to decide which items to run through
// wire.ValidateSlideLayers, and it is the value a slide-capable player declares
// in its `content_types` so filterContentTypes serves it one; a player that
// does not declare it is transparently never served a slide (see
// filterContentTypes). It is a local const, mirroring schedulehost's own
// leaseContentTypeImage, so the string lives in one place rather than as a
// literal spread across the match, the filter tests, and any producer.
const leaseContentTypeSlide = "slide"

// filterContentTypes returns only the content items whose Type is present
// in declaredTypes (PLY-013/PLY-096): a relay MUST NOT hand back a content
// item of a type the player hasn't most-recently declared support for. An
// empty or nil declaredTypes excludes every item, never included by
// default.
//
// This gate is deliberately GENERIC over the item's Type, and that is what
// makes the additive `slide` content type (native slide rendering, parity
// milestone 2) capability-gated for free: a `type:"slide"` item is served
// only to a player whose declared `content_types` include "slide", and an
// older player that declares only `image`/`video` transparently never receives
// one — no per-type branch here, because a new content type must be gated the
// SAME way `image`/`video` already are, not by a special case that could drift.
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
