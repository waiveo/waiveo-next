// Package auth is the app-side security-model/1 implementation the api/1 and
// events/1 bindings authenticate and authorize through: the principal store and
// its separate credential relation (SEC-001/003), server-side session and
// API-key records (SEC-020/021), role bindings and their scope-node resolution
// (SEC-005/010/011), the generic purpose-scoped grant with its atomic
// check-and-consume redemption (SEC-030–036), per-(credential, source-IP class)
// lockout (SEC-090), and the HTTP middleware + credential-exchange handlers that
// put those in front of a request.
//
// Three structural decisions, stated here rather than left implicit:
//
//   - Sessions are SERVER-SIDE RECORDS, not stateless bearer assertions. SEC-021
//     wants the AAL claim readable from the token itself and EVT-114 wants a
//     revocation to tear down already-open streams within a bounded delay; the
//     second needs a lookup regardless, so the first is satisfied by encoding the
//     AAL in the token's own format (token.go) rather than by signing a JWT.
//     Tokens are opaque, >=128 bits of crypto/rand entropy, and stored HASHED
//     (SHA-256) at rest — the raw value exists only in the response that mints it
//     and in the caller's own credential store.
//
//   - Credentials are their OWN relation (SEC-001/003), never columns on a
//     users-shaped table: one principal holds N credentials, including more than
//     one of the same kind. Only `password` and `api-key` are implemented here;
//     the `passkey`/`totp`/`oidc-subject`/`cert` kinds are accepted by the schema's
//     enum but have no verification path yet.
//
//   - Authorization NEVER default-permits (SEC-005). A request whose principal
//     cannot be resolved is refused 401; a resolved principal holding no role
//     binding at all is refused 403. There is no anonymous fallback identity.
//
// This package owns its own SQLite file rather than sharing the authoring
// store's: every write there bumps the desired-state generation (a relay pull
// nudge, relay/1 REL-057), and a login has no business advancing desired state.
package auth

import (
	"context"
	"net/http"
)

// Principal kinds (SEC-001). The closed set a principal row's `kind` column
// accepts; `system-console` is the sole kind that carries no credential row
// (SEC-002) — admission over the console binding is itself its proof of
// identity, and that binding (SEC-070–078) is not served by this package.
const (
	KindUser          = "user"
	KindScreen        = "screen"
	KindRelay         = "relay"
	KindPackService   = "pack-service"
	KindIngestToken   = "ingest-token"
	KindSystemConsole = "system-console"
)

// validPrincipalKinds is SEC-001's closed enum, as a set.
var validPrincipalKinds = map[string]struct{}{
	KindUser: {}, KindScreen: {}, KindRelay: {},
	KindPackService: {}, KindIngestToken: {}, KindSystemConsole: {},
}

// ValidPrincipalKind reports whether kind is one of SEC-001's six principal
// kinds.
func ValidPrincipalKind(kind string) bool {
	_, ok := validPrincipalKinds[kind]
	return ok
}

// Credential kinds (SEC-003). The closed set a credential row's `kind` column
// accepts. This package implements verification for CredentialPassword and
// CredentialAPIKey only; the other four are schema-legal so a later increment
// adds a verifier without a migration, never so an unverifiable row can
// authenticate today (verifyCredential refuses any kind it has no path for).
const (
	CredentialPassword    = "password"
	CredentialPasskey     = "passkey"
	CredentialTOTP        = "totp"
	CredentialOIDCSubject = "oidc-subject"
	CredentialAPIKey      = "api-key"
	CredentialCert        = "cert"
)

// validCredentialKinds is SEC-003's closed enum, as a set.
var validCredentialKinds = map[string]struct{}{
	CredentialPassword: {}, CredentialPasskey: {}, CredentialTOTP: {},
	CredentialOIDCSubject: {}, CredentialAPIKey: {}, CredentialCert: {},
}

// ValidCredentialKind reports whether kind is one of SEC-003's six credential
// kinds.
func ValidCredentialKind(kind string) bool {
	_, ok := validCredentialKinds[kind]
	return ok
}

// AAL is the Authenticator Assurance Level a session or API-key token carries
// (SEC-021/022): AALStandard for an ordinarily-authenticated session,
// AALRecovery for one minted by redeeming a `recovery`-purpose grant. It rides
// in the token's own format (token.go), not merely as a database column.
type AAL string

const (
	AALStandard AAL = "standard"
	AALRecovery AAL = "recovery"
)

// Principal is the resolved, authenticated caller of one request: who they are
// (ID/Kind), what they may do (Role — the coarse effective role resolved from
// their bindings, see role.go), how strongly they authenticated (AAL, SEC-022),
// and which revocable record admitted them (SessionID/TokenKind, SEC-020). It is
// only ever constructed by this package's middleware from a live, unrevoked
// session row — there is no anonymous or default-permit value (SEC-005).
type Principal struct {
	ID        string
	Kind      string
	Role      Role
	AAL       AAL
	SessionID string
	// TokenKind is TokenKindSession (a browser cookie) or TokenKindAPIKey (a
	// bearer header). The distinction drives CSRF: a cookie is ambient
	// authority a cross-site form post can ride, a bearer header is not
	// (SEC-024, middleware.go).
	TokenKind string
	// Bindings is this principal's COMPLETE role-binding set (SEC-010), carried
	// on the resolved identity rather than re-read per handler. The middleware
	// already loads it to make its own admission decision, so passing it forward
	// costs nothing and buys the property that a request's visible scope-node
	// set is computed from the same bindings the request was admitted on — a
	// second read could observe a concurrent re-binding and answer "may reach
	// this method" and "may see this row" from two different worlds.
	//
	// A zero Principal therefore carries NO bindings, which CanRead reads as "no
	// authority anywhere". That is the intended fail-closed default: a handler
	// reached without authentication sees nothing rather than everything.
	Bindings []Binding
}

// principalContextKey is the unexported context key the middleware stores a
// request's resolved principal under — the same unexported-struct-key idiom
// apihttp.WithTraceID uses, so no other package can forge or overwrite it.
type principalContextKey struct{}

// WithPrincipal returns a child context carrying p as the request's
// authenticated principal.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// FromContext returns the principal the middleware resolved for this request,
// and whether one was resolved at all. A false second return means the request
// never passed authentication — a handler MUST refuse rather than substitute a
// default identity (SEC-005).
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	return p, ok
}

// PrincipalID returns r's authenticated principal id, or "" when r carries
// none. It is the convenience accessor the api/1 handlers use where they need
// only the id (an Idempotency-Key scope, a Job's created_by); a handler that
// must BRANCH on authentication uses FromContext instead, since "" is a value a
// caller could otherwise mistake for a real, if odd, principal.
func PrincipalID(r *http.Request) string {
	p, ok := FromContext(r.Context())
	if !ok {
		return ""
	}
	return p.ID
}
