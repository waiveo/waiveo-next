package packs_test

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// A sweep of this package found 14 refusals that survive deletion with the whole
// TREE green. The ones pinned here share a shape sharper than "untested", and it
// is the shape worth naming:
//
//	A CODE IS ASSERTED. THE GUARD THAT RAISES IT IS NOT.
//
// PACK_LOCALE_INVALID has two emitters — the catalog's JSON validity
// (packs.go:441) and its flat-map shape (packs.go:453) — and both existing
// assertions of that code reach only the second, because well-formed JSON of the
// wrong shape sails past the first. PACK_ARTIFACT_FILE_TOO_LARGE likewise has two
// (reader.go:158, reader.go:211). So the code appears covered while one guard of
// each pair is held by nothing.
//
// That is why these fixtures are built the awkward way — a catalog that is not
// JSON at all, a zip header that misreports its own contents. An honest fixture
// cannot reach the code path that matters.
//
// One of the two pairs did not survive contact with a test, and the correction is
// recorded on the test itself rather than quietly dropped: reader.go:211 is
// unreachable by construction, not untested. Writing the test is what showed it.

// TestAZipHeaderThatUnderstatesItsContentIsRefused pins the PROPERTY the reader
// needs — a per-file cap that the artifact cannot talk its way past — rather
// than any one line that enforces it.
//
// It is written this way because the sweep's finding here turned out to be
// different from what it looked like. readCapped reads cap+1 bytes through an
// io.LimitReader and refuses a longer result, and its doc says that check exists
// for "a lying uncompressed-size header trying to beat the per-file cap". That
// check survives deletion, and the reason is not a missing test:
//
//	IT CANNOT FIRE. Two other layers already close the gap. reader.go refuses a
//	DECLARED size over the cap before reading, so UncompressedSize64 <= cap; and
//	archive/zip's own checksumReader returns ErrFormat as soon as the
//	decompressed stream exceeds UncompressedSize64. So len(data) <= cap always,
//	and the guard's condition is unreachable by construction.
//
// The first version of this test asserted PACK_ARTIFACT_FILE_TOO_LARGE for a
// lying header and failed — which is how the unreachability surfaced. Writing
// the test is what disproved the finding.
//
// So what is pinned here is the property across all three layers: a header that
// UNDERSTATES its content is refused, and one that OVERSTATES it past the cap is
// refused. Neither assertion names the layer that acts, because which layer acts
// is exactly what a future change may move.
func TestAZipHeaderThatUnderstatesItsContentIsRefused(t *testing.T) {
	lim := packs.Limits{MaxArtifactBytes: 1 << 20, MaxFileBytes: 64, MaxTotalUncompressed: 1 << 20, MaxFiles: 100}

	// Four kilobytes of real content behind a header claiming ten bytes.
	// CreateRaw is what makes the lie possible: the ordinary writer measures the
	// body, which is precisely what whoever built a hostile archive would not do.
	if _, err := packs.ReadBundle(rawZip(t, "big.json", strings.Repeat("y", 4096), 10), lim); err == nil {
		t.Error("an entry declaring 10 bytes and delivering 4096 was ACCEPTED — the declared size comes from the " +
			"archive itself, so a cap enforced only against it is a cap the artifact chooses for itself")
	}

	// And the other direction: a header that declares MORE than the cap is
	// refused on the declaration alone, before anything is decompressed.
	if _, err := packs.ReadBundle(rawZip(t, "big.json", strings.Repeat("y", 4096), 4096), lim); err == nil {
		t.Error("an entry declaring 4096 bytes under a 64-byte cap was accepted")
	}

	// The control: the same raw construction, truthful and under the cap, still
	// reads. Without it, a reader that refused every raw-written entry would
	// satisfy both assertions above for a reason that has nothing to do with size.
	const small = "small"
	b, err := packs.ReadBundle(rawZip(t, "small.json", small, int64(len(small))), lim)
	if err != nil {
		t.Fatalf("a truthful entry under the cap was refused: %v", err)
	}
	if body, ok := b.File("small.json"); !ok || string(body) != small {
		t.Errorf("the truthful entry read back as %q (ok=%v), want %q", body, ok, small)
	}
}

// rawZip writes one deflate-compressed entry whose UncompressedSize64 header is
// DECLARED rather than measured, so a test can build an archive that misreports
// its own contents. With declared == len(body) it builds an ordinary, truthful
// entry — which is what makes it usable as its own control.
func rawZip(t *testing.T, name, body string, declared int64) []byte {
	t.Helper()

	var comp bytes.Buffer
	fw, err := flate.NewWriter(&comp, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate writer: %v", err)
	}
	if _, err := fw.Write([]byte(body)); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(0o644)
	h.UncompressedSize64 = uint64(declared)
	h.CompressedSize64 = uint64(comp.Len())
	// The CRC of the REAL body: a mismatched checksum would be a different fault,
	// and this fixture is about the size claim alone.
	h.CRC32 = crc32.ChecksumIEEE([]byte(body))

	w, err := zw.CreateRaw(h)
	if err != nil {
		t.Fatalf("zip CreateRaw: %v", err)
	}
	if _, err := w.Write(comp.Bytes()); err != nil {
		t.Fatalf("zip write raw: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// TestALocaleCatalogThatIsNotJSONIsRefusedAsSuch exercises the FIRST emitter of
// PACK_LOCALE_INVALID, which the two existing assertions of that code never
// reach: both supply well-formed JSON of the wrong SHAPE, so the flat-map check
// refuses them and json.Valid is never what fires.
//
// THE ASSERTION IS ON THE MESSAGE, NOT THE CODE, AND THAT IS THE WHOLE POINT.
// The first version of this test asserted the code, and the mutant SURVIVED it:
// with json.Valid deleted, unparseable bytes fall through to the shape check,
// whose unmarshal also fails, which also returns PACK_LOCALE_INVALID. Identical
// code, so a code assertion cannot tell the two apart — the same
// cannot-distinguish-two-outcomes defect this file was opened to pin, reproduced
// in the test written to pin it.
//
// What actually differs is the DIAGNOSIS an author reads: "is not valid JSON"
// against "must be a flat object of string values (MAN-110)". Those are
// different faults with different fixes, and collapsing them tells someone whose
// file is truncated to go restructure a document that never parsed.
func TestALocaleCatalogThatIsNotJSONIsRefusedAsSuch(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not json at all", "this is not json"},
		{"a truncated object", `{"greeting": "hello`},
		{"an empty file", ""},
		{"trailing garbage after a valid object", `{"greeting":"hello"} and then some`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := installWithEntry(t, "messages/fr.json", tc.body)
			if err == nil {
				t.Fatal("a locale catalog that is not valid JSON was accepted")
			}
			artifactCode(t, err, "PACK_LOCALE_INVALID")

			var aerr *packs.ArtifactError
			if !errors.As(err, &aerr) {
				t.Fatalf("error = %v (%T), want *packs.ArtifactError", err, err)
			}
			if !strings.Contains(aerr.Message, "is not valid JSON") {
				t.Errorf("bytes that do not parse were refused as %q — the refusal an author reads must name the "+
					"fault they have (the file does not parse) rather than the one they do not (its shape is "+
					"wrong), since the two have different fixes", aerr.Message)
			}
		})
	}

	// The other emitter still owns its own diagnosis: well-formed JSON of the
	// wrong shape must NOT be reported as unparseable. Without this the test
	// above is satisfied by collapsing both faults into the first message
	// instead of the second.
	err := installWithEntry(t, "messages/fr.json", `["not","a","map"]`)
	var aerr *packs.ArtifactError
	if !errors.As(err, &aerr) {
		t.Fatalf("a non-object catalog: error = %v (%T), want *packs.ArtifactError", err, err)
	}
	if strings.Contains(aerr.Message, "is not valid JSON") {
		t.Errorf("well-formed JSON of the wrong shape was reported as unparseable: %q", aerr.Message)
	}

	// The control: a well-formed flat catalog at the same path installs. Without
	// it, an install that refused every bundle would satisfy every case above.
	if err := installWithEntry(t, "messages/fr.json", `{"greeting":"bonjour"}`); err != nil {
		t.Fatalf("a valid flat locale catalog was refused: %v", err)
	}
}

// TestANestedPathUnderMessagesIsNotALocaleCatalog pins localeName's own guard,
// and it is the one case in this file where the guard's job is to make the
// platform do LESS.
//
// The locale is the segment between `messages/` and `.json`. Without the guard
// that segment may be empty or may itself contain a path, so `messages/.json`
// registers the EMPTY locale and `messages/en/US.json` registers a locale
// literally named "en/US" — neither of which any resolver will ask for.
//
// The distinguishing fixture is content that is NOT valid JSON at such a path.
// With the guard it is not a locale catalog, so the catalog rules do not apply
// and the pack installs. Without it, the same bytes are validated as a catalog
// and the install is refused — a pack rejected for the contents of a file that
// was never a locale catalog in the first place.
func TestANestedPathUnderMessagesIsNotALocaleCatalog(t *testing.T) {
	for _, name := range []string{
		"messages/.json",      // the empty locale
		"messages/en/US.json", // a locale whose name would contain a path separator
		"messages/a/b/c.json", // deeper still
	} {
		if err := installWithEntry(t, name, "this is not json"); err != nil {
			t.Errorf("a bundle carrying %q was refused (%v) — that entry is not a locale catalog, so its contents "+
				"are not subject to the catalog rules; refusing it rejects a pack for a file it never shipped as "+
				"a catalog", name, err)
		}
	}

	// The control: at a REAL locale path the same bytes ARE refused, so the loop
	// above is not passing merely because catalog validation stopped happening.
	if err := installWithEntry(t, "messages/fr.json", "this is not json"); err == nil {
		t.Error("the same invalid content at a real locale path was accepted — then nothing above distinguishes " +
			"a nested path from a locale at all")
	}
}

// installWithEntry installs the base pack with one extra (or replaced) bundle
// entry, returning the install error. The base bundle is otherwise valid, so any
// refusal is attributable to the entry under test.
func installWithEntry(t *testing.T, name, body string) error {
	t.Helper()
	st := openStore(t)
	files := basePackFiles(t, baseManifest())
	files[name] = body
	in, signer := newInstaller(t, st)
	_, err := in.Install(context.Background(), signedFilesZip(t, signer, files, "acme/menu-board", "1.0.0"))
	return err
}

// TestMeetsFloorRefusesAVersionOutsideTheGrammar pins the fail-closed rule on the
// required-pack floor comparison specifically.
//
// versionrules_test.go already pins that the ordering primitive fails closed on
// an unparseable input, and that test cannot fail here: this is a second,
// separate check, in the function the store's required-pack enforcement and the
// revert path's candidate narrowing both compare with.
//
// Without it the comparison runs on strings the grammar rejects, and it is built
// as `!VersionLower(...)` — which answers TRUE whenever the ordering cannot place
// them. So a garbage version SATISFIES a required floor, which is the single
// direction a floor exists to prevent: MKT-093b says a required pack below its
// floor MUST NOT be installed on any path.
func TestMeetsFloorRefusesAVersionOutsideTheGrammar(t *testing.T) {
	r, err := packs.NewRoster(map[string]string{"waiveo/core": "1.2.0"})
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}

	for _, tc := range []struct{ version, floor, why string }{
		{"", "1.2.0", "an empty version"},
		{"1.2", "1.2.0", "two components"},
		{"1.2.0.1", "1.2.0", "four components"},
		{"v1.2.0", "1.2.0", "a leading v"},
		{"latest", "1.2.0", "a channel name where a version belongs"},
		{"1.2.0", "", "an empty FLOOR — the roster's own side of the comparison"},
		{"1.2.0", "not-a-version", "a floor outside the grammar"},
	} {
		if r.MeetsFloor(tc.version, tc.floor) {
			t.Errorf("MeetsFloor(%q, %q) = true — %s must not satisfy a floor, because a value the ordering cannot "+
				"place has no position relative to it (MKT-093b)", tc.version, tc.floor, tc.why)
		}
	}

	// The controls: the comparison still works, in both directions. Without
	// these, a MeetsFloor answering false to everything would satisfy every case
	// above while refusing every conformant required pack on the deployment.
	if !r.MeetsFloor("1.2.0", "1.2.0") {
		t.Error("a version exactly at its floor did not meet it")
	}
	if !r.MeetsFloor("1.3.0", "1.2.0") {
		t.Error("a version above its floor did not meet it")
	}
	if r.MeetsFloor("1.1.9", "1.2.0") {
		t.Error("a version below its floor met it")
	}
}

// TestAnUnresolvedRosterRefusesEvenAWellFormedComparison keeps MeetsFloor's two
// halves distinguishable: the resolved-roster gate and the grammar gate are
// separate refusals, and a test driving only one would let the other go.
func TestAnUnresolvedRosterRefusesEvenAWellFormedComparison(t *testing.T) {
	var zero packs.Roster // never resolved
	if zero.MeetsFloor("9.9.9", "1.0.0") {
		t.Error("an UNRESOLVED roster reported a version as meeting a floor — an unresolved roster knows no floors, " +
			"so answering the question at all is the failure")
	}
}
