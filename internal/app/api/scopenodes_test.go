package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// A single fixed injected clock so idempotency retention is deterministic and no
// wall-clock read leaks into the api-layer tests.
const fixedNowMs = int64(1_700_000_000_000)

// boundaryOrgID is a fixture ULID (no secrets) that createNode RESOLVES to the
// env's own lazily-created org root. It used to name a never-created
// "subtree-boundary" parent, which the store tolerated — but the app store
// holds the FULL tree, where DAT-002 makes an unresolvable parent_id a
// violation, so a fixture site now has to hang off a real org like production
// data does. A body that reaches the server without passing through createNode
// keeps the literal value, which is exactly what a test needs when the point IS
// a nonexistent parent. Every id an actual create mints is server-assigned
// (rejectClientSuppliedID), so every OTHER fixture id below is captured from
// its create response instead of pinned as a constant.
const boundaryOrgID = "01J8Z0A0000000000000000000"

// neverMintedULID is well-formed and was never created, and that combination is
// the whole point: a case about parent RESOLUTION has to send an id the store can
// look up and fail to find. A malformed id draws the same code from shape
// validation alone, so a test using one would pass with resolution removed
// entirely — a refusal for the wrong reason, which reads exactly like the right
// one. Unlike boundaryOrgID, createNode never substitutes anything for it.
const neverMintedULID = "01J8Z0B0000000000000000000"

const (
	siteTZ   = "America/Chicago"
	siteLat  = 41.8781
	siteLong = -87.6298
)

func strp(s string) *string   { return &s }
func f64p(f float64) *float64 { return &f }

func siteNode(id string) datamodel.ScopeNode {
	return datamodel.ScopeNode{
		ID:       id,
		Kind:     "site",
		ParentID: strp(boundaryOrgID),
		Name:     "Demo Site",
		TZ:       strp(siteTZ),
		Lat:      f64p(siteLat),
		Long:     f64p(siteLong),
	}
}

func screenNode(id, parent, externalID string) datamodel.ScopeNode {
	return datamodel.ScopeNode{
		ID:         id,
		Kind:       "screen",
		ParentID:   strp(parent),
		Name:       "Demo Screen",
		ExternalID: externalID,
	}
}

// testEnv is a live httptest server over a fresh :memory: store, sharing one
// content origin store (contentBase is the feeder-injected content base URL the
// upload endpoint builds direct-fetch URLs from).
type testEnv struct {
	ts          *httptest.Server
	store       *store.Store
	content     *origin.Store
	contentBase string
	// auth is the seeded principal every request in this package is driven AS.
	// api.New requires an authenticator and refuses a request whose principal
	// cannot be resolved (SEC-005), so these tests hold a real credential rather
	// than a bypass — which is what keeps them exercising the handler that
	// actually ships.
	auth *authtest.Fixture
	// jobs is the runner accepted async work executes on, wired STOPPED. Nothing
	// this package accepts with 202 runs until a case asks for it (runJobs), so
	// a case about acceptance observes acceptance and a case about execution
	// observes execution — neither races the other, and no case waits on a
	// clock to find out which it got.
	jobs *api.JobRunner
	// orgID is the env's lazily-created org root — see orgRoot.
	orgID string
}

// runJobs releases the env's job runner and blocks until every job accepted so
// far has finished executing. It is the deterministic completion signal the
// runner exposes for exactly this: no polling loop, no sleep, no timeout.
func (e *testEnv) runJobs() {
	e.jobs.Start()
	e.jobs.Wait()
}

// newAuthFixture seeds one owner principal with a live session, or fails the
// test. Each env gets its own so a test that revokes or re-binds cannot leak
// into another.
func newAuthFixture(t *testing.T) *authtest.Fixture {
	t.Helper()
	f, err := authtest.New(authtest.Config{NowMs: func() int64 { return fixedNowMs }})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	t.Cleanup(f.Close)
	return f
}

// testContentBase is the fixed content-origin base URL the api-layer tests
// inject into api.New — a fixture host (no secrets), byte-identical to the form
// snapshot.Build uses: <base>/content/<hex>.
const testContentBase = "https://content.example"

func newEnv(t *testing.T) *testEnv {
	return newEnvWithContent(t, origin.New())
}

// newEnvWithContent is newEnv over a caller-supplied content origin — used by the
// restart regression to hand the api handler an origin.Open'd store already
// carrying content uploaded in a prior "lifetime".
func newEnvWithContent(t *testing.T, content *origin.Store) *testEnv {
	return newEnvWith(t, content, nil)
}

// newEnvWithOptions is newEnv with extra api.New options — the seam a family
// whose behaviour depends on an optional dependency (the webhook secret sealer)
// builds its own env from, without every other env growing that dependency.
func newEnvWithOptions(t *testing.T, opts ...api.Option) *testEnv {
	t.Helper()
	return newEnvWith(t, origin.New(), opts)
}

func newEnvWith(t *testing.T, content *origin.Store, opts []api.Option) *testEnv {
	t.Helper()
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	clock := func() int64 { return fixedNowMs }
	idem := apihttp.NewIdempotencyStore(clock, 0)
	// A resource's id is now exclusively server-assigned (rejectClientSuppliedID),
	// so every fixture-suffix-ordering test in this package relies on this env's
	// minted ids, not a client-supplied one — ulid.Monotonic (not plain ulid.New)
	// guarantees each successive create mints a STRICTLY greater id even within
	// the same millisecond, preserving the "creation order == id order" invariant
	// several list/pagination tests depend on.
	fixture := newAuthFixture(t)
	jobs := api.NewJobRunner()
	// Every env trusts the package's fixture pack signer (apiPackAnchors), so
	// the REAL artifact-signature gate runs on every install fixture; a caller's
	// own later WithPackTrust option overrides it.
	ts := httptest.NewServer(api.New(st, idem, clock, ulid.Monotonic(), content, testContentBase, fixture.Auth,
		append([]api.Option{api.WithJobRunner(jobs), api.WithPackTrust(apiPackAnchors())}, opts...)...))
	t.Cleanup(ts.Close)
	return &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture, jobs: jobs}
}

func (e *testEnv) do(t *testing.T, method, path string, body []byte, headers map[string]string) (*http.Response, []byte) {
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
	// The env's real session cookie and CSRF token, attached centrally so no
	// individual test carries credentials that are not part of what it asserts.
	e.auth.Authorize(req)
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

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// createNode POSTs a scope node (its own id left empty — server-assigned,
// rejectClientSuppliedID) and returns the server-minted id, failing if the
// status is not 201. A parent_id naming the boundaryOrgID sentinel is resolved
// to the env's org root, created on first use — the full tree the store
// validates has no tolerance for a parent that does not exist (DAT-002).
func (e *testEnv) createNode(t *testing.T, n datamodel.ScopeNode) string {
	t.Helper()
	n = e.resolveBoundaryParent(t, n)
	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, n), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s node %q: status %d, body %s", n.Kind, n.Name, resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// resolveBoundaryParent substitutes the env's real org root for a parent_id
// naming the boundaryOrgID sentinel. createNode applies it for every create it
// issues; a test that POSTs a fixture body ITSELF (because it asserts on the
// raw response) applies it by hand.
//
// It is a separate step from the POST precisely so a case whose subject IS a
// nonexistent parent can bypass it and send the literal.
func (e *testEnv) resolveBoundaryParent(t *testing.T, n datamodel.ScopeNode) datamodel.ScopeNode {
	t.Helper()
	if n.ParentID != nil && *n.ParentID == boundaryOrgID {
		n.ParentID = strp(e.orgRoot(t))
	}
	return n
}

// orgRoot returns the env's org-kind root, creating it on first use. Lazy
// rather than seeded in newEnv so a test about the org itself (or about an
// empty store) starts from the same blank slate it always did.
//
// It carries `account_state` because DAT-010 says an org-kind node MUST, and
// because the workspace surface reads it (the deployment's org node IS the
// workspace's identity) — so the one org row this env creates is the same row
// both the tree and the workspace mean.
func (e *testEnv) orgRoot(t *testing.T) string {
	t.Helper()
	if e.orgID == "" {
		e.orgID = e.createNode(t, datamodel.ScopeNode{Kind: "org", Name: "Fixture Org", AccountState: "active"})
	}
	return e.orgID
}

// autoSiteNodeID is the site the fixture placement node hangs off — see
// placementNode. A fixture ULID, no secrets.
const autoSiteNodeID = "01J8Z0F1XTVRES1TEF0RR0WS01"

// placementNode seeds the scope node the automation, job and pack-data fixtures
// place their rows at, and returns its id: the autoScopeNode constant itself,
// which those fixture bodies name directly.
//
// A row's scope_node is a reference the store resolves (DAT-006), so a fixture
// that skipped this was authoring a row placed at nothing — which is why every
// such test now says out loud that its node exists.
//
// It writes through the STORE, not the api, for two reasons: the api assigns a
// resource's own id (rejectClientSuppliedID) so the constant could not be
// reached over HTTP, and one test counts every id an injected source mints.
// Each node is created only if absent, so calling this beside orgRoot, or twice,
// is safe — and it hangs the tree off whatever org the env already has, because
// DAT-002 admits exactly one.
func (e *testEnv) placementNode(t *testing.T) string {
	t.Helper()
	e.seedPlacementNodes(t, autoScopeNode)
	return autoScopeNode
}

// seedPlacementNodes seeds one screen-kind scope node per id, under the env's
// fixture site, so a fixture row may be placed at any of them. See placementNode
// for why these are written through the store and why re-seeding is safe.
func (e *testEnv) seedPlacementNodes(t *testing.T, ids ...string) {
	t.Helper()
	site := e.fixtureSite(t)
	for _, id := range ids {
		e.ensureNode(t, datamodel.ScopeNode{ID: id, Kind: "screen", ParentID: strp(site), Name: "Fixture Screen"})
	}
}

// fixtureSite returns the site every seeded placement node hangs off, creating it
// — and the single org root DAT-002 requires above it — on first use.
//
// The org is looked for in the STORE, not only in e.orgID, because a fixture may
// have created one without going through orgRoot (seedScopedTree builds a whole
// tree of its own). DAT-002 admits exactly one org-kind node, so a second would be
// SCOPE_NODE_MULTIPLE_ORG — the site has to hang off whichever one is already
// there.
func (e *testEnv) fixtureSite(t *testing.T) string {
	t.Helper()
	if e.orgID == "" {
		e.orgID = e.storedOrgID(t)
	}
	if e.orgID == "" {
		e.ensureNode(t, datamodel.ScopeNode{ID: boundaryOrgID, Kind: "org", Name: "Fixture Org", AccountState: "active"})
		e.orgID = boundaryOrgID
	}
	e.ensureNode(t, datamodel.ScopeNode{
		ID: autoSiteNodeID, Kind: "site", ParentID: strp(e.orgID), Name: "Fixture Site",
		TZ: strp(siteTZ), Lat: f64p(siteLat), Long: f64p(siteLong),
	})
	return autoSiteNodeID
}

// storedOrgID returns the id of the stored org-kind scope node, or "" when the
// tree has no root yet.
func (e *testEnv) storedOrgID(t *testing.T) string {
	t.Helper()
	rows, err := e.store.List(context.Background(), store.KindScopeNode, store.ListFilter{})
	if err != nil {
		t.Fatalf("list scope nodes: %v", err)
	}
	for _, r := range rows {
		var n datamodel.ScopeNode
		if err := json.Unmarshal(r.Body, &n); err != nil {
			t.Fatalf("decode scope node %s: %v", r.ID, err)
		}
		if n.Kind == "org" {
			return n.ID
		}
	}
	return ""
}

// ensureNode writes n through the store unless a row already carries its id.
func (e *testEnv) ensureNode(t *testing.T, n datamodel.ScopeNode) {
	t.Helper()
	ctx := context.Background()
	if _, found, err := e.store.Get(ctx, store.KindScopeNode, n.ID); err != nil {
		t.Fatalf("read scope node %s: %v", n.ID, err)
	} else if found {
		return
	}
	if _, err := e.store.Create(ctx, store.KindScopeNode, mustJSON(t, n)); err != nil {
		t.Fatalf("seed scope node %s: %v", n.ID, err)
	}
}

// seedOrgRootThroughStore writes the org root at the literal boundaryOrgID,
// bypassing the api surface entirely.
//
// It exists for the two tests that build their own server around a store they
// hold directly and cannot afford a create through the api: one counts every id
// an injected id source mints, the other sends raw JSON bodies that name the
// parent by that constant. Neither could get this row over HTTP — a resource's
// own id is exclusively server-assigned (rejectClientSuppliedID) — and both need
// it to exist, because the store validates the WHOLE tree and an unresolvable
// parent_id is a DAT-002 violation there rather than a subtree boundary.
func seedOrgRootThroughStore(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.Create(context.Background(), store.KindScopeNode, mustJSON(t, datamodel.ScopeNode{
		ID: boundaryOrgID, Kind: "org", Name: "Fixture Org", AccountState: "active",
	})); err != nil {
		t.Fatalf("seed org root %s: %v", boundaryOrgID, err)
	}
}

// assertProblem asserts the body is an api/1 Problem carrying the expected code
// and a non-empty trace_id equal to the Trace-Id response header.
func assertProblem(t *testing.T, resp *http.Response, raw []byte, wantCode string) map[string]any {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != apihttp.ProblemContentType {
		t.Fatalf("problem content-type = %q, want %q (body %s)", ct, apihttp.ProblemContentType, raw)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode problem: %v (body %s)", err, raw)
	}
	if p["code"] != wantCode {
		t.Fatalf("problem code = %v, want %s (body %s)", p["code"], wantCode, raw)
	}
	if p["type"] != "about:blank" {
		t.Fatalf("problem type = %v, want about:blank", p["type"])
	}
	tid, _ := p["trace_id"].(string)
	if tid == "" {
		t.Fatalf("problem missing trace_id (body %s)", raw)
	}
	if h := resp.Header.Get("Trace-Id"); h != tid {
		t.Fatalf("problem trace_id %q != Trace-Id header %q", tid, h)
	}
	return p
}

func decodeID(t *testing.T, raw []byte) string {
	t.Helper()
	var b struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("decode id: %v (body %s)", err, raw)
	}
	return b.ID
}

func TestCreateAndGetScopeNode(t *testing.T) {
	e := newEnv(t)

	// POSTed here rather than through createNode because the assertions below are
	// about the raw create RESPONSE (ETag, Location, Trace-Id), which createNode
	// discards — so the sentinel parent is resolved by hand.
	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, e.resolveBoundaryParent(t, siteNode(""))), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", resp.StatusCode, raw)
	}
	if etag := resp.Header.Get("ETag"); etag != `"1"` {
		t.Fatalf("create ETag = %q, want \"1\"", etag)
	}
	siteID := decodeID(t, raw)
	if loc := resp.Header.Get("Location"); loc != "/api/v1/scope-nodes/"+siteID {
		t.Fatalf("create Location = %q, want /api/v1/scope-nodes/%s", loc, siteID)
	}
	if resp.Header.Get("Trace-Id") == "" {
		t.Fatalf("create missing Trace-Id header")
	}

	// GET the created node.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/scope-nodes/"+siteID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, body %s", resp.StatusCode, raw)
	}
	if etag := resp.Header.Get("ETag"); etag != `"1"` {
		t.Fatalf("get ETag = %q, want \"1\"", etag)
	}
	if got := decodeID(t, raw); got != siteID {
		t.Fatalf("got id = %q, want %q", got, siteID)
	}

	// A missing id is 404 / NOT_FOUND.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/scope-nodes/01J8Z0Z0000000000000000000", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get-missing status = %d, want 404", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")
}

func TestListPaginationRoundtrip(t *testing.T) {
	e := newEnv(t)
	// ulid.Monotonic (newEnvWithContent) guarantees the site's minted id is
	// strictly less than the screen's — creation order is id order. The org root
	// is created FIRST and named here, because the collection this walks is the
	// whole scope_nodes table: DAT-002's single root is one of its rows, not an
	// implied ancestor outside it.
	orgID := e.orgRoot(t)
	siteID := e.createNode(t, siteNode(""))
	screen1ID := e.createNode(t, screenNode("", siteID, ""))
	want := []string{orgID, siteID, screen1ID}

	type page struct {
		Items  []json.RawMessage `json:"items"`
		Cursor *string           `json:"cursor"`
	}

	// limit=1, replaying each returned cursor: every page carries exactly one
	// item, the walk terminates on a null cursor, and it covers the collection in
	// id order with no skip and no repeat.
	var walked []string
	cursor := ""
	for i := 0; i <= len(want); i++ { // hard cap: a paging bug must fail, not hang
		u := "/api/v1/scope-nodes?limit=1"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		resp, raw := e.do(t, http.MethodGet, u, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list page %d status = %d, body %s", i+1, resp.StatusCode, raw)
		}
		var p page
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode page %d: %v (body %s)", i+1, err, raw)
		}
		if len(p.Items) != 1 {
			t.Fatalf("page %d items = %d, want 1", i+1, len(p.Items))
		}
		walked = append(walked, decodeID(t, p.Items[0]))
		if p.Cursor == nil {
			break
		}
		cursor = *p.Cursor
	}
	if !reflect.DeepEqual(walked, want) {
		t.Fatalf("cursor walk covered %v, want exactly %v (in id order, once each)", walked, want)
	}

	// A malformed cursor is CURSOR_INVALID, never "from the beginning".
	resp, raw := e.do(t, http.MethodGet, "/api/v1/scope-nodes?cursor="+url.QueryEscape("not a cursor!!"), nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-cursor status = %d, want 400", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "CURSOR_INVALID")

	// An out-of-range limit is VALIDATION_FAILED, never clamped.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/scope-nodes?limit=0", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("limit=0 status = %d, want 400", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")
}

func TestListSelectorFilter(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))
	e.createNode(t, screenNode("", siteID, ""))
	e.createNode(t, screenNode("", siteID, ""))

	// selector=kind=site selects only the site node (the reserved intrinsic kind).
	q := url.Values{"selector": {"kind=site"}}
	resp, raw := e.do(t, http.MethodGet, "/api/v1/scope-nodes?"+q.Encode(), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("selector status = %d, body %s", resp.StatusCode, raw)
	}
	var p struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(p.Items) != 1 {
		t.Fatalf("selector kind=site returned %d items, want 1", len(p.Items))
	}
	if got := decodeID(t, p.Items[0]); got != siteID {
		t.Fatalf("selector returned id %q, want the site %q", got, siteID)
	}

	// A malformed selector is SELECTOR_INVALID.
	q = url.Values{"selector": {"kind = site"}}
	resp, raw = e.do(t, http.MethodGet, "/api/v1/scope-nodes?"+q.Encode(), nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-selector status = %d, want 400", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "SELECTOR_INVALID")
}

func TestPatchIfMatch(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))

	patch := mustJSON(t, map[string]string{"name": "Renamed Site"})

	// No If-Match → 428 / IF_MATCH_REQUIRED, no write.
	resp, raw := e.do(t, http.MethodPatch, "/api/v1/scope-nodes/"+siteID, patch, nil)
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("patch-no-ifmatch status = %d, want 428", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "IF_MATCH_REQUIRED")

	// Stale If-Match → 412 / REVISION_CONFLICT carrying current_revision.
	resp, raw = e.do(t, http.MethodPatch, "/api/v1/scope-nodes/"+siteID, patch, map[string]string{"If-Match": `"5"`})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("patch-stale status = %d, want 412", resp.StatusCode)
	}
	p := assertProblem(t, resp, raw, "REVISION_CONFLICT")
	if cr, _ := p["current_revision"].(float64); int64(cr) != 1 {
		t.Fatalf("current_revision = %v, want 1", p["current_revision"])
	}

	// Correct If-Match → 200 + ETag "2".
	resp, raw = e.do(t, http.MethodPatch, "/api/v1/scope-nodes/"+siteID, patch, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch-ok status = %d, body %s", resp.StatusCode, raw)
	}
	if etag := resp.Header.Get("ETag"); etag != `"2"` {
		t.Fatalf("patch-ok ETag = %q, want \"2\"", etag)
	}
	var body struct {
		Name     string `json:"name"`
		Revision int    `json:"revision"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode patched: %v", err)
	}
	if body.Name != "Renamed Site" || body.Revision != 2 {
		t.Fatalf("patched body = %+v, want name=Renamed Site revision=2", body)
	}
}

func TestDeleteIfMatch(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))
	screen1ID := e.createNode(t, screenNode("", siteID, ""))

	// DELETE requires If-Match.
	resp, raw := e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+screen1ID, nil, nil)
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("delete-no-ifmatch status = %d, want 428", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "IF_MATCH_REQUIRED")

	// With the right ETag → 204, gone.
	resp, _ = e.do(t, http.MethodDelete, "/api/v1/scope-nodes/"+screen1ID, nil, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp, _ = e.do(t, http.MethodGet, "/api/v1/scope-nodes/"+screen1ID, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d, want 404", resp.StatusCode)
	}
}

func TestExternalIDConflict(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))
	site2ID := e.createNode(t, siteNode(""))
	e.createNode(t, screenNode("", siteID, "lobby-screen-1"))

	countScreens := func() int {
		t.Helper()
		resp, raw := e.do(t, http.MethodGet, "/api/v1/scope-nodes?selector="+url.QueryEscape("kind=screen"), nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list screens status = %d, body %s", resp.StatusCode, raw)
		}
		var p struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return len(p.Items)
	}

	// A duplicate external_id under the SAME parent → 400 / EXTERNAL_ID_CONFLICT,
	// no write.
	dup := screenNode("", siteID, "lobby-screen-1")
	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, dup), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("dup-external-id status = %d, want 400, body %s", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "EXTERNAL_ID_CONFLICT")
	// The colliding node was not written: still exactly the one screen.
	if n := countScreens(); n != 1 {
		t.Fatalf("screens after rejected duplicate = %d, want 1 (colliding node was created)", n)
	}

	// The SAME external_id under a DIFFERENT parent is NOT a collision (API-101).
	ok := screenNode("", site2ID, "lobby-screen-1")
	resp, raw = e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, ok), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("same-external-id-different-parent status = %d, want 201, body %s", resp.StatusCode, raw)
	}
	if n := countScreens(); n != 2 {
		t.Fatalf("screens after the second (non-colliding) create = %d, want 2", n)
	}
}

// TestCreateClientSuppliedIDRejectedAsClientProblem: a create body naming a
// non-empty "id" — whether or not it collides with an existing row — is a
// well-defined client Problem (422 / ID_SERVER_ASSIGNED), never a masked 500
// INTERNAL. A resource's id is exclusively server-assigned (api/1 API-105);
// rejectClientSuppliedID rejects it upfront, before the request ever reaches the
// store's own identity checks. API-105 fixes the code at the TOP LEVEL of the
// Problem — that is where a client's error handling switches (API-016) — so a
// VALIDATION_FAILED whose errors[] carried ID_SERVER_ASSIGNED would leave the
// one value this requirement names unreachable from where it is read.
func TestCreateClientSuppliedIDRejectedAsClientProblem(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))

	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, siteNode(siteID)), nil)
	if resp.StatusCode == http.StatusInternalServerError {
		t.Fatalf("client-supplied id surfaced as 500 INTERNAL (body %s)", raw)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("client-supplied-id status = %d, want 422, body %s", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "ID_SERVER_ASSIGNED")
	if _, present := p["errors"]; present {
		t.Fatalf("the Problem carries an errors[] member without VALIDATION_FAILED (body %s)", raw)
	}
	// The detail must name the field the refusal is ABOUT. A bare
	// Contains(detail, "id") passes on the template's own "identity" and
	// "external_id", so it would hold for any field name at all — the assertion
	// it replaced (errors[0].field == "id") genuinely pinned this, and dropping
	// errors[] must not drop the property with it.
	detail, _ := p["detail"].(string)
	if !strings.Contains(detail, `a resource's id is assigned by the server`) {
		t.Fatalf("detail = %q, want it to name `id` as the offending field (body %s)", detail, raw)
	}
	// The original row is untouched — the rejected create wrote nothing.
	resp, _ = e.do(t, http.MethodGet, "/api/v1/scope-nodes/"+siteID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("original site missing after rejected create: status %d", resp.StatusCode)
	}
}

// TestPatchClientSuppliedIDRejectedAsClientProblem: a PATCH body naming a
// non-empty "id" is rejected the same way a create is (422 with a TOP-LEVEL
// ID_SERVER_ASSIGNED, the offending field named in detail) — and, crucially,
// the row is left entirely
// untouched: not merely its id, but every OTHER field the same patch tried to
// change. This pins the exact danger commit 76dd3a3 named as its own motivation
// for calling rejectClientSuppliedID from patch() too: store.Update's
// merge-over-current-body has no id-immutability check of its own, so a patch
// smuggling an "id" alongside a legitimate field change must never reach it —
// the guard was ADDED to api.go's patch path but, before this test, had zero
// test coverage of its own on that path (only the create path was pinned).
func TestPatchClientSuppliedIDRejectedAsClientProblem(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))

	patch := mustJSON(t, map[string]any{"id": "someone-elses-id", "name": "Renamed Site"})
	resp, raw := e.do(t, http.MethodPatch, "/api/v1/scope-nodes/"+siteID, patch, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode == http.StatusInternalServerError {
		t.Fatalf("client-supplied id on PATCH surfaced as 500 INTERNAL (body %s)", raw)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("client-supplied-id PATCH status = %d, want 422, body %s", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "ID_SERVER_ASSIGNED")
	if _, present := p["errors"]; present {
		t.Fatalf("the Problem carries an errors[] member without VALIDATION_FAILED (body %s)", raw)
	}
	// The detail must name the field the refusal is ABOUT. A bare
	// Contains(detail, "id") passes on the template's own "identity" and
	// "external_id", so it would hold for any field name at all — the assertion
	// it replaced (errors[0].field == "id") genuinely pinned this, and dropping
	// errors[] must not drop the property with it.
	detail, _ := p["detail"].(string)
	if !strings.Contains(detail, `a resource's id is assigned by the server`) {
		t.Fatalf("detail = %q, want it to name `id` as the offending field (body %s)", detail, raw)
	}

	// The row is untouched: same id, same (never-renamed) name, same revision —
	// the rejected patch wrote nothing, neither the smuggled id nor the
	// legitimate name change riding alongside it.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/scope-nodes/"+siteID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("original site missing after rejected patch: status %d", resp.StatusCode)
	}
	var got datamodel.ScopeNode
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode site: %v (body %s)", err, raw)
	}
	if got.ID != siteID {
		t.Fatalf("site id after rejected patch = %q, want unchanged %q", got.ID, siteID)
	}
	if got.Name != "Demo Site" {
		t.Fatalf("site name after rejected patch = %q, want unchanged \"Demo Site\" (patch must not have applied)", got.Name)
	}
	if got.Revision != 1 {
		t.Fatalf("site revision after rejected patch = %d, want unchanged 1", got.Revision)
	}
}

// TestConcurrentCreateExternalIDUniqueness: many concurrent creates that share one
// external_id under one parent (distinct ids, so only the external_id rule can gate
// them) must yield EXACTLY ONE winner — the check-then-write must be atomic
// (API-101/102), not a pre-write snapshot two requests can race past. Each round
// uses a fresh external_id (independent trials); a non-atomic check lets more than
// one winner through in at least one round with overwhelming probability, while an
// atomic guard yields exactly one every round.
func TestConcurrentCreateExternalIDUniqueness(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))

	const rounds = 8
	const n = 16
	for r := 0; r < rounds; r++ {
		externalID := fmt.Sprintf("race-screen-%d", r)
		bodies := make([][]byte, n)
		for i := 0; i < n; i++ {
			// Server-assigned id (ulid.Monotonic, safe for concurrent use) — the
			// PRIMARY KEY never collides regardless of client input.
			bodies[i] = mustJSON(t, screenNode("", siteID, externalID))
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, n)
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				<-start
				resp, _ := e.do(t, http.MethodPost, "/api/v1/scope-nodes", bodies[i], nil)
				codes[i] = resp.StatusCode
			}(i)
		}
		close(start) // release all goroutines at once to maximize overlap
		wg.Wait()

		created := 0
		for _, c := range codes {
			switch c {
			case http.StatusCreated:
				created++
			case http.StatusBadRequest: // EXTERNAL_ID_CONFLICT — expected loser
			default:
				t.Fatalf("round %d: unexpected concurrent create status %d", r, c)
			}
		}
		if created != 1 {
			t.Fatalf("round %d: concurrent creates sharing external_id %q produced %d winners, want exactly 1", r, externalID, created)
		}
	}

	// Exactly `rounds` screen rows persisted overall — one winner per external_id.
	resp, raw := e.do(t, http.MethodGet, "/api/v1/scope-nodes?selector="+url.QueryEscape("kind=screen")+"&limit=200", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list screens status = %d", resp.StatusCode)
	}
	var p struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(p.Items) != rounds {
		t.Fatalf("screens persisted = %d, want exactly %d (one winner per external_id)", len(p.Items), rounds)
	}
}

// TestIdempotentCreateReplaysFailure: a keyed create that fails deterministically
// (external_id conflict) must, on an identical-key+body retry, REPLAY that same
// failure (API-052) — not wedge the key InProgress by never completing the entry.
func TestIdempotentCreateReplaysFailure(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))
	e.createNode(t, screenNode("", siteID, "lobby-screen-1"))

	body := mustJSON(t, screenNode("", siteID, "lobby-screen-1"))
	hdr := map[string]string{"Idempotency-Key": "dup-external-id-key"}

	// First keyed create collides → 400 EXTERNAL_ID_CONFLICT (a fresh request:
	// header and body trace_id agree).
	resp1, raw1 := e.do(t, http.MethodPost, "/api/v1/scope-nodes", body, hdr)
	if resp1.StatusCode != http.StatusBadRequest {
		t.Fatalf("first keyed create status = %d, want 400, body %s", resp1.StatusCode, raw1)
	}
	assertProblem(t, resp1, raw1, "EXTERNAL_ID_CONFLICT")

	// Retry with the identical key+body replays the SAME failed response verbatim,
	// never a 409 IDEMPOTENCY_KEY_IN_PROGRESS from a wedged in-flight marker.
	resp2, raw2 := e.do(t, http.MethodPost, "/api/v1/scope-nodes", body, hdr)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed keyed create status = %d, want 400 (wedged idempotency key?), body %s", resp2.StatusCode, raw2)
	}
	var p2 map[string]any
	if err := json.Unmarshal(raw2, &p2); err != nil {
		t.Fatalf("decode replayed problem: %v (body %s)", err, raw2)
	}
	if p2["code"] != "EXTERNAL_ID_CONFLICT" {
		t.Fatalf("replayed problem code = %v, want EXTERNAL_ID_CONFLICT (body %s)", p2["code"], raw2)
	}
	// Verbatim replay (API-052): the retained failed response is returned byte-for-byte.
	if !bytes.Equal(raw1, raw2) {
		t.Fatalf("failure replay body differs (not verbatim):\n first:  %s\n replay: %s", raw1, raw2)
	}
}

func TestIdempotentCreateReplay(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))

	// A create body WITHOUT an id (server-assigned) so the two requests are
	// byte-identical and the second replays the first.
	body := mustJSON(t, map[string]any{
		"kind":      "screen",
		"parent_id": siteID,
		"name":      "Idempotent Screen",
	})
	hdr := map[string]string{"Idempotency-Key": "create-screen-key-1"}

	resp1, raw1 := e.do(t, http.MethodPost, "/api/v1/scope-nodes", body, hdr)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, body %s", resp1.StatusCode, raw1)
	}
	id1 := decodeID(t, raw1)

	resp2, raw2 := e.do(t, http.MethodPost, "/api/v1/scope-nodes", body, hdr)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("replayed create status = %d, want 201 (body %s)", resp2.StatusCode, raw2)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Fatalf("replay body differs:\n first: %s\n replay: %s", raw1, raw2)
	}

	// Only ONE resource was created (the replay did not execute a second write).
	resp, raw := e.do(t, http.MethodGet, "/api/v1/scope-nodes?selector="+url.QueryEscape("kind=screen"), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list screens status = %d", resp.StatusCode)
	}
	var p struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(p.Items) != 1 {
		t.Fatalf("screens after idempotent replay = %d, want 1", len(p.Items))
	}
	if got := decodeID(t, p.Items[0]); got != id1 {
		t.Fatalf("stored screen id = %q, want the first-created %q", got, id1)
	}
}

// doAsPrincipal is testEnv.do driven as a SPECIFIC principal rather than as the
// fixture's own default credential — for the authorization cases, where who is
// asking is the whole point of the test.
func (e *testEnv) doAsPrincipal(t *testing.T, who authtest.Credential, method, path string, body []byte) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
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
