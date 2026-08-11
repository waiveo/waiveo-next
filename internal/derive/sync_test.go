package derive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
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
// one cast and one content origin, so the test can assert on what the tool
// actually WROTE rather than on what it claims to have done.
type fakeFeeder struct {
	cast    datamodel.Cast
	etag    string
	content map[string][]byte

	patches     int
	lastIfMatch string
	// patchStatus, when non-zero, is returned instead of applying the patch —
	// used to simulate the 409 a concurrent human edit produces.
	patchStatus int
}

func (f *fakeFeeder) pending() []PendingJob {
	var out []PendingJob
	for _, s := range f.cast.Slides {
		for i, l := range s.Layers {
			st := wire.LayerDeriveState(l)
			if st == wire.DeriveCurrent {
				continue
			}
			out = append(out, PendingJob{
				CastID: f.cast.ID, CastName: f.cast.Name, SlideID: s.ID, LayerIndex: i,
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
		f.content[ref] = buf.Bytes()
		writeJSON(w, http.StatusCreated, map[string]string{"asset_ref": ref})

	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/casts/"+f.cast.ID:
		w.Header().Set("ETag", f.etag)
		writeJSON(w, http.StatusOK, f.cast)

	case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/casts/"+f.cast.ID:
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
// are the bytes a real renderer would produce, and each spec gets its own
// picture so two layers cannot accidentally share a digest.
type pngRenderer struct{ n int }

func (p *pngRenderer) Render(_ context.Context, page Page) ([]byte, error) {
	p.n++
	img := image.NewNRGBA(image.Rect(0, 0, page.ClipW, page.ClipH))
	img.SetNRGBA(0, 0, color.NRGBA{R: uint8(p.n), G: 40, B: 60, A: 255})
	return mustPNG(img), nil
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

func newSyncEnv(t *testing.T) (*fakeFeeder, *Client, *Runner, *pngRenderer) {
	t.Helper()
	ff := &fakeFeeder{cast: deriveCastFixture(), etag: `"rev-1"`, content: map[string][]byte{}}
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
	if rep.Listed != 2 || len(rep.Rendered) != 2 || len(rep.Failed) != 0 || len(rep.PatchErrs) != 0 {
		t.Fatalf("report = %d listed / %d rendered / %d failed / %d write-back errors; want 2/2/0/0",
			rep.Listed, len(rep.Rendered), len(rep.Failed), len(rep.PatchErrs))
	}
	if pr.n != 2 {
		t.Errorf("the renderer ran %d times, want 2", pr.n)
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
	firstRenders, firstPatches := pr.n, ff.patches

	rep, err := Sync(context.Background(), c, r)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if rep.Listed != 0 || len(rep.Rendered) != 0 {
		t.Errorf("the second pass found %d listed / %d rendered, want nothing outstanding", rep.Listed, len(rep.Rendered))
	}
	if pr.n != firstRenders {
		t.Errorf("the second pass rendered %d more layer(s)", pr.n-firstRenders)
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
	afterFirst := pr.n
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
	if pr.n != afterFirst+1 {
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
	if len(rep.PatchErrs) != 1 {
		t.Fatalf("a 409 write-back produced %d reported error(s), want 1", len(rep.PatchErrs))
	}
	// The layers stay outstanding, so the next pass picks them up.
	for _, li := range []int{1, 2} {
		if ff.cast.Slides[0].Layers[li].AssetRef != "" {
			t.Errorf("layer %d was mutated despite the conflict", li)
		}
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
	slides := deriveCastFixture().Slides
	if err := ApplyRendered(slides, 0, 1, "sha256:"+strings.Repeat("a", 64), ""); err == nil {
		t.Error("a reference with no digest was accepted")
	}
	if err := ApplyRendered(slides, 0, 1, "", "digest"); err == nil {
		t.Error("a digest with no reference was accepted")
	}
	if err := ApplyRendered(slides, 0, 99, "ref", "digest"); err == nil {
		t.Error("an out-of-range layer index was accepted")
	}
	if err := ApplyRendered(slides, 9, 0, "ref", "digest"); err == nil {
		t.Error("an out-of-range slide index was accepted")
	}
	if err := ApplyRendered(slides, 0, 1, "sha256:x", "digest"); err != nil {
		t.Errorf("a complete pair was refused: %v", err)
	}
	if slides[0].Layers[1].AssetRef != "sha256:x" || slides[0].Layers[1].DerivedFrom != "digest" {
		t.Errorf("the pair was not written: %+v", slides[0].Layers[1])
	}
}
