package api_test

// castbundles_test.go is parity row 1.9's validation, driven through the real
// mux: a cast — with its image assets — round-trips out of one deployment and
// into a SECOND, EMPTY one.
//
// The second deployment is what makes it a test of portability rather than of
// serialization. Importing back onto the box that exported already has every
// asset in its origin, so a bundle that carried none at all would still produce
// a cast that renders — and the defect would only appear on the box the operator
// actually cared about.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/castbundle"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

var (
	bundleLogo = []byte("waiveo-next: the logo a cast layer draws")
	bundleHero = []byte("waiveo-next: the hero photo a cast layer draws")
)

// writeBundle produces a real `.cast` for a test that needs one to feed the
// import route, running both halves of the producer API explicitly.
//
// It is spelled out rather than hidden behind a one-call helper in castbundle
// itself: that helper existed, had no non-test caller, and its own doc had to
// ask HTTP handlers not to use it — see castbundle.go where it used to be. A
// test package is exactly the caller it was for, so the two lines live here.
func writeBundle(t *testing.T, w io.Writer, m castbundle.Manifest, assets map[string][]byte) {
	t.Helper()
	plan, err := castbundle.NewPlan(m, assets)
	if err != nil {
		t.Fatalf("castbundle.NewPlan: %v", err)
	}
	if err := plan.Stream(w); err != nil {
		t.Fatalf("castbundle Plan.Stream: %v", err)
	}
}

// seedBundleCast authors a two-slide cast with two images and returns its id,
// the scope node it sits at, and its name.
func seedBundleCast(t *testing.T, e *testEnv, name string) (castID, scopeNode string) {
	t.Helper()
	scopeNode = seedSchedulingScope(t, e)
	logoRef := e.uploadContent(t, bundleLogo).AssetRef
	heroRef := e.uploadContent(t, bundleHero).AssetRef

	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, datamodel.Cast{
		ScopeNode: scopeNode, Name: name, DefaultDurationMS: 8000,
		Labels: map[string]string{"site": "shop"},
		Slides: []datamodel.CastSlide{
			{ID: "hero", Layers: []wire.Layer{
				{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: heroRef},
				{Kind: wire.LayerKindImage, X: 20, Y: 20, W: 200, H: 80, AssetRef: logoRef},
			}},
			{ID: "closing", Layers: []wire.Layer{
				{Kind: wire.LayerKindImage, X: 20, Y: 20, W: 200, H: 80, AssetRef: logoRef},
			}},
		},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed cast: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw), scopeNode
}

// exportBundle downloads a cast's bundle and returns the bytes.
func exportBundle(t *testing.T, e *testEnv, castID string) []byte {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, "/api/v1/casts/"+castID+"/export", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", got)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, ".cast") {
		t.Errorf("Content-Disposition = %q, want a .cast attachment name", cd)
	}
	return raw
}

// importBundle POSTs a bundle to e, placed at scopeNode.
func importBundle(t *testing.T, e *testEnv, bundle []byte, scopeNode, rename string) (*http.Response, []byte) {
	t.Helper()
	path := "/api/v1/casts/import?scope_node=" + scopeNode
	if rename != "" {
		path += "&name=" + rename
	}
	return e.do(t, http.MethodPost, path, bundle, map[string]string{"Content-Type": "application/octet-stream"})
}

// TestACastRoundTripsOntoASecondBoxWithItsImages is row 1.9's claim.
func TestACastRoundTripsOntoASecondBoxWithItsImages(t *testing.T) {
	source := newEnv(t)
	castID, _ := seedBundleCast(t, source, "Lunch — Tuesday (v2)")
	bundle := exportBundle(t, source, castID)

	// A SECOND deployment: its own store, its own content origin, its own
	// principals. Nothing about the design is here yet.
	dest := newEnv(t)
	destNode := seedSchedulingScope(t, dest)
	if dest.content.Has(hexOf(bundleHero)) {
		t.Fatal("the destination already holds the hero image; this test would prove nothing about assets")
	}

	resp, raw := importBundle(t, dest, bundle, destNode, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("import = %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	var imported map[string]any
	mustUnmarshal(t, raw, &imported)

	// The design came across.
	if imported["name"] != "Lunch — Tuesday (v2)" {
		t.Errorf("imported name = %v", imported["name"])
	}
	if imported["scope_node"] != destNode {
		t.Errorf("imported scope_node = %v, want the DESTINATION's node %q — a bundle must not carry the source's placement",
			imported["scope_node"], destNode)
	}
	if got := int64(imported["default_duration_ms"].(float64)); got != 8000 {
		t.Errorf("default_duration_ms = %d, want 8000", got)
	}
	slides, _ := imported["slides"].([]any)
	if len(slides) != 2 {
		t.Fatalf("imported %d slide(s), want 2: %s", len(slides), raw)
	}

	// The IDENTITY did not. The imported cast is a new row here.
	if imported["id"] == castID {
		t.Errorf("the imported cast reused the source's id %q — a bundle must carry no identity", castID)
	}
	if imported["id"] == "" || imported["id"] == nil {
		t.Fatal("the imported cast has no id")
	}
	if rev, ok := imported["revision"].(float64); !ok || int(rev) != 1 {
		t.Errorf("revision = %v, want 1 — a fresh row here, not the source's revision", imported["revision"])
	}
	if resp.Header.Get("ETag") == "" || resp.Header.Get("Location") == "" {
		t.Errorf("import answered without ETag/Location; it must be the ordinary create's own response")
	}

	// THE IMAGES came across, and are servable HERE — the difference between a
	// row that mentions an image and a screen that can draw one.
	if !dest.content.Has(hexOf(bundleHero)) || !dest.content.Has(hexOf(bundleLogo)) {
		t.Fatal("the import did not bring the cast's images: every slide would reference an asset_ref this box has " +
			"never heard of, and every screen would render a blank while the import reported success")
	}
	if got := dest.content.Serve(hexOf(bundleHero)); !bytes.Equal(got, bundleHero) {
		t.Errorf("the destination serves %d byte(s) for the hero image", len(got))
	}

	// And the imported cast reads back through the ordinary API.
	reread := readRow(t, dest, "/api/v1/casts/"+imported["id"].(string))
	if reread["name"] != "Lunch — Tuesday (v2)" {
		t.Errorf("re-read name = %v", reread["name"])
	}
}

// TestAnImportGoesThroughTHEcreatePathNotAroundIt.
//
// The bundle is untrusted input that arrived as a file. If the import had its
// own write path it would be the one door into the store that skips a rule, and
// every rule about a cast exists because breaking it renders wrong on a
// television. Proven by handing it a bundle whose slides the platform's own
// authoring validation refuses: the SAME 422 the editor would get, and nothing
// stored.
func TestAnImportIsHeldToTheSameAuthoringRulesAsTheEditor(t *testing.T) {
	dest := newEnv(t)
	destNode := seedSchedulingScope(t, dest)

	// A layer that runs off the 1920x1080 canvas — a data-model rule, not a
	// bundle-format rule, so only the real create path can catch it.
	var buf bytes.Buffer
	writeBundle(t, &buf, castbundle.Manifest{Cast: castbundle.CastPayload{
		Name: "Off canvas",
		Slides: []datamodel.CastSlide{{ID: "s1", Layers: []wire.Layer{
			{Kind: wire.LayerKindRect, X: 9000, Y: 9000, W: 100, H: 100, Color: "#112233"},
		}}},
	}}, map[string][]byte{})

	before := castCount(t, dest)
	resp, raw := importBundle(t, dest, buf.Bytes(), destNode, "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("import of an off-canvas slide = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	if got := castCount(t, dest); got != before {
		t.Errorf("a refused import stored %d cast(s)", got-before)
	}
}

// TestAnImportIsHeldToTheSameBodyCeilingAsTheEditor closes the one route that
// could author a cast this box cannot export.
//
// castbundle.MaxManifestBytes (4 MiB) is argued as safe on the grounds that
// "nothing this box authors can approach it, because a cast's create body is
// capped at maxJSONBodyBytes (1 MiB)". That was true of every path except this
// one: the import assembles its create body IN PROCESS from the bundle's own
// manifest, so it never passed through readBodyLimit. A bundle carrying a 2 MiB
// manifest therefore imported cleanly and produced a cast whose own re-export
// needs a manifest of 2 MiB plus every asset entry — a design an operator can
// import and cannot get back out, which is precisely the export/import
// disagreement that limit block exists to make impossible.
//
// The refusal is checked rather than the arithmetic, because the arithmetic is
// what was wrong: a comment claiming a bound that a code path does not apply is
// the shape of this defect.
func TestAnImportIsHeldToTheSameBodyCeilingAsTheEditor(t *testing.T) {
	dest := newEnv(t)
	destNode := seedSchedulingScope(t, dest)

	// A cast whose SLIDES alone exceed the create-body ceiling, while its
	// manifest stays inside what castbundle.Read accepts — the window this hole
	// lived in. One long text layer is the cheapest way there; the bundle format
	// itself has no objection to it.
	huge := strings.Repeat("a", 1_500_000)
	var buf bytes.Buffer
	writeBundle(t, &buf, castbundle.Manifest{Cast: castbundle.CastPayload{
		Name: "A cast no box could export again",
		Slides: []datamodel.CastSlide{{ID: "s1", Layers: []wire.Layer{
			{Kind: wire.LayerKindText, X: 0, Y: 0, W: 100, H: 100, Text: huge},
		}}},
	}}, map[string][]byte{})
	if buf.Len() > castbundle.MaxBundleBytes {
		t.Fatalf("fixture is %d bytes, past the %d the import route accepts at all: this would be refused by the body limit rather than by the ceiling under test",
			buf.Len(), int64(castbundle.MaxBundleBytes))
	}

	before := castCount(t, dest)
	resp, raw := importBundle(t, dest, buf.Bytes(), destNode, "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("import of a cast larger than the create-body ceiling = %d, want 422.\n"+
			"Accepting it stores a design this box will refuse to export, which is the disagreement castbundle's size block exists to prevent. Body: %s",
			resp.StatusCode, raw)
	}
	if got := castCount(t, dest); got != before {
		t.Errorf("a refused import stored %d cast(s)", got-before)
	}
}

// TestAnImportIsAuthorizedAtTheDESTINATIONPlacement. An import is a write, and a
// row written under a node the caller has no authority at is invisible to its
// own author the moment it lands.
func TestAnImportIsAuthorizedAtTheDestinationPlacement(t *testing.T) {
	source := newEnv(t)
	castID, _ := seedBundleCast(t, source, "Menu")
	bundle := exportBundle(t, source, castID)

	dest := newEnv(t)
	org := dest.orgRoot(t)
	destNode := seedSchedulingScope(t, dest)
	elsewhere := dest.createNode(t, siteUnder(org))
	viewer := dest.principalWith(t, roleAt{node: elsewhere, role: auth.RoleAdmin})

	before := castCount(t, dest)
	resp, raw := dest.as(t, viewer, http.MethodPost,
		"/api/v1/casts/import?scope_node="+destNode, bundle,
		map[string]string{"Content-Type": "application/octet-stream"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("import at a node the caller has no authority at = %d, want 403 (body %s)", resp.StatusCode, raw)
	}
	if got := castCount(t, dest); got != before {
		t.Errorf("a refused import stored %d cast(s)", got-before)
	}
}

// TestAnImportRequiresADestinationPlacement. A bundle carries none — a scope
// node names a tree the source had and this box does not — so guessing would put
// an operator's design at a node they did not choose and, half the time, cannot
// see.
func TestAnImportRequiresADestinationPlacement(t *testing.T) {
	source := newEnv(t)
	castID, _ := seedBundleCast(t, source, "Menu")
	bundle := exportBundle(t, source, castID)

	dest := newEnv(t)
	resp, raw := dest.do(t, http.MethodPost, "/api/v1/casts/import", bundle,
		map[string]string{"Content-Type": "application/octet-stream"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("import with no scope_node = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")
}

// TestAnImportCanRenameOnTheWayIn — what importing a second copy of a design
// onto one box needs.
func TestAnImportCanRenameOnTheWayIn(t *testing.T) {
	e := newEnv(t)
	castID, node := seedBundleCast(t, e, "Menu")
	bundle := exportBundle(t, e, castID)

	resp, raw := importBundle(t, e, bundle, node, "Menu%20copy")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("import = %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	var imported map[string]any
	mustUnmarshal(t, raw, &imported)
	if imported["name"] != "Menu copy" {
		t.Errorf("imported name = %v, want the rename", imported["name"])
	}
}

// TestARefusedBundleIsRefusedWithASENTENCE, distinct per failure, because "you
// picked the wrong file", "this file is damaged" and "this file has been
// altered" are three different next actions.
func TestARefusedBundleSaysWhichKindOfWrongItIs(t *testing.T) {
	e := newEnv(t)
	node := seedSchedulingScope(t, e)

	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"not a bundle at all", []byte("just a text file an operator dragged in"), "not a cast bundle"},
		{"an empty body", nil, "must carry a cast bundle"},
	}
	for _, tc := range cases {
		resp, raw := importBundle(t, e, tc.body, node, "")
		if resp.StatusCode != http.StatusUnprocessableEntity && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d (body %s)", tc.name, resp.StatusCode, raw)
			continue
		}
		var p map[string]any
		mustUnmarshal(t, raw, &p)
		detail, _ := p["detail"].(string)
		if !strings.Contains(detail, tc.want) {
			t.Errorf("%s: detail = %q, want it to contain %q", tc.name, detail, tc.want)
		}
	}
}

// TestExportingACastWhoseImageIsGoneIsRefused, not exported with a hole. The
// hole would surface only on the destination, as a slide that renders blank,
// long after anyone could connect it to this export.
func TestExportingACastWhoseImageIsGoneIsRefused(t *testing.T) {
	// The origin is built here so the test can remove bytes from underneath the
	// row — which is exactly what the retention sweep does to an asset a cast
	// stopped referencing and then started referencing again.
	content := origin.New()
	e := newEnvWithContent(t, content)
	castID, _ := seedBundleCast(t, e, "Menu")
	if err := content.Remove(hexOf(bundleHero)); err != nil {
		t.Fatalf("remove the hero image: %v", err)
	}

	resp, raw := e.do(t, http.MethodGet, "/api/v1/casts/"+castID+"/export", nil, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("export of a cast with a missing image = %d, want 409 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "CONFLICT")
}

// TestExportingACastYouCannotSeeIs404, never 403 — a distinct refusal would tell
// a caller a cast they may not see exists.
func TestExportingACastYouCannotSeeIs404(t *testing.T) {
	e := newEnv(t)
	castID, _ := seedBundleCast(t, e, "Menu")
	org := e.orgRoot(t)
	elsewhere := e.createNode(t, siteUnder(org))
	stranger := e.principalWith(t, roleAt{node: elsewhere, role: auth.RoleAdmin})

	resp, raw := e.as(t, stranger, http.MethodGet, "/api/v1/casts/"+castID+"/export", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("export as a principal who cannot see the cast = %d, want 404 (body %s)", resp.StatusCode, raw)
	}
}

// TestTheExportedBundleIsTheDeclaredFORMAT, verified by reading it back with the
// format's own reader — which checks every asset's hash and refuses an entry the
// manifest does not declare.
func TestTheExportedBundleIsTheDeclaredFormat(t *testing.T) {
	e := newEnv(t)
	castID, _ := seedBundleCast(t, e, "Menu")
	got, err := castbundle.Read(exportBundle(t, e, castID))
	if err != nil {
		t.Fatalf("the exported bundle does not read back as one: %v", err)
	}
	if got.Manifest.SourceCastID != castID {
		t.Errorf("source_cast_id = %q, want the exporting box's own cast id %q (provenance)", got.Manifest.SourceCastID, castID)
	}
	if len(got.Assets) != 2 {
		t.Errorf("bundle carries %d asset(s), want the 2 distinct images the cast references", len(got.Assets))
	}
	if got.Manifest.Cast.Labels["site"] != "shop" {
		t.Errorf("labels did not travel: %v", got.Manifest.Cast.Labels)
	}
	// Identity and placement must NOT be TOP-LEVEL members of the manifest's
	// cast payload. Checked against the serialized JSON rather than against the
	// Go struct, because the struct having no field for them is the thing under
	// test; and scoped to the payload's own keys, because a SLIDE legitimately
	// has an `id` (it names a position in the document, not a row) and a
	// whole-document substring search would fail on it and prove nothing.
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(got.Manifest.Cast); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	var payload map[string]any
	mustUnmarshal(t, buf.Bytes(), &payload)
	for _, forbidden := range []string{"id", "scope_node", "external_id", "revision", "created_at", "updated_at"} {
		if _, present := payload[forbidden]; present {
			t.Errorf("the bundle's cast payload carries %q, which describes the SOURCE deployment", forbidden)
		}
	}
	// And the members that MUST be there, so this is not a test that passes on
	// an empty payload.
	for _, required := range []string{"name", "slides"} {
		if _, present := payload[required]; !present {
			t.Errorf("the bundle's cast payload omits %q", required)
		}
	}
}

// TestImportingTheSameBundleTwiceUnderOneKeyReplaysRatherThanDuplicating —
// API-050/052: a retry-on-timeout must not import a second copy.
func TestImportingTheSameBundleTwiceUnderOneKeyReplays(t *testing.T) {
	e := newEnv(t)
	castID, node := seedBundleCast(t, e, "Menu")
	bundle := exportBundle(t, e, castID)
	headers := map[string]string{"Content-Type": "application/octet-stream", "Idempotency-Key": "01J8ZIMPORTKEY000000000001"}

	before := castCount(t, e)
	resp1, raw1 := e.do(t, http.MethodPost, "/api/v1/casts/import?scope_node="+node, bundle, headers)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first import = %d (body %s)", resp1.StatusCode, raw1)
	}
	resp2, raw2 := e.do(t, http.MethodPost, "/api/v1/casts/import?scope_node="+node, bundle, headers)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("replayed import = %d (body %s)", resp2.StatusCode, raw2)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Errorf("the replay returned a DIFFERENT cast:\n first: %s\nsecond: %s", raw1, raw2)
	}
	if got := castCount(t, e) - before; got != 1 {
		t.Errorf("two keyed imports created %d cast(s), want 1", got)
	}
}

// castCount reads how many cast rows the deployment holds, straight from the
// store — a count taken through the API would be filtered by the reader's own
// visible set, which is the thing several of these cases are varying.
func castCount(t *testing.T, e *testEnv) int {
	t.Helper()
	rows, err := e.store.List(t.Context(), store.KindCast, store.ListFilter{})
	if err != nil {
		t.Fatalf("list casts: %v", err)
	}
	return len(rows)
}

// ── Size: the round trip has to close for a cast this platform can hold ─────

// bigAsset returns distinct, incompressible-in-practice-irrelevant bytes of the
// given size. The asset entries in a bundle are STORED rather than deflated, so
// a run of zeros is as large on the wire as any photograph — which is what keeps
// these tests affordable.
func bigAsset(size int, marker byte) []byte {
	b := make([]byte, size)
	b[0] = marker
	b[size-1] = marker
	return b
}

// seedCastWithAssets authors a one-slide cast whose layers reference bytes put
// straight into the origin, bypassing POST /content — a 64 MiB upload through
// the HTTP layer would cost another whole copy and prove nothing this does not.
func seedCastWithAssets(t *testing.T, e *testEnv, content *origin.Store, name string, bodies [][]byte) (castID, scopeNode string) {
	t.Helper()
	scopeNode = seedSchedulingScope(t, e)
	var layers []wire.Layer
	for _, body := range bodies {
		ref, err := content.Add(body)
		if err != nil {
			t.Fatalf("seed the origin: %v", err)
		}
		layers = append(layers, wire.Layer{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: ref})
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, datamodel.Cast{
		ScopeNode: scopeNode, Name: name,
		Slides: []datamodel.CastSlide{{ID: "s1", Layers: layers}},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed cast: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw), scopeNode
}

// TestABundleBiggerThanASingleUploadStillImports is the finding, driven end to
// end: a `.cast` this box produced must be a `.cast` this box accepts.
//
// The import route used to cap the request body at the SINGLE-ASSET upload
// ceiling (64 MiB) while the reader advertised 512 MiB and the export was
// bounded by nothing. A cast with video layers — one asset at the platform's own
// upload ceiling is enough — exported cleanly and then hit
// `http.MaxBytesReader` on arrival: 400 VALIDATION_FAILED, permanently, about a
// limit the reader claimed not to have. The operator's whole stated workflow
// ("build it on the office box, put it on the shop's box") failed for a
// supported cast shape.
//
// The asset is exactly at the upload ceiling, which is the smallest size that
// proves the old cap is gone: the bundle around it is necessarily larger.
func TestABundleBiggerThanASingleUploadStillImports(t *testing.T) {
	sourceContent := origin.New()
	source := newEnvWithContent(t, sourceContent)
	video := bigAsset(castbundle.MaxAssetBytes, 0xA1)
	castID, _ := seedCastWithAssets(t, source, sourceContent, "Shop loop", [][]byte{video})

	bundle := exportBundle(t, source, castID)
	if len(bundle) <= castbundle.MaxAssetBytes {
		t.Fatalf("the exported bundle is %d bytes, not larger than one upload (%d) — this test would pass under the old cap and prove nothing",
			len(bundle), int64(castbundle.MaxAssetBytes))
	}

	destContent := origin.New()
	dest := newEnvWithContent(t, destContent)
	destNode := seedSchedulingScope(t, dest)

	resp, raw := importBundle(t, dest, bundle, destNode, "")
	if resp.StatusCode != http.StatusCreated {
		detail := raw
		if len(detail) > 400 {
			detail = detail[:400]
		}
		t.Fatalf("importing a %d-byte bundle THIS PLATFORM just produced = %d, want 201 (body %s)",
			len(bundle), resp.StatusCode, detail)
	}
	if !destContent.Has(hexOf(video)) {
		t.Fatal("the destination did not receive the video layer's bytes; the imported cast references an asset this box cannot serve")
	}
}

// TestAnExportTooLargeToImportIsRefusedRatherThanProduced is the other half of
// the same agreement, and the availability half of the finding.
//
// Before, the export was bounded by nothing at all: MaxAssets entries at the
// per-upload ceiling is 32 GiB, and an authenticated caller could ask a
// Pi-class appliance to marshal that with one GET. Refusing here is also the
// only honest answer — a bundle over the limit could not be imported anywhere,
// so producing one would hand an operator a file with no destination.
func TestAnExportTooLargeToImportIsRefusedRatherThanProduced(t *testing.T) {
	content := origin.New()
	e := newEnvWithContent(t, content)
	// Three assets at the per-asset ceiling: past MaxBundleContentBytes, which
	// is two.
	bodies := [][]byte{
		bigAsset(castbundle.MaxAssetBytes, 0xB1),
		bigAsset(castbundle.MaxAssetBytes, 0xB2),
		bigAsset(castbundle.MaxAssetBytes, 0xB3),
	}
	castID, _ := seedCastWithAssets(t, e, content, "A media library, not a design", bodies)

	resp, raw := e.do(t, http.MethodGet, "/api/v1/casts/"+castID+"/export", nil, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("export of a cast past the bundle limit = %d, want 409 (first bytes %q)", resp.StatusCode, raw[:min(len(raw), 200)])
	}
	assertProblem(t, resp, raw, "CONFLICT")
	var p map[string]any
	mustUnmarshal(t, raw, &p)
	if detail, _ := p["detail"].(string); !strings.Contains(detail, "limit a cast bundle carries") {
		t.Errorf("detail = %q, want it to name the limit the operator has hit", detail)
	}
}

// TestEveryExportRefusalIsAProblemAndNotAZeroByteCast is the finding this file's
// second round was opened for, driven through the real mux.
//
// The export used to pre-check ONE of the writer's refusals — the asset total —
// commit `200 application/zip` with a `.cast` filename, and only then call the
// writer, which has more refusals than that. All of them fire before the first
// zip byte, so the response was a 200 with a ZERO-BYTE body and the error went
// to a log line. An operator saved a 0-byte `.cast`, nothing said it had failed,
// and the receiving box answered "That file is not a cast bundle… a cast bundle
// is the file a Waiveo box produces from a cast's Export" — false, and pointing
// them at the wrong box.
//
// Each case below is one of those refusals, reached the way it is actually
// reachable on a box:
//
//   - 513 images: nothing caps slides or layers (datamodel validate only rejects
//     zero slides) and the 1 MiB create-body limit admits thousands of layers,
//     so this is an ordinary authoring accident, not an exotic input.
//   - an image past the per-asset ceiling: `POST /content` would not accept one,
//     but a workspace restore places asset bytes with no per-entry cap
//     (internal/archive), so the origin can hold one.
//
// The assertion is deliberately about the RESPONSE SHAPE rather than the
// sentence: a Problem document, a Problem content type, and — the part that
// actually failed — not a zip, and not empty.
func TestEveryExportRefusalIsAProblemAndNotAZeroByteCast(t *testing.T) {
	cases := []struct {
		name       string
		bodies     [][]byte
		wantDetail string
	}{
		{
			name: "more images than a bundle may carry",
			bodies: func() [][]byte {
				out := make([][]byte, castbundle.MaxAssets+1)
				for i := range out {
					out[i] = []byte{byte(i), byte(i >> 8), byte(i >> 16)}
				}
				return out
			}(),
			wantDetail: "more than the 512 a cast bundle can carry",
		},
		{
			name:       "an image larger than one bundle entry may be",
			bodies:     [][]byte{bigAsset(castbundle.MaxAssetBytes+1, 0xE7)},
			wantDetail: "more than the 67108864-byte limit one bundle entry may be",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := origin.New()
			e := newEnvWithContent(t, content)
			castID, _ := seedCastWithAssets(t, e, content, "Refused export — "+tc.name, tc.bodies)

			resp, raw := e.do(t, http.MethodGet, "/api/v1/casts/"+castID+"/export", nil, nil)

			if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/zip") {
				t.Fatalf("the export answered %d %s with %d body bytes: a refusal reached the operator as a %s file, which their destination box will blame on itself",
					resp.StatusCode, ct, len(raw), resp.Header.Get("Content-Disposition"))
			}
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("export = %d, want 409 (body %q)", resp.StatusCode, raw[:min(len(raw), 300)])
			}
			if len(raw) == 0 {
				t.Fatal("the refusal carried no body at all — the operator is told nothing")
			}
			assertProblem(t, resp, raw, "CONFLICT")
			var p map[string]any
			mustUnmarshal(t, raw, &p)
			detail, _ := p["detail"].(string)
			if !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q — the refusal must name what the operator actually hit", detail, tc.wantDetail)
			}
			if !strings.Contains(detail, "workspace archive") {
				t.Errorf("detail = %q, want it to name the instrument that CAN move this design", detail)
			}
		})
	}
}

// TestExportAndImportAgreeOnOneBundleLimit is the pin that stops the two halves
// drifting apart again, and it is deliberately behavioural rather than a
// comparison of constants: it proves the IMPORT ROUTE accepts a body larger than
// the single-upload ceiling, which is the specific number it used to be capped
// at.
//
// A bundle at that size is refused for being the wrong FILE, not for being too
// big — which is the whole distinction the old behaviour collapsed.
func TestExportAndImportAgreeOnOneBundleLimit(t *testing.T) {
	e := newEnv(t)
	node := seedSchedulingScope(t, e)

	// Not a zip, and one byte past the old cap.
	body := bigAsset(castbundle.MaxAssetBytes+1, 0xC1)
	resp, raw := importBundle(t, e, body, node, "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a %d-byte body was answered %d; want 422 (refused as not-a-bundle). A 400 here means the route is still capped at the single-upload ceiling.",
			len(body), resp.StatusCode)
	}
	var p map[string]any
	mustUnmarshal(t, raw, &p)
	detail, _ := p["detail"].(string)
	if strings.Contains(detail, "exceeds the") {
		t.Fatalf("detail = %q — the import refused a bundle-sized body on SIZE. Export and import must bound the same number, or a bundle this box produced is one it cannot read.", detail)
	}
	if !strings.Contains(detail, "not a cast bundle") {
		t.Errorf("detail = %q, want the not-a-bundle refusal", detail)
	}
}

// TestImportingACastStaysInsideItsAllocationBudget is the IMPORT half of the
// same availability question, and it exists because the export got a measured
// budget and the import got a paragraph.
//
// The import cannot stream: castbundle.Read verifies every asset's hash against
// the reference it is carried under before anything is written, and a hash over
// bytes you have not kept is a hash you cannot check. So the whole file really
// is resident, and the honest thing is to know by how much rather than to guess:
//
//	1× the request body   readBodyLimit buffers it (io.ReadAll, which grows by
//	                      doubling — the transient peak is larger than the file)
//	1× the decoded assets castbundle.Read materialises each entry to verify it
//	0× the origin         origin.Add RETAINS the slice Read produced rather than
//	                      copying it, which is the one place this is cheap
//
// The budget below is stated as a multiple of the BUNDLE, measured, with
// headroom for the doubling. It is a smoke alarm, not a specification: what it
// catches is a future import that adds another whole copy — a re-encode, a
// defensive `append([]byte(nil), …)`, a second parse — on a Pi-class appliance
// where 136 MiB is already most of the machine.
func TestImportingACastStaysInsideItsAllocationBudget(t *testing.T) {
	sourceContent := origin.New()
	source := newEnvWithContent(t, sourceContent)
	castID, _ := seedCastWithAssets(t, source, sourceContent, "One full-size image",
		[][]byte{bigAsset(castbundle.MaxAssetBytes, 0xF3)})
	bundle := exportBundle(t, source, castID)

	dest := newEnvWithContent(t, origin.New())
	node := seedSchedulingScope(t, dest)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	resp, raw := importBundle(t, dest, bundle, node, "")
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("import = %d, want 201 (body %q)", resp.StatusCode, raw[:min(len(raw), 300)])
	}
	// Six copies of the bundle. Measured at a very steady 4.9× (five runs, all
	// within 0.02%): two doubling-growth reads of ~2× each, plus change. The
	// slack is one fifth of a copy, which is enough for a runner that chunks the
	// body differently and nowhere near enough to hide another whole one.
	budget := uint64(len(bundle)) * 6
	if allocated > budget {
		t.Fatalf("importing a %d-byte bundle allocated %d bytes (budget %d, %.1f× the bundle): the import has grown another whole copy of the file",
			len(bundle), allocated, budget, float64(allocated)/float64(len(bundle)))
	}
	t.Logf("import of a %d-byte bundle allocated %d bytes (%.1f× the bundle)", len(bundle), allocated, float64(allocated)/float64(len(bundle)))
}

// TestExportingACastDoesNotHoldTheWholeBundleInMemory is the availability half
// of the size finding, and it is measured rather than asserted in a comment.
//
// The export used to assemble the whole zip in a bytes.Buffer and then write it
// out, so a bundle at the content ceiling cost a second full copy of every asset
// on a box whose content origin already holds them all resident — and
// bytes.Buffer's growth doubling makes the transient cost larger still. On a
// Pi-class appliance that is an OOM an authenticated caller can trigger with one
// GET.
//
// The response is DISCARDED as it arrives rather than read into a slice: this
// test runs the client in the same process as the server, so a client that
// buffered the body would swamp the very measurement being taken.
func TestExportingACastDoesNotHoldTheWholeBundleInMemory(t *testing.T) {
	content := origin.New()
	e := newEnvWithContent(t, content)
	// A cast at the content ceiling — the largest export this box will produce.
	bodies := [][]byte{
		bigAsset(castbundle.MaxAssetBytes, 0xD1),
		bigAsset(castbundle.MaxAssetBytes, 0xD2),
	}
	castID, _ := seedCastWithAssets(t, e, content, "Two full-size layers", bodies)

	req, err := http.NewRequest(http.MethodGet, e.ts.URL+"/api/v1/casts/"+castID+"/export", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	e.auth.Authorize(req)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	written, err := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("stream the export: %v", err)
	}

	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export = %d, want 200", resp.StatusCode)
	}
	if written <= castbundle.MaxAssetBytes {
		t.Fatalf("the export was only %d bytes; this test needs a bundle larger than one asset to mean anything", written)
	}
	// Generous by design: the point is the ORDER OF MAGNITUDE. Streaming
	// allocates copy buffers; buffering allocates the bundle, twice over as the
	// buffer grows.
	if budget := uint64(castbundle.MaxAssetBytes); allocated > budget {
		t.Fatalf("streaming a %d-byte export allocated %d bytes (budget %d): the handler is holding the bundle in memory instead of writing it to the socket",
			written, allocated, budget)
	}
}
