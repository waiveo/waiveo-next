// Package authtest builds a ready-to-drive auth fixture for the in-process
// harnesses: an ephemeral auth store holding one real principal with one real
// role binding and one real, unrevoked session.
//
// It exists because removing the fixed POC principal from the api layer made
// every in-process harness — the api/1 conformance driver, the api package's own
// end-to-end tests, the feeder's tests — need a genuine authenticated identity
// to drive requests as. Handing them a bypass switch instead ("skip auth in
// tests") would have made every one of those tests prove something other than
// what ships, and would have made security-model/1 SEC-005 an untested claim.
// Seeding a real principal and presenting its real credential is the honest fix:
// the harnesses exercise the same middleware, the same session lookup and the
// same authorization decision a browser does.
package authtest

import (
	"context"
	"fmt"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/auth"
)

// Config parameterizes New. Every field has a working default, so the common
// case is New(Config{}).
type Config struct {
	// NowMs is the injected clock. Defaults to a fixed instant, so a fixture's
	// behavior does not depend on when the test ran.
	NowMs func() int64
	// PrincipalID, when set, is the exact id the seeded principal is created
	// with — for a conformance case that pins a principal-derived field (a
	// Job's created_by) and must reproduce it. It MUST be a valid ULID.
	PrincipalID string
	// Kind defaults to auth.KindUser.
	Kind string
	// Role is the role the principal is bound at. Defaults to auth.RoleOwner,
	// so a fixture can drive every operation unless a test is specifically
	// exercising an authorization refusal.
	Role auth.Role
	// ScopeNode is the node the role is bound at. Defaults to
	// auth.RootScopeNode, which inherits to every node in the tree.
	ScopeNode string
	// Sink receives the audit events the fixture's authenticator emits; nil for
	// a fixture that does not assert on them.
	Sink auth.EventSink
}

// Fixture is a seeded auth environment plus the credential to drive it with.
type Fixture struct {
	Store       *auth.Store
	Auth        *auth.Authenticator
	Handlers    *auth.Handlers
	Revocations *auth.Revocations

	PrincipalID string
	SessionID   string
	// Token is the raw session token: present it as the `session` cookie.
	Token string
	// CSRFToken is the double-submit value a mutating cookie-authenticated
	// request must echo in the X-CSRF-Token header (SEC-024).
	CSRFToken string
	// APIKey is a second, independently-minted credential for the SAME
	// principal, presented as `Authorization: Bearer`. Having both on one
	// fixture is what lets a test prove the two presentation forms resolve to
	// one identity and that only the cookie form carries the CSRF requirement.
	APIKey string
}

// fixedNowMs is the default injected clock reading: 2026-07-15T00:00:00Z, the
// instant the frozen corpora already pin their timestamps to.
const fixedNowMs int64 = 1752537600000

// New builds an in-memory auth store seeded per cfg and returns the fixture.
// The caller MUST Close the returned Store.
func New(cfg Config) (*Fixture, error) {
	if cfg.NowMs == nil {
		cfg.NowMs = func() int64 { return fixedNowMs }
	}
	if cfg.Kind == "" {
		cfg.Kind = auth.KindUser
	}
	if cfg.Role == "" {
		cfg.Role = auth.RoleOwner
	}
	if cfg.ScopeNode == "" {
		cfg.ScopeNode = auth.RootScopeNode
	}

	newID := seededIDs(cfg.PrincipalID)
	store, err := auth.Open(":memory:", cfg.NowMs, newID)
	if err != nil {
		return nil, fmt.Errorf("authtest: open store: %w", err)
	}
	ctx := context.Background()

	p, err := store.CreatePrincipal(ctx, cfg.Kind, "fixture")
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("authtest: create principal: %w", err)
	}
	if _, err := store.PutRoleBinding(ctx, p.PrincipalID, cfg.ScopeNode, cfg.Role); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("authtest: bind role: %w", err)
	}
	minted, err := store.MintSession(ctx, p.PrincipalID, auth.TokenKindSession, "", auth.AALStandard,
		map[string]string{"user_agent": "authtest", "ip_class": auth.IPClassLoopback})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("authtest: mint session: %w", err)
	}
	key, err := store.MintAPIKey(ctx, p.PrincipalID, "authtest")
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("authtest: mint api key: %w", err)
	}

	revocations := auth.NewRevocations()
	store.OnRevoke(revocations.Revoked)
	auditor := auth.NewAuditor(cfg.Sink, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", cfg.NowMs, newID, nil)
	authenticator := auth.NewAuthenticator(store, auditor, auth.NewDefaultLockout(), revocations)

	return &Fixture{
		Store:       store,
		Auth:        authenticator,
		Handlers:    auth.NewHandlers(authenticator, nil, cfg.ScopeNode),
		Revocations: revocations,
		PrincipalID: p.PrincipalID,
		SessionID:   minted.Session.SessionID,
		Token:       minted.Token,
		CSRFToken:   minted.CSRFToken,
		APIKey:      key.Token,
	}, nil
}

// Close releases the fixture's store.
func (f *Fixture) Close() { _ = f.Store.Close() }

// Authorize stamps r with the fixture's session cookie and, for a mutating
// method, its CSRF header — everything a real browser request would carry. It is
// the single call a harness makes so no test hand-rolls (and mis-spells) the
// header and cookie names.
func (f *Fixture) Authorize(r *http.Request) {
	r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: f.Token})
	r.Header.Set(auth.CSRFHeaderName, f.CSRFToken)
}

// AuthorizeHeaders returns the same credential as a plain header map, for a
// harness that drives requests through a map rather than an *http.Request.
func (f *Fixture) AuthorizeHeaders() map[string]string {
	return map[string]string{
		"Cookie":            auth.SessionCookieName + "=" + f.Token,
		auth.CSRFHeaderName: f.CSRFToken,
	}
}

// seededIDs returns an id source yielding first (when non-empty) the exact
// principal id a caller pinned, then a deterministic ascending ULID sequence.
// Deterministic rather than random so a fixture's ids are reproducible across
// runs, and ascending so they sort in mint order like a real ULID would.
func seededIDs(first string) func() string {
	const prefix = "01J8Z9AUTHTEST0F1XTURE00"
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	n := 0
	pinned := first
	return func() string {
		if pinned != "" {
			out := pinned
			pinned = ""
			return out
		}
		hi := alphabet[(n/32)%32]
		lo := alphabet[n%32]
		n++
		return prefix + string([]byte{hi, lo})
	}
}
