package archive1

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/maaxton/waiveo-next/internal/archive"
)

// payloadSpanningFrames is a relational-snapshot size chosen to produce SEVERAL
// body frames at the fixture's frame size, because two of the cases are about the
// frame SEQUENCE — a truncated tail (ARC-016) and aborting at the first bad frame
// (ARC-014). A single-frame archive would make both vacuous: there would be no
// tail to drop and no "subsequent frames" to decline to attempt.
const payloadSpanningFrames = 5 * fixtureFrameSize

// fixtureFrameSize is deliberately tiny. The container's own default is 1 MiB,
// which would make a multi-frame fixture megabytes long and every case slow for
// no gain — the frame machinery is size-independent, and Create takes the size as
// an option precisely so a driver can exercise the sequence cheaply.
const fixtureFrameSize = 4096

// fixture holds one archive/1 export identity: a signing key, its id, and the
// passphrase. Everything a case needs to build a real container and read it back.
//
// The signing key is generated per fixture from a FIXED seed rather than from
// crypto/rand: a driver's output should be identical run to run, and a fresh
// random key each run would mean a failure could not be reproduced from the
// report alone. It is a conformance fixture and guards nothing.
type fixture struct {
	priv        ed25519.PrivateKey
	pub         ed25519.PublicKey
	signerKeyID string
	passphrase  string
}

func newFixture() *fixture {
	seed := bytes.Repeat([]byte{0x2a}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	return &fixture{
		priv:        priv,
		pub:         priv.Public().(ed25519.PublicKey),
		signerKeyID: "01J8Z3K4N5P6Q7R8S9T0V1W2ZB",
		// Twelve characters, matching api/1's floor, so the fixture is a passphrase
		// the surface would actually accept rather than one only this package would.
		passphrase: "conformance-export-passphrase",
	}
}

// minimalManifest is the smallest full-mode manifest a case can build on: every
// ARC-031 required member present, nothing optional. A case that cares about one
// member overwrites just that one.
func (f *fixture) minimalManifest() archive.Manifest {
	return archive.Manifest{
		CreatedAt:           1752537600000,
		Mode:                "full",
		WorkspaceID:         "01J8Z3K4N5P6Q7R8S9T0V1W2ZC",
		PlatformSchemaEpoch: 4,
		Packs:               []archive.PackLock{},
		Assets:              []archive.AssetEntry{},
		SecretStubs:         []archive.SecretStub{},
	}
}

// create builds a container carrying a snapshot of n bytes and a minimal
// manifest — the fixture for every case whose subject is the CONTAINER rather
// than the manifest.
func (f *fixture) create(snapshotBytes int) ([]byte, error) {
	return f.createFrom(f.minimalManifest(), func() (io.ReadCloser, int64, error) {
		body := incompressible(snapshotBytes)
		return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
	})
}

// multiFrameArchive builds a container whose body is SEVERAL frames, and asserts
// that it is — the assertion is the point. A payload size alone does not guarantee
// a frame count, because zstd sits between the two, and a fixture that quietly
// collapses to one frame makes every frame-sequence case vacuous while still
// passing.
func (f *fixture) multiFrameArchive() ([]byte, error) {
	c, err := f.create(payloadSpanningFrames)
	if err != nil {
		return nil, err
	}
	if n := frameCount(c); n < 2 {
		return nil, fmt.Errorf("fixture archive carries %d frame(s), want at least 2: "+
			"the payload compressed below one frame, so there is no tail to drop and no subsequent frame to decline", n)
	}
	return c, nil
}

// createFrom builds a container whose manifest is m. snapshot may be nil, in
// which case a small fixed snapshot is used.
//
// Every embedded asset m declares is served synthetic bytes whose content hash
// must match its asset_ref — Open verifies that (ARC-062), so a case declaring an
// embedded asset with an arbitrary ref would fail on the asset check rather than
// on its own subject. Cases here declare by-reference or inherited assets for that
// reason, and this helper refuses an embedded one loudly instead of producing a
// container that fails for a reason the case is not about.
func (f *fixture) createFrom(m archive.Manifest, snapshot func() (io.ReadCloser, int64, error)) ([]byte, error) {
	for _, a := range m.Assets {
		if a.Storage != archive.StorageEmbedded {
			continue
		}
		if _, ok := corpusPreimages[a.AssetRef]; !ok {
			return nil, fmt.Errorf("fixture has no preimage for embedded asset %s: Open recomputes an embedded entry's "+
				"bytes against its own asset_ref (ARC-062), so a fixture must serve bytes that actually hash to it — "+
				"add the preimage to corpusPreimages, or the ref is not a digest of anything", a.AssetRef)
		}
	}
	if snapshot == nil {
		snapshot = func() (io.ReadCloser, int64, error) {
			body := []byte("fixture-snapshot")
			return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
		}
	}

	asset := func(ref string) (io.ReadCloser, int64, error) {
		body, ok := corpusPreimages[ref]
		if !ok {
			return nil, 0, fmt.Errorf("no preimage for %s", ref)
		}
		return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
	}

	src := archive.Source{
		CreatedAt:           m.CreatedAt,
		WorkspaceID:         m.WorkspaceID,
		PlatformSchemaEpoch: m.PlatformSchemaEpoch,
		Packs:               m.Packs,
		Assets:              m.Assets,
		SecretStubs:         m.SecretStubs,
		Snapshot:            snapshot,
		Asset:               asset,
		// The wrap is opaque to archive/1 by its own Scope (ARC-071 forbids carrying a
		// raw data key and leaves the algorithm to the key hierarchy), so the fixture
		// returns a deterministic stand-in derived from the wrap key it is handed —
		// enough to be present and to differ if the sub-key derivation changed.
		WrapDataKey: func(wrapKey []byte) (string, error) {
			return base64.RawStdEncoding.EncodeToString(wrapKey[:16]), nil
		},
	}
	// An incremental manifest's base reference rides through unchanged; nil leaves
	// it a full archive.
	src.BaseArchive = m.BaseArchive

	var buf writeSeekBuffer
	err := archive.Create(&buf, src, archive.Options{
		Passphrase:  f.passphrase,
		Signer:      f.priv,
		SignerKeyID: f.signerKeyID,
		FrameSize:   fixtureFrameSize,
	})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// incompressible returns n deterministic, high-entropy bytes: a sha256 chain,
// which zstd cannot shrink.
//
// This is not fussiness — it is the difference between a fixture that tests the
// frame SEQUENCE and one that silently does not. Plausible "row 1,xxx" text
// compressed a 20 KiB snapshot to under one 4 KiB frame, so the multi-frame
// fixture was a single-frame archive: `truncateFinalFrame` then removed the only
// frame there was, and the corrupt-frame-plus-truncate construction destroyed the
// very frame it meant to corrupt. It reported ARCHIVE_TRUNCATED and looked like an
// implementation defect. The frame count is asserted directly now (frameCount)
// rather than assumed from a byte size, because a compressor sits between the two.
func incompressible(n int) []byte {
	out := make([]byte, 0, n+sha256.Size)
	block := sha256.Sum256([]byte("archive1 conformance fixture"))
	for len(out) < n {
		out = append(out, block[:]...)
		block = sha256.Sum256(block[:])
	}
	return out[:n]
}

// frameCount reports how many body frames container carries, so a case that needs
// several can assert it got them instead of trusting a payload size to survive the
// compressor.
func frameCount(container []byte) int {
	at, n := bodyOffset(container), 0
	for at+4 <= len(container) {
		flen := int(binary.BigEndian.Uint32(container[at : at+4]))
		if flen <= 0 || at+4+flen > len(container) {
			return n
		}
		n++
		at += 4 + flen
	}
	return n
}

// corpusPreimages are the bytes whose sha256 the frozen archive-1 cases use as
// asset_refs. An embedded entry's bytes are recomputed against its own ref when
// the archive is opened (ARC-062), so a fixture cannot invent them — it has to
// serve the actual preimage.
//
// The corpus authors used well-known short strings, which is what makes this
// possible at all. A ref with no entry here is a ref that is not the digest of
// anything the driver can produce, and createFrom says so rather than serving
// wrong bytes and failing on the asset check for a reason the case is not about.
var corpusPreimages = map[string][]byte{
	// sha256("test") — ARC-091's freshly embedded asset.
	"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08": []byte("test"),
	// sha256("hello") — ARC-031/060's by-reference asset. Present for completeness:
	// a by-reference entry carries no bytes, so nothing asks for this one today.
	"sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824": []byte("hello"),
	// sha256("") — ARC-031's embedded asset. The EMPTY byte string, which is what
	// that digest names and why the case declares size 0. It read as an image/png
	// of 20481 bytes while its digest was a 63-character truncation of this one:
	// two inconsistencies, the second hiding the first.
	"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855": {},
}

// writeSeekBuffer is an in-memory io.WriteSeeker, which Create requires because
// it back-patches the header once the body's digest is known. A bytes.Buffer
// alone cannot seek.
type writeSeekBuffer struct {
	buf []byte
	pos int64
}

func (w *writeSeekBuffer) Write(p []byte) (int, error) {
	end := w.pos + int64(len(p))
	if end > int64(len(w.buf)) {
		grown := make([]byte, end)
		copy(grown, w.buf)
		w.buf = grown
	}
	copy(w.buf[w.pos:end], p)
	w.pos = end
	return len(p), nil
}

func (w *writeSeekBuffer) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = w.pos + offset
	case io.SeekEnd:
		next = int64(len(w.buf)) + offset
	default:
		return 0, fmt.Errorf("writeSeekBuffer: unknown whence %d", whence)
	}
	if next < 0 {
		return 0, fmt.Errorf("writeSeekBuffer: negative position %d", next)
	}
	w.pos = next
	return next, nil
}

func (w *writeSeekBuffer) Bytes() []byte { return w.buf }
