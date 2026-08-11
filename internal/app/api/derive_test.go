package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apiv1 "github.com/maaxton/waiveo-next/api/gen/go"

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
	if job["cast_id"] != stored.ID || job["slide_id"] != "poster" {
		t.Errorf("the work order does not locate the layer: %+v", job)
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
	// built from a declared shape rather than from an untyped map.
	var list apiv1.DerivePendingList
	list.DeriveJobs = []apiv1.DerivePendingLayer{{
		CastId: "c", SlideId: "s", LayerIndex: 0, W: 400, H: 400,
		State: apiv1.DerivePendingLayerStatePending, SpecDigest: "d",
		Spec: apiv1.DeriveSpec{Kind: apiv1.DeriveSpecKindQr, Data: ptrTo("x")},
	}}
	if !apiv1.DerivePendingLayerStateStale.Valid() {
		t.Error("the generated pending-state enum does not accept `stale`")
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
