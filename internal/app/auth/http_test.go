package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"os"

	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// recordingSink captures every audit.event the code under test emits, so the
// mandatory-emission assertions (EVT-081, SEC-150) are made against real
// envelopes rather than a log scrape.
type recordingSink struct {
	mu     sync.Mutex
	events []events.Envelope
}

func (s *recordingSink) Append(env events.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, env)
}

func (s *recordingSink) payloads(action string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, env := range s.events {
		var m map[string]any
		if err := json.Unmarshal(env.Payload, &m); err != nil {
			continue
		}
		if m["action"] == action {
			out = append(out, m)
		}
	}
	return out
}

// snapshot returns every envelope recorded so far, for an assertion that has to
// look across actions rather than at one.
func (s *recordingSink) snapshot() []events.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.Envelope(nil), s.events...)
}

// harness is a full auth HTTP environment: a store with one owner principal
// holding a password, the authenticator, the handlers, and the mux they and a
// protected route are mounted on — exactly the composition the feeder runs.
type harness struct {
	store  *Store
	auth   *Authenticator
	h      *Handlers
	sink   *recordingSink
	clock  *testClock
	mux    http.Handler
	owner  PrincipalRow
	ident  string
	passwd string
}

const testProtectedPath = "/api/v1/scope-nodes"

// newHTTPHarness builds the harness with a REAL secret sealer — not a stub: the
// second-factor cases assert that a stored TOTP secret is not recoverable from
// its row, and a no-op sealer would make that assertion pass against an
// implementation storing it in the clear.
func newHTTPHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, testSealer(t))
}

// newHTTPHarnessWithoutSealer is the deployment that holds no workspace key, for
// the cases that pin what happens when a recoverable secret cannot be protected.
func newHTTPHarnessWithoutSealer(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, nil)
}

func newHarness(t *testing.T, sealer SecretSealer) *harness {
	t.Helper()
	clock := newTestClock()
	var opts []StoreOption
	if sealer != nil {
		opts = append(opts, WithSecretSealer(sealer))
	}
	st, err := Open(":memory:", clock.now, ulid.New, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	owner, err := st.CreatePrincipal(ctx, KindUser, "owner")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	const ident, passwd = "owner@example.test", "correct horse battery staple"
	if _, err := st.PutPasswordCredential(ctx, owner.PrincipalID, ident, passwd); err != nil {
		t.Fatalf("PutPasswordCredential: %v", err)
	}
	if _, err := st.PutRoleBinding(ctx, owner.PrincipalID, RootScopeNode, RoleOwner); err != nil {
		t.Fatalf("PutRoleBinding: %v", err)
	}

	sink := &recordingSink{}
	revocations := NewRevocations()
	st.OnRevoke(revocations.Revoked)
	auditor := NewAuditor(sink, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", clock.now, ulid.New, nil)
	a := NewAuthenticator(st, auditor, NewLockout(3, 1000, 60_000), revocations)
	handlers := NewHandlers(a, nil, RootScopeNode)

	protected := http.NewServeMux()
	protected.HandleFunc(testProtectedPath, func(w http.ResponseWriter, r *http.Request) {
		p, err := RequirePrincipal(r.Context())
		if err != nil {
			t.Errorf("a protected route must always see a principal; got %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"principal_id": p.ID, "role": string(p.Role)})
	})
	protected.HandleFunc("POST /api/v1/auth/logout", handlers.Logout)
	protected.HandleFunc("GET /api/v1/auth/session", handlers.Session)
	protected.HandleFunc("POST /api/v1/auth/totp/enroll", handlers.EnrollTOTP)
	protected.HandleFunc("POST /api/v1/auth/totp/confirm", handlers.ConfirmTOTP)

	root := http.NewServeMux()
	root.HandleFunc("POST /api/v1/auth/login", handlers.Login)
	root.HandleFunc("POST /api/v1/auth/setup", handlers.Claim)
	root.Handle("/", a.Middleware(APICodes, nil)(protected))

	return &harness{
		store: st, auth: a, h: handlers, sink: sink, clock: clock,
		mux:   apihttp.WithTraceID(root),
		owner: owner, ident: ident, passwd: passwd,
	}
}

func (h *harness) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func (h *harness) login(t *testing.T, identifier, password string) *httptest.ResponseRecorder {
	t.Helper()
	return h.loginWithCode(t, identifier, password, "")
}

// loginWithCode drives the login exchange carrying a second factor (SEC-004).
func (h *harness) loginWithCode(t *testing.T, identifier, password, code string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(loginRequest{Identifier: identifier, Password: password, TOTPCode: code})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(body)))
	req.RemoteAddr = "192.168.50.9:41234"
	return h.do(req)
}

// problemCode reads the machine-readable `code` out of a Problem body.
func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("error body must be a Problem document; got %q", rec.Body.String())
	}
	code, _ := p["code"].(string)
	return code
}

// ---- API-090/091: login authenticates from the body -----------------------

// TestLoginIsACredentialExchangeOperation drives API-091: a login "MUST
// authenticate the request from credentials carried in the request body; it MUST
// NOT require a pre-existing session or API key as a precondition of its own
// success" — which is why this request carries no cookie and no bearer header
// and still succeeds.
func TestLoginIsACredentialExchangeOperation(t *testing.T) {
	h := newHTTPHarness(t)
	rec := h.login(t, h.ident, h.passwd)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s; want 200", rec.Code, rec.Body.String())
	}
	var got sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode login body: %v", err)
	}
	if got.PrincipalID != h.owner.PrincipalID {
		t.Fatalf("login attributed to %q, want %q", got.PrincipalID, h.owner.PrincipalID)
	}
	if got.Role != RoleOwner {
		t.Fatalf("login role = %q, want owner", got.Role)
	}
	if got.CSRFToken == "" {
		t.Fatal("a browser login must return the double-submit CSRF token (SEC-024)")
	}

	// SEC-023: the session cookie is host-only — no Domain attribute at all.
	var sawSession, sawCSRF bool
	for _, c := range rec.Result().Cookies() {
		if c.Domain != "" {
			t.Fatalf("cookie %q carries Domain=%q; SEC-023 requires a host-only session cookie", c.Name, c.Domain)
		}
		switch c.Name {
		case SessionCookieName:
			sawSession = true
			if !c.HttpOnly {
				t.Fatal("the session cookie must be HttpOnly")
			}
			if c.SameSite == http.SameSiteNoneMode || c.SameSite == 0 {
				t.Fatalf("the session cookie must carry SameSite scoping (SEC-024); got %v", c.SameSite)
			}
		case CSRFCookieName:
			sawCSRF = true
			if c.HttpOnly {
				t.Fatal("the CSRF cookie must be readable by the page's own script — that is the double-submit mechanism")
			}
		}
	}
	if !sawSession || !sawCSRF {
		t.Fatalf("login must set both cookies; session=%v csrf=%v", sawSession, sawCSRF)
	}

	// EVT-081/SEC-150: both the authentication event and the session issuance
	// are on the mandatory-emission list.
	if len(h.sink.payloads(ActionLoginSuccess)) != 1 {
		t.Fatal("a successful login must emit an audit.event (EVT-081)")
	}
	if len(h.sink.payloads(ActionSessionIssued)) != 1 {
		t.Fatal("session issuance must emit its own audit.event (EVT-081)")
	}
}

// TestLoginBodyValidationIs422 drives API-013a: a body-level validation failure
// carries 422, not the 400 a query-parameter failure carries.
func TestLoginBodyValidationIs422(t *testing.T) {
	h := newHTTPHarness(t)
	rec := h.login(t, "", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an empty login body must be 422 (API-013a); got %d", rec.Code)
	}
	if code := problemCode(t, rec); code != "VALIDATION_FAILED" {
		t.Fatalf("code = %q, want VALIDATION_FAILED", code)
	}
	var p struct {
		Errors []fieldError `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if len(p.Errors) != 2 {
		t.Fatalf("a multi-field failure must carry one errors[] entry per field (API-013); got %d", len(p.Errors))
	}
}

// TestLoginFailureAuditCarriesClockAssessment drives SEC-091: "Every
// login-failure audit.event MUST carry the app's current clock-accuracy
// assessment alongside its other fields."
func TestLoginFailureAuditCarriesClockAssessment(t *testing.T) {
	h := newHTTPHarness(t)
	rec := h.login(t, h.ident, "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong password must be 401; got %d", rec.Code)
	}
	failures := h.sink.payloads(ActionLoginFailure)
	if len(failures) != 1 {
		t.Fatalf("a failed login must emit an audit.event (EVT-081); got %d", len(failures))
	}
	assessment, _ := failures[0]["clock_assessment"].(string)
	if assessment != ClockUntrusted && assessment != ClockTrusted {
		t.Fatalf("a login-failure audit.event must carry the clock assessment (SEC-091); got %q", assessment)
	}
	// EVT-083: a failure record carries every other field a success would.
	for _, field := range []string{"actor_principal", "action", "target", "result"} {
		if _, ok := failures[0][field]; !ok {
			t.Fatalf("a result:failure audit.event must still carry %q (EVT-083)", field)
		}
	}
}

// TestLoginDoesNotDiscloseWhetherAnIdentifierExists pins that a wrong password
// and an unknown identifier are indistinguishable to the caller.
func TestLoginDoesNotDiscloseWhetherAnIdentifierExists(t *testing.T) {
	h := newHTTPHarness(t)
	wrongPassword := h.login(t, h.ident, "nope")
	unknownUser := h.login(t, "nobody@example.test", "nope")
	if wrongPassword.Code != unknownUser.Code {
		t.Fatalf("status differs: wrong password %d vs unknown identifier %d", wrongPassword.Code, unknownUser.Code)
	}
	if wrongPassword.Body.String() == "" || unknownUser.Body.String() == "" {
		t.Fatal("both failures must carry a Problem body")
	}
	var a, b map[string]any
	_ = json.Unmarshal(wrongPassword.Body.Bytes(), &a)
	_ = json.Unmarshal(unknownUser.Body.Bytes(), &b)
	if a["code"] != b["code"] || a["detail"] != b["detail"] {
		t.Fatalf("a wrong password and an unknown identifier must be indistinguishable; got %v vs %v", a, b)
	}
}

// TestLoginLockout drives SEC-090's refusal: a locked-out attempt is refused
// with CREDENTIAL_LOCKED, and the lock lifts on the injected clock rather than
// on a sleep.
func TestLoginLockout(t *testing.T) {
	h := newHTTPHarness(t)
	// The harness lockout tolerates 3 failures then locks for 1000ms.
	for i := 0; i < 4; i++ {
		h.login(t, h.ident, "wrong")
	}
	rec := h.login(t, h.ident, h.passwd) // the RIGHT password, while locked
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a locked credential must be refused; got %d", rec.Code)
	}
	if code := problemCode(t, rec); code != "CREDENTIAL_LOCKED" {
		t.Fatalf("code = %q, want CREDENTIAL_LOCKED (SEC-090)", code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("a lockout refusal must state its backoff")
	}
	if len(h.sink.payloads(ActionLoginLockout)) == 0 {
		t.Fatal("a lockout must emit an audit.event (EVT-081, SEC-150)")
	}

	h.clock.advance(2000)
	if rec := h.login(t, h.ident, h.passwd); rec.Code != http.StatusOK {
		t.Fatalf("the lock must lift once its backoff elapses; got %d %s", rec.Code, rec.Body.String())
	}
}

// ---- SEC-005: refuse, never default-permit --------------------------------

// TestUnauthenticatedRequestIsRefused drives SEC-005: "a route that cannot
// resolve an authorization decision for its caller MUST refuse the request
// rather than default-permit."
func TestUnauthenticatedRequestIsRefused(t *testing.T) {
	h := newHTTPHarness(t)
	rec := h.do(httptest.NewRequest(http.MethodGet, testProtectedPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated request must be 401; got %d %s", rec.Code, rec.Body.String())
	}
	if code := problemCode(t, rec); code != "UNAUTHENTICATED" {
		t.Fatalf("code = %q, want UNAUTHENTICATED", code)
	}
}

// TestAuthenticatedButUnboundPrincipalIsForbidden is SEC-005's other half:
// authentication is not authorization. A principal holding a live session but no
// role binding anywhere is refused 403, not served.
func TestAuthenticatedButUnboundPrincipalIsForbidden(t *testing.T) {
	h := newHTTPHarness(t)
	ctx := context.Background()
	stranger, err := h.store.CreatePrincipal(ctx, KindUser, "stranger")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	minted, err := h.store.MintSession(ctx, stranger.PrincipalID, TokenKindSession, "", AALStandard, nil)
	if err != nil {
		t.Fatalf("MintSession: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, testProtectedPath, nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: minted.Token})
	rec := h.do(req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a principal with no role binding must be 403; got %d %s", rec.Code, rec.Body.String())
	}
	if code := problemCode(t, rec); code != "FORBIDDEN" {
		t.Fatalf("code = %q, want FORBIDDEN", code)
	}
}

// TestViewerCannotMutate drives the coarse authority floor: a viewer reads and
// cannot write.
func TestViewerCannotMutate(t *testing.T) {
	h := newHTTPHarness(t)
	ctx := context.Background()
	viewer, err := h.store.CreatePrincipal(ctx, KindUser, "viewer")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if _, err := h.store.PutRoleBinding(ctx, viewer.PrincipalID, RootScopeNode, RoleViewer); err != nil {
		t.Fatalf("PutRoleBinding: %v", err)
	}
	minted, err := h.store.MintSession(ctx, viewer.PrincipalID, TokenKindSession, "", AALStandard, nil)
	if err != nil {
		t.Fatalf("MintSession: %v", err)
	}

	read := httptest.NewRequest(http.MethodGet, testProtectedPath, nil)
	read.AddCookie(&http.Cookie{Name: SessionCookieName, Value: minted.Token})
	if rec := h.do(read); rec.Code != http.StatusOK {
		t.Fatalf("a viewer must be able to read; got %d %s", rec.Code, rec.Body.String())
	}

	write := httptest.NewRequest(http.MethodPost, testProtectedPath, strings.NewReader("{}"))
	write.AddCookie(&http.Cookie{Name: SessionCookieName, Value: minted.Token})
	write.Header.Set(CSRFHeaderName, minted.CSRFToken)
	rec := h.do(write)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer must not be able to write; got %d %s", rec.Code, rec.Body.String())
	}
}

// TestRevokedSessionIsRefused pins that a revoked token is indistinguishable
// from a never-issued one at the door.
func TestRevokedSessionIsRefused(t *testing.T) {
	h := newHTTPHarness(t)
	rec := h.login(t, h.ident, h.passwd)
	token := sessionCookieFrom(t, rec)

	req := httptest.NewRequest(http.MethodGet, testProtectedPath, nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	if got := h.do(req); got.Code != http.StatusOK {
		t.Fatalf("the fresh session must work; got %d", got.Code)
	}

	var body sessionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if err := h.store.RevokeSession(context.Background(), body.SessionID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	req2 := httptest.NewRequest(http.MethodGet, testProtectedPath, nil)
	req2.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	got := h.do(req2)
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked session must be 401; got %d", got.Code)
	}
}

// ---- SEC-024: double-submit CSRF ------------------------------------------

// TestMutatingCookieRequestRequiresCSRFToken drives SEC-024: "Every mutating
// api/1 route reachable from a browser session MUST require both SameSite cookie
// scoping and a double-submit CSRF token; SameSite alone MUST NOT be treated as
// sufficient."
//
// The bearer half is the other side of the same requirement: an API key is not
// ambient authority a cross-site page can cause a browser to attach, so
// requiring a CSRF token from it would be ceremony with no threat behind it.
func TestMutatingCookieRequestRequiresCSRFToken(t *testing.T) {
	h := newHTTPHarness(t)
	rec := h.login(t, h.ident, h.passwd)
	token := sessionCookieFrom(t, rec)
	var body sessionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	t.Run("cookie without CSRF header is refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, testProtectedPath, strings.NewReader("{}"))
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
		got := h.do(req)
		if got.Code != http.StatusForbidden {
			t.Fatalf("a mutating cookie request without a CSRF token must be 403 (SEC-024); got %d %s", got.Code, got.Body.String())
		}
	})

	t.Run("cookie with a WRONG CSRF header is refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, testProtectedPath, strings.NewReader("{}"))
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
		req.Header.Set(CSRFHeaderName, strings.Repeat("a", 64))
		if got := h.do(req); got.Code != http.StatusForbidden {
			t.Fatalf("a mismatched CSRF token must be 403; got %d", got.Code)
		}
	})

	t.Run("cookie with the matching CSRF header is served", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, testProtectedPath, strings.NewReader("{}"))
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
		req.Header.Set(CSRFHeaderName, body.CSRFToken)
		if got := h.do(req); got.Code != http.StatusOK {
			t.Fatalf("a correct double-submit must be served; got %d %s", got.Code, got.Body.String())
		}
	})

	t.Run("a GET needs no CSRF token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, testProtectedPath, nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
		if got := h.do(req); got.Code != http.StatusOK {
			t.Fatalf("a non-mutating request must not require a CSRF token; got %d", got.Code)
		}
	})

	t.Run("a bearer API key needs no CSRF token", func(t *testing.T) {
		key, err := h.store.MintAPIKey(context.Background(), h.owner.PrincipalID, "cli")
		if err != nil {
			t.Fatalf("MintAPIKey: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, testProtectedPath, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+key.Token)
		if got := h.do(req); got.Code != http.StatusOK {
			t.Fatalf("a bearer-authenticated mutation must not require a CSRF token; got %d %s", got.Code, got.Body.String())
		}
	})
}

// ---- EVT-112: no credential in the query string ---------------------------

// TestCredentialInQueryStringIsRefused drives EVT-112: "An API key or session
// credential MUST NOT be accepted as a query-string parameter on either binding
// — a token that could ride a URL is a token that leaks into server access logs
// and intermediate proxies."
func TestCredentialInQueryStringIsRefused(t *testing.T) {
	h := newHTTPHarness(t)
	rec := h.login(t, h.ident, h.passwd)
	token := sessionCookieFrom(t, rec)

	for _, param := range []string{"token", "api_key", "access_token", "session"} {
		req := httptest.NewRequest(http.MethodGet, testProtectedPath+"?"+param+"="+token, nil)
		got := h.do(req)
		if got.Code != http.StatusUnauthorized {
			t.Fatalf("a credential in the %q query parameter must be refused (EVT-112); got %d", param, got.Code)
		}
	}
}

// ---- SEC-120: first-boot claim is not first-come-first-served -------------

// TestClaimRequiresTheSetupGrant drives SEC-120: "the setup endpoint MUST be
// claimable only by redeeming this grant. An installed-but-unclaimed box MUST
// NOT be first-come-first-served to whoever reaches its setup endpoint first on a
// shared network."
func TestClaimRequiresTheSetupGrant(t *testing.T) {
	clock := newTestClock()
	sink := &recordingSink{}
	auditor := NewAuditor(sink, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", clock.now, ulid.New, nil)
	// The store carries the auditor, because SEC-034's grant.created and
	// grant.redeemed records are emitted by MintGrant and RedeemGrant rather
	// than by the handlers that call them — "every grant creation and every
	// grant redemption", taken as a property of the two functions that perform
	// those acts. A store opened without a sink records neither, which is what
	// makes WithAuditor a deployment requirement rather than a convenience.
	st, err := Open(":memory:", clock.now, ulid.New, WithAuditor(auditor))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	a := NewAuthenticator(st, auditor, nil, nil)
	handlers := NewHandlers(a, nil, RootScopeNode)

	mux := apihttp.WithTraceID(http.HandlerFunc(handlers.Claim))
	claim := func(code string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(claimRequest{Code: code, Identifier: "first@example.test", Password: "a long enough passphrase"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(string(body)))
		req.RemoteAddr = "192.168.50.9:41234"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// An unclaimed box with a live setup grant still refuses a caller who does
	// not hold the code — arrival order buys nothing.
	boot, err := EnsureClaimWindow(ctx, st, t.TempDir(), RootScopeNode)
	if err != nil {
		t.Fatalf("EnsureClaimWindow: %v", err)
	}
	if boot.Claimed {
		t.Fatal("a store with no owner must report unclaimed")
	}
	if rec := claim("0000000000000000000000000000000000000000000000000000000000000000"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong setup code must be refused (SEC-120); got %d %s", rec.Code, rec.Body.String())
	}
	if owners, _ := st.CountOwnerBindings(ctx); owners != 0 {
		t.Fatal("a refused claim must create no owner")
	}

	// The real code claims the box, once.
	rec := claim(boot.Code)
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeeming the setup code must claim the box; got %d %s", rec.Code, rec.Body.String())
	}
	var claimed sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &claimed); err != nil {
		t.Fatalf("decode claim body: %v", err)
	}
	if claimed.Role != RoleOwner {
		t.Fatalf("the first claim must produce an owner; got %q", claimed.Role)
	}
	owners, err := st.CountOwnerBindings(ctx)
	if err != nil {
		t.Fatalf("CountOwnerBindings: %v", err)
	}
	if owners != 1 {
		t.Fatalf("owner bindings after claim = %d, want 1", owners)
	}

	// SEC-031/036: the code is one-time.
	if rec := claim(boot.Code); rec.Code == http.StatusCreated {
		t.Fatal("a setup code must not be redeemable twice (SEC-031/036)")
	} else if code := problemCode(t, rec); code != "GRANT_ALREADY_REDEEMED" {
		t.Fatalf("a second redemption must be GRANT_ALREADY_REDEEMED (SEC-035); got %q", code)
	}

	// SEC-034: the redemption's audit record carries purpose and issued_via.
	redemptions := sink.payloads(ActionGrantRedeemed)
	if len(redemptions) != 1 {
		t.Fatalf("a grant redemption must emit exactly one audit.event (SEC-034); got %d", len(redemptions))
	}
	if redemptions[0]["purpose"] != PurposeSetup {
		t.Fatalf("the redemption audit record must carry the grant's purpose (SEC-034); got %v", redemptions[0]["purpose"])
	}
	if redemptions[0]["issued_via"] != IssuedViaConsole {
		t.Fatalf("the redemption audit record must carry issued_via (SEC-034); got %v", redemptions[0]["issued_via"])
	}

	// Re-running the bootstrap on a now-claimed box shuts the window.
	boot2, err := EnsureClaimWindow(ctx, st, t.TempDir(), RootScopeNode)
	if err != nil {
		t.Fatalf("EnsureClaimWindow (claimed): %v", err)
	}
	if !boot2.Claimed || boot2.Code != "" {
		t.Fatalf("a claimed box must mint no setup grant; got %+v", boot2)
	}
}

// TestSetupCodeIsPersisted0600 drives the repo's secret-at-rest discipline: the
// setup code lands 0600 via a temp-and-rename, so a crash mid-write can never
// leave an operator locked out of an unclaimed box with a truncated code file.
func TestSetupCodeIsPersisted0600(t *testing.T) {
	clock := newTestClock()
	st, err := Open(":memory:", clock.now, ulid.New)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	dir := t.TempDir()
	boot, err := EnsureClaimWindow(context.Background(), st, dir, RootScopeNode)
	if err != nil {
		t.Fatalf("EnsureClaimWindow: %v", err)
	}
	info, err := os.Stat(boot.CodePath)
	if err != nil {
		t.Fatalf("stat setup code: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the setup code file must be 0600; got %v", perm)
	}
	raw, err := os.ReadFile(boot.CodePath)
	if err != nil {
		t.Fatalf("read setup code: %v", err)
	}
	if strings.TrimSpace(string(raw)) != boot.Code {
		t.Fatal("the persisted file must carry exactly the returned code")
	}
	// No temp file left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the write must leave exactly the code file behind; got %d entries", len(entries))
	}
}

// TestFactoryResetReopensClaimOnlyAtTheNextBoot pins the seam between SEC-121's
// promise and the machinery that keeps it. The destruction re-opens the claim
// window by removing the last `owner` binding, and that is genuinely all it does:
// nothing in the reset path mints a `setup` grant, and nothing removes the code
// file the PREVIOUS claim window left on disk. Both effects arrive at the next
// boot, when EnsureClaimWindow runs again.
//
// Between the two the box is unclaimed and unclaimable, and the stale
// `setup-code.txt` beside it reads exactly like a live one — so an operator who
// resets a box and retries the code they still have is refused, with the same
// 401 a wrong code draws, and the only remedy is a restart. The setup route's
// 401 message names it because of this test.
func TestFactoryResetReopensClaimOnlyAtTheNextBoot(t *testing.T) {
	clock := newTestClock()
	st, err := Open(":memory:", clock.now, ulid.New)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	handlers := NewHandlers(NewAuthenticator(st, nil, nil, nil), nil, RootScopeNode)
	mux := apihttp.WithTraceID(http.HandlerFunc(handlers.Claim))
	claim := func(code, identifier string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(claimRequest{Code: code, Identifier: identifier, Password: "a long enough passphrase"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(string(body)))
		req.RemoteAddr = "192.168.50.9:41234"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	dir := t.TempDir()
	codePath := filepath.Join(dir, SetupGrantFile)
	boot, err := EnsureClaimWindow(ctx, st, dir, RootScopeNode)
	if err != nil {
		t.Fatalf("EnsureClaimWindow: %v", err)
	}
	if rec := claim(boot.Code, "first@example.test"); rec.Code != http.StatusCreated {
		t.Fatalf("the first claim must succeed; got %d %s", rec.Code, rec.Body.String())
	}

	// The claim itself leaves the code file exactly where the bootstrap wrote
	// it — the handler never touches disk. Its presence is therefore "no boot
	// since the claim", never "unclaimed", and anything reading it as claim
	// state is reading the wrong thing.
	if _, err := os.Stat(codePath); err != nil {
		t.Fatalf("a successful claim must leave the code file for the next boot to clear; stat: %v", err)
	}

	if err := st.DestroyLocalAuthState(ctx); err != nil {
		t.Fatalf("DestroyLocalAuthState: %v", err)
	}
	owners, err := st.CountOwnerBindings(ctx)
	if err != nil {
		t.Fatalf("CountOwnerBindings: %v", err)
	}
	if owners != 0 {
		t.Fatalf("the reset must leave no owner binding, so the next boot reads the box as unclaimed; got %d", owners)
	}

	// ... and yet, in-process, the window is NOT open: the grant rows went with
	// everything else, so the code still sitting on disk resolves to nothing.
	raw, err := os.ReadFile(codePath)
	if err != nil {
		t.Fatalf("the reset must not remove the stale code file (nothing in the reset path touches it); read: %v", err)
	}
	stale := strings.TrimSpace(string(raw))
	if stale != boot.Code {
		t.Fatalf("the stale file must still hold the code that claimed the box; got %q", stale)
	}
	if rec := claim(stale, "second@example.test"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a reset box that has not rebooted must refuse the stale code with the same 401 a wrong code draws; got %d %s", rec.Code, rec.Body.String())
	}

	// The restart is what re-opens it: a fresh grant, a fresh code, and the
	// stale file overwritten with it.
	boot2, err := EnsureClaimWindow(ctx, st, dir, RootScopeNode)
	if err != nil {
		t.Fatalf("EnsureClaimWindow (after reset): %v", err)
	}
	if boot2.Claimed || boot2.Code == "" {
		t.Fatalf("the boot after a reset must mint a fresh setup grant (SEC-121); got %+v", boot2)
	}
	if boot2.Code == boot.Code {
		t.Fatal("the boot after a reset must mint a DIFFERENT code, not re-present the redeemed one")
	}
	if rec := claim(boot2.Code, "second@example.test"); rec.Code != http.StatusCreated {
		t.Fatalf("the freshly minted code must claim the box; got %d %s", rec.Code, rec.Body.String())
	}
}

// ---- logout ---------------------------------------------------------------

// TestLogoutRevokesAndClears pins that logout revokes the calling session (so the
// token stops working) and expires both cookies.
func TestLogoutRevokesAndClears(t *testing.T) {
	h := newHTTPHarness(t)
	rec := h.login(t, h.ident, h.passwd)
	token := sessionCookieFrom(t, rec)
	var body sessionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	req.Header.Set(CSRFHeaderName, body.CSRFToken)
	out := h.do(req)
	if out.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s; want 204", out.Code, out.Body.String())
	}
	for _, c := range out.Result().Cookies() {
		if c.MaxAge >= 0 {
			t.Fatalf("logout must expire cookie %q; got MaxAge=%d", c.Name, c.MaxAge)
		}
	}
	if len(h.sink.payloads(ActionSessionRevoked)) != 1 {
		t.Fatal("session revocation must emit an audit.event (EVT-081)")
	}

	after := httptest.NewRequest(http.MethodGet, testProtectedPath, nil)
	after.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	if got := h.do(after); got.Code != http.StatusUnauthorized {
		t.Fatalf("the token must stop working after logout; got %d", got.Code)
	}
}

// TestLogoutRequiresAuthentication pins that logout is mounted BEHIND the
// middleware — a cross-site page cannot log a user out, because the route needs
// the CSRF token it cannot read.
func TestLogoutRequiresAuthentication(t *testing.T) {
	h := newHTTPHarness(t)
	rec := h.do(httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated logout must be 401; got %d", rec.Code)
	}
}

// sessionCookieFrom extracts the session cookie value a login response set.
func sessionCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			return c.Value
		}
	}
	t.Fatal("the response set no session cookie")
	return ""
}
