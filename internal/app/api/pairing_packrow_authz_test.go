package api_test

import (
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
)

// TestPairingCodeIssuanceIsAuthorizedAtTheScreensNode pins the per-node write
// check on pairing-code issuance, which could be deleted with the tree green.
//
// This is the highest-value operation of the six that guard carried: it MINTS A
// SCREEN CREDENTIAL. The grant it returns is redeemable into a channel token
// that pulls that screen's program, so a caller who could mint one at a node it
// may only read would be provisioning hardware in someone else's site.
//
// Two bindings, per the correction on the sweep's own finding: the coarse role
// gate in auth's middleware refuses a POST on EFFECTIVE role alone, so a plain
// viewer never reaches this handler and a test built with one proves nothing.
func TestPairingCodeIssuanceIsAuthorizedAtTheScreensNode(t *testing.T) {
	dir, _ := pairingDirFixture(t, "192.0.2.40:7443")
	e := newEnvWithOptions(t, api.WithPairing(dir))
	tr := seedScopedTree(t, e)
	screenID := createScreenRow(t, e, tr.siteA)

	mixed := e.principalWith(t, roleAt{tr.siteA, auth.RoleViewer}, roleAt{tr.siteB, auth.RoleOperator})

	// It can genuinely SEE the screen, so the refusal below is about the verb.
	if resp, raw := e.as(t, mixed, http.MethodGet, "/api/v1/screens/"+screenID, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the caller cannot READ the screen (%d) — this test would then be asserting visibility (body %s)",
			resp.StatusCode, raw)
	}

	grantsBefore := len(desiredStateOf(t, e).PairingGrants)

	resp, raw := e.as(t, mixed, http.MethodPost, "/api/v1/screens/"+screenID+"/pairing-code", nil, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a caller with only READ authority at the screen's node minted a pairing code = %d, want 403 "+
			"(body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "FORBIDDEN")

	// The assertion that matters: nothing was minted. A refusal that still
	// produced a redeemable grant would have issued the credential and merely
	// declined to show it.
	if after := len(desiredStateOf(t, e).PairingGrants); after != grantsBefore {
		t.Fatalf("a refused issuance still minted a grant: desired-state pairing_grants %d -> %d", grantsBefore, after)
	}
}

// TestPairingCodeIssuanceSucceedsWithWriteAuthority is the control: without it a
// guard refusing everyone satisfies the test above while making screens
// unpairable.
func TestPairingCodeIssuanceSucceedsWithWriteAuthority(t *testing.T) {
	dir, _ := pairingDirFixture(t, "192.0.2.40:7443")
	e := newEnvWithOptions(t, api.WithPairing(dir))
	tr := seedScopedTree(t, e)
	screenID := createScreenRow(t, e, tr.siteA)

	operator := e.principalWith(t, roleAt{tr.siteA, auth.RoleOperator})
	resp, raw := e.as(t, operator, http.MethodPost, "/api/v1/screens/"+screenID+"/pairing-code", nil, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a principal that DOES hold write authority at the screen's node was refused = %d, want 201 (body %s)",
			resp.StatusCode, raw)
	}
}

// TestPackRowWritesAreAuthorizedAtTheRowsNode pins packRowWritable, the helper
// both the pack-row create and patch paths share. It too could be deleted with
// the tree green.
//
// A pack's rows carry the universal envelope's scope_node (MAN-052), so they are
// authorized exactly as a first-party resource is — a pack does not get a
// private door around the visible set because its schema is declared rather than
// built in.
func TestPackRowWritesAreAuthorizedAtTheRowsNode(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)
	mixed := e.principalWith(t, roleAt{rowScopeNodeA, auth.RoleViewer}, roleAt{rowScopeNodeB, auth.RoleOperator})

	body := mustJSON(t, map[string]any{
		"scope_node": rowScopeNodeA, "name": "Burger", "price": 9.5, "available": true, "section": "Mains",
	})
	resp, raw := e.as(t, mixed, http.MethodPost, menuRowsPath, body, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a caller with only READ authority at the row's node created a pack row = %d, want 403 (body %s)",
			resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "FORBIDDEN")

	// The control: write authority at that node still creates.
	operator := e.principalWith(t, roleAt{rowScopeNodeA, auth.RoleOperator})
	if resp, raw := e.as(t, operator, http.MethodPost, menuRowsPath, body, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("a principal that DOES hold write authority at the row's node was refused = %d, want 201 (body %s)",
			resp.StatusCode, raw)
	}
}
