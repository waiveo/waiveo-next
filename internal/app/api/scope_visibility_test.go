package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// scope_visibility_test.go drives the per-principal visible scope-node set over
// the REAL /api/v1 mux: which rows a caller may see is decided by their role
// bindings and SEC-010's inheritance rule, not by which rows happen to be in the
// store. Every case here seeds ONE tree with ONE root-bound identity and then
// varies only WHO is asking — so a difference in what comes back can only be the
// scoping, never the data.

// absentScopeNodeID is a fixture ULID (Crockford base32, no secrets) that is
// deliberately never created, so a read of it exercises a genuinely nonexistent
// resource — the answer an out-of-reach resource's read must be
// indistinguishable from.
const absentScopeNodeID = "01J8Z0ABSENTSCPNDE000000ZZ"

// scopedTree is the fixture tree every case below is driven against:
//
//	org
//	├── siteA ── screenA1 screenA2 screenA3 screenA4
//	└── siteB ── screenB1 screenB2 screenB3 screenB4
//
// The two subtrees are DISJOINT and each is deep enough (a site plus four
// screens) that a page limit smaller than one subtree is meaningful. The screens
// are created ALTERNATING between the two sites, so the A-rows and B-rows
// interleave in id (creation) order — which is what makes the pagination case
// below load-bearing: if the visible-set filter ran after the keyset window were
// cut, an interleaved collection would return short pages.
type scopedTree struct {
	org      string
	siteA    string
	siteB    string
	screensA []string
	screensB []string
}

// all returns every node id in the tree, in creation (id) order.
func (tr scopedTree) all() []string {
	ids := []string{tr.org, tr.siteA, tr.siteB}
	for i := range tr.screensA {
		ids = append(ids, tr.screensA[i], tr.screensB[i])
	}
	return ids
}

// subtreeA / subtreeB are the node sets a principal bound at the respective site
// can read: the site itself plus its screens (SEC-010 — a binding applies to its
// node AND its descendants).
func (tr scopedTree) subtreeA() []string { return append([]string{tr.siteA}, tr.screensA...) }
func (tr scopedTree) subtreeB() []string { return append([]string{tr.siteB}, tr.screensB...) }

// seedScopedTree builds the tree AS THE ENV'S OWN root-bound principal, so every
// row genuinely exists before any scope-bound caller looks for it. A case that
// seeded through the same principal it later reads as could not tell "the row is
// invisible" apart from "the row was never written".
func seedScopedTree(t *testing.T, e *testEnv) scopedTree {
	t.Helper()
	tr := scopedTree{}
	tr.org = e.createNode(t, datamodel.ScopeNode{Kind: "org", Name: "Demo Org"})
	site := func(name string) string {
		return e.createNode(t, datamodel.ScopeNode{
			Kind: "site", ParentID: strp(tr.org), Name: name,
			TZ: strp(siteTZ), Lat: f64p(siteLat), Long: f64p(siteLong),
		})
	}
	tr.siteA = site("Site A")
	tr.siteB = site("Site B")
	// Alternating, so the two subtrees interleave in id order.
	for i := 0; i < 4; i++ {
		tr.screensA = append(tr.screensA, e.createNode(t, screenNode("", tr.siteA, "")))
		tr.screensB = append(tr.screensB, e.createNode(t, screenNode("", tr.siteB, "")))
	}
	return tr
}

// principalAt seeds a NEW principal holding `owner` at each of the named scope
// nodes and returns its credential.
//
// `owner` is deliberate: it is the strongest role SEC-010 defines, so nothing
// these cases observe can be explained by the coarse per-method role floor the
// middleware applies (auth.RequiredRole). The ONLY thing separating these
// callers from the env's root-bound seeder is WHERE their authority is bound —
// which is exactly the variable under test.
func (e *testEnv) principalAt(t *testing.T, nodes ...string) authtest.Credential {
	t.Helper()
	if len(nodes) == 0 {
		t.Fatal("principalAt: a principal with no binding is refused 403 by the middleware; name at least one node")
	}
	cred, err := e.auth.AddPrincipal(authtest.Config{ScopeNode: nodes[0], Role: auth.RoleOwner})
	if err != nil {
		t.Fatalf("AddPrincipal at %s: %v", nodes[0], err)
	}
	for _, node := range nodes[1:] {
		if _, err := e.auth.Store.PutRoleBinding(context.Background(), cred.PrincipalID, node, auth.RoleOwner); err != nil {
			t.Fatalf("PutRoleBinding at %s: %v", node, err)
		}
	}
	return cred
}

// listScopeNodeIDs drives one scope-nodes list as who and returns the page's ids
// and its continuation cursor, failing on any non-200.
func (e *testEnv) listScopeNodeIDs(t *testing.T, who authtest.Credential, query string) ([]string, *string) {
	t.Helper()
	path := "/api/v1/scope-nodes"
	if query != "" {
		path += "?" + query
	}
	resp, raw := e.as(t, who, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body %s)", path, resp.StatusCode, raw)
	}
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		Cursor *string `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode page: %v (body %s)", err, raw)
	}
	ids := make([]string, 0, len(page.Items))
	for _, it := range page.Items {
		ids = append(ids, it.ID)
	}
	return ids, page.Cursor
}

// sortedCopy returns a sorted copy, so a set comparison does not depend on the
// order the fixture happened to build its expectation in.
func sortedCopy(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// intersect returns the ids present in both sets.
func intersect(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, id := range b {
		inB[id] = true
	}
	var out []string
	for _, id := range a {
		if inB[id] {
			out = append(out, id)
		}
	}
	return out
}

// TestDisjointPrincipalsSeeDisjointScopeNodes is the central claim: two
// principals whose role bindings sit on disjoint subtrees see disjoint scope
// nodes from the SAME query against the SAME collection.
//
// SEC-010 fixes the rule this rests on — "a role bound at a scope node applies
// to that node and, absent a more specific binding, to its descendants" — and
// SEC-005 fixes the direction of failure: a caller whose authority does not
// reach a node is not authorized for it, and a surface that showed it anyway
// would be default-permitting the read half of every operation.
//
// The seeder's own list is asserted too, and that assertion is what makes the
// rest meaningful: it proves all eleven rows genuinely exist, so an empty
// intersection is scoping and not an empty store.
func TestDisjointPrincipalsSeeDisjointScopeNodes(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)

	alice := e.principalAt(t, tr.siteA)
	bob := e.principalAt(t, tr.siteB)

	// The root-bound seeder sees the whole tree — the control.
	seeded, _ := e.listScopeNodeIDs(t, e.auth.Credential(), "limit=200")
	if got, want := sortedCopy(seeded), sortedCopy(tr.all()); !reflect.DeepEqual(got, want) {
		t.Fatalf("the root-bound principal must see every seeded node (SEC-010: a root binding inherits everywhere)\n got %v\nwant %v", got, want)
	}

	aliceIDs, _ := e.listScopeNodeIDs(t, alice, "limit=200")
	bobIDs, _ := e.listScopeNodeIDs(t, bob, "limit=200")

	if got, want := sortedCopy(aliceIDs), sortedCopy(tr.subtreeA()); !reflect.DeepEqual(got, want) {
		t.Fatalf("a principal bound at site A must see exactly site A and its descendants (SEC-010)\n got %v\nwant %v", got, want)
	}
	if got, want := sortedCopy(bobIDs), sortedCopy(tr.subtreeB()); !reflect.DeepEqual(got, want) {
		t.Fatalf("a principal bound at site B must see exactly site B and its descendants (SEC-010)\n got %v\nwant %v", got, want)
	}
	if overlap := intersect(aliceIDs, bobIDs); len(overlap) != 0 {
		t.Fatalf("two principals bound on disjoint subtrees returned overlapping scope nodes %v from one query; "+
			"the visible set is not being computed per principal", overlap)
	}

	// Neither sees the org node ABOVE their binding. Inheritance runs downward
	// only — a binding at a site does not confer sight of its parent, which is
	// the direction an implementation that walked the chain the wrong way (or
	// took the max across levels) would get wrong while still passing the
	// disjointness check above.
	for name, ids := range map[string][]string{"site-A principal": aliceIDs, "site-B principal": bobIDs} {
		for _, id := range ids {
			if id == tr.org {
				t.Fatalf("%s saw the org node above its own binding; SEC-010 inheritance applies to a node's DESCENDANTS, not its ancestors", name)
			}
		}
	}
}

// TestSharedAncestorGrantMakesSubtreesOverlap is the same tree and the same two
// callers, with ONE binding added to each: a second binding at the shared org
// ancestor. SEC-010's inheritance then reaches both subtrees for both of them,
// and the sets that were disjoint above overlap completely.
//
// It is the necessary complement to the disjointness case: an implementation
// that simply returned "rows whose scope_node equals a node I am bound at"
// would pass that case and fail this one, because it never walks the ancestor
// chain at all.
func TestSharedAncestorGrantMakesSubtreesOverlap(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)

	// Each caller keeps its own site binding AND gains one at the shared org.
	alice := e.principalAt(t, tr.siteA, tr.org)
	bob := e.principalAt(t, tr.siteB, tr.org)

	aliceIDs, _ := e.listScopeNodeIDs(t, alice, "limit=200")
	bobIDs, _ := e.listScopeNodeIDs(t, bob, "limit=200")

	want := sortedCopy(tr.all())
	if got := sortedCopy(aliceIDs); !reflect.DeepEqual(got, want) {
		t.Fatalf("a binding at the shared org ancestor must make the whole tree visible (SEC-010)\n got %v\nwant %v", got, want)
	}
	if got := sortedCopy(bobIDs); !reflect.DeepEqual(got, want) {
		t.Fatalf("a binding at the shared org ancestor must make the whole tree visible (SEC-010)\n got %v\nwant %v", got, want)
	}

	// The overlap is not incidental: each caller now sees screens from the OTHER
	// caller's site, which is precisely what the disjoint case proved they could
	// not.
	for _, screen := range tr.screensB {
		if len(intersect(aliceIDs, []string{screen})) == 0 {
			t.Fatalf("the site-A principal's org binding must reach screen %s under site B", screen)
		}
	}
	for _, screen := range tr.screensA {
		if len(intersect(bobIDs, []string{screen})) == 0 {
			t.Fatalf("the site-B principal's org binding must reach screen %s under site A", screen)
		}
	}
}

// TestOutOfReachRowReadsAsNonexistent pins the anti-probing posture on
// single-resource reads: a row outside the caller's visible set answers exactly
// as an id that names nothing — 404 / NOT_FOUND, named by the resource's own
// displayName — and NOT 403, which would confirm the row exists.
//
// events/1 EVT-122 states the reasoning for the sibling surface ("treating an
// out-of-reach scope node as an error would let a selector probe for the
// existence of scope nodes the principal cannot read") and EVT-120 ties that
// surface's visible set to this one ("computed the same way any other
// api/1-governed read is scoped"), so the posture is shared rather than
// re-decided. api/1's own taxonomy supplies the codes: FORBIDDEN is about the
// PRINCIPAL ("authenticated but not authorized for this operation"), NOT_FOUND
// is about the RESOURCE ("no resource exists at the identifier named by the
// request") — and a caller not entitled to know a row exists must be told the
// latter.
//
// The assertion is deliberately indistinguishability, not merely "the status is
// 404": the two Problem documents must agree member for member (trace_id aside,
// which is per-request by construction). A 404 whose detail differed would be
// the same oracle in a politer wrapper.
func TestOutOfReachRowReadsAsNonexistent(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	alice := e.principalAt(t, tr.siteA)
	outOfReach := tr.screensB[0]

	problemOf := func(method, path string) (int, map[string]any) {
		t.Helper()
		resp, raw := e.as(t, alice, method, path, nil, nil)
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("%s %s: decode problem: %v (body %s)", method, path, err, raw)
		}
		// trace_id is per-request by construction (API-010) and instance is a
		// verbatim echo of the request path (API-015) — both differ between any
		// two requests and neither says anything about the resource. Every OTHER
		// member must agree, which is what "indistinguishable" has to mean here.
		delete(p, "trace_id")
		delete(p, "instance")
		return resp.StatusCode, p
	}

	absentStatus, absentBody := problemOf(http.MethodGet, "/api/v1/scope-nodes/"+absentScopeNodeID)
	if absentStatus != http.StatusNotFound {
		t.Fatalf("GET a never-created scope node = %d, want 404", absentStatus)
	}
	if absentBody["code"] != "NOT_FOUND" {
		t.Fatalf("a never-created scope node's Problem code = %v, want NOT_FOUND", absentBody["code"])
	}

	hiddenStatus, hiddenBody := problemOf(http.MethodGet, "/api/v1/scope-nodes/"+outOfReach)
	if hiddenStatus != http.StatusNotFound {
		t.Fatalf("GET a scope node outside the visible set = %d, want 404 — a 403 confirms the row exists to a caller "+
			"not entitled to know it does (events/1 EVT-122's reasoning, api/1's NOT_FOUND/FORBIDDEN split)", hiddenStatus)
	}
	if !reflect.DeepEqual(hiddenBody, absentBody) {
		t.Fatalf("an out-of-reach row's 404 must be INDISTINGUISHABLE from a nonexistent id's\n hidden %v\nabsent %v",
			hiddenBody, absentBody)
	}

	// The mutating verbs answer the same way, and answer it BEFORE the
	// optimistic-concurrency precondition: a PATCH carrying no If-Match would
	// otherwise draw 428 / IF_MATCH_REQUIRED, which confirms the row's existence
	// just as surely as a 403 would.
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		status, body := problemOf(method, "/api/v1/scope-nodes/"+outOfReach)
		if status != http.StatusNotFound {
			t.Fatalf("%s on a scope node outside the visible set = %d, want 404 (never 403, and never the 412/428 "+
				"an addressable row's failed precondition would produce)", method, status)
		}
		if body["code"] != "NOT_FOUND" {
			t.Fatalf("%s on an out-of-reach scope node: code = %v, want NOT_FOUND", method, body["code"])
		}
	}

	// The rule is not scope-nodes-specific: a scheduling-core row placed under an
	// out-of-reach screen is equally unaddressable, and its 404 names its OWN
	// resource noun (api.go's displayName), not a generic one.
	e.uploadContent(t, playlistFixtureAsset)
	playlistID := decodeID(t, e.createOK(t, "/api/v1/playlists", mustJSON(t, playlistFixture(outOfReach, nil))))
	status, body := problemOf(http.MethodGet, "/api/v1/playlists/"+playlistID)
	if status != http.StatusNotFound {
		t.Fatalf("GET a playlist placed at an out-of-reach scope node = %d, want 404", status)
	}
	detail, _ := body["detail"].(string)
	if detail != "No playlist exists with this identifier." {
		t.Fatalf("an out-of-reach playlist's 404 detail = %q, want the playlist's own displayName in the prose", detail)
	}

	// And the seeder still reads both rows perfectly well — so every 404 above is
	// about WHO asked, not about a row that quietly failed to be written.
	for path := range map[string]struct{}{
		"/api/v1/scope-nodes/" + outOfReach: {},
		"/api/v1/playlists/" + playlistID:   {},
	} {
		if resp, raw := e.do(t, http.MethodGet, path, nil, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s as the root-bound seeder = %d, want 200 (body %s)", path, resp.StatusCode, raw)
		}
	}
}

// TestSelectorOnlyNarrowsTheVisibleSet pins the api/1 statement of events/1
// EVT-121/122 over the same selector grammar (API-046 makes it the platform's
// sole definition, so the two surfaces cannot differ):
//
//   - a selector naming a scope node the caller cannot reach returns an EMPTY
//     page, not an error — a 403 or a 400 would each be a working existence
//     probe, and EVT-122 forbids exactly that;
//   - a selector cannot widen the visible set: naming the other subtree's site
//     yields nothing, and naming an ancestor above the caller's binding yields
//     only the part of that subtree the caller could already see;
//   - a selector that does not PARSE is still 400 / SELECTOR_INVALID (API-045).
//     That distinction is the point: a syntax error is a statement about the
//     REQUEST and reveals nothing about which nodes exist.
func TestSelectorOnlyNarrowsTheVisibleSet(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	alice := e.principalAt(t, tr.siteA)

	emptyCases := map[string]string{
		"subtree of the other site":      "scope_node subtree " + tr.siteB,
		"exact node in the other site":   "scope_node=" + tr.screensB[0],
		"subtree of an unreachable node": "scope_node subtree " + absentScopeNodeID,
	}
	for name, selector := range emptyCases {
		ids, cursor := e.listScopeNodeIDs(t, alice, "selector="+url.QueryEscape(selector))
		if len(ids) != 0 {
			t.Fatalf("%s: selector %q returned %v; a term resolving outside the visible set matches nothing (EVT-122)", name, selector, ids)
		}
		if cursor != nil {
			t.Fatalf("%s: an empty page must carry a null cursor (API-032), got %q", name, *cursor)
		}
	}

	// A subtree term naming the ORG — an ancestor above alice's own binding —
	// selects the whole tree on its own terms, but delivers only alice's own
	// subtree: the intersection with the visible set, never the union.
	ids, _ := e.listScopeNodeIDs(t, alice, "limit=200&selector="+url.QueryEscape("scope_node subtree "+tr.org))
	if got, want := sortedCopy(ids), sortedCopy(tr.subtreeA()); !reflect.DeepEqual(got, want) {
		t.Fatalf("a subtree term naming an ancestor ABOVE the caller's binding must narrow, never widen (EVT-121)\n got %v\nwant %v", got, want)
	}

	// A subtree term inside alice's own reach still selects normally — the named
	// node AND its descendants (API-044) — so scoping narrowed nothing it should
	// not have.
	ids, _ = e.listScopeNodeIDs(t, alice, "limit=200&selector="+url.QueryEscape("scope_node subtree "+tr.siteA))
	if got, want := sortedCopy(ids), sortedCopy(tr.subtreeA()); !reflect.DeepEqual(got, want) {
		t.Fatalf("a subtree term inside the caller's own reach must select the named node and its descendants (API-044)\n got %v\nwant %v", got, want)
	}

	// And an exact-node term still narrows to one row: the selector is doing real
	// work inside the visible set, not being swallowed by it.
	ids, _ = e.listScopeNodeIDs(t, alice, "limit=200&selector="+url.QueryEscape("scope_node="+tr.screensA[1]))
	if got, want := ids, []string{tr.screensA[1]}; !reflect.DeepEqual(got, want) {
		t.Fatalf("an exact scope_node term inside the caller's reach must select that node only (API-044)\n got %v\nwant %v", got, want)
	}

	// A malformed selector is still a request-syntax failure: 400 /
	// SELECTOR_INVALID, unchanged by scoping (API-045).
	resp, raw := e.as(t, alice, http.MethodGet, "/api/v1/scope-nodes?selector="+url.QueryEscape("kind = screen"), nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a malformed selector must still be 400 / SELECTOR_INVALID (API-045); got %d (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "SELECTOR_INVALID")
}

// TestPageSizeIsHonoredUnderScopeFiltering pins where the visible-set filter
// belongs relative to the keyset window: BEFORE it, not after.
//
// The fixture interleaves the two subtrees in id order, so a filter applied to
// an already-cut page would drop roughly half of every page — a caller asking
// for 3 would get 1 or 2, and the shortfall would itself count the rows removed.
// API-032/034 make a short page mean "the collection is exhausted", so the
// filter has to run while the window is being built.
//
// Paging is then walked to the end: every visible row appears exactly once,
// nothing from the other subtree ever appears, and the last page's cursor is
// null.
func TestPageSizeIsHonoredUnderScopeFiltering(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	alice := e.principalAt(t, tr.siteA)

	const limit = 3
	visible := tr.subtreeA() // 5 rows: the site plus its four screens.
	if len(visible) <= limit {
		t.Fatalf("fixture must hold more visible rows (%d) than one page (%d) or this case proves nothing", len(visible), limit)
	}

	first, cursor := e.listScopeNodeIDs(t, alice, "limit=3")
	if len(first) != limit {
		t.Fatalf("page 1 returned %d rows for limit=%d: %v — a full page must be full. The visible-set filter has to run "+
			"BEFORE the keyset window is cut, or an interleaved collection silently returns short pages (API-032/034)",
			len(first), limit, first)
	}
	if cursor == nil {
		t.Fatalf("page 1 of %d visible rows at limit=%d must carry a continuation cursor (API-032)", len(visible), limit)
	}

	seen := append([]string(nil), first...)
	for cursor != nil {
		var page []string
		page, cursor = e.listScopeNodeIDs(t, alice, "limit=3&cursor="+url.QueryEscape(*cursor))
		if len(page) == 0 {
			t.Fatalf("a non-null cursor produced an empty page; the cursor should have been null (API-032)")
		}
		if cursor != nil && len(page) != limit {
			t.Fatalf("a non-final page returned %d rows for limit=%d: %v", len(page), limit, page)
		}
		seen = append(seen, page...)
	}

	if got, want := sortedCopy(seen), sortedCopy(visible); !reflect.DeepEqual(got, want) {
		t.Fatalf("paging to the end must cover the visible set exactly once — no repeats, no skips (API-034)\n got %v\nwant %v", got, want)
	}
	if overlap := intersect(seen, tr.subtreeB()); len(overlap) != 0 {
		t.Fatalf("paging surfaced rows from outside the visible set: %v", overlap)
	}
}

// TestBulkEnableTargetsOnlyVisibleAutomations pins the fleet-mutating half.
// API-110 extends the SAME selector convention a list read uses to a
// fleet-mutating operation's target predicate, so it inherits the same scoping:
// the returned Job's targets[] (API-112) enumerates matched ids in its own
// response body, which is a list read wearing a different status code.
func TestBulkEnableTargetsOnlyVisibleAutomations(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	alice := e.principalAt(t, tr.siteA)

	mine := decodeID(t, e.createOK(t, "/api/v1/automations",
		edgeAutomationBody("", tr.screensA[0], map[string]string{"env": "prod"})))
	theirs := decodeID(t, e.createOK(t, "/api/v1/automations",
		edgeAutomationBody("", tr.screensB[0], map[string]string{"env": "prod"})))

	resp, raw := e.as(t, alice, http.MethodPost, "/api/v1/automations/bulk-enable",
		mustJSON(t, map[string]any{"selector": "env=prod", "enabled": true}), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("bulk-enable = %d, want 202 (body %s)", resp.StatusCode, raw)
	}
	var job struct {
		Targets []struct {
			TargetID string `json:"target_id"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatalf("decode Job: %v (body %s)", err, raw)
	}
	got := make([]string, 0, len(job.Targets))
	for _, tg := range job.Targets {
		got = append(got, tg.TargetID)
	}
	if !reflect.DeepEqual(got, []string{mine}) {
		t.Fatalf("a fleet-mutating operation's target set must be drawn from the rows the submitter can READ "+
			"(API-110 reuses the list selector; API-112 publishes the ids)\n got %v\nwant %v", got, []string{mine})
	}

	// The out-of-reach automation is equally unaddressable one at a time.
	runResp, runRaw := e.as(t, alice, http.MethodPost, "/api/v1/automations/"+theirs+"/run", []byte(`{}`), nil)
	if runResp.StatusCode != http.StatusNotFound {
		t.Fatalf("running an automation outside the visible set = %d, want 404 (body %s)", runResp.StatusCode, runRaw)
	}
	assertProblem(t, runResp, runRaw, "NOT_FOUND")
}

// TestDevicePlaneReadsAreScoped extends the same rule to the device plane's two
// read families and its one mutating operation. A device is a read-only
// projection of what a relay discovered and adopted rather than an authored
// resource, but it carries a scope_node like everything else on this surface, so
// it is scoped like everything else — and a command addressed to an entity the
// caller cannot see is refused as an entity that does not exist, never as one
// they are merely not allowed to command.
func TestDevicePlaneReadsAreScoped(t *testing.T) {
	e, tr := newScopedDevicePlaneEnv(t)
	alice := e.principalAt(t, tr.siteA)

	listIDs := func(who authtest.Credential, path string) []string {
		t.Helper()
		resp, raw := e.as(t, who, http.MethodGet, path, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 (body %s)", path, resp.StatusCode, raw)
		}
		p := decodePage(t, raw)
		ids := make([]string, 0, len(p.Items))
		for _, it := range p.Items {
			id, _ := it["id"].(string)
			ids = append(ids, id)
		}
		return ids
	}

	// The seeder sees both halves of the plane — the control that proves the
	// registry really holds them.
	if got := listIDs(e.auth.Credential(), "/api/v1/devices"); len(got) != 2 {
		t.Fatalf("the root-bound principal must see both devices, got %v", got)
	}
	if got, want := listIDs(alice, "/api/v1/devices"), []string{scopedDeviceA}; !reflect.DeepEqual(got, want) {
		t.Fatalf("devices list is not scoped to the caller's visible set\n got %v\nwant %v", got, want)
	}
	if got, want := listIDs(alice, "/api/v1/entities"), []string{scopedEntityA}; !reflect.DeepEqual(got, want) {
		t.Fatalf("entities list is not scoped to the caller's visible set\n got %v\nwant %v", got, want)
	}

	// Commanding an out-of-reach entity is refused with the SAME 404 an entity
	// nobody adopted draws — and, decisively, nothing is dispatched: a refusal
	// that still reached the relay would be an authorization check performed too
	// late to matter.
	body := mustJSON(t, map[string]any{"command": "launch"})
	resp, raw := e.as(t, alice, http.MethodPost, "/api/v1/entities/"+scopedEntityB+"/commands", body, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("commanding an entity outside the visible set = %d, want 404 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")
	if calls := e.dispatcher.dispatched(); len(calls) != 0 {
		t.Fatalf("an out-of-reach command reached the relay anyway: %+v", calls)
	}

	// The caller's OWN entity still commands normally, so the refusal above is
	// scoping and not a broken command path.
	resp, raw = e.as(t, alice, http.MethodPost, "/api/v1/entities/"+scopedEntityA+"/commands", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("commanding an entity inside the visible set = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
}

// Fixture ids for the scoped device-plane env: canonical ULIDs (DAT-005a),
// ordered so the list assertions above are deterministic.
const (
	scopedRelayA  = "01J8Z50000000000000000RA01"
	scopedRelayB  = "01J8Z50000000000000000RB01"
	scopedDeviceA = "01J8Z50000000000000000DA01"
	scopedDeviceB = "01J8Z50000000000000000DB01"
	scopedEntityA = "01J8Z50000000000000000EA01"
	scopedEntityB = "01J8Z50000000000000000EB01"
)

// newScopedDevicePlaneEnv mounts the api handler with a device plane whose rows
// are placed at REAL scope nodes from the seeded tree — unlike
// newDevicePlaneEnv, whose fixture placements are ids no scope-node row carries.
// That difference matters: the visible set is resolved through the actual tree,
// so a device placed at a node the tree does not contain is reachable only by a
// root-bound principal (the fail-closed case), which is not what this test is
// about.
func newScopedDevicePlaneEnv(t *testing.T) (*devicePlaneEnv, scopedTree) {
	t.Helper()
	registry := devices.New(devScopeA)
	dispatcher := &fakeDispatcher{result: wire.DeviceCommandResultBody{OK: true}}

	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	clock := func() int64 { return fixedNowMs }
	content := origin.New()
	fixture := newAuthFixture(t)
	jobs := api.NewJobRunner()
	ts := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, ulid.Monotonic(),
		content, testContentBase, fixture.Auth, api.WithDevicePlane(registry, dispatcher), api.WithJobRunner(jobs)))
	t.Cleanup(ts.Close)

	env := &devicePlaneEnv{
		testEnv:    &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture, jobs: jobs},
		registry:   registry,
		dispatcher: dispatcher,
	}
	tr := seedScopedTree(t, env.testEnv)

	mustPutDevice(t, registry, devices.Device{
		ID: scopedDeviceA, RelayID: scopedRelayA, DeviceClass: "media-player",
		Name: "Site A TV", ScopeNode: tr.screensA[0],
	})
	mustPutDevice(t, registry, devices.Device{
		ID: scopedDeviceB, RelayID: scopedRelayB, DeviceClass: "media-player",
		Name: "Site B TV", ScopeNode: tr.screensB[0],
	})
	mustPutEntity(t, registry, devices.Entity{
		ID: scopedEntityA, DeviceID: scopedDeviceA, RelayID: scopedRelayA, DeviceClass: "media-player",
		Name: "Site A player", ScopeNode: tr.screensA[0],
	})
	mustPutEntity(t, registry, devices.Entity{
		ID: scopedEntityB, DeviceID: scopedDeviceB, RelayID: scopedRelayB, DeviceClass: "media-player",
		Name: "Site B player", ScopeNode: tr.screensB[0],
	})
	return env, tr
}

// TestPackDataRowReadsAreScoped extends the rule to a pack's own collection
// rows. A pack row carries the universal envelope's scope_node (MAN-052), so it
// is scoped exactly as a first-party resource is — a pack does not get a private
// door around the visible set just because its schema is declared rather than
// built in.
func TestPackDataRowReadsAreScoped(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	e.installDataPack(t)
	alice := e.principalAt(t, tr.siteA)

	create := func(scopeNode, name string) string {
		t.Helper()
		resp, row := e.createRow(t, map[string]any{"scope_node": scopeNode, "name": name, "price": 1.0}, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create pack row at %s: status %d (%+v)", scopeNode, resp.StatusCode, row)
		}
		id, _ := row["entity_id"].(string)
		return id
	}
	mine := create(tr.screensA[0], "Visible")
	theirs := create(tr.screensB[0], "Hidden")

	resp, raw := e.as(t, alice, http.MethodGet, menuRowsPath+"?limit=200", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list pack rows = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	p := decodePage(t, raw)
	ids := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		id, _ := it["entity_id"].(string)
		ids = append(ids, id)
	}
	if !reflect.DeepEqual(ids, []string{mine}) {
		t.Fatalf("a pack collection's list must be scoped to the caller's visible set\n got %v\nwant %v", ids, []string{mine})
	}

	resp, raw = e.as(t, alice, http.MethodGet, menuRowsPath+"/"+theirs, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET a pack row outside the visible set = %d, want 404 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")

	// Unaddressable ahead of the concurrency precondition: a DELETE with no
	// If-Match would otherwise draw 428 and confirm the row exists.
	resp, raw = e.as(t, alice, http.MethodDelete, menuRowsPath+"/"+theirs, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE a pack row outside the visible set = %d, want 404 (body %s)", resp.StatusCode, raw)
	}
}
