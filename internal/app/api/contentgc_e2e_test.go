package api_test

// This is the content-reclamation end-to-end oracle, and it is the counterpart of
// content_e2e_test.go's: that one proves an uploaded asset reaches a screen, this
// one proves an asset no screen can reach any more stops occupying the box.
//
// It is driven through the surfaces a deployment actually has — the api/1 upload
// route, the api/1 playlist authoring routes, a real dir-backed content origin
// (files on disk, not a map), a real SQLite app store advancing real generations,
// and the shipping sweeper — and it asserts against the two things that decide
// whether a screen goes dark: the bytes on disk, and what the content origin's own
// HTTP handler answers for the asset's url.
//
// The generation-floor case is the one worth reading. It is the failure this whole
// design is arranged around: an asset dropped from a playlist is unreferenced at
// the current generation while a relay that has not caught up is still serving a
// program that names it. Nothing in the store can see that older generation — it
// is not retained anywhere — so the test drives the fleet oracle to the state a
// lagging relay produces and requires that the sweep declines.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/contentgc"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
)

// gcTestNow is the instant the reclamation cases run at. Every window in the
// sweep is compared against an injected clock, so no case here waits out a real
// window or sleeps.
const gcTestNow = int64(1_800_000_000_000)

// gcOriginEnv is an env whose content origin is DIR-BACKED, so reclamation can be
// asserted against the filesystem rather than against a map this package owns. It
// returns the env, the origin, and the directory the assets land in.
func gcOriginEnv(t *testing.T) (*testEnv, *origin.Store, string) {
	t.Helper()
	dir := t.TempDir()
	// The origin's own clock is pinned to the same instant the sweeper reads, so
	// an asset uploaded during the test is stamped at gcTestNow and its age is
	// whatever the case decides to advance the sweeper's clock by.
	content, err := origin.Open(dir, origin.WithClock(func() int64 { return gcTestNow }))
	if err != nil {
		t.Fatalf("origin.Open: %v", err)
	}
	return newEnvWithContent(t, content), content, dir
}

// gcSweeper builds the shipping sweeper over env's store and origin, with the
// fleet reported as converged on whatever generation the store is at. now is the
// sweeper's clock.
func gcSweeper(t *testing.T, e *testEnv, content *origin.Store, now func() int64) *contentgc.Sweeper {
	t.Helper()
	sw, err := contentgc.New(contentgc.Config{
		Origin:     content,
		References: e.store,
		Fleet:      convergedFleet(t, e),
		NowMs:      now,
	})
	if err != nil {
		t.Fatalf("contentgc.New: %v", err)
	}
	return sw
}

// convergedFleet reports the fleet as having applied exactly the store's current
// generation — the steady state on a box whose relay is connected and caught up.
func convergedFleet(t *testing.T, e *testEnv) contentgc.FleetConverged {
	return func(target int64) (bool, bool) {
		gen, err := e.store.Generation(context.Background())
		if err != nil {
			t.Errorf("read generation for the fleet oracle: %v", err)
			return false, false
		}
		return gen == target, true
	}
}

// gcAuthorPlaylist uploads assets and authors one playlist naming them, returning
// the playlist id and the asset_refs in upload order.
func gcAuthorPlaylist(t *testing.T, e *testEnv, assets ...[]byte) (string, []string) {
	t.Helper()
	siteID := e.createNode(t, siteNode(""))
	screenID := e.createNode(t, screenNode("", siteID, ""))
	refs := make([]string, 0, len(assets))
	items := make([]datamodel.PlaylistItem, 0, len(assets))
	for _, a := range assets {
		ref := e.uploadContent(t, a).AssetRef
		refs = append(refs, ref)
		items = append(items, datamodel.PlaylistItem{Source: "asset", AssetRef: ref})
	}
	pl := datamodel.Playlist{ScopeNode: screenID, Name: "Retention Playlist", Items: items}
	id := decodeID(t, e.createOK(t, "/api/v1/playlists", mustJSON(t, pl)))
	return id, refs
}

// refHex is the origin key an asset_ref names: the hex digest, prefix stripped.
func refHex(assetRef string) string { return strings.TrimPrefix(assetRef, "sha256:") }

// gcServed reports what the content origin's own handler answers for an
// asset_ref — the exact question a screen asks (REL-140: the screen fetches these
// bytes directly, the relay is never in the path).
func gcServed(t *testing.T, content *origin.Store, assetRef string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "/content/"+refHex(assetRef), nil)
	if err != nil {
		t.Fatalf("build content request: %v", err)
	}
	rec := httptest.NewRecorder()
	content.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// gcOnDisk reports whether the origin's directory still holds the asset's file.
func gcOnDisk(t *testing.T, dir, assetRef string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, refHex(assetRef)))
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat content file: %v", err)
	return false
}

// gcSweepTwice runs the sweep twice, at now and then at now+advance. Two runs is
// the minimum that can reclaim anything: the first observation of an unreferenced
// asset only marks it (contentgc guard 3), so a case that swept once and found
// the asset still present would be asserting nothing about the guards it means to
// exercise.
func gcSweepTwice(t *testing.T, sw *contentgc.Sweeper, clock *int64, advance int64) contentgc.Result {
	t.Helper()
	if _, err := sw.Sweep(context.Background()); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	*clock += advance
	res, err := sw.Sweep(context.Background())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	return res
}

// TestContentReclamationEndToEnd is the reclamation oracle: an asset no playlist
// references is reclaimed — gone from disk and 404 from the origin's handler —
// while an asset a playlist still references survives both, in the same sweep of
// the same origin.
//
// The two assets are uploaded together and differ in exactly one respect: one is
// named by the authored playlist. That is the whole hypothesis under test, so
// nothing else about them may differ.
func TestContentReclamationEndToEnd(t *testing.T) {
	e, content, dir := gcOriginEnv(t)

	kept := []byte("waiveo-next retention: the asset a playlist still plays")
	dropped := []byte("waiveo-next retention: the asset nothing references any more")

	_, refs := gcAuthorPlaylist(t, e, kept)
	keptRef := refs[0]
	droppedRef := e.uploadContent(t, dropped).AssetRef

	if gcServed(t, content, droppedRef) != http.StatusOK {
		t.Fatal("the unreferenced asset is not being served before the sweep; the case would prove nothing")
	}

	clock := gcTestNow + contentgc.DefaultMinAssetAgeMs
	sw := gcSweeper(t, e, content, func() int64 { return clock })
	res := gcSweepTwice(t, sw, &clock, contentgc.DefaultMinUnreferencedAgeMs)

	if res.Reclaimed != 1 {
		t.Fatalf("reclaimed %d asset(s), want exactly 1 (retained: %v)", res.Reclaimed, res.Retained)
	}
	if res.ReclaimedBytes != int64(len(dropped)) {
		t.Fatalf("reclaimed %d byte(s), want %d", res.ReclaimedBytes, len(dropped))
	}

	// The unreferenced asset is gone from BOTH representations. Either alone
	// would be a half-reclamation: bytes left on disk come back at the next boot,
	// and bytes left in memory keep being served until one.
	if gcOnDisk(t, dir, droppedRef) {
		t.Error("the unreferenced asset's file is still on disk; it would be reloaded at the next boot")
	}
	if got := gcServed(t, content, droppedRef); got != http.StatusNotFound {
		t.Errorf("the reclaimed asset still serves %d, want 404", got)
	}

	// The referenced asset is untouched — the property that matters more than
	// reclamation working at all.
	if !gcOnDisk(t, dir, keptRef) {
		t.Error("the REFERENCED asset's file was deleted; every screen playing that playlist would go blank")
	}
	if got := gcServed(t, content, keptRef); got != http.StatusOK {
		t.Errorf("the referenced asset serves %d, want 200", got)
	}
}

// TestContentReferencedByALaggingRelayGenerationIsNotReclaimed is the
// older-generation case, and it is the one this design exists for.
//
// An asset is dropped from its playlist, so it is unreferenced at the CURRENT
// generation — but a relay is still serving the previous one, whose screen
// programs name it. The store cannot show that older generation to anybody: it
// keeps no history. The only thing that stands between the sweep and a blank
// screen is the fleet floor, so this drives the floor to the lagging value and
// requires the sweep to decline, then drives it to converged and requires the
// same sweep to proceed — which is what makes this a test of the guard rather
// than a test of a sweep that happened not to fire.
func TestContentReferencedByALaggingRelayGenerationIsNotReclaimed(t *testing.T) {
	e, content, dir := gcOriginEnv(t)

	asset := []byte("waiveo-next retention: content a lagging relay is still showing")
	playlistID, refs := gcAuthorPlaylist(t, e, asset)
	assetRef := refs[0]

	// The generation at which the relay pulled — the one whose screen programs
	// name this asset. Captured BEFORE the drop, exactly as a relay's own
	// applied_generation would be.
	laggingGen, err := e.store.Generation(context.Background())
	if err != nil {
		t.Fatalf("read generation: %v", err)
	}

	// Drop the asset: the playlist keeps existing, with no items.
	empty := mustJSON(t, map[string]any{"items": []datamodel.PlaylistItem{}})
	resp, raw := e.do(t, http.MethodPatch, "/api/v1/playlists/"+playlistID, empty,
		map[string]string{"If-Match": etagOf(t, e, playlistID)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drop the playlist item: status %d (body %s)", resp.StatusCode, raw)
	}

	clock := gcTestNow + contentgc.DefaultMinAssetAgeMs
	fleetGen := laggingGen // the relay has not applied the drop yet
	sw, err := contentgc.New(contentgc.Config{
		Origin: content, References: e.store,
		Fleet: func(target int64) (bool, bool) { return fleetGen == target, true },
		NowMs: func() int64 { return clock },
	})
	if err != nil {
		t.Fatalf("contentgc.New: %v", err)
	}

	res := gcSweepTwice(t, sw, &clock, contentgc.DefaultMinUnreferencedAgeMs)
	if res.Reclaimed != 0 {
		t.Fatalf("reclaimed %d asset(s) while a relay is still serving generation %d; those screens would go blank",
			res.Reclaimed, laggingGen)
	}
	if res.Retained[contentgc.ReasonFleetNotConverged] != 1 {
		t.Fatalf("retained reasons = %v, want the asset held as %q", res.Retained, contentgc.ReasonFleetNotConverged)
	}
	if !gcOnDisk(t, dir, assetRef) {
		t.Fatal("the asset a lagging relay still names was deleted from disk")
	}

	// The SAME sweeper, the SAME asset, the SAME windows — only the fleet's
	// reported generation changes. If it now reclaims, the guard above is what
	// held it, and nothing else was doing the work.
	current, err := e.store.Generation(context.Background())
	if err != nil {
		t.Fatalf("read generation: %v", err)
	}
	fleetGen = current
	after, err := sw.Sweep(context.Background())
	if err != nil {
		t.Fatalf("converged sweep: %v", err)
	}
	if after.Reclaimed != 1 {
		t.Fatalf("with the fleet converged on generation %d the sweep reclaimed %d asset(s), want 1 (retained: %v) — "+
			"the lagging case above proves nothing unless this one reclaims", current, after.Reclaimed, after.Retained)
	}
	if gcOnDisk(t, dir, assetRef) {
		t.Error("the asset survived a converged sweep; nothing would ever reclaim it")
	}
}

// TestContentSweepHoldsWhenARelayIsUnreachable pins the disconnected-relay
// posture: a fleet floor that cannot be determined reclaims NOTHING, rather than
// falling back to the current generation. A relay that is enrolled but offline is
// serving its screens from a program this process cannot read.
func TestContentSweepHoldsWhenARelayIsUnreachable(t *testing.T) {
	e, content, dir := gcOriginEnv(t)

	asset := []byte("waiveo-next retention: unreferenced, with an unreachable relay")
	assetRef := e.uploadContent(t, asset).AssetRef

	clock := gcTestNow + contentgc.DefaultMinAssetAgeMs
	sw, err := contentgc.New(contentgc.Config{
		Origin: content, References: e.store,
		Fleet: func(int64) (bool, bool) { return false, false },
		NowMs: func() int64 { return clock },
	})
	if err != nil {
		t.Fatalf("contentgc.New: %v", err)
	}
	res := gcSweepTwice(t, sw, &clock, contentgc.DefaultMinUnreferencedAgeMs)
	if res.Reclaimed != 0 {
		t.Fatalf("reclaimed %d asset(s) with an unaccountable fleet", res.Reclaimed)
	}
	if !gcOnDisk(t, dir, assetRef) {
		t.Fatal("an asset was deleted while the fleet's generation was unknown")
	}
}

// TestFreshUploadSurvivesASweepBeforeItIsScheduled pins the workflow the minimum
// asset age exists for: a client uploads content and authors the playlist that
// names it later. In between, the asset is referenced by nothing and is
// indistinguishable from garbage — and it must still be there when the playlist
// arrives.
func TestFreshUploadSurvivesASweepBeforeItIsScheduled(t *testing.T) {
	e, content, dir := gcOriginEnv(t)

	asset := []byte("waiveo-next retention: uploaded now, scheduled in a moment")
	assetRef := e.uploadContent(t, asset).AssetRef

	// A day later — long past the unreferenced-observation window, nowhere near
	// the minimum asset age.
	clock := gcTestNow
	sw := gcSweeper(t, e, content, func() int64 { return clock })
	res := gcSweepTwice(t, sw, &clock, contentgc.DefaultMinUnreferencedAgeMs+1)
	if res.Reclaimed != 0 {
		t.Fatalf("reclaimed %d freshly uploaded asset(s)", res.Reclaimed)
	}
	if res.Retained[contentgc.ReasonTooNew] != 1 {
		t.Fatalf("retained reasons = %v, want the upload held as %q", res.Retained, contentgc.ReasonTooNew)
	}
	if !gcOnDisk(t, dir, assetRef) {
		t.Fatal("a freshly uploaded asset was reclaimed before its author could schedule it")
	}

	// And the upload is still schedulable — the failure this guard prevents is
	// not only the missing bytes, it is the 422 the author would get.
	siteID := e.createNode(t, siteNode(""))
	screenID := e.createNode(t, screenNode("", siteID, ""))
	pl := datamodel.Playlist{
		ScopeNode: screenID, Name: "Authored Later",
		Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: assetRef}},
	}
	e.createOK(t, "/api/v1/playlists", mustJSON(t, pl))
}

// etagOf reads the current ETag of a playlist, so a PATCH can carry the
// If-Match the surface requires.
func etagOf(t *testing.T, e *testEnv, playlistID string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, "/api/v1/playlists/"+playlistID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read playlist: status %d (body %s)", resp.StatusCode, raw)
	}
	return resp.Header.Get("ETag")
}
