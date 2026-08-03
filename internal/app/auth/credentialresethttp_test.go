package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// credentialresethttp_test.go covers the credential-reset ROUTES' own refusals
// and guards — the branches the frozen corpus's happy-path case never reaches,
// and each of the properties that would silently disappear if its guard were
// removed.

// resetEnv is the two credential-reset routes mounted the way api.New mounts
// them: the issuance BEHIND the auth middleware, the redemption AHEAD of it.
// Anything that depends on which side of the middleware a route sits on is
// therefore a real property of this fixture rather than an assumption.
type resetEnv struct {
	store  *Store
	sink   *recordingSink
	mux    http.Handler
	clock  *testClock
	admin  PrincipalRow
	target PrincipalRow
	token  string
}

const (
	resetIdentifier  = "reset-target@example.test"
	resetOldPassword = "the-password-the-target-forgot"
	resetNewPassword = "the-passphrase-the-target-chooses"
)

func newResetEnv(t *testing.T, adminRole Role) *resetEnv {
	t.Helper()
	ctx := context.Background()
	clock := newTestClock()
	sink := &recordingSink{}
	auditor := NewAuditor(sink, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", clock.now, ulid.New, nil)
	st, err := Open(":memory:", clock.now, ulid.New, WithAuditor(auditor))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	admin, err := st.CreatePrincipal(ctx, KindUser, "admin")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := st.PutRoleBinding(ctx, admin.PrincipalID, RootScopeNode, adminRole); err != nil {
		t.Fatalf("bind admin: %v", err)
	}
	key, err := st.MintAPIKey(ctx, admin.PrincipalID, "cli", 0)
	if err != nil {
		t.Fatalf("mint api key: %v", err)
	}
	target, err := st.CreatePrincipal(ctx, KindUser, "target")
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := st.PutPasswordCredential(ctx, target.PrincipalID, resetIdentifier, resetOldPassword); err != nil {
		t.Fatalf("seed target credential: %v", err)
	}

	a := NewAuthenticator(st, auditor, NewDefaultLockout(), NewRevocations())
	handlers := NewHandlers(a, nil, RootScopeNode)

	protected := http.NewServeMux()
	protected.HandleFunc("POST /api/v1/auth/credential-reset", handlers.IssueCredentialReset)
	root := http.NewServeMux()
	root.HandleFunc("POST /api/v1/auth/credential-reset/redeem", handlers.RedeemCredentialReset)
	root.Handle("/", a.Middleware(APICodes, nil)(protected))

	return &resetEnv{store: st, sink: sink, mux: apihttp.WithTraceID(root), clock: clock,
		admin: admin, target: target, token: key.Token}
}

func (e *resetEnv) issue(t *testing.T, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/credential-reset", strings.NewReader(string(raw)))
	req.RemoteAddr = "192.168.50.9:41234"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *resetEnv) redeem(t *testing.T, code, password string) *httptest.ResponseRecorder {
	t.Helper()
	return e.redeemFrom(t, "192.168.50.9:41234", code, password)
}

// redeemFrom is redeem from a caller-chosen source address — the input the
// SEC-033 attempt budget is keyed on, so it is the only way to observe at this
// surface WHICH allocation an attempt was charged to.
func (e *resetEnv) redeemFrom(t *testing.T, remoteAddr, code, password string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"code": code, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/credential-reset/redeem", strings.NewReader(string(raw)))
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *resetEnv) issueCode(t *testing.T) string {
	t.Helper()
	rec := e.issue(t, e.token, map[string]any{"target_principal_id": e.target.PrincipalID})
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue = %d %s, want 201", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode issuance: %v", err)
	}
	code, _ := body["code"].(string)
	if code == "" {
		t.Fatalf("the issuance returned no code: %s", rec.Body.String())
	}
	return code
}

// TestCredentialResetIssuanceRequiresAdmin is SEC-012's issuer restriction on
// the wire. Without it an `operator` — a role the middleware admits for POST —
// could mint a live reset code for any account on the box, including an owner's.
func TestCredentialResetIssuanceRequiresAdmin(t *testing.T) {
	e := newResetEnv(t, RoleOperator)
	rec := e.issue(t, e.token, map[string]any{"target_principal_id": e.target.PrincipalID})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("issue as operator = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if code := problemCode(t, rec); code != "FORBIDDEN" {
		t.Fatalf("code = %q, want FORBIDDEN", code)
	}
	n, err := e.store.CountGrants(t.Context(), PurposeCredentialReset)
	if err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if n != 0 {
		t.Fatalf("a refused issuance minted %d grant(s) — refused must mean nothing was created", n)
	}
	// EVT-083: the refused attempt is recorded.
	if got := e.sink.payloads(ActionGrantCreated); len(got) != 1 || got[0]["result"] != "failure" {
		t.Fatalf("a refused issuance must emit a failure record (EVT-083); got %v", got)
	}
}

// TestCredentialResetIssuanceRequiresAuthentication pins which side of the auth
// middleware the ISSUING route sits on. Only the redemption is exempt; an
// unauthenticated issuance would let anyone who can reach the box mint a live
// reset code for the owner.
func TestCredentialResetIssuanceRequiresAuthentication(t *testing.T) {
	e := newResetEnv(t, RoleAdmin)
	rec := e.issue(t, "", map[string]any{"target_principal_id": e.target.PrincipalID})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated issue = %d %s, want 401", rec.Code, rec.Body.String())
	}
}

// TestCredentialResetRedemptionNeedsNoCredential is the other side of that line,
// and it is API-091's own reason: the caller is the person who cannot sign in.
func TestCredentialResetRedemptionNeedsNoCredential(t *testing.T) {
	e := newResetEnv(t, RoleAdmin)
	code := e.issueCode(t)
	if rec := e.redeem(t, code, resetNewPassword); rec.Code != http.StatusNoContent {
		t.Fatalf("unauthenticated redeem = %d %s, want 204", rec.Code, rec.Body.String())
	}
	cred, err := e.store.FindPasswordCredential(t.Context(), resetIdentifier)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if err := VerifyPassword(cred.Secret, resetNewPassword); err != nil {
		t.Fatalf("the redemption returned 204 and did not set the credential: %v", err)
	}
}

// TestCredentialResetRedemptionMintsNoSession is the 2FA-bypass guard.
//
// The convenient thing — sign the target in, they just proved possession of the
// code — would walk straight past their second factor, because SEC-052 makes a
// credential-reset grant explicitly NOT authorize a TOTP change and the
// principal therefore still holds one. Disabling this guard (returning a
// sessionResponse and calling SetSessionCookies) fails here on both counts.
func TestCredentialResetRedemptionMintsNoSession(t *testing.T) {
	e := newResetEnv(t, RoleAdmin)
	code := e.issueCode(t)
	rec := e.redeem(t, code, resetNewPassword)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("redeem = %d %s, want 204", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Fatalf("the redemption returned a body (%q); it must mint nothing the caller can present", body)
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("the redemption set %d cookie(s); a reset must not become a way past the second factor", len(cookies))
	}
	ids, err := e.store.ListSessionIDs(t.Context(), e.target.PrincipalID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("the redemption left %d live session(s) for the target", len(ids))
	}
}

// TestCredentialResetHandoffCarriesNoURLDerivedFromTheHostHeader is the
// host-header-injection guard.
//
// A `url` built from `r.Host` would hand the issuing admin a link carrying a
// LIVE one-time code to whatever origin the request's Host header named — a
// credential handoff to a third party, produced by the platform, and looking to
// the admin exactly like their own box's link.
func TestCredentialResetHandoffCarriesNoURLDerivedFromTheHostHeader(t *testing.T) {
	e := newResetEnv(t, RoleAdmin)
	raw, _ := json.Marshal(map[string]any{"target_principal_id": e.target.PrincipalID})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/credential-reset", strings.NewReader(string(raw)))
	req.Host = "attacker.example"
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue = %d %s, want 201", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "attacker.example") {
		t.Fatalf("the handoff names a host taken from the request: %s", rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := body["url"]; present {
		t.Fatalf("the api handoff carries a url; the only host it could name comes from the request: %s", rec.Body.String())
	}
}

// TestCredentialResetIssuanceIgnoresASuppliedPassword is SEC-050's shape check
// on the wire: an admin who sends a credential value must not be able to set
// one, and the target's existing credential must be untouched by the issuance.
func TestCredentialResetIssuanceIgnoresASuppliedPassword(t *testing.T) {
	e := newResetEnv(t, RoleAdmin)
	const adminChosen = "the-value-the-admin-tried-to-choose"
	rec := e.issue(t, e.token, map[string]any{
		"target_principal_id": e.target.PrincipalID,
		"password":            adminChosen,
		"new_password":        adminChosen,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue = %d %s, want 201", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), adminChosen) {
		t.Fatalf("the issuance echoed the supplied value: %s", rec.Body.String())
	}
	cred, err := e.store.FindPasswordCredential(t.Context(), resetIdentifier)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if VerifyPassword(cred.Secret, adminChosen) == nil {
		t.Fatal("the value the admin supplied became the target's credential (SEC-050)")
	}
	if err := VerifyPassword(cred.Secret, resetOldPassword); err != nil {
		t.Fatalf("the issuance changed the target's credential; it must change nothing at all: %v", err)
	}
}

// TestCredentialResetRedemptionIsRateLimitedBeforeTheLookup is SEC-033: "a
// grant-issuing endpoint MUST enforce a redemption rate limit and an attempt
// budget against repeated guesses of a live grant's code — the enforced
// control".
//
// The budget is keyed on the credential-reset purpose, so exhausting it must not
// depend on any other endpoint's traffic; and the last attempt below presents a
// VALID code, which must still be refused — that is what proves the budget
// refuses before the lookup rather than filtering its result.
func TestCredentialResetRedemptionIsRateLimitedBeforeTheLookup(t *testing.T) {
	e := newResetEnv(t, RoleAdmin)
	code := e.issueCode(t)

	wrong := strings.Repeat("0", len(code))
	var limited bool
	for i := 0; i < DefaultGrantAttemptLimit+1; i++ {
		rec := e.redeem(t, wrong, resetNewPassword)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("%d wrong reset codes from one source were never rate-limited (SEC-033)", DefaultGrantAttemptLimit+1)
	}
	// The real code, now, is refused too — the budget stopped the lookup.
	if rec := e.redeem(t, code, resetNewPassword); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a valid code after budget exhaustion = %d, want 429 — the budget must refuse before the code is checked", rec.Code)
	}
	cred, err := e.store.FindPasswordCredential(t.Context(), resetIdentifier)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if err := VerifyPassword(cred.Secret, resetOldPassword); err != nil {
		t.Fatal("a rate-limited redemption still changed the credential")
	}
}

// TestCredentialResetBudgetIsKeyedOnTheSourceAllocation drives SEC-033's keying
// through this ROUTE, end to end, rather than through the budget object.
//
// It exists because of a coverage asymmetry that let a real defect through: the
// keying rule lives in apihttp.RequestSource and is exercised there and at the
// relay's pairing surface, but the app's own redemption endpoints — the ones
// whose doc claims /64 keying, and whose setup-code sibling mints `owner` at
// the root scope over an unauthenticated route — asserted only that ONE source
// exhausts its own budget. A change making IPv6 key on the full address failed
// those other two packages and passed this one, so nothing here could tell that
// the endpoint's stated bound had stopped existing.
//
// Both phases below spend the budget from addresses a single host mints for
// FREE. If the key were the address, each attempt would open a fresh bucket and
// the endpoint would never rate-limit at all — a budget counting nothing while
// reading as enforced.
func TestCredentialResetBudgetIsKeyedOnTheSourceAllocation(t *testing.T) {
	e := newResetEnv(t, RoleAdmin)
	code := e.issueCode(t)
	wrong := strings.Repeat("0", len(code))

	// Phase 1: a routable /64, the standard SLAAC allocation. Every attempt
	// arrives from a different address inside it, as RFC 8981 privacy
	// extensions produce with no attacker involved at all.
	limited := false
	for i := 0; i < DefaultGrantAttemptLimit+1; i++ {
		addr := fmt.Sprintf("[2001:db8:0:1::%x]:%d", i+1, 41000+i)
		if rec := e.redeemFrom(t, addr, wrong, resetNewPassword); rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Errorf("%d redemption attempts, each from a different address inside one /64, were never rate-limited — the budget is keyed on something the host mints for free (SEC-033)", DefaultGrantAttemptLimit+1)
	}

	// A DIFFERENT /64 is a different allocation and must still be served:
	// widening past the allocation boundary turns a per-source budget back into
	// the shared one an unrelated caller can exhaust for everybody.
	if rec := e.redeemFrom(t, "[2001:db8:0:2::1]:41000", wrong, resetNewPassword); rec.Code == http.StatusTooManyRequests {
		t.Error("a caller in a different /64 was refused because another allocation exhausted its budget — the bucket is shared across allocations")
	}

	// Phase 2: the zoned link-local form net/http actually hands this handler
	// for an on-link peer (net.TCPAddr.String() appends %zone). net.ParseIP
	// returns nil for it, so this form once bypassed the /64 reduction entirely
	// and every attempt bought its own bucket.
	limited = false
	for i := 0; i < DefaultGrantAttemptLimit+1; i++ {
		addr := (&net.TCPAddr{IP: net.ParseIP(fmt.Sprintf("fe80::%x", i+1)), Zone: "en0", Port: 41000 + i}).String()
		if !strings.Contains(addr, "%en0") {
			t.Fatalf("fixture RemoteAddr %q carries no zone; this phase no longer exercises a zoned source", addr)
		}
		if rec := e.redeemFrom(t, addr, wrong, resetNewPassword); rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Errorf("%d redemption attempts, each from a different ZONED link-local address on one link, were never rate-limited — a zoned source bypasses the allocation key (SEC-033)", DefaultGrantAttemptLimit+1)
	}
}

// TestCredentialResetRefusesAPairingCodeAtTheResetEndpoint is SEC-035's own
// worked example, which until now had no endpoint to be worked on: "a
// `pairing`-purpose code MUST NOT redeem against the credential-reset endpoint,
// even if otherwise well-formed."
func TestCredentialResetRefusesAPairingCodeAtTheResetEndpoint(t *testing.T) {
	e := newResetEnv(t, RoleAdmin)
	minted, err := e.store.MintGrant(t.Context(), MintGrantOptions{
		Purpose:                PurposePairing,
		ResultingPrincipalKind: KindScreen,
		TTLMs:                  DefaultResetGrantTTLMs,
		RedemptionMode:         RedemptionOneTime,
	})
	if err != nil {
		t.Fatalf("mint pairing grant: %v", err)
	}
	rec := e.redeem(t, minted.Code, resetNewPassword)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a pairing code at the reset endpoint = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if code := problemCode(t, rec); code != "GRANT_PURPOSE_MISMATCH" {
		t.Fatalf("code = %q, want GRANT_PURPOSE_MISMATCH (SEC-035)", code)
	}
	cred, err := e.store.FindPasswordCredential(t.Context(), resetIdentifier)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if err := VerifyPassword(cred.Secret, resetOldPassword); err != nil {
		t.Fatal("a purpose-mismatched redemption still changed the credential")
	}
}

// TestCredentialResetCodeIsSingleUse is SEC-031/036 on this endpoint: the second
// presentation of a spent code is refused, and refused as ALREADY_REDEEMED
// rather than as an unknown code, so an operator reading the trail can tell a
// replay from a guess.
func TestCredentialResetCodeIsSingleUse(t *testing.T) {
	e := newResetEnv(t, RoleAdmin)
	code := e.issueCode(t)
	if rec := e.redeem(t, code, resetNewPassword); rec.Code != http.StatusNoContent {
		t.Fatalf("first redemption = %d %s", rec.Code, rec.Body.String())
	}
	rec := e.redeem(t, code, "a-third-passphrase-entirely")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("replayed code = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if code := problemCode(t, rec); code != "GRANT_ALREADY_REDEEMED" {
		t.Fatalf("code = %q, want GRANT_ALREADY_REDEEMED", code)
	}
	cred, err := e.store.FindPasswordCredential(t.Context(), resetIdentifier)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if VerifyPassword(cred.Secret, "a-third-passphrase-entirely") == nil {
		t.Fatal("a replayed code set the credential a second time")
	}
}

// TestCredentialResetRefusesAPrincipalWithNoPasswordCredential covers the
// not-found branch, and pins that "no such principal" and "that principal holds
// nothing to reset" answer identically — so the route is not a probe for which
// principals hold password credentials.
func TestCredentialResetRefusesAPrincipalWithNoPasswordCredential(t *testing.T) {
	e := newResetEnv(t, RoleAdmin)
	bare, err := e.store.CreatePrincipal(t.Context(), KindUser, "no-credential")
	if err != nil {
		t.Fatalf("create principal: %v", err)
	}
	noCred := e.issue(t, e.token, map[string]any{"target_principal_id": bare.PrincipalID})
	absent := e.issue(t, e.token, map[string]any{"target_principal_id": "01J8Z3K4N5P6Q7R8S9T0V1W2X9"})
	if noCred.Code != http.StatusNotFound || absent.Code != http.StatusNotFound {
		t.Fatalf("no-credential = %d, absent = %d; want 404 for both", noCred.Code, absent.Code)
	}
	// Compared with `trace_id` dropped: that member is per-request by definition
	// (API-061) and differing on it discloses nothing about the subject.
	if a, b := problemWithoutTrace(t, noCred), problemWithoutTrace(t, absent); a != b {
		t.Fatalf("the two refusals differ, which makes this route a probe:\n  %s\n  %s", a, b)
	}
}

// problemWithoutTrace renders a Problem body with its per-request `trace_id`
// removed, so two refusals can be compared for what they DISCLOSE rather than
// for the id that necessarily differs between them.
func problemWithoutTrace(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	delete(body, "trace_id")
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("re-encode problem: %v", err)
	}
	return string(raw)
}

// TestCredentialResetRefusesANonUserTarget is SEC-012's subject restriction: a
// `screen` or `relay` principal authenticates by a mechanism this flow does not
// reset, and a grant that could never be usefully redeemed would be a live code
// with no legitimate redemption.
func TestCredentialResetRefusesANonUserTarget(t *testing.T) {
	e := newResetEnv(t, RoleAdmin)
	screen, err := e.store.CreatePrincipal(t.Context(), KindScreen, "lobby")
	if err != nil {
		t.Fatalf("create screen principal: %v", err)
	}
	if _, err := e.store.PutPasswordCredential(t.Context(), screen.PrincipalID, "lobby-screen", "pw"); err != nil {
		t.Fatalf("seed screen credential: %v", err)
	}
	rec := e.issue(t, e.token, map[string]any{"target_principal_id": screen.PrincipalID})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("issue for a screen = %d %s, want 422", rec.Code, rec.Body.String())
	}
}
