package api_test

// This is the content-pipeline end-to-end oracle: it wires the content-addressed
// asset upload (POST /api/v1/content), the api/1 authoring surface (a playlist
// referencing the uploaded asset_ref + a schedule showing it), the feeder's
// store-derived signed desired-state (snapshot.BuildFromStore, carrying the
// content-origin base in revocation_and_site.content_origin), the relay's
// desired-state apply gate (the hash + signature verification desiredstate.Pull
// performs), and the relay's schedule resolver (schedulehost) IN-PROCESS — no
// network — and then performs the fetch a real screen would: a GET of the
// resolved Lease content item's url against the content origin's own Handler().
//
// The oracle is content-addressing verified end to end: the fetched bytes equal
// the uploaded asset AND their sha256 equals the resolved asset_ref. That is the
// difference between "works in software" (a resolved Lease carries an asset_ref)
// and "renders on hardware" (a screen can actually fetch those bytes). The relay
// is never in this data path (REL-140): it only constructs the url pointer from
// the desired-state content-origin base + the asset_ref; the test does the GET.

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

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/schedulehost"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// TestContentPipelineEndToEnd is the make-it-real oracle for the whole content
// pipeline: upload an asset, author a playlist referencing its asset_ref + a
// schedule showing it, resolve the screen's program through the full signed
// desired-state path, and GET the resolved content item's url — the fetched bytes
// must equal the uploaded asset and hash back to the asset_ref.
func TestContentPipelineEndToEnd(t *testing.T) {
	e := newEnv(t)

	// --- 1. Upload an asset over the content-addressed origin.
	asset := []byte("waiveo-next content-pipeline end-to-end asset — the quick brown fox\x00\x01\x02")
	up := e.uploadContent(t, asset)
	wantRef := signhash.ContentID(asset)
	if up.AssetRef != wantRef {
		t.Fatalf("upload asset_ref = %q, want %q (server-computed sha256)", up.AssetRef, wantRef)
	}

	// --- 2. Author a playlist referencing the uploaded asset_ref + a schedule
	// showing it, all over the api/1 handler.
	siteID := e.createNode(t, siteNode(""))
	screenID := e.createNode(t, screenNode("", siteID, ""))

	pl := datamodel.Playlist{
		ScopeNode: screenID,
		Name:      "Content Pipeline Playlist",
		Items:     []datamodel.PlaylistItem{{Source: "asset", AssetRef: up.AssetRef}},
	}
	playlistID := decodeID(t, e.createOK(t, "/api/v1/playlists", mustJSON(t, pl)))

	sch := datamodel.Schedule{ScopeNode: screenID, Name: "Content Pipeline Schedule"}
	scheduleID := decodeID(t, e.createOK(t, "/api/v1/schedules", mustJSON(t, sch)))

	// A content daypart 06:00–22:00 every day, playing the playlist — the demo
	// resolves at noon Chicago (e2eContentInstant), inside this window.
	dp := datamodel.Daypart{
		ScheduleID: scheduleID, ScopeNode: screenID,
		DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "06:00:00", EndTime: "22:00:00",
		DisplayPower: "on", PlaylistID: playlistID, Name: "Content Hours",
	}
	e.createOK(t, "/api/v1/dayparts", mustJSON(t, dp))

	// --- 3. Resolve the screen's program through the full signed desired-state
	// path (BuildFromStore carries content_origin; the apply gate verifies it),
	// then project the governed screen's Lease at noon Chicago.
	content := resolveContentThroughDesiredState(t, e, screenID, e2eContentInstant(t))
	if len(content) != 1 {
		t.Fatalf("resolved content items = %d, want 1 (the authored playlist asset)", len(content))
	}
	item := content[0]
	if item.AssetRef != up.AssetRef {
		t.Fatalf("resolved asset_ref = %q, want the uploaded %q", item.AssetRef, up.AssetRef)
	}
	// The resolved url is the single-sourced <base>/content/<hex> form (REL-061):
	// byte-identical to what the upload endpoint returned for the same asset+base.
	if item.URL != up.URL {
		t.Fatalf("resolved url = %q, want the upload's url %q (single-sourced form)", item.URL, up.URL)
	}

	// --- 4. Fetch the resolved content item's url against the content origin's
	// own Handler() — the GET a real screen performs (the relay is never in this
	// path, REL-140). The bytes must equal the uploaded asset and hash back to the
	// asset_ref: content-addressing verified end to end.
	u, err := url.Parse(item.URL)
	if err != nil {
		t.Fatalf("parse resolved url %q: %v", item.URL, err)
	}
	cs := httptest.NewServer(e.content.Handler())
	t.Cleanup(cs.Close)

	fresp, err := http.Get(cs.URL + u.Path)
	if err != nil {
		t.Fatalf("GET resolved content url: %v", err)
	}
	defer fresp.Body.Close()
	fetched, err := io.ReadAll(fresp.Body)
	if err != nil {
		t.Fatalf("read content body: %v", err)
	}
	if fresp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200 (body %s)", u.Path, fresp.StatusCode, fetched)
	}
	if !bytes.Equal(fetched, asset) {
		t.Fatalf("fetched bytes != uploaded asset (%d vs %d bytes)", len(fetched), len(asset))
	}
	if id := signhash.ContentID(fetched); id != up.AssetRef {
		t.Fatalf("fetched-bytes content id = %q, want the resolved asset_ref %q", id, up.AssetRef)
	}
}

// TestPlaylistUnknownAssetRefRejected pins the authoring guard: a playlist item
// whose asset_ref was never uploaded to the shared content origin is rejected
// 422 / VALIDATION_FAILED naming the missing asset (you cannot schedule content
// that cannot be served), and nothing is stored.
func TestPlaylistUnknownAssetRefRejected(t *testing.T) {
	e := newEnv(t)
	siteID := e.createNode(t, siteNode(""))
	screenID := e.createNode(t, screenNode("", siteID, ""))

	// A content-addressed ref for bytes that are NEVER uploaded to the origin.
	unknown := signhash.ContentID([]byte("never uploaded to the content origin"))
	pl := datamodel.Playlist{
		ScopeNode: screenID, Name: "Dangling Asset Playlist",
		Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: unknown}},
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/playlists", mustJSON(t, pl), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("un-uploaded asset_ref playlist status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")

	// The per-field errors name the missing asset_ref (API-013).
	errsAny, _ := p["errors"].([]any)
	found := false
	for _, ei := range errsAny {
		em, _ := ei.(map[string]any)
		msg, _ := em["message"].(string)
		if em["code"] == "REFERENCE_INVALID" && strings.Contains(msg, unknown) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("VALIDATION_FAILED errors did not name the missing asset_ref %q: %v", unknown, errsAny)
	}

	// The playlist was not stored.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/playlists", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list playlists status = %d, body %s", resp.StatusCode, raw)
	}
	var listed struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed.Items) != 0 {
		t.Fatalf("dangling-asset playlist was stored despite VALIDATION_FAILED: %d items", len(listed.Items))
	}
}

// TestPlaylistReauthoringSurvivesOriginRestart pins the second-order effect of
// the persistence asymmetry: validatePlaylistAssets gates on origin.Store.Has at
// write time, so if the content origin lost an uploaded asset on restart, a PATCH
// (or re-POST) of a playlist referencing that already-uploaded content would be
// spuriously rejected 422 REFERENCE_INVALID — the guard "you cannot schedule
// content that cannot be served" inverting into "you cannot re-edit a playlist
// for content you already uploaded." With a dir-backed origin the asset survives,
// so authoring the playlist after a restart succeeds.
func TestPlaylistReauthoringSurvivesOriginRestart(t *testing.T) {
	dir := t.TempDir()
	asset := []byte("waiveo-next restart-survivor asset — uploaded before the feeder restarts")
	ref := signhash.ContentID(asset)

	// Lifetime 1: an operator uploads the asset to a persistent content origin.
	c1, err := origin.Open(dir)
	if err != nil {
		t.Fatalf("origin.Open (lifetime 1): %v", err)
	}
	if _, err := c1.Add(asset); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Restart: a fresh feeder process reopens the SAME content dir — no shared
	// in-memory map, exactly as a real restart would reload persisted content.
	c2, err := origin.Open(dir)
	if err != nil {
		t.Fatalf("origin.Open (restart): %v", err)
	}
	if !c2.Has(strings.TrimPrefix(ref, "sha256:")) {
		t.Fatalf("precondition: the uploaded asset did not survive the restart")
	}

	// Author a playlist referencing the already-uploaded asset over a fresh api
	// handler bound to the reopened origin. The write MUST NOT be rejected 422 —
	// the content is present in the origin, so the guard is satisfied.
	e := newEnvWithContent(t, c2)
	siteID := e.createNode(t, siteNode(""))
	screenID := e.createNode(t, screenNode("", siteID, ""))

	pl := datamodel.Playlist{
		ScopeNode: screenID, Name: "Restart Survivor Playlist",
		Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: ref}},
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/playlists", mustJSON(t, pl), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("authoring a playlist for content uploaded before the restart: status %d, body %s (want 201 — the guard must not invert into rejecting already-uploaded content)", resp.StatusCode, raw)
	}
}

// resolveContentThroughDesiredState runs the make-it-real path a relay drives, in
// process and with no network: it derives the store's desired state, signs it
// (snapshot.BuildFromStore, carrying content_origin), verifies it with the exact
// gate the relay's desiredstate.Pull enforces (the sections hash to the snapshot's
// own hash, REL-053, and the signature verifies under the feeder key, REL-075),
// then parses the carried schedule section into a data-model RowStore and projects
// the governed screen's Lease at nowMs — carrying the desired-state content-origin
// base into resolution so each resolved content item gets its fetchable url
// (REL-061/140). It returns the resolved content items.
func resolveContentThroughDesiredState(t *testing.T, e *testEnv, screenNodeID string, nowMs int64) []wire.LeaseContent {
	t.Helper()

	ds, err := e.store.DesiredState(context.Background())
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	// The content-origin base the snapshot carries is the SAME base the upload
	// endpoint built its url from (e.contentBase) — so an app-authored url and a
	// schedule-resolved url are byte-identical for the same asset (REL-061).
	snap, err := snapshot.BuildFromStore(ds, []byte("content-pipeline-screen-image"), e.contentBase, id, nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}

	// Apply gate: the same verification the relay's desiredstate.Pull performs.
	recomputed, err := wire.HashSections(snap.Sections)
	if err != nil {
		t.Fatalf("HashSections: %v", err)
	}
	if recomputed != snap.Hash {
		t.Fatalf("snapshot hash %q != recomputed %q (REL-053)", snap.Hash, recomputed)
	}
	canon, err := wire.SignedScopeBytes(snap.Generation, snap.Hash)
	if err != nil {
		t.Fatalf("SignedScopeBytes: %v", err)
	}
	sig, err := wire.DecodeSignature(snap.Signature)
	if err != nil {
		t.Fatalf("DecodeSignature: %v", err)
	}
	if !signhash.Verify(id.SigningPub(), canon, sig) {
		t.Fatalf("snapshot signature did not verify under the feeder key (REL-075)")
	}

	// Resolve the governed screen's program from the verified schedule section,
	// threading the content-origin base into the projection.
	rowStore, errs := schedulehost.BuildStore(snap.Sections.Schedule)
	if len(errs) != 0 {
		t.Fatalf("schedulehost.BuildStore reported errors over the authored schedule: %+v", errs)
	}
	if !schedulehost.Governs(rowStore, screenNodeID) {
		t.Fatalf("the authored schedule does not govern screen %s", screenNodeID)
	}
	display, _, content, _, err := schedulehost.ProjectLease(rowStore, screenNodeID, nowMs, snap.Sections.RevocationAndSite.ContentOrigin)
	if err != nil {
		t.Fatalf("ProjectLease: %v", err)
	}
	if display != "content" {
		t.Fatalf("resolved display = %q, want content (authored content daypart at noon Chicago)", display)
	}
	return content
}
