package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// The revocation operation (api/1 API-140-144): the authoring half of two
// controls whose enforcement halves were built and could not be invoked.
//
// A relay certificate is refused on every handshake and telemetry push
// (relay/1 REL-016/041); a screen's revocation rides the signed snapshot and is
// enforced on every channel token, including while the relay is disconnected
// (REL-066, player/1 PLY-072). Both were real, tested, and unreachable.
//
// # Why one operation for two subject kinds
//
// They are the same decision twice — what act revokes a thing, and where an
// operator performs it — and deciding them apart guarantees drift in the three
// things that must not differ between two revocations reached for in the same
// incident: the authorization floor, the audit shape, and the confirmation.

// revokeRequest is POST /api/v1/revocations' body.
//
// `confirm` is the second half of API-143's round trip. An absent or false
// confirm makes this a RADIUS QUERY: nothing changes and the response reports
// what would stop working. That is deliberately the default, so a caller that
// forgets the field learns the blast radius rather than causing it.
type revokeRequest struct {
	SubjectKind string `json:"subject_kind"`
	SubjectID   string `json:"subject_id"`
	Confirm     bool   `json:"confirm"`
}

// revokeResponse reports the blast radius, and whether the act was performed.
type revokeResponse struct {
	SubjectKind string `json:"subject_kind"`
	SubjectID   string `json:"subject_id"`
	// ScreensAffected is how many screens stop being served if this proceeds
	// (API-143). For a screen it is one; for a relay it is every screen that
	// relay serves.
	ScreensAffected int `json:"screens_affected"`
	// Revoked is false on the unconfirmed radius query and true once the act
	// has been recorded.
	Revoked bool `json:"revoked"`
	// AlreadyRevoked reports that the subject was revoked before this call.
	// Revocation is terminal (API-142) and recording it twice is a no-op, so
	// this is how a caller tells "I did that" from "that was already true".
	AlreadyRevoked bool `json:"already_revoked"`
	// CertificatesRevoked is how many of a relay's issued certificates this
	// call marked revoked. Zero for a screen, and zero for a relay that has
	// never enrolled — which is not a failure: the record stands, and the
	// relay is refused if it later tries.
	CertificatesRevoked int `json:"certificates_revoked"`
}

// revokeSubject is POST /api/v1/revocations.
func (srv *server) revokeSubject(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if srv.undeclaredMemberRejected(w, r, "RevocationRequest", body) {
		return
	}
	srv.idempotent(w, r, body, func(w http.ResponseWriter) { srv.revokeSubjectExec(w, r, body) })
}

func (srv *server) revokeSubjectExec(w http.ResponseWriter, r *http.Request, body []byte) {
	// SEC-003f: owner at the workspace root. The floor is set by what the act
	// can do at its worst — a site's only relay revoked takes every screen at
	// that site dark — not by how often it is reached.
	if _, ok := srv.authorizeWorkspaceOwner(w, r); !ok {
		return
	}

	var req revokeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", "The request body could not be parsed.")
		return
	}
	kind := strings.TrimSpace(req.SubjectKind)
	if kind != store.RevocationSubjectScreen && kind != store.RevocationSubjectRelay {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"`subject_kind` must be `screen` or `relay`.")
		return
	}
	subjectID := strings.TrimSpace(req.SubjectID)
	if subjectID == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", "`subject_id` is required.")
		return
	}

	ctx := r.Context()
	already, err := srv.store.IsRevoked(ctx, kind, subjectID)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	radius, err := srv.revocationRadius(ctx, kind, subjectID)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	resp := revokeResponse{
		SubjectKind:     kind,
		SubjectID:       subjectID,
		ScreensAffected: radius,
		AlreadyRevoked:  already,
	}
	if !req.Confirm {
		// API-143's unconfirmed half: nothing changes, and the answer is the
		// radius the caller needs in order to confirm knowingly.
		writeJSONValue(w, http.StatusOK, resp)
		return
	}

	if err := srv.store.RevokeSubject(ctx, kind, subjectID, auth.PrincipalID(r)); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	resp.Revoked = true

	// A relay's certificates are held by the enrollment authority, not by this
	// store, so the record above is not by itself the revocation for that kind
	// — the connection-time check reads the issuance registry (relay/1
	// REL-016/041). Reaching it is what makes this operation the AUTHORING half
	// rather than a second inert record beside an inert enforcement.
	//
	// Every issuance, not one serial: a re-enrolled relay holds more than one,
	// and leaving the others valid means it reconnects on the next one while the
	// operator believes it is cut off.
	if kind == store.RevocationSubjectRelay && srv.relayCertRevoker != nil {
		resp.CertificatesRevoked = srv.relayCertRevoker(subjectID)
	}

	// API-144: the record carries the radius REPORTED at confirmation, because
	// that is what the operator was shown when they decided. A trail that omits
	// it cannot answer whether they understood what they were about to do.
	if srv.auditor != nil {
		srv.auditor.Emit(auth.Record{
			TraceID: apihttp.TraceID(r),
			Actor:   auth.PrincipalID(r),
			Action:  "revocation.recorded",
			Target:  kind + ":" + subjectID,
			Result:  "success",
			Detail:  "screens_affected=" + strconv.Itoa(radius),
		})
	}
	writeJSONValue(w, http.StatusOK, resp)
}

// revocationRadius counts the screens that stop being served if this revocation
// proceeds (API-143).
//
// A screen is one screen. A relay is every screen it currently serves — and
// that count comes from the device registry's own view rather than from a
// stored association, because the relay that serves a screen is a live fact
// (which relay is reporting it) rather than an authored one.
func (srv *server) revocationRadius(ctx context.Context, kind, subjectID string) (int, error) {
	switch kind {
	case store.RevocationSubjectScreen:
		return 1, nil
	case store.RevocationSubjectRelay:
		ds, err := srv.store.DesiredState(ctx)
		if err != nil {
			return 0, err
		}
		// Every screen the deployment serves. This deployment runs one relay
		// per site (the self-hosted cap), so a relay's revocation darkens every
		// screen at that site — and reporting the whole count is the honest
		// answer rather than a narrower one that would understate it.
		//
		// A multi-relay deployment would narrow this by which relay serves each
		// screen; over-reporting is the safe direction to be wrong in for a
		// confirmation prompt, and under-reporting is not.
		return len(ds.Screens), nil
	}
	return 0, nil
}

// WithRelayCertRevoker wires the revocation operation to the enrollment
// authority that holds a relay's certificates.
//
// It is a function rather than the enroll server itself because the api package
// must not depend on the feeder's enrollment internals: this surface needs one
// verb — "revoke every certificate for this relay, tell me how many" — and
// taking the whole server would let a later change here reach into a registry
// this package has no business knowing the shape of.
//
// Unset, a relay revocation still records the decision and reports zero
// certificates. That is the honest degraded answer for a deployment whose api
// runs apart from its enrollment authority, and the count in the response is
// what makes it visible rather than silent.
func WithRelayCertRevoker(fn func(relayID string) int) Option {
	return func(srv *server) { srv.relayCertRevoker = fn }
}
