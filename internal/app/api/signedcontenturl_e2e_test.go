package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
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
