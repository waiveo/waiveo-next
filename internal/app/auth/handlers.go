package auth

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// contentTypeJSON is the media type every success body here uses; errors use
// apihttp.ProblemContentType.
const contentTypeJSON = "application/json"

// Handlers serves the credential-exchange and session-management operations.
// Login and first-boot claim are api/1 `security: []` operations (API-090/091):
// their whole purpose is to mint a credential for a caller that does not yet
// hold one, so requiring a pre-existing session would be circular. Logout and
// the session read are ordinary authenticated operations and are mounted BEHIND
// the middleware, which is what subjects logout to the same CSRF discipline
// every other mutating browser route carries (SEC-024).
type Handlers struct {
	auth *Authenticator
	// budget bounds repeated guesses at a live grant's code (SEC-033). It is on
	// the claim path, not the login path — login is bounded by per-credential
	// lockout (SEC-090) instead, which is the control that requirement names.
	budget *GrantAttemptBudget
	// claimScopeNode is the scope node the first owner binding is placed at
	// (see Bootstrap).
	claimScopeNode string
}

// NewHandlers returns the auth HTTP surface over a. budget may be nil, in which
// case the default attempt budget is used.
func NewHandlers(a *Authenticator, budget *GrantAttemptBudget, claimScopeNode string) *Handlers {
	if budget == nil {
		budget = NewDefaultGrantAttemptBudget()
	}
	if claimScopeNode == "" {
		claimScopeNode = RootScopeNode
	}
	return &Handlers{auth: a, budget: budget, claimScopeNode: claimScopeNode}
}

// sessionResponse is the body a login, a claim, and a session read all return —
// one shape, so a client has one thing to parse. It carries no token: the
// session token itself rides the HttpOnly cookie and is deliberately not
// readable by the page, while csrf_token is the double-submit value the page
// MUST echo on every mutating request (SEC-024).
type sessionResponse struct {
	PrincipalID string `json:"principal_id"`
	Kind        string `json:"kind"`
	Role        Role   `json:"role"`
	AAL         AAL    `json:"aal"`
	SessionID   string `json:"session_id"`
	CSRFToken   string `json:"csrf_token,omitempty"`
}

// loginRequest is POST /auth/login's body. API-091: a credential-exchange
// operation "MUST authenticate the request from credentials carried in the
// request body; it MUST NOT require a pre-existing session or API key as a
// precondition of its own success."
//
// TOTPCode is the second factor (SEC-004), optional in the SHAPE and mandatory
// in EFFECT for any principal holding a `totp` credential — a login that omits
// it for such a principal is refused, so the optionality here is only the
// two-call shape the flow below describes, never a way to skip the factor.
type loginRequest struct {
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
	TOTPCode   string `json:"totp_code,omitempty"`
}

// secondFactorProblemMember is the Problem extension member a login refusal
// carries when — and only when — the presented password was accepted and the
// principal holds a second-factor credential the request did not satisfy. The
// value names the factor to collect.
//
// It is an extension member on the existing UNAUTHENTICATED refusal rather than
// a new error code, deliberately: api/1's `code` registry is closed and shared,
// and this condition is not a new KIND of refusal — the caller is still simply
// not authenticated. It sits beside `errors` (VALIDATION_FAILED) and
// `current_revision` (REVISION_CONFLICT) as a per-code member, which is the
// pattern the Problem schema already establishes.
const secondFactorProblemMember = "second_factor"

// secondFactorTOTP is the only value secondFactorProblemMember takes today.
const secondFactorTOTP = "totp"

// Login exchanges an identifier, a password, and — for a principal holding a
// `totp` credential — a second-factor code, for a session (API-090/091, SEC-004).
//
// Every failure path — unknown identifier, wrong password, wrong code, replayed
// code, locked credential — returns the SAME generic detail, so the response
// BODY cannot be used to enumerate which identifiers exist or to tell a wrong
// password from a wrong code. Only the machine-readable `code` differs, and only
// where the contract requires it to (CREDENTIAL_LOCKED, SEC-090), because a
// caller that is locked out needs to know to back off rather than to keep
// retrying a credential it may well have right.
//
// # The claim covers the elapsed time, not only the bytes
//
// An identical body is worth nothing if the two refusals take visibly different
// amounts of time to produce, and the natural implementation makes them: an
// identifier resolving to no credential has nothing to verify against, so it
// short-circuits before Argon2id and answers in microseconds where a wrong
// password pays the full KDF. That gap enumerates identifiers over a network as
// reliably as a distinguishable body would.
//
// So an unresolved identifier is verified against DummyPasswordHash instead —
// same Argon2id parameters, no password that satisfies it — and the KDF runs on
// both paths. Measured over this handler (min of 7 interleaved rounds, in-process):
// a wrong password refuses in 32.3ms and an unknown identifier in 32.3ms, a
// ratio of 1.00x, where the short-circuiting version answered the unknown
// identifier in 71µs — 453x sooner. TestLoginTimingDoesNotDiscloseWhetherAnIdentifierExists
// re-measures both paths and fails the build if they drift apart, so this is an
// assertion rather than a claim maintained by hand.
//
// A locked credential does still refuse sooner, and visibly so — but it also
// answers with a different status and code on purpose (SEC-090), so its timing
// discloses nothing its body has not already said, and refusing before the KDF
// is the entire point of a lockout.
//
// # The shape of the second factor, and why there is no intermediate state
//
// The flow is ONE operation the client may call twice, not two operations. A
// login that satisfies both factors carries both in one body; a login whose
// principal needs a factor the body did not carry is REFUSED, with the Problem
// naming the factor to collect, and the client calls again with everything.
//
// The alternative — mint something after the password and exchange it for a
// session after the code — was rejected on the requirement it would violate.
// Whatever that intermediate thing is, it is a bearer credential issued on one
// factor: a value that exists, that the client holds, and that is one server bug
// (a missing AAL check, a route that forgot to look) away from being a session.
// Here nothing is minted until both factors are satisfied, so there is no state
// in which partial authentication has been persisted at all — the property is
// structural rather than enforced.
//
// The cost is that a two-step client verifies the password twice, paying
// Argon2id twice. On an appliance-class box verifying one interactive login,
// that is affordable; a credential the server has to keep valid between the two
// calls is not.
//
// Refusing for a missing factor deliberately does NOT count against the
// credential's lockout budget (SEC-090). Nothing failed — the request was
// incomplete — and counting it would burn a real user's budget on the ordinary
// path every single time they sign in.
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)
	if r.Method != http.MethodPost {
		apihttp.WriteProblem(w, r, traceID, http.StatusMethodNotAllowed, "VALIDATION_FAILED", "Method Not Allowed")
		return
	}
	var req loginRequest
	if !decodeBody(w, r, traceID, &req) {
		return
	}
	var fieldErrs []fieldError
	if req.Identifier == "" {
		fieldErrs = append(fieldErrs, fieldError{"identifier", "required", "an identifier is required"})
	}
	if req.Password == "" {
		fieldErrs = append(fieldErrs, fieldError{"password", "required", "a password is required"})
	}
	if len(fieldErrs) > 0 {
		// API-013a: a body-level validation failure is 422, never 400.
		writeValidationProblem(w, r, traceID, fieldErrs)
		return
	}

	ctx := r.Context()
	store := h.auth.store
	ipClass := RequestIPClass(r)

	cred, credErr := store.FindPasswordCredential(ctx, req.Identifier)
	// SEC-090 scopes lockout per (credential, source-IP class). Where the
	// identifier resolves to no credential there is nothing to key on, so the
	// key falls back to a hash of what was submitted — which keeps an
	// enumeration sweep against non-existent accounts under the same budget
	// without ever recording the guesses themselves.
	credRef := cred.CredentialID
	actor := cred.PrincipalID
	if credErr != nil {
		credRef = IdentifierRef(req.Identifier)
		actor = UnresolvedActorPrincipal
	}
	key := LockoutKey(credRef, ipClass)

	if locked, retryMs := h.auth.lockout.Locked(key, store.Now()); locked {
		h.auth.auditor.Emit(Record{
			TraceID: traceID, Actor: actor, Action: ActionLoginLockout,
			Target: "credential:" + credRef, Result: "failure",
		})
		// Retry-After is in seconds, rounded UP so a client that honours it
		// exactly does not return one tick early and burn another attempt.
		w.Header().Set("Retry-After", strconv.FormatInt((retryMs+999)/1000, 10))
		apihttp.WriteProblemExt(w, r, traceID, http.StatusTooManyRequests,
			"CREDENTIAL_LOCKED", "Too Many Requests",
			"Too many failed attempts for this credential from this source; retry after the stated backoff.", nil)
		return
	}

	// The password is verified only after the lockout check, so a locked
	// credential costs no Argon2id work — the lock exists to stop the work, not
	// merely to discard its result.
	//
	// An identifier that resolved to NO credential is verified against
	// DummyPasswordHash, which carries the same Argon2id parameters a real
	// credential's hash does. That is what makes the two paths cost the same: the
	// KDF runs whether or not the identifier exists, so elapsed time discloses
	// nothing the response body does not (see this handler's own doc).
	//
	// The dummy verification's result is CONSUMED (it gates passwordOK, which
	// gates the refusal below), not discarded into a blank identifier, so no
	// compiler is free to elide the work. It cannot authenticate anything either:
	// credErr is an independent conjunct, so a login whose identifier resolved to
	// nothing fails even in the impossible event that some password derives to the
	// dummy's random key.
	secret := cred.Secret
	if credErr != nil {
		secret = DummyPasswordHash
	}
	passwordOK := VerifyPassword(secret, req.Password) == nil
	authOK := credErr == nil && passwordOK
	if !authOK {
		h.auth.lockout.Fail(key, store.Now())
		h.auth.auditor.Emit(Record{
			TraceID: traceID, Actor: actor, Action: ActionLoginFailure,
			Target: "credential:" + credRef, Result: "failure",
		})
		apihttp.WriteProblemExt(w, r, traceID, http.StatusUnauthorized,
			"UNAUTHENTICATED", "Unauthorized", "The identifier or password is incorrect.", nil)
		return
	}

	// SEC-004's floor. The password has verified; if this principal holds a
	// `totp` credential, the login is not complete until that credential is
	// satisfied too, and no session exists until it is.
	if !h.secondFactorSatisfied(w, r, traceID, cred.PrincipalID, req.TOTPCode, ipClass) {
		return
	}

	h.auth.lockout.Succeed(key)
	minted, err := store.MintSession(ctx, cred.PrincipalID, TokenKindSession, "", AALStandard, map[string]string{
		"user_agent": r.UserAgent(),
		"ip_class":   ipClass,
	})
	if err != nil {
		writeInternal(w, r, traceID)
		return
	}
	_ = store.TouchCredential(ctx, cred.CredentialID)

	bindings, err := store.RoleBindings(ctx, cred.PrincipalID)
	if err != nil {
		writeInternal(w, r, traceID)
		return
	}
	role, _ := Effective(bindings)
	principal, err := store.GetPrincipal(ctx, cred.PrincipalID)
	if err != nil {
		writeInternal(w, r, traceID)
		return
	}

	// EVT-081 names BOTH "authentication events" and "session issuance" on its
	// mandatory-emission list; they are two distinct facts about one request
	// (who proved who they were, and what durable credential that produced), so
	// both are recorded rather than one standing in for the other.
	h.auth.auditor.Success(traceID, cred.PrincipalID, ActionLoginSuccess, "credential:"+cred.CredentialID)
	h.auth.auditor.Success(traceID, cred.PrincipalID, ActionSessionIssued, "session:"+minted.Session.SessionID)

	SetSessionCookies(w, r, minted)
	writeJSON(w, http.StatusOK, sessionResponse{
		PrincipalID: principal.PrincipalID,
		Kind:        principal.Kind,
		Role:        role,
		AAL:         minted.Session.AAL,
		SessionID:   minted.Session.SessionID,
		CSRFToken:   minted.CSRFToken,
	})
}

// Logout revokes the calling session and clears its cookies. It is mounted
// BEHIND the middleware, so it can only be reached with a live session and (for
// a browser) a valid CSRF token — a cross-site page cannot log a user out.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)
	p, err := RequirePrincipal(r.Context())
	if err != nil {
		writeInternal(w, r, traceID)
		return
	}
	if err := h.auth.store.RevokeSession(r.Context(), p.SessionID); err != nil && !errors.Is(err, ErrSessionNotFound) {
		writeInternal(w, r, traceID)
		return
	}
	action := ActionSessionRevoked
	if p.TokenKind == TokenKindAPIKey {
		action = ActionAPIKeyRevoked
	}
	h.auth.auditor.Success(traceID, p.ID, action, "session:"+p.SessionID)
	ClearSessionCookies(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// Session returns the calling principal's own session summary — what the
// console reads on load to decide whether to render the app or the login page.
func (h *Handlers) Session(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)
	p, err := RequirePrincipal(r.Context())
	if err != nil {
		writeInternal(w, r, traceID)
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{
		PrincipalID: p.ID,
		Kind:        p.Kind,
		Role:        p.Role,
		AAL:         p.AAL,
		SessionID:   p.SessionID,
	})
}

// claimRequest is POST /auth/setup's body: the one-time setup code the installer
// generated (SEC-120) plus the credential the first owner is choosing.
type claimRequest struct {
	Code       string `json:"code"`
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
}

// Claim is the first-boot setup endpoint (SEC-120): "The installer MUST
// auto-generate a one-time `setup`-purpose grant at install time and present it
// printed, as a QR code, or on-screen; the setup endpoint MUST be claimable only
// by redeeming this grant. An installed-but-unclaimed box MUST NOT be
// first-come-first-served to whoever reaches its setup endpoint first on a shared
// network."
//
// That last sentence is the requirement this handler exists to satisfy, and the
// reason there is no "no owner yet, so anyone may claim" branch anywhere below:
// possession of the code, not arrival order, is what claims the box. A caller
// presenting no code or a wrong code is refused exactly as it would be after the
// box was claimed.
//
// The whole claim — consuming the grant, creating the principal, storing its
// credential, and binding it `owner` — happens in ONE transaction (SEC-036), so
// two racing claims cannot both produce an owner, and a claim whose credential
// write failed cannot burn the code.
func (h *Handlers) Claim(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)
	if r.Method != http.MethodPost {
		apihttp.WriteProblem(w, r, traceID, http.StatusMethodNotAllowed, "VALIDATION_FAILED", "Method Not Allowed")
		return
	}
	var req claimRequest
	if !decodeBody(w, r, traceID, &req) {
		return
	}
	var fieldErrs []fieldError
	if req.Code == "" {
		fieldErrs = append(fieldErrs, fieldError{"code", "required", "the one-time setup code is required"})
	}
	if req.Identifier == "" {
		fieldErrs = append(fieldErrs, fieldError{"identifier", "required", "an identifier is required"})
	}
	if req.Password == "" {
		fieldErrs = append(fieldErrs, fieldError{"password", "required", "a password is required"})
	}
	if len(fieldErrs) > 0 {
		writeValidationProblem(w, r, traceID, fieldErrs)
		return
	}

	ctx := r.Context()
	store := h.auth.store

	// SEC-033's attempt budget, enforced BEFORE the lookup: the control bounds
	// guesses at a live code, so it must refuse without checking the guess.
	if !h.budget.Allow(PurposeSetup, RequestIPClass(r), store.Now()) {
		apihttp.WriteProblemExt(w, r, traceID, http.StatusTooManyRequests,
			"RATE_LIMITED", "Too Many Requests",
			"Too many setup-code redemption attempts from this source; retry after a backoff.", nil)
		return
	}

	var (
		principal PrincipalRow
		granted   Role
	)
	_, err := store.RedeemGrant(ctx, req.Code, PurposeSetup, func(tx *sql.Tx, g GrantRow) error {
		p, err := store.CreatePrincipalTx(ctx, tx, g.ResultingPrincipalKind, req.Identifier)
		if err != nil {
			return err
		}
		if _, err := store.PutPasswordCredentialTx(ctx, tx, p.PrincipalID, req.Identifier, req.Password); err != nil {
			return err
		}
		role := g.Role
		if role == "" {
			role = RoleOwner
		}
		scope := g.ScopeNode
		if scope == "" {
			scope = h.claimScopeNode
		}
		if _, err := store.PutRoleBindingTx(ctx, tx, p.PrincipalID, scope, role); err != nil {
			return err
		}
		principal, granted = p, role
		return nil
	},
		// SEC-034's record is RedeemGrant's own. The owner principal this claim
		// creates does not exist until the consume function above has run, so the
		// actor is supplied as a closure the store resolves after commit rather
		// than as a value that would still be empty here.
		RedeemedBy(func() string { return principal.PrincipalID }),
		RedeemTraceID(traceID))
	if err != nil {
		h.writeGrantProblem(w, r, traceID, err, "The setup code is not valid.")
		return
	}

	h.auth.auditor.Success(traceID, principal.PrincipalID, ActionPrincipalCreate, "principal:"+principal.PrincipalID)
	h.auth.auditor.Success(traceID, principal.PrincipalID, ActionSetupClaimed, "principal:"+principal.PrincipalID)

	minted, err := store.MintSession(ctx, principal.PrincipalID, TokenKindSession, "", AALStandard, map[string]string{
		"user_agent": r.UserAgent(),
		"ip_class":   RequestIPClass(r),
	})
	if err != nil {
		writeInternal(w, r, traceID)
		return
	}
	h.auth.auditor.Success(traceID, principal.PrincipalID, ActionSessionIssued, "session:"+minted.Session.SessionID)

	SetSessionCookies(w, r, minted)
	writeJSON(w, http.StatusCreated, sessionResponse{
		PrincipalID: principal.PrincipalID,
		Kind:        principal.Kind,
		Role:        granted,
		AAL:         minted.Session.AAL,
		SessionID:   minted.Session.SessionID,
		CSRFToken:   minted.CSRFToken,
	})
}

// credentialResetRequest is POST /auth/credential-reset's body: WHO is being
// reset, and nothing else that could carry a value.
//
// The absence is the requirement. SEC-050: the issuing admin "MUST NOT be shown,
// and MUST have no path to choose, the credential value the target user
// eventually sets." This struct is the whole of what the wire accepts on the
// issuance side, so there is no field to send one in — a request that includes
// a password field is not refused, it is simply not parsed into anything.
type credentialResetRequest struct {
	TargetPrincipalID string `json:"target_principal_id"`
	// KeepExistingSessions is SEC-053's per-issuance opt-out from the
	// revoke-everything default.
	KeepExistingSessions bool `json:"keep_existing_sessions,omitempty"`
}

// credentialResetResponse is what the issuing admin receives (SEC-050's "a
// one-time code or URL for the issuing admin to hand to the target user").
//
// # Why there is no `url`, only a `code`
//
// A URL would have to name a host, and the only host this handler could learn
// from the request is the `Host` header — which the CLIENT sends. An admin
// tricked into issuing a reset through a request carrying `Host:
// attacker.example` would be handed a link that walks the target to the
// attacker's origin with a LIVE one-time code in the query string, and would
// hand it over believing it came from their own box. That is a credential
// handoff to a third party produced by the platform itself.
//
// So this surface returns the code alone, which SEC-050 admits in as many words
// ("a one-time code or URL"), and the URL form stays on the console binding
// where the operator supplies the base themselves and no header is involved.
type credentialResetResponse struct {
	GrantID                     string `json:"grant_id"`
	Code                        string `json:"code"`
	TargetPrincipalID           string `json:"target_principal_id"`
	ExpiresAtMs                 int64  `json:"expires_at_ms"`
	SessionsRevokedOnRedemption bool   `json:"sessions_revoked_on_redemption"`
}

// IssueCredentialReset is SEC-050's issuance over api/1: an authenticated
// `admin` mints a `credential-reset` grant for a `user` principal and receives
// the one-time code to hand over.
//
// It is an AUTHENTICATED operation, mounted behind the middleware like any other
// mutating route — the person issuing a reset is not the person who lost their
// credential, so nothing here is circular and nothing needs a `security: []`
// override. SEC-012's issuer restriction (`admin` or above, anywhere in the
// tree) is enforced by the store, in the one function that mints the grant,
// rather than a second time here: a check duplicated at the handler is a check
// that can be forgotten by the next surface that calls the same store function.
func (h *Handlers) IssueCredentialReset(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)
	p, err := RequirePrincipal(r.Context())
	if err != nil {
		writeInternal(w, r, traceID)
		return
	}
	var req credentialResetRequest
	if !decodeBody(w, r, traceID, &req) {
		return
	}
	if req.TargetPrincipalID == "" {
		writeValidationProblem(w, r, traceID, []fieldError{
			{"target_principal_id", "required", "the principal whose credential is being reset is required"}})
		return
	}

	handoff, err := h.auth.store.IssueCredentialResetGrant(r.Context(), p.ID, req.TargetPrincipalID,
		CredentialResetOptions{KeepExistingSessions: req.KeepExistingSessions, TraceID: traceID})
	switch {
	case err == nil:
	case errors.Is(err, ErrResetNotAdmin):
		// EVT-083: a refused issuance is exactly as auditable as an accepted one.
		// The record names the ATTEMPT — a non-admin trying to mint a reset for
		// somebody else is the event an operator most wants to find later — and it
		// is emitted here rather than by the store, which never saw a grant.
		h.auth.auditor.Failure(traceID, p.ID, ActionGrantCreated, "principal:"+req.TargetPrincipalID)
		apihttp.WriteProblemExt(w, r, traceID, http.StatusForbidden,
			"FORBIDDEN", "Forbidden", "Issuing a credential-reset grant requires at least the `admin` role.", nil)
		return
	case errors.Is(err, ErrPrincipalNotFound), errors.Is(err, ErrResetIdentifierUnknown):
		// One answer for "no such principal" and "that principal holds no password
		// credential", deliberately: an admin can list principals through the api
		// anyway, so this is not a disclosure boundary — it is that both mean the
		// same thing to the caller, and branching would make this route a probe for
		// which principals hold password credentials.
		apihttp.WriteProblemExt(w, r, traceID, http.StatusNotFound,
			"NOT_FOUND", "Not Found", "No such principal holds a password credential to reset.", nil)
		return
	case errors.Is(err, ErrResetTargetNotUser):
		writeValidationProblem(w, r, traceID, []fieldError{
			{"target_principal_id", "invalid", "a credential-reset grant targets a `user` principal"}})
		return
	default:
		writeInternal(w, r, traceID)
		return
	}

	writeJSON(w, http.StatusCreated, credentialResetResponse{
		GrantID:                     handoff.GrantID,
		Code:                        handoff.Code,
		TargetPrincipalID:           handoff.TargetPrincipalID,
		ExpiresAtMs:                 handoff.ExpiresAtMs,
		SessionsRevokedOnRedemption: !req.KeepExistingSessions,
	})
}

// credentialResetRedeemRequest is POST /auth/credential-reset/redeem's body: the
// one-time code the target was handed, and the credential value THEY choose.
type credentialResetRedeemRequest struct {
	Code     string `json:"code"`
	Password string `json:"password"`
}

// RedeemCredentialReset is SEC-050's redemption half, and the only point in the
// whole flow where a credential value is accepted.
//
// # Why this one is unauthenticated
//
// The caller is a person who cannot log in — that is the entire premise of a
// credential reset — so requiring a session would make the operation
// unreachable by the only party SEC-050 permits to perform it. It is
// `security: []` for the same reason API-091 makes login and first-boot claim so:
// its whole purpose is to serve a caller holding no session.
//
// What stands in for a session is the code itself, and three things bound what
// that admits:
//
//   - 256 bits of crypto/rand entropy in the code (SEC-032 floors it at 128),
//     stored only as a hash, so the database cannot yield a redeemable code;
//   - SEC-033's attempt budget, checked BEFORE the code is looked up, so a
//     guessing sweep is refused without the lookups it is trying to make;
//   - SEC-036's atomic check-and-consume with a ~15-minute ttl (SEC-032).
//
// # Why it does not return a session
//
// The obvious convenience — sign the target in, they just proved possession —
// would walk straight past their second factor. SEC-052 makes a credential-reset
// grant explicitly NOT authorize a TOTP change, so a principal holding a `totp`
// credential still holds it after this call; minting a session here would hand
// out an authenticated session for that principal without the factor ever being
// presented, turning "reset my password" into "bypass 2FA". So this returns 204
// and the target logs in through the front door, factor included.
func (h *Handlers) RedeemCredentialReset(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)
	if r.Method != http.MethodPost {
		apihttp.WriteProblem(w, r, traceID, http.StatusMethodNotAllowed, "VALIDATION_FAILED", "Method Not Allowed")
		return
	}
	var req credentialResetRedeemRequest
	if !decodeBody(w, r, traceID, &req) {
		return
	}
	var fieldErrs []fieldError
	if req.Code == "" {
		fieldErrs = append(fieldErrs, fieldError{"code", "required", "the one-time reset code is required"})
	}
	if req.Password == "" {
		fieldErrs = append(fieldErrs, fieldError{"password", "required", "a new password is required"})
	}
	if len(fieldErrs) > 0 {
		writeValidationProblem(w, r, traceID, fieldErrs)
		return
	}

	store := h.auth.store
	// SEC-033, enforced BEFORE the lookup — "the attack this bounds" is guessing
	// a code that already exists, so the budget must refuse without checking the
	// guess. Keyed on this purpose, so a sweep against reset codes cannot be
	// masked by ordinary setup-code traffic and vice versa.
	if !h.budget.Allow(PurposeCredentialReset, RequestIPClass(r), store.Now()) {
		apihttp.WriteProblemExt(w, r, traceID, http.StatusTooManyRequests,
			"RATE_LIMITED", "Too Many Requests",
			"Too many reset-code redemption attempts from this source; retry after a backoff.", nil)
		return
	}

	if _, err := store.RedeemCredentialResetGrant(r.Context(), req.Code, req.Password, traceID); err != nil {
		h.writeGrantProblem(w, r, traceID, err, "The reset code is not valid.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeGrantProblem maps a redemption failure onto its security-model/1 error
// code (SEC-035). ErrGrantNotFound has no code of its own in that taxonomy — the
// contract only names expired / already-redeemed / purpose-mismatch — so an
// unknown code is refused as UNAUTHENTICATED, which is both accurate (no valid
// principal was presented) and non-disclosing (it does not confirm that some
// other code would have worked).
//
// unknownDetail is the human-readable line for that last case, supplied by the
// caller because it is the ONE branch whose wording names the kind of code the
// endpoint takes ("setup code" / "reset code"); every other branch's detail is
// about the grant lifecycle and is identical on every redemption endpoint.
func (h *Handlers) writeGrantProblem(w http.ResponseWriter, r *http.Request, traceID string, err error, unknownDetail string) {
	switch {
	case errors.Is(err, ErrGrantExpired):
		apihttp.WriteProblemExt(w, r, traceID, http.StatusForbidden,
			"GRANT_EXPIRED", "Forbidden", "This grant's ttl elapsed before redemption; a fresh grant is required.", nil)
	case errors.Is(err, ErrGrantAlreadyRedeemed):
		apihttp.WriteProblemExt(w, r, traceID, http.StatusForbidden,
			"GRANT_ALREADY_REDEEMED", "Forbidden", "This one-time grant's code has already been redeemed.", nil)
	case errors.Is(err, ErrGrantPurposeMismatch):
		apihttp.WriteProblemExt(w, r, traceID, http.StatusForbidden,
			"GRANT_PURPOSE_MISMATCH", "Forbidden", "This grant's purpose does not match the endpoint it was presented to.", nil)
	case errors.Is(err, ErrGrantNotFound):
		apihttp.WriteProblemExt(w, r, traceID, http.StatusUnauthorized,
			"UNAUTHENTICATED", "Unauthorized", unknownDetail, nil)
	default:
		writeInternal(w, r, traceID)
	}
}

// ---- small shared helpers -------------------------------------------------

type fieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// decodeBody reads and decodes a JSON request body, writing the api/1 Problem
// and reporting false on failure. A malformed body is a BODY validation failure,
// so it is 422 per API-013a — not the 400 a query-parameter failure carries.
func decodeBody(w http.ResponseWriter, r *http.Request, traceID string, v any) bool {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeValidationProblem(w, r, traceID, []fieldError{{"body", "unreadable", "the request body could not be read"}})
		return false
	}
	if err := json.Unmarshal(raw, v); err != nil {
		writeValidationProblem(w, r, traceID, []fieldError{{"body", "malformed", "the request body is not a JSON object of the expected shape"}})
		return false
	}
	return true
}

// writeValidationProblem writes API-013's multi-field VALIDATION_FAILED Problem
// at API-013a's body status (422).
func writeValidationProblem(w http.ResponseWriter, r *http.Request, traceID string, errs []fieldError) {
	apihttp.WriteProblemExt(w, r, traceID, http.StatusUnprocessableEntity,
		"VALIDATION_FAILED", "Validation Failed", "One or more fields failed validation.",
		map[string]any{"errors": errs})
}

func writeInternal(w http.ResponseWriter, r *http.Request, traceID string) {
	apihttp.WriteProblemExt(w, r, traceID, http.StatusInternalServerError,
		"INTERNAL", "Internal Server Error", "An unexpected server error occurred.", nil)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
