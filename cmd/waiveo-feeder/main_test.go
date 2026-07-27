package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/enroll"
	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	relayclient "github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
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
const feederE2EContentDaypartID = "01J8Z7DEM0DAYPARTC0NTENT01"

// TestDesiredStateSourceCurrentRebuildsOnAPIWriteGenerationBump covers the real
// propagation seam desiredStateSource.current() is: main wires it as the
// /relay/v1 connection server's SnapshotProvider (relayconn.New(src.current,
// …)), so it is the ONLY thing that makes an authored edit reach the wire. It
// caches the signed snapshot by store generation and must rebuild when an api
// write advances that generation.
//
// This is the feeder half of the authoring loop the e2e oracle (internal/app/api)
// does NOT exercise — that oracle calls snapshot.BuildFromStore directly and never
// touches current(). Without this test, a regression that made current() build
// once and cache forever (or key its cache on a constant/wrong field) would leave
// every test green while the served program never flipped: the api PATCH would
// bump the store generation, but every state.pull would keep answering with the
// old one, so the relay would never see a higher generation. The test drives
// current() across a real api-handler PATCH (the same authoring surface main
// mounts) and asserts the served snapshot advances to the new generation with
// new content, while an unchanged generation is served from cache (both
// branches of current()).
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
		items:          []snapshot.CastItem{{Bytes: img}},
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
	ts := httptest.NewServer(api.New(st, idem, clock, ulid.New, origin.New(), "https://192.0.2.12:7420"))
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

// TestAPIWriteNudgesConnectedRelayEndToEnd is the authoring loop's live half
// on the persistent transport, all in-process and with both sides real: an
// api/1 write commits through the store, the store's post-commit hook (main
// wires st.OnCommit(relayConnSrv.NotifyGenerationAdvance)) nudges the live
// /relay/v1 connection with state.changed (REL-057), the relay's nudge
// handler answers with its own state.pull, and the pulled snapshot verifies
// + applies + persists at the advanced generation — write → nudge → pull →
// applied, no poll ticker anywhere.
func TestAPIWriteNudgesConnectedRelayEndToEnd(t *testing.T) {
	ctx := context.Background()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	img := placeholderImage()
	if err := st.SeedDemo(ctx, signhash.ContentID(img)); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}

	// The desired-state source + enrollment + /relay/v1 servers, wired exactly
	// as main wires them (one mux, one mTLS listener, the store's post-commit
	// hook nudging the connection server).
	src := &desiredStateSource{
		store: st, items: []snapshot.CastItem{{Bytes: img}},
		contentBaseURL: "https://192.0.2.12:7420", id: id,
		grants: []wire.PairingGrant{grant.Mint()},
	}
	if _, err := src.current(); err != nil {
		t.Fatalf("src.current: %v", err)
	}
	enrollSrv, err := enroll.NewServer(id)
	if err != nil {
		t.Fatalf("enroll.NewServer: %v", err)
	}
	connSrv := relayconn.New(
		src.current,
		enrollSrv.RelayEnrollmentKey,
		enrollSrv.IsRevoked,
		firstPhotonSite,
		hello.AppPeerImplementedMinors(1, 1),
		firstPhotonRecognizedFeatures,
	)
	mux := http.NewServeMux()
	enrollSrv.Register(mux)
	mux.Handle("/relay/v1", connSrv.Handler())
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = &tls.Config{
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  enrollSrv.ClientCAPool(),
		MinVersion: tls.VersionTLS13,
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)

	// The seam under test: every committed write nudges every live connection.
	st.OnCommit(connSrv.NotifyGenerationAdvance)

	// A real enrolled relay on one persistent connection, whose nudge handler
	// pulls + verifies + applies (the relay binary's own shape).
	relayStore, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = relayStore.Close() })
	if err := relayenroll.Run(ts.URL, relayStore); err != nil {
		t.Fatalf("relayenroll.Run: %v", err)
	}

	appliedCh := make(chan desiredstate.Applied, 4)
	errCh := make(chan error, 4)
	var clientMu sync.Mutex
	var clientRef *relayclient.Client
	onGeneration := func(int64) {
		clientMu.Lock()
		c := clientRef
		clientMu.Unlock()
		since, _, ok, _ := relayStore.LastAppliedGeneration()
		var sincePtr *int64
		if ok {
			sincePtr = &since
		}
		reply, err := c.Pull("trace-nudge-e2e", sincePtr)
		if err != nil {
			errCh <- err
			return
		}
		if reply.Type != wire.FrameTypeStateSnapshot {
			return // already converged (state.unchanged) — nothing to apply
		}
		body, raw, err := relayclient.SnapshotFromFrame(reply)
		if err != nil {
			errCh <- err
			return
		}
		applied, err := desiredstate.VerifyAndApply(relayStore, body, raw)
		if err != nil {
			errCh <- err
			return
		}
		appliedCh <- applied
	}
	client, err := relayclient.Dial(relayclient.Config{
		URL: ts.URL, Store: relayStore,
		Declaration: hello.Declaration{
			ProtocolVersion: "1.0",
			Features:        firstPhotonRecognizedFeatures,
			ClockState:      hello.ClockState{State: "untrusted", Source: "cold_boot"},
		},
		OnGenerationAdvance: onGeneration,
	})
	if err != nil {
		t.Fatalf("relayclient.Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	clientMu.Lock()
	clientRef = client
	clientMu.Unlock()

	// Boot pull, so the relay holds the pre-write generation.
	reply, err := client.Pull("trace-boot-e2e", nil)
	if err != nil {
		t.Fatalf("boot Pull: %v", err)
	}
	body, raw, err := relayclient.SnapshotFromFrame(reply)
	if err != nil {
		t.Fatalf("SnapshotFromFrame: %v", err)
	}
	booted, err := desiredstate.VerifyAndApply(relayStore, body, raw)
	if err != nil {
		t.Fatalf("boot VerifyAndApply: %v", err)
	}

	// The authoring write, over the REAL api handler (the same surface main
	// mounts): PATCH the seeded content daypart under If-Match.
	clock := func() int64 { return int64(1_700_000_000_000) }
	apiTS := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, ulid.New, origin.New(), "https://192.0.2.12:7420"))
	t.Cleanup(apiTS.Close)
	getResp, _ := doFeederReq(t, apiTS, http.MethodGet, "/api/v1/dayparts/"+feederE2EContentDaypartID, nil, nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET content daypart: status %d", getResp.StatusCode)
	}
	patchResp, rawBody := doFeederReq(t, apiTS, http.MethodPatch, "/api/v1/dayparts/"+feederE2EContentDaypartID,
		[]byte(`{"display_power":"blank"}`), map[string]string{"If-Match": getResp.Header.Get("ETag")})
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH daypart: status %d, body %s", patchResp.StatusCode, rawBody)
	}

	// write → nudge → pull → applied: the relay converges on the advanced
	// generation without any poll loop.
	select {
	case applied := <-appliedCh:
		if applied.Generation <= booted.Generation {
			t.Fatalf("nudge applied generation %d, want > the booted %d", applied.Generation, booted.Generation)
		}
		if gen, _, ok, err := relayStore.LastAppliedGeneration(); err != nil || !ok || gen != applied.Generation {
			t.Fatalf("persisted last-applied = (%d,%v,%v), want (%d,true,nil)", gen, ok, err, applied.Generation)
		}
	case err := <-errCh:
		t.Fatalf("nudge-triggered pull/apply failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no state.changed nudge reached the relay within 5s of the api write (OnCommit seam broken)")
	}
}

// TestLoadConfigDemoCastDefaultAndOverride asserts WAIVEO_FEEDER_DEMO_CAST
// defaults to "" (the single first-photon image, byte-identical to every
// prior release) and an explicit "multi" override is read through — the flag
// a box deployment sets to serve the real 3-item demo cast instead.
func TestLoadConfigDemoCastDefaultAndOverride(t *testing.T) {
	def := loadConfig(func(string) string { return "" })
	if def.demoCast != "" {
		t.Errorf("default demoCast = %q, want \"\" (single first-photon image)", def.demoCast)
	}
	env := map[string]string{"WAIVEO_FEEDER_DEMO_CAST": "multi"}
	got := loadConfig(func(k string) string { return env[k] })
	if got.demoCast != "multi" {
		t.Errorf("demoCast = %q, want the explicit override %q", got.demoCast, "multi")
	}
}

// TestDesiredStateSourceMultiItemCastOrderedAndVerifiable asserts a
// desiredStateSource configured with snapshot.DemoCastItems' real 3-item cast
// (the WAIVEO_FEEDER_DEMO_CAST=multi path) serves a snapshot whose
// screen_programs[0].content carries all 3 items, in order, each keeping its
// own asset_ref matching its own bytes — the per-item content-digest
// integrity property, exercised here against this codebase's real committed
// demo assets rather than synthetic fixture bytes.
func TestDesiredStateSourceMultiItemCastOrderedAndVerifiable(t *testing.T) {
	ctx := context.Background()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	img := placeholderImage()
	if err := st.SeedDemo(ctx, signhash.ContentID(img)); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	items, err := snapshot.DemoCastItems()
	if err != nil {
		t.Fatalf("snapshot.DemoCastItems: %v", err)
	}
	if len(items) < 2 {
		t.Fatalf("DemoCastItems returned %d items, want at least 2 to exercise ordering", len(items))
	}

	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}

	src := &desiredStateSource{
		store:          st,
		items:          items,
		contentBaseURL: "https://192.0.2.12:7420",
		id:             id,
		grants:         []wire.PairingGrant{grant.Mint()},
	}

	snap, err := src.current()
	if err != nil {
		t.Fatalf("current(): %v", err)
	}

	if len(snap.Sections.ScreenPrograms) != 1 {
		t.Fatalf("len(ScreenPrograms) = %d, want 1", len(snap.Sections.ScreenPrograms))
	}
	content := snap.Sections.ScreenPrograms[0].Content
	if len(content) != len(items) {
		t.Fatalf("len(Content) = %d, want %d", len(content), len(items))
	}
	for i, item := range items {
		wantAssetRef := signhash.ContentID(item.Bytes)
		if content[i].AssetRef != wantAssetRef {
			t.Errorf("item %d: asset_ref = %q, want %q (its own content digest)", i, content[i].AssetRef, wantAssetRef)
		}
		if content[i].ContentType != "image" {
			t.Errorf("item %d: content_type = %q, want %q", i, content[i].ContentType, "image")
		}
	}
}
