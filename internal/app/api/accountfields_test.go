package api_test

import (
	"net/http"
	"testing"
)

// accountfields_test.go drives DAT-010 (account_state) and DAT-013
// (entitlements) through the live handler. Each rule has TWO halves that fail in
// opposite directions — mandatory on an org, forbidden on every other kind — and
// a test covering one half says nothing about the other, so all four are here.
//
// Both codes were published in data-model/1's taxonomy and emitted by nothing.
// The reason is worth keeping: ScopeNodeCreate declared neither member, so a
// server that required them would have refused every body the declared surface
// could produce. The schema had to be completed before the refusal could exist,
// which is why these arrive together.

// orgBody builds an org create body from parts, so a case can omit exactly one
// member and nothing else varies.
func orgBody(members map[string]any) map[string]any {
	b := map[string]any{"kind": "org", "name": "Account Fields Org"}
	for k, v := range members {
		b[k] = v
	}
	return b
}

func TestOrgMustCarryAccountState(t *testing.T) {
	e := newEnv(t)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes",
		mustJSON(t, orgBody(map[string]any{"entitlements": map[string]any{}})), nil)
	assertValidationError(t, resp, raw, "SCOPE_NODE_ACCOUNT_STATE_INVALID")

	// A value outside DAT-010's closed set is the same code — "missing or invalid"
	// is one row in the taxonomy — so a client cannot tell them apart by code and
	// must not have to: both remedies are "send a valid one".
	resp, raw = e.do(t, http.MethodPost, "/api/v1/scope-nodes",
		mustJSON(t, orgBody(map[string]any{"account_state": "bankrupt", "entitlements": map[string]any{}})), nil)
	assertValidationError(t, resp, raw, "SCOPE_NODE_ACCOUNT_STATE_INVALID")

	// And every member of the closed set is accepted, so the check is a set
	// membership test rather than a hardcoded "active".
	for _, state := range []string{"trial", "active", "suspended", "closed", "purged"} {
		e := newEnv(t) // one org per store (DAT-002)
		resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes",
			mustJSON(t, orgBody(map[string]any{"account_state": state, "entitlements": map[string]any{}})), nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("account_state %q = %d, want 201 (body %s)", state, resp.StatusCode, raw)
		}
	}
}

func TestNonOrgMustNotCarryAccountState(t *testing.T) {
	e := newEnv(t)
	org := e.createNode(t, orgNode("Root"))

	body := map[string]any{
		"kind": "site", "name": "Site", "parent_id": org,
		"tz": siteTZ, "lat": siteLat, "long": siteLong,
		"account_state": "active",
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, body), nil)
	assertValidationError(t, resp, raw, "SCOPE_NODE_ACCOUNT_STATE_INVALID")
}

func TestOrgMustCarryEntitlements(t *testing.T) {
	e := newEnv(t)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes",
		mustJSON(t, orgBody(map[string]any{"account_state": "active"})), nil)
	assertValidationError(t, resp, raw, "SCOPE_NODE_ENTITLEMENTS_INVALID")

	// A non-object is refused: DAT-013 says "an object", and the document's own
	// internal schema being defined elsewhere does not make its JSON type free.
	for _, bad := range []any{[]any{}, "active", 7, true} {
		e := newEnv(t)
		resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes",
			mustJSON(t, orgBody(map[string]any{"account_state": "active", "entitlements": bad})), nil)
		assertValidationError(t, resp, raw, "SCOPE_NODE_ENTITLEMENTS_INVALID")
	}

	// The EMPTY object is admitted explicitly by DAT-013 and is the value a
	// deployment with no tiering sends, so it is the one case a "must be
	// non-empty" reading would break.
	e = newEnv(t)
	resp, raw = e.do(t, http.MethodPost, "/api/v1/scope-nodes",
		mustJSON(t, orgBody(map[string]any{"account_state": "active", "entitlements": map[string]any{}})), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("org with entitlements {} = %d, want 201 (body %s)", resp.StatusCode, raw)
	}
}

func TestNonOrgMustNotCarryEntitlements(t *testing.T) {
	e := newEnv(t)
	org := e.createNode(t, orgNode("Root"))

	body := map[string]any{
		"kind": "site", "name": "Site", "parent_id": org,
		"tz": siteTZ, "lat": siteLat, "long": siteLong,
		"entitlements": map[string]any{"seats": 5},
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, body), nil)
	assertValidationError(t, resp, raw, "SCOPE_NODE_ENTITLEMENTS_INVALID")
}

// TestAPatchCannotClearTheOrgsAccountFields is the half a create-only rule
// misses, and the sharper one: the org already conforms, and a PATCH walks it out
// of conformance. Both members are patchable on this surface — that is what makes
// this reachable — so the rule has to hold on the merged row rather than on the
// request body alone.
func TestAPatchCannotClearTheOrgsAccountFields(t *testing.T) {
	for _, tc := range []struct{ name, member, code string }{
		{"account_state", "account_state", "SCOPE_NODE_ACCOUNT_STATE_INVALID"},
		{"entitlements", "entitlements", "SCOPE_NODE_ENTITLEMENTS_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			org := e.createNode(t, orgNode("Root"))

			resp, raw := e.do(t, http.MethodPatch, "/api/v1/scope-nodes/"+org,
				mustJSON(t, map[string]any{tc.member: nil}), map[string]string{"If-Match": `"1"`})
			assertValidationError(t, resp, raw, tc.code)

			// The refusal stored nothing: a valid org is still a valid org.
			get, graw := e.do(t, http.MethodGet, "/api/v1/scope-nodes/"+org, nil, nil)
			if get.StatusCode != http.StatusOK {
				t.Fatalf("read back the org: %d %s", get.StatusCode, graw)
			}
		})
	}
}

// TestAPatchCanMoveTheOrgBetweenAccountStates confirms the enforcement did not
// freeze the field it guards: DAT-011/012's own lifecycle moves an org through
// these states, so a rule that refused every change would break the thing it
// exists to protect.
func TestAPatchCanMoveTheOrgBetweenAccountStates(t *testing.T) {
	e := newEnv(t)
	org := e.createNode(t, orgNode("Root"))

	resp, raw := e.do(t, http.MethodPatch, "/api/v1/scope-nodes/"+org,
		mustJSON(t, map[string]any{"account_state": "suspended"}), map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch account_state active->suspended = %d, want 200 (body %s)", resp.StatusCode, raw)
	}

	resp, raw = e.do(t, http.MethodPatch, "/api/v1/scope-nodes/"+org,
		mustJSON(t, map[string]any{"entitlements": map[string]any{"seats": 9}}), map[string]string{"If-Match": `"2"`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch entitlements = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
}
