package derive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// sync_test.go drives the WHOLE derive loop against a stand-in feeder that
// speaks the real api/1 shapes: list the queue, read the cast, upload the PNG,
// write the reference back under If-Match — and then asserts the thing that
// actually matters, which is that the layer now PROJECTS onto the wire as an
// image a player already knows how to draw.
//
// The renderer is faked and the server is an httptest handler, so this runs
// everywhere. What it cannot prove is that Chromium draws the right picture;
// browser_test.go does that half when a browser is present.

// fakeFeeder is a minimal stand-in for the api/1 surface the tool uses. It holds
// one cast, one playlist and one content origin, so the test can assert on what
// the tool actually WROTE rather than on what it claims to have done.
//
// Both authored shapes are here because both really carry derive layers: a cast
// slide, and a `source: "slide"` playlist item's inline slide. A fake that held
// only casts would let the tool pass every test while an inline-slide layer
// stayed unrenderable forever, which is exactly what shipped.
type fakeFeeder struct {
	mu       sync.Mutex
	cast     datamodel.Cast
	playlist datamodel.Playlist
	etag     string
	listETag string
	content  map[string][]byte

	patches      int
	listPatches  int
	lastIfMatch  string
	lastListIfMa string
	// patchStatus, when non-zero, is returned instead of applying the patch —
	// used to simulate the 409 a concurrent human edit produces.
	patchStatus int
	// omitETag drops the ETag from a cast read, so the write-back has no
	// precondition to state.
	omitETag bool
}

func (f *fakeFeeder) pending() []PendingJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []PendingJob{}
	for _, s := range f.cast.Slides {
		for i, l := range s.Layers {
			st := wire.LayerDeriveState(l)
			if st == wire.DeriveCurrent {
				continue
			}
			out = append(out, PendingJob{
				Source: SourceCast, ResourceID: f.cast.ID, ResourceName: f.cast.Name,
				SlideID: s.ID, LayerIndex: i,
				State: st.String(), SpecDigest: wire.DeriveDigest(l), W: l.W, H: l.H, Spec: l.Derive,
			})
		}
	}
	for ii := range f.playlist.Items {
		if f.playlist.Items[ii].Slide == nil {
			continue
		}
		for i, l := range f.playlist.Items[ii].Slide.Layers {
			st := wire.LayerDeriveState(l)
			if st == wire.DeriveCurrent {
				continue
			}
			idx := ii
			out = append(out, PendingJob{
				Source: SourcePlaylist, ResourceID: f.playlist.ID, ResourceName: f.playlist.Name,
				ItemIndex: &idx, LayerIndex: i,
				State: st.String(), SpecDigest: wire.DeriveDigest(l), W: l.W, H: l.H, Spec: l.Derive,
			})
		}
	}
	return out
}

func (f *fakeFeeder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, `{"code":"UNAUTHENTICATED"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/derive/pending":
		writeJSON(w, http.StatusOK, map[string]any{"derive_jobs": f.pending()})

	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/content":
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		sum := sha256.Sum256(buf.Bytes())
		ref := "sha256:" + hex.EncodeToString(sum[:])
		f.mu.Lock()
		f.content[ref] = buf.Bytes()
		f.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]string{"asset_ref": ref})

	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/casts/"+f.cast.ID:
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.omitETag {
			w.Header().Set("ETag", f.etag)
		}
		writeJSON(w, http.StatusOK, f.cast)

	case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/casts/"+f.cast.ID:
		f.mu.Lock()
		defer f.mu.Unlock()
		f.patches++
		f.lastIfMatch = r.Header.Get("If-Match")
		if f.patchStatus != 0 {
			http.Error(w, `{"code":"REVISION_CONFLICT"}`, f.patchStatus)
			return
		}
		var body struct {
			Slides []datamodel.CastSlide `json:"slides"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.cast.Slides = body.Slides
		writeJSON(w, http.StatusOK, f.cast)

	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/playlists/"+f.playlist.ID:
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("ETag", f.listETag)
		writeJSON(w, http.StatusOK, f.playlist)

	case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/playlists/"+f.playlist.ID:
		f.mu.Lock()
		defer f.mu.Unlock()
		f.listPatches++
		f.lastListIfMa = r.Header.Get("If-Match")
		var body struct {
			Items []datamodel.PlaylistItem `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.playlist.Items = body.Items
		writeJSON(w, http.StatusOK, f.playlist)

	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// pngRenderer returns a real (tiny) PNG so the bytes that flow through the loop
// are the bytes a real renderer would produce, and each page gets its own
// picture so two layers cannot accidentally share a digest.
//
// The picture is keyed off the PAGE, not off a call counter: the loop renders
// concurrently, so a counter would make the same spec produce different bytes
// depending on scheduling — and this fake would then be the only reason two
// layers got distinct asset_refs.
type pngRenderer struct {
	mu sync.Mutex
	n  int
}

func (p *pngRenderer) Render(_ context.Context, page Page) ([]byte, error) {
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(page.HTML))
	img := image.NewNRGBA(image.Rect(0, 0, page.ClipW, page.ClipH))
	img.SetNRGBA(0, 0, color.NRGBA{R: sum[0], G: sum[1], B: sum[2], A: 255})
	return mustPNG(img), nil
}

// calls reports how many renders happened, safely.
func (p *pngRenderer) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

// mustPNG encodes an in-memory image, panicking on the impossible. It exists
// because a Renderer has no *testing.T to fail through, and an encode of an
// NRGBA into a bytes.Buffer has no failure mode a test could provoke.
func mustPNG(img image.Image) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func deriveCastFixture() datamodel.Cast {
	return datamodel.Cast{
		ID: "01J8CAST0000000000000000AA", Name: "Lobby",
		Slides: []datamodel.CastSlide{{
			ID: "s1",
			Layers: []wire.Layer{
				{Kind: wire.LayerKindText, X: 0, Y: 0, W: 800, H: 120, Text: "Welcome"},
				{Kind: wire.LayerKindDerive, X: 1400, Y: 80, W: 400, H: 400, Derive: &wire.DeriveSpec{
					Kind: wire.DeriveKindQR, Data: "https://waiveo.local/pair/ABCD-1234", ECLevel: "M",
				}},
				{Kind: wire.LayerKindDerive, X: 100, Y: 700, W: 900, H: 260, Derive: &wire.DeriveSpec{
					Kind: wire.DeriveKindText, Text: "TODAY", FontPx: 120,
					Fill:   &wire.DeriveFill{Kind: wire.DeriveFillLinear, From: "#7C3AED", To: "#0EA5E9", AngleDeg: 90},
					Shadow: &wire.DeriveShadow{DY: 8, Blur: 20, OpacityPct: 40},
				}},
			},
		}},
	}
}

// derivePlaylistFixture is a playlist whose SECOND item is an inline
// `source: "slide"` stack — the shape the seeded demo workspace itself uses. Its
// derive layer is not in item 0, deliberately: an implementation that assumed
// the first item, or that ignored the index, still writes to the right place
// only if it carries the index through.
func derivePlaylistFixture() datamodel.Playlist {
	return datamodel.Playlist{
		ID: "01J8LIST0000000000000000BB", Name: "Foyer loop",
		Items: []datamodel.PlaylistItem{
			{Source: "asset", AssetRef: "sha256:" + strings.Repeat("c", 64), ContentType: "image"},
			{Source: "slide", Slide: &datamodel.Slide{Layers: []wire.Layer{
				{Kind: wire.LayerKindText, X: 0, Y: 0, W: 800, H: 120, Text: "Scan to pair"},
				{Kind: wire.LayerKindDerive, X: 1400, Y: 80, W: 360, H: 360, Derive: &wire.DeriveSpec{
					Kind: wire.DeriveKindQR, Data: "https://waiveo.local/pair/INLINE-1", ECLevel: "Q",
				}},
			}}},
		},
	}
}

func newSyncEnv(t *testing.T) (*fakeFeeder, *Client, *Runner, *pngRenderer) {
	t.Helper()
	// The playlist starts with NO inline slide, so the cast-only cases keep
	// counting exactly what they always counted; the cases that need the inline
	// shape install derivePlaylistFixture themselves.
	ff := &fakeFeeder{
		cast:     deriveCastFixture(),
		playlist: datamodel.Playlist{ID: "01J8LIST0000000000000000BB", Name: "Foyer loop"},
		etag:     `"rev-1"`, listETag: `"list-1"`,
		content: map[string][]byte{},
	}
	srv := httptest.NewServer(ff)
	t.Cleanup(srv.Close)
	c, err := NewClient(ClientOptions{BaseURL: srv.URL, Token: "test-token", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	pr := &pngRenderer{}
	return ff, c, NewRunner(pr, RunnerOptions{Concurrency: 2, JobTimeout: 10 * time.Second}), pr
}

// TestSyncCarriesADeriveLayerAllTheWayToAnImageOnTheWire is THE test for this
// track. It follows one authored derive layer through every hop the real system
// makes and stops only at the point a player takes over:
//
//	authored derive layer -> the feeder reports it outstanding -> the off-box
//	renderer renders it -> the PNG is uploaded content-addressed -> the
//	reference and its digest are written back -> the layer reads CURRENT ->
//	the content projection turns it into an ORDINARY image layer carrying that
//	asset_ref -> the shared serve-time gate accepts the stack.
//
// A test that stopped at "the PATCH returned 200" would pass with the projection
// missing entirely, which is precisely the half-built shape this repo keeps
// producing: the write path built and the read path never wired.
func TestSyncCarriesADeriveLayerAllTheWayToAnImageOnTheWire(t *testing.T) {
	ff, c, r, pr := newSyncEnv(t)

	rep, err := Sync(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if rep.Listed != 2 || len(rep.Rendered) != 2 || len(rep.Failed) != 0 || len(rep.RowErrs) != 0 {
		t.Fatalf("report = %d listed / %d rendered / %d failed / %d row errors; want 2/2/0/0",
			rep.Listed, len(rep.Rendered), len(rep.Failed), len(rep.RowErrs))
	}
	if pr.calls() != 2 {
		t.Errorf("the renderer ran %d times, want 2", pr.calls())
	}
	// ONE write for the cast, not one per layer: a second PATCH would carry the
	// first one's now-stale ETag and 409 against the tool's own write.
	if ff.patches != 1 {
		t.Errorf("the tool issued %d PATCHes for one cast, want 1", ff.patches)
	}
	if ff.lastIfMatch != `"rev-1"` {
		t.Errorf("If-Match = %q, want the ETag the read returned — an unconditional write would silently discard a concurrent human edit", ff.lastIfMatch)
	}

	// The stored cast now carries both members on both derive layers, and the
	// layers read CURRENT.
	for _, li := range []int{1, 2} {
		l := ff.cast.Slides[0].Layers[li]
		if l.AssetRef == "" || l.DerivedFrom == "" {
			t.Fatalf("layer %d was written back half-formed: asset_ref=%q derived_from=%q", li, l.AssetRef, l.DerivedFrom)
		}
		if got := wire.LayerDeriveState(l); got != wire.DeriveCurrent {
			t.Errorf("layer %d reads %s after a successful render — it will be re-rendered on every pass forever", li, got)
		}
		if _, ok := ff.content[l.AssetRef]; !ok {
			t.Errorf("layer %d names %s, which is not in the content origin", li, l.AssetRef)
		}
	}
	// Two different specs must not collapse onto one asset.
	if ff.cast.Slides[0].Layers[1].AssetRef == ff.cast.Slides[0].Layers[2].AssetRef {
		t.Error("two different derive layers were given the same asset_ref")
	}

	// The payoff: the projection turns the derive layers into plain image layers
	// and the SERVE-time gate — the one a relay applies before a screen sees
	// anything — accepts the stack.
	var served []wire.Layer
	for _, l := range ff.cast.Slides[0].Layers {
		p, ok := wire.DeriveProjection(l)
		if !ok {
			t.Fatalf("a rendered layer is still not projectable: %+v", l)
		}
		if p.Kind == wire.LayerKindDerive {
			t.Fatalf("a derive layer reached the wire unrewritten: %+v", p)
		}
		if wire.LayerFetchesContent(p.Kind) {
			p.URL = "https://origin.invalid/content/" + strings.TrimPrefix(p.AssetRef, "sha256:")
		}
		served = append(served, p)
	}
	if len(served) != 3 {
		t.Fatalf("the projected stack has %d layers, want 3", len(served))
	}
	if served[1].Kind != wire.LayerKindImage || served[2].Kind != wire.LayerKindImage {
		t.Errorf("the derive layers did not project to image: %s / %s", served[1].Kind, served[2].Kind)
	}
	// Geometry is preserved exactly — the PNG was rendered at this size.
	if served[1].X != 1400 || served[1].Y != 80 || served[1].W != 400 || served[1].H != 400 {
		t.Errorf("the projected layer moved or resized: %+v", served[1])
	}
	if err := wire.ValidateSlideLayers(served); err != nil {
		t.Fatalf("the serve-time gate rejected the projected stack: %v", err)
	}
}

// TestASecondSyncDoesNothing is the idempotence half, and it is what makes the
// tool safe to run from a cron or a post-authoring hook: a pass over a fully
// rendered workspace must upload nothing, write nothing, and cost nothing.
//
// Without the derived_from digest it would re-render and re-upload every layer
// on every pass — the same bytes, the same asset_ref, but a PATCH and a browser
// launch each time.
func TestASecondSyncDoesNothing(t *testing.T) {
	ff, c, r, pr := newSyncEnv(t)
	if _, err := Sync(context.Background(), c, r); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	firstRenders, firstPatches := pr.calls(), ff.patches

	rep, err := Sync(context.Background(), c, r)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if rep.Listed != 0 || len(rep.Rendered) != 0 {
		t.Errorf("the second pass found %d listed / %d rendered, want nothing outstanding", rep.Listed, len(rep.Rendered))
	}
	if pr.calls() != firstRenders {
		t.Errorf("the second pass rendered %d more layer(s)", pr.calls()-firstRenders)
	}
	if ff.patches != firstPatches {
		t.Errorf("the second pass wrote %d more time(s)", ff.patches-firstPatches)
	}
}

// TestEditingASpecMakesTheLayerStaleAndReRendersIt is the other direction, and
// it is the one a digest over the spec ALONE would get wrong: this test edits
// the GEOMETRY, not the spec.
//
// The raster is produced at exactly the layer's pixel size, so resizing a layer
// changes the picture as surely as changing its text does. If the digest did not
// cover w/h, a resized layer would read CURRENT and the panel would show a
// stretched PNG — no error, no queue entry, nothing to notice.
func TestEditingASpecMakesTheLayerStaleAndReRendersIt(t *testing.T) {
	ff, c, r, pr := newSyncEnv(t)
	if _, err := Sync(context.Background(), c, r); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	afterFirst := pr.calls()
	firstRef := ff.cast.Slides[0].Layers[1].AssetRef

	// An operator resizes the QR layer in the Studio. Nothing else changes.
	ff.cast.Slides[0].Layers[1].W = 520
	ff.cast.Slides[0].Layers[1].H = 520
	ff.etag = `"rev-2"`

	if got := wire.LayerDeriveState(ff.cast.Slides[0].Layers[1]); got != wire.DeriveStale {
		t.Fatalf("a resized layer reads %s, want stale — the digest does not cover geometry", got)
	}
	// While stale it STILL projects, from the old asset: an edit nobody has
	// rendered yet must never blank a screen.
	if p, ok := wire.DeriveProjection(ff.cast.Slides[0].Layers[1]); !ok || p.AssetRef != firstRef {
		t.Errorf("a stale layer stopped projecting — the screen would go blank until the tool caught up (ok=%v)", ok)
	}

	rep, err := Sync(context.Background(), c, r)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if rep.Listed != 1 || len(rep.Rendered) != 1 {
		t.Fatalf("the resize produced %d listed / %d rendered, want 1/1", rep.Listed, len(rep.Rendered))
	}
	if pr.calls() != afterFirst+1 {
		t.Errorf("the stale layer was not re-rendered")
	}
	if ff.lastIfMatch != `"rev-2"` {
		t.Errorf("If-Match = %q, want the CURRENT ETag", ff.lastIfMatch)
	}
	if got := wire.LayerDeriveState(ff.cast.Slides[0].Layers[1]); got != wire.DeriveCurrent {
		t.Errorf("the re-rendered layer reads %s", got)
	}
}

// TestAConcurrentEditLosesTheWriteBackRatherThanClobberingIt: a 409 on the
// write-back is the precondition doing its job. The run reports it and the next
// pass renders the new spec — which is strictly better than overwriting an
// operator's edit with a pre-render snapshot they would never see fail.
func TestAConcurrentEditLosesTheWriteBackRatherThanClobberingIt(t *testing.T) {
	ff, c, r, _ := newSyncEnv(t)
	ff.patchStatus = http.StatusConflict

	rep, err := Sync(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Sync returned a fatal error for a per-cast conflict: %v", err)
	}
	if len(rep.RowErrs) != 1 {
		t.Fatalf("a 409 write-back produced %d reported error(s), want 1", len(rep.RowErrs))
	}
	// The layers stay outstanding, so the next pass picks them up.
	for _, li := range []int{1, 2} {
		if ff.cast.Slides[0].Layers[li].AssetRef != "" {
			t.Errorf("layer %d was mutated despite the conflict", li)
		}
	}
}

// TestADeriveLayerInsideAnInlinePlaylistSlideIsRenderedAndWrittenBack is the
// second authored shape, end to end.
//
// A `source: "slide"` playlist item carries its layer stack INLINE, with no cast
// and no slide id. That shape is accepted by the surface, projected by both
// content projections (resolveLayers is shared with the cast expansion) and held
// against the retention sweep — and it was invisible to this loop, so the layer
// was queued nowhere and drawn never. A tool that only knows casts passes every
// other test in this file.
func TestADeriveLayerInsideAnInlinePlaylistSlideIsRenderedAndWrittenBack(t *testing.T) {
	ff, c, r, pr := newSyncEnv(t)
	ff.playlist = derivePlaylistFixture()

	rep, err := Sync(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Two cast layers plus the one inside the playlist's second item.
	if rep.Listed != 3 || len(rep.Rendered) != 3 || len(rep.Failed) != 0 || len(rep.RowErrs) != 0 {
		t.Fatalf("report = %d listed / %d rendered / %d failed / %d row errors; want 3/3/0/0 — an inline slide's derive layer is work this loop owes",
			rep.Listed, len(rep.Rendered), len(rep.Failed), len(rep.RowErrs))
	}
	if pr.calls() != 3 {
		t.Errorf("the renderer ran %d times, want 3", pr.calls())
	}
	// BOTH rows were read — the number the summary line reports, and the one that
	// tells "nothing outstanding" apart from "every read failed".
	if rep.RowsRead != 2 {
		t.Errorf("the pass read %d row(s), want 2 (the cast and the playlist)", rep.RowsRead)
	}
	if ff.listPatches != 1 {
		t.Fatalf("the tool issued %d PATCHes for the playlist, want 1", ff.listPatches)
	}
	if ff.lastListIfMa != `"list-1"` {
		t.Errorf("the playlist write-back's If-Match = %q, want the ETag the read returned", ff.lastListIfMa)
	}

	// It landed in item 1's inline slide — the index the queue reported — and not
	// in item 0, which carries no slide at all.
	inline := ff.playlist.Items[1].Slide.Layers[1]
	if inline.AssetRef == "" || inline.DerivedFrom == "" {
		t.Fatalf("the inline layer was written back half-formed: %+v", inline)
	}
	if got := wire.LayerDeriveState(inline); got != wire.DeriveCurrent {
		t.Errorf("the inline layer reads %s after a successful render", got)
	}
	if _, ok := ff.content[inline.AssetRef]; !ok {
		t.Errorf("the inline layer names %s, which is not in the content origin", inline.AssetRef)
	}
	if ff.playlist.Items[0].AssetRef != "sha256:"+strings.Repeat("c", 64) {
		t.Errorf("the write-back disturbed the item it did not render: %+v", ff.playlist.Items[0])
	}
	// And the report says WHERE, in the queue's own grammar, so an operator can
	// find the layer that was drawn.
	var found bool
	for _, o := range rep.Rendered {
		if o.Source == SourcePlaylist {
			found = true
			if o.Where != "item 1" || o.LayerIndex != 1 || o.ResourceID != ff.playlist.ID {
				t.Errorf("the playlist outcome does not locate the layer: %+v", o)
			}
		}
	}
	if !found {
		t.Error("no outcome was reported for the playlist at all")
	}

	// The payoff, same as the cast's: it projects to a plain image layer the
	// serve-time gate accepts.
	projected, ok := wire.DeriveProjection(inline)
	if !ok || projected.Kind != wire.LayerKindImage {
		t.Fatalf("the rendered inline layer does not project to an image (ok=%v, kind=%s)", ok, projected.Kind)
	}
}

// TestASecondSyncLeavesTheInlineSlideAlone is the idempotence half for the
// second shape: the playlist write-back must clear its own queue entries, or the
// tool re-renders and re-writes that item on every pass forever.
func TestASecondSyncLeavesTheInlineSlideAlone(t *testing.T) {
	ff, c, r, _ := newSyncEnv(t)
	ff.playlist = derivePlaylistFixture()
	if _, err := Sync(context.Background(), c, r); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	patches := ff.listPatches

	rep, err := Sync(context.Background(), c, r)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if rep.Listed != 0 {
		t.Errorf("the second pass still lists %d job(s)", rep.Listed)
	}
	if ff.listPatches != patches {
		t.Errorf("the second pass wrote the playlist %d more time(s)", ff.listPatches-patches)
	}
}

// TestRendersRunConcurrentlyUpToTheClamp is what makes -concurrency a control
// rather than a banner.
//
// The clamp test in runner_test.go proves the SEMAPHORE holds when several
// callers push against it; it says nothing about whether this loop ever produces
// several callers. It did not: Sync rendered strictly sequentially, so four jobs
// at -concurrency 4 took four times one render and the flag changed nothing an
// operator could measure.
//
// The renderer here blocks until `want` renders are in flight at once, with the
// context deadline as the only way out — so a serial loop does not merely report
// a smaller number, it never reaches the barrier and the test fails on the wait.
func TestRendersRunConcurrentlyUpToTheClamp(t *testing.T) {
	const want = 4
	ff, c, _, _ := newSyncEnv(t)
	ff.cast = manyLayerCastFixture(want)

	bar := &barrierRenderer{want: want, release: make(chan struct{})}
	r := NewRunner(bar, RunnerOptions{Concurrency: want, JobTimeout: 10 * time.Second})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rep, err := Sync(ctx, c, r)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(rep.Rendered) != want {
		t.Fatalf("report = %d rendered, want %d (failed: %+v)", len(rep.Rendered), want, rep.Failed)
	}
	if got := bar.max(); got != want {
		t.Errorf("at most %d render(s) ever ran at once with -concurrency %d — the flag is bound, printed and inert", got, want)
	}

	// …and the clamp still BINDS in the other direction: the same four jobs
	// through a Runner of two must never put three browsers on the host at once.
	ff2, c2, _, _ := newSyncEnv(t)
	ff2.cast = manyLayerCastFixture(want)
	bar2 := &barrierRenderer{want: 2, release: make(chan struct{})}
	r2 := NewRunner(bar2, RunnerOptions{Concurrency: 2, JobTimeout: 10 * time.Second})
	if _, err := Sync(ctx, c2, r2); err != nil {
		t.Fatalf("clamped Sync: %v", err)
	}
	if got := bar2.max(); got > 2 {
		t.Errorf("%d renders ran at once with the clamp set to 2", got)
	}
}

// manyLayerCastFixture is one slide carrying n distinct pending QR layers.
func manyLayerCastFixture(n int) datamodel.Cast {
	c := datamodel.Cast{ID: "01J8CAST0000000000000000AA", Name: "Lobby",
		Slides: []datamodel.CastSlide{{ID: "s1"}}}
	for i := range n {
		c.Slides[0].Layers = append(c.Slides[0].Layers, wire.Layer{
			Kind: wire.LayerKindDerive, X: 0, Y: 0, W: 200, H: 200,
			Derive: &wire.DeriveSpec{Kind: wire.DeriveKindQR, Data: fmt.Sprintf("https://waiveo.local/%d", i)},
		})
	}
	return c
}

// barrierRenderer holds every render until `want` of them are in flight at once,
// then releases them all. It records the high-water mark.
type barrierRenderer struct {
	want    int
	mu      sync.Mutex
	inFlt   int
	maxSeen int
	release chan struct{}
	once    sync.Once
}

func (b *barrierRenderer) Render(ctx context.Context, page Page) ([]byte, error) {
	b.mu.Lock()
	b.inFlt++
	if b.inFlt > b.maxSeen {
		b.maxSeen = b.inFlt
	}
	reached := b.inFlt >= b.want
	b.mu.Unlock()
	if reached {
		b.once.Do(func() { close(b.release) })
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		// A serial loop never reaches the barrier; the per-job deadline is what
		// ends the wait, and the run then reports failures instead of renders.
	}
	b.mu.Lock()
	b.inFlt--
	b.mu.Unlock()
	img := image.NewNRGBA(image.Rect(0, 0, page.ClipW, page.ClipH))
	sum := sha256.Sum256([]byte(page.HTML))
	img.SetNRGBA(0, 0, color.NRGBA{R: sum[0], G: sum[1], B: sum[2], A: 255})
	return mustPNG(img), nil
}

func (b *barrierRenderer) max() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxSeen
}

// TestAWriteBackWithNoETagIsRefusedRatherThanSentUnconditionally: the
// precondition is the only thing standing between a slow render and a silently
// discarded human edit, so a read that produced no ETag must stop the write, not
// downgrade it. An unconditional PATCH here would overwrite whatever the row
// had become while the browser was working.
func TestAWriteBackWithNoETagIsRefusedRatherThanSentUnconditionally(t *testing.T) {
	ff, c, r, _ := newSyncEnv(t)
	ff.omitETag = true

	rep, err := Sync(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(rep.RowErrs) != 1 {
		t.Fatalf("an ETag-less read produced %d reported write-back error(s), want 1", len(rep.RowErrs))
	}
	if ff.patches != 0 {
		t.Errorf("the tool sent %d unconditional PATCH(es) after a read with no ETag", ff.patches)
	}
}

// TestOneBadLayerDoesNotStopTheOthers: a per-layer failure is reported and the
// run continues. The alternative — abort the pass — means one permanently broken
// layer keeps every other layer in the workspace unrendered.
func TestOneBadLayerDoesNotStopTheOthers(t *testing.T) {
	ff, c, _, _ := newSyncEnv(t)
	r := NewRunner(&selectiveRenderer{failKindQR: true}, RunnerOptions{Concurrency: 1, JobTimeout: time.Second})

	rep, err := Sync(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(rep.Rendered) != 1 || len(rep.Failed) != 1 {
		t.Fatalf("report = %d rendered / %d failed, want 1/1", len(rep.Rendered), len(rep.Failed))
	}
	// The good layer was still written back.
	if ff.cast.Slides[0].Layers[2].AssetRef == "" {
		t.Error("the layer that rendered was not written back")
	}
	if ff.cast.Slides[0].Layers[1].AssetRef != "" {
		t.Error("the layer that failed was written back anyway")
	}
}

// selectiveRenderer fails QR pages and renders everything else.
type selectiveRenderer struct{ failKindQR bool }

func (s *selectiveRenderer) Render(_ context.Context, page Page) ([]byte, error) {
	if s.failKindQR && strings.Contains(page.HTML, "<svg") {
		return nil, errors.New("simulated qr render failure")
	}
	img := image.NewNRGBA(image.Rect(0, 0, page.ClipW, page.ClipH))
	img.SetNRGBA(0, 0, color.NRGBA{R: 9, A: 255})
	return mustPNG(img), nil
}

// TestApplyRenderedRefusesHalfAPair pins the invariant the whole staleness model
// rests on, from the inside. Both directions are wrong in different ways: an
// asset_ref with no derived_from re-renders forever, and a derived_from with no
// asset_ref is refused outright by the layer gate — so a caller must never be
// able to write one.
func TestApplyRenderedRefusesHalfAPair(t *testing.T) {
	layers := deriveCastFixture().Slides[0].Layers
	if err := ApplyRendered(layers, 1, "sha256:"+strings.Repeat("a", 64), ""); err == nil {
		t.Error("a reference with no digest was accepted")
	}
	if err := ApplyRendered(layers, 1, "", "digest"); err == nil {
		t.Error("a digest with no reference was accepted")
	}
	if err := ApplyRendered(layers, 99, "ref", "digest"); err == nil {
		t.Error("an out-of-range layer index was accepted")
	}
	if err := ApplyRendered(layers, -1, "ref", "digest"); err == nil {
		t.Error("a negative layer index was accepted")
	}
	if err := ApplyRendered(layers, 1, "sha256:x", "digest"); err != nil {
		t.Errorf("a complete pair was refused: %v", err)
	}
	if layers[1].AssetRef != "sha256:x" || layers[1].DerivedFrom != "digest" {
		t.Errorf("the pair was not written: %+v", layers[1])
	}

	// The SAME function serves the playlist shape — a cast slide's stack and an
	// inline slide's stack are one type, so there is no second write-back path
	// that could forget the pair.
	inline := derivePlaylistFixture().Items[1].Slide.Layers
	if err := ApplyRendered(inline, 1, "sha256:y", "digest"); err != nil {
		t.Errorf("an inline slide's layer was refused: %v", err)
	}
	if inline[1].AssetRef != "sha256:y" || inline[1].DerivedFrom != "digest" {
		t.Errorf("the pair was not written into the inline slide: %+v", inline[1])
	}
}

// ── The pass survives one bad row ───────────────────────────────────────────

// TestASpeclessLayerFailsItsOwnJobAndNothingElse is the regression test for the
// defect that took down whole passes.
//
// A `derive` layer with NO SPEC was authorable — the inline playlist path ran no
// layer validation at all — so it reached the queue, and the queue reached
// renderOne, which dereferenced the nil. Because a pass renders EVERYTHING
// before it uploads, applies and writes anything back, the process died holding
// every other layer's finished PNG: one malformed row in one playlist discarded
// the completed work of every other row in the workspace.
//
// Nothing upstream is trusted to make the shape unreachable. A cast slide's
// stack is gated at authoring time; an inline `source: "slide"` item's is not on
// this branch, and either way a row can arrive from a restore, a seed, or a
// build older than whatever gate is current. Both halves are asserted: the bad
// layer FAILS with a reason, and every other layer in the same pass is rendered,
// uploaded and written back.
func TestASpeclessLayerFailsItsOwnJobAndNothingElse(t *testing.T) {
	ff, c, r, pr := newSyncEnv(t)
	// Slide 1 keeps the two good derive layers; a second slide carries the
	// malformed one, so the failure is in a different stack from the successes.
	ff.cast.Slides = append(ff.cast.Slides, datamodel.CastSlide{
		ID: "s2",
		Layers: []wire.Layer{
			{Kind: wire.LayerKindDerive, X: 0, Y: 0, W: 400, H: 400},
		},
	})

	rep, err := Sync(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(rep.Rendered) != 2 {
		t.Errorf("rendered = %d, want the 2 good layers (failed: %+v)", len(rep.Rendered), rep.Failed)
	}
	if len(rep.Failed) != 1 {
		t.Fatalf("failed = %d, want exactly the spec-less layer: %+v", len(rep.Failed), rep.Failed)
	}
	if got := rep.Failed[0].Err; got == nil || !strings.Contains(got.Error(), "no spec") {
		t.Errorf("the spec-less layer failed with %v, want a reason naming the missing spec", got)
	}
	if pr.calls() != 2 {
		t.Errorf("the renderer was called %d time(s), want 2 — the spec-less layer must never reach it", pr.calls())
	}
	// The good layers really were written back: the whole point is that the bad
	// row costs its own layer and NOTHING else.
	for _, li := range []int{1, 2} {
		if ff.cast.Slides[0].Layers[li].AssetRef == "" {
			t.Errorf("slide 1 layer %d was not written back; a malformed row in another slide discarded it", li)
		}
	}
	if ff.cast.Slides[1].Layers[0].AssetRef != "" {
		t.Error("the spec-less layer was written back anyway")
	}
}

// TestSyncSurvivesEveryMalformedShapeAStoreCanHold is the STANDALONE-SAFETY
// assertion for this branch, and it is the reason the branch does not carry an
// inline authoring gate of its own.
//
// The four shapes below are exactly the ones `POST /playlists` accepts with a
// 201 today — a zero-layer inline slide, a `derive` layer with no spec, an
// unknown layer kind, geometry off the canvas — and the same four a workspace
// restore or a seed bundle can deliver whatever any gate does next. An inline
// gate is coming from the interactive-layers track together with the prior-fault
// diff that makes adding one safe; duplicating it here would have bought nothing
// this test does not already prove, and (measured) would have bricked CREATE,
// UPDATE and DELETE for every playlist in a store holding one such row.
//
// The stand-in feeder deliberately does NOT omit the undrawable job the way the
// real queue does, so what is exercised here is the renderer's own defences with
// nothing upstream helping: renderOne's refusal and renderGuarded's recover.
//
// The assertion is all three of: the pass RETURNS (no crash), it REPORTS the
// malformed layer with a reason, and it still renders and writes back the good
// work sitting in the same rows.
func TestSyncSurvivesEveryMalformedShapeAStoreCanHold(t *testing.T) {
	ff, c, r, pr := newSyncEnv(t)

	// Every shape a store can hold, in ONE workspace, alongside real work.
	ff.playlist = datamodel.Playlist{
		ID: "01J8LIST0000000000000000BB", Name: "Foyer loop",
		Items: []datamodel.PlaylistItem{
			// 1. A zero-layer inline slide.
			{Source: "slide", Slide: &datamodel.Slide{Layers: []wire.Layer{}}},
			// 2. A `derive` layer with no spec — the shape that killed the pass.
			{Source: "slide", Slide: &datamodel.Slide{Layers: []wire.Layer{
				{Kind: wire.LayerKindDerive, X: 0, Y: 0, W: 400, H: 400},
			}}},
			// 3. An unknown layer kind, sharing a stack with genuine outstanding
			//    work so a tool that gave up on the ROW would be caught.
			{Source: "slide", Slide: &datamodel.Slide{Layers: []wire.Layer{
				{Kind: "hologram", X: 0, Y: 0, W: 100, H: 100},
				{Kind: wire.LayerKindDerive, X: 200, Y: 200, W: 360, H: 360, Derive: &wire.DeriveSpec{
					Kind: wire.DeriveKindQR, Data: "https://waiveo.local/pair/SURVIVOR", ECLevel: "Q",
				}},
			}}},
			// 4. Geometry off the canvas.
			{Source: "slide", Slide: &datamodel.Slide{Layers: []wire.Layer{
				{Kind: wire.LayerKindRect, X: 1900, Y: 0, W: 100, H: 100, Color: "#ffffff"},
			}}},
		},
	}

	rep, err := Sync(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Sync returned an error rather than a report: %v", err)
	}

	// The two cast layers plus the one good inline layer: real work, finished,
	// while four malformed shapes sat in the same pass.
	if len(rep.Rendered) != 3 {
		t.Errorf("rendered = %d, want 3 (2 cast + 1 inline); failed = %+v", len(rep.Rendered), rep.Failed)
	}
	if pr.calls() != 3 {
		t.Errorf("the renderer ran %d time(s), want 3 — no malformed layer may reach it", pr.calls())
	}
	if ff.playlist.Items[2].Slide.Layers[1].AssetRef == "" {
		t.Error("the good inline layer sharing a stack with an unknown kind was not written back")
	}

	// The spec-less layer is REPORTED, not silently dropped: an operator asked
	// for something and has to learn the row is malformed.
	if len(rep.Failed) != 1 {
		t.Fatalf("failed = %d, want exactly the spec-less layer: %+v", len(rep.Failed), rep.Failed)
	}
	if got := rep.Failed[0].Err; got == nil || !strings.Contains(got.Error(), "no spec") {
		t.Errorf("the spec-less layer failed with %v, want a reason naming the missing spec", got)
	}
	if len(rep.RowErrs) != 0 {
		t.Errorf("row errors = %+v, want none — a malformed LAYER must never fail its whole row", rep.RowErrs)
	}
}

// TestAPanickingRenderCostsOneLayer is the CLASS the guard above is one instance
// of. A renderer is a browser driver over third-party bytes; a panic anywhere
// under it — the page builder, an image encoder, the driver itself — must cost
// the unit that provoked it and nothing else, because the pass holds every other
// unit's finished work in memory until the serial phase runs.
func TestAPanickingRenderCostsOneLayer(t *testing.T) {
	ff, c, _, _ := newSyncEnv(t)
	r := NewRunner(&panicRenderer{on: "<svg"}, RunnerOptions{Concurrency: 2, JobTimeout: 10 * time.Second})

	rep, err := Sync(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Sync returned an error rather than a report: %v", err)
	}
	if len(rep.Rendered) != 1 || len(rep.Failed) != 1 {
		t.Fatalf("report = %d rendered / %d failed, want 1/1: %+v %+v", len(rep.Rendered), len(rep.Failed), rep.Rendered, rep.Failed)
	}
	if got := rep.Failed[0].Err; got == nil || !strings.Contains(got.Error(), "panicked") {
		t.Errorf("the panicking layer failed with %v, want a reason naming the panic", got)
	}
	if ff.cast.Slides[0].Layers[2].AssetRef == "" {
		t.Error("the layer that rendered was not written back — a panic in one unit discarded a finished PNG in another")
	}
}

// TestAPanickingRenderChargesTheCircuitBreaker is the OTHER half of the guard
// above, and it is the half the round that wrote the recover() did not build.
//
// Surviving a panic is not the whole failure path. Runner.Render charges
// recordFailure on each of its error returns and a panic unwinds straight past
// every one of them, so a recovered panic used to be reported and then
// forgotten: measured as three consecutive attempts on one key with circuitOpen
// false every time and the browser relaunched three times. A deterministically
// panicking layer — a malformed row, a spec the page builder cannot handle — is
// precisely the input the breaker exists for, and it got a full-rate relaunch
// every pass, forever.
//
// Driven through renderGuarded directly rather than through Sync because the
// property is about the SECOND attempt on one key, and Sync renders each distinct
// digest exactly once per pass.
func TestAPanickingRenderChargesTheCircuitBreaker(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	pr := &countingPanicRenderer{}
	r := NewRunner(pr, RunnerOptions{Concurrency: 1, JobTimeout: time.Minute, Now: clk.now})

	layer := wire.Layer{Kind: wire.LayerKindDerive, W: 400, H: 400, Derive: &wire.DeriveSpec{
		Kind: wire.DeriveKindQR, Data: "https://waiveo.local/pair/BOOM",
	}}
	digest := wire.DeriveDigest(layer)

	if _, err := renderGuarded(context.Background(), r, layer, digest, nil); err == nil {
		t.Fatal("a panicking render returned no error")
	}
	if _, open := r.circuitOpen(digest); !open {
		t.Fatal("a recovered panic left the circuit CLOSED: the layer will be retried at full rate on " +
			"every pass, relaunching the browser each time, until the row itself changes — the survive " +
			"half of the guard without the backoff half")
	}

	// Two more passes over the same key: the open breaker must now refuse them
	// before any render capacity — and therefore any browser — is taken.
	for i := 2; i <= 3; i++ {
		_, err := renderGuarded(context.Background(), r, layer, digest, nil)
		if !errors.Is(err, ErrCircuitOpen) {
			t.Errorf("attempt %d returned %v, want ErrCircuitOpen", i, err)
		}
	}
	if got := pr.calls.Load(); got != 1 {
		t.Errorf("the renderer was entered %d time(s) across three attempts, want 1", got)
	}
}

// TestAPanicReportsItsStack: the panic VALUE alone is one line with no frames,
// and the recover is what stopped the runtime from printing the trace, so if the
// guard does not carry it nothing does. A genuine bug then surfaces as a shrug.
func TestAPanicReportsItsStack(t *testing.T) {
	r := NewRunner(&countingPanicRenderer{}, RunnerOptions{Concurrency: 1, JobTimeout: time.Minute})
	layer := wire.Layer{Kind: wire.LayerKindDerive, W: 400, H: 400, Derive: &wire.DeriveSpec{
		Kind: wire.DeriveKindQR, Data: "https://waiveo.local/pair/TRACE",
	}}

	_, err := renderGuarded(context.Background(), r, layer, wire.DeriveDigest(layer), nil)
	if err == nil {
		t.Fatal("a panicking render returned no error")
	}
	// The frame the panic came FROM, named. Asserting on "goroutine" alone would
	// pass for a trace that begins after the panicking frames were unwound.
	if !strings.Contains(err.Error(), "countingPanicRenderer") {
		t.Errorf("the panic error names no frame from the panicking call:\n%v", err)
	}
}

// countingPanicRenderer panics on EVERY page and counts how often it was entered
// — which is how "the breaker refused the attempt" is told apart from "the
// attempt ran and failed again".
type countingPanicRenderer struct{ calls atomic.Int32 }

func (p *countingPanicRenderer) Render(_ context.Context, _ Page) ([]byte, error) {
	p.calls.Add(1)
	panic("simulated renderer panic")
}

// panicRenderer panics on any page whose HTML contains `on`, and renders
// everything else.
type panicRenderer struct{ on string }

func (p *panicRenderer) Render(_ context.Context, page Page) ([]byte, error) {
	if strings.Contains(page.HTML, p.on) {
		panic("simulated renderer panic")
	}
	img := image.NewNRGBA(image.Rect(0, 0, page.ClipW, page.ClipH))
	img.SetNRGBA(0, 0, color.NRGBA{G: 200, A: 255})
	return mustPNG(img), nil
}

// ── One picture, one render ─────────────────────────────────────────────────

// TestDuplicateLayersRenderOnceAndReportTheSame pins the determinism this loop
// claims, on the input that broke it.
//
// wire.DeriveDigest is the identity of a layer's PIXELS, and it is ALSO
// Runner.breaker's key. Four layers sharing one digest were four units: with a
// failing renderer at concurrency 2 they each charged the breaker, so a failing
// layer backed off twice as fast as the published schedule, and which error each
// unit reported depended on which browser finished first — 40 runs of that
// arrangement produced three different reports.
//
// Rendering each distinct digest once and fanning the answer out makes the
// report a function of the input alone. Both halves are asserted, because the
// count is what makes the determinism structural rather than lucky.
func TestDuplicateLayersRenderOnceAndReportTheSame(t *testing.T) {
	const duplicates = 4
	var reports []string
	for run := range 20 {
		ff, c, _, _ := newSyncEnv(t)
		ff.cast = duplicateLayerCastFixture(duplicates)
		fr := &failingRenderer{}
		r := NewRunner(fr, RunnerOptions{Concurrency: 2, JobTimeout: 10 * time.Second})

		rep, err := Sync(context.Background(), c, r)
		if err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if len(rep.Failed) != duplicates {
			t.Fatalf("run %d: failed = %d, want one outcome per authored layer", run, len(rep.Failed))
		}
		if got := fr.calls(); got != 1 {
			t.Fatalf("run %d: the renderer ran %d time(s) for %d layers that share one digest — "+
				"one picture is one render, and the digest is also the circuit breaker's key, "+
				"so N attempts advance the backoff N times per pass", run, got, duplicates)
		}
		var sb strings.Builder
		for _, o := range rep.Failed {
			fmt.Fprintf(&sb, "%s/%s/%d: %v\n", o.ResourceID, o.Where, o.LayerIndex, o.Err)
		}
		reports = append(reports, sb.String())
	}
	for i, rep := range reports {
		if rep != reports[0] {
			t.Fatalf("run %d reported\n%s\nrun 0 reported\n%s\nthe same workspace must produce the same report", i, rep, reports[0])
		}
	}
}

// duplicateLayerCastFixture is one slide carrying n layers with the IDENTICAL
// spec and geometry — the same picture placed n times, which is an ordinary
// thing to author (a repeated badge, a QR on every panel) and which gives every
// one of them the same DeriveDigest. They sit at different x/y on purpose:
// position is deliberately NOT in the digest, so moving a copy must not make it
// a second picture.
func duplicateLayerCastFixture(n int) datamodel.Cast {
	c := datamodel.Cast{ID: "01J8CAST0000000000000000AA", Name: "Lobby",
		Slides: []datamodel.CastSlide{{ID: "s1"}}}
	for i := range n {
		c.Slides[0].Layers = append(c.Slides[0].Layers, wire.Layer{
			Kind: wire.LayerKindDerive, X: i * 220, Y: 0, W: 200, H: 200,
			Derive: &wire.DeriveSpec{Kind: wire.DeriveKindQR, Data: "https://waiveo.local/same"},
		})
	}
	return c
}

// failingRenderer fails every page and counts its attempts.
type failingRenderer struct {
	mu sync.Mutex
	n  int
}

func (f *failingRenderer) Render(_ context.Context, _ Page) ([]byte, error) {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	return nil, errors.New("simulated render failure")
}

func (f *failingRenderer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// TestOnePoolSpansTheWholePassNotOneRow is the case the concurrency argument is
// actually about, and it had no test.
//
// TestRendersRunConcurrentlyUpToTheClamp uses four layers in ONE slide of ONE
// row, so it cannot distinguish "one pool for the pass" from "one pool per row":
// both give that fixture a pool of four. The distinguishing workspace is the one
// the design comment names — rows carrying a SINGLE layer each — where a
// per-row pool is a pool of one and the whole pass runs serially no matter what
// -concurrency says.
//
// Two rows of one pending layer each, a barrier that only opens when two renders
// are in flight, and a clamp of two: with one pass-wide pool the barrier opens;
// with a pool per row neither render ever sees the other and both die on the
// per-job deadline.
func TestOnePoolSpansTheWholePassNotOneRow(t *testing.T) {
	ff, c, _, _ := newSyncEnv(t)
	ff.cast = manyLayerCastFixture(1)
	ff.playlist = derivePlaylistFixture()

	bar := &barrierRenderer{want: 2, release: make(chan struct{})}
	r := NewRunner(bar, RunnerOptions{Concurrency: 2, JobTimeout: 3 * time.Second})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rep, err := Sync(ctx, c, r)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(rep.Rendered) != 2 {
		t.Fatalf("rendered = %d, want the one layer in each of the two rows (failed: %+v)", len(rep.Rendered), rep.Failed)
	}
	if got := bar.max(); got != 2 {
		t.Errorf("at most %d render(s) ran at once across two single-layer rows — "+
			"a pool per row leaves a workspace of single-layer casts serial however -concurrency is set", got)
	}
}
