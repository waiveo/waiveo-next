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

// program is the relay's one served screen-program (Wave-1 first-photon:
// exactly one applied screen-program system-wide, `relay/1` REL-061),
// carried unmodified from the verified desired-state snapshot
// (internal/relay/desiredstate.Applied) — SetProgram is main.go's own
// hand-off point from that verified value into this server.
type program struct {
	ProgramRevision string
	Priority        string
	Display         string
	Content         []wire.LeaseContent
}

// SetProgram configures the program-delivery state GET /player/v1/program
// serves: programRevision/priority/display/content carried UNMODIFIED onto
// every Lease this server issues (PLY-108 priority, PLY-109 display —
// REL-061's entry reflected exactly), signed with signingKey.
//
// signingKey MUST be the relay's own enrollment private key
// (internal/relay/identity.RelayIdentity.PrivateKey) — the SAME keypair
// the certificate passed to NewServer (relayCertPEM) certifies, so a
// player's Steady-state-pinning verification of a Lease's signature
// (PLY-090, "verifiable... against the same trust anchor its... connection
// to this relay is itself pinned to") checks against the exact cert this
// relay's player/1 listener presents.
//
// Wave-1 first-photon calls this once at boot with the single applied
// screen-program; nothing here refreshes it on a later desired-state
// re-pull (out of this task's scope — a later task wires a live update
// path).
func (s *Server) SetProgram(programRevision, priority, display string, content []wire.LeaseContent, signingKey ed25519.PrivateKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.program = program{
		ProgramRevision: programRevision,
		Priority:        priority,
		Display:         display,
		Content:         content,
	}
	s.signingKey = signingKey
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

	s.mu.Lock()
	prog := s.program
	signingKey := s.signingKey
	s.mu.Unlock()

	lease := wire.Lease{
		LeaseID:         newOpaqueToken("lease"),
		ScreenID:        screenID,
		ProgramRevision: prog.ProgramRevision,
		Priority:        prog.Priority,
		Display:         prog.Display,
		Content:         filterContentTypes(prog.Content, req.Capabilities.ContentTypes),
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

// filterContentTypes returns only the content items whose Type is present
// in declaredTypes (PLY-013/PLY-096): a relay MUST NOT hand back a content
// item of a type the player hasn't most-recently declared support for. An
// empty or nil declaredTypes excludes every item, never included by
// default.
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
