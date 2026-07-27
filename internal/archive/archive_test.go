package archive

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

// Fixture identifiers. ULIDs are spelled out rather than generated so a failing
// assertion prints a value that is the same on every run and in every log.
const (
	testWorkspaceID = "01J8Z3K4N5P6Q7R8S9T0V1W2ZC"
	testStubID      = "01J8Z3K4N5P6Q7R8S9T0V1W2ZD"
	testSignerKeyID = "01J8Z3K4N5P6Q7R8S9T0V1W2ZB"
	testCreatedAt   = int64(1752537600000)
	testPassphrase  = "correct horse battery staple"
	testSchemaEpoch = 4
)

// lightKDF is the argon2id profile the SUITE runs with: token cost, so a test
// that creates a dozen containers finishes in milliseconds. Production uses
// DefaultKDFParams (256 MiB / 3 passes / 4 lanes) — see TestDefaultKDFParams,
// which pins those numbers so nobody "simplifies" the production profile down to
// this one.
func lightKDF() KDFParams { return KDFParams{MemoryKiB: 8, Iterations: 1, Parallelism: 1} }

// fixedRand is Options.rand for the suite: a fixed byte stream, so a container's
// KDF salt and base nonce are the same on every run and a failure is
// reproducible. It is settable only from inside this package (Options.rand is
// unexported); production leaves it nil and gets crypto/rand.Reader.
func fixedRand() io.Reader {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return bytes.NewReader(b)
}

// testSigner returns a deterministic ed25519 keypair for the workspace signing
// identity ARC-021's signature is produced and verified under.
func testSigner(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(0xA0 + i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatalf("ed25519 private key's public half is %T", priv.Public())
	}
	return pub, priv
}

// memFile is an in-memory io.WriteSeeker, which is what Create requires so it can
// rewrite the header in place over its fixed-width placeholder (ARC-080).
type memFile struct {
	buf []byte
	pos int64
}

func (f *memFile) Write(p []byte) (int, error) {
	end := f.pos + int64(len(p))
	if end > int64(len(f.buf)) {
		f.buf = append(f.buf, make([]byte, end-int64(len(f.buf)))...)
	}
	copy(f.buf[f.pos:end], p)
	f.pos = end
	return len(p), nil
}

func (f *memFile) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.pos + offset
	case io.SeekEnd:
		abs = int64(len(f.buf)) + offset
	default:
		return 0, fmt.Errorf("memFile: bad whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("memFile: negative position %d", abs)
	}
	f.pos = abs
	return abs, nil
}

// refOf builds an asset_ref for fixture bytes, in the `sha256:<hex>` form
// ARC-060 requires.
func refOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fixture is one test workspace: the bytes each entry carries, kept beside the
// Source that describes them so an assertion can compare against the ORIGINAL
// bytes rather than against anything the container produced.
type fixture struct {
	snapshot []byte
	assets   map[string][]byte // asset_ref -> bytes
	src      Source
	// wrapped records what the fixture's WrapDataKey callback actually returned
	// for THIS container. An assertion compares the manifest against it rather
	// than against a hard-coded string, which is what proves the carried value is
	// a function of this archive's own derivation (ARC-011's data-key sub-key,
	// itself a function of this archive's random salt) rather than a constant a
	// broken Create could still have emitted. It is a pointer so the closure
	// installed in Source keeps writing to the fixture a caller holds by value.
	wrapped *string
}

// testWrapDataKey stands in for the platform key hierarchy's own data-key wrap
// (ARC-071): archive/1 hands it the sub-key ARC-011 derives for that purpose and
// carries whatever comes back as opaque bytes. It asserts nothing about the
// wrapping ALGORITHM — that is deliberately outside this contract's scope — only
// that a wrap happened under the key it was given, which is what makes the
// emitted value a function of this archive's own derivation rather than a
// constant a test could have hard-coded.
func testWrapDataKey(wrapKey []byte) (string, error) {
	if len(wrapKey) == 0 {
		return "", fmt.Errorf("test: empty data-key wrapping key")
	}
	sum := sha256.Sum256(append([]byte("test-data-key-wrap"), wrapKey...))
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// exportKeys re-derives the export sub-keys any container createContainer wrote
// is encrypted and wrapped under. The suite pins Options.rand, so this archive's
// KDF salt is the first saltSize bytes of that stream and its cost profile is
// lightKDF — both EXPLICIT inputs. Deriving the expectation from them, rather
// than from the header or the wrap the container emitted, is what keeps the
// assertion independent of the value under test.
func exportKeys(t *testing.T) Keys {
	t.Helper()
	salt := make([]byte, saltSize)
	if _, err := io.ReadFull(fixedRand(), salt); err != nil {
		t.Fatalf("read the pinned kdf salt: %v", err)
	}
	keys, err := DeriveKeys(testPassphrase, lightKDF(), salt)
	if err != nil {
		t.Fatalf("DeriveKeys: %v", err)
	}
	return keys
}

// newFixture builds a workspace with a snapshot, two embedded assets, and one
// by-reference asset (whose bytes ARC-061 forbids the container from carrying).
func newFixture() *fixture {
	snapshot := bytes.Repeat([]byte("SQLite format 3\x00 workspace rows "), 64)
	assetA := bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 300)
	assetB := []byte("a small text asset\n")
	remote := []byte("bytes that live in the destination's own asset store")

	f := &fixture{
		snapshot: snapshot,
		assets: map[string][]byte{
			refOf(assetA): assetA,
			refOf(assetB): assetB,
		},
		wrapped: new(string),
	}
	f.src = Source{
		CreatedAt:           testCreatedAt,
		WorkspaceID:         testWorkspaceID,
		PlatformSchemaEpoch: testSchemaEpoch,
		Packs: []PackLock{
			{PackID: "waiveo/slidecast", Version: "2.2.0", Channel: "first-party", Source: "https://index.example/waiveo", SchemaEpoch: 3},
			{PackID: "acme/weather-widget", Version: "1.2.0", Channel: "verified", Source: "https://index.example/community", SchemaEpoch: 1},
		},
		Assets: []AssetEntry{
			{AssetRef: refOf(assetA), Size: int64(len(assetA)), ContentType: "image/png", Storage: StorageEmbedded},
			{AssetRef: refOf(assetB), Size: int64(len(assetB)), ContentType: "text/plain", Storage: StorageEmbedded},
			{AssetRef: refOf(remote), Size: int64(len(remote)), ContentType: "video/mp4", Storage: StorageByReference},
		},
		SecretStubs: []SecretStub{{StubID: testStubID, WrappedValue: "AQIDBAUGBwgJCgsMDQ4PEA"}},
		Snapshot: func() (io.ReadCloser, int64, error) {
			return io.NopCloser(bytes.NewReader(snapshot)), int64(len(snapshot)), nil
		},
	}
	// The data-key wrap ARC-071 requires, recorded as it is produced so an
	// assertion can compare the manifest against what actually came back rather
	// than against a constant.
	f.src.WrapDataKey = func(wrapKey []byte) (string, error) {
		v, err := testWrapDataKey(wrapKey)
		if err != nil {
			return "", err
		}
		*f.wrapped = v
		return v, nil
	}
	f.src.Asset = func(ref string) (io.ReadCloser, int64, error) {
		b, ok := f.assets[ref]
		if !ok {
			return nil, 0, fmt.Errorf("no such fixture asset %s", ref)
		}
		return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
	}
	return f
}

// createContainer writes fixture f to a container and returns its bytes.
func createContainer(t *testing.T, f *fixture, opt Options) []byte {
	t.Helper()
	if opt.KDF == (KDFParams{}) {
		opt.KDF = lightKDF()
	}
	if opt.rand == nil {
		opt.rand = fixedRand()
	}
	if opt.Passphrase == "" {
		opt.Passphrase = testPassphrase
	}
	if opt.SignerKeyID == "" {
		opt.SignerKeyID = testSignerKeyID
	}
	if opt.Signer == nil {
		_, priv := testSigner(t)
		opt.Signer = priv
	}
	var out memFile
	if err := Create(&out, f.src, opt); err != nil {
		t.Fatalf("Create() error = %v, want nil", err)
	}
	return out.buf
}

// splitContainer returns a container's header JSON and its encrypted body
// region (ARC-001).
func splitContainer(t *testing.T, container []byte) (headerJSON, body []byte) {
	t.Helper()
	if len(container) < 4 {
		t.Fatalf("container is %d bytes, too short to hold a length prefix", len(container))
	}
	n := int(binary.BigEndian.Uint32(container[:4]))
	if 4+n > len(container) {
		t.Fatalf("container declares a %d-byte header but is only %d bytes", n, len(container))
	}
	return container[4 : 4+n], container[4+n:]
}

// resignHeader rebuilds a container with a mutated outer header that is VALIDLY
// SIGNED — the only way to test a check that sits BEHIND signature verification
// (ARC-024's digest recompute, ARC-004's newer-minor tolerance) without the
// signature check firing first and masking it. The body is copied verbatim.
func resignHeader(t *testing.T, container []byte, priv ed25519.PrivateKey, mutate func(map[string]any)) []byte {
	t.Helper()
	headerJSON, body := splitContainer(t, container)

	dec := json.NewDecoder(bytes.NewReader(headerJSON))
	dec.UseNumber()
	var h map[string]any
	if err := dec.Decode(&h); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	mutate(h)

	signable := make(map[string]any, len(h))
	for k, v := range h {
		if k != "signature" {
			signable[k] = v
		}
	}
	signed, err := json.Marshal(signable)
	if err != nil {
		t.Fatalf("marshal signable header: %v", err)
	}
	h["signature"] = b64.EncodeToString(ed25519.Sign(priv, signed))

	out, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	var buf bytes.Buffer
	var lenBE [4]byte
	binary.BigEndian.PutUint32(lenBE[:], uint32(len(out)))
	buf.Write(lenBE[:])
	buf.Write(out)
	buf.Write(body)
	return buf.Bytes()
}

// frameBounds returns each body frame's [start, end) offsets within container,
// by walking the 4-byte length prefixes (ARC-013). It is how the truncation
// tests cut on an exact frame boundary rather than at an arbitrary byte.
func frameBounds(t *testing.T, container []byte) [][2]int {
	t.Helper()
	_, body := splitContainer(t, container)
	base := len(container) - len(body)

	var bounds [][2]int
	for off := 0; off < len(body); {
		if off+4 > len(body) {
			t.Fatalf("body ends inside a frame length prefix at offset %d", off)
		}
		n := int(binary.BigEndian.Uint32(body[off : off+4]))
		end := off + 4 + n
		if end > len(body) {
			t.Fatalf("frame at offset %d claims %d bytes but the body ends first", off, n)
		}
		bounds = append(bounds, [2]int{base + off, base + end})
		off = end
	}
	return bounds
}

// TestCreateOpenRoundTrip is the whole contract's happy path: a container
// written by Create is read back by Open with its header signature verified, its
// frames authenticated, its body digest recomputed and matched (ARC-024), its
// manifest validated (ARC-031), and every embedded asset's hash checked against
// its own asset_ref (ARC-062) — recovering the manifest and every entry's bytes
// exactly.
func TestCreateOpenRoundTrip(t *testing.T) {
	f := newFixture()
	pub, _ := testSigner(t)
	container := createContainer(t, f, Options{})

	manifest, entries, err := Open(bytes.NewReader(container), testPassphrase, pub)
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}

	// Every assertion below compares against the explicit fixture constants the
	// Source was built from, never against a value read back out of the
	// container.
	if manifest.Mode != ModeFull {
		t.Errorf("manifest.Mode = %q, want %q", manifest.Mode, ModeFull)
	}
	if manifest.CreatedAt != testCreatedAt {
		t.Errorf("manifest.CreatedAt = %d, want %d", manifest.CreatedAt, testCreatedAt)
	}
	if manifest.WorkspaceID != testWorkspaceID {
		t.Errorf("manifest.WorkspaceID = %q, want %q", manifest.WorkspaceID, testWorkspaceID)
	}
	if manifest.PlatformSchemaEpoch != testSchemaEpoch {
		t.Errorf("manifest.PlatformSchemaEpoch = %d, want %d", manifest.PlatformSchemaEpoch, testSchemaEpoch)
	}
	if manifest.BaseArchive != nil {
		t.Errorf("manifest.BaseArchive = %+v, want nil for a full-mode archive", manifest.BaseArchive)
	}
	if len(manifest.Packs) != 2 || manifest.Packs[0].PackID != "waiveo/slidecast" || manifest.Packs[1].Version != "1.2.0" {
		t.Errorf("manifest.Packs = %+v, want the two fixture locks in order", manifest.Packs)
	}
	if len(manifest.SecretStubs) != 1 || manifest.SecretStubs[0].StubID != testStubID ||
		manifest.SecretStubs[0].WrappedValue != "AQIDBAUGBwgJCgsMDQ4PEA" {
		t.Errorf("manifest.SecretStubs = %+v, want the fixture stub carried unchanged (ARC-070)", manifest.SecretStubs)
	}
	// ARC-071's wrap is produced by the caller's key hierarchy under the sub-key
	// ARC-011 derives, and carried here unchanged. The expected value is
	// recomputed from that same derivation rather than hard-coded, which is what
	// proves the emitted wrap is a function of THIS archive's own salt and
	// passphrase — a constant would pass even if Create handed the wrapper the
	// wrong key, or no key at all.
	if *f.wrapped == "" {
		t.Fatal("Create never invoked Source.WrapDataKey — nothing re-wrapped the data key under ARC-011's data-key sub-key (ARC-071)")
	}
	wantWrap, err := testWrapDataKey(exportKeys(t).DataKeyWrap)
	if err != nil {
		t.Fatalf("recompute expected data-key wrap: %v", err)
	}
	if manifest.DataKeyWrap.WrappedValue != wantWrap {
		t.Errorf("manifest.DataKeyWrap.WrappedValue = %q, want %q — the wrap archive/1 carried under ARC-011's data-key sub-key",
			manifest.DataKeyWrap.WrappedValue, wantWrap)
	}
	// And it is the value the caller's own hierarchy actually produced, carried
	// through unchanged (ARC-071).
	if manifest.DataKeyWrap.WrappedValue != *f.wrapped {
		t.Errorf("manifest.DataKeyWrap.WrappedValue = %q, but the fixture's wrapper returned %q",
			manifest.DataKeyWrap.WrappedValue, *f.wrapped)
	}
	if bytes.Equal(exportKeys(t).DataKeyWrap, exportKeys(t).Body) {
		t.Error("the data-key sub-key handed to WrapDataKey is the body key — ARC-011 requires two independent sub-keys")
	}

	// ARC-030: the manifest is the first entry. ARC-061: exactly the two
	// `embedded` assets have entries; the by-reference one does not.
	wantNames := []string{ManifestEntryName, SnapshotEntryName}
	for _, a := range f.src.Assets {
		if a.Storage == StorageEmbedded {
			wantNames = append(wantNames, assetEntryName(a.AssetRef))
		}
	}
	if len(entries) != len(wantNames) {
		t.Fatalf("Open() returned %d entries (%s), want %d (%s)",
			len(entries), entryNames(entries), len(wantNames), strings.Join(wantNames, ", "))
	}
	for i, want := range wantNames {
		if entries[i].Name != want {
			t.Errorf("entries[%d].Name = %q, want %q", i, entries[i].Name, want)
		}
	}

	if !bytes.Equal(entries[1].Body, f.snapshot) {
		t.Errorf("workspace.sqlite body = %d bytes, want the fixture snapshot's %d bytes byte-for-byte",
			len(entries[1].Body), len(f.snapshot))
	}
	for _, e := range entries[2:] {
		ref, ok := assetRefFromEntryName(e.Name)
		if !ok {
			t.Fatalf("entry %q is not an asset entry", e.Name)
		}
		if !bytes.Equal(e.Body, f.assets[ref]) {
			t.Errorf("asset %s body = %d bytes, want the fixture's %d bytes byte-for-byte",
				ref, len(e.Body), len(f.assets[ref]))
		}
	}

	// The manifest entry's bytes are exactly the marshaling of the manifest the
	// fixture describes, which pins the wire shape (field names and order) too.
	wantManifest, err := json.Marshal(Manifest{
		CreatedAt:           testCreatedAt,
		Mode:                ModeFull,
		WorkspaceID:         testWorkspaceID,
		PlatformSchemaEpoch: testSchemaEpoch,
		Packs:               f.src.Packs,
		Assets:              f.src.Assets,
		SecretStubs:         f.src.SecretStubs,
		DataKeyWrap:         DataKeyWrap{WrappedValue: wantWrap},
	})
	if err != nil {
		t.Fatalf("marshal expected manifest: %v", err)
	}
	if !bytes.Equal(entries[0].Body, wantManifest) {
		t.Errorf("manifest.json body =\n%s\nwant\n%s", entries[0].Body, wantManifest)
	}
}

func entryNames(entries []Entry) string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return strings.Join(names, ", ")
}

// TestCreateHeaderIsSignedInPlace proves the placeholder-rewrite (ARC-080) left
// a real header behind: the length prefix still counts the header exactly, and
// neither fixed-width placeholder survived into the written file. A rewrite that
// silently failed would leave the placeholders in place and produce a container
// whose signature verifies against nothing.
func TestCreateHeaderIsSignedInPlace(t *testing.T) {
	f := newFixture()
	container := createContainer(t, f, Options{})
	headerJSON, body := splitContainer(t, container)

	var h Header
	if err := json.Unmarshal(headerJSON, &h); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if h.Digest == placeholderDigest {
		t.Error("header.Digest is still the placeholder — the in-place rewrite did not happen")
	}
	if h.Signature == placeholderSignature {
		t.Error("header.Signature is still the placeholder — the in-place rewrite did not happen")
	}
	if h.Format != FormatMagic {
		t.Errorf("header.Format = %q, want %q", h.Format, FormatMagic)
	}
	if h.ArchiveFormatVersion != FormatVersion {
		t.Errorf("header.ArchiveFormatVersion = %q, want %q", h.ArchiveFormatVersion, FormatVersion)
	}
	if h.KDF.Algorithm != KDFAlgorithm {
		t.Errorf("header.KDF.Algorithm = %q, want %q", h.KDF.Algorithm, KDFAlgorithm)
	}
	if h.SignerKeyID != testSignerKeyID {
		t.Errorf("header.SignerKeyID = %q, want %q", h.SignerKeyID, testSignerKeyID)
	}

	// ARC-020: `digest` is the sha256 of the encrypted body region exactly as it
	// appears in the container — recomputable with no passphrase at all.
	sum := sha256.Sum256(body)
	if want := hex.EncodeToString(sum[:]); h.Digest != want {
		t.Errorf("header.Digest = %s, want %s (sha256 of the body region)", h.Digest, want)
	}

	// ARC-017: the base nonce is fresh random material, not something derived
	// from the passphrase or the workspace id. It comes from Options.rand, which
	// the suite pins — so it must equal that stream's bytes 16..40 (the salt
	// takes the first 16).
	wantRand := make([]byte, 256)
	for i := range wantRand {
		wantRand[i] = byte(i * 7)
	}
	if got := b64.EncodeToString(wantRand[saltSize : saltSize+24]); h.BaseNonce != got {
		t.Errorf("header.BaseNonce = %q, want %q — the base nonce must come from Options.rand (ARC-017)", h.BaseNonce, got)
	}
}

// TestOpenWrongPassphrase is ARC-015/074: a wrong export passphrase derives a
// wrong body key, so the first frame fails AEAD authentication — and that MUST
// surface as DECRYPT_FAILED ("retype the passphrase"), never as
// ARCHIVE_SIGNATURE_INVALID ("go find an untampered copy"). Reporting the two as
// one undifferentiated error is precisely what the contract forbids, so this
// test asserts both the code that IS returned and the one that must not be.
func TestOpenWrongPassphrase(t *testing.T) {
	f := newFixture()
	pub, _ := testSigner(t)
	container := createContainer(t, f, Options{})

	_, _, err := Open(bytes.NewReader(container), "not the export passphrase", pub)
	if got := Code(err); got != CodeDecryptFailed {
		t.Fatalf("Open() with a wrong passphrase = %v (code %q), want code %q", err, got, CodeDecryptFailed)
	}
	if Code(err) == CodeArchiveSignatureInvalid {
		t.Fatal("a wrong passphrase reported as ARCHIVE_SIGNATURE_INVALID (ARC-015)")
	}
}

// TestOpenTamperedHeaderFailsSignature is ARC-021/023: the signature covers the
// ENTIRE cleartext header, so mutating any field of it — not just `digest` —
// fails verification.
//
// The `kdf` cases carry the requirement's real point. ARC-021 requires the
// signature to be verified BEFORE the KDF is invoked with any `kdf`-supplied
// parameter, and two sub-cases prove the ordering rather than assuming it:
//
//   - "kdf algorithm" swaps argon2id for a value this implementation rejects. If
//     the header's crypto material were consumed before the signature check,
//     Open would refuse with the plain "declares kdf algorithm" error and no
//     taxonomy code at all. Getting ARCHIVE_SIGNATURE_INVALID is deterministic
//     proof the signature ran first.
//   - "kdf memory_kib" inflates the cost parameter to 16 GiB. If it were honored
//     before verification, argon2id would try to allocate 16 GiB and this test
//     would hang or die rather than fail — which is the loud failure ARC-021
//     exists to prevent in production.
func TestOpenTamperedHeaderFailsSignature(t *testing.T) {
	f := newFixture()
	pub, _ := testSigner(t)
	container := createContainer(t, f, Options{})

	tests := map[string]func(h map[string]any){
		"kdf memory_kib": func(h map[string]any) {
			h["kdf"].(map[string]any)["memory_kib"] = json.Number("16777216")
		},
		"kdf iterations": func(h map[string]any) {
			h["kdf"].(map[string]any)["iterations"] = json.Number("999999")
		},
		"kdf algorithm": func(h map[string]any) {
			h["kdf"].(map[string]any)["algorithm"] = "scrypt"
		},
		"kdf salt": func(h map[string]any) {
			h["kdf"].(map[string]any)["salt"] = b64.EncodeToString(bytes.Repeat([]byte{0xFF}, saltSize))
		},
		"base_nonce": func(h map[string]any) {
			h["base_nonce"] = b64.EncodeToString(bytes.Repeat([]byte{0x01}, 24))
		},
		"digest": func(h map[string]any) {
			h["digest"] = strings.Repeat("a", 64)
		},
		"signer_key_id": func(h map[string]any) {
			h["signer_key_id"] = "01J8Z3K4N5P6Q7R8S9T0V1W2ZZ"
		},
		"an added field": func(h map[string]any) {
			h["injected"] = "a field the exporter never signed"
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			headerJSON, body := splitContainer(t, container)
			dec := json.NewDecoder(bytes.NewReader(headerJSON))
			dec.UseNumber()
			var h map[string]any
			if err := dec.Decode(&h); err != nil {
				t.Fatalf("decode header: %v", err)
			}
			mutate(h)
			mutated, err := json.Marshal(h)
			if err != nil {
				t.Fatalf("marshal mutated header: %v", err)
			}

			var buf bytes.Buffer
			var lenBE [4]byte
			binary.BigEndian.PutUint32(lenBE[:], uint32(len(mutated)))
			buf.Write(lenBE[:])
			buf.Write(mutated)
			buf.Write(body)

			_, _, err = Open(bytes.NewReader(buf.Bytes()), testPassphrase, pub)
			if got := Code(err); got != CodeArchiveSignatureInvalid {
				t.Fatalf("Open() with a tampered %s = %v (code %q), want code %q", name, err, got, CodeArchiveSignatureInvalid)
			}
		})
	}
}

// TestOpenWrongSignerKey is the other half of ARC-023: a container signed by a
// different workspace's identity is refused, because a signature that verifies
// against nothing the reader trusts is no signature at all. The wrong-length key
// case also pins fail-closed behavior — crypto/ed25519.Verify panics on one, and
// a panic mid-verification is not a refusal.
func TestOpenWrongSignerKey(t *testing.T) {
	f := newFixture()
	container := createContainer(t, f, Options{})

	otherSeed := make([]byte, ed25519.SeedSize)
	for i := range otherSeed {
		otherSeed[i] = byte(0x5B - i)
	}
	otherPub, ok := ed25519.NewKeyFromSeed(otherSeed).Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("public half is not an ed25519.PublicKey")
	}

	tests := map[string]ed25519.PublicKey{
		"a different workspace's key": otherPub,
		"an empty key":                {},
		"a truncated key":             make(ed25519.PublicKey, ed25519.PublicKeySize-1),
	}
	for name, pub := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := Open(bytes.NewReader(container), testPassphrase, pub)
			if got := Code(err); got != CodeArchiveSignatureInvalid {
				t.Fatalf("Open() with %s = %v (code %q), want code %q", name, err, got, CodeArchiveSignatureInvalid)
			}
		})
	}
}

// TestOpenBodyDigestMismatch is ARC-024: the digest is recomputed over the body's
// ACTUAL streamed bytes and compared to the header's `digest`, refusing on
// mismatch.
//
// The header here is mutated AND VALIDLY RE-SIGNED, which is the only way to
// reach this check — a plain mutation would be caught by ARC-023 first. That is
// exactly the gap ARC-024 exists to close: a valid signature proves only that the
// recorded digest value was signed, never that the bytes delivered are the bytes
// it describes.
func TestOpenBodyDigestMismatch(t *testing.T) {
	f := newFixture()
	pub, priv := testSigner(t)
	container := createContainer(t, f, Options{})

	tampered := resignHeader(t, container, priv, func(h map[string]any) {
		h["digest"] = strings.Repeat("b", 64)
	})

	_, _, err := Open(bytes.NewReader(tampered), testPassphrase, pub)
	if got := Code(err); got != CodeArchiveSignatureInvalid {
		t.Fatalf("Open() with a validly-signed wrong digest = %v (code %q), want code %q", err, got, CodeArchiveSignatureInvalid)
	}
}

// TestOpenTruncatedOrExtended is ARC-016: a frame sequence shorter or longer than
// the one produced at export refuses with ARCHIVE_TRUNCATED — distinguishable
// from both DECRYPT_FAILED and ARCHIVE_SIGNATURE_INVALID, neither of which means
// content is missing.
//
// "final frame dropped" is the case per-frame authentication provably cannot
// catch: every remaining frame still authenticates individually, so only the
// explicit search for an authenticated final-marked frame notices the tail is
// gone. "a byte appended" is its mirror: a byte after the final frame is caught
// only by looking for it.
func TestOpenTruncatedOrExtended(t *testing.T) {
	f := newFixture()
	pub, _ := testSigner(t)
	// A small frame size forces a multi-frame body, so "drop the last frame"
	// leaves earlier frames that all authenticate.
	container := createContainer(t, f, Options{FrameSize: 256})

	bounds := frameBounds(t, container)
	if len(bounds) < 2 {
		t.Fatalf("fixture produced %d frame(s); this test needs at least 2", len(bounds))
	}
	headerLen := bounds[0][0]

	tests := map[string][]byte{
		"final frame dropped":       container[:bounds[len(bounds)-1][0]],
		"last byte removed":         container[:len(container)-1],
		"body truncated to nothing": container[:headerLen],
		"half the body removed":     container[:headerLen+(len(container)-headerLen)/2],
		"a byte appended":           append(append([]byte{}, container...), 0x00),
		"a whole frame's worth added": append(append([]byte{}, container...),
			container[bounds[0][0]:bounds[0][1]]...),
	}

	for name, mutated := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := Open(bytes.NewReader(mutated), testPassphrase, pub)
			if got := Code(err); got != CodeArchiveTruncated {
				t.Fatalf("Open() with %s = %v (code %q), want code %q", name, err, got, CodeArchiveTruncated)
			}
		})
	}
}

// TestOpenFlippedCiphertextByte is ARC-014: a frame that fails AEAD
// authentication aborts the read immediately with DECRYPT_FAILED, emitting no
// plaintext from it. The flip is applied inside the first frame's ciphertext, so
// the failure happens before any entry could have been produced.
func TestOpenFlippedCiphertextByte(t *testing.T) {
	f := newFixture()
	pub, _ := testSigner(t)
	container := createContainer(t, f, Options{FrameSize: 256})
	bounds := frameBounds(t, container)

	tests := map[string]int{
		"first frame, first ciphertext byte": bounds[0][0] + 4,
		"first frame, last byte (its tag)":   bounds[0][1] - 1,
		"a later frame":                      bounds[len(bounds)-1][0] + 4,
	}

	for name, at := range tests {
		t.Run(name, func(t *testing.T) {
			mutated := append([]byte{}, container...)
			mutated[at] ^= 0x01

			_, entries, err := Open(bytes.NewReader(mutated), testPassphrase, pub)
			if got := Code(err); got != CodeDecryptFailed {
				t.Fatalf("Open() with a flipped byte at %d = %v (code %q), want code %q", at, err, got, CodeDecryptFailed)
			}
			if entries != nil {
				t.Errorf("Open() returned %d entries alongside a DECRYPT_FAILED refusal, want none (ARC-014)", len(entries))
			}
		})
	}
}

// TestOpenEmbeddedAssetHashMismatch is ARC-062: an asset is trusted only by
// matching its own name. The fixture declares one asset's asset_ref as the hash
// of bytes the container does not actually carry (same length, so the size check
// cannot be what catches it), and the restore must refuse with MANIFEST_INVALID.
func TestOpenEmbeddedAssetHashMismatch(t *testing.T) {
	pub, _ := testSigner(t)

	declaredBytes := []byte("the bytes this asset_ref names___")
	carriedBytes := []byte("entirely different bytes, same len")[:len(declaredBytes)]
	if bytes.Equal(declaredBytes, carriedBytes) || len(declaredBytes) != len(carriedBytes) {
		t.Fatalf("fixture setup: the two byte strings must differ but share a length")
	}
	ref := refOf(declaredBytes)

	snapshot := []byte("SQLite format 3\x00")
	src := Source{
		CreatedAt:           testCreatedAt,
		WorkspaceID:         testWorkspaceID,
		PlatformSchemaEpoch: testSchemaEpoch,
		Packs:               []PackLock{},
		Assets: []AssetEntry{
			{AssetRef: ref, Size: int64(len(declaredBytes)), Storage: StorageEmbedded},
		},
		SecretStubs: []SecretStub{},
		WrapDataKey: testWrapDataKey,
		Snapshot: func() (io.ReadCloser, int64, error) {
			return io.NopCloser(bytes.NewReader(snapshot)), int64(len(snapshot)), nil
		},
		Asset: func(string) (io.ReadCloser, int64, error) {
			// The lie: bytes that are not the ones asset_ref names.
			return io.NopCloser(bytes.NewReader(carriedBytes)), int64(len(carriedBytes)), nil
		},
	}

	container := createContainer(t, &fixture{src: src}, Options{})

	_, _, err := Open(bytes.NewReader(container), testPassphrase, pub)
	if got := Code(err); got != CodeManifestInvalid {
		t.Fatalf("Open() with an asset whose bytes do not match its asset_ref = %v (code %q), want code %q", err, got, CodeManifestInvalid)
	}
}

// TestOpenForeignFormat is ARC-003: a `format` value other than
// `waiveo-archive` — or a file that is not one of ours at all — is rejected
// immediately, before anything past the outer header is parsed.
//
// Note what the "a foreign format" case proves about ORDERING: mutating `format`
// also invalidates the signature, yet the code is FORMAT_UNRECOGNIZED, because
// ARC-003's check precedes ARC-023's.
func TestOpenForeignFormat(t *testing.T) {
	f := newFixture()
	pub, priv := testSigner(t)
	container := createContainer(t, f, Options{})

	tests := map[string][]byte{
		"a foreign format value": resignHeader(t, container, priv, func(h map[string]any) {
			h["format"] = "some-other-archive"
		}),
		"a validly-signed foreign format": resignHeader(t, container, priv, func(h map[string]any) {
			h["format"] = "tar.gz"
		}),
		"an empty file":             {},
		"a two-byte file":           {0x00, 0x01},
		"a header that is not JSON": append([]byte{0x00, 0x00, 0x00, 0x05}, []byte("hello")...),
		"an absurd header length":   {0xFF, 0xFF, 0xFF, 0xFF, 0x00},
		"a zero header length":      {0x00, 0x00, 0x00, 0x00},
		"a header that ends early":  append([]byte{0x00, 0x00, 0x01, 0x00}, []byte("{}")...),
	}

	for name, mutated := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := Open(bytes.NewReader(mutated), testPassphrase, pub)
			if got := Code(err); got != CodeFormatUnrecognized {
				t.Fatalf("Open() with %s = %v (code %q), want code %q", name, err, got, CodeFormatUnrecognized)
			}
		})
	}
}

// TestOpenMajorVersionMismatch is ARC-004: a differing MAJOR component refuses
// with VERSION_UNSUPPORTED before the encrypted body is read. Like ARC-003's
// check, it precedes signature verification, so even a validly re-signed header
// declaring another major is refused on the version, not the signature.
func TestOpenMajorVersionMismatch(t *testing.T) {
	f := newFixture()
	pub, priv := testSigner(t)
	container := createContainer(t, f, Options{})

	tests := map[string]string{
		"a newer major":    "2.0",
		"an older major":   "0.9",
		"a far-off major":  "17.3",
		"no minor at all":  "1",
		"not a version":    "one point oh",
		"an empty version": "",
		"a negative minor": "1.-1",
		"three components": "1.0.0",
	}

	for name, version := range tests {
		t.Run(name, func(t *testing.T) {
			mutated := resignHeader(t, container, priv, func(h map[string]any) {
				h["archive_format_version"] = version
			})
			_, _, err := Open(bytes.NewReader(mutated), testPassphrase, pub)
			if got := Code(err); got != CodeVersionUnsupported {
				t.Fatalf("Open() with %s (%q) = %v (code %q), want code %q", name, version, err, got, CodeVersionUnsupported)
			}
		})
	}
}

// TestOpenNewerMinorTolerated is ARC-004's other half, and ARC-032's on the
// header: a reader MAY accept a container whose MINOR is newer than its own,
// tolerating additive fields it does not recognize. The unknown header field is
// added inside the signature's scope (it is re-signed), because the
// canonicalization covers every field actually present — the property that keeps
// an additive field from becoming an unauthenticated slot.
func TestOpenNewerMinorTolerated(t *testing.T) {
	f := newFixture()
	pub, priv := testSigner(t)
	container := createContainer(t, f, Options{})

	mutated := resignHeader(t, container, priv, func(h map[string]any) {
		h["archive_format_version"] = "1.9"
		h["compression_dictionary_id"] = "a field a future minor added"
	})

	manifest, entries, err := Open(bytes.NewReader(mutated), testPassphrase, pub)
	if err != nil {
		t.Fatalf("Open() on a newer-minor container = %v, want nil (ARC-004)", err)
	}
	if manifest.WorkspaceID != testWorkspaceID {
		t.Errorf("manifest.WorkspaceID = %q, want %q", manifest.WorkspaceID, testWorkspaceID)
	}
	if len(entries) == 0 {
		t.Error("Open() returned no entries")
	}
}

// TestFrameSequenceHasExactlyOneFinalFrame is ARC-013 checked against the actual
// bytes: every frame authenticates under the non-final associated data except the
// LAST, which authenticates only under the final one. A container with two final
// frames, or with none, would fail here even though every individual frame
// authenticates — which is the whole reason ARC-016's check exists separately
// from ARC-014's.
func TestFrameSequenceHasExactlyOneFinalFrame(t *testing.T) {
	f := newFixture()
	container := createContainer(t, f, Options{FrameSize: 256})

	headerJSON, _ := splitContainer(t, container)
	var h Header
	if err := json.Unmarshal(headerJSON, &h); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	salt, err := b64.DecodeString(h.KDF.Salt)
	if err != nil {
		t.Fatalf("decode salt: %v", err)
	}
	baseNonce, err := b64.DecodeString(h.BaseNonce)
	if err != nil {
		t.Fatalf("decode base_nonce: %v", err)
	}
	keys, err := DeriveKeys(testPassphrase, lightKDF(), salt)
	if err != nil {
		t.Fatalf("DeriveKeys: %v", err)
	}
	aead, err := chacha20poly1305.NewX(keys.Body)
	if err != nil {
		t.Fatalf("init AEAD: %v", err)
	}

	bounds := frameBounds(t, container)
	if len(bounds) < 2 {
		t.Fatalf("fixture produced %d frame(s); this test needs at least 2", len(bounds))
	}
	for i, b := range bounds {
		ct := container[b[0]+4 : b[1]]
		nonce := deriveFrameNonce(baseNonce, uint64(i))
		_, nonFinalErr := aead.Open(nil, nonce, ct, aadNonFinal)
		_, finalErr := aead.Open(nil, nonce, ct, aadFinal)

		last := i == len(bounds)-1
		switch {
		case last && finalErr != nil:
			t.Errorf("frame %d is the last one but does not authenticate as final: %v", i, finalErr)
		case last && nonFinalErr == nil:
			t.Errorf("frame %d authenticates as BOTH final and non-final", i)
		case !last && nonFinalErr != nil:
			t.Errorf("frame %d is not the last one but does not authenticate as non-final: %v", i, nonFinalErr)
		case !last && finalErr == nil:
			t.Errorf("frame %d is not the last one but authenticates as final", i)
		}
	}
}

// TestCreateEmitsAtLeastOneFrame pins the "always emit, even with nothing left
// to emit" rule in the frame writer's Close: a reader looks for an authenticated
// final-marked frame (ARC-016), and a body with no frames at all would give it
// nothing to find.
func TestCreateEmitsAtLeastOneFrame(t *testing.T) {
	f := newFixture()
	// A frame size larger than the whole compressed body means nothing is ever
	// flushed mid-stream, so the sole frame comes from Close.
	container := createContainer(t, f, Options{FrameSize: 1 << 20})
	if got := len(frameBounds(t, container)); got != 1 {
		t.Fatalf("body carries %d frames, want exactly 1 for this fixture and frame size", got)
	}

	pub, _ := testSigner(t)
	if _, _, err := Open(bytes.NewReader(container), testPassphrase, pub); err != nil {
		t.Fatalf("Open() on a single-frame container = %v, want nil", err)
	}
}

// TestRoundTripAcrossFrameSizes confirms the framing is transparent: the same
// workspace round-trips byte-for-byte whether it lands in one frame or hundreds.
func TestRoundTripAcrossFrameSizes(t *testing.T) {
	pub, _ := testSigner(t)
	for _, frameSize := range []int{1, 17, 256, 4096, 1 << 20} {
		t.Run(fmt.Sprintf("frame size %d", frameSize), func(t *testing.T) {
			f := newFixture()
			container := createContainer(t, f, Options{FrameSize: frameSize})
			_, entries, err := Open(bytes.NewReader(container), testPassphrase, pub)
			if err != nil {
				t.Fatalf("Open() = %v, want nil", err)
			}
			if len(entries) != 4 {
				t.Fatalf("Open() returned %d entries (%s), want 4", len(entries), entryNames(entries))
			}
			if !bytes.Equal(entries[1].Body, f.snapshot) {
				t.Error("workspace.sqlite did not round-trip byte-for-byte")
			}
		})
	}
}

// TestCreateRejectsMalformedInput confirms Create refuses to emit a container its
// own reader would refuse: the manifest is built and validated before a byte is
// written (ARC-031/033), and the signing material is checked at the edge.
func TestCreateRejectsMalformedInput(t *testing.T) {
	_, priv := testSigner(t)

	tests := map[string]struct {
		mutate func(*Source)
		opt    Options
	}{
		"a non-ULID workspace_id":      {mutate: func(s *Source) { s.WorkspaceID = "not-a-ulid" }},
		"a zero platform_schema_epoch": {mutate: func(s *Source) { s.PlatformSchemaEpoch = 0 }},
		"a zero created_at":            {mutate: func(s *Source) { s.CreatedAt = 0 }},
		"a duplicate pack_id": {mutate: func(s *Source) {
			s.Packs = append(s.Packs, PackLock{PackID: "waiveo/slidecast", Version: "9.9.9", Channel: "dev", Source: "x", SchemaEpoch: 1})
		}},
		"an inherited asset in a full archive": {mutate: func(s *Source) {
			s.Assets[0].Storage = StorageInherited
		}},
		"a malformed asset_ref": {mutate: func(s *Source) {
			s.Assets[0].AssetRef = "sha256:NOTHEX"
		}},
		"an empty data_key_wrap": {mutate: func(s *Source) {
			s.WrapDataKey = func([]byte) (string, error) { return "", nil }
		}},
		"no way to wrap the data key": {mutate: func(s *Source) { s.WrapDataKey = nil }},
		"a missing snapshot":          {mutate: func(s *Source) { s.Snapshot = nil }},
		"no signer key":               {mutate: func(*Source) {}, opt: Options{Signer: ed25519.PrivateKey{}, SignerKeyID: testSignerKeyID}},
		"no signer_key_id":            {mutate: func(*Source) {}, opt: Options{Signer: priv}},
		// A destroyed workspace signing key (security-model.md SEC-121) that was
		// zeroed in place is exactly this: full length, all zeros, and still
		// perfectly capable of returning 64 bytes from ed25519.Sign. The length
		// check above accepts it, which is why this case exists separately.
		"a zeroed signer key": {mutate: func(*Source) {}, opt: Options{
			Signer: make(ed25519.PrivateKey, ed25519.PrivateKeySize), SignerKeyID: testSignerKeyID,
		}},
		"an impossible kdf profile": {mutate: func(*Source) {}, opt: Options{
			Signer: priv, SignerKeyID: testSignerKeyID, KDF: KDFParams{MemoryKiB: 1024, Iterations: 0, Parallelism: 1},
		}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			tc.mutate(&f.src)

			opt := tc.opt
			if opt.Signer == nil && opt.SignerKeyID == "" {
				opt = Options{Signer: priv, SignerKeyID: testSignerKeyID}
			}
			if opt.KDF == (KDFParams{}) {
				opt.KDF = lightKDF()
			}
			opt.Passphrase = testPassphrase
			opt.rand = fixedRand()

			var out memFile
			if err := Create(&out, f.src, opt); err == nil {
				t.Fatalf("Create() with %s = nil, want an error", name)
			}
		})
	}
}

// TestCreateWritesNoContainerForAZeroedSigner is the archive half of the
// destroyed-key regression.
//
// A workspace signing key destroyed in place is 64 zero bytes. ed25519.Sign
// accepts them and returns a well-formed 64-byte signature, so a writer that
// only length-checked its signer emitted a complete, plausible container whose
// signature verifies under nothing — and reported success. The operator is then
// holding a backup no restorer can open, and does not know it.
//
// Create must refuse BEFORE writing anything: an unusable container that exists
// is worse than no container, because only the first one gets mistaken for a
// backup.
func TestCreateWritesNoContainerForAZeroedSigner(t *testing.T) {
	f := newFixture()
	zeroed := make(ed25519.PrivateKey, ed25519.PrivateKeySize)

	var out memFile
	err := Create(&out, f.src, Options{
		Passphrase: testPassphrase, Signer: zeroed, SignerKeyID: testSignerKeyID,
		KDF: lightKDF(), rand: fixedRand(),
	})
	if err == nil {
		t.Fatal("Create() with an all-zero signer = nil, want an error")
	}
	if len(out.buf) != 0 {
		t.Errorf("Create() wrote %d bytes before refusing; a partial container must never reach the destination", len(out.buf))
	}
}

// TestCreateRefusesASignerWhosePublicHalfDisagrees covers the general case the
// all-zeros check alone does not: key material that signs, is not all zeros, and
// still produces a signature no reader can verify — an ed25519 private key whose
// trailing public half has been swapped for a different key's.
//
// Go's ed25519.Sign derives the signature from the seed but the VERIFIER resolves
// `signer_key_id` to the public half. When the two disagree, every conformant
// restorer sees ARCHIVE_SIGNATURE_INVALID on a container this deployment called a
// success. Create catches it by verifying the signature it just produced against
// the signer's own public half before writing it.
func TestCreateRefusesASignerWhosePublicHalfDisagrees(t *testing.T) {
	f := newFixture()
	_, priv := testSigner(t)

	otherSeed := make([]byte, ed25519.SeedSize)
	for i := range otherSeed {
		otherSeed[i] = byte(0x11 + i)
	}
	other := ed25519.NewKeyFromSeed(otherSeed)

	mismatched := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
	copy(mismatched, priv)
	copy(mismatched[ed25519.SeedSize:], other[ed25519.SeedSize:])
	if mismatched.Equal(priv) {
		t.Fatal("precondition: the mismatched key is identical to the well-formed one")
	}

	var out memFile
	if err := Create(&out, f.src, Options{
		Passphrase: testPassphrase, Signer: mismatched, SignerKeyID: testSignerKeyID,
		KDF: lightKDF(), rand: fixedRand(),
	}); err == nil {
		t.Fatal("Create() with a signer whose public half disagrees with its seed = nil, want an error")
	}
}

// TestCreateRefusesAnEmptyPassphrase: a container whose body key is stretched
// from "" is decryptable by anyone who knows this format, which is not
// encryption. api/1's twelve-character floor is policy stated at the surface;
// this is the floor below which the format's own guarantee stops existing, and it
// belongs where the encryption happens.
func TestCreateRefusesAnEmptyPassphrase(t *testing.T) {
	f := newFixture()
	_, priv := testSigner(t)

	var out memFile
	err := Create(&out, f.src, Options{
		Passphrase: "", Signer: priv, SignerKeyID: testSignerKeyID,
		KDF: lightKDF(), rand: fixedRand(),
	})
	if err == nil {
		t.Fatal("Create() with an empty passphrase = nil, want an error")
	}
	if len(out.buf) != 0 {
		t.Errorf("Create() wrote %d bytes before refusing an empty passphrase", len(out.buf))
	}
}

// TestCanonicalSignedHeaderRefusesDuplicateMembers closes a divergence between
// two readers that both consider a container valid.
//
// The signature covers a canonicalization of the header, not its raw bytes, and
// encoding/json resolves a duplicated member by keeping the LAST occurrence.
// A reader built on a parser that keeps the FIRST — several do — canonicalizes
// the same signed bytes into a different header, verifies the same signature,
// and then honors a different digest or signer_key_id than this reader does.
// Refusing the duplicate outright is what keeps the two from disagreeing.
func TestCanonicalSignedHeaderRefusesDuplicateMembers(t *testing.T) {
	tests := map[string]string{
		"a duplicated top-level member": `{"format":"waiveo-archive","signer_key_id":"A","signer_key_id":"B"}`,
		"a duplicated nested member":    `{"format":"waiveo-archive","kdf":{"algorithm":"argon2id","algorithm":"scrypt"}}`,
		"a duplicated signature member": `{"format":"waiveo-archive","signature":"AAA","signature":"BBB"}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := canonicalSignedHeader([]byte(raw)); err == nil {
				t.Fatalf("canonicalSignedHeader(%s) = nil, want an error", raw)
			}
		})
	}

	// The control: a header with no duplicate still canonicalizes, so the case
	// above is refusing duplicates rather than everything.
	ok := `{"format":"waiveo-archive","kdf":{"algorithm":"argon2id"},"signature":"AAA"}`
	got, err := canonicalSignedHeader([]byte(ok))
	if err != nil {
		t.Fatalf("canonicalSignedHeader on a well-formed header = %v, want nil", err)
	}
	if bytes.Contains(got, []byte("signature")) {
		t.Errorf("canonicalization %s still carries the signature member", got)
	}
}

// TestOpenRefusesAHeaderWithDuplicateMembers is the same rule reached through
// the real read path, on a real container: the duplicate is refused during
// verification, so no member of a header this reader would read differently from
// another one ever reaches a decision.
func TestOpenRefusesAHeaderWithDuplicateMembers(t *testing.T) {
	f := newFixture()
	pub, _ := testSigner(t)
	container := createContainer(t, f, Options{})

	headerJSON, body := splitContainer(t, container)
	// Splice a second `signer_key_id` in textually: json.Marshal cannot emit a
	// duplicate member, and a hostile producer is under no such constraint.
	const marker = `{"format":`
	if !bytes.HasPrefix(headerJSON, []byte(marker)) {
		t.Fatalf("header does not begin with %s: %s", marker, headerJSON)
	}
	spliced := append([]byte(`{"signer_key_id":"01J8Z3K4N5P6Q7R8S9T0V1W2ZZ","format":`), headerJSON[len(marker):]...)

	var buf bytes.Buffer
	var lenBE [4]byte
	binary.BigEndian.PutUint32(lenBE[:], uint32(len(spliced)))
	buf.Write(lenBE[:])
	buf.Write(spliced)
	buf.Write(body)

	_, _, err := Open(bytes.NewReader(buf.Bytes()), testPassphrase, pub)
	if err == nil {
		t.Fatal("Open() on a header carrying a duplicated member = nil, want a refusal")
	}
	if got := Code(err); got != CodeArchiveSignatureInvalid {
		t.Errorf("Open() = %v (code %q), want code %q", err, got, CodeArchiveSignatureInvalid)
	}
}

// TestCreateWritesNothingBeyondTheContainer confirms Create leaves the writer at
// the container's end after seeking back to rewrite the header — a rewound
// cursor would make a caller's next write land in the middle of the archive.
func TestCreateWritesNothingBeyondTheContainer(t *testing.T) {
	f := newFixture()
	_, priv := testSigner(t)

	var out memFile
	if err := Create(&out, f.src, Options{
		Passphrase: testPassphrase, Signer: priv, SignerKeyID: testSignerKeyID,
		KDF: lightKDF(), rand: fixedRand(),
	}); err != nil {
		t.Fatalf("Create() = %v, want nil", err)
	}
	if out.pos != int64(len(out.buf)) {
		t.Errorf("write position after Create = %d, want %d (the container's end)", out.pos, len(out.buf))
	}

	trailer := []byte("APPENDED")
	if _, err := out.Write(trailer); err != nil {
		t.Fatalf("Write after Create: %v", err)
	}
	if !bytes.HasSuffix(out.buf, trailer) {
		t.Error("a write after Create did not land at the end of the container")
	}
}
