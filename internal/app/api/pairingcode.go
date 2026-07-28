package api

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/paircode"
	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// pairingcode.go is the operator half of screen pairing: POST
// /api/v1/screens/{screen_id}/pairing-code mints a screen-bound pairing grant
// (security-model/1 SEC-030 with relay/1 REL-121a's screen binding), persists
// it as desired state (store.AddPairingGrant — the write that bumps the
// generation, so the grant rides the next signed snapshot's `pairing_grants`
// section to every live relay, REL-067/REL-057), and answers with the
// human-enterable pairing code the operator reads onto the screen.
//
// # Why the APP forms the code
//
// REL-126 fixes what a pairing code encodes — the relay's dial address, a
// grant_selector, and a fingerprint_commitment over the relay's own current
// trust-anchor public key — and the relay logs such codes for the grants it
// holds at boot (boot only: a grant applied over a live re-pull is never
// logged, so for a mid-run mint this surface is the code's ONLY display).
// An operator pairing a screen is standing at
// the console, not at the relay's log, and player/1 explicitly contemplates
// the app's own display surface as the code's delivery channel (its Residual
// risk section names "the relay's or app's own display surface", and PLY-051's
// clause (b) admits a commitment delivered by any channel independent of the
// bootstrap fetch). The app peer independently holds all three components:
//
//   - grant_selector: the grant_id it just minted (the same value the relay
//     resolves against pairing_grants — playerserver.redeem keys on GrantID);
//   - dial address: the relay's OWN canonical advertised address from its
//     hello's subnet_metadata (REL-037 — by definition "the same address it
//     advertises in its own discovery/pairing responses"), retained by
//     relayconn.ConnectedRelays;
//   - fingerprint_commitment: computed over the relay's trust-anchor SPKI —
//     the very enrollment key the app itself certified (the relay serves
//     player/1 over its enrollment identity), so the commitment the app
//     computes here is byte-identical to the one the relay's own
//     FormPairingCode computes over its certificate.
//
// The shared paircode codec does the packing, so an app-formed code and a
// relay-formed code for the same grant are the same code.
//
// # What this operation does NOT know
//
// Whether the code was ever redeemed. The relay redeems autonomously
// (REL-121/122) and its redemption report upstream (REL-124) is not yet
// implemented, so this surface truthfully reports the grant's issuance and
// expiry — never a "paired" state it has no evidence for.

// PairingRelay is one live relay the pairing-code issuance may dial a code
// at: its enrollment identity and the canonical advertised address its hello
// declared (REL-037).
type PairingRelay struct {
	RelayID           string
	AdvertisedAddress string
}

// PairingRelayDirectory is the seam the pairing-code operation reads the
// relay side of a code from. Both funcs are wired by the deployment (main):
// ConnectedRelays from the live relay-connection server, RelaySPKI from the
// enrollment registry (the SubjectPublicKeyInfo DER of the relay's current
// enrollment key — the trust anchor PLY-052's commitment is computed over).
type PairingRelayDirectory struct {
	ConnectedRelays func() []PairingRelay
	RelaySPKI       func(relayID string) ([]byte, bool)
}

// WithPairing wires the relay directory the pairing-code operation forms
// codes from. Without it the route still mounts, still mints and persists
// the grant, and answers with no code and an honest reason — the same
// degrade-not-404 posture the other optional collaborators take.
func WithPairing(dir PairingRelayDirectory) Option {
	return func(s *server) { s.pairingRelays = dir }
}

// pairingCodeResult is the operation's response body (openapi
// PairingCodeResult). Timestamps are epoch ms; ttl_seconds matches the wire
// grant's own unit (REL-121).
type pairingCodeResult struct {
	GrantID        string `json:"grant_id"`
	ScreenID       string `json:"screen_id"`
	TTLSeconds     int64  `json:"ttl_seconds"`
	RedemptionMode string `json:"redemption_mode"`
	IssuedAt       int64  `json:"issued_at"`
	ExpiresAt      int64  `json:"expires_at"`
	PairingCode    string `json:"pairing_code,omitempty"`
	RelayID        string `json:"relay_id,omitempty"`
	// CodeUnavailableReason is set exactly when PairingCode is not: the grant
	// was minted and delivered regardless, and this says why no code could be
	// formed app-side (no relay connected, or its address/key unusable).
	CodeUnavailableReason string `json:"code_unavailable_reason,omitempty"`
}

// issuePairingCode implements POST /screens/{screen_id}/pairing-code. The
// body is read (and hashed) up front so Idempotency-Key replay-vs-reuse
// (API-052) covers the empty-body POST exactly as it covers a JSON one.
func (srv *server) issuePairingCode(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	srv.idempotent(w, r, raw, func(w http.ResponseWriter) { srv.issuePairingCodeExec(w, r) })
}

// issuePairingCodeExec is the operation's actual work, run once per fresh
// (non-replayed) request under the Idempotency-Key guard.
func (srv *server) issuePairingCodeExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("screen_id")
	res, found, err := srv.store.Get(r.Context(), store.KindScreen, id)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	if !found {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No screen exists with this identifier.")
		return
	}
	// Issuing a pairing credential begins by reading the screen row, so it is
	// scoped as a read first (a row outside the visible set answers exactly
	// as an id that names nothing) — and it ENDS as a write at the screen's
	// own placement: redeeming the minted grant makes a physical display
	// into this row's screen, which is authorized like any other mutation of
	// the row (SEC-005). Same read-then-write split runAutomationExec uses.
	view, verr := srv.scopeView(r)
	if verr != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	node := parseFields(res.Body).ScopeNode
	if !view.canRead(node) {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No screen exists with this identifier.")
		return
	}
	if !view.canWrite(node) {
		writeProblem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden", unauthorizedWriteDetail)
		return
	}

	g, err := grant.MintForScreen(id)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	if err := srv.store.AddPairingGrant(r.Context(), g, node, auth.IssuedViaAPI); err != nil {
		// The row was just resolved above, so this arm is the delete race:
		// the screen vanished between the read and the mint's own in-tx
		// existence check.
		if err == store.ErrPairingGrantScreenUnknown {
			writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No screen exists with this identifier.")
			return
		}
		// The move race: the screen still exists but was re-placed between
		// the authorization read above and the mint's in-tx re-check, so the
		// authority just established no longer describes the row. A retry
		// re-runs authorization against the current placement.
		if err == store.ErrPairingGrantScreenMoved {
			writeProblem(w, r, http.StatusConflict, "PLACEMENT_CHANGED", "Conflict", "The screen's placement changed while the pairing code was being issued. Retry the request.")
			return
		}
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	// SEC-034: every grant creation emits an audit.event carrying the grant's
	// purpose and issued_via. This is the grant-lifecycle record, subject
	// `grant:<id>`, filed at the screen's own node so exactly the operators
	// whose authority covers this screen can see it; the mutation middleware's
	// own `screens.pairing-code` record (subject `screens:<id>`) describes the
	// HTTP operation and stands alongside it, not instead of it.
	srv.auditor.Emit(auth.Record{
		TraceID:   apihttp.TraceID(r),
		Actor:     auth.PrincipalID(r),
		Action:    auth.ActionGrantCreated,
		Target:    "grant:" + g.GrantID,
		Result:    "success",
		Purpose:   g.Purpose,
		IssuedVia: auth.IssuedViaAPI,
		ScopeNode: node,
	})

	out := pairingCodeResult{
		GrantID:        g.GrantID,
		ScreenID:       id,
		TTLSeconds:     g.TTL,
		RedemptionMode: g.RedemptionMode,
		IssuedAt:       g.IssuedAt,
		ExpiresAt:      g.IssuedAt + g.TTL*1000,
	}
	out.PairingCode, out.RelayID, out.CodeUnavailableReason = srv.formPairingCode(g)

	body, err := json.Marshal(out)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	writeJSON(w, http.StatusCreated, body)
}

// formPairingCode forms the REL-126 code for g against the first connected
// relay, or explains why it could not. Every degrade is a reason, never an
// error status: the grant IS minted and delivered either way, and the
// operator's remedy (connect a relay, read the code off the relay itself)
// is theirs to choose.
func (srv *server) formPairingCode(g wire.PairingGrant) (code, relayID, reason string) {
	dir := srv.pairingRelays
	if dir.ConnectedRelays == nil || dir.RelaySPKI == nil {
		return "", "", "this deployment has no relay directory wired; read the pairing code off the relay's own display"
	}
	relays := dir.ConnectedRelays()
	if len(relays) == 0 {
		return "", "", "no relay is connected; the code can be formed once one is"
	}
	relay := relays[0]
	host, portStr, err := net.SplitHostPort(relay.AdvertisedAddress)
	if err != nil {
		return "", "", "the connected relay advertised no dialable address"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return "", "", "the connected relay advertised no dialable address"
	}
	spki, ok := dir.RelaySPKI(relay.RelayID)
	if !ok {
		return "", "", "the connected relay's trust-anchor key is not on record"
	}
	return paircode.Encode(host, port, g.GrantID, tlsboot.Commitment(spki)), relay.RelayID, ""
}
