package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

func TestLoadConfigDefaultsAreLoopback(t *testing.T) {
	// No env set -> the exact Wave-1 loopback behavior, so make dev / CI are
	// unchanged by the config plumbing.
	cfg := loadConfig(func(string) string { return "" })
	if cfg.listen != "127.0.0.1:7420" {
		t.Errorf("default listen = %q, want 127.0.0.1:7420", cfg.listen)
	}
	if cfg.contentBaseURL != "https://127.0.0.1:7420" {
		t.Errorf("default contentBaseURL = %q, want https://127.0.0.1:7420", cfg.contentBaseURL)
	}
}

func TestLoadConfigStoreDefaultAndOverride(t *testing.T) {
	// Unset -> the make-dev-local default under .dev/.
	def := loadConfig(func(string) string { return "" })
	if def.storePath != ".dev/feeder-store.db" {
		t.Errorf("default storePath = %q, want .dev/feeder-store.db", def.storePath)
	}
	// Explicit override wins (the on-box deployment points it at its data volume).
	env := map[string]string{"WAIVEO_FEEDER_STORE": "/var/lib/waiveo/feeder.db"}
	got := loadConfig(func(k string) string { return env[k] })
	if got.storePath != "/var/lib/waiveo/feeder.db" {
		t.Errorf("storePath = %q, want the explicit override", got.storePath)
	}
}

func TestLoadConfigContentDirDefaultAndOverride(t *testing.T) {
	// Unset -> the make-dev-local default under .dev/, a sibling of the store DB;
	// it MUST persist alongside storePath so uploaded content survives a restart
	// in lock-step with the playlists that reference it.
	def := loadConfig(func(string) string { return "" })
	if def.contentPath != ".dev/feeder-content" {
		t.Errorf("default contentPath = %q, want .dev/feeder-content", def.contentPath)
	}
	// Explicit override wins (the on-box deployment points it at its data volume).
	env := map[string]string{"WAIVEO_FEEDER_CONTENT_DIR": "/var/lib/waiveo/content"}
	got := loadConfig(func(k string) string { return env[k] })
	if got.contentPath != "/var/lib/waiveo/content" {
		t.Errorf("contentPath = %q, want the explicit override", got.contentPath)
	}
}

func TestLoadConfigContentURLDefaultsToListen(t *testing.T) {
	// Overriding only the listen address carries into the content base URL, so
	// a screen's direct fetch targets the same host the feeder binds — unless
	// an explicit content URL says otherwise (next test).
	env := map[string]string{"WAIVEO_FEEDER_LISTEN": "0.0.0.0:7420"}
	cfg := loadConfig(func(k string) string { return env[k] })
	if cfg.listen != "0.0.0.0:7420" {
		t.Errorf("listen = %q, want 0.0.0.0:7420", cfg.listen)
	}
	if cfg.contentBaseURL != "https://0.0.0.0:7420" {
		t.Errorf("contentBaseURL = %q, want https://0.0.0.0:7420", cfg.contentBaseURL)
	}
}

func TestLoadConfigExplicitContentURLWins(t *testing.T) {
	// The real on-box shape: bind all interfaces, but advertise the LAN IP the
	// Roku can actually reach for the direct content fetch.
	env := map[string]string{
		"WAIVEO_FEEDER_LISTEN":      "0.0.0.0:7420",
		"WAIVEO_FEEDER_CONTENT_URL": "https://192.0.2.12:7420",
	}
	cfg := loadConfig(func(k string) string { return env[k] })
	if cfg.contentBaseURL != "https://192.0.2.12:7420" {
		t.Errorf("contentBaseURL = %q, want the explicit LAN URL", cfg.contentBaseURL)
	}
}

// feederE2EContentDaypartID is the seed demo's content daypart (fixture ULID,
// mirroring internal/app/store/seed.go). Blanking it over the api is the authored
// edit that must reach the served desired-state.
const feederE2EContentDaypartID = "01J8Z7DEMODAYPARTCONTENT01"

// TestDesiredStateSourceCurrentRebuildsOnAPIWriteGenerationBump covers the real
// propagation seam desiredStateSource.current() is: main wires it to /state/pull
// (enrollSrv.SetSnapshotProvider(src.current)), so it is the ONLY thing that makes
// an authored edit reach the wire. It caches the signed snapshot by store
// generation and must rebuild when an api write advances that generation.
//
// This is the feeder half of the authoring loop the e2e oracle (internal/app/api)
// does NOT exercise — that oracle calls snapshot.BuildFromStore directly and never
// touches current(). Without this test, a regression that made current() build
// once and cache forever (or key its cache on a constant/wrong field) would leave
// every test green while the served program never flipped: the api PATCH would
// bump the store generation, but /state/pull would keep serving the old one, so
// the relay would never see a higher generation. The test drives current() across
// a real api-handler PATCH (the same authoring surface main mounts) and asserts
// the served snapshot advances to the new generation with new content, while an
// unchanged generation is served from cache (both branches of current()).
func TestDesiredStateSourceCurrentRebuildsOnAPIWriteGenerationBump(t *testing.T) {
	ctx := context.Background()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Seed the demo (site + screen + playlist + two-daypart schedule) — the same
	// authored rows a fresh make dev resolves a program from.
	img := placeholderImage()
	assetRef := signhash.ContentID(img)
	if err := st.SeedDemo(ctx, assetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	// A throwaway per-test desired-state signing identity.
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}

	// The desired-state source exactly as main wires it (fixture content host).
	src := &desiredStateSource{
		store:          st,
		img:            img,
		contentBaseURL: "https://192.0.2.12:7420",
		id:             id,
		grants:         []wire.PairingGrant{grant.Mint()},
	}

	// --- Snapshot A: the seeded generation.
	snapA, err := src.current()
	if err != nil {
		t.Fatalf("current() A: %v", err)
	}

	// Cache-hit branch: a second pull at the unchanged generation serves the same
	// snapshot (same generation, byte-identical content hash) — no spurious rebuild.
	snapACached, err := src.current()
	if err != nil {
		t.Fatalf("current() A (cached): %v", err)
	}
	if snapACached.Generation != snapA.Generation || snapACached.Hash != snapA.Hash {
		t.Fatalf("cache-hit pull changed the served snapshot without a write: gen %d/%d hash %q/%q",
			snapA.Generation, snapACached.Generation, snapA.Hash, snapACached.Hash)
	}

	// --- Author a schedule change over the real api handler (the surface main
	// mounts): GET the content daypart's ETag, then PATCH display_power -> blank
	// under If-Match. The store validates + bumps revision + generation atomically.
	clock := func() int64 { return int64(1_700_000_000_000) }
	idem := apihttp.NewIdempotencyStore(clock, 0)
	ts := httptest.NewServer(api.New(st, idem, clock, origin.New(), "https://192.0.2.12:7420"))
	t.Cleanup(ts.Close)

	getResp, _ := doFeederReq(t, ts, http.MethodGet, "/api/v1/dayparts/"+feederE2EContentDaypartID, nil, nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET content daypart: status %d", getResp.StatusCode)
	}
	etag := getResp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("GET content daypart returned no ETag")
	}
	patch := []byte(`{"display_power":"blank"}`)
	patchResp, raw := doFeederReq(t, ts, http.MethodPatch, "/api/v1/dayparts/"+feederE2EContentDaypartID, patch,
		map[string]string{"If-Match": etag})
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH content daypart display_power->blank: status %d, body %s", patchResp.StatusCode, raw)
	}

	// --- Snapshot B: current() MUST rebuild to the advanced generation.
	snapB, err := src.current()
	if err != nil {
		t.Fatalf("current() B: %v", err)
	}

	// The store generation strictly advanced across the api write (REL-052).
	genNow, err := st.Generation(ctx)
	if err != nil {
		t.Fatalf("Generation: %v", err)
	}
	if !(genNow > snapA.Generation) {
		t.Fatalf("store generation did not advance across the api write: %d -> %d", snapA.Generation, genNow)
	}
	// current() serves THAT generation, not the stale cached one. This is the exact
	// regression the seam exists to prevent: if current() cached forever (or keyed
	// its cache on a constant/wrong field), snapB would still carry snapA.Generation
	// and the relay would never see the higher generation (REL-052/056).
	if snapB.Generation != genNow {
		t.Fatalf("current() served generation %d after the api write, want the store's current %d — the cache did not invalidate",
			snapB.Generation, genNow)
	}
	if !(snapB.Generation > snapA.Generation) {
		t.Fatalf("served snapshot generation did not advance: %d -> %d", snapA.Generation, snapB.Generation)
	}
	// The rebuild carries the authored edit, not just a bumped counter: blanking the
	// content daypart changes the schedule section, so the sections hash flips.
	if snapB.Hash == snapA.Hash {
		t.Fatalf("served snapshot content hash unchanged across the authored edit (%q) — the rebuild did not carry the edit", snapB.Hash)
	}
}

// doFeederReq issues one request against the feeder's httptest-mounted api handler
// and returns the response and body (the request's own body is fully consumed).
func doFeederReq(t *testing.T, ts *httptest.Server, method, path string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
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
