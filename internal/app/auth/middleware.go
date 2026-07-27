package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// SessionCookieName is the cookie `api/openapi.yaml`'s SessionCookie security
// scheme publishes: `session`.
const SessionCookieName = "session"

// CSRFCookieName is the companion, deliberately NON-HttpOnly cookie carrying the
// double-submit CSRF token (SEC-024). The page's own JavaScript must be able to
// read it in order to echo it back in CSRFHeaderName — that readability is the
// mechanism, not a weakness: an attacker's cross-site page can cause the browser
// to SEND this cookie but cannot READ it, which is exactly the asymmetry
// double-submit exploits.
const CSRFCookieName = "csrf_token"

// CSRFHeaderName is the request header a mutating, cookie-authenticated request
// echoes the CSRF token in.
const CSRFHeaderName = "X-CSRF-Token"

// CodeSet is the per-binding error vocabulary the middleware writes refusals in.
// api/1 and events/1 name the same two conditions differently — api/1 has
// UNAUTHENTICATED/FORBIDDEN (Error taxonomy), events/1 has AUTH_REQUIRED
// (EVT-113) — and each binding MUST use its own contract's registry (API-011),
// so the vocabulary is a parameter rather than a hardcoded string.
type CodeSet struct {
	Unauthenticated string
	Forbidden       string
}

// APICodes is api/1's vocabulary (Error taxonomy).
var APICodes = CodeSet{Unauthenticated: "UNAUTHENTICATED", Forbidden: "FORBIDDEN"}

// EventsCodes is events/1's vocabulary. EVT-113 names AUTH_REQUIRED for a
// failed authentication on either the SSE or WS binding; events/1 defines no
// separate authorization code, so an authorization refusal on that binding
// reuses api/1's FORBIDDEN by name — the same reuse-by-name discipline
// player/1 PLY-007 applies to a whole Problem's top-level code.
var EventsCodes = CodeSet{Unauthenticated: "AUTH_REQUIRED", Forbidden: "FORBIDDEN"}

// credentialQueryParams are the parameter names a caller might be tempted to
// carry a bearer credential in. EVT-112: "An API key or session credential MUST
// NOT be accepted as a query-string parameter on either binding — a token that
// could ride a URL is a token that leaks into server access logs and
// intermediate proxies." This package never READS a credential from the query,
// which already satisfies "not accepted"; the events binding additionally
// REFUSES a request carrying one, so a client doing it wrong is told rather than
// left silently unauthenticated with its token already written to the access log.
var credentialQueryParams = []string{"token", "api_key", "apikey", "access_token", "session", "authorization"}

// Authenticator resolves a request's principal from the presented credential and
// refuses everything it cannot resolve. It is the single implementation both the
// api/1 and events/1 bindings mount, so the two cannot drift on what counts as
// authenticated.
type Authenticator struct {
	store    *Store
	auditor  *Auditor
	lockout  *Lockout
	sessions *Revocations
}

// NewAuthenticator returns an Authenticator over store. auditor and sessions may
// be nil (a unit test with no event sink and no live streams); lockout is
// created on demand when nil.
func NewAuthenticator(store *Store, auditor *Auditor, lockout *Lockout, sessions *Revocations) *Authenticator {
	if lockout == nil {
		lockout = NewDefaultLockout()
	}
	return &Authenticator{store: store, auditor: auditor, lockout: lockout, sessions: sessions}
}

// Store exposes the underlying store to the handlers built on this
// authenticator.
func (a *Authenticator) Store() *Store { return a.store }

// Revocations exposes the live-stream revocation registry, or nil.
func (a *Authenticator) Revocations() *Revocations { return a.sessions }

// Auditor exposes the Auditor this Authenticator emits its own security-model/1
// records through, so a surface mounted BEHIND this middleware audits through
// the same sink rather than being handed a second one to keep in step.
//
// That sharing is the point rather than a convenience. SEC-150 requires every
// audited flow to produce "an ordinary events/1 audit.event" and adds "no second
// audit schema"; giving a downstream surface its own sink is how a deployment
// ends up with two audit trails that disagree about which events exist. Reaching
// the one already wired here also means an audited surface inherits its sink
// from a collaborator it CANNOT be constructed without, so there is no separate
// wiring step to forget. May be nil (an Auditor is nil-safe and silent).
func (a *Authenticator) Auditor() *Auditor { return a.auditor }

// Middleware returns an http.Handler that authenticates and authorizes every
// request before next sees it, refusing in codes' vocabulary. exempt, when
// non-nil, reports the requests that carry their own `security: []` override
// (API-090) — a credential-exchange or unauthenticated-intake operation, which
// by definition cannot be required to present a pre-existing session (API-091).
//
// The order of checks is deliberate and is the order the contract's own
// refusals fall in:
//
//  1. No credential presented at all → 401. (SEC-005: refuse, never
//     default-permit; there is no anonymous principal to fall back to.)
//  2. Credential presented but naming no live session → 401. A revoked token and
//     a never-issued token are indistinguishable here on purpose.
//  3. Session live, but its principal holds no role binding anywhere → 403.
//     Authenticated is not authorized (SEC-005/010).
//  4. Role insufficient for the method's floor → 403 (SEC-010, coarse; see
//     role.go's Effective for why the fine matrix is deferred).
//  5. Cookie-authenticated mutating request without a matching double-submit
//     CSRF token → 403 (SEC-024 — SameSite alone is explicitly not sufficient).
func (a *Authenticator) Middleware(codes CodeSet, exempt func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt != nil && exempt(r) {
				next.ServeHTTP(w, r)
				return
			}
			p, ok := a.Authenticate(w, r, codes)
			if !ok {
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

// Authenticate resolves r's principal, writing the refusal Problem and reporting
// ok=false when it cannot. A caller that gets ok=true MUST put the returned
// principal on the request context (Middleware does this) so downstream handlers
// read one resolved identity rather than re-deriving it.
func (a *Authenticator) Authenticate(w http.ResponseWriter, r *http.Request, codes CodeSet) (Principal, bool) {
	traceID := apihttp.TraceID(r)

	// EVT-112: a credential riding the query string is refused outright rather
	// than merely ignored — by the time it is ignored it is already in the
	// access log.
	if q := r.URL.Query(); len(q) > 0 {
		for _, name := range credentialQueryParams {
			if q.Has(name) {
				a.refuse(w, r, traceID, http.StatusUnauthorized, codes.Unauthenticated,
					"A credential MUST NOT be presented as a query-string parameter; use the session cookie or an Authorization: Bearer header.")
				return Principal{}, false
			}
		}
	}

	token, viaCookie, ok := presentedToken(r)
	if !ok {
		a.refuse(w, r, traceID, http.StatusUnauthorized, codes.Unauthenticated,
			"No session cookie or Authorization: Bearer credential was presented.")
		return Principal{}, false
	}

	session, err := a.store.LookupSession(r.Context(), token)
	if err != nil {
		a.refuse(w, r, traceID, http.StatusUnauthorized, codes.Unauthenticated,
			"The presented credential does not name a live session.")
		return Principal{}, false
	}

	principal, err := a.store.GetPrincipal(r.Context(), session.PrincipalID)
	if err != nil {
		// A session whose principal has vanished is not authenticable. This is
		// a fail-closed branch, not an expected one.
		a.refuse(w, r, traceID, http.StatusUnauthorized, codes.Unauthenticated,
			"The presented credential does not name a live session.")
		return Principal{}, false
	}

	bindings, err := a.store.RoleBindings(r.Context(), session.PrincipalID)
	if err != nil {
		apihttp.WriteProblemExt(w, r, traceID, http.StatusInternalServerError,
			"INTERNAL", "Internal Server Error", "An unexpected server error occurred.", nil)
		return Principal{}, false
	}
	role, bound := Effective(bindings)
	if !bound {
		// SEC-005's central rule: an authorization decision that cannot be
		// resolved REFUSES. A principal with no binding is authenticated and
		// authorized for nothing.
		a.refuse(w, r, traceID, http.StatusForbidden, codes.Forbidden,
			"This principal holds no role binding at any scope node.")
		return Principal{}, false
	}
	if !role.AtLeast(RequiredRole(r.Method)) {
		a.refuse(w, r, traceID, http.StatusForbidden, codes.Forbidden,
			"This principal's role does not permit this operation.")
		return Principal{}, false
	}

	// SEC-024: a mutating route reachable from a BROWSER SESSION needs the
	// double-submit token as well as SameSite. A bearer API key is not ambient
	// authority — a cross-site page cannot cause a browser to attach an
	// Authorization header it does not know — so the check is scoped to
	// cookie-authenticated requests rather than applied blanket, which would
	// only make the CLI carry a token it gains nothing from.
	if viaCookie && isMutatingMethod(r.Method) {
		if !csrfOK(r, session.CSRFToken) {
			a.refuse(w, r, traceID, http.StatusForbidden, codes.Forbidden,
				"A mutating request authenticated by the session cookie must echo the double-submit CSRF token in the "+CSRFHeaderName+" header.")
			return Principal{}, false
		}
	}

	return Principal{
		ID:        principal.PrincipalID,
		Kind:      principal.Kind,
		Role:      role,
		AAL:       session.AAL,
		SessionID: session.SessionID,
		TokenKind: session.TokenKind,
		// The very bindings this admission decision was made on, carried forward
		// so a handler's per-row visibility check (auth.CanRead) resolves against
		// the same set rather than re-reading one that may since have changed.
		Bindings: bindings,
	}, true
}

// refuse writes an api/1-shaped Problem (API-010) in the binding's own
// vocabulary. EVT-113 requires exactly this on the events bindings too — "an
// HTTP error response in api/1's Problem shape ... never with a WS/SSE-level
// frame, since no session has been established yet to frame one over" — which is
// why there is one refusal path rather than a per-binding one.
func (a *Authenticator) refuse(w http.ResponseWriter, r *http.Request, traceID string, status int, code, detail string) {
	title := "Unauthorized"
	if status == http.StatusForbidden {
		title = "Forbidden"
	}
	apihttp.WriteProblemExt(w, r, traceID, status, code, title, detail, nil)
}

// presentedToken extracts the request's bearer credential: an `Authorization:
// Bearer <api-key>` header first (EVT-110/111's non-browser form), else the
// session cookie (the browser form, carried automatically same-origin). viaCookie
// reports which, since only the cookie form is ambient authority a CSRF check
// applies to.
//
// The query string is deliberately not consulted (EVT-112).
func presentedToken(r *http.Request) (token string, viaCookie, ok bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return strings.TrimSpace(h[len(prefix):]), false, true
		}
		// An Authorization header present but not Bearer-shaped is a failed
		// credential presentation, not an absent one — falling through to the
		// cookie would silently authenticate a different identity than the
		// caller asked for.
		return "", false, false
	}
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return "", false, false
	}
	return c.Value, true, true
}

// csrfOK performs the double-submit comparison in constant time. An empty stored
// token never matches, so a session minted without one (an API key) can never
// pass this check by presenting an empty header.
func csrfOK(r *http.Request, want string) bool {
	if want == "" {
		return false
	}
	got := r.Header.Get(CSRFHeaderName)
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// SetSessionCookies writes the session and CSRF cookies for a freshly minted
// browser session.
//
// SEC-023 is the load-bearing detail: the cookie is issued HOST-ONLY — no
// `Domain` attribute at all. A cookie with `Domain=example.com` is sent to every
// subdomain of it; omitting the attribute entirely restricts it to the exact
// host that set it, which is the precondition `surface/1` SUR-062 depends on and
// states but does not itself impose. There is deliberately no configuration knob
// here: a deployment that could widen the cookie's domain would be a deployment
// that could violate SEC-023.
//
// `secure` is set when the request arrived over TLS. It is not unconditional
// because the same binary serves a plain-HTTP loopback dev stack, where a Secure
// cookie is silently dropped by the browser and the session simply never sticks
// — the exact failure mode a Secure-always default produces on first contact.
func SetSessionCookies(w http.ResponseWriter, r *http.Request, m Minted) {
	secure := r.TLS != nil
	http.SetCookie(w, &http.Cookie{
		Name:  SessionCookieName,
		Value: m.Token,
		Path:  "/",
		// No Domain: host-only (SEC-023).
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:  CSRFCookieName,
		Value: m.CSRFToken,
		Path:  "/",
		// No Domain: host-only (SEC-023).
		// Not HttpOnly, by design — see CSRFCookieName's doc.
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookies expires both cookies on logout. It mirrors
// SetSessionCookies' attributes exactly (path, host-only, SameSite) because a
// browser only replaces a cookie whose name/path/domain triple matches.
func ClearSessionCookies(w http.ResponseWriter, r *http.Request) {
	secure := r.TLS != nil
	for _, name := range []string{SessionCookieName, CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			HttpOnly: name == SessionCookieName,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})
	}
}

// ErrNoPrincipal is returned by RequirePrincipal when a handler that must have
// an authenticated caller was reached without one — a wiring bug (a route
// mounted outside the middleware), never a client condition.
var ErrNoPrincipal = errors.New("auth: request carries no authenticated principal")

// RequirePrincipal returns ctx's principal or ErrNoPrincipal. It exists so a
// handler that depends on an authenticated identity fails loudly when mounted
// wrong, instead of quietly attributing the request to the empty string.
func RequirePrincipal(ctx context.Context) (Principal, error) {
	p, ok := FromContext(ctx)
	if !ok {
		return Principal{}, ErrNoPrincipal
	}
	return p, nil
}
