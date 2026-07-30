package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// twoPrincipalEnv is an api/1 server plus TWO distinct authenticated callers on
// one auth store. Both hold `owner` at the workspace root, so nothing either one
// is refused can be blamed on authorization — the only difference between them
// is WHO they are, which is exactly the variable these tests isolate.
type twoPrincipalEnv struct {
	// testEnv drives as alice, so every seeding helper this package already has
	// (createNode, uploadContent, createOK) works unchanged.
	*testEnv
	alice authtest.Credential
	bob   authtest.Credential
}

func newTwoPrincipalEnv(t *testing.T) *twoPrincipalEnv {
	t.Helper()
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clock := func() int64 { return fixedNowMs }
	fixture, err := authtest.New(authtest.Config{NowMs: clock})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	t.Cleanup(fixture.Close)
	bob, err := fixture.AddPrincipal(authtest.Config{})
	if err != nil {
		t.Fatalf("AddPrincipal: %v", err)
	}
	alice := fixture.Credential()
	if alice.PrincipalID == bob.PrincipalID {
		t.Fatal("the two seeded principals must be distinct")
	}

	content := origin.New()
	ts := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock,
		ulid.Monotonic(), content, testContentBase, fixture.Auth))
	t.Cleanup(ts.Close)
	return &twoPrincipalEnv{
		testEnv: &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture},
		alice:   alice,
		bob:     bob,
	}
}

// as drives one request as the named principal, rather than as the env's own
// seeding identity (testEnv.do). It hangs off testEnv so every multi-principal
// fixture in this package — this file's two root-bound callers and
// scope_visibility_test.go's scope-bound ones — drives requests through one
// helper and no test hand-rolls credential attachment.
func (e *testEnv) as(t *testing.T, who authtest.Credential, method, path string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	who.Authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, raw
}

// TestIdempotencyKeyIsScopedPerPrincipal drives API-051: "An Idempotency-Key is
// scoped to the tuple (authenticated principal, HTTP method, request path). The
// identical key value presented by a DIFFERENT principal, or against a different
// method or path, MUST be treated as an unrelated, fresh request — never as a
// replay."
//
// This is the requirement a fixed, shared principal made untestable and quietly
// false: with one identity for every caller, Bob's request would have replayed
// Alice's stored response and returned HER resource, silently, with a 201. The
// test therefore checks three things, and the third is the one that matters —
// not merely that Bob got a 2xx, but that a SECOND, DISTINCT resource actually
// exists afterward. A replay would also return 201; only the resource count
// distinguishes "executed" from "replayed".
func TestIdempotencyKeyIsScopedPerPrincipal(t *testing.T) {
	e := newTwoPrincipalEnv(t)
	screenID := seedSchedulingScope(t, e.testEnv)

	// The identical key AND the identical body from both callers.
	const key = "shared-idempotency-key-1"
	body := mustJSON(t, playlistFixture(screenID, nil))
	headers := map[string]string{"Idempotency-Key": key, "Content-Type": "application/json"}

	aliceResp, aliceRaw := e.as(t, e.alice, http.MethodPost, "/api/v1/playlists", body, headers)
	if aliceResp.StatusCode != http.StatusCreated {
		t.Fatalf("alice's create = %d, want 201 (body %s)", aliceResp.StatusCode, aliceRaw)
	}
	aliceID := decodeID(t, aliceRaw)

	// Alice replaying her OWN key is a replay: same scope, same body, so the
	// retained response comes back verbatim and nothing is created (API-052).
	// This half is the control — without it, a Bob-executes result could be
	// explained by idempotency simply not working at all.
	_, aliceReplayRaw := e.as(t, e.alice, http.MethodPost, "/api/v1/playlists", body, headers)
	if got := decodeID(t, aliceReplayRaw); got != aliceID {
		t.Fatalf("alice's own repeat must REPLAY her retained response (API-052); got id %q want %q", got, aliceID)
	}

	// Bob presenting the same key and the same body is an unrelated, FRESH
	// request (API-051) — a different principal, therefore a different scope.
	bobResp, bobRaw := e.as(t, e.bob, http.MethodPost, "/api/v1/playlists", body, headers)
	if bobResp.StatusCode != http.StatusCreated {
		t.Fatalf("bob's create with alice's key = %d, want 201 (body %s)", bobResp.StatusCode, bobRaw)
	}
	bobID := decodeID(t, bobRaw)
	if bobID == aliceID {
		t.Fatalf("bob's request replayed alice's retained response: both returned id %q. "+
			"API-051 scopes an Idempotency-Key to (authenticated principal, method, path), so a different "+
			"principal presenting the same key MUST execute afresh", bobID)
	}

	// The decisive check: two resources exist, so Bob's request genuinely
	// EXECUTED rather than replaying a 201 it did not cause. A replay returns
	// 201 with a body too — only the resource count separates the two.
	listResp, listRaw := e.as(t, e.alice, http.MethodGet, "/api/v1/playlists", nil, nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d, want 200", listResp.StatusCode)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(listRaw, &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("two principals presenting one key must have created TWO playlists (API-051); got %d: %s",
			len(page.Items), listRaw)
	}
}

// TestExternalIDCollidesAcrossPrincipalsUnderOneScopeNode drives API-101, and
// deliberately drives it in the direction that is easy to get backwards.
//
// API-101: "external_id, when set, MUST be unique among resources of the same
// type placed under the same scope node; the same value used by a different
// resource type, or by the same type under a different scope node, is not a
// collision." The scoping tuple is (resource type, scope node) — the PRINCIPAL
// is not part of it. So two different callers writing the same external_id under
// one scope node DO collide, and the second is refused 400 /
// EXTERNAL_ID_CONFLICT (API-102).
//
// This is the exact opposite of API-051's rule one function above, which is why
// both are pinned here together: an implementation that "helpfully" carried the
// principal into external_id scoping would let two users each claim `lobby-tv`
// under one site, and every cross-reference resolving that external_id (API-103)
// would then have two candidates and no rule to choose between them.
func TestExternalIDCollidesAcrossPrincipalsUnderOneScopeNode(t *testing.T) {
	e := newTwoPrincipalEnv(t)
	screenID := seedSchedulingScope(t, e.testEnv)

	playlist := func(scopeNode string) []byte {
		p := playlistFixture(scopeNode, nil)
		p.ExternalID = "lobby-loop"
		return mustJSON(t, p)
	}

	aliceResp, aliceRaw := e.as(t, e.alice, http.MethodPost, "/api/v1/playlists", playlist(screenID), nil)
	if aliceResp.StatusCode != http.StatusCreated {
		t.Fatalf("alice's playlist = %d, want 201 (body %s)", aliceResp.StatusCode, aliceRaw)
	}

	// Bob, a DIFFERENT principal, same external_id, same scope node, same
	// resource type: a collision, because API-101's scope tuple contains no
	// principal.
	bobResp, bobRaw := e.as(t, e.bob, http.MethodPost, "/api/v1/playlists", playlist(screenID), nil)
	if bobResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a second principal reusing an external_id under the SAME scope node must be refused "+
			"400 / EXTERNAL_ID_CONFLICT (API-101/102) — external_id is scoped by (resource type, scope node), "+
			"NOT by principal; got %d (body %s)", bobResp.StatusCode, bobRaw)
	}
	assertProblem(t, bobResp, bobRaw, "EXTERNAL_ID_CONFLICT")

	// The other half of API-101, for contrast: the same external_id under a
	// DIFFERENT scope node is not a collision — which is what proves the refusal
	// above came from the scope node, and not from a blanket global uniqueness
	// rule that would be just as wrong in the opposite direction.
	otherScreenID := e.createNode(t, screenNode("", parentOf(t, e.testEnv, screenID), ""))
	otherResp, otherRaw := e.as(t, e.bob, http.MethodPost, "/api/v1/playlists", playlist(otherScreenID), nil)
	if otherResp.StatusCode != http.StatusCreated {
		t.Fatalf("the same external_id under a DIFFERENT scope node is not a collision (API-101); got %d (body %s)",
			otherResp.StatusCode, otherRaw)
	}
}

// parentOf reads a scope node's parent_id, so a sibling can be created under the
// same parent without the test hardcoding the seed's own tree shape.
func parentOf(t *testing.T, e *testEnv, nodeID string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, "/api/v1/scope-nodes/"+nodeID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET scope node %s = %d", nodeID, resp.StatusCode)
	}
	var n struct {
		ParentID *string `json:"parent_id"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatalf("decode scope node: %v", err)
	}
	if n.ParentID == nil {
		t.Fatalf("scope node %s has no parent", nodeID)
	}
	return *n.ParentID
}

// TestJobCreatedByIsTheAuthenticatedPrincipal drives API-112: a Job carries
// `created_by` (Principal). With the fixed POC principal gone, the field names
// whoever actually submitted the job — so two principals submitting the same
// operation produce two differently-attributed Jobs.
func TestJobCreatedByIsTheAuthenticatedPrincipal(t *testing.T) {
	e := newTwoPrincipalEnv(t)

	body := mustJSON(t, map[string]any{"selector": "env=prod", "enabled": true})
	for _, who := range []authtest.Credential{e.alice, e.bob} {
		resp, raw := e.as(t, who, http.MethodPost, "/api/v1/automations/bulk-enable", body, nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("bulk-enable = %d, want 202 (body %s)", resp.StatusCode, raw)
		}
		var job struct {
			CreatedBy string `json:"created_by"`
		}
		if err := json.Unmarshal(raw, &job); err != nil {
			t.Fatalf("decode Job: %v", err)
		}
		if job.CreatedBy != who.PrincipalID {
			t.Fatalf("Job created_by = %q, want the submitting principal %q (API-112)", job.CreatedBy, who.PrincipalID)
		}
	}
}

// TestAPIV1RoutesRefuseAnUnauthenticatedCaller is the surface-level statement of
// SEC-005 over the REAL mux: every family under /api/v1 is refused without a
// credential, and only the three `security: []` credential-exchange operations
// (API-090/091) are reachable — which is what makes "the surface is
// authenticated" a checked property rather than a claim about the routes anyone
// happened to remember to protect.
func TestAPIV1RoutesRefuseAnUnauthenticatedCaller(t *testing.T) {
	e := newTwoPrincipalEnv(t)

	protected := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/scope-nodes"},
		{http.MethodPost, "/api/v1/scope-nodes"},
		{http.MethodGet, "/api/v1/schedules"},
		{http.MethodGet, "/api/v1/dayparts"},
		{http.MethodGet, "/api/v1/playlists"},
		{http.MethodGet, "/api/v1/automations"},
		{http.MethodPost, "/api/v1/automations/bulk-enable"},
		{http.MethodGet, "/api/v1/devices"},
		{http.MethodGet, "/api/v1/entities"},
		{http.MethodGet, "/api/v1/packs"},
		{http.MethodPost, "/api/v1/content"},
		{http.MethodPost, "/api/v1/auth/logout"},
		{http.MethodGet, "/api/v1/auth/session"},
		// The credential-reset ISSUING route. Its sibling below is deliberately
		// exempt; this one must not be, or anyone could mint a reset code for
		// anyone. The pair is the point — an exemption that widened by one path
		// segment would be caught here rather than by a reader.
		{http.MethodPost, "/api/v1/auth/credential-reset"},
	}
	for _, r := range protected {
		req, err := http.NewRequest(r.method, e.ts.URL+r.path, bytes.NewReader([]byte("{}")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do %s %s: %v", r.method, r.path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s must refuse an unauthenticated caller with 401 (SEC-005); got %d (body %s)",
				r.method, r.path, resp.StatusCode, raw)
		}
	}

	// The three credential-exchange operations are reachable WITHOUT a credential
	// (API-090/091) — they are refused on their own merits (a bad body, a bad
	// code), never for lacking the session they exist to mint.
	//
	// credential-reset/redeem is the third and the newest: its caller is by
	// definition the person who cannot sign in. Its authenticated SIBLING (the
	// issuing route, /auth/credential-reset with no suffix) is asserted in the
	// list ABOVE, so this pair pins that the exemption covers the redeem route
	// and stops there — an exemption that widened to the issuing route would let
	// anyone mint a reset code for anyone.
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/setup", "/api/v1/auth/credential-reset/redeem"} {
		req, err := http.NewRequest(http.MethodPost, e.ts.URL+path, bytes.NewReader([]byte("{}")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do POST %s: %v", path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Fatalf("POST %s declares security: [] (API-090/091) and MUST NOT require a pre-existing session; got 401 (body %s)",
				path, raw)
		}
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("POST %s with an empty body must be 422 on its own body validation (API-013a); got %d (body %s)",
				path, resp.StatusCode, raw)
		}
	}
}

// TestCSRFIsRequiredOnTheRealAPIMux pins SEC-024 over the shipped mux rather
// than only over the auth package's own test harness: a mutating request
// carrying a valid session cookie but no double-submit token is refused.
func TestCSRFIsRequiredOnTheRealAPIMux(t *testing.T) {
	e := newTwoPrincipalEnv(t)

	req, err := http.NewRequest(http.MethodPost, e.ts.URL+"/api/v1/scope-nodes",
		bytes.NewReader(mustJSON(t, map[string]any{"kind": "org", "parent_id": nil, "name": "X", "labels": map[string]string{},
			"account_state": "active", "entitlements": map[string]any{}})))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// The cookie alone — no X-CSRF-Token header.
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: e.alice.Token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a mutating cookie-authenticated request without the double-submit CSRF token must be 403 "+
			"(SEC-024: SameSite alone is explicitly not sufficient); got %d (body %s)", resp.StatusCode, raw)
	}
}
