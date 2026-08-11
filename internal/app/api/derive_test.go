package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apiv1 "github.com/maaxton/waiveo-next/api/gen/go"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// derive_test.go drives the AUTHORING surface for the rasterized fallback
// (parity row 2.4) over the real mounted mux, and it exists for the reason
// video_test.go does: api/openapi.yaml declares these bodies and the api layer
// enforces the declaration with `additionalProperties: false` and per-field
// enums, so a layer the wire supports and the document does not is a capability
// that exists everywhere except where an operator can reach it. That is not
// hypothetical here — it shipped in wave 1, when the four widget kinds landed on
// the wire and the player and POST /casts answered 422 for every one of them.

func derivePendingList(t *testing.T, e *testEnv) []map[string]any {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, "/api/v1/derive/pending", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /derive/pending: %d %s", resp.StatusCode, raw)
	}
	var out struct {
		Jobs []map[string]any `json:"derive_jobs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode the pending listing: %v (body %s)", err, raw)
	}
	return out.Jobs
}

// TestACastSlideMayCarryADeriveLayer is the authoring half: POST /casts with a
// QR derive layer is accepted and reads back with the whole nested spec intact.
//
// It is the case that fails if the SlideLayer schema is not extended alongside
// the wire — and unlike the projection tests, nothing else in the suite would
// notice, because the projections never pass through the declared-schema gate.
func TestACastSlideMayCarryADeriveLayer(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	cast := datamodel.Cast{
		ScopeNode: screenID,
		Name:      "Pairing Poster",
		Slides: []datamodel.CastSlide{{
			ID: "poster", DurationMS: 10000,
			Layers: []wire.Layer{
				{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#101828"},
				{Kind: wire.LayerKindDerive, X: 1400, Y: 80, W: 400, H: 400, Derive: &wire.DeriveSpec{
					Kind: wire.DeriveKindQR, Data: "https://waiveo.local/pair/ABCD-1234", ECLevel: "H",
					Color: "#111827",
					Fill:  &wire.DeriveFill{Kind: wire.DeriveFillSolid, From: "#FFFFFF"},
				}},
				{Kind: wire.LayerKindDerive, X: 120, Y: 700, W: 900, H: 280, Derive: &wire.DeriveSpec{
					Kind: wire.DeriveKindText, Text: "SCAN TO PAIR", FontPx: 110, Color: "#FFFFFF",
					Align: "center", VAlign: "middle",
					Fill:   &wire.DeriveFill{Kind: wire.DeriveFillLinear, From: "#7C3AED", To: "#0EA5E9", AngleDeg: 135},
					Shadow: &wire.DeriveShadow{DY: 10, Blur: 26, Color: "#000000", OpacityPct: 55},
					Border: &wire.DeriveBorder{Radius: 28},
				}},
			},
		}},
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, cast), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST a cast with derive layers: status %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	stored := decodeCast(t, raw)
	if len(stored.Slides) != 1 || len(stored.Slides[0].Layers) != 3 {
		t.Fatalf("stored cast shape unexpected: %+v", stored.Slides)
	}

	qr := stored.Slides[0].Layers[1]
	if qr.Kind != wire.LayerKindDerive || qr.Derive == nil {
		t.Fatalf("the QR layer did not read back as a derive layer: %+v", qr)
	}
	if qr.Derive.Data != "https://waiveo.local/pair/ABCD-1234" || qr.Derive.ECLevel != "H" {
		t.Errorf("the QR spec did not survive the round trip: %+v", qr.Derive)
	}

	// The STYLING is what this row exists for, so every un-native member is
	// checked individually. A member the surface accepts and drops is a control
	// an operator sets and never sees applied — and with a nested schema, a
	// dropped member is exactly as silent as a rejected one is loud.
	styled := stored.Slides[0].Layers[2]
	if styled.Derive == nil {
		t.Fatalf("the styled text layer lost its spec: %+v", styled)
	}
	s := styled.Derive
	if s.Fill == nil || s.Fill.Kind != wire.DeriveFillLinear || s.Fill.From != "#7C3AED" || s.Fill.To != "#0EA5E9" || s.Fill.AngleDeg != 135 {
		t.Errorf("the gradient did not survive: %+v", s.Fill)
	}
	if s.Shadow == nil || s.Shadow.DY != 10 || s.Shadow.Blur != 26 || s.Shadow.OpacityPct != 55 {
		t.Errorf("the drop shadow did not survive: %+v", s.Shadow)
	}
	if s.Border == nil || s.Border.Radius != 28 {
		t.Errorf("the corner radius did not survive: %+v", s.Border)
	}
	if s.VAlign != "middle" || s.FontPx != 110 {
		t.Errorf("the type settings did not survive: valign=%q font_px=%d", s.VAlign, s.FontPx)
	}
}

// TestACastDeriveLayerIsValidatedAtTheSurface: the spec vocabulary is closed at
// the authoring boundary, not only inside the renderer. A spec the store accepts
// and the off-appliance renderer refuses is a layer that sits pending forever,
// with the refusal visible only in a tool the operator may not be running.
func TestACastDeriveLayerIsValidatedAtTheSurface(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	cases := []struct {
		name  string
		layer map[string]any
	}{
		{"unknown derive kind", map[string]any{"kind": "derive", "x": 0, "y": 0, "w": 400, "h": 400,
			"derive": map[string]any{"kind": "hologram"}}},
		{"qr with no payload", map[string]any{"kind": "derive", "x": 0, "y": 0, "w": 400, "h": 400,
			"derive": map[string]any{"kind": "qr"}}},
		{"derive layer with no spec", map[string]any{"kind": "derive", "x": 0, "y": 0, "w": 400, "h": 400}},
		{"solid fill with a second stop", map[string]any{"kind": "derive", "x": 0, "y": 0, "w": 400, "h": 400,
			"derive": map[string]any{"kind": "rect", "fill": map[string]any{"kind": "solid", "from": "#ffffff", "to": "#000000"}}}},
		{"derive spec on a text layer", map[string]any{"kind": "text", "x": 0, "y": 0, "w": 400, "h": 400, "text": "hi",
			"derive": map[string]any{"kind": "rect", "fill": map[string]any{"kind": "solid", "from": "#ffffff"}}}},
		// TYPOGRAPHY on a spec that draws no text. Refused rather than ignored,
		// exactly as an alignment on a qr is: the renderer writes the size and
		// family into the text rule only and embeds the face for a text run only.
		// The FACE is the one that costs something beyond confusion — it is a real
		// content reference, so an accepted inert one pins a font file against the
		// retention sweep on behalf of a layer that will never draw with it.
		{"font size on a qr spec", map[string]any{"kind": "derive", "x": 0, "y": 0, "w": 400, "h": 400,
			"derive": map[string]any{"kind": "qr", "data": "x", "font_px": 40}}},
		{"custom face on a qr spec", map[string]any{"kind": "derive", "x": 0, "y": 0, "w": 400, "h": 400,
			"derive": map[string]any{"kind": "qr", "data": "x", "font_asset_ref": "sha256:" + strings.Repeat("a", 64)}}},
		{"colour on a rect spec", map[string]any{"kind": "derive", "x": 0, "y": 0, "w": 400, "h": 400,
			"derive": map[string]any{"kind": "rect", "color": "#112233",
				"fill": map[string]any{"kind": "solid", "from": "#ffffff"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := mustJSON(t, map[string]any{
				"scope_node": screenID,
				"name":       "Bad Derive",
				"slides":     []map[string]any{{"id": "s1", "layers": []map[string]any{tc.layer}}},
			})
			resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", body, nil)
			if resp.StatusCode == http.StatusCreated {
				t.Fatalf("the surface accepted %s (body %s)", tc.name, raw)
			}
			if resp.StatusCode != http.StatusUnprocessableEntity && resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d for %s, want 422 or 400 (body %s)", resp.StatusCode, tc.name, raw)
			}
		})
	}
}

// TestADeriveLayersCustomFontMustNameUploadedContent is the retention half, and
// it is the blind spot this projection has already had twice: a cast's images
// were once invisible to store.RowAssetReferences, which made them
// simultaneously unchecked at write time AND reclaimable by the content sweep
// while a screen was playing them.
//
// A derive layer's font is a SECOND reference inside a nested object, which is
// exactly where a field-path projection stops looking.
func TestADeriveLayersCustomFontMustNameUploadedContent(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	cast := datamodel.Cast{
		ScopeNode: screenID, Name: "Custom Face",
		Slides: []datamodel.CastSlide{{
			ID: "s1",
			Layers: []wire.Layer{{Kind: wire.LayerKindDerive, X: 0, Y: 0, W: 900, H: 300, Derive: &wire.DeriveSpec{
				Kind: wire.DeriveKindText, Text: "Oswald", FontFamily: "Oswald",
				FontAssetRef: "sha256:" + strings.Repeat("0", 64),
			}}},
		}},
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, cast), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST a cast whose derive font names un-uploaded content: status %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	if !problemNamesField(p, "slides[0].layers[0].derive.font_asset_ref") {
		t.Errorf("the refusal does not name the unresolvable font reference: %s", raw)
	}
}

// TestPendingDerivesAreReportedAndClearedByAWriteBack drives the work queue over
// the real surface: an authored derive layer appears, and writing the reference
// and its digest back through an ordinary PATCH clears it.
//
// The clearing half is what proves the queue is COMPUTED rather than stored. A
// queue table would have to be told; this one simply stops matching.
func TestPendingDerivesAreReportedAndClearedByAWriteBack(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	layer := wire.Layer{Kind: wire.LayerKindDerive, X: 1400, Y: 80, W: 400, H: 400, Derive: &wire.DeriveSpec{
		Kind: wire.DeriveKindQR, Data: "https://waiveo.local/pair/ABCD-1234",
	}}
	cast := datamodel.Cast{ScopeNode: screenID, Name: "Poster",
		Slides: []datamodel.CastSlide{{ID: "poster", Layers: []wire.Layer{
			{Kind: wire.LayerKindText, X: 0, Y: 0, W: 800, H: 100, Text: "Welcome"},
			layer,
		}}}}
	created := e.createOK(t, "/api/v1/casts", rowCreateBody(t, cast))
	stored := decodeCast(t, created)

	jobs := derivePendingList(t, e)
	if len(jobs) != 1 {
		t.Fatalf("the queue reports %d job(s), want 1: %+v", len(jobs), jobs)
	}
	job := jobs[0]
	if job["source"] != "cast" || job["resource_id"] != stored.ID || job["slide_id"] != "poster" {
		t.Errorf("the work order does not locate the layer: %+v", job)
	}
	if _, ok := job["item_index"]; ok {
		t.Errorf("a cast job carries an item_index, which addresses nothing: %+v", job)
	}
	if job["state"] != "pending" {
		t.Errorf("state = %v, want pending", job["state"])
	}
	if job["layer_index"] != float64(1) || job["w"] != float64(400) || job["h"] != float64(400) {
		t.Errorf("the work order is not a complete instruction: %+v", job)
	}
	digest, _ := job["spec_digest"].(string)
	if digest != wire.DeriveDigest(stored.Slides[0].Layers[1]) {
		t.Errorf("spec_digest %q does not match the layer's own digest", digest)
	}

	// Upload a PNG-shaped asset and write the reference back the way the tool
	// does — an ordinary content upload and an ordinary conditional PATCH.
	assetRef := e.uploadContent(t, []byte("\x89PNG\r\n\x1a\n derived pixels")).AssetRef
	stored.Slides[0].Layers[1].AssetRef = assetRef
	stored.Slides[0].Layers[1].DerivedFrom = digest

	etag := e.etagOfDefault(t, "/api/v1/casts/"+stored.ID)
	resp, raw := e.do(t, http.MethodPatch, "/api/v1/casts/"+stored.ID,
		mustJSON(t, map[string]any{"slides": stored.Slides}),
		map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH the rendered reference back: %d %s", resp.StatusCode, raw)
	}

	if jobs := derivePendingList(t, e); len(jobs) != 0 {
		t.Fatalf("the queue still reports %d job(s) after the write-back: %+v", len(jobs), jobs)
	}

	// And an edit puts it straight back, as STALE rather than pending — the
	// distinction that tells an operator a picture is still on the wall.
	after := decodeCast(t, raw)
	after.Slides[0].Layers[1].W = 520
	etag = e.etagOfDefault(t, "/api/v1/casts/"+stored.ID)
	resp, raw = e.do(t, http.MethodPatch, "/api/v1/casts/"+stored.ID,
		mustJSON(t, map[string]any{"slides": after.Slides}),
		map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH the resize: %d %s", resp.StatusCode, raw)
	}
	jobs = derivePendingList(t, e)
	if len(jobs) != 1 || jobs[0]["state"] != "stale" {
		t.Fatalf("a resized layer produced %+v, want one job in state stale", jobs)
	}
}

// TestTheDeclaredSurfaceCarriesTheDeriveVocabulary pins the DOCUMENT through the
// GENERATED types — the artifact clients are actually built from — so it proves
// both that the schema declares the members and that the codegen output in the
// tree is regenerated rather than stale. An undeclared member is a member no
// typed client can send: the console would have no way to author a gradient, and
// the whole feature would be reachable only by hand-rolled JSON.
func TestTheDeclaredSurfaceCarriesTheDeriveVocabulary(t *testing.T) {
	if !apiv1.SlideLayerKindDerive.Valid() {
		t.Error("the generated SlideLayer kind enum does not accept `derive`")
	}
	if apiv1.SlideLayerKind("hologram").Valid() {
		t.Error("the generated SlideLayer kind enum accepts a value outside the closed set")
	}
	for _, k := range []apiv1.DeriveSpecKind{apiv1.DeriveSpecKindQr, apiv1.DeriveSpecKindText, apiv1.DeriveSpecKindRect} {
		if !k.Valid() {
			t.Errorf("the generated DeriveSpec kind enum rejects %q", k)
		}
	}
	if apiv1.DeriveSpecKind("hologram").Valid() {
		t.Error("the generated DeriveSpec kind enum accepts a value outside the closed set")
	}
	for _, k := range []apiv1.DeriveFillKind{apiv1.DeriveFillKindSolid, apiv1.DeriveFillKindLinear, apiv1.DeriveFillKindRadial} {
		if !k.Valid() {
			t.Errorf("the generated DeriveFill kind enum rejects %q", k)
		}
	}

	// The layer must be constructible with every un-native member set, from the
	// generated types alone.
	angle := 135
	dy, blur, op := 10, 26, 55
	radius := 28
	layer := apiv1.SlideLayer{
		Kind: apiv1.SlideLayerKindDerive, X: 0, Y: 0, W: 900, H: 300,
		Derive: &apiv1.DeriveSpec{
			Kind:   apiv1.DeriveSpecKindText,
			Text:   ptrTo("SCAN TO PAIR"),
			Fill:   &apiv1.DeriveFill{Kind: apiv1.DeriveFillKindLinear, From: "#7C3AED", To: ptrTo("#0EA5E9"), AngleDeg: &angle},
			Shadow: &apiv1.DeriveShadow{Dy: &dy, Blur: &blur, OpacityPct: &op},
			Border: &apiv1.DeriveBorder{Radius: &radius},
		},
		DerivedFrom: ptrTo("deadbeef"),
	}
	if layer.Derive == nil || layer.Derive.Fill == nil || layer.Derive.Shadow == nil || layer.Derive.Border == nil {
		t.Fatal("the generated SlideLayer cannot express a fully styled derive layer")
	}

	// And the work queue's own response type, so the tool and the console are
	// built from a declared shape rather than from an untyped map. BOTH authored
	// shapes must be expressible: a queue that could only name a cast is a queue
	// an inline slide's layer can never appear in.
	var list apiv1.DerivePendingList
	list.DeriveJobs = []apiv1.DerivePendingLayer{{
		Source: apiv1.DerivePendingLayerSourceCast, ResourceId: "c", SlideId: ptrTo("s"),
		LayerIndex: 0, W: 400, H: 400,
		State: apiv1.DerivePendingLayerStatePending, SpecDigest: "d",
		Spec: apiv1.DeriveSpec{Kind: apiv1.DeriveSpecKindQr, Data: ptrTo("x")},
	}, {
		Source: apiv1.DerivePendingLayerSourcePlaylist, ResourceId: "p", ItemIndex: ptrTo(2),
		LayerIndex: 1, W: 360, H: 360,
		State: apiv1.DerivePendingLayerStateStale, SpecDigest: "d2",
		Spec: apiv1.DeriveSpec{Kind: apiv1.DeriveSpecKindQr, Data: ptrTo("y")},
	}}
	if !apiv1.DerivePendingLayerStateStale.Valid() {
		t.Error("the generated pending-state enum does not accept `stale`")
	}
	if !apiv1.DerivePendingLayerSourcePlaylist.Valid() {
		t.Error("the generated pending-source enum does not accept `playlist`")
	}
	if apiv1.DerivePendingLayerSource("cast_group").Valid() {
		t.Error("the generated pending-source enum accepts a value outside the closed set")
	}
}

// TestADeriveLayerInsideAnInlinePlaylistSlideIsQueued is the SECOND authored
// shape, and it is the one the queue could not see.
//
// A `source: "slide"` playlist item carries its layer stack inline. That stack
// is accepted by the surface, rewritten into an ordinary `image` by BOTH content
// projections (the feeder's resolveSlideLayers and the relay's, through the one
// shared wire.DeriveProjection), and its custom font is held against the content
// retention sweep by store.RowAssetReferences. Every half of the mechanism was
// built for it except the queue that reports the work — so the layer was
// accepted, protected, and never once drawn.
func TestADeriveLayerInsideAnInlinePlaylistSlideIsQueued(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	layer := wire.Layer{Kind: wire.LayerKindDerive, X: 1400, Y: 80, W: 360, H: 360, Derive: &wire.DeriveSpec{
		Kind: wire.DeriveKindQR, Data: "https://waiveo.local/pair/INLINE-1",
	}}
	pl := datamodel.Playlist{
		ScopeNode: screenID, Name: "Foyer loop",
		Items: []datamodel.PlaylistItem{
			{Source: "slide", Slide: &datamodel.Slide{Layers: []wire.Layer{
				{Kind: wire.LayerKindText, X: 0, Y: 0, W: 800, H: 100, Text: "Scan to pair"},
			}}},
			{Source: "slide", Slide: &datamodel.Slide{Layers: []wire.Layer{
				{Kind: wire.LayerKindText, X: 0, Y: 0, W: 800, H: 100, Text: "Scan to pair"},
				layer,
			}}},
		},
	}
	created := e.createOK(t, "/api/v1/playlists", rowCreateBody(t, pl))
	id := decodeID(t, created)

	jobs := derivePendingList(t, e)
	if len(jobs) != 1 {
		t.Fatalf("the queue reports %d job(s) for an inline-slide derive layer, want 1: %+v", len(jobs), jobs)
	}
	job := jobs[0]
	if job["source"] != "playlist" || job["resource_id"] != id {
		t.Errorf("the work order does not name the playlist that carries the layer: %+v", job)
	}
	// The ITEM index, not the first item — an inline slide has no id of its own,
	// so this is the only thing that says where the raster goes back.
	if job["item_index"] != float64(1) || job["layer_index"] != float64(1) {
		t.Errorf("the work order does not locate the layer inside the playlist: %+v", job)
	}
	if _, ok := job["slide_id"]; ok {
		t.Errorf("an inline-slide job carries a slide_id, which no inline slide has: %+v", job)
	}
	if job["w"] != float64(360) || job["h"] != float64(360) || job["state"] != "pending" {
		t.Errorf("the work order is not a complete instruction: %+v", job)
	}

	// And the write-back clears it, exactly as a cast's does — the queue is
	// computed from the authored rows, so it stops matching with nothing told.
	assetRef := e.uploadContent(t, []byte("\x89PNG\r\n\x1a\n inline derived pixels")).AssetRef
	digest, _ := job["spec_digest"].(string)
	pl.Items[1].Slide.Layers[1].AssetRef = assetRef
	pl.Items[1].Slide.Layers[1].DerivedFrom = digest

	etag := e.etagOfDefault(t, "/api/v1/playlists/"+id)
	resp, raw := e.do(t, http.MethodPatch, "/api/v1/playlists/"+id,
		mustJSON(t, map[string]any{"items": pl.Items}),
		map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH the rendered reference back onto the inline slide: %d %s", resp.StatusCode, raw)
	}
	if jobs := derivePendingList(t, e); len(jobs) != 0 {
		t.Fatalf("the queue still reports %d job(s) after the inline write-back: %+v", len(jobs), jobs)
	}
}

// TestBothAuthoredShapesAreQueuedAndProtectedTogether is the MIRROR check, and
// it is the one that keeps this fixed.
//
// Two projections read the same authored layer stacks: the retention/write-time
// projection (store.RowAssetReferences) and this work queue. They disagreed once
// — the sweep protected an inline slide's derive font while the queue could not
// see the layer at all — and the fix is only durable if the reverse is caught
// too. Both shapes, both projections, one test.
func TestBothAuthoredShapesAreQueuedAndProtectedTogether(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)
	fontRef := e.uploadContent(t, []byte("waiveo-next: not really a TTF, but real bytes in the origin")).AssetRef

	spec := func(text string) *wire.DeriveSpec {
		return &wire.DeriveSpec{Kind: wire.DeriveKindText, Text: text, FontFamily: "Oswald", FontAssetRef: fontRef}
	}
	castID := decodeID(t, e.createOK(t, "/api/v1/casts", rowCreateBody(t, datamodel.Cast{
		ScopeNode: screenID, Name: "Custom Face Cast",
		Slides: []datamodel.CastSlide{{ID: "s1", Layers: []wire.Layer{
			{Kind: wire.LayerKindDerive, X: 0, Y: 0, W: 900, H: 300, Derive: spec("IN A CAST")},
		}}},
	})))
	listID := decodeID(t, e.createOK(t, "/api/v1/playlists", rowCreateBody(t, datamodel.Playlist{
		ScopeNode: screenID, Name: "Custom Face Playlist",
		Items: []datamodel.PlaylistItem{{Source: "slide", Slide: &datamodel.Slide{Layers: []wire.Layer{
			{Kind: wire.LayerKindDerive, X: 0, Y: 0, W: 900, H: 300, Derive: spec("IN A PLAYLIST")},
		}}}},
	})))

	// The QUEUE sees both.
	jobs := derivePendingList(t, e)
	byResource := map[string]map[string]any{}
	for _, j := range jobs {
		id, _ := j["resource_id"].(string)
		byResource[id] = j
	}
	if _, ok := byResource[castID]; !ok {
		t.Errorf("the cast's derive layer is not in the queue: %+v", jobs)
	}
	if _, ok := byResource[listID]; !ok {
		t.Errorf("the inline slide's derive layer is not in the queue: %+v", jobs)
	}

	// …and the RETENTION projection holds the font for both. store.RowLayerStacks
	// is the single enumeration behind each answer, which is what makes the two
	// impossible to fix on one side only.
	for _, kind := range []store.Kind{store.KindCast, store.KindPlaylist} {
		rows, err := e.store.List(t.Context(), kind, store.ListFilter{})
		if err != nil {
			t.Fatalf("list %s: %v", kind, err)
		}
		var found bool
		for _, row := range rows {
			refs, err := store.RowAssetReferences(kind, row.Body)
			if err != nil {
				t.Fatalf("project %s asset references: %v", kind, err)
			}
			for _, ref := range refs {
				if ref.Ref == fontRef {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("no %s row reports the derive font — the sweep would reclaim bytes a screen is drawing", kind)
		}
	}
}

func ptrTo[T any](v T) *T { return &v }

// etagOfDefault reads a row's ETag as the env's default principal, which is who
// e.do already acts as. The existing etagOf takes an explicit credential because
// the authorization suite needs one; here the point is the conditional write, not
// who makes it.
func (e *testEnv) etagOfDefault(t *testing.T, path string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body %s)", path, resp.StatusCode, raw)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("GET %s returned no ETag", path)
	}
	return etag
}
