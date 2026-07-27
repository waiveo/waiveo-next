package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// The second-factor floor (SEC-004) driven through the REAL authenticated
// surface: the same mux, the same middleware, the same handlers a browser
// reaches. Every case below reaches its conclusion by making a request and
// reading the response, never by inspecting a UI affordance or asserting on an
// internal call — "the button is not rendered" is not evidence that a login
// cannot complete.

// enrolledSession is a harness whose owner principal has completed the whole
// enrollment flow over HTTP, plus the cookie jar of the session that did it.
type enrolledSession struct {
	*harness
	secret    []byte
	sessionID string
	token     string
	csrf      string
}

// signIn drives a password-only login and returns the session cookie and CSRF
// token from the response — everything a browser would then carry.
func (h *harness) signIn(t *testing.T, code string) (token, csrf, sessionID string) {
	t.Helper()
	rec := h.loginWithCode(t, h.ident, h.passwd, code)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s; want 200", rec.Code, rec.Body.String())
	}
	var body sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode login body: %v", err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("login set no session cookie")
	}
	return token, body.CSRFToken, body.SessionID
}

// authed builds a request carrying a session cookie and its CSRF token.
func authedRequest(method, path, body, token, csrf string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.RemoteAddr = "192.168.50.9:41234"
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	req.Header.Set(CSRFHeaderName, csrf)
	return req
}

// enrollOverHTTP runs the complete console enrollment flow — sign in, start the
// enrollment, return a code from it — through the mux, and returns the resulting
// state. It is the fixture every case below builds on, and it doubles as the
// end-to-end proof that the flow works as a console would drive it.
func enrollOverHTTP(t *testing.T) *enrolledSession {
	t.Helper()
	h := newHTTPHarness(t)
	token, csrf, sessionID := h.signIn(t, "")

	rec := h.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/enroll", "", token, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d %s; want 200", rec.Code, rec.Body.String())
	}
	var enroll totpEnrollmentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &enroll); err != nil {
		t.Fatalf("decode enrollment: %v", err)
	}
	if enroll.Secret == "" || !strings.HasPrefix(enroll.OtpauthURI, "otpauth://totp/") {
		t.Fatalf("enrollment body is not importable: %+v", enroll)
	}
	secret, err := DecodeTOTPSecret(enroll.Secret)
	if err != nil {
		t.Fatalf("the returned secret is not base32: %v", err)
	}

	// Nothing is armed yet: the credential relation must still hold no totp row.
	if armed, _ := h.store.HasTOTPCredential(context.Background(), h.owner.PrincipalID); armed {
		t.Fatal("starting an enrollment armed a credential; the confirming code has not been presented yet")
	}

	code := TOTPCode(secret, TOTPStep(h.clock.now()))
	body, _ := json.Marshal(totpConfirmRequest{Code: code})
	rec = h.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/confirm", string(body), token, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d %s; want 200", rec.Code, rec.Body.String())
	}
	var armed totpCredentialResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &armed); err != nil {
		t.Fatalf("decode confirmation: %v", err)
	}
	if armed.Kind != CredentialTOTP || armed.PrincipalID != h.owner.PrincipalID {
		t.Fatalf("armed credential = %+v; want a totp credential for the owner", armed)
	}
	if strings.Contains(rec.Body.String(), enroll.Secret) {
		t.Fatal("the confirmation response echoes the shared secret")
	}

	return &enrolledSession{harness: h, secret: secret, sessionID: sessionID, token: token, csrf: csrf}
}

// ---- the floor itself -----------------------------------------------------

// TestPasswordAloneCannotCompleteLoginOnceEnrolled is SEC-004's floor, proven by
// the login FAILING rather than by an absent form field: a principal holding a
// `totp` credential does not get a session from a password.
func TestPasswordAloneCannotCompleteLoginOnceEnrolled(t *testing.T) {
	e := enrollOverHTTP(t)

	rec := e.login(t, e.ident, e.passwd)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("password-only login after enrollment = %d %s; want 401", rec.Code, rec.Body.String())
	}
	if got := len(rec.Result().Cookies()); got != 0 {
		t.Fatalf("a refused login set %d cookie(s); nothing may be minted before the second factor", got)
	}
	// The refusal names the factor to collect, so a client knows to ask.
	var problem map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem["code"] != "UNAUTHENTICATED" {
		t.Fatalf("code = %v, want UNAUTHENTICATED", problem["code"])
	}
	if problem[secondFactorProblemMember] != secondFactorTOTP {
		t.Fatalf("the refusal must name the factor to collect; got %v", problem[secondFactorProblemMember])
	}
	// And no session row was created behind the scenes either — the absence of
	// an intermediate credential is structural, not merely un-returned.
	ids, err := e.store.ListSessionIDs(context.Background(), e.owner.PrincipalID)
	if err != nil {
		t.Fatalf("ListSessionIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != e.sessionID {
		t.Fatalf("live sessions = %v; only the pre-existing enrolling session may exist", ids)
	}
}

// TestSecondFactorCompletesLoginAndTokenCarriesAAL is SEC-021: the assurance
// level rides the TOKEN's own format, readable without a database lookup.
func TestSecondFactorCompletesLoginAndTokenCarriesAAL(t *testing.T) {
	e := enrollOverHTTP(t)
	// A step the enrollment did not spend.
	e.clock.advance(30_000)

	code := TOTPCode(e.secret, TOTPStep(e.clock.now()))
	rec := e.loginWithCode(t, e.ident, e.passwd, code)
	if rec.Code != http.StatusOK {
		t.Fatalf("login with a correct code = %d %s; want 200", rec.Code, rec.Body.String())
	}
	var body sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if body.AAL != AALStandard {
		t.Fatalf("session aal = %q, want %q", body.AAL, AALStandard)
	}

	var token string
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("a completed login set no session cookie")
	}
	// SEC-021: read the claim out of the token itself, with no store access.
	kind, aal, ok := ParseToken(token)
	if !ok {
		t.Fatalf("the minted token %q does not parse", token)
	}
	if kind != TokenKindSession || aal != AALStandard {
		t.Fatalf("token carries kind=%q aal=%q; want session/%s", kind, aal, AALStandard)
	}
	if !strings.HasPrefix(token, "wvs_std_") {
		t.Fatalf("the aal claim is not in the token's own format: %q", token)
	}
}

// TestWrongCodeIsIndistinguishableFromAWrongPassword is the non-disclosure
// requirement: a refused code must not tell the caller that the password was
// right, or a password list becomes verifiable against the login endpoint.
func TestWrongCodeIsIndistinguishableFromAWrongPassword(t *testing.T) {
	e := enrollOverHTTP(t)
	e.clock.advance(30_000)

	wrongCode := e.loginWithCode(t, e.ident, e.passwd, "000000")
	wrongPass := e.loginWithCode(t, e.ident, "not the password", "000000")

	if wrongCode.Code != http.StatusUnauthorized || wrongPass.Code != http.StatusUnauthorized {
		t.Fatalf("statuses = %d / %d; both must be 401", wrongCode.Code, wrongPass.Code)
	}
	normalize := func(rec *httptest.ResponseRecorder) string {
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode problem: %v", err)
		}
		delete(m, "trace_id")
		out, _ := json.Marshal(m)
		return string(out)
	}
	if a, b := normalize(wrongCode), normalize(wrongPass); a != b {
		t.Fatalf("a wrong code and a wrong password produce different Problems:\n  code: %s\n  pass: %s", a, b)
	}
}

// TestReplayOfAJustUsedCodeIsRefused is the replay defense end to end: the exact
// digits that just completed a login do not complete a second one, even though
// the clock has not left their window.
func TestReplayOfAJustUsedCodeIsRefused(t *testing.T) {
	e := enrollOverHTTP(t)
	e.clock.advance(30_000)

	code := TOTPCode(e.secret, TOTPStep(e.clock.now()))
	if rec := e.loginWithCode(t, e.ident, e.passwd, code); rec.Code != http.StatusOK {
		t.Fatalf("first use of the code = %d %s; want 200", rec.Code, rec.Body.String())
	}
	// Same clock reading, same digits, same window — and still refused.
	rec := e.loginWithCode(t, e.ident, e.passwd, code)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("replaying a just-used code = %d; want 401", rec.Code)
	}
	if got := len(rec.Result().Cookies()); got != 0 {
		t.Fatalf("a replayed code set %d cookie(s)", got)
	}
	// It is refused as a plain failure, disclosing nothing about WHY.
	if code := problemCode(t, rec); code != "UNAUTHENTICATED" {
		t.Fatalf("code = %q, want UNAUTHENTICATED", code)
	}
}

// TestConfirmingCodeCannotAlsoOpenASession closes the enrollment-side replay
// over HTTP: the code that armed the credential is spent from the credential's
// first instant.
func TestConfirmingCodeCannotAlsoOpenASession(t *testing.T) {
	e := enrollOverHTTP(t)
	confirming := TOTPCode(e.secret, TOTPStep(e.clock.now()))
	if rec := e.loginWithCode(t, e.ident, e.passwd, confirming); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the confirming code opened a session (%d); it must be spent at arming", rec.Code)
	}
}

// TestWrongCodeIsRateLimitedOnItsOwnCredential drives SEC-090 at the second
// factor: the lockout keys on the TOTP credential, so guessing codes burns the
// budget of the credential being guessed at, and the refusal is CREDENTIAL_LOCKED.
func TestWrongCodeIsRateLimitedOnItsOwnCredential(t *testing.T) {
	e := enrollOverHTTP(t)
	e.clock.advance(30_000)

	// The harness lockout tolerates 3 failures; the fourth crosses the threshold
	// and engages the backoff, so the fifth attempt is the one that is refused.
	for i := 0; i < 4; i++ {
		if rec := e.loginWithCode(t, e.ident, e.passwd, "000000"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d; want 401 while under the threshold", i, rec.Code)
		}
	}
	rec := e.loginWithCode(t, e.ident, e.passwd, "000000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the attempt past the threshold = %d; want 429", rec.Code)
	}
	if code := problemCode(t, rec); code != "CREDENTIAL_LOCKED" {
		t.Fatalf("code = %q, want CREDENTIAL_LOCKED (SEC-090)", code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("a lockout refusal must state its backoff")
	}
	// A locked second factor refuses even the CORRECT code — the lock stops the
	// verification work, it does not merely discard its result.
	good := TOTPCode(e.secret, TOTPStep(e.clock.now()))
	if rec := e.loginWithCode(t, e.ident, e.passwd, good); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a correct code during lockout = %d; want 429", rec.Code)
	}
	// And it lifts on the injected clock, never on a wall-clock sleep.
	e.clock.advance(60_000)
	after := TOTPCode(e.secret, TOTPStep(e.clock.now()))
	if rec := e.loginWithCode(t, e.ident, e.passwd, after); rec.Code != http.StatusOK {
		t.Fatalf("after the backoff elapsed, login = %d %s; want 200", rec.Code, rec.Body.String())
	}
}

// TestSecondFactorLockoutDoesNotLockThePasswordCredential is the "second place a
// credential can fail" hazard stated in SEC-090's own terms: lockout is per
// (credential, source-IP class), so exhausting the TOTP credential's budget must
// not lock the password credential — and must not open a bypass either.
func TestSecondFactorLockoutDoesNotLockThePasswordCredential(t *testing.T) {
	e := enrollOverHTTP(t)
	e.clock.advance(30_000)
	for i := 0; i < 4; i++ {
		e.loginWithCode(t, e.ident, e.passwd, "000000")
	}
	ctx := context.Background()
	pw, err := e.store.FindPasswordCredential(ctx, e.ident)
	if err != nil {
		t.Fatalf("FindPasswordCredential: %v", err)
	}
	totp, err := e.store.FindTOTPCredential(ctx, e.owner.PrincipalID)
	if err != nil {
		t.Fatalf("FindTOTPCredential: %v", err)
	}
	const ipClass = IPClassLAN
	if locked, _ := e.auth.lockout.Locked(LockoutKey(totp.CredentialID, ipClass), e.clock.now()); !locked {
		t.Fatal("the TOTP credential's own lockout key did not engage")
	}
	if locked, _ := e.auth.lockout.Locked(LockoutKey(pw.CredentialID, ipClass), e.clock.now()); locked {
		t.Fatal("guessing codes locked the PASSWORD credential; SEC-090 keys lockout per credential")
	}
}

// ---- audit ----------------------------------------------------------------

// TestSecondFactorEmitsItsOwnAuditEvents drives EVT-081/SEC-150 for the three
// acts this feature adds: enrollment, second-factor success, second-factor
// failure — each attributed to the acting principal.
func TestSecondFactorEmitsItsOwnAuditEvents(t *testing.T) {
	e := enrollOverHTTP(t)

	enrolled := e.sink.payloads(ActionTOTPEnrolled)
	if len(enrolled) != 1 {
		t.Fatalf("enrollment emitted %d audit.event(s); want 1 (SEC-150: a credential change)", len(enrolled))
	}
	if enrolled[0]["actor_principal"] != e.owner.PrincipalID {
		t.Fatalf("enrollment attributed to %v, want %s", enrolled[0]["actor_principal"], e.owner.PrincipalID)
	}
	if enrolled[0]["result"] != "success" {
		t.Fatalf("enrollment result = %v, want success", enrolled[0]["result"])
	}

	e.clock.advance(30_000)
	if rec := e.loginWithCode(t, e.ident, e.passwd, "000000"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code = %d; want 401", rec.Code)
	}
	fails := e.sink.payloads(ActionSecondFactorFailure)
	if len(fails) != 1 {
		t.Fatalf("a refused second factor emitted %d audit.event(s); want 1", len(fails))
	}
	if fails[0]["actor_principal"] != e.owner.PrincipalID || fails[0]["result"] != "failure" {
		t.Fatalf("second-factor failure record = %v", fails[0])
	}
	// SEC-091 via SEC-062: a time-windowed factor's failure must carry the
	// clock-accuracy assessment, or a burst caused by skew is undiagnosable.
	if fails[0]["clock_assessment"] != ClockUntrusted {
		t.Fatalf("second-factor failure clock_assessment = %v, want %q", fails[0]["clock_assessment"], ClockUntrusted)
	}
	// EVT-081 names "login failure" itself on the mandatory list, so the generic
	// record is emitted alongside the specific one.
	if len(e.sink.payloads(ActionLoginFailure)) != 1 {
		t.Fatal("a refused second factor must also register as a login failure (EVT-081)")
	}

	code := TOTPCode(e.secret, TOTPStep(e.clock.now()))
	if rec := e.loginWithCode(t, e.ident, e.passwd, code); rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s; want 200", rec.Code, rec.Body.String())
	}
	oks := e.sink.payloads(ActionSecondFactorSuccess)
	if len(oks) != 1 {
		t.Fatalf("a satisfied second factor emitted %d audit.event(s); want 1", len(oks))
	}
	if oks[0]["actor_principal"] != e.owner.PrincipalID || oks[0]["result"] != "success" {
		t.Fatalf("second-factor success record = %v", oks[0])
	}
}

// ---- enrollment surface ---------------------------------------------------

// TestEnrollmentEvictsEveryOtherSession: a session minted on one factor does not
// outlive the moment the principal decided one factor was not enough. The
// enrolling session survives, having just proven the new factor.
func TestEnrollmentEvictsEveryOtherSession(t *testing.T) {
	h := newHTTPHarness(t)
	ctx := context.Background()

	// A second session for the same principal — the "already signed in on the
	// laptop" case.
	other, err := h.store.MintSession(ctx, h.owner.PrincipalID, TokenKindSession, "", AALStandard, nil)
	if err != nil {
		t.Fatalf("MintSession: %v", err)
	}

	token, csrf, sessionID := h.signIn(t, "")
	rec := h.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/enroll", "", token, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d %s", rec.Code, rec.Body.String())
	}
	var enroll totpEnrollmentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &enroll)
	secret, _ := DecodeTOTPSecret(enroll.Secret)
	body, _ := json.Marshal(totpConfirmRequest{Code: TOTPCode(secret, TOTPStep(h.clock.now()))})
	rec = h.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/confirm", string(body), token, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d %s", rec.Code, rec.Body.String())
	}
	var armed totpCredentialResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &armed)
	if armed.RevokedSessions < 1 {
		t.Fatalf("revoked_sessions = %d; the other live session must have been evicted", armed.RevokedSessions)
	}

	if _, err := h.store.LookupSession(ctx, other.Token); err == nil {
		t.Fatal("a session minted before enrollment survived it")
	}
	ids, err := h.store.ListSessionIDs(ctx, h.owner.PrincipalID)
	if err != nil {
		t.Fatalf("ListSessionIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != sessionID {
		t.Fatalf("live sessions after enrollment = %v; only the enrolling session may survive", ids)
	}
	// And the surviving session still works.
	if rec := h.do(authedRequest(http.MethodGet, "/api/v1/auth/session", "", token, csrf)); rec.Code != http.StatusOK {
		t.Fatalf("the enrolling session was invalidated by its own enrollment (%d)", rec.Code)
	}
}

func TestEnrollmentRequiresAuthentication(t *testing.T) {
	h := newHTTPHarness(t)
	for _, path := range []string{"/api/v1/auth/totp/enroll", "/api/v1/auth/totp/confirm"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"code":"123456"}`))
		req.RemoteAddr = "192.168.50.9:41234"
		if rec := h.do(req); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a credential = %d; want 401", path, rec.Code)
		}
	}
}

// TestEnrollmentIsSubjectToCSRF: both routes are mutating and cookie-reachable,
// so SEC-024's double-submit applies — a cross-site page must not be able to
// start or arm an enrollment.
func TestEnrollmentIsSubjectToCSRF(t *testing.T) {
	h := newHTTPHarness(t)
	token, _, _ := h.signIn(t, "")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/totp/enroll", nil)
	req.RemoteAddr = "192.168.50.9:41234"
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	if rec := h.do(req); rec.Code != http.StatusForbidden {
		t.Fatalf("enroll without the CSRF header = %d; want 403 (SEC-024)", rec.Code)
	}
}

func TestConfirmWithoutAnEnrollmentIs404(t *testing.T) {
	h := newHTTPHarness(t)
	token, csrf, _ := h.signIn(t, "")
	body, _ := json.Marshal(totpConfirmRequest{Code: "123456"})
	rec := h.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/confirm", string(body), token, csrf))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("confirm with nothing in flight = %d; want 404", rec.Code)
	}
}

// TestExpiredEnrollmentIsIndistinguishableFromNoEnrollment drives the ttl
// through the real surface, and pins the shape of the refusal.
//
// The code presented is one that WOULD match — computed for the step the clock
// now reads — so nothing about the digits explains the refusal. The enrollment
// is refused because its window closed, and it is refused with the identical
// response an operator gets when they never started one: same status, same code,
// same detail, differing only in the per-request trace id. That equality is
// asserted against a SECOND request made after the row is provably gone, not
// against a string this test wrote down — the property under test is that the
// two paths agree, and a hardcoded expectation could not tell whether they did.
func TestExpiredEnrollmentIsIndistinguishableFromNoEnrollment(t *testing.T) {
	h := newHTTPHarness(t)
	token, csrf, _ := h.signIn(t, "")

	rec := h.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/enroll", "", token, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d %s; want 200", rec.Code, rec.Body.String())
	}
	var enroll totpEnrollmentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &enroll); err != nil {
		t.Fatalf("decode enrollment: %v", err)
	}
	secret, err := DecodeTOTPSecret(enroll.Secret)
	if err != nil {
		t.Fatalf("the returned secret is not base32: %v", err)
	}

	// Half an hour passes on the injected clock: no sleep, and the QR code the
	// operator is looking at is now stale.
	h.clock.advance(PendingTOTPEnrollmentTTLMs)

	code := TOTPCode(secret, TOTPStep(h.clock.now()))
	body, _ := json.Marshal(totpConfirmRequest{Code: code})
	expired := h.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/confirm", string(body), token, csrf))
	if expired.Code != http.StatusNotFound {
		t.Fatalf("confirming an expired enrollment = %d %s; want 404", expired.Code, expired.Body.String())
	}
	if armed, _ := h.store.HasTOTPCredential(context.Background(), h.owner.PrincipalID); armed {
		t.Fatal("an expired enrollment armed a second factor")
	}
	if pendingTOTPRows(t, h.store, h.owner.PrincipalID) != 0 {
		t.Fatal("the expired enrollment's sealed secret is still in the database")
	}
	if strings.Contains(expired.Body.String(), enroll.Secret) {
		t.Fatal("the refusal echoes the shared secret")
	}

	// The row is gone, so this second attempt is the genuine no-enrollment case.
	absent := h.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/confirm", string(body), token, csrf))
	if absent.Code != expired.Code {
		t.Fatalf("expired = %d but absent = %d; the two refusals must not be distinguishable", expired.Code, absent.Code)
	}
	if got, want := problemBodySansTrace(t, expired), problemBodySansTrace(t, absent); !reflect.DeepEqual(got, want) {
		t.Fatalf("the expired refusal %v differs from the no-enrollment refusal %v", got, want)
	}
}

// problemBodySansTrace decodes a problem body with the one member that is
// legitimately per-request removed, so two refusals can be compared for the
// difference that would actually leak something.
func problemBodySansTrace(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	delete(body, "trace_id")
	return body
}

func TestConfirmBodyValidationIs422(t *testing.T) {
	h := newHTTPHarness(t)
	token, csrf, _ := h.signIn(t, "")
	rec := h.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/confirm", `{"code":""}`, token, csrf))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an empty code = %d; want 422 (API-013a)", rec.Code)
	}
	if code := problemCode(t, rec); code != "VALIDATION_FAILED" {
		t.Fatalf("code = %q, want VALIDATION_FAILED", code)
	}
}

// TestConfirmWithAWrongCodeArmsNothing: a failed confirmation leaves the
// principal with no second factor and the enrollment still in flight, so a
// mistyped code costs a retry rather than the whole enrollment.
func TestConfirmWithAWrongCodeArmsNothing(t *testing.T) {
	h := newHTTPHarness(t)
	token, csrf, _ := h.signIn(t, "")
	rec := h.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/enroll", "", token, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d", rec.Code)
	}
	var enroll totpEnrollmentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &enroll)
	secret, _ := DecodeTOTPSecret(enroll.Secret)

	rec = h.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/confirm", `{"code":"000000"}`, token, csrf))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("confirm with a wrong code = %d; want 401", rec.Code)
	}
	if armed, _ := h.store.HasTOTPCredential(context.Background(), h.owner.PrincipalID); armed {
		t.Fatal("a failed confirmation armed a credential")
	}
	if len(h.sink.payloads(ActionTOTPEnrolled)) != 1 || h.sink.payloads(ActionTOTPEnrolled)[0]["result"] != "failure" {
		t.Fatal("a failed enrollment must still be audited (EVT-083)")
	}
	// The enrollment survives, so the right code still arms it.
	body, _ := json.Marshal(totpConfirmRequest{Code: TOTPCode(secret, TOTPStep(h.clock.now()))})
	if rec := h.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/confirm", string(body), token, csrf)); rec.Code != http.StatusOK {
		t.Fatalf("retrying with the right code = %d %s; want 200", rec.Code, rec.Body.String())
	}
}

// TestOrdinaryReEnrollmentIsRefusedOverHTTP pins SEC-052's boundary at the
// route: a live session is not authority to swap out the second factor.
func TestOrdinaryReEnrollmentIsRefusedOverHTTP(t *testing.T) {
	e := enrollOverHTTP(t)
	rec := e.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/enroll", "", e.token, e.csrf))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("re-enrolling over an armed credential = %d; want 403 (SEC-052)", rec.Code)
	}
	if code := problemCode(t, rec); code != "FORBIDDEN" {
		t.Fatalf("code = %q, want FORBIDDEN", code)
	}
}

// TestEnrollmentWithoutAWorkspaceKeyIsUnavailable pins the fail-closed edge at
// the route: no key, no enrollment — never an enrollment stored in the clear.
func TestEnrollmentWithoutAWorkspaceKeyIsUnavailable(t *testing.T) {
	h := newHTTPHarnessWithoutSealer(t)
	token, csrf, _ := h.signIn(t, "")
	rec := h.do(authedRequest(http.MethodPost, "/api/v1/auth/totp/enroll", "", token, csrf))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("enroll with no sealer = %d; want 503", rec.Code)
	}
	if code := problemCode(t, rec); code != "UNAVAILABLE" {
		t.Fatalf("code = %q, want UNAVAILABLE", code)
	}
}

// TestLoginWithoutTOTPIsUnchanged guards the rest of the suite's premise: a
// principal that has not enrolled still signs in with a password alone, so this
// feature adds a floor without moving the existing one.
func TestLoginWithoutTOTPIsUnchanged(t *testing.T) {
	h := newHTTPHarness(t)
	rec := h.login(t, h.ident, h.passwd)
	if rec.Code != http.StatusOK {
		t.Fatalf("login without an enrolled second factor = %d %s; want 200", rec.Code, rec.Body.String())
	}
	var problem map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &problem)
	if _, present := problem[secondFactorProblemMember]; present {
		t.Fatal("a successful login body carries a second-factor member")
	}
}

// TestUnknownIdentifierNeverNamesASecondFactor: the hint appears only after a
// correct password, so it can never be used to learn which identifiers exist or
// which of them have a second factor.
func TestUnknownIdentifierNeverNamesASecondFactor(t *testing.T) {
	e := enrollOverHTTP(t)
	rec := e.login(t, "nobody@example.test", "whatever")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown identifier = %d; want 401", rec.Code)
	}
	var problem map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &problem)
	if _, present := problem[secondFactorProblemMember]; present {
		t.Fatal("a refusal for an unknown identifier named a second factor")
	}
}
