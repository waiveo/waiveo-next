package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// identityrows_test.go drives the two identity families — screens and
// adopted-devices — through the LIVE handler, proving they are addressable
// through the SAME api/1 conventions every other authored kind is, rather than
// merely returning 201 on create.
//
// The cases here deliberately cover the conventions a conformance corpus case
// cannot reach from the outside in one flow: the 422/400 validation split, the
// resource-scoped cursor's refusal of a foreign token, external_id uniqueness
// under a placement, and the visible-set read filter.

// fixtureEntityID is a canonical ULID naming the entity an adopted-device
// fixture exposes.
const fixtureEntityID = "01J8ZAENTTYMEDAPAYERAAAAA1"

// identityGroupNode is a group-kind scope node under parent — the placement a
// selector's subtree term is exercised against. The fixtures below are
// hand-built maps rather than typed structs on purpose: a create body has to be
// able to OMIT a member entirely, which a typed struct cannot express for every
// field, and omission is exactly what the declared-member defaults are about.
func identityGroupNode(parent string) datamodel.ScopeNode {
	return datamodel.ScopeNode{Kind: "group", ParentID: &parent, Name: "Demo Group"}
}

func screenFixture(scopeNode, name string, labels map[string]string) map[string]any {
	m := map[string]any{"name": name, "scope_node": scopeNode}
	if labels != nil {
		m["labels"] = labels
	}
	return m
}

func adoptedDeviceFixture(scopeNode, name, driver, nativeID string, labels map[string]string) map[string]any {
	m := map[string]any{
		"name":       name,
		"scope_node": scopeNode,
		"driver":     driver,
		"native_id":  nativeID,
		"entities": []any{map[string]any{
			"entity_id":    fixtureEntityID,
			"device_class": "media-player",
			"enabled":      true,
			"hidden":       false,
			"display_name": name,
			"category":     "primary",
		}},
	}
	if labels != nil {
		m["labels"] = labels
	}
	return m
}

// TestIdentityFamiliesFullCRUDRoundTrip walks a screen and an adopted device
// through create → read → conditional update → delete, asserting the api/1
// envelope at each step: a server-assigned ULID id, a Location header, an ETag
// that tracks the revision, and an If-Match-gated write.
func TestIdentityFamiliesFullCRUDRoundTrip(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))

	for _, tc := range []struct {
		path string
		body map[string]any
	}{
		{"/api/v1/screens", screenFixture(siteID, "Lobby Screen", nil)},
		{"/api/v1/adopted-devices", adoptedDeviceFixture(siteID, "Lobby Roku", "roku-ecp", "10.0.0.41", nil)},
	} {
		t.Run(tc.path, func(t *testing.T) {
			resp, raw := e.do(t, http.MethodPost, tc.path, mustJSON(t, tc.body), nil)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("create: %d %s", resp.StatusCode, raw)
			}
			id := decodeID(t, raw)
			if got, want := resp.Header.Get("ETag"), `"1"`; got != want {
				t.Errorf("create ETag = %q, want %q", got, want)
			}
			if got, want := resp.Header.Get("Location"), tc.path+"/"+id; got != want {
				t.Errorf("create Location = %q, want %q", got, want)
			}

			// A conditional update at the wrong ETag is refused; at the right
			// one it succeeds and advances the revision.
			bad := map[string]string{"If-Match": `"9"`}
			if resp, raw := e.do(t, http.MethodPatch, tc.path+"/"+id, mustJSON(t, map[string]any{"name": "Renamed"}), bad); resp.StatusCode != http.StatusPreconditionFailed {
				t.Fatalf("patch at a stale ETag: %d %s, want 412", resp.StatusCode, raw)
			}
			good := map[string]string{"If-Match": `"1"`}
			resp, raw = e.do(t, http.MethodPatch, tc.path+"/"+id, mustJSON(t, map[string]any{"name": "Renamed"}), good)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("patch at the current ETag: %d %s", resp.StatusCode, raw)
			}
			if got, want := resp.Header.Get("ETag"), `"2"`; got != want {
				t.Errorf("patch ETag = %q, want %q", got, want)
			}

			// A patch with no If-Match at all is 428, not a silent overwrite.
			if resp, raw := e.do(t, http.MethodPatch, tc.path+"/"+id, mustJSON(t, map[string]any{"name": "Unconditional"}), nil); resp.StatusCode != http.StatusPreconditionRequired {
				t.Fatalf("unconditional patch: %d %s, want 428", resp.StatusCode, raw)
			}

			if resp, raw := e.do(t, http.MethodDelete, tc.path+"/"+id, nil, map[string]string{"If-Match": `"2"`}); resp.StatusCode != http.StatusNoContent {
				t.Fatalf("delete: %d %s", resp.StatusCode, raw)
			}
			if resp, _ := e.do(t, http.MethodGet, tc.path+"/"+id, nil, nil); resp.StatusCode != http.StatusNotFound {
				t.Fatalf("read after delete: %d, want 404", resp.StatusCode)
			}
		})
	}
}

// TestIdentityFamiliesRejectClientSuppliedID pins API-105 on both families: a
// resource's id is server-assigned, and external_id is the client-assigned slot.
func TestIdentityFamiliesRejectClientSuppliedID(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))

	for _, path := range []string{"/api/v1/screens", "/api/v1/adopted-devices"} {
		body := map[string]any{"id": "01J8ZACLIENTSUPPLIEDIDAAA1", "name": "X", "scope_node": siteID}
		resp, raw := e.do(t, http.MethodPost, path, mustJSON(t, body), nil)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("POST %s with a client id: %d %s, want 422", path, resp.StatusCode, raw)
		}
	}
}

// TestIdentityFamiliesValidationSplit pins API-013a's status split on the new
// families: a BODY that fails validation is 422, a QUERY parameter that fails is
// 400 — never the other way round.
func TestIdentityFamiliesValidationSplit(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))

	// Body: an adopted device with no driver has half an identity tuple, which
	// relay/1 REL-063 makes a device_id's identity. api/openapi.yaml declares
	// `driver` as minLength 1, so this body no longer reaches the store: it is
	// refused against the declared schema before any write (bodyschema.go), 422
	// with the offending member named. The datamodel rule behind the same field
	// (DEVICE_IDENTITY_INCOMPLETE) still guards every non-HTTP write path and is
	// pinned by internal/datamodel's own tests.
	bad := adoptedDeviceFixture(siteID, "Nameless Driver", "", "10.0.0.41", nil)
	resp, raw := e.do(t, http.MethodPost, "/api/v1/adopted-devices", mustJSON(t, bad), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create with no driver: %d %s, want 422", resp.StatusCode, raw)
	}
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &problem); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, raw)
	}
	if problem.Code != "VALIDATION_FAILED" {
		t.Errorf("problem code = %q, want VALIDATION_FAILED", problem.Code)
	}
	if !strings.Contains(problem.Detail, "driver") {
		t.Errorf("the 422 body does not name the offending field: %s", raw)
	}

	// Query: a limit outside the accepted range is a 400, on both families.
	for _, path := range []string{"/api/v1/screens?limit=0", "/api/v1/adopted-devices?limit=abc"} {
		if resp, raw := e.do(t, http.MethodGet, path, nil, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s: %d %s, want 400", path, resp.StatusCode, raw)
		}
	}
}

// TestIdentityFamilyCursorIsResourceScoped pins API-033/035: a cursor minted by
// one family's list names a keyset position only in that family and is refused
// by another's, even though both ids are ULIDs from the same generator.
func TestIdentityFamilyCursorIsResourceScoped(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))
	e.createOK(t, "/api/v1/screens", mustJSON(t, screenFixture(siteID, "Screen A", nil)))
	e.createOK(t, "/api/v1/screens", mustJSON(t, screenFixture(siteID, "Screen B", nil)))
	e.createOK(t, "/api/v1/adopted-devices", mustJSON(t, adoptedDeviceFixture(siteID, "Device A", "roku-ecp", "10.0.0.41", nil)))

	_, cursor := listIdentityIDs(t, e, "/api/v1/screens?limit=1")
	if cursor == nil || *cursor == "" {
		t.Fatal("a first page of two screens minted no continuation cursor")
	}
	// The screens cursor pages the screens list...
	if ids, _ := listIdentityIDs(t, e, "/api/v1/screens?limit=1&cursor="+url.QueryEscape(*cursor)); len(ids) != 1 {
		t.Fatalf("page 2 of screens returned %d row(s), want 1", len(ids))
	}
	// ...and is refused by the adopted-devices list.
	resp, raw := e.do(t, http.MethodGet, "/api/v1/adopted-devices?cursor="+url.QueryEscape(*cursor), nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a screens cursor on the adopted-devices list: %d %s, want 400", resp.StatusCode, raw)
	}
}

// TestIdentityFamilySelectorNarrowsByLabelAndPlacement pins API-040/044 on the
// new families: a label term and a scope_node subtree term both select.
func TestIdentityFamilySelectorNarrowsByLabelAndPlacement(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))
	groupID := e.createNode(t, identityGroupNode(siteID))

	prod := decodeID(t, e.createOK(t, "/api/v1/screens", mustJSON(t, screenFixture(siteID, "Prod Screen", map[string]string{"env": "prod"}))))
	staging := decodeID(t, e.createOK(t, "/api/v1/screens", mustJSON(t, screenFixture(groupID, "Staging Screen", map[string]string{"env": "staging"}))))

	got, _ := listIdentityIDs(t, e, "/api/v1/screens?selector="+url.QueryEscape("env=prod"))
	if len(got) != 1 || got[0] != prod {
		t.Fatalf("selector env=prod returned %v, want [%s]", got, prod)
	}
	got, _ = listIdentityIDs(t, e, "/api/v1/screens?selector="+url.QueryEscape("scope_node="+groupID))
	if len(got) != 1 || got[0] != staging {
		t.Fatalf("selector scope_node=%s returned %v, want [%s]", groupID, got, staging)
	}
	// A subtree term selects both, since the group hangs under the site.
	got, _ = listIdentityIDs(t, e, "/api/v1/screens?selector="+url.QueryEscape("scope_node subtree "+siteID))
	if len(got) != 2 {
		t.Fatalf("subtree selector returned %v, want both screens", got)
	}
}

// TestIdentityFamilyExternalIDUniquePerPlacement pins API-101/102: an
// external_id is unique among rows of the same type under one scope node, and
// free to repeat under another.
func TestIdentityFamilyExternalIDUniquePerPlacement(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))
	groupID := e.createNode(t, identityGroupNode(siteID))

	first := screenFixture(siteID, "Lobby", nil)
	first["external_id"] = "lobby"
	e.createOK(t, "/api/v1/screens", mustJSON(t, first))

	dup := screenFixture(siteID, "Lobby Again", nil)
	dup["external_id"] = "lobby"
	resp, raw := e.do(t, http.MethodPost, "/api/v1/screens", mustJSON(t, dup), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a repeated external_id under one node: %d %s, want 400", resp.StatusCode, raw)
	}

	elsewhere := screenFixture(groupID, "Lobby Elsewhere", nil)
	elsewhere["external_id"] = "lobby"
	e.createOK(t, "/api/v1/screens", mustJSON(t, elsewhere))
}

// TestIdentityFamilyReadsAreScopeFiltered pins that the visible-set filter
// applies here exactly as it does everywhere else: a principal bound below the
// root neither lists nor reads a row placed outside its reach, and the refusal
// is a 404 rather than a 403 that would confirm the row exists.
func TestIdentityFamilyReadsAreScopeFiltered(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))
	groupA := e.createNode(t, identityGroupNode(siteID))
	groupB := e.createNode(t, identityGroupNode(siteID))

	inA := decodeID(t, e.createOK(t, "/api/v1/screens", mustJSON(t, screenFixture(groupA, "Screen in A", nil))))
	inB := decodeID(t, e.createOK(t, "/api/v1/screens", mustJSON(t, screenFixture(groupB, "Screen in B", nil))))

	alice := e.principalAt(t, groupA)
	got := listIdentityIDsAs(t, e, alice, "/api/v1/screens")
	if len(got) != 1 || got[0] != inA {
		t.Fatalf("a principal bound at group A listed %v, want [%s]", got, inA)
	}
	if resp, _ := e.as(t, alice, http.MethodGet, "/api/v1/screens/"+inB, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("reading an out-of-reach screen by id: %d, want 404", resp.StatusCode)
	}
}

// TestScreenDeviceLinkSurfacedAs422 pins that the cross-row screen→device
// reference (player/1 PLY-124) is enforced through the HTTP surface too: a
// dangling link is a 422 naming device_id, and nothing is stored.
func TestScreenDeviceLinkSurfacedAs422(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))

	body := screenFixture(siteID, "Linked Screen", nil)
	body["device_id"] = "01J8ZANEVERCREATEDDEVCEAA1"
	resp, raw := e.do(t, http.MethodPost, "/api/v1/screens", mustJSON(t, body), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("screen linked to an absent device: %d %s, want 422", resp.StatusCode, raw)
	}
	if ids, _ := listIdentityIDs(t, e, "/api/v1/screens"); len(ids) != 0 {
		t.Fatalf("a rejected create left %d screen row(s) behind", len(ids))
	}

	// The same link, to a device that DOES exist, is accepted.
	devID := decodeID(t, e.createOK(t, "/api/v1/adopted-devices", mustJSON(t, adoptedDeviceFixture(siteID, "Lobby Roku", "roku-ecp", "10.0.0.41", nil))))
	body["device_id"] = devID
	e.createOK(t, "/api/v1/screens", mustJSON(t, body))
}

// TestScopeNodeIDIsNeverResolvableAsAScreen is data-model/1 DAT-004a's
// substantive half, pinned on the surface that is the only place it can be
// pinned: a screen identity row and the scope node it is placed under are
// distinct rows with distinct ids, and a scope node's own id — including a
// SCREEN-kind node's, the one pairing that would otherwise read as the same
// thing — is never resolvable as a screen.
//
// What makes it hold is structural rather than a check: screen rows are their
// own store kind, so every screen_id resolution here is kind-scoped and a
// scope-node id is simply absent. That is worth a test precisely BECAUSE it is
// structural — nothing states it, so a later kind-crossing fallback (resolve a
// screen, and if that misses try a scope node) would look like a helpful
// convenience and would silently make a screen-kind node answer as the screen it
// classifies. The frozen corpus cannot reach this: it drives the identity-row
// validator, which is handed rows and never a reference, so its DAT-004a-shaped
// assertions are properties of the fixture and hold whatever any implementation
// does. That is why DAT-004a's traceability row does not claim coverage.
//
// The refusal asserted here is the ordinary 404 NOT_FOUND, which is what the
// platform answers today. data-model/1 publishes SCREEN_ID_UNRESOLVED for a
// screen_id that "resolves to a scope node", and nothing raises it — because on
// every mounted surface no such resolution happens. The status is deliberately
// not the point of this test: any 2xx here is the defect.
func TestScopeNodeIDIsNeverResolvableAsAScreen(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))
	screenKindNodeID := e.createNode(t, screenNode("", siteID, ""))
	groupID := e.createNode(t, identityGroupNode(siteID))

	// A real screen row, placed under the GROUP — so the screen-kind node below
	// classifies no row at all, and the two ids cannot be confused by accident.
	rowID := decodeID(t, e.createOK(t, "/api/v1/screens", mustJSON(t, screenFixture(groupID, "Lobby Screen", nil))))

	// The control: the row's own id does resolve, so a blanket 404 from a broken
	// mount could not pass this test by refusing everything.
	if resp, raw := e.do(t, http.MethodGet, "/api/v1/screens/"+rowID, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("a screen row's own id must resolve: %d %s", resp.StatusCode, raw)
	}

	for _, node := range []struct{ role, id string }{
		{"screen-kind", screenKindNodeID},
		{"site", siteID},
		{"group", groupID},
	} {
		for _, probe := range []struct {
			method, path string
			body         []byte
		}{
			{http.MethodGet, "/api/v1/screens/" + node.id, nil},
			{http.MethodPatch, "/api/v1/screens/" + node.id, mustJSON(t, map[string]any{"name": "Renamed"})},
			{http.MethodDelete, "/api/v1/screens/" + node.id, nil},
			{http.MethodPost, "/api/v1/screens/" + node.id + "/pairing-code", nil},
		} {
			resp, raw := e.do(t, probe.method, probe.path, probe.body, nil)
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				t.Errorf("%s %s: %d — the %s scope node's own id was accepted as a screen identity (DAT-004a): %s",
					probe.method, probe.path, resp.StatusCode, node.role, raw)
				continue
			}
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s: %d, want 404 — a scope-node id names no screen, so it must be refused as an unknown screen: %s",
					probe.method, probe.path, resp.StatusCode, raw)
				continue
			}
			var problem struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(raw, &problem); err != nil {
				t.Errorf("%s %s: decode problem body %s: %v", probe.method, probe.path, raw, err)
				continue
			}
			if problem.Code != "NOT_FOUND" {
				t.Errorf("%s %s: code = %q, want NOT_FOUND", probe.method, probe.path, problem.Code)
			}
		}
	}
}

// listIdentityIDs lists path as the environment's default principal and returns
// the item ids plus the page cursor.
func listIdentityIDs(t *testing.T, e *testEnv, path string) ([]string, *string) {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
	}
	return decodeItemIDs(t, raw)
}

// listIdentityIDsAs is listIdentityIDs driven as a specific principal.
func listIdentityIDsAs(t *testing.T, e *testEnv, who authtest.Credential, path string) []string {
	t.Helper()
	resp, raw := e.as(t, who, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
	}
	ids, _ := decodeItemIDs(t, raw)
	return ids
}

func decodeItemIDs(t *testing.T, raw []byte) ([]string, *string) {
	t.Helper()
	var page struct {
		Items  []struct{ ID string } `json:"items"`
		Cursor *string               `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode list page: %v (%s)", err, raw)
	}
	ids := make([]string, 0, len(page.Items))
	for _, it := range page.Items {
		ids = append(ids, it.ID)
	}
	return ids, page.Cursor
}
