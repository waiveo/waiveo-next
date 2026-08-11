package api_test

// workspaceroundtrip_test.go is parity row 7.5's actual claim, and it is the one
// claim none of the existing workspace tests made:
//
//	export -> a FRESH, EMPTY box -> restore -> the same casts, schedules,
//	screens and IMAGES are back, read through the API.
//
// # Why the tests that already existed do not cover this
//
// workspace_test.go proves an export writes a container that verifies.
// workspacerestore_test.go proves a restore stages the container's snapshot
// bytes beside the live store, and that adopting them puts those bytes live.
// Both compare BYTES. Neither ever opens the restored store and asks it a
// question, and neither restores onto a destination that does not already hold
// the workspace — which is the only destination a restore actually matters on
// (new hardware, a wiped appliance, a migration).
//
// That gap hid a real defect: the restore staged the snapshot and DROPPED every
// embedded asset. The rows came back, the console listed the casts, and every
// image layer named an asset_ref the fresh box's content origin had never heard
// of. Nothing failed — the job reported success and the screens rendered blanks.
// This file is what makes that impossible to reintroduce, because it asserts on
// what the restored deployment can SERVE rather than on what it was handed.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/restoreswap"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/app/workspacekey"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// authoredWorkspace is what a round trip has to bring back, captured by id so
// the restored box is asked about the SAME rows rather than about "some rows".
type authoredWorkspace struct {
	castID     string
	castName   string
	castAsset  []byte
	castRef    string
	scheduleID string
	playlistID string
	screenID   string
	screenName string
}

// authorWorkspace creates one of everything an operator would lose: a cast with
// an image layer, a schedule, a playlist, and a screen row.
func authorWorkspace(t *testing.T, e *workspaceEnv) authoredWorkspace {
	t.Helper()
	who := e.auth.Credential()
	scopeNode := seedSchedulingScope(t, e.testEnv)

	out := authoredWorkspace{
		castName:   "Round-trip Menu",
		castAsset:  []byte("waiveo-next: the image the restored cast must still draw"),
		screenName: "Round-trip Lobby",
	}
	out.castRef = e.uploadContent(t, out.castAsset).AssetRef

	resp, raw := e.as(t, who, http.MethodPost, "/api/v1/casts", rowCreateBody(t, datamodel.Cast{
		ScopeNode: scopeNode, Name: out.castName,
		Slides: []datamodel.CastSlide{{ID: "photo", Layers: []wire.Layer{
			{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: out.castRef},
		}}},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create cast: %d %s", resp.StatusCode, raw)
	}
	out.castID = decodeID(t, raw)

	resp, raw = e.as(t, who, http.MethodPost, "/api/v1/schedules", rowCreateBody(t, scheduleFixture(scopeNode)), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create schedule: %d %s", resp.StatusCode, raw)
	}
	out.scheduleID = decodeID(t, raw)

	resp, raw = e.as(t, who, http.MethodPost, "/api/v1/playlists", rowCreateBody(t, playlistFixture(scopeNode, nil)), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create playlist: %d %s", resp.StatusCode, raw)
	}
	out.playlistID = decodeID(t, raw)

	resp, raw = e.as(t, who, http.MethodPost, "/api/v1/screens",
		mustJSON(t, map[string]any{"scope_node": scopeNode, "name": out.screenName, "labels": map[string]string{}}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create screen: %d %s", resp.StatusCode, raw)
	}
	out.screenID = decodeID(t, raw)
	return out
}

// freshBox is a SECOND, EMPTY deployment: its own store file, its own content
// origin, its own auth fixture — sharing only the archive directory and the
// workspace signing key, which is exactly what an operator carries to new
// hardware (the container, and the key that signed it).
//
// It has to be a genuinely separate deployment for this test to mean anything.
// Restoring onto the box that produced the archive proves nothing about assets:
// the origin already holds every one of them, so a restore that dropped all of
// them would still look perfect.
type freshBox struct {
	*workspaceEnv
}

func newFreshBox(t *testing.T, archiveDir, keyDir string) *freshBox {
	t.Helper()
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	key, err := workspacekey.LoadOrCreate(keyDir, func() string { return workspaceKeyID })
	if err != nil {
		t.Fatalf("workspacekey.LoadOrCreate: %v", err)
	}
	storePath := filepath.Join(t.TempDir(), "fresh.db")
	clock := func() int64 { return fixedNowMs }
	fixture := newAuthFixture(t)
	content := origin.New()
	jobs := api.NewJobRunner()
	ts := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, ulid.Monotonic(),
		content, testContentBase, fixture.Auth,
		api.WithJobRunner(jobs),
		api.WithWorkspaceArchive(&api.WorkspaceArchive{Dir: archiveDir, Key: key, KDF: lightKDF()}),
		api.WithStorePath(storePath)))
	t.Cleanup(ts.Close)

	return &freshBox{workspaceEnv: &workspaceEnv{
		testEnv:    &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture, jobs: jobs},
		archiveDir: archiveDir,
		keyDir:     keyDir,
		key:        key,
		storePath:  storePath,
	}}
}

// servedBy opens a THIRD handler over the store file the fresh box adopted at
// its simulated next boot, plus the content origin the restore populated. This
// is the deployment an operator is actually left with after a restore + restart,
// and it is the one every assertion below interrogates.
func servedBy(t *testing.T, storePath string, content *origin.Store) *testEnv {
	t.Helper()
	st, err := store.Open(storePath, store.WallClockMs)
	if err != nil {
		t.Fatalf("open the adopted store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	clock := func() int64 { return fixedNowMs }
	fixture := newAuthFixture(t)
	jobs := api.NewJobRunner()
	ts := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, ulid.Monotonic(),
		content, testContentBase, fixture.Auth, api.WithJobRunner(jobs)))
	t.Cleanup(ts.Close)
	return &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture, jobs: jobs}
}

// TestAWorkspaceRoundTripsOntoAFreshBox is parity row 7.5's validation, driven
// end to end through the API on both sides.
func TestAWorkspaceRoundTripsOntoAFreshBox(t *testing.T) {
	origBox := newWorkspaceEnv(t)
	origBox.seedWorkspace(t)
	authored := authorWorkspace(t, origBox)

	// ── EXPORT ────────────────────────────────────────────────────────────────
	resp, raw := origBox.postWorkspace(t, origBox.auth.Credential(), "export",
		map[string]any{"passphrase": testExportPassphrase})
	job := acceptedJob(t, resp, raw)
	origBox.runJobs()
	if got := origBox.polledJob(t, origBox.auth.Credential(), job.ID); got.State != "succeeded" {
		t.Fatalf("export job = %q, want succeeded (%+v)", got.State, got.Targets)
	}

	// The operator DISCOVERS the container through the API rather than knowing
	// the job id — the whole reason the listing exists.
	archiveName := onlyArchiveName(t, origBox)
	if archiveName != "workspace-"+job.ID+".waiveo-archive" {
		t.Fatalf("listed archive %q does not name the export job", archiveName)
	}

	// ── A FRESH, EMPTY BOX ────────────────────────────────────────────────────
	fresh := newFreshBox(t, origBox.archiveDir, origBox.keyDir)
	// The fresh box is CLAIMED but empty — which is the real state of new
	// hardware at the moment a restore is performed. An org node exists (the
	// first-boot claim's own row, the thing that makes the box a workspace with
	// an owner to authorize the restore) and nothing else does. Its id differs
	// from the archived workspace's; the swap replaces the whole store at the
	// next boot, so the archived identity is what survives.
	fresh.seedWorkspace(t)
	// Proving it is empty first, so "the rows came back" is a statement about
	// the restore rather than about a fixture that was already there.
	if got := listIDs(t, fresh.testEnv, "/api/v1/casts"); len(got) != 0 {
		t.Fatalf("the fresh box already holds %d cast(s): %v", len(got), got)
	}
	if fresh.content.Has(hexOf(authored.castAsset)) {
		t.Fatal("the fresh box's content origin already holds the cast's image; this test would prove nothing about assets")
	}

	// ── RESTORE ───────────────────────────────────────────────────────────────
	resp, raw = fresh.postWorkspace(t, fresh.auth.Credential(), "restore",
		map[string]any{"archive": archiveName, "passphrase": testExportPassphrase})
	restoreJob := acceptedJob(t, resp, raw)
	fresh.runJobs()
	if got := fresh.polledJob(t, fresh.auth.Credential(), restoreJob.ID); got.State != "succeeded" {
		t.Fatalf("restore job = %q, want succeeded (%+v)", got.State, got.Targets)
	}

	// THE ASSETS ARE BACK, and they are back BEFORE the restart: the store swap
	// is deferred to the next boot, but the bytes a restored row will name have
	// to exist by then, and the content origin is append-only and
	// content-addressed so writing them now disturbs nothing.
	if !fresh.content.Has(hexOf(authored.castAsset)) {
		t.Fatal("the restore did not return the cast's image to the content origin: every restored slide would " +
			"reference an asset_ref this box has never heard of, and every screen would render a blank while the " +
			"restore reported success")
	}

	// ── THE RESTART THAT ADOPTS ───────────────────────────────────────────────
	adopted, err := restoreswap.Adopt(fresh.storePath)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !adopted {
		t.Fatal("the restore staged nothing, so the next boot would adopt nothing")
	}
	restored := servedBy(t, fresh.storePath, fresh.content)

	// ── THE SAME WORKSPACE, READ BACK THROUGH THE API ─────────────────────────
	cast := readRow(t, restored, "/api/v1/casts/"+authored.castID)
	if cast["name"] != authored.castName {
		t.Errorf("restored cast name = %v, want %q", cast["name"], authored.castName)
	}
	if got := castImageRef(t, cast); got != authored.castRef {
		t.Errorf("restored cast's image layer references %q, want %q", got, authored.castRef)
	}
	// And the bytes that ref names really are servable from this deployment —
	// the difference between a row that mentions an image and a screen that can
	// display one.
	if got := restored.content.Serve(hexOf(authored.castAsset)); string(got) != string(authored.castAsset) {
		t.Errorf("the restored deployment cannot serve the cast's image (%d byte(s) back)", len(got))
	}

	if got := readRow(t, restored, "/api/v1/schedules/"+authored.scheduleID); got["id"] != authored.scheduleID {
		t.Errorf("restored schedule = %v", got)
	}
	if got := readRow(t, restored, "/api/v1/playlists/"+authored.playlistID); got["id"] != authored.playlistID {
		t.Errorf("restored playlist = %v", got)
	}
	screen := readRow(t, restored, "/api/v1/screens/"+authored.screenID)
	if screen["name"] != authored.screenName {
		t.Errorf("restored screen name = %v, want %q", screen["name"], authored.screenName)
	}
}

// TestARestoreReturnsEVERYEmbeddedAssetNotSome is the mirror-direction check.
//
// The round trip above proves the restore returns THE asset its one cast names,
// which a fix that happened to write the first entry would also satisfy. This
// counts: the number of assets the restore added to the destination's origin
// must equal the number the container carries. A restore that returns SOME
// assets is a workspace with some blank screens — the same defect wearing a
// smaller number, and exactly the shape a partial fix leaves behind.
func TestARestoreReturnsEVERYEmbeddedAssetNotSome(t *testing.T) {
	origBox := newWorkspaceEnv(t)
	origBox.seedWorkspace(t)
	authorWorkspace(t, origBox)

	resp, raw := origBox.postWorkspace(t, origBox.auth.Credential(), "export",
		map[string]any{"passphrase": testExportPassphrase})
	job := acceptedJob(t, resp, raw)
	origBox.runJobs()
	if got := origBox.polledJob(t, origBox.auth.Credential(), job.ID); got.State != "succeeded" {
		t.Fatalf("export job = %q, want succeeded", got.State)
	}
	name := onlyArchiveName(t, origBox)

	fresh := newFreshBox(t, origBox.archiveDir, origBox.keyDir)
	fresh.seedWorkspace(t)
	before := len(fresh.content.Entries())
	resp, raw = fresh.postWorkspace(t, fresh.auth.Credential(), "restore",
		map[string]any{"archive": name, "passphrase": testExportPassphrase})
	restoreJob := acceptedJob(t, resp, raw)
	fresh.runJobs()
	if got := fresh.polledJob(t, fresh.auth.Credential(), restoreJob.ID); got.State != "succeeded" {
		t.Fatalf("restore job = %q, want succeeded (%+v)", got.State, got.Targets)
	}
	manifest, _ := origBox.openArchive(t, job.ID)
	after := len(fresh.content.Entries())
	if after-before != len(manifest.Assets) {
		t.Fatalf("the restore wrote %d asset(s) but the container carries %d; a restore that returns SOME assets is a "+
			"workspace with some blank screens, which is the same defect wearing a smaller number",
			after-before, len(manifest.Assets))
	}
	if len(manifest.Assets) == 0 {
		t.Fatal("the fixture container carries no assets, so this comparison proves nothing")
	}
}

// mustUnmarshal decodes raw into v, failing the test on a parse error.
func mustUnmarshal(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

// readRow GETs one resource and decodes it, failing on anything but 200 — so a
// row that did NOT come back fails loudly at the read rather than quietly at a
// field comparison against an empty map.
func readRow(t *testing.T, e *testEnv, path string) map[string]any {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s after the restore = %d, want 200 — the row did not come back (body %s)", path, resp.StatusCode, raw)
	}
	var out map[string]any
	mustUnmarshal(t, raw, &out)
	return out
}

// listIDs returns the ids a list operation answers with.
func listIDs(t *testing.T, e *testEnv, path string) []string {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d (body %s)", path, resp.StatusCode, raw)
	}
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	mustUnmarshal(t, raw, &page)
	out := make([]string, 0, len(page.Items))
	for _, it := range page.Items {
		out = append(out, it.ID)
	}
	return out
}

// onlyArchiveName reads GET /workspace/archives and returns the single
// container's name, failing if there is not exactly one.
func onlyArchiveName(t *testing.T, e *workspaceEnv) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, "/api/v1/workspace/archives", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list archives = %d (body %s)", resp.StatusCode, raw)
	}
	var page struct {
		Items []struct {
			Name         string `json:"name"`
			SizeBytes    int64  `json:"size_bytes"`
			DownloadPath string `json:"download_path"`
		} `json:"items"`
		Directory string `json:"directory"`
	}
	mustUnmarshal(t, raw, &page)
	if len(page.Items) != 1 {
		t.Fatalf("listed %d archive(s), want exactly 1: %s", len(page.Items), raw)
	}
	if page.Items[0].SizeBytes <= 0 {
		t.Errorf("listed archive size = %d; an empty container is not a backup", page.Items[0].SizeBytes)
	}
	if page.Directory == "" {
		t.Error("the listing publishes no directory; an operator copying a container back from off-box storage has nowhere to put it")
	}
	return page.Items[0].Name
}

// castImageRef pulls the asset_ref out of the first image layer of a cast's
// first slide, failing if the shape is not what was authored.
func castImageRef(t *testing.T, cast map[string]any) string {
	t.Helper()
	slides, _ := cast["slides"].([]any)
	if len(slides) == 0 {
		t.Fatalf("restored cast carries no slides: %v", cast)
	}
	slide, _ := slides[0].(map[string]any)
	layers, _ := slide["layers"].([]any)
	for _, l := range layers {
		m, _ := l.(map[string]any)
		if m["kind"] == "image" {
			ref, _ := m["asset_ref"].(string)
			return ref
		}
	}
	t.Fatalf("restored cast's first slide has no image layer: %v", slide)
	return ""
}
