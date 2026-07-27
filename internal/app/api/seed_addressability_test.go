package api_test

// This is DAT-005a's end-to-end proof for the demo seed: every row
// store.SeedDemo inserts, for every kind api/1 mounts a route for (scope-nodes,
// playlists, schedules, dayparts, automations — preset-batch and
// validity-window/fallback have no mounted route at all, a deferred scope
// data-model/1 itself documents), is addressable through the conventions
// layer — not merely present in SQLite. A selector query and a keyset-cursor
// query against the LIVE api.New handler must both succeed and account for
// every seeded row of that kind.
//
// Before DAT-005a's own datamodel.ValidateRows/BuildScopeTree enforcement, the
// demo seed's ids were NOT valid ULIDs (7 of its 8 constants carried a
// disallowed Crockford character; 6 tripped SeedDemo — the seventh, the org
// ancestor, is referenced but never inserted), so this exact test — run
// against those old
// constants — fails at the very first line, SeedDemo itself: see this file's
// sibling commit message for the quoted pre-fix failure.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// seedDemoAssetRef is a syntactically well-formed content-addressed ref for
// SeedDemo's playlist item. SeedDemo writes directly through the store, never
// the api-layer create handler, so the playlist-asset-presence guard
// (validatePlaylistAssets) never runs against it here — this ref need not name
// content actually uploaded to any origin, only look like one.
const seedDemoAssetRef = "sha256:00000000000000000000000000000000000000000000000000000000aabb"

func TestSeedDemoRowsAddressableThroughAPIConventions(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	if err := st.SeedDemo(ctx, seedDemoAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	clock := func() int64 { return fixedNowMs }
	idem := apihttp.NewIdempotencyStore(clock, 0)
	fixture := newAuthFixture(t)
	ts := httptest.NewServer(api.New(st, idem, clock, ulid.Monotonic(), origin.New(), testContentBase, fixture.Auth))
	t.Cleanup(ts.Close)
	e := &testEnv{ts: ts, store: st, content: origin.New(), contentBase: testContentBase, auth: fixture}

	list := func(t *testing.T, path, selector string) []json.RawMessage {
		t.Helper()
		q := url.Values{}
		if selector != "" {
			q.Set("selector", selector)
		}
		resp, raw := e.do(t, http.MethodGet, "/api/v1/"+path+"?"+q.Encode(), nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list %s selector=%q status = %d, body %s", path, selector, resp.StatusCode, raw)
		}
		var page struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatalf("decode %s page: %v (body %s)", path, err, raw)
		}
		return page.Items
	}

	getByID := func(t *testing.T, path, id string) {
		t.Helper()
		resp, raw := e.do(t, http.MethodGet, "/api/v1/"+path+"/"+id, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get %s/%s status = %d, body %s", path, id, resp.StatusCode, raw)
		}
	}

	// Discover the seeded screen's own id via a real selector query — every
	// scheduling-core row's scope_node selector below is keyed on it.
	screenItems := list(t, "scope-nodes", "kind=screen")
	if len(screenItems) != 1 {
		t.Fatalf("selector kind=screen returned %d items, want 1 (the seeded screen)", len(screenItems))
	}
	screenID := decodeID(t, screenItems[0])

	// --- Selector query: for every mounted kind, a query that specifically
	// isolates its seeded row(s) — not merely "list returns something" —
	// succeeds and accounts for exactly the expected row(s), each individually
	// addressable by id.
	selectorChecks := []struct {
		path     string
		selector string // "" = the sanctioned matches-everything selector
		want     int
	}{
		{"scope-nodes", "kind=site", 1},
		{"scope-nodes", "kind=screen", 1},
		{"playlists", "scope_node=" + screenID, 1},
		{"schedules", "scope_node=" + screenID, 1},
		{"dayparts", "scope_node=" + screenID, 2},
		{"automations", "", 1},
	}
	for _, c := range selectorChecks {
		t.Run("selector/"+c.path+"/"+c.selector, func(t *testing.T) {
			items := list(t, c.path, c.selector)
			if len(items) != c.want {
				t.Fatalf("selector %q over %s matched %d rows, want %d", c.selector, c.path, len(items), c.want)
			}
			for _, it := range items {
				getByID(t, c.path, decodeID(t, it))
			}
		})
	}

	// --- Cursor query: for every mounted kind, paginating its FULL collection
	// one row at a time (limit=1, replaying the returned cursor) terminates and
	// accounts for exactly the seeded row count — the keyset-cursor convention
	// works over these seeded rows, each individually addressable by id.
	cursorChecks := []struct {
		path  string
		total int
	}{
		{"scope-nodes", 2},
		{"playlists", 1},
		{"schedules", 1},
		{"dayparts", 2},
		{"automations", 1},
	}
	for _, c := range cursorChecks {
		t.Run("cursor/"+c.path, func(t *testing.T) {
			var seen []string
			cursor := ""
			for i := 0; i < c.total+2; i++ { // hard cap: a bug here must not hang the test
				u := "/api/v1/" + c.path + "?limit=1"
				if cursor != "" {
					u += "&cursor=" + url.QueryEscape(cursor)
				}
				resp, raw := e.do(t, http.MethodGet, u, nil, nil)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("cursor page over %s status = %d, body %s", c.path, resp.StatusCode, raw)
				}
				var p struct {
					Items  []json.RawMessage `json:"items"`
					Cursor *string           `json:"cursor"`
				}
				if err := json.Unmarshal(raw, &p); err != nil {
					t.Fatalf("decode %s cursor page: %v (body %s)", c.path, err, raw)
				}
				for _, it := range p.Items {
					id := decodeID(t, it)
					seen = append(seen, id)
					getByID(t, c.path, id)
				}
				if p.Cursor == nil {
					break
				}
				cursor = *p.Cursor
			}
			if len(seen) != c.total {
				t.Fatalf("cursor walk over %s covered %d rows, want exactly %d (the seeded count)", c.path, len(seen), c.total)
			}
		})
	}
}
