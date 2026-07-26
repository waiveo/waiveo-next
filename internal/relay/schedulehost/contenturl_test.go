package schedulehost

import (
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// singleAssetStore builds the minimal RowStore playlistContent reads — one
// playlist carrying a single `asset` item whose asset_ref is assetRef (a
// `sha256:<hex>` content id) — enough to exercise the asset_ref -> content-URL
// projection without a full scope tree / schedule.
func singleAssetStore(playlistID, assetRef string) datamodel.RowStore {
	return datamodel.RowStore{
		Rows: datamodel.RowSet{
			Playlists: []datamodel.Playlist{
				{
					ID:   playlistID,
					Name: "Content URL Fixture",
					Items: []datamodel.PlaylistItem{
						{Source: "asset", AssetRef: assetRef},
					},
					Revision: 1, CreatedAt: 1, UpdatedAt: 1,
				},
			},
		},
	}
}

// TestPlaylistContentThreadsContentOriginURL asserts playlistContent stamps
// each asset item's Lease `url` as `<contentOrigin>/content/<hex(asset_ref)>`
// (the sha256: prefix stripped), carrying the matching asset_ref and a zero
// expires_at — the single-sourced REL-061 URL grammar, no box-local origin.
func TestPlaylistContentThreadsContentOriginURL(t *testing.T) {
	const (
		playlistID = "01J8ZURLFIXTUREPLAYLIST001"
		assetRef   = "sha256:ABC"
	)
	store := singleAssetStore(playlistID, assetRef)

	content := playlistContent(store, playlistID, "https://origin.example")
	if len(content) != 1 {
		t.Fatalf("playlistContent returned %d items, want 1", len(content))
	}
	if content[0].AssetRef != assetRef {
		t.Errorf("content[0].asset_ref = %q, want %q", content[0].AssetRef, assetRef)
	}
	if want := "https://origin.example/content/ABC"; content[0].URL != want {
		t.Errorf("content[0].url = %q, want %q (<content-origin>/content/<hex>, REL-061)", content[0].URL, want)
	}
	if content[0].ExpiresAt != 0 {
		t.Errorf("content[0].expires_at = %d, want 0 (no TTL policy yet, matching snapshot.Build)", content[0].ExpiresAt)
	}
}

// multiAssetStore builds a RowStore carrying one playlist with three ordered
// `asset` items, the middle one carrying a `duration_seconds` override — enough
// to exercise playlistContent's ordered multi-item projection and its
// duration_seconds -> duration_ms carry (PLY-083b).
func multiAssetStore(playlistID string, assetRefs [3]string, middleDurationSeconds int) datamodel.RowStore {
	dur := middleDurationSeconds
	return datamodel.RowStore{
		Rows: datamodel.RowSet{
			Playlists: []datamodel.Playlist{
				{
					ID:   playlistID,
					Name: "Multi-Item Cast Fixture",
					Items: []datamodel.PlaylistItem{
						{Source: "asset", AssetRef: assetRefs[0]},
						{Source: "asset", AssetRef: assetRefs[1], DurationSeconds: &dur},
						{Source: "asset", AssetRef: assetRefs[2]},
					},
					Revision: 1, CreatedAt: 1, UpdatedAt: 1,
				},
			},
		},
	}
}

// TestPlaylistContentOrderedMultiItemCarriesDurationOverride asserts
// playlistContent (a) projects an N-item playlist into an N-item Lease
// content array IN ORDER (PLY-083a) — not just its first item — and (b)
// carries an item's own duration_seconds override onto that item's own
// duration_ms as *1000 (PLY-083b), while an item with no override carries no
// duration_ms key at all (omitempty, byte-identical to this function's
// pre-existing no-duration output).
func TestPlaylistContentOrderedMultiItemCarriesDurationOverride(t *testing.T) {
	const playlistID = "01J8ZURLFIXTUREPLAYLIST004"
	assetRefs := [3]string{"sha256:AAA", "sha256:BBB", "sha256:CCC"}
	store := multiAssetStore(playlistID, assetRefs, 9)

	content := playlistContent(store, playlistID, "https://origin.example")
	if len(content) != 3 {
		t.Fatalf("playlistContent returned %d items, want 3", len(content))
	}
	for i, want := range assetRefs {
		if content[i].AssetRef != want {
			t.Errorf("content[%d].asset_ref = %q, want %q (order must be preserved)", i, content[i].AssetRef, want)
		}
	}
	if content[0].DurationMS != 0 {
		t.Errorf("content[0].duration_ms = %d, want 0 (no override)", content[0].DurationMS)
	}
	if content[1].DurationMS != 9000 {
		t.Errorf("content[1].duration_ms = %d, want 9000 (duration_seconds:9 override -> *1000)", content[1].DurationMS)
	}
	if content[2].DurationMS != 0 {
		t.Errorf("content[2].duration_ms = %d, want 0 (no override)", content[2].DurationMS)
	}
}

// TestPlaylistContentEmptyOriginLeavesURLEmpty asserts that when the desired
// state carried no content-origin base, playlistContent degrades exactly as
// today — the asset_ref still projects but the url is left EMPTY, never a
// fabricated box-local origin (REL-140).
func TestPlaylistContentEmptyOriginLeavesURLEmpty(t *testing.T) {
	const (
		playlistID = "01J8ZURLFIXTUREPLAYLIST002"
		assetRef   = "sha256:ABC"
	)
	store := singleAssetStore(playlistID, assetRef)

	content := playlistContent(store, playlistID, "")
	if len(content) != 1 {
		t.Fatalf("playlistContent returned %d items, want 1", len(content))
	}
	if content[0].AssetRef != assetRef {
		t.Errorf("content[0].asset_ref = %q, want %q", content[0].AssetRef, assetRef)
	}
	if content[0].URL != "" {
		t.Errorf("content[0].url = %q, want \"\" — a missing content origin degrades, it never fabricates a box-local url (REL-140)", content[0].URL)
	}
}

// TestScheduleResolvedURLMatchesSnapshotBuildByteForByte is the single-sourced-
// URL-form oracle (REL-061): the url a schedule-resolved content item carries
// for a given asset + content-origin base is BYTE-IDENTICAL to what
// snapshot.Build (the app-authored path) produces for the same asset + base —
// there is no second content-URL grammar.
func TestScheduleResolvedURLMatchesSnapshotBuildByteForByte(t *testing.T) {
	img := []byte("fixture-image-bytes")
	const base = "https://origin.example"

	assetRef := signhash.ContentID(img)
	store := singleAssetStore("01J8ZURLFIXTUREPLAYLIST003", assetRef)

	resolved := playlistContent(store, "01J8ZURLFIXTUREPLAYLIST003", base)
	if len(resolved) != 1 {
		t.Fatalf("playlistContent returned %d items, want 1", len(resolved))
	}

	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	snap, err := snapshot.Build(img, base, id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	if len(snap.Sections.ScreenPrograms) == 0 || len(snap.Sections.ScreenPrograms[0].Content) == 0 {
		t.Fatalf("snapshot.Build produced no content ref to compare against")
	}
	want := snap.Sections.ScreenPrograms[0].Content[0].URL

	if resolved[0].URL != want {
		t.Errorf("schedule-resolved url = %q, snapshot.Build url = %q — the two content-URL forms must be byte-identical (REL-061)", resolved[0].URL, want)
	}
}

// TestProjectLeaseThreadsContentOriginToResolvedContent asserts the
// content-origin base threads all the way through resolution: resolving the
// demo content daypart with a content origin yields a Lease content item whose
// url is `<origin>/content/<hex(asset_ref)>`, and resolving with an empty
// origin degrades the url to empty (never fabricated).
func TestProjectLeaseThreadsContentOriginToResolvedContent(t *testing.T) {
	store, errs := BuildStore(buildDemoSection(t))
	if len(errs) != 0 {
		t.Fatalf("BuildStore(demo section) errs = %+v, want none", errs)
	}

	_, _, content, _, err := ProjectLease(store, demoScreenScopeNodeID, demoLocalInstant(t, 12, 0), "https://origin.example")
	if err != nil {
		t.Fatalf("ProjectLease(mid-day, origin set): %v", err)
	}
	if len(content) != 1 {
		t.Fatalf("resolved content has %d items, want 1", len(content))
	}
	wantURL := "https://origin.example/content/" + strings.TrimPrefix(content[0].AssetRef, "sha256:")
	if content[0].URL != wantURL {
		t.Errorf("resolved content[0].url = %q, want %q (content origin threaded through resolution, REL-061)", content[0].URL, wantURL)
	}

	_, _, degraded, _, err := ProjectLease(store, demoScreenScopeNodeID, demoLocalInstant(t, 12, 0), "")
	if err != nil {
		t.Fatalf("ProjectLease(mid-day, empty origin): %v", err)
	}
	if len(degraded) != 1 {
		t.Fatalf("degraded content has %d items, want 1", len(degraded))
	}
	if degraded[0].URL != "" {
		t.Errorf("resolved content[0].url = %q with an empty content origin, want \"\" (degrade, REL-140)", degraded[0].URL)
	}
}
