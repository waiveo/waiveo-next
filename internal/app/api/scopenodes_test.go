package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// A single fixed injected clock so idempotency retention is deterministic and no
// wall-clock read leaks into the api-layer tests.
const fixedNowMs = int64(1_700_000_000_000)

// Fixture ULIDs (fixture-ULID convention; no secrets). The suffix letter orders
// them A<B<C<D<E so keyset (id-ascending) pagination is deterministic.
const (
	boundaryOrgID = "01J8Z0A0000000000000000000" // a subtree boundary parent, never created
	siteID        = "01J8Z0B0000000000000000000"
	screen1ID     = "01J8Z0C0000000000000000000"
	screen2ID     = "01J8Z0D0000000000000000000"
	site2ID       = "01J8Z0E0000000000000000000"

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
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	clock := func() int64 { return fixedNowMs }
	idem := apihttp.NewIdempotencyStore(clock, 0)
	ts := httptest.NewServer(api.New(st, idem, clock, ulid.New, content, testContentBase))
	t.Cleanup(ts.Close)
	return &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase}
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

// createNode POSTs a scope node and fails if the status is not 201.
func (e *testEnv) createNode(t *testing.T, n datamodel.ScopeNode) {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, n), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s: status %d, body %s", n.ID, resp.StatusCode, raw)
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

	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, siteNode(siteID)), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", resp.StatusCode, raw)
	}
	if etag := resp.Header.Get("ETag"); etag != `"1"` {
		t.Fatalf("create ETag = %q, want \"1\"", etag)
	}
	if loc := resp.Header.Get("Location"); loc != "/api/v1/scope-nodes/"+siteID {
		t.Fatalf("create Location = %q, want /api/v1/scope-nodes/%s", loc, siteID)
	}
	if resp.Header.Get("Trace-Id") == "" {
		t.Fatalf("create missing Trace-Id header")
	}
	if got := decodeID(t, raw); got != siteID {
		t.Fatalf("created id = %q, want %q", got, siteID)
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
	e.createNode(t, siteNode(siteID))                  // id ...B
	e.createNode(t, screenNode(screen1ID, siteID, "")) // id ...C

	type page struct {
		Items  []json.RawMessage `json:"items"`
		Cursor *string           `json:"cursor"`
	}

	// Page 1: limit=1 → the first node (site) + a cursor.
	resp, raw := e.do(t, http.MethodGet, "/api/v1/scope-nodes?limit=1", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list p1 status = %d, body %s", resp.StatusCode, raw)
	}
	var p1 page
	if err := json.Unmarshal(raw, &p1); err != nil {
		t.Fatalf("decode p1: %v (body %s)", err, raw)
	}
	if len(p1.Items) != 1 {
		t.Fatalf("page 1 items = %d, want 1", len(p1.Items))
	}
	if p1.Cursor == nil {
		t.Fatalf("page 1 cursor is null, want a continuation token")
	}
	got1 := decodeID(t, p1.Items[0])

	// Page 2: replay the cursor → the remaining node + a null cursor.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/scope-nodes?limit=1&cursor="+url.QueryEscape(*p1.Cursor), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list p2 status = %d, body %s", resp.StatusCode, raw)
	}
	var p2 page
	if err := json.Unmarshal(raw, &p2); err != nil {
		t.Fatalf("decode p2: %v (body %s)", err, raw)
	}
	if len(p2.Items) != 1 {
		t.Fatalf("page 2 items = %d, want 1", len(p2.Items))
	}
	if p2.Cursor != nil {
		t.Fatalf("page 2 cursor = %q, want null (last page)", *p2.Cursor)
	}
	got2 := decodeID(t, p2.Items[0])

	// The two pages cover the collection exactly once, no skip/repeat.
	seen := map[string]bool{got1: true, got2: true}
	if !seen[siteID] || !seen[screen1ID] || got1 == got2 {
		t.Fatalf("pages did not cover {%s,%s} exactly once: p1=%s p2=%s", siteID, screen1ID, got1, got2)
	}

	// A malformed cursor is CURSOR_INVALID, never "from the beginning".
	resp, raw = e.do(t, http.MethodGet, "/api/v1/scope-nodes?cursor="+url.QueryEscape("not a cursor!!"), nil, nil)
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
	e.createNode(t, siteNode(siteID))
	e.createNode(t, screenNode(screen1ID, siteID, ""))
	e.createNode(t, screenNode(screen2ID, siteID, ""))

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
	e.createNode(t, siteNode(siteID))

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
	e.createNode(t, siteNode(siteID))
	e.createNode(t, screenNode(screen1ID, siteID, ""))

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
	e.createNode(t, siteNode(siteID))
	e.createNode(t, siteNode(site2ID))
	e.createNode(t, screenNode(screen1ID, siteID, "lobby-screen-1"))

	// A duplicate external_id under the SAME parent → 400 / EXTERNAL_ID_CONFLICT,
	// no write.
	dup := screenNode(screen2ID, siteID, "lobby-screen-1")
	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, dup), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("dup-external-id status = %d, want 400, body %s", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "EXTERNAL_ID_CONFLICT")
	// The colliding node was not written.
	resp, _ = e.do(t, http.MethodGet, "/api/v1/scope-nodes/"+screen2ID, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("colliding node was created: get status %d, want 404", resp.StatusCode)
	}

	// The SAME external_id under a DIFFERENT parent is NOT a collision (API-101).
	ok := screenNode(screen2ID, site2ID, "lobby-screen-1")
	resp, raw = e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, ok), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("same-external-id-different-parent status = %d, want 201, body %s", resp.StatusCode, raw)
	}
}

// TestCreateDuplicateIDIsClientProblem: a create whose client-supplied id already
// names an existing row is a well-defined client Problem (VALIDATION_FAILED with
// an id-field error), never a masked 500 INTERNAL from a raw PRIMARY KEY constraint.
func TestCreateDuplicateIDIsClientProblem(t *testing.T) {
	e := newEnv(t)
	e.createNode(t, siteNode(siteID))

	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, siteNode(siteID)), nil)
	if resp.StatusCode == http.StatusInternalServerError {
		t.Fatalf("duplicate client-supplied id surfaced as 500 INTERNAL (body %s)", raw)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate-id status = %d, want 422 VALIDATION_FAILED, body %s", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	errsAny, _ := p["errors"].([]any)
	if len(errsAny) == 0 {
		t.Fatalf("VALIDATION_FAILED carried no per-field errors array (body %s)", raw)
	}
	first, _ := errsAny[0].(map[string]any)
	if first["field"] != "id" {
		t.Fatalf("duplicate-id error field = %v, want id (body %s)", first["field"], raw)
	}
	// The original row is untouched — the duplicate create wrote nothing.
	resp, _ = e.do(t, http.MethodGet, "/api/v1/scope-nodes/"+siteID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("original site missing after rejected duplicate create: status %d", resp.StatusCode)
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
	e.createNode(t, siteNode(siteID))

	const rounds = 8
	const n = 16
	for r := 0; r < rounds; r++ {
		externalID := fmt.Sprintf("race-screen-%d", r)
		bodies := make([][]byte, n)
		for i := 0; i < n; i++ {
			// 26-char ULID-shaped ids, globally distinct, so the PRIMARY KEY never collides.
			id := fmt.Sprintf("01J8Z0F%02d%017d", r, i)
			bodies[i] = mustJSON(t, screenNode(id, siteID, externalID))
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
	e.createNode(t, siteNode(siteID))
	e.createNode(t, screenNode(screen1ID, siteID, "lobby-screen-1"))

	body := mustJSON(t, screenNode(screen2ID, siteID, "lobby-screen-1"))
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
	e.createNode(t, siteNode(siteID))

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
