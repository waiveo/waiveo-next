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
	"net/http"
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
	if err := castbundle.Write(&buf, castbundle.Manifest{Cast: castbundle.CastPayload{
		Name: "Off canvas",
		Slides: []datamodel.CastSlide{{ID: "s1", Layers: []wire.Layer{
			{Kind: wire.LayerKindRect, X: 9000, Y: 9000, W: 100, H: 100, Color: "#112233"},
		}}},
	}}, map[string][]byte{}); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	before := castCount(t, dest)
	resp, raw := importBundle(t, dest, buf.Bytes(), destNode, "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("import of an off-canvas slide = %d, want 422 (body %s)", resp.StatusCode, raw)
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
