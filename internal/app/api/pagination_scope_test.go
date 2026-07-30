package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// TestListCursorScopedPerResource pins API-033/035 across the generic list()
// handler: a keyset cursor a list mints is bound to the resource type that
// minted it, so it names a keyset position ONLY for that resource and is
// rejected 400 / CURSOR_INVALID when replayed against a DIFFERENT resource's
// list — never silently paged from as an arbitrary position in the wrong
// collection. Before the fix, list() hardcoded the cursor scope to "" for every
// kind, so a scope-nodes cursor was silently accepted as a valid position by
// the playlists list.
//
// It drives the REAL mux (New), not a hand-mounted subset: two resource
// families going through one shared list() handler is the whole point of the
// case, and New already mounts both. Driving the real mux also means driving it
// AUTHENTICATED — every list is now filtered to the caller's visible scope-node
// set, so a list mounted without the auth middleware would return an empty page
// (no principal, no bindings, nothing visible) and this case could not observe a
// cursor at all. The fixture principal is bound at the workspace root, which
// inherits to every node, so nothing here is filtered and the case still isolates
// exactly one variable: which resource type minted the cursor.
func TestListCursorScopedPerResource(t *testing.T) {
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clock := func() int64 { return int64(1_700_000_000_000) }
	idem := apihttp.NewIdempotencyStore(clock, 0)

	fixture, err := authtest.New(authtest.Config{NowMs: clock})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	t.Cleanup(fixture.Close)

	// A resource's id is exclusively server-assigned (rejectClientSuppliedID) —
	// so the ...B/...C keyset ordering this test depends on is pinned through
	// the injected newID seam instead of a client-supplied id, minting in POST
	// order below (site first, screen second).
	minted := []string{
		"01J8Z0B0000000000000000000",
		"01J8Z0C0000000000000000000",
	}
	next := 0
	newID := func() string {
		id := minted[next]
		next++
		return id
	}
	ts := httptest.NewServer(New(st, idem, clock, newID, origin.New(), "https://content.example", fixture.Auth))
	t.Cleanup(ts.Close)

	do := func(method, path string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		fixture.Authorize(req)
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

	post := func(path, body string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		fixture.Authorize(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("post %s: status %d, body %s", path, resp.StatusCode, raw)
		}
	}

	// The org root the site below hangs off, written directly through the store:
	// the api assigns a resource's own id (rejectClientSuppliedID), and the site
	// body has to name its parent by a constant. The store holds the FULL tree,
	// where an unresolvable parent_id is a DAT-002 violation rather than a
	// subtree boundary, so this row has to be real.
	if _, err := st.Create(context.Background(), store.KindScopeNode,
		json.RawMessage(`{"id":"01J8Z0A0000000000000000000","kind":"org","name":"Demo Org","account_state":"active","entitlements":{}}`)); err != nil {
		t.Fatalf("seed org root: %v", err)
	}

	// Two more scope nodes so a limit=1 list returns a continuation cursor
	// (server-minted fixture ULIDs, per the injected newID sequence above; the
	// ...B/...C suffixes order them so keyset pagination is deterministic). No
	// playlist rows are needed — a foreign cursor is rejected at decode, before
	// any store read.
	post("/api/v1/scope-nodes", `{"kind":"site","parent_id":"01J8Z0A0000000000000000000","name":"Demo Site","tz":"America/Chicago","lat":41.8781,"long":-87.6298}`)
	post("/api/v1/scope-nodes", `{"kind":"screen","parent_id":"01J8Z0B0000000000000000000","name":"Demo Screen"}`)

	// The scope-nodes list mints a cursor bound to its own resource type: it is
	// NOT the bare ULID the unscoped form would emit — it carries the
	// "scope-nodes" scope tag (API-033), which is what makes it un-replayable
	// against another resource.
	resp, raw := do(http.MethodGet, "/api/v1/scope-nodes?limit=1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list scope-nodes status = %d, body %s", resp.StatusCode, raw)
	}
	var p struct {
		Cursor *string `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode page: %v (body %s)", err, raw)
	}
	if p.Cursor == nil {
		t.Fatalf("scope-nodes page cursor is null, want a continuation token")
	}
	if !strings.HasPrefix(*p.Cursor, "scope-nodes_") {
		t.Fatalf("cursor %q is not scoped to its resource type; an unscoped bare-ULID cursor is silently valid across resources (API-033)", *p.Cursor)
	}

	// The SAME cursor replayed against the playlists list — a DIFFERENT resource
	// mounted through the same shared list() handler — is rejected
	// 400 / CURSOR_INVALID, never silently treated as a valid keyset position in
	// the wrong collection (API-035).
	resp, raw = do(http.MethodGet, "/api/v1/playlists?cursor="+url.QueryEscape(*p.Cursor))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("cross-resource cursor status = %d, want 400; a foreign cursor was silently accepted (API-035) (body %s)", resp.StatusCode, raw)
	}
	var prob struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &prob); err != nil {
		t.Fatalf("decode problem: %v (body %s)", err, raw)
	}
	if prob.Code != "CURSOR_INVALID" {
		t.Fatalf("cross-resource cursor code = %q, want CURSOR_INVALID (body %s)", prob.Code, raw)
	}

	// The cursor IS still a valid position for its OWN resource — same-resource
	// replay pages normally, so scoping did not break the roundtrip.
	resp, raw = do(http.MethodGet, "/api/v1/scope-nodes?limit=1&cursor="+url.QueryEscape(*p.Cursor))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same-resource cursor replay status = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
}
