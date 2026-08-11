package snapshot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// signedcontenturl_e2e_test.go drives the two halves of content delivery against
// EACH OTHER: a snapshot built the way the running feeder builds it, and the
// real content origin serving the URLs that snapshot carries.
//
// It exists because both halves were separately, thoroughly tested and the
// system was still completely broken. The origin's tests proved it accepts a URL
// contenturl minted; this package's tests proved it stamps a `url` of the
// expected shape; the relay's proved its own minting round-trips. Nothing
// anywhere fetched an APP-AUTHORED url from an origin that was actually
// enforcing signatures — which is the only question a screen ever asks — so
// every `image` and `video` layer on every screen answered 403, through every
// gate, two waves of adversarial review, and a hardware validation pass that
// happened to use text-only slides.
//
// The bar here is therefore deliberately not "the url has a sig parameter".

// e2eContentKey is the origin's signing key. Any 32 bytes; what matters is that
// ONE value reaches both the origin and — via origin.Store.Signer — the builder.
const e2eContentKey = "a-32-byte-test-key-for-hmac-0001"

// e2eOrigin is the content-origin base every URL in these tests is stated
// against. The origin handler is mounted at the path, so the host half only has
// to be a parseable URL.
const e2eOrigin = "https://origin.example"

// e2eFixture is one store wired to one content origin, with every content-
// bearing projection this package has exercised at once.
type e2eFixture struct {
	store  *store.Store
	origin *origin.Store
	nowMs  int64
	// bytesByRef is what each asset_ref really resolves to, so a 200 can be
	// checked for the RIGHT bytes rather than merely for a 200.
	bytesByRef map[string][]byte
}

// newE2EFixture seeds a store whose delivered program exercises every shape that
// puts a content URL on the wire, and an origin holding the real bytes for each:
//
//   - a plain `asset` playlist item (an image),
//   - a plain `asset` playlist item carrying content_type `video`,
//   - an inline `slide` item with an `image` layer,
//   - a `cast` item whose slides carry an `image` layer and a `video` layer,
//
// which between them cover both kinds wire.LayerFetchesContent names and both
// item sources that carry an asset_ref. The clock is the seeded content daypart's
// midday, so the seeded schedule resolves to display:content.
func newE2EFixture(t *testing.T) *e2eFixture {
	t.Helper()
	at := contentInstant(t)

	imgBytes := []byte("e2e-image-bytes")
	vidBytes := []byte("e2e-video-bytes")
	slideImgBytes := []byte("e2e-inline-slide-image-bytes")
	castVidBytes := []byte("e2e-cast-video-bytes")

	// The origin's own clock is the instant the snapshot is built at, because
	// that is the wiring the feeder has: one clock (the app clock floor) feeds
	// the origin, the deadline, and the resolution instant alike.
	oc := origin.New(
		origin.WithSigningKey([]byte(e2eContentKey)),
		origin.WithClock(func() int64 { return at }),
	)

	f := &e2eFixture{origin: oc, nowMs: at, bytesByRef: map[string][]byte{}}
	add := func(b []byte) string {
		t.Helper()
		ref, err := oc.Add(b)
		if err != nil {
			t.Fatalf("origin.Add: %v", err)
		}
		f.bytesByRef[ref] = b
		return ref
	}
	imgRef := add(imgBytes)
	vidRef := add(vidBytes)
	slideImgRef := add(slideImgBytes)
	castVidRef := add(castVidBytes)

	s := seededStore(t, imgRef)
	f.store = s

	cast := writeCast(t, s, datamodel.Cast{
		ID: "01J8ZE2ECAST000000000000CT", ScopeNode: castScopeNode, Name: "E2E Cast",
		Slides: []datamodel.CastSlide{
			{ID: "photo", DurationMS: 5000, Layers: []wire.Layer{
				{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: imgRef},
			}},
			{ID: "clip", DurationMS: 7000, Layers: []wire.Layer{
				{Kind: wire.LayerKindVideo, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: castVidRef},
			}},
		},
	})

	replaceSeedPlaylistItems(t, s, []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceAsset, AssetRef: imgRef},
		{Source: datamodel.PlaylistSourceAsset, AssetRef: vidRef, ContentType: datamodel.PlaylistContentTypeVideo},
		{Source: sourceSlide, Slide: &datamodel.Slide{Layers: []wire.Layer{
			{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#101828"},
			{Kind: wire.LayerKindImage, X: 200, Y: 200, W: 800, H: 600, AssetRef: slideImgRef},
		}}},
		{Source: datamodel.PlaylistSourceCast, CastID: cast},
	})
	return f
}

// build produces the signed snapshot exactly as the feeder does: the signer
// comes from the ORIGIN, so there is no opportunity for the test to pair a
// builder key with a different verifier key and prove nothing.
func (f *e2eFixture) build(t *testing.T) SignedSnapshot {
	t.Helper()
	snap, degrades, err := BuildFromStore(
		desiredState(t, f.store),
		f.origin.Signer(e2eOrigin, contenturl.SnapshotTTL),
		testIdentity(t),
		f.nowMs,
	)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	if len(degrades) != 0 {
		t.Fatalf("unexpected degrades over a well-formed store: %+v", degrades)
	}
	return snap
}

// fetch asks the REAL origin handler for rawURL, the way a screen would, and
// returns the status and body.
func (f *e2eFixture) fetch(t *testing.T, rawURL string) (int, []byte) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("the snapshot carried an unparseable url %q: %v", rawURL, err)
	}
	rec := httptest.NewRecorder()
	f.origin.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
	return rec.Code, rec.Body.Bytes()
}

// contentURLPattern matches any string that is a content-origin fetch URL,
// FOUND STRUCTURALLY rather than by field name.
//
// Matching on the shape rather than on `url` keys is the point: a future
// producer that puts a fetch URL somewhere new — a second content array, a
// per-layer poster, a field nobody has thought of — still lands in the sweep, and
// a producer that renames a field does not quietly leave it.
var contentURLPattern = regexp.MustCompile(`^https?://[^\s"]+` + regexp.QuoteMeta(contenturl.PathPrefix) + `[^\s"]+$`)

// everyContentURL walks a marshaled snapshot's whole JSON tree and returns every
// content URL in it, with the JSON path it was found at (for the failure
// message). It descends objects and arrays without knowing any field name.
func everyContentURL(t *testing.T, snap SignedSnapshot) map[string]string {
	t.Helper()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	found := map[string]string{}
	var walk func(path string, n any)
	walk = func(path string, n any) {
		switch v := n.(type) {
		case map[string]any:
			for k, child := range v {
				walk(path+"."+k, child)
			}
		case []any:
			for i, child := range v {
				walk(path+"["+itoa(i)+"]", child)
			}
		case string:
			if contentURLPattern.MatchString(v) {
				found[v] = path
			}
		}
	}
	walk("$", tree)
	return found
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestEveryContentURLTheSnapshotCarriesIsFetchableAtTheOrigin is the
// end-to-end proof, and the anti-regression invariant, in one assertion:
// EVERY url anywhere in a built generation is fetched from the real origin and
// must answer 200 with the right bytes.
//
// The quantifier is what makes this a class guard rather than a check on today's
// four call sites. The URLs are discovered by walking the snapshot's JSON for
// anything shaped like a content-origin fetch (contentURLPattern), so a NEW
// producer — a new item source, a new content-bearing layer kind, a second
// content array on some future section — is swept in the moment its output
// reaches the wire, with no test edit. If that producer forgets to sign, this
// fails with a 403 naming the JSON path the offending url sits at.
//
// Its complement is TestEveryContentURLIsMintedByThisPackage over in
// internal/feeder/contenturl, which catches the producer that never reaches this
// fixture at all. Neither subsumes the other: this one cannot see a route the
// fixture does not drive, and that one cannot see a producer that assembles the
// grammar some way other than the literal.
func TestEveryContentURLTheSnapshotCarriesIsFetchableAtTheOrigin(t *testing.T) {
	f := newE2EFixture(t)
	snap := f.build(t)

	urls := everyContentURL(t, snap)

	// A sweep over nothing passes vacuously, so the fixture's own coverage is
	// asserted first: four distinct assets, reached through four distinct
	// projections (plain asset, plain video asset, inline slide layer, cast
	// slide layers).
	if len(urls) < 4 {
		t.Fatalf("the fixture produced only %d content url(s) — the sweep below would prove almost nothing; urls=%v", len(urls), urls)
	}
	seenRefs := map[string]bool{}
	for u := range urls {
		seenRefs[digestOf(u)] = true
	}
	for ref := range f.bytesByRef {
		if !seenRefs[strings.TrimPrefix(ref, "sha256:")] {
			t.Errorf("the fixture stored asset %s but no url for it reached the snapshot — a projection dropped it, so this sweep is not covering the shape it was meant to", ref)
		}
	}

	for u, path := range urls {
		code, body := f.fetch(t, u)
		if code != http.StatusOK {
			t.Errorf("the origin answered %d for the url the snapshot carries at %s (%q).\n"+
				"This is the HV-1 shape: the app authored a content url the very origin that stores the bytes refuses. "+
				"A url that reaches a screen MUST be minted by contenturl under the origin's own key.", code, path, u)
			continue
		}
		want, ok := f.bytesByRef["sha256:"+digestOf(u)]
		if !ok {
			t.Errorf("url at %s (%q) resolved to a digest this fixture never stored", path, u)
			continue
		}
		if string(body) != string(want) {
			t.Errorf("url at %s served %q, want %q", path, body, want)
		}
	}
}

// digestOf extracts the hex digest from a content URL's path.
func digestOf(rawURL string) string {
	_, after, ok := strings.Cut(rawURL, contenturl.PathPrefix)
	if !ok {
		return ""
	}
	hex, _, _ := strings.Cut(after, "?")
	return hex
}

// TestAPinnedOverridesContentIsFetchableToo covers the path the defect was
// actually found on: push-now writes a screen override, and a pinned override's
// program is the one the relay serves through UNMODIFIED
// (playerserver.SetServedProgram) rather than re-resolving — so the app-minted
// url is the only one that screen will ever be handed.
func TestAPinnedOverridesContentIsFetchableToo(t *testing.T) {
	f := newE2EFixture(t)

	pushBytes := []byte("e2e-pushed-cast-bytes")
	pushRef, err := f.origin.Add(pushBytes)
	if err != nil {
		t.Fatalf("origin.Add: %v", err)
	}
	f.bytesByRef[pushRef] = pushBytes

	pushCast := writeCast(t, f.store, datamodel.Cast{
		ID: "01J8ZE2EPSHCAST00000000001", ScopeNode: castScopeNode, Name: "Pushed",
		Slides: []datamodel.CastSlide{{ID: "notice", DurationMS: 5000, Layers: []wire.Layer{
			{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: pushRef},
		}}},
	})
	pushOverride(t, f.store, store.SeedScreenID, &datamodel.ScreenOverride{
		Mode: datamodel.ScreenOverrideModeAlert, CastID: pushCast,
	})

	prog := programForScreen(t, f.build(t).Sections.ScreenPrograms, store.SeedScreenID)
	if !prog.Pinned {
		t.Fatalf("the override did not take: program = %+v", prog)
	}
	if len(prog.Content) != 1 || len(prog.Content[0].Layers) != 1 {
		t.Fatalf("pushed program = %+v, want one slide carrying one image layer", prog.Content)
	}
	layerURL := prog.Content[0].Layers[0].URL
	if code, body := f.fetch(t, layerURL); code != http.StatusOK || string(body) != string(pushBytes) {
		t.Fatalf("the pushed slide's image answered %d (%q) at %q — a push-now that a screen cannot fetch is the whole defect",
			code, body, layerURL)
	}
}

// TestTheWireDeadlineIsTheOneTheURLActuallyDies_At: `expires_at` used to be a
// hardcoded 0 with a doc claiming the origin served without expiry, which
// stopped being true when signing landed. A deadline on the wire that does not
// match the deadline in the signature is worse than no deadline: PLY-086 has a
// player refuse to fetch a url past its `expires_at`, so the two must be one
// number.
func TestTheWireDeadlineIsTheOneTheURLActuallyDies_At(t *testing.T) {
	f := newE2EFixture(t)
	prog := programForScreen(t, f.build(t).Sections.ScreenPrograms, store.SeedScreenID)
	if len(prog.Content) == 0 {
		t.Fatal("no content to check")
	}

	item := prog.Content[0]
	if item.ExpiresAt == 0 {
		t.Fatal("expires_at is still 0 on a SIGNED content reference — the wire is telling a screen the url never dies while the signature says otherwise")
	}
	if want := f.nowMs + contenturl.SnapshotTTL.Milliseconds(); item.ExpiresAt != want {
		t.Errorf("expires_at = %d, want %d (nowMs + SnapshotTTL)", item.ExpiresAt, want)
	}

	// And the number is TRUE of the origin, not merely present: at the stated
	// instant the url still serves; one millisecond later it does not.
	at := func(ms int64) int {
		t.Helper()
		oc := f.origin
		_ = oc
		u, err := url.Parse(item.URL)
		if err != nil {
			t.Fatalf("parse %q: %v", item.URL, err)
		}
		clocked := origin.New(
			origin.WithSigningKey([]byte(e2eContentKey)),
			origin.WithClock(func() int64 { return ms }),
		)
		for _, b := range f.bytesByRef {
			if _, err := clocked.Add(b); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		rec := httptest.NewRecorder()
		clocked.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
		return rec.Code
	}
	if got := at(item.ExpiresAt); got != http.StatusOK {
		t.Errorf("at the stated expires_at the origin answered %d, want 200 — the deadline is inclusive of its own instant", got)
	}
	if got := at(item.ExpiresAt + 1); got != http.StatusForbidden {
		t.Errorf("one ms after the stated expires_at the origin answered %d, want 403 — the wire deadline must be the real one", got)
	}
}

// TestTheSnapshotTTLOutlivesAnAuthoringLull is the sizing argument, asserted.
//
// The app mints at BUILD time and the built generation is cached by store
// generation, so it is served for as long as nobody authors anything. A TTL
// chosen against the player's poll interval (seconds) or against the relay's own
// serve-time lifetime (a day) would expire mid-deployment on any site that goes
// a weekend without an edit — the same P0 in slow motion. This pins the property
// rather than the number, so a future change to SnapshotTTL has to keep it.
func TestTheSnapshotTTLOutlivesAnAuthoringLull(t *testing.T) {
	const aFortnight = 14 * 24 * time.Hour
	if contenturl.SnapshotTTL < aFortnight {
		t.Errorf("SnapshotTTL is %s — shorter than a fortnight. A snapshot is re-minted only when an api write advances "+
			"the store generation, so a site nobody edits for that long serves urls minted that long ago, and every one of "+
			"them 403s.", contenturl.SnapshotTTL)
	}
	if contenturl.SnapshotTTL <= contenturl.ServeTTL {
		t.Errorf("SnapshotTTL (%s) is not longer than ServeTTL (%s) — a deadline fixed at build time cannot be sized like "+
			"one minted at serve time.", contenturl.SnapshotTTL, contenturl.ServeTTL)
	}
}

// TestRebuildingTheSameProgramReproducesItsRevision guards the cost of signing.
//
// A minted url carries `exp` and `sig` that differ on every build BY DESIGN, so
// digesting the url whole would churn program_revision on every rebuild — and a
// player treats a changed revision as a new program and restarts the rotation
// (PLY-090/108). Signing would then make every unrelated authored write visibly
// restart every screen in the site.
func TestRebuildingTheSameProgramReproducesItsRevision(t *testing.T) {
	f := newE2EFixture(t)
	first := programForScreen(t, f.build(t).Sections.ScreenPrograms, store.SeedScreenID)

	// A write that touches nothing this screen plays: it advances the store
	// generation, so the snapshot is genuinely rebuilt and its urls re-minted.
	createRow(t, f.store, store.KindPlaylist, datamodel.Playlist{
		ID: "01J8ZE2ENREFP1AY11ST000001", ScopeNode: castScopeNode, Name: "Unreferenced",
		Items: []datamodel.PlaylistItem{{Source: datamodel.PlaylistSourceAsset, AssetRef: signhash.ContentID([]byte("e2e-image-bytes"))}},
	})
	second := programForScreen(t, f.build(t).Sections.ScreenPrograms, store.SeedScreenID)

	if first.ProgramRevision != second.ProgramRevision {
		t.Errorf("program_revision churned (%q -> %q) across a rebuild that changed nothing this screen plays — every "+
			"screen on the site would restart its rotation on every unrelated write", first.ProgramRevision, second.ProgramRevision)
	}
	// The premise: the urls really were re-minted, so the invariant above is
	// doing work rather than describing a build that produced identical bytes.
	if first.Content[0].URL == "" || second.Content[0].URL == "" {
		t.Fatal("no url to compare; the fixture is not exercising minting")
	}
}
