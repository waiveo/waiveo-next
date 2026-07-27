package auth

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// The HTTP half of SEC-004's second-factor floor: the second-factor step inside
// the login exchange, and the two operations that enroll a `totp` credential in
// the first place.
//
// Nothing here consults r.TLS. SEC-004 requires `totp` be "available without a
// secure-context precondition" — a deployment on the self-signed fallback
// (SEC-110) "MUST continue to accept `totp` while declining to offer `passkey`
// enrollment or authentication" — so a secure-context gate on these routes would
// be a violation rather than a hardening. When `passkey` lands it gets that gate;
// this does not.

// totpIssuer is the issuer label an `otpauth://` URI carries. It is what the
// operator's authenticator app shows beside the code, so it names the product,
// not the host.
const totpIssuer = "Waiveo"

// secondFactorSatisfied runs SEC-004's floor for a principal whose password has
// just verified, writing the refusal Problem and reporting false when the factor
// is due and not satisfied.
//
// The lockout is keyed on the TOTP credential's OWN id, not on the password
// credential's. SEC-090 scopes lockout per `(credential, source-IP class)`, and a
// second factor is a second credential — keying both factors on the password's
// id would mean a wrong code consumed the password's budget, and, worse, that an
// attacker who already HAS the password could brute-force codes under a budget
// the password's own successes keep resetting. Two credentials, two keys, and
// the code guesser burns down the one that belongs to the credential they are
// guessing at.
func (h *Handlers) secondFactorSatisfied(w http.ResponseWriter, r *http.Request, traceID, principalID, code, ipClass string) bool {
	ctx := r.Context()
	store := h.auth.store

	cred, err := store.FindTOTPCredential(ctx, principalID)
	if errors.Is(err, ErrCredentialNotFound) {
		// No second factor enrolled: the password alone completes this login.
		// SEC-004 requires the platform to SUPPORT totp as the guaranteed floor,
		// which is what the enrollment routes below provide; it does not itself
		// declare every principal enrolled.
		return true
	}
	if err != nil {
		writeInternal(w, r, traceID)
		return false
	}

	key := LockoutKey(cred.CredentialID, ipClass)
	if locked, retryMs := h.auth.lockout.Locked(key, store.Now()); locked {
		h.auth.auditor.Emit(Record{
			TraceID: traceID, Actor: principalID, Action: ActionLoginLockout,
			Target: "credential:" + cred.CredentialID, Result: "failure",
		})
		w.Header().Set("Retry-After", strconv.FormatInt((retryMs+999)/1000, 10))
		apihttp.WriteProblemExt(w, r, traceID, http.StatusTooManyRequests,
			"CREDENTIAL_LOCKED", "Too Many Requests",
			"Too many failed attempts for this credential from this source; retry after the stated backoff.", nil)
		return false
	}

	if code == "" {
		// The factor is due and the body carried none. This is the ONLY response
		// on this path that says anything more than the generic refusal, and it
		// says only "a totp code is required" — never whether an identifier
		// exists, since reaching this branch already required a correct password.
		apihttp.WriteProblemExt(w, r, traceID, http.StatusUnauthorized,
			"UNAUTHENTICATED", "Unauthorized",
			"A second-factor code is required to complete this sign-in.",
			map[string]any{secondFactorProblemMember: secondFactorTOTP})
		return false
	}

	secret, err := store.TOTPSecret(cred)
	if err != nil {
		// The credential exists but its secret will not open — a key-material
		// problem, not a caller problem. Failing CLOSED is the only safe answer:
		// treating an unopenable second factor as absent would turn a lost
		// workspace key into a silent downgrade to password-only.
		writeInternal(w, r, traceID)
		return false
	}

	step, matched := MatchTOTPCode(secret, code, store.Now())
	// A matched code is claimed atomically; an unclaimed step is a REPLAY of a
	// code already spent (or of one from a window this credential has passed),
	// and is refused exactly as a wrong code is (totpstore.go).
	claimed := false
	if matched {
		claimed, err = store.ConsumeTOTPStep(ctx, cred.CredentialID, step)
		if err != nil {
			writeInternal(w, r, traceID)
			return false
		}
	}
	if !claimed {
		h.auth.lockout.Fail(key, store.Now())
		// Two records for one refusal, on purpose. EVT-081 names "login failure"
		// on its mandatory-emission list and a refused second factor IS a failed
		// login, so `login.failure` is emitted; SEC-064's discipline against a
		// shared generic action string is why the specific act gets its own
		// record too. They share a trace id, so the pair reads as one event.
		h.auth.auditor.Emit(Record{
			TraceID: traceID, Actor: principalID, Action: ActionSecondFactorFailure,
			Target: "credential:" + cred.CredentialID, Result: "failure",
		})
		h.auth.auditor.Emit(Record{
			TraceID: traceID, Actor: principalID, Action: ActionLoginFailure,
			Target: "credential:" + cred.CredentialID, Result: "failure",
		})
		// Byte-identical to the wrong-password refusal, and deliberately so: a
		// response that distinguished them would tell an attacker holding a
		// password list which entries were right.
		apihttp.WriteProblemExt(w, r, traceID, http.StatusUnauthorized,
			"UNAUTHENTICATED", "Unauthorized", "The identifier or password is incorrect.", nil)
		return false
	}

	h.auth.lockout.Succeed(key)
	_ = store.TouchCredential(ctx, cred.CredentialID)
	h.auth.auditor.Success(traceID, principalID, ActionSecondFactorSuccess, "credential:"+cred.CredentialID)
	return true
}

// ---- enrollment -----------------------------------------------------------

// totpEnrollmentResponse is POST /auth/totp/enroll's body: the shared secret in
// the two forms an operator can act on — the `otpauth://` URI a QR code encodes,
// and the base32 key they can type by hand when a camera is not an option.
//
// The secret leaves the server exactly once, in this response body, over the
// same TLS the session cookie rides. It is never placed in a query string, a
// redirect, a log line or an error string (EVT-112's reasoning applies to any
// secret, not only to bearer tokens), and after this response the only copy on
// this side is sealed.
type totpEnrollmentResponse struct {
	Secret     string `json:"secret"`
	OtpauthURI string `json:"otpauth_uri"`
}

// totpConfirmRequest is POST /auth/totp/confirm's body: a code computed from the
// pending secret, which is the only available evidence that the secret reached
// an authenticator rather than a QR code nobody scanned.
type totpConfirmRequest struct {
	Code string `json:"code"`
}

// totpCredentialResponse is the armed credential, in security-model/1's own
// Credential wire shape (metadata only — never a raw secret value), plus the
// count of other sessions the enrollment evicted.
type totpCredentialResponse struct {
	CredentialID    string `json:"credential_id"`
	PrincipalID     string `json:"principal_id"`
	Kind            string `json:"kind"`
	CreatedAt       int64  `json:"created_at"`
	RevokedSessions int    `json:"revoked_sessions"`
}

// EnrollTOTP begins a second-factor enrollment for the CALLING principal.
//
// Who may enroll: a principal, for itself, and nobody else. There is no
// principal_id in the body and no admin-enrolls-a-user path, for two reasons.
// The mechanical one is that enrollment hands back a secret, and an operator who
// can see another principal's TOTP secret holds that principal's second factor.
// The contractual one is SEC-052: clearing or re-enrolling a target's TOTP is
// explicitly NOT within plain `admin` authority, and requires either an
// owner-explicit path or the console binding — neither of which is this route.
//
// Re-enrollment over an armed credential is refused here (ErrTOTPAlreadyEnrolled)
// with ONE exception: a caller on an `aal: recovery` session. SEC-022 keeps such
// a session restricted "until the target principal completes TOTP
// re-enrollment", so refusing that caller for already holding a TOTP credential
// would wall off the contract's own exit from the restricted tier. This route is
// therefore one of the operations a recovery session must keep when the AAL
// restriction lands.
func (h *Handlers) EnrollTOTP(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)
	p, err := RequirePrincipal(r.Context())
	if err != nil {
		writeInternal(w, r, traceID)
		return
	}
	store := h.auth.store

	secret, err := store.BeginTOTPEnrollment(r.Context(), p.ID, p.AAL == AALRecovery)
	switch {
	case errors.Is(err, ErrTOTPAlreadyEnrolled):
		apihttp.WriteProblemExt(w, r, traceID, http.StatusForbidden,
			"FORBIDDEN", "Forbidden",
			"This principal already holds a second-factor credential; replacing it is a credential-reset or recovery operation, not a self-service one.", nil)
		return
	case errors.Is(err, ErrNoSecretSealer):
		// No workspace key means no way to store the secret safely. UNAVAILABLE
		// rather than INTERNAL: it is a missing dependency, and it is fixed by
		// establishing the key, not by retrying blindly.
		apihttp.WriteProblemExt(w, r, traceID, http.StatusServiceUnavailable,
			"UNAVAILABLE", "Service Unavailable",
			"This deployment holds no workspace key to protect a second-factor secret with; second-factor enrollment is unavailable until one is established.", nil)
		return
	case err != nil:
		writeInternal(w, r, traceID)
		return
	}

	account := p.ID
	if principal, err := store.GetPrincipal(r.Context(), p.ID); err == nil && principal.DisplayName != "" {
		account = principal.DisplayName
	}
	writeJSON(w, http.StatusOK, totpEnrollmentResponse{
		Secret:     EncodeTOTPSecret(secret),
		OtpauthURI: TOTPURI(totpIssuer, account, secret),
	})
}

// ConfirmTOTP arms the pending enrollment once the calling principal returns a
// code computed from it.
//
// Two things happen at arming beyond the credential row itself:
//
//   - The confirming code's own step is spent (Store.ArmTOTPCredential), so the
//     code that proved the enrollment cannot also open a session.
//   - Every OTHER live session and API key belonging to this principal is
//     revoked. A session minted on one factor should not outlive the moment the
//     principal decided one factor was not enough — this is the same reasoning
//     SEC-053 applies to a credential reset, at the one other point where a
//     principal's authentication strength changes. The CALLING session survives,
//     because the caller has, in this very request, demonstrated possession of
//     the second factor; signing them out would only mean signing straight back
//     in with the credential they just proved.
//
// The confirmation is not rate-limited beyond the ordinary authenticated
// surface, and that is a deliberate omission rather than a gap: the secret being
// confirmed was handed to THIS caller, in cleartext, seconds ago. There is
// nothing here to guess.
func (h *Handlers) ConfirmTOTP(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)
	p, err := RequirePrincipal(r.Context())
	if err != nil {
		writeInternal(w, r, traceID)
		return
	}
	var req totpConfirmRequest
	if !decodeBody(w, r, traceID, &req) {
		return
	}
	if req.Code == "" {
		writeValidationProblem(w, r, traceID, []fieldError{{"code", "required", "a verification code is required"}})
		return
	}

	ctx := r.Context()
	store := h.auth.store

	secret, err := store.PendingTOTPSecret(ctx, p.ID)
	switch {
	case errors.Is(err, ErrNoPendingTOTP):
		apihttp.WriteProblemExt(w, r, traceID, http.StatusNotFound,
			"NOT_FOUND", "Not Found",
			"No second-factor enrollment is in progress for this principal; start one first.", nil)
		return
	case errors.Is(err, ErrNoSecretSealer):
		apihttp.WriteProblemExt(w, r, traceID, http.StatusServiceUnavailable,
			"UNAVAILABLE", "Service Unavailable",
			"This deployment holds no workspace key to protect a second-factor secret with; second-factor enrollment is unavailable until one is established.", nil)
		return
	case err != nil:
		writeInternal(w, r, traceID)
		return
	}

	step, matched := MatchTOTPCode(secret, req.Code, store.Now())
	if !matched {
		// EVT-083: a failed action is exactly as auditable as a successful one.
		h.auth.auditor.Failure(traceID, p.ID, ActionTOTPEnrolled, "principal:"+p.ID)
		apihttp.WriteProblemExt(w, r, traceID, http.StatusUnauthorized,
			"UNAUTHENTICATED", "Unauthorized",
			"That code does not match this enrollment. Check the clock on the device generating it, then try the next code.", nil)
		return
	}

	cred, err := store.ArmTOTPCredential(ctx, p.ID, secret, step)
	if err != nil {
		writeInternal(w, r, traceID)
		return
	}

	// Evict every other session this principal holds (see the doc comment).
	revoked := 0
	if ids, err := store.ListSessionIDs(ctx, p.ID); err == nil {
		for _, id := range ids {
			if id == p.SessionID {
				continue
			}
			if err := store.RevokeSession(ctx, id); err == nil {
				revoked++
				h.auth.auditor.Success(traceID, p.ID, ActionSessionRevoked, "session:"+id)
			}
		}
	}

	h.auth.auditor.Success(traceID, p.ID, ActionTOTPEnrolled, "credential:"+cred.CredentialID)
	writeJSON(w, http.StatusOK, totpCredentialResponse{
		CredentialID:    cred.CredentialID,
		PrincipalID:     cred.PrincipalID,
		Kind:            cred.Kind,
		CreatedAt:       cred.CreatedAt,
		RevokedSessions: revoked,
	})
}
