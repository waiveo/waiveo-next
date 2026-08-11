package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// signedcontenturl_e2e_test.go is the exact observation that opened HV-1,
// turned into a test: upload an asset, then fetch the url the 201 handed back —
// against an origin configured the way EVERY real deployment's is.
//
// The pre-existing round-trip test next door (TestContentUploadRoundTrip) drives
// the same two steps and passed throughout, because its origin holds no signing
// key. cmd/waiveo-feeder loads-or-creates that key unconditionally, so no
// deployment has ever run without one; the one configuration the tests covered
// was the one nothing runs in. On real hardware the same two steps answered
// `201 {asset_ref, url}` and then `403 Forbidden` for that exact url.

// signingContentKey is the key the origin verifies under in these tests. The
// api never receives it as a parameter — it reads it back off the store through
// origin.Store.Signer, which is the whole point.
const signingContentKey = "a-32-byte-test-key-for-hmac-0001"

// newSigningEnv is newEnv with the one difference every real deployment has: the
// content origin ENFORCES signed URLs.
func newSigningEnv(t *testing.T) *testEnv {
	t.Helper()
	return newEnvWith(t, origin.New(
		origin.WithSigningKey([]byte(signingContentKey)),
		origin.WithClock(func() int64 { return fixedNowMs }),
	), nil)
}

// fetchThroughOrigin asks the REAL origin handler for rawURL exactly as a screen
// or a browser would, and returns the status and body.
func fetchThroughOrigin(t *testing.T, e *testEnv, rawURL string) (int, []byte) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("the api returned an unparseable url %q: %v", rawURL, err)
	}
	rec := httptest.NewRecorder()
	e.content.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
	return rec.Code, rec.Body.Bytes()
}

// TestTheURLAnUploadReturnsIsFetchable is HV-1's headline: an upload api that
// hands back a url the very same process refuses.
func TestTheURLAnUploadReturnsIsFetchable(t *testing.T) {
	e := newSigningEnv(t)
	asset := []byte("the bytes an operator actually uploaded")

	resp, raw := e.do(t, http.MethodPost, "/api/v1/content", asset, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	var got struct {
		AssetRef string `json:"asset_ref"`
		URL      string `json:"url"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode upload response: %v (body %s)", err, raw)
	}
	if got.URL == "" {
		t.Fatal("the upload returned no url at all")
	}

	code, body := fetchThroughOrigin(t, e, got.URL)
	if code != http.StatusOK {
		t.Fatalf("fetching the url the upload just returned (%q) answered %d — an upload that hands a caller an "+
			"unfetchable url is the whole of HV-1", got.URL, code)
	}
	if string(body) != string(asset) {
		t.Fatalf("the url served %q, want the uploaded bytes %q", body, asset)
	}
}

// TestEveryListedAssetsURLIsFetchable covers the read half: the media library an
// authoring surface renders. Unsigned, every thumbnail in the console was a
// broken image against a real origin.
func TestEveryListedAssetsURLIsFetchable(t *testing.T) {
	e := newSigningEnv(t)
	uploaded := map[string][]byte{}
	for _, b := range [][]byte{[]byte("library asset one"), []byte("library asset two"), []byte("library asset three")} {
		resp, raw := e.do(t, http.MethodPost, "/api/v1/content", b, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload status = %d (body %s)", resp.StatusCode, raw)
		}
		var got struct {
			AssetRef string `json:"asset_ref"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode upload response: %v", err)
		}
		uploaded[strings.TrimPrefix(got.AssetRef, "sha256:")] = b
	}

	resp, raw := e.do(t, http.MethodGet, "/api/v1/content", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d (body %s)", resp.StatusCode, raw)
	}
	var listed struct {
		Content []struct {
			AssetRef string `json:"asset_ref"`
			URL      string `json:"url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatalf("decode listing: %v (body %s)", err, raw)
	}
	if len(listed.Content) != len(uploaded) {
		t.Fatalf("listing carried %d rows, want %d", len(listed.Content), len(uploaded))
	}
	for _, row := range listed.Content {
		code, body := fetchThroughOrigin(t, e, row.URL)
		if code != http.StatusOK {
			t.Errorf("listing row %s carried url %q, which the origin answered %d", row.AssetRef, row.URL, code)
			continue
		}
		want := uploaded[strings.TrimPrefix(row.AssetRef, "sha256:")]
		if string(body) != string(want) {
			t.Errorf("listing row %s served %q, want %q", row.AssetRef, body, want)
		}
	}
}

// TestASigningOriginStillRefusesAnUnsignedURL is the control. Without it, an
// origin that had quietly stopped enforcing anything would satisfy both tests
// above, and they would prove exactly what the pre-existing round-trip test
// proved: nothing about signing.
func TestASigningOriginStillRefusesAnUnsignedURL(t *testing.T) {
	e := newSigningEnv(t)
	asset := []byte("the bytes an operator actually uploaded")
	resp, raw := e.do(t, http.MethodPost, "/api/v1/content", asset, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d (body %s)", resp.StatusCode, raw)
	}
	var got struct {
		AssetRef string `json:"asset_ref"`
		URL      string `json:"url"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	bare, _, _ := strings.Cut(got.URL, "?")
	if bare == got.URL {
		t.Fatalf("the upload returned %q with no signature at all — nothing was minted", got.URL)
	}
	if code, _ := fetchThroughOrigin(t, e, bare); code != http.StatusForbidden {
		t.Fatalf("the origin served the UNSIGNED form of the same url with %d, want 403 — it is not enforcing anything, "+
			"so the tests above prove nothing", code)
	}
}

// TestAKeylessOriginKeepsHandingOutUnsignedURLs pins the other side of the
// no-key decision, which is the one that must not become a landmine.
//
// An empty key means THIS DEPLOYMENT DOES NOT SIGN — and it is only safe to mint
// an unsigned url on that basis because the key is read back off the very origin
// that will (or will not) verify it. The two ends agree by construction, so a
// keyless origin's unsigned url is fetchable rather than a 403 waiting to
// happen; the pre-existing keyless fixtures depend on exactly this.
func TestAKeylessOriginKeepsHandingOutUnsignedURLs(t *testing.T) {
	e := newEnv(t)
	asset := []byte("bytes on a deployment with no key")
	resp, raw := e.do(t, http.MethodPost, "/api/v1/content", asset, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d (body %s)", resp.StatusCode, raw)
	}
	var got struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if strings.Contains(got.URL, "?") {
		t.Errorf("a keyless origin's url carries a query (%q) — it was signed under a key its own verifier does not hold", got.URL)
	}
	if !strings.Contains(got.URL, contenturl.PathPrefix) {
		t.Errorf("url = %q, want the %s grammar", got.URL, contenturl.PathPrefix)
	}
	if code, body := fetchThroughOrigin(t, e, got.URL); code != http.StatusOK || string(body) != string(asset) {
		t.Errorf("a keyless origin refused its own unsigned url: %d %q", code, body)
	}
}

// ── The authoring surface's own copy of the same defect ──────────────────────
//
// HV-1's fix made every content url an EXPIRING capability, and the console's
// media picker hands the Studio one of those (GET /content, minted with
// contenturl.ServeTTL). The Studio patched it into the layer and the save sent
// it; nothing on the server side removed it, so the cast row PERSISTED a url
// that dies within a day.
//
// That never reaches a screen — both projections re-mint every content-bearing
// layer's url from the asset_ref — so it is authoring-surface-only. It is
// nonetheless a regression this feature introduced: on main the persisted url
// was permanent, and after it the operator who builds a cast today and reopens
// it tomorrow gets a canvas of broken images and a properties panel showing a
// dead link, with nothing anywhere saying why.
//
// The tests below pin the server half (a derived member is not stored) and the
// live half (the listing is where a fetchable url comes from, on demand).

// uploadAsset posts bytes and returns the {asset_ref, url} pair the console
// works from — the same response the media picker reads.
func (e *testEnv) uploadAsset(t *testing.T, b []byte) (assetRef, url string) {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/content", b, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d (body %s)", resp.StatusCode, raw)
	}
	var got struct {
		AssetRef string `json:"asset_ref"`
		URL      string `json:"url"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode upload response: %v (body %s)", err, raw)
	}
	return got.AssetRef, got.URL
}

// TestACastDoesNotStoreTheDerivedLayerURL drives the Studio's exact sequence —
// upload, pick, patch {asset_ref, url} into the layer, save — and requires the
// stored cast to carry the asset_ref and NOT the url.
func TestACastDoesNotStoreTheDerivedLayerURL(t *testing.T) {
	e := newSigningEnv(t)
	scope := seedSchedulingScope(t, e)

	poster := []byte("the poster an operator uploaded from the Studio")
	assetRef, picked := e.uploadAsset(t, poster)

	// The premise, asserted: the url the picker hands over really is a capability
	// that DIES. Against a keyless origin it would be a permanent address, this
	// whole test would be about tidiness, and it would prove nothing about the
	// regression.
	if !strings.Contains(picked, contenturl.QueryExpires+"=") {
		t.Fatalf("PREMISE FALSE: the picked url %q carries no %s — it is not an expiring capability, so storing it "+
			"would not rot and this test is not exercising the defect", picked, contenturl.QueryExpires)
	}
	if code, _ := fetchThroughOrigin(t, e, picked); code != http.StatusOK {
		t.Fatalf("PREMISE FALSE: the picked url answered %d today; it must be good NOW and stale LATER", code)
	}
	if code := fetchAfterDeadline(t, picked, poster); code != http.StatusForbidden {
		t.Fatalf("PREMISE FALSE: the picked url still answered %d past its own deadline, so a persisted copy of it "+
			"would never go stale and this test would pass for the wrong reason", code)
	}

	authored := datamodel.Cast{
		ScopeNode: scope,
		Name:      "Lunch Menu",
		Slides: []datamodel.CastSlide{{
			ID: "photo", DurationMS: 6000,
			Layers: []wire.Layer{
				{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#101828"},
				// Exactly what studio-route.tsx's picker used to patch in:
				// asset_ref AND url together.
				{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: assetRef, URL: picked},
			},
		}},
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, authored), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /casts: status %d, body %s", resp.StatusCode, raw)
	}
	created := decodeCast(t, raw)
	assertNoStoredLayerURL(t, created, "the create response")

	// And in the STORE, not merely in the response the writer happened to get
	// back: the response is composed from the persisted bytes, but a reader
	// tomorrow is the one this is for.
	_, getRaw := e.do(t, http.MethodGet, "/api/v1/casts/"+created.ID, nil, nil)
	assertNoStoredLayerURL(t, decodeCast(t, getRaw), "a later GET")

	// The asset_ref — the half that IS authored — must survive untouched, or the
	// strip has simply broken the layer.
	if got := decodeCast(t, getRaw).Slides[0].Layers[1].AssetRef; got != assetRef {
		t.Errorf("the stored layer's asset_ref = %q, want %q — the derived half was dropped and the authored half with it", got, assetRef)
	}

	// The live half: the console re-resolves from the listing, so the SAME
	// asset_ref must be fetchable there right now.
	if !listingCarriesFetchableURLFor(t, e, assetRef, poster) {
		t.Errorf("the content listing offers no fetchable url for %s — the console has nothing to render the stored "+
			"asset_ref from, which is the reason the derived url may be dropped at all", assetRef)
	}
}

// TestAPatchDoesNotStoreTheDerivedLayerURL is the same rule on the verb the
// Studio actually saves with. A console read-modify-write echoes the whole
// document back, so a patch is where a derived member most easily arrives — and
// a strip that covered only create would leave every EDIT of a cast re-poisoning
// the row.
func TestAPatchDoesNotStoreTheDerivedLayerURL(t *testing.T) {
	e := newSigningEnv(t)
	scope := seedSchedulingScope(t, e)
	poster := []byte("a poster picked during an edit")
	assetRef, picked := e.uploadAsset(t, poster)

	id := e.createCast(t, scope)
	_, raw := e.do(t, http.MethodGet, "/api/v1/casts/"+id, nil, nil)
	etag := castETag(t, e, id)

	patched := datamodel.Cast{Slides: []datamodel.CastSlide{{
		ID: "photo", DurationMS: 6000,
		Layers: []wire.Layer{
			{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: assetRef, URL: picked},
		},
	}}}
	body, err := json.Marshal(map[string]any{"slides": patched.Slides})
	if err != nil {
		t.Fatalf("marshal patch: %v", err)
	}
	resp, praw := e.do(t, http.MethodPatch, "/api/v1/casts/"+id, body, map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /casts/%s: status %d, body %s (read body was %s)", id, resp.StatusCode, praw, raw)
	}
	assertNoStoredLayerURL(t, decodeCast(t, praw), "the patch response")

	_, getRaw := e.do(t, http.MethodGet, "/api/v1/casts/"+id, nil, nil)
	assertNoStoredLayerURL(t, decodeCast(t, getRaw), "a later GET")
}

// assertNoStoredLayerURL fails when any layer of any slide carries a `url`.
//
// It asks about EVERY layer rather than the one the test wrote, because the rule
// is about the member and not about a position: a strip that missed the second
// slide, or the second layer, would leave exactly the same operator staring at
// exactly the same broken image.
func assertNoStoredLayerURL(t *testing.T, c datamodel.Cast, where string) {
	t.Helper()
	for i, s := range c.Slides {
		for j, l := range s.Layers {
			if l.URL == "" {
				continue
			}
			t.Errorf("%s carries slides[%d].layers[%d].url = %q.\n"+
				"A content-bearing layer's url is DERIVED from the content origin at projection time "+
				"(wire.ValidateAuthoredSlideLayers), and since HV-1 it is a signed capability that expires. Persisting "+
				"one means an operator who reopens this cast after it lapses sees a broken image on the canvas and a "+
				"dead link in the properties panel. The console re-resolves from GET /content instead.",
				where, i, j, l.URL)
		}
	}
}

// castETag reads a cast's current ETag, so a PATCH can carry the If-Match the
// surface requires. (etagOf next door is the playlist family's.)
func castETag(t *testing.T, e *testEnv, id string) string {
	t.Helper()
	resp, _ := e.do(t, http.MethodGet, "/api/v1/casts/"+id, nil, nil)
	tag := resp.Header.Get("ETag")
	if tag == "" {
		t.Fatalf("GET /casts/%s carried no ETag", id)
	}
	return tag
}

// listingCarriesFetchableURLFor reports whether GET /content offers assetRef
// with a url that serves want RIGHT NOW — the source a console resolves a stored
// asset_ref through.
func listingCarriesFetchableURLFor(t *testing.T, e *testEnv, assetRef string, want []byte) bool {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, "/api/v1/content", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d (body %s)", resp.StatusCode, raw)
	}
	var listed struct {
		Content []struct {
			AssetRef string `json:"asset_ref"`
			URL      string `json:"url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatalf("decode listing: %v (body %s)", err, raw)
	}
	for _, row := range listed.Content {
		if row.AssetRef != assetRef {
			continue
		}
		code, body := fetchThroughOrigin(t, e, row.URL)
		return code == http.StatusOK && string(body) == string(want)
	}
	return false
}

// fetchAfterDeadline serves rawURL from an origin identical to the test's own
// except that its clock stands one millisecond past the url's stated `exp`, and
// returns the status. It is how "this value goes stale" is demonstrated rather
// than asserted about a constant.
func fetchAfterDeadline(t *testing.T, rawURL string, bytes []byte) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	exp, err := strconv.ParseInt(u.Query().Get(contenturl.QueryExpires), 10, 64)
	if err != nil {
		t.Fatalf("the url %q carries no parseable %s: %v", rawURL, contenturl.QueryExpires, err)
	}
	later := origin.New(
		origin.WithSigningKey([]byte(signingContentKey)),
		origin.WithClock(func() int64 { return exp + 1 }),
	)
	if _, err := later.Add(bytes); err != nil {
		t.Fatalf("Add: %v", err)
	}
	rec := httptest.NewRecorder()
	later.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
	return rec.Code
}
