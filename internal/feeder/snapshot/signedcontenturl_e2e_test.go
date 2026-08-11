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
	return fetchAtOrigin(t, f.origin, e2eOrigin, rawURL)
}

// fetchAtOrigin serves rawURL from oc's real handler, first requiring that the
// url is actually STATED AGAINST wantBase.
//
// The prefix check is not decoration. An httptest request is built from
// url.RequestURI(), which is path+query and DISCARDS the scheme and host — so
// without this, a producer that minted `http://127.0.0.1:9/content/<hex>?…`
// against a box-local origin would be handed to this same handler, answer 200,
// and pass. That is the REL-140 violation the whole content path is written to
// refuse (a relay never fabricates its own origin), and it is precisely the kind
// of defect this file exists for: a wrong url that every green test agrees with,
// because every green test threw away the half that was wrong.
func fetchAtOrigin(t *testing.T, oc *origin.Store, wantBase, rawURL string) (int, []byte) {
	t.Helper()
	if !strings.HasPrefix(rawURL, wantBase+contenturl.PathPrefix) {
		t.Fatalf("the url %q is not stated against the content origin %q.\n"+
			"A screen fetches the ORIGIN this snapshot was built against; a url pointing anywhere else (a box-local "+
			"address, a stale base) is unreachable in the deployment that will be handed it, and this fixture would "+
			"otherwise serve it anyway because an httptest request keeps only the path and query.", rawURL, wantBase)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("unparseable content url %q: %v", rawURL, err)
	}
	rec := httptest.NewRecorder()
	oc.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
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

// TestTheSnapshotTTLOutlivesTheGapItIsMintedAcross is the sizing argument,
// asserted from this side.
//
// The app mints at BUILD time, so what its deadline must outlive is the longest
// gap between a build and a fetch made with it. That gap used to be "however
// long nobody authors anything", which is unbounded, and the answer taken was to
// stretch the deadline to thirty days — a month-long bearer capability for every
// asset a site displays, bought to cover a gap it still did not close.
//
// The gap is now bounded by the feeder itself (contenturl.SnapshotRemintInterval,
// enforced by cmd/waiveo-feeder's cacheWindowEnd), so the deadline is sized
// against THAT. Pinning it here rather than only next door to the constants is
// deliberate: this is the package whose output carries the deadline onto the
// wire, and a future change to SnapshotTTL made for a snapshot-side reason has to
// meet the bound where it is actually spent.
func TestTheSnapshotTTLOutlivesTheGapItIsMintedAcross(t *testing.T) {
	if contenturl.SnapshotTTL < 2*contenturl.SnapshotRemintInterval {
		t.Errorf("SnapshotTTL is %s, less than twice the feeder's re-mint interval (%s). A generation is served until it "+
			"is re-minted, and a relay that pulled just before a re-mint holds those urls for a full interval after it — "+
			"so anything shorter hands a screen a url that dies before its next pull.",
			contenturl.SnapshotTTL, contenturl.SnapshotRemintInterval)
	}
	// The floor is still a real duration, not merely a multiple: a re-mint
	// interval driven to microseconds would satisfy the ratio above while making
	// every relay pull re-derive and re-sign a whole snapshot.
	if contenturl.SnapshotTTL < time.Hour {
		t.Errorf("SnapshotTTL is %s. A screen prefetching a program it just pulled, retrying over a flaky link, needs "+
			"far more headroom than that, and no re-mint cadence can substitute for it.", contenturl.SnapshotTTL)
	}
}

// TestRebuildingTheSameProgramReproducesItsRevision guards the cost of signing.
//
// A minted url carries `exp` and `sig` that differ on every build BY DESIGN, so
// digesting the url whole would churn program_revision on every rebuild — and a
// player treats a changed revision as a new program and restarts the rotation
// (PLY-090/108). Signing would then make every unrelated authored write visibly
// restart every screen in the site.
//
// # The clock has to move, and the premise has to be an assertion
//
// This test shipped unable to fail. Both builds ran at the fixture's ONE fixed
// instant, so `exp` — and therefore `sig` — came out byte-identical, the two
// urls were the same string, and the revision would have matched whether or not
// screenprograms.revisionContent existed at all. A reviewer deleted the
// reduction from the digest and the whole module still passed. The guard that
// was supposed to establish the premise only checked the urls were non-EMPTY,
// under a comment claiming it proved they had been re-minted.
//
// So the second build is made a second later, and "the urls really did change"
// is now asserted rather than described. Advancing the clock by a second cannot
// move the schedule (the fixture's instant is the middle of the seeded content
// daypart), so the only thing that differs between the two builds is the mint.
func TestRebuildingTheSameProgramReproducesItsRevision(t *testing.T) {
	f := newE2EFixture(t)
	first := programForScreen(t, f.build(t).Sections.ScreenPrograms, store.SeedScreenID)

	// A write that touches nothing this screen plays: it advances the store
	// generation, so the snapshot is genuinely rebuilt and its urls re-minted.
	createRow(t, f.store, store.KindPlaylist, datamodel.Playlist{
		ID: "01J8ZE2ENREFP1AY11ST000001", ScopeNode: castScopeNode, Name: "Unreferenced",
		Items: []datamodel.PlaylistItem{{Source: datamodel.PlaylistSourceAsset, AssetRef: signhash.ContentID([]byte("e2e-image-bytes"))}},
	})
	// …at a LATER instant, which is what makes the deadline — and so the
	// signature — genuinely different bytes.
	f.nowMs += 1000
	second := programForScreen(t, f.build(t).Sections.ScreenPrograms, store.SeedScreenID)

	// The premise, ASSERTED. If the two builds minted the identical url, the
	// invariant below is describing a build that produced identical bytes and
	// proves nothing about the reduction.
	if len(first.Content) == 0 || len(second.Content) == 0 {
		t.Fatal("no content to compare; the fixture is not exercising minting")
	}
	if first.Content[0].URL == second.Content[0].URL {
		t.Fatalf("PREMISE FALSE: the two builds minted the IDENTICAL url %q, so the revision below would match whether or "+
			"not the minted query is excluded from the digest. Advance the clock between the builds.", first.Content[0].URL)
	}
	if first.Content[0].ExpiresAt == second.Content[0].ExpiresAt {
		t.Fatalf("PREMISE FALSE: both builds stated the same expires_at %d — the clock did not move", first.Content[0].ExpiresAt)
	}
	// Every content-bearing LAYER too, not just the item-level url: the layer
	// stack is digested as well, and a reduction applied to one and not the other
	// would churn the revision on exactly the slides this fixture carries.
	if a, b := firstLayerURL(t, first), firstLayerURL(t, second); a == b {
		t.Fatalf("PREMISE FALSE: the two builds minted the identical LAYER url %q", a)
	}

	if first.ProgramRevision != second.ProgramRevision {
		t.Errorf("program_revision churned (%q -> %q) across a rebuild that changed nothing this screen plays — every "+
			"screen on the site would restart its rotation on every unrelated write, and every rotation would restart "+
			"from item 1 (PLY-090/108)", first.ProgramRevision, second.ProgramRevision)
	}
}

// TestChangingWhatAScreenPlaysMovesItsRevision is the other half, and the reason
// the reduction above cannot simply drop the whole content array.
//
// program_revision must NOT move while the delivered program is unchanged AND
// MUST move when it changes; a reduction sized wrong satisfies the first by
// destroying the second. A screen whose revision is frozen never swaps to the
// cast an operator just published, which is the same "the wall does not show
// what was authored" failure as HV-1, arrived at from the opposite direction.
func TestChangingWhatAScreenPlaysMovesItsRevision(t *testing.T) {
	f := newE2EFixture(t)
	before := programForScreen(t, f.build(t).Sections.ScreenPrograms, store.SeedScreenID)

	// A DIFFERENT asset in the first playlist item — same origin, same shape,
	// same everything else. What changes is the bytes the screen fetches, which
	// survives the reduction as the content-addressed path.
	swapped := []byte("e2e-image-bytes-but-different")
	swappedRef, err := f.origin.Add(swapped)
	if err != nil {
		t.Fatalf("origin.Add: %v", err)
	}
	f.bytesByRef[swappedRef] = swapped
	replaceSeedPlaylistItems(t, f.store, []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceAsset, AssetRef: swappedRef},
	})
	after := programForScreen(t, f.build(t).Sections.ScreenPrograms, store.SeedScreenID)

	if len(after.Content) == 0 || after.Content[0].AssetRef != swappedRef {
		t.Fatalf("the swap did not take: content = %+v", after.Content)
	}
	if before.ProgramRevision == after.ProgramRevision {
		t.Errorf("program_revision did not move (%q) when the screen was pointed at a different asset — a frozen revision "+
			"means a player never swaps to what was just published (PLY-090/108)", before.ProgramRevision)
	}
}

// firstLayerURL returns the url of the first content-bearing layer anywhere in
// prog, failing when there is none — so a premise stated about layers is not
// quietly satisfied by a program that carries no layer at all.
func firstLayerURL(t *testing.T, prog wire.ScreenProgram) string {
	t.Helper()
	for _, c := range prog.Content {
		for _, l := range c.Layers {
			if wire.LayerFetchesContent(l.Kind) {
				return l.URL
			}
		}
	}
	t.Fatalf("no content-bearing layer in %+v", prog.Content)
	return ""
}
