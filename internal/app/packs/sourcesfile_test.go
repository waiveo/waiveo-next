package packs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// sourcesfile_test.go covers the registry sources document — the host-provisioned
// declaration that turns marketplace/1's resolver from unreachable code into
// something an operator can point at a registry.
//
// The last test in this file is the one that matters most: it takes a document
// off disk, resolves a signed pack through the sources it declares, and installs
// it. Everything above it is a refusal, and a refusal test cannot tell you the
// configuration is CONNECTED to anything — that is precisely how the resolver
// came to sit complete and unmounted in the first place.

// writeSources writes a sources document at 0600 in its own directory and
// returns the path. t.TempDir() is 0700, so the ancestor walk passes.
func writeSources(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pack-sources.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write sources document: %v", err)
	}
	return path
}

// sourceIDs renders a loaded list as its ids, in order.
func sourceIDs(sources []packs.Source) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.ID)
	}
	return out
}

// ---- absent, empty, unusable ------------------------------------------------

// TestLoadSourcesAbsentConfiguresNoMarketplace: no document is the DEFAULT, and
// it is not an error. Every dev checkout, every CI run and every unprovisioned
// box is in this state, and each must boot exactly as it did before this
// configuration existed.
func TestLoadSourcesAbsentConfiguresNoMarketplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nothing-here.json")

	sources, err := packs.LoadSources(path)
	if err != nil {
		t.Fatalf("an absent sources document is an error (%v) — an unprovisioned box would report a broken deployment", err)
	}
	if len(sources) != 0 {
		t.Fatalf("an absent document produced %d source(s)", len(sources))
	}
	if !packs.SourcesAbsent(path) {
		t.Error("SourcesAbsent is false for a path with no file — the boot report cannot tell 'never authored' from 'authored empty'")
	}
}

// TestLoadSourcesEmptyIsDistinguishableFromAbsent: a document declaring no
// source enforces the same nothing, and reports as a different event. A boot
// report that cannot tell the two apart cannot confirm the deployment loaded the
// list it meant to — a typo'd env var and a deliberate empty list look identical.
func TestLoadSourcesEmptyIsDistinguishableFromAbsent(t *testing.T) {
	path := writeSources(t, `{"format":"registry-sources/1","sources":[]}`)

	sources, err := packs.LoadSources(path)
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("got %d source(s), want none", len(sources))
	}
	if packs.SourcesAbsent(path) {
		t.Error("SourcesAbsent is true for a document that exists and declares nothing")
	}
}

// TestLoadSourcesUnusableConfiguresNoSourceRatherThanSome is the fail-closed
// bar, and the assertion that matters is the SECOND one.
//
// A document the host cannot fully validate must configure NOTHING — not the
// sources it happened to parse before the bad one. A partial list silently
// changes the operator's declared resolution ORDER (MKT-061): the source meant
// to answer first may now be second, or absent, and every install afterwards
// resolves against a preference nobody wrote.
func TestLoadSourcesUnusableConfiguresNoSourceRatherThanSome(t *testing.T) {
	// Two perfectly good sources, then one with a plaintext-http index url.
	path := writeSources(t, `{"format":"registry-sources/1","sources":[
		{"id":"first","channel":"marketplace/stable","index_url":"https://registry.example/index.json"},
		{"id":"second","channel":"marketplace/stable","index_url":"https://mirror.example/index.json"},
		{"id":"third","channel":"marketplace/stable","index_url":"http://plaintext.example/index.json"}
	]}`)

	sources, err := packs.LoadSources(path)
	if err == nil {
		t.Fatal("a document with an invalid source loaded without error")
	}
	if len(sources) != 0 {
		t.Fatalf("a partly-valid document configured %v — the declared resolution order (MKT-061) silently became a different one", sourceIDs(sources))
	}
	if !strings.Contains(err.Error(), "NO registry source is configured") {
		t.Errorf("refusal = %v, want it to say no source is configured — an operator reading anything softer would assume the good sources loaded", err)
	}
}

// TestLoadSourcesRefusesADocumentThatIsNotASourceList: the format discriminant.
//
// The neighbouring documents are the reason it exists. Point this loader at the
// required-pack roster or the trust anchors — provisioned by the same party,
// into the same directory — and both parse perfectly into a sourcesFile with
// zero sources: a well-formed, empty source list from a file that is not one.
func TestLoadSourcesRefusesADocumentThatIsNotASourceList(t *testing.T) {
	roster := writeSources(t, `{"format":"required-packs/1","required":[{"pack_id":"waiveo/system","floor_version":"1.0.0"}]}`)

	if _, err := packs.LoadSources(roster); err == nil {
		t.Fatal("the required-pack roster loaded as a registry sources document")
	}
}

// TestLoadSourcesRefusesTheLaxJSONShapes covers the three ways a document can be
// well-formed JSON and still not mean what it reads as. Each is masked by
// nothing else here: all three produce a document that parses.
func TestLoadSourcesRefusesTheLaxJSONShapes(t *testing.T) {
	const good = `{"id":"upstream","channel":"marketplace/stable","index_url":"https://registry.example/index.json"}`
	for _, tc := range []struct{ name, body string }{
		{
			// A typo'd member decodes to a document with the right format and no
			// sources — a valid, empty list from a file that declares one.
			"unknown field",
			`{"format":"registry-sources/1","sourcs":[` + good + `]}`,
		},
		{
			// encoding/json keeps the LAST of two same-named members, so appending
			// `,"sources":[]` produces a document that reads as declaring a
			// registry and resolves to nothing.
			"duplicate member",
			`{"format":"registry-sources/1","sources":[` + good + `],"sources":[]}`,
		},
		{
			// Decode stops at the end of the first value, so a real declaration
			// sitting after a decoy would never be read.
			"trailing content",
			`{"format":"registry-sources/1","sources":[]} {"format":"registry-sources/1","sources":[` + good + `]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sources, err := packs.LoadSources(writeSources(t, tc.body))
			if err == nil {
				t.Fatalf("accepted, resolving to %v", sourceIDs(sources))
			}
		})
	}
}

// TestLoadSourcesRefusesAWritableDocument / …Directory: whoever can write this
// file names a registry this box fetches code from, and can authorize it for the
// `waiveo` namespace (MKT-062). Same posture as the trust anchors and the
// roster, through the same shared guard.
func TestLoadSourcesRefusesAWritableDocument(t *testing.T) {
	path := writeSources(t, `{"format":"registry-sources/1","sources":[]}`)
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := packs.LoadSources(path); err == nil {
		t.Fatal("a world-writable sources document was read — anyone who can write it can point this box at their own registry")
	}
}

func TestLoadSourcesRefusesAWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pack-sources.json")
	if err := os.WriteFile(path, []byte(`{"format":"registry-sources/1","sources":[]}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, err := packs.LoadSources(path); err == nil {
		t.Fatal("a sources document in a world-writable directory was read — an attacker renames their own 0600 document over the path and the file's mode check passes on a file they wrote")
	}
}

// ---- the source list itself -------------------------------------------------

// TestLoadSourcesKeepsTheConfiguredOrder pins MKT-061's ordered list.
//
// Order is contract-specified — "order MAY be used only as a plain resolution
// preference among sources that each independently pass verification" — and the
// resolver walks it as written. The ids here are deliberately in an order no
// sort produces, so a loader that sorted, reversed, or map-iterated them fails
// rather than coincidentally agreeing.
func TestLoadSourcesKeepsTheConfiguredOrder(t *testing.T) {
	path := writeSources(t, `{"format":"registry-sources/1","sources":[
		{"id":"mirror","channel":"c","index_url":"https://a.example/index.json"},
		{"id":"alpha","channel":"c","index_url":"https://b.example/index.json"},
		{"id":"zulu","channel":"c","index_url":"https://c.example/index.json"},
		{"id":"beta","channel":"c","index_url":"https://d.example/index.json"}
	]}`)

	sources, err := packs.LoadSources(path)
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	got := strings.Join(sourceIDs(sources), ",")
	if want := "mirror,alpha,zulu,beta"; got != want {
		t.Fatalf("order = %s, want %s — the configured resolution preference (MKT-061) is not the one the operator declared", got, want)
	}

	// …and it survives the trip through NewMarket, which is the only consumer.
	market := packs.NewMarket(func() int64 { return fixedNow }, sources...)
	if got := strings.Join(sourceIDs(market.Sources()), ","); got != "mirror,alpha,zulu,beta" {
		t.Fatalf("Market order = %s, want the document order", got)
	}
}

// TestLoadSourcesRefusesADuplicateSourceID: the id is what an install record
// pins as `source` (MKT-094) and what a source-pinned reference selects by
// (MKT-060a(d)). Two sources under one id make the record unable to say which
// registry served the bytes.
func TestLoadSourcesRefusesADuplicateSourceID(t *testing.T) {
	path := writeSources(t, `{"format":"registry-sources/1","sources":[
		{"id":"upstream","channel":"c","index_url":"https://a.example/index.json"},
		{"id":"upstream","channel":"c","index_url":"https://b.example/index.json"}
	]}`)

	if _, err := packs.LoadSources(path); err == nil {
		t.Fatal("two sources sharing one id were accepted")
	}
}

// TestLoadSourcesRefusesTheDirectSentinelAsASourceID.
//
// NewMarket already refuses this id — by SILENTLY DROPPING the source, which is
// right for a programmatic caller and wrong for a document: the operator's
// source would simply vanish, the box would answer "nothing resolves this" for a
// registry it appeared to have, and no line anywhere would say why. The loader
// has someone to tell, so it tells them.
func TestLoadSourcesRefusesTheDirectSentinelAsASourceID(t *testing.T) {
	path := writeSources(t, `{"format":"registry-sources/1","sources":[
		{"id":"direct","channel":"c","index_url":"https://a.example/index.json"}
	]}`)

	sources, err := packs.LoadSources(path)
	if err == nil {
		t.Fatalf("the direct-install sentinel was accepted as a source id, resolving to %v", sourceIDs(sources))
	}
	if !strings.Contains(err.Error(), "direct") {
		t.Errorf("refusal = %v, want it to name the offending id", err)
	}
}

// TestLoadSourcesRefusesASourceWithNoChannelBinding.
//
// Source.Channel left empty makes fetchIndex skip the channel-agreement check
// entirely, so an omitted member would silently buy that source the right to
// serve an index naming ANY channel. A missing value must not read as a granted
// one — that is the direction the whole document refuses to fail in.
func TestLoadSourcesRefusesASourceWithNoChannelBinding(t *testing.T) {
	path := writeSources(t, `{"format":"registry-sources/1","sources":[
		{"id":"upstream","index_url":"https://a.example/index.json"}
	]}`)

	if _, err := packs.LoadSources(path); err == nil {
		t.Fatal("a source with no channel binding was accepted — its index could name any channel at all")
	}
}

// TestLoadSourcesIndexURLSchemes pins which transports exist and, for the two
// that do, what each one derives.
func TestLoadSourcesIndexURLSchemes(t *testing.T) {
	t.Run("https configures an http fetcher and is not stale", func(t *testing.T) {
		sources, err := packs.LoadSources(writeSources(t, `{"format":"registry-sources/1","sources":[
			{"id":"upstream","channel":"c","index_url":"https://registry.example/marketplace/index.json"}
		]}`))
		if err != nil {
			t.Fatalf("LoadSources: %v", err)
		}
		s := sources[0]
		if _, ok := s.Fetcher.(packs.HTTPFetcher); !ok {
			t.Fatalf("fetcher = %T, want packs.HTTPFetcher", s.Fetcher)
		}
		if s.IndexURL != "https://registry.example/marketplace/index.json" {
			t.Errorf("IndexURL = %q, want the configured url untouched", s.IndexURL)
		}
		if s.StaleSource {
			t.Error("a network source is marked stale_source; MKT-063 scopes that mark to file:// resolutions")
		}
	})

	t.Run("file confines to the index's own directory and IS stale", func(t *testing.T) {
		dir := t.TempDir()
		sources, err := packs.LoadSources(writeSources(t, `{"format":"registry-sources/1","sources":[
			{"id":"local","channel":"c","index_url":"file://`+filepath.ToSlash(dir)+`/index.json"}
		]}`))
		if err != nil {
			t.Fatalf("LoadSources: %v", err)
		}
		s := sources[0]
		ff, ok := s.Fetcher.(packs.FileFetcher)
		if !ok {
			t.Fatalf("fetcher = %T, want packs.FileFetcher", s.Fetcher)
		}
		if ff.Root != dir {
			t.Errorf("registry root = %q, want the index's own directory %q", ff.Root, dir)
		}
		// Rewritten root-relative: FileFetcher resolves every url against Root,
		// so an absolute path here would resolve to root+the-whole-path.
		if s.IndexURL != "file:///index.json" {
			t.Errorf("IndexURL = %q, want the root-relative form the FileFetcher resolves in", s.IndexURL)
		}
		// MKT-063, and derived rather than declared: there is no document member
		// that could clear it beside a file:// url.
		if !s.StaleSource {
			t.Error("a file:// source is not marked stale_source (MKT-063)")
		}
	})

	t.Run("refusals", func(t *testing.T) {
		for _, tc := range []struct{ name, url, want string }{
			{"plaintext http", "http://registry.example/index.json", "plaintext"},
			{"no scheme", "registry.example/index.json", "scheme"},
			{"unknown scheme", "ftp://registry.example/index.json", "scheme"},
			{"credentials in the url", "https://user:pw@registry.example/index.json", "credentials"},
			{"file with a host", "file://elsewhere/srv/registry/index.json", "host"},
			// The whole filesystem would become the registry root, so every path
			// an index author writes becomes a read of this host.
			{"file at the filesystem root", "file:///index.json", "registry directory"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := packs.LoadSources(writeSources(t, `{"format":"registry-sources/1","sources":[
					{"id":"upstream","channel":"c","index_url":"`+tc.url+`"}
				]}`))
				if err == nil {
					t.Fatalf("index_url %q was accepted", tc.url)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("refusal = %v, want it to name %q — a refusal for the wrong reason leaves the operator fixing the wrong thing", err, tc.want)
				}
			})
		}
	})
}

// TestLoadSourcesRefusesAnInertReservedNamespaceAuthorization.
//
// MKT-062's reserved set is fixed by the contract. Authorizing anything outside
// it grants NOTHING — a non-reserved namespace resolves from any source without
// authorization — so an operator who wrote one believed they had granted
// something and had not. That is the failure this whole document is written
// against: a value that parses, reads as policy, and enforces nothing.
func TestLoadSourcesRefusesAnInertReservedNamespaceAuthorization(t *testing.T) {
	path := writeSources(t, `{"format":"registry-sources/1","sources":[
		{"id":"upstream","channel":"c","index_url":"https://a.example/index.json","reserved_namespaces":["acme"]}
	]}`)

	_, err := packs.LoadSources(path)
	if err == nil {
		t.Fatal("a non-reserved namespace was accepted as a source authorization, granting nothing while reading as a grant")
	}
	if !strings.Contains(err.Error(), "MKT-062") {
		t.Errorf("refusal = %v, want it to cite the requirement whose set the operator is outside", err)
	}
}

// TestLoadSourcesReservedNamespacesDefaultToNone is MKT-062's fail-closed
// default, and it is asserted THROUGH A RESOLUTION rather than on the struct: a
// field left empty proves nothing about what the resolver does with it.
//
// The same registry, the same signed artifact, the same reference — and the only
// difference is one line of the sources document.
func TestLoadSourcesReservedNamespacesDefaultToNone(t *testing.T) {
	// A reserved-namespace pack. `waiveo` carries first-party and nothing else
	// may (MKT-021), so the reference is first-party too.
	const packID = "waiveo/menu-board"
	m := baseManifest()
	m["id"] = packID
	signer := newTestSigner(t)
	art := signer.sign(t, basePackZip(t, m), packID, "1.0.0")

	reg := newRegistry(t, "upstream")
	reg.publish(packID, "1.0.0", art, nil)
	reg.point(packID, "first-party", "1.0.0")
	reg.source() // writes the index document into reg.dir

	load := func(t *testing.T, reserved string) []packs.Source {
		t.Helper()
		sources, err := packs.LoadSources(writeSources(t, `{"format":"registry-sources/1","sources":[
			{"id":"upstream","channel":"marketplace/stable","index_url":"file://`+filepath.ToSlash(reg.dir)+`/index.json"`+reserved+`}
		]}`))
		if err != nil {
			t.Fatalf("LoadSources: %v", err)
		}
		return sources
	}

	install := func(t *testing.T, sources []packs.Source) error {
		t.Helper()
		st := openStore(t)
		market := packs.NewMarket(func() int64 { return fixedNow }, sources...)
		in := packs.NewInstaller(st, signer.anchorsFor("waiveo"), packs.WithMarketplace(market))
		_, err := in.InstallRef(context.Background(), packs.Ref{PackID: packID, TrustChannel: "first-party"})
		return err
	}

	// (a) the member omitted entirely — the default, and it must be NONE.
	err := install(t, load(t, ""))
	if err == nil {
		t.Fatal("a source that authorized no namespace served a reserved-namespace pack (MKT-062) — the default is not fail-closed")
	}
	if !strings.Contains(err.Error(), "REGISTRY_SOURCE_NOT_DELEGATED") {
		t.Fatalf("refusal = %v, want REGISTRY_SOURCE_NOT_DELEGATED", err)
	}

	// (b) the SAME everything with the authorization written in. Without this
	// half, a loader that dropped reserved_namespaces on the floor — or a
	// resolver that refused every reserved namespace outright — would pass (a).
	if err := install(t, load(t, `,"reserved_namespaces":["waiveo"]`)); err != nil {
		t.Fatalf("an authorized source could not serve its namespace: %v", err)
	}
}

// ---- the whole chain --------------------------------------------------------

// TestSourcesDocumentResolvesAndInstalls is the test this file exists for.
//
// Every other case here is a refusal, and refusals are exactly what a completely
// unwired marketplace produces too. This one starts at a JSON document on disk,
// goes through LoadSources → NewMarket → Installer.InstallRef, and ends with a
// pack row and an install record naming the source the document declared. It is
// the only assertion here that the configuration is CONNECTED — and connecting
// it is the whole change.
func TestSourcesDocumentResolvesAndInstalls(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)

	signer := newTestSigner(t)
	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")
	reg := newRegistry(t, "shop")
	reg.publish("acme/menu-board", "1.0.0", art, nil)
	reg.point("acme/menu-board", "community", "1.0.0")
	reg.source()

	sources, err := packs.LoadSources(writeSources(t, `{"format":"registry-sources/1","sources":[
		{"id":"shop","channel":"marketplace/stable","index_url":"file://`+filepath.ToSlash(reg.dir)+`/index.json"}
	]}`))
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...),
		packs.WithMarketplace(packs.NewMarket(func() int64 { return fixedNow }, sources...)))

	res, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	if err != nil {
		t.Fatalf("a pack named in a configured registry did not install: %v", err)
	}
	if res.ID != "acme/menu-board" || res.Version != "1.0.0" {
		t.Fatalf("installed %s@%s, want acme/menu-board@1.0.0", res.ID, res.Version)
	}

	recs, err := st.ListPackInstalls(ctx, "acme/menu-board")
	if err != nil || len(recs) != 1 {
		t.Fatalf("install records = %v (err %v), want exactly one", recs, err)
	}
	// The id the DOCUMENT declared is the one the record pins (MKT-094): the
	// operator's configuration reached the audit trail, not merely the resolver.
	if recs[0].Source != "shop" {
		t.Errorf("record source = %q, want the declared source id %q", recs[0].Source, "shop")
	}
	// MKT-063, end to end: derived from the file:// scheme at load, carried
	// through resolution, persisted on the record.
	if !recs[0].StaleSource {
		t.Error("a file://-resolved install is not marked stale_source (MKT-063)")
	}
}

// TestNoSourcesDocumentLeavesTheInstallerWithNoMarketplace walks the exact value
// chain the feeder takes when nothing is declared, all the way to a refusal.
//
// It is the nil case, and it is separate from the empty-Market one
// (TestADeploymentWithNoRegistryConfiguredSaysSo) because the binary produces a
// DIFFERENT value here: no document means no sources means no Market at all, so
// api.WithMarketplace is handed a nil *Market and the installer is built over
// it. Nothing else in this package passes nil, so a later change making
// WithMarketplace or InstallRef assume a non-nil Market would compile, pass
// every existing test, and panic on the most common deployment there is — a box
// that never authored the document.
func TestNoSourcesDocumentLeavesTheInstallerWithNoMarketplace(t *testing.T) {
	sources, err := packs.LoadSources(filepath.Join(t.TempDir(), "never-authored.json"))
	if err != nil || len(sources) != 0 {
		t.Fatalf("LoadSources on an absent document = %v, %v", sources, err)
	}
	// Exactly what main does with an empty list: no Market is built.
	var market *packs.Market
	if len(sources) > 0 {
		market = packs.NewMarket(func() int64 { return fixedNow }, sources...)
	}

	st := openStore(t)
	in := packs.NewInstaller(st, newTestSigner(t).anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))

	before := gen(t, st)
	_, err = in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	if err == nil {
		t.Fatal("an unprovisioned deployment resolved a marketplace reference")
	}
	if !strings.Contains(err.Error(), "MARKETPLACE_REF_UNRESOLVED") {
		t.Fatalf("refusal = %v, want MARKETPLACE_REF_UNRESOLVED — the same answer this box gave before the marketplace was mounted", err)
	}
	noResidue(t, st, "acme/menu-board", before)
}

// TestSourcesDocumentGrantsNoTrust is the property everything above rests on,
// stated as a test: a configured source can LOCATE bytes and cannot make the
// host accept them.
//
// The registry here is perfectly configured and serves an artifact whose
// signature this deployment's anchors do not vouch for. The refusal must be the
// install pipeline's, identical to the one a hand-uploaded copy of the same
// bytes would get.
func TestSourcesDocumentGrantsNoTrust(t *testing.T) {
	st := openStore(t)

	// Signed by a publisher this deployment has never heard of.
	stranger := newTestSigner(t)
	art := stranger.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")
	reg := newRegistry(t, "shop")
	reg.publish("acme/menu-board", "1.0.0", art, nil)
	reg.point("acme/menu-board", "community", "1.0.0")
	reg.source()

	sources, err := packs.LoadSources(writeSources(t, `{"format":"registry-sources/1","sources":[
		{"id":"shop","channel":"marketplace/stable","index_url":"file://`+filepath.ToSlash(reg.dir)+`/index.json"}
	]}`))
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	// The host's own anchors — a DIFFERENT signer's.
	host := newTestSigner(t)
	in := packs.NewInstaller(st, host.anchorsFor(fixtureNamespaces...),
		packs.WithMarketplace(packs.NewMarket(func() int64 { return fixedNow }, sources...)))

	before := gen(t, st)
	_, err = in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	if err == nil {
		t.Fatal("configuring a registry source admitted an artifact the host's trust anchors do not vouch for — resolution granted trust")
	}
	if !strings.Contains(err.Error(), "PACK_SIGNER_UNTRUSTED") {
		t.Fatalf("refusal = %v, want PACK_SIGNER_UNTRUSTED — the same refusal the direct-upload path gives", err)
	}
	noResidue(t, st, "acme/menu-board", before)
}
