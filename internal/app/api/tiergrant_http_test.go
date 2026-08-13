package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth"
)

// The tier-grant ceremony over HTTP (SEC-037): the host mints in-process at pack
// start, and the pack process exchanges the code here for the session it makes
// every subsequent call with.
//
// Driven end to end rather than against the store, because the properties that
// only exist at the route are the ones worth pinning: that a caller with no
// credential can reach it at all, that the token it returns actually
// authenticates, and that the code is spent by its first use over the wire.

// mintTierGrantFor mints a tier grant the way the host will — in-process, never
// over HTTP. There is deliberately no route that issues one: a mint endpoint
// would be a way to ask the platform for a pack identity from outside.
func mintTierGrantFor(t *testing.T, e *testEnv, packID string) string {
	t.Helper()
	g, err := e.auth.Store.MintTierGrant(t.Context(), packID, rowScopeNodeA, auth.RoleOperator)
	if err != nil {
		t.Fatalf("MintTierGrant(%s): %v", packID, err)
	}
	return g.Code
}

func redeemTierGrant(t *testing.T, e *testEnv, code string) (*http.Response, map[string]any) {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/auth/tier-grant/redeem",
		mustJSON(t, map[string]any{"code": code}), nil)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

// A pack redeems its start-time code and receives a session that WORKS. The
// last assertion is the one that matters: a token that does not authenticate
// would make every other field decoration.
func TestAPackRedeemsItsTierGrantAndTheTokenAuthenticates(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)

	resp, body := redeemTierGrant(t, e, mintTierGrantFor(t, e, "waiveo/backups"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("redeem status = %d, want 201 (%+v)", resp.StatusCode, body)
	}
	if body["pack_id"] != "waiveo/backups" {
		t.Fatalf("pack_id = %v, want waiveo/backups", body["pack_id"])
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("redemption returned no bearer token: %+v", body)
	}

	// Present it the way a pack will — as a bearer credential, with no cookie
	// and no CSRF token, because a pack is not a browser. Built raw rather than
	// through e.do, which attaches the env's own session centrally and would
	// authenticate the call for reasons that have nothing to do with the token.
	req, err := http.NewRequest(http.MethodGet, e.ts.URL+"/api/v1/scope-nodes", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	got, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authenticated call: %v", err)
	}
	defer got.Body.Close()
	if got.StatusCode == http.StatusUnauthorized {
		t.Fatal("the token a pack was just handed does not authenticate")
	}
}

// The route is reachable WITHOUT a credential (API-090/091). A pack that had to
// authenticate to acquire an identity could never acquire one.
func TestTheTierGrantRouteNeedsNoPriorCredential(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	code := mintTierGrantFor(t, e, "waiveo/backups")

	// Built raw and deliberately NOT passed through e.do: that helper attaches
	// the env's session centrally, which is exactly the credential this test
	// asserts is unnecessary.
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+"/api/v1/auth/tier-grant/redeem",
		bytes.NewReader(mustJSON(t, map[string]any{"code": code})))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("unauthenticated redeem = %d, want 201 — the caller has no credential by construction", resp.StatusCode)
	}
}

// The code is spent by its first use, over the wire. A replayable code handed to
// a child process would be a long-lived credential wearing a one-time costume.
func TestATierGrantCodeIsNotReplayableOverHTTP(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	code := mintTierGrantFor(t, e, "waiveo/backups")

	if resp, body := redeemTierGrant(t, e, code); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first redeem = %d, want 201 (%+v)", resp.StatusCode, body)
	}
	resp, body := redeemTierGrant(t, e, code)
	if resp.StatusCode == http.StatusCreated {
		t.Fatalf("the same code redeemed TWICE over HTTP (%+v)", body)
	}
}

// A wrong code is refused, and the refusal says nothing about which part was
// wrong. There is also no route that mints one — asserted here so that a future
// mint endpoint has to break this test to exist.
func TestAnUnknownTierGrantCodeIsRefusedAndNoRouteMintsOne(t *testing.T) {
	e := newEnv(t)
	resp, _ := redeemTierGrant(t, e, "not-a-real-code")
	if resp.StatusCode == http.StatusCreated {
		t.Fatal("an unknown code minted a session")
	}

	// The host mints in-process. A POST that tries to issue a grant must not
	// find a handler.
	mint, _ := e.do(t, http.MethodPost, "/api/v1/auth/tier-grant", mustJSON(t, map[string]any{"pack_id": "waiveo/backups"}), nil)
	if mint.StatusCode != http.StatusNotFound && mint.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /auth/tier-grant = %d; nothing may mint a pack identity over HTTP", mint.StatusCode)
	}
}

// SEC-033's attempt budget bounds guessing at this route. Without it the code
// is a 128-bit secret with unlimited attempts, which is a different security
// property from the one SEC-032 thinks it bought.
//
// Asserted as "a sweep is eventually refused with RATE_LIMITED", not as an exact
// attempt count: the budget's size is deployment tuning, and a test that pinned
// it would fail on a legitimate retune rather than on a lost bound.
func TestTierGrantRedemptionIsRateLimited(t *testing.T) {
	e := newEnv(t)

	var limited bool
	for i := 0; i < 40 && !limited; i++ {
		resp, body := redeemTierGrant(t, e, "wrong-code-guess")
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
			if body["code"] != "RATE_LIMITED" {
				t.Fatalf("throttled response code = %v, want RATE_LIMITED", body["code"])
			}
		}
	}
	if !limited {
		t.Fatal("40 wrong codes in a row were never rate-limited — the SEC-033 budget is not bounding this route")
	}
}

// A missing code is a FIELD error naming `code`, not a generic refusal. The
// distinction matters because the two failures have different remedies: a
// malformed request is the caller's bug, an unknown code is a stale or spent
// grant, and a pack author debugging a start failure needs to know which.
func TestAMissingTierGrantCodeIsAFieldError(t *testing.T) {
	e := newEnv(t)
	resp, body := redeemTierGrant(t, e, "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty code = %d, want 422 (%+v)", resp.StatusCode, body)
	}
	errs, _ := body["errors"].([]any)
	var named bool
	for _, e := range errs {
		if m, ok := e.(map[string]any); ok && m["field"] == "code" {
			named = true
		}
	}
	if !named {
		t.Fatalf("422 does not name the `code` field: %+v", body)
	}
}
