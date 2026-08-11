package schedulehost

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// video_test.go drives the RELAY side of a scheduled video (parity rows 2.5 and
// 1.5-video).
//
// This projection is not a duplicate of the app-side one for testing purposes —
// it is the one a screen falls back to the moment a daypart boundary passes with
// no app peer connected, which on this fleet is a routine condition, not an edge
// case. So the two must agree, and where they cannot agree literally (a REL-061
// ContentRef's content_type is optional; a Lease's `type` is required) they must
// agree on the RESOLVED value. snapshot.TestDerivedContentMatchesRelaySideProjection
// pins the agreement; these cases pin what this side answers on its own.

// videoAsset is the content-addressed clip these cases schedule.
const videoAsset = "sha256:c0ffee3333333333333333333333333333333333333333333333333333333333"

// TestLeaseContentTypeForResolvesTheRequiredWireDefault pins the one rule this
// side owns that the app side does not: a Lease `type` cannot be empty, so an
// item that authored no content_type is served as `image` — REL-061a's stated
// default and the identical substitution playerserver.SetServedProgram applies
// to the app-signed baseline. If the two ever substituted differently, a screen
// would change what it plays at a daypart boundary for no authored reason.
func TestLeaseContentTypeForResolvesTheRequiredWireDefault(t *testing.T) {
	if got := leaseContentTypeFor(""); got != leaseContentTypeImage {
		t.Errorf("leaseContentTypeFor(\"\") = %q, want %q (REL-061a's absent-means-image default)", got, leaseContentTypeImage)
	}
	if got := leaseContentTypeFor(datamodel.PlaylistContentTypeVideo); got != "video" {
		t.Errorf("leaseContentTypeFor(%q) = %q, want video — an authored type is carried, never overridden", datamodel.PlaylistContentTypeVideo, got)
	}
	if got := leaseContentTypeFor(datamodel.PlaylistContentTypeImage); got != "image" {
		t.Errorf("leaseContentTypeFor(%q) = %q, want image", datamodel.PlaylistContentTypeImage, got)
	}
}

// videoSiteNodeID is the site these cases place everything at, and the node a
// projection is asked about.
const videoSiteNodeID = "01J8ZV1DE0S1TE000000000001"

// videoRowStore is a minimal governed row set — one site node with geo (so an
// effective timezone resolves, DAT-033/034), one schedule, one 06:00–22:00
// content daypart, and the playlist under test — so a projection at midday in
// the site's own zone resolves to that playlist. It is deliberately hand-built rather than derived from the
// demo fixture: these cases need to state the playlist's items exactly, and the
// demo's are fixed.
func videoRowStore(t *testing.T, items []datamodel.PlaylistItem) datamodel.RowStore {
	t.Helper()
	const playlistID = "01J8ZV1DE0P1AY11ST00000001"
	const scheduleID = "01J8ZV1DE0SCHEDV1E00000001"
	const daypartID = "01J8ZV1DE0DAYPART000000001"
	orgBound := "01J8ZV1DE00RGB0VND00000001"

	tz, lat, long := demoSiteTZ, 41.8781, -87.6298
	tree, errs := datamodel.BuildScopeTree([]datamodel.ScopeNode{
		{ID: videoSiteNodeID, Kind: "site", ParentID: &orgBound, Name: "Site", TZ: &tz, Lat: &lat, Long: &long, Revision: 1, CreatedAt: 1, UpdatedAt: 1},
	})
	if len(errs) != 0 {
		t.Fatalf("BuildScopeTree: %+v", errs)
	}
	rows := datamodel.RowSet{
		Playlists: []datamodel.Playlist{{ID: playlistID, ScopeNode: videoSiteNodeID, Name: "p", Items: items, Revision: 1, CreatedAt: 1, UpdatedAt: 1}},
		Schedules: []datamodel.Schedule{{ID: scheduleID, ScopeNode: videoSiteNodeID, Name: "s", Revision: 1, CreatedAt: 1, UpdatedAt: 1}},
		Dayparts: []datamodel.Daypart{{
			ID: daypartID, ScheduleID: scheduleID, ScopeNode: videoSiteNodeID,
			DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6},
			StartTime:  "06:00:00", EndTime: "22:00:00",
			DisplayPower: "on", PlaylistID: playlistID,
			Revision: 1, CreatedAt: 1, UpdatedAt: 1,
		}},
	}
	return datamodel.RowStore{Tree: tree, Rows: rows}
}

// TestProjectLeaseCarriesAnAuthoredVideoContentType is parity row 2.5 on the
// relay's own re-resolution path: an authored `content_type: "video"` item
// reaches the Lease as `type: "video"`, beside an item that authored none and is
// served as `image`.
//
// Both items are in ONE playlist on purpose. A projection that hardcoded a
// single type would pass a test that scheduled only a video (it would look
// right for the wrong reason) or only an image; two items of different types in
// one array is what proves the type is carried PER ITEM.
func TestProjectLeaseCarriesAnAuthoredVideoContentType(t *testing.T) {
	store := videoRowStore(t, []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceAsset, AssetRef: videoAsset, ContentType: datamodel.PlaylistContentTypeVideo},
		{Source: datamodel.PlaylistSourceAsset, AssetRef: videoAsset},
	})

	_, _, content, _, err := ProjectLease(store, videoSiteNodeID, demoLocalInstant(t, 12, 0), "https://origin.example", nil)
	if err != nil {
		t.Fatalf("ProjectLease: %v", err)
	}
	if len(content) != 2 {
		t.Fatalf("content has %d items, want 2; got %+v", len(content), content)
	}
	if content[0].Type != "video" {
		t.Errorf("content[0].type = %q, want video (the authored content_type)", content[0].Type)
	}
	if content[1].Type != "image" {
		t.Errorf("content[1].type = %q, want image (no authored content_type, REL-061a default)", content[1].Type)
	}
	if content[0].URL != "https://origin.example/content/"+videoAsset[len("sha256:"):] {
		t.Errorf("content[0].url = %q, want the content origin's own /content/<hex>", content[0].URL)
	}
}

// TestProjectLeaseDerivesASlideVideoLayerURL is parity row 1.5-video on the
// relay side: a slide's `video` layer gets its fetch url minted from the same
// content origin an image layer's is, so the stack passes the shared
// wire.ValidateSlideLayers gate instead of being dropped.
//
// The item count is asserted first for the same reason it is on the app side:
// the symptom of a missed url derivation is not a broken video but a VANISHED
// slide, so an assertion that only inspected layers would pass over an empty
// content array.
func TestProjectLeaseDerivesASlideVideoLayerURL(t *testing.T) {
	store := videoRowStore(t, []datamodel.PlaylistItem{{
		Source: "slide",
		Slide: &datamodel.Slide{Layers: []wire.Layer{
			{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#000000"},
			{Kind: wire.LayerKindVideo, X: 160, Y: 90, W: 1600, H: 900, AssetRef: videoAsset},
		}},
	}})

	_, _, content, _, err := ProjectLease(store, videoSiteNodeID, demoLocalInstant(t, 12, 0), "https://origin.example", nil)
	if err != nil {
		t.Fatalf("ProjectLease: %v", err)
	}
	if len(content) != 1 {
		t.Fatalf("content has %d items, want 1 (the slide); a video layer with no derived url fails validation and the slide is dropped. Got %+v", len(content), content)
	}
	if content[0].Type != leaseContentTypeSlide {
		t.Fatalf("content[0].type = %q, want slide", content[0].Type)
	}
	if len(content[0].Layers) != 2 {
		t.Fatalf("slide carried %d layers, want 2; got %+v", len(content[0].Layers), content[0].Layers)
	}
	video := content[0].Layers[1]
	if video.Kind != wire.LayerKindVideo {
		t.Fatalf("layers[1].kind = %q, want video", video.Kind)
	}
	want := "https://origin.example/content/" + videoAsset[len("sha256:"):]
	if video.URL != want {
		t.Errorf("the video layer's derived url = %q, want %q", video.URL, want)
	}
	if err := wire.ValidateSlideLayers(content[0].Layers); err != nil {
		t.Errorf("the projected slide layers do not pass the serve-time gate: %v", err)
	}
}
