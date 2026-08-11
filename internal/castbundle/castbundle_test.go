package castbundle

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// The bundle format, held to the two properties everything else rests on: a
// bundle written from a cast reads back as that cast, and a bundle that has been
// tampered with, truncated, or hand-assembled is REFUSED rather than partially
// applied.

var (
	logoBytes = []byte("PNG-ish bytes for the logo layer")
	menuBytes = []byte("PNG-ish bytes for the menu photo")
)

func logoRef() string { return AssetRefOf(logoBytes) }
func menuRef() string { return AssetRefOf(menuBytes) }

// twoImageCast is the fixture design: two slides, three layers, two DISTINCT
// images with one of them used twice — so de-duplication is exercised by the
// ordinary case rather than by a special one.
func twoImageCast() CastPayload {
	return CastPayload{
		Name:              "Lunch — Tuesday (v2)",
		DefaultDurationMS: 8000,
		Labels:            map[string]string{"site": "shop"},
		Slides: []datamodel.CastSlide{
			{ID: "hero", Layers: []wire.Layer{
				{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: menuRef()},
				{Kind: wire.LayerKindImage, X: 20, Y: 20, W: 200, H: 80, AssetRef: logoRef()},
			}},
			{ID: "closing", Layers: []wire.Layer{
				{Kind: wire.LayerKindImage, X: 20, Y: 20, W: 200, H: 80, AssetRef: logoRef()},
			}},
		},
	}
}

func fixtureAssets() map[string][]byte {
	return map[string][]byte{logoRef(): logoBytes, menuRef(): menuBytes}
}

func writeFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, Manifest{ExportedAtMs: 1_752_537_600_000, SourceCastID: "01J8ZCAST0000000000000001", Cast: twoImageCast()}, fixtureAssets()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.Bytes()
}

// TestABundleRoundTripsItsDesignAndItsImages is the format's whole claim.
func TestABundleRoundTripsItsDesignAndItsImages(t *testing.T) {
	got, err := Read(writeFixture(t))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Manifest.Format != Format {
		t.Errorf("format = %q, want %q", got.Manifest.Format, Format)
	}
	want := twoImageCast()
	if got.Manifest.Cast.Name != want.Name {
		t.Errorf("name = %q, want %q", got.Manifest.Cast.Name, want.Name)
	}
	if got.Manifest.Cast.DefaultDurationMS != want.DefaultDurationMS {
		t.Errorf("default_duration_ms = %d, want %d", got.Manifest.Cast.DefaultDurationMS, want.DefaultDurationMS)
	}
	if got.Manifest.Cast.Labels["site"] != "shop" {
		t.Errorf("labels = %v", got.Manifest.Cast.Labels)
	}
	if len(got.Manifest.Cast.Slides) != 2 || len(got.Manifest.Cast.Slides[0].Layers) != 2 {
		t.Fatalf("slides did not survive: %+v", got.Manifest.Cast.Slides)
	}
	if got.Manifest.Cast.Slides[0].Layers[0].AssetRef != menuRef() {
		t.Errorf("layer asset_ref = %q", got.Manifest.Cast.Slides[0].Layers[0].AssetRef)
	}
	if !bytes.Equal(got.Assets[menuRef()], menuBytes) || !bytes.Equal(got.Assets[logoRef()], logoBytes) {
		t.Error("the image bytes did not survive the round trip")
	}
	if got.Manifest.SourceCastID != "01J8ZCAST0000000000000001" {
		t.Errorf("source_cast_id = %q — provenance must survive", got.Manifest.SourceCastID)
	}
}

// TestTheAssetListIsDERIVEDFromTheSlides. A hand-supplied list can disagree with
// what the cast references — either listing an asset no layer names (bloat) or
// omitting one that is named (a slide with a hole). Deriving it makes both
// impossible.
func TestTheAssetListIsDERIVEDFromTheSlides(t *testing.T) {
	var buf bytes.Buffer
	m := Manifest{Cast: twoImageCast()}
	// A caller's own (wrong) list, which Write must ignore.
	m.Assets = []AssetEntry{{AssetRef: "sha256:" + strings.Repeat("ff", 32), SizeBytes: 1}}
	assets := fixtureAssets()
	assets["sha256:"+strings.Repeat("ff", 32)] = []byte("an asset nothing references")
	if err := Write(&buf, m, assets); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(buf.Bytes())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Assets) != 2 {
		t.Fatalf("bundle carries %d asset(s), want the 2 the slides reference: %v", len(got.Assets), keysOf(got.Assets))
	}
	if _, ok := got.Assets["sha256:"+strings.Repeat("ff", 32)]; ok {
		t.Error("an asset no layer references was carried")
	}
}

// TestWriteRefusesACastWhoseImageBytesWereNotSupplied. A bundle missing one of
// its own images imports as a cast with a hole in it; the write must fail rather
// than produce it.
func TestWriteRefusesACastWhoseImageBytesWereNotSupplied(t *testing.T) {
	var buf bytes.Buffer
	err := Write(&buf, Manifest{Cast: twoImageCast()}, map[string][]byte{logoRef(): logoBytes})
	if err == nil {
		t.Fatal("Write succeeded with a referenced image missing")
	}
	if !strings.Contains(err.Error(), menuRef()) {
		t.Errorf("error = %v, want it to name the missing asset", err)
	}
}

// TestABundleIsByteIdenticalWhenWrittenTwice — so an operator can tell "this is
// the design I already have" from "this is a different one" by comparing files.
func TestABundleIsByteIdenticalWhenWrittenTwice(t *testing.T) {
	if !bytes.Equal(writeFixture(t), writeFixture(t)) {
		t.Error("two writes of the same cast produced different bytes")
	}
}

// TestReadRefusesTamperedBytes: the asset_ref a slide names cannot be pointed at
// different bytes. This is what makes an UNSIGNED bundle safe to import — trust
// comes from re-deriving the hash, not from the file.
func TestReadRefusesTamperedBytes(t *testing.T) {
	raw := writeFixture(t)
	// Rebuild the zip with one asset's content swapped, keeping the manifest.
	tampered := rebuild(t, raw, func(name string, body []byte) []byte {
		if strings.HasPrefix(name, "assets/") && bytes.Equal(body, menuBytes) {
			return []byte("bytes an attacker would rather the screen drew")
		}
		return body
	})
	_, err := Read(tampered)
	if !errors.Is(err, ErrAssetMismatch) {
		t.Fatalf("Read on tampered bytes = %v, want ErrAssetMismatch", err)
	}
}

// TestReadRefusesAnUndeclaredEntry. An importer that iterated the ZIP rather
// than the manifest would write a smuggled file into the destination's content
// origin without it ever appearing in the manifest anyone read.
func TestReadRefusesAnUndeclaredEntry(t *testing.T) {
	raw := writeFixture(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, f := range zr.File {
		w, _ := zw.Create(f.Name)
		rc, _ := f.Open()
		body := make([]byte, f.UncompressedSize64)
		_, _ = rc.Read(body)
		_ = rc.Close()
		_, _ = w.Write(body)
	}
	smuggled := []byte("a file the manifest never mentions")
	w, _ := zw.Create("assets/" + strings.TrimPrefix(AssetRefOf(smuggled), "sha256:"))
	_, _ = w.Write(smuggled)
	_ = zw.Close()

	_, err = Read(buf.Bytes())
	if !errors.Is(err, ErrNotABundle) {
		t.Fatalf("Read with a smuggled entry = %v, want ErrNotABundle", err)
	}
}

// TestReadRefusesAnEntryTheManifestDeclaresButTheBundleOmits — the mirror
// direction, which is a damaged file rather than a hostile one and must say so.
func TestReadRefusesAMissingDeclaredAsset(t *testing.T) {
	raw := writeFixture(t)
	stripped := rebuildDropping(t, raw, func(name string) bool {
		return strings.HasPrefix(name, "assets/")
	})
	_, err := Read(stripped)
	if !errors.Is(err, ErrDamaged) {
		t.Fatalf("Read with a declared asset missing = %v, want ErrDamaged", err)
	}
}

// TestReadRefusesAWrongFormatByNAME, before decompressing anything. A future
// format-2 bundle must be refused legibly, not fail somewhere deeper.
func TestReadRefusesAWrongFormat(t *testing.T) {
	raw := writeFixture(t)
	retagged := rebuild(t, raw, func(name string, body []byte) []byte {
		if name != ManifestName {
			return body
		}
		var m Manifest
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("decode manifest: %v", err)
		}
		m.Format = "waiveo.cast/9"
		out, _ := json.Marshal(m)
		return out
	})
	_, err := Read(retagged)
	if !errors.Is(err, ErrWrongFormat) {
		t.Fatalf("Read of a format-9 bundle = %v, want ErrWrongFormat", err)
	}
}

// TestReadRefusesThingsThatAreNotBundlesAtAll.
func TestReadRefusesThingsThatAreNotBundlesAtAll(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"empty", nil},
		{"not a zip", []byte("just some text an operator dragged in")},
		{"a zip with no manifest", zipOf(t, map[string][]byte{"readme.txt": []byte("hi")})},
	} {
		if _, err := Read(tc.raw); !errors.Is(err, ErrNotABundle) {
			t.Errorf("%s: Read = %v, want ErrNotABundle", tc.name, err)
		}
	}
}

// TestReadRefusesAnOversizedBundle without allocating it — the decompression
// bomb guard. The fixture declares more content than the reader will accept.
func TestReadRefusesAnOversizedBundle(t *testing.T) {
	if _, err := Read(make([]byte, MaxBundleBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Read of an over-limit input = %v, want ErrTooLarge", err)
	}
}

// TestReferencedAssetsIsSortedAndDeDuplicated.
func TestReferencedAssetsIsSortedAndDeDuplicated(t *testing.T) {
	got := ReferencedAssets(twoImageCast().Slides)
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 distinct refs", got)
	}
	if got[0] > got[1] {
		t.Errorf("got %v, want sorted", got)
	}
	if n := len(ReferencedAssets(nil)); n != 0 {
		t.Errorf("a cast with no slides references %d asset(s)", n)
	}
}

// TestFileNameReducesRatherThanRejects. An operator's cast is called "Lunch —
// Tuesday (v2)"; the tool must not tell them their title is wrong, and must not
// quote it raw into a Content-Disposition header either.
func TestFileName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Lunch — Tuesday (v2)", "Lunch-Tuesday-v2.cast"},
		{"Menu", "Menu.cast"},
		{"", "cast.cast"},
		{"???", "cast.cast"},
		{`evil"; rm -rf /`, "evil-rm-rf.cast"},
		{"../../etc/passwd", "etc-passwd.cast"},
	}
	for _, tc := range cases {
		got := FileName(tc.in)
		if got != tc.want {
			t.Errorf("FileName(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.ContainsAny(got, `"/\;`) {
			t.Errorf("FileName(%q) = %q, which carries a character a header or a path would interpret", tc.in, got)
		}
	}
	if got := FileName(strings.Repeat("a", 200)); len(got) > 70 {
		t.Errorf("FileName of a 200-char title is %d chars", len(got))
	}
}

// ── zip fixtures ────────────────────────────────────────────────────────────

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func zipOf(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}

// rebuild re-writes a bundle, passing each entry's body through transform.
func rebuild(t *testing.T, raw []byte, transform func(name string, body []byte) []byte) []byte {
	t.Helper()
	entries := map[string][]byte{}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		var body bytes.Buffer
		if _, err := body.ReadFrom(rc); err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		_ = rc.Close()
		entries[f.Name] = transform(f.Name, body.Bytes())
	}
	return zipOf(t, entries)
}

// rebuildDropping re-writes a bundle omitting the first entry drop reports true
// for — enough to make one declared asset missing.
func rebuildDropping(t *testing.T, raw []byte, drop func(name string) bool) []byte {
	t.Helper()
	entries := map[string][]byte{}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	dropped := false
	for _, f := range zr.File {
		if !dropped && drop(f.Name) {
			dropped = true
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		var body bytes.Buffer
		if _, err := body.ReadFrom(rc); err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		_ = rc.Close()
		entries[f.Name] = body.Bytes()
	}
	if !dropped {
		t.Fatal("fixture: nothing was dropped, so this proves nothing")
	}
	return zipOf(t, entries)
}

// ── The size limits ─────────────────────────────────────────────────────────
//
// These pin the arithmetic that makes "a bundle this box wrote is a bundle this
// box reads" true by construction rather than by luck. The defect they close:
// the reader used to advertise 512 MiB while the import route capped a request
// body at 64 MiB and the export was bounded by nothing, so this box could
// produce a file it would then refuse forever.

// TestTheOverheadReserveCoversTheWorstManifestAndFraming: the room set aside for
// everything that is not asset bytes must actually cover the worst case, or a
// bundle whose assets are exactly at the content ceiling would overflow the
// whole-bundle limit and be un-importable — the original defect, one layer down.
func TestTheOverheadReserveCoversTheWorstManifestAndFraming(t *testing.T) {
	worst := int64(MaxManifestBytes) + zipFramingBytesFor(MaxAssets)
	if worst > MaxBundleOverheadBytes {
		t.Fatalf("the worst manifest (%d) plus the framing for %d entries (%d) is %d bytes, past the %d reserved: a bundle at the content ceiling would exceed MaxBundleBytes and could not be imported",
			int64(MaxManifestBytes), MaxAssets, zipFramingBytesFor(MaxAssets), worst, int64(MaxBundleOverheadBytes))
	}
	if MaxBundleBytes != MaxBundleContentBytes+MaxBundleOverheadBytes {
		t.Fatalf("MaxBundleBytes = %d, want content %d + overhead %d — the whole-file limit must be DERIVED from the two parts, or the export's refusal and the reader's stop bounding the same thing",
			int64(MaxBundleBytes), int64(MaxBundleContentBytes), int64(MaxBundleOverheadBytes))
	}
}

// TestABundleAtTheContentCeilingIsReadable is the round trip at the size that
// matters, in this package: Write a bundle whose asset total is exactly what the
// export permits, and Read it back.
//
// It is the unit-level half of the api layer's end-to-end proof. If this passes
// and that one fails, the disagreement is in the HTTP limits; if both fail, it
// is here.
func TestABundleAtTheContentCeilingIsReadable(t *testing.T) {
	// Two assets at the per-asset ceiling — the shape the limit is derived from.
	// Zero-filled: the asset entries are STORED, so the bundle is genuinely this
	// big on the wire, which is the property under test.
	assets := map[string][]byte{}
	var layers []wire.Layer
	for i := range 2 {
		body := make([]byte, MaxAssetBytes)
		body[0] = byte(i + 1) // distinct content, therefore distinct refs
		ref := AssetRefOf(body)
		assets[ref] = body
		layers = append(layers, wire.Layer{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: ref})
	}

	var buf bytes.Buffer
	m := Manifest{Cast: CastPayload{
		Name:   "Two video layers",
		Slides: []datamodel.CastSlide{{ID: "s1", Layers: layers}},
	}}
	if err := Write(&buf, m, assets); err != nil {
		t.Fatalf("Write a bundle at the content ceiling: %v", err)
	}
	if int64(buf.Len()) > MaxBundleBytes {
		t.Fatalf("a bundle at the content ceiling is %d bytes, past the %d-byte limit its own reader enforces — an export nobody could import",
			buf.Len(), int64(MaxBundleBytes))
	}
	if int64(buf.Len()) <= MaxAssetBytes {
		t.Fatalf("the bundle is only %d bytes; the assets must be STORED for this test to exercise anything (a deflated run of zeros would prove nothing)", buf.Len())
	}
	got, err := Read(buf.Bytes())
	if err != nil {
		t.Fatalf("Read a bundle this package just wrote: %v", err)
	}
	if len(got.Assets) != 2 {
		t.Fatalf("read back %d asset(s), want 2", len(got.Assets))
	}
}

// TestWriteRefusesAnAssetLargerThanOneEntryMayBe: the write side of the
// per-asset ceiling. Producing an entry Read rejects is the export/import
// disagreement in miniature.
func TestWriteRefusesAnAssetLargerThanOneEntryMayBe(t *testing.T) {
	body := make([]byte, MaxAssetBytes+1)
	ref := AssetRefOf(body)
	err := Write(&bytes.Buffer{}, Manifest{Cast: CastPayload{
		Name:   "One impossible image",
		Slides: []datamodel.CastSlide{{ID: "s1", Layers: []wire.Layer{{Kind: wire.LayerKindImage, W: 1, H: 1, AssetRef: ref}}}},
	}}, map[string][]byte{ref: body})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Write of an oversize asset returned %v, want ErrTooLarge", err)
	}
}

// TestReadRefusesAnEntryLargerThanOneAssetMayBe is the same ceiling on the read
// side, and it is the one that has to hold against a hostile file rather than
// against this package's own writer.
func TestReadRefusesAnEntryLargerThanOneAssetMayBe(t *testing.T) {
	body := make([]byte, MaxAssetBytes+1)
	ref := AssetRefOf(body)
	m := Manifest{
		Format: Format,
		Cast:   CastPayload{Name: "Hand-made", Slides: []datamodel.CastSlide{{ID: "s1", Layers: []wire.Layer{{Kind: wire.LayerKindImage, W: 1, H: 1, AssetRef: ref}}}}},
		Assets: []AssetEntry{{AssetRef: ref, SizeBytes: int64(len(body))}},
	}
	mf, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := zipOf(t, map[string][]byte{ManifestName: mf, entryNameOf(ref): body})
	if _, err := Read(raw); !errors.Is(err, ErrDamaged) && !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Read of an oversize entry returned %v, want a refusal", err)
	}
}
