// Package castbundle implements the `.cast` bundle: the portable file one
// authored slidecast — and the images it draws — moves between boxes in
// (parity row 1.9, the legacy stack's CastExporter/CastImporter).
//
// # What it is for, and why it is not archive/1
//
// archive/1 moves a WORKSPACE: every row, every asset, encrypted under a
// passphrase, signed by the workspace key, gated by a preflight that refuses a
// destination whose schema epoch or pack set does not match. That is the right
// shape for a backup and the wrong shape for the thing an operator actually does
// weekly — "I built this menu on the office box; put it on the shop's box."
//
// A cast bundle is therefore deliberately small, deliberately unencrypted, and
// deliberately NOT signed:
//
//   - Unencrypted because its content is a design an operator is choosing to
//     hand to someone. A passphrase on it would be ceremony protecting nothing
//     while making the common case (email it, drop it on a USB stick) worse.
//   - Unsigned because trust is established by the IMPORT, not by the file. The
//     importing box re-derives every asset's hash from the bytes themselves and
//     re-runs the full authoring validation on the slides before anything is
//     written, so a tampered bundle cannot produce a row this platform would not
//     have accepted from its own editor. A signature would only tell the
//     importer who made it, which is not the question.
//
// # Layout
//
// A zip, because an operator ends up holding this file and a zip is the one
// container format every desktop opens.
//
//	cast.json      the manifest: format, the cast's authored shape, the asset list
//	assets/<hex>   one entry per referenced asset, the raw bytes
//
// The manifest is FIRST in the stream so a reader can refuse a wrong-format file
// before decompressing megabytes of images.
//
// # What travels and what does not
//
// The bundle carries what an operator authored: the name, the slides, the
// per-slide dwell, the labels, and the bytes of every image any layer names.
//
// It deliberately does NOT carry the row's identity or its placement — no `id`,
// no `scope_node`, no `external_id`, no `revision`. Each of those names
// something about the SOURCE deployment that is either meaningless or actively
// wrong on the destination: an id would collide or resurrect a deleted row, a
// scope node names a tree the destination does not have, an external_id is
// unique under a placement this cast is not at any more, and a revision is a
// concurrency token for a row that does not exist yet. The importer supplies the
// placement, and the platform mints the identity, exactly as it does for a cast
// authored in the editor.
package castbundle

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// Format is the manifest's `format` value. Versioned in the value rather than
// in the file extension, so a future format-2 bundle is refused by name by a
// reader that predates it instead of failing somewhere deeper with a confusing
// error.
const Format = "waiveo.cast/1"

// ManifestName and assetPrefix are the two entry-name shapes a bundle contains.
const (
	ManifestName = "cast.json"
	assetPrefix  = "assets/"
)

// MaxAssets and MaxBundleBytes bound what a reader will accept.
//
// These are decompression-bomb guards, not policy: a zip is an attacker-supplied
// file and a reader that trusted its declared sizes could be made to allocate
// arbitrarily. The limits are generous relative to a real design (a slidecast is
// a handful of images) and small relative to memory on any box this runs on.
const (
	MaxAssets      = 512
	MaxBundleBytes = 512 << 20 // 512 MiB of decompressed content
	// MaxManifestBytes bounds cast.json alone. A manifest is JSON describing a
	// few dozen layers; anything approaching this is not one.
	MaxManifestBytes = 8 << 20
)

// Manifest is cast.json.
type Manifest struct {
	Format string `json:"format"`
	// ExportedAtMs is when the bundle was written, epoch milliseconds. Carried
	// for the operator's benefit only — nothing about the import depends on it,
	// and an importer must never compare it against its own clock, since the two
	// boxes' clocks are exactly what a portable file cannot assume about.
	ExportedAtMs int64 `json:"exported_at_ms"`
	// SourceCastID is the id the cast had on the box that exported it. It is
	// PROVENANCE, never identity: the importer mints a fresh id and this is here
	// so an operator tracing "where did this design come from" has an answer.
	SourceCastID string `json:"source_cast_id,omitempty"`
	// Cast is the authored shape, with identity and placement stripped.
	Cast CastPayload `json:"cast"`
	// Assets lists every asset the slides reference, each with the size a reader
	// checks the entry against before reading it.
	Assets []AssetEntry `json:"assets"`
}

// CastPayload is the authored half of a cast — everything an operator made, and
// nothing the source deployment assigned.
type CastPayload struct {
	Name              string                `json:"name"`
	Slides            []datamodel.CastSlide `json:"slides"`
	DefaultDurationMS int64                 `json:"default_duration_ms,omitempty"`
	Template          bool                  `json:"template,omitempty"`
	Labels            map[string]string     `json:"labels,omitempty"`
}

// AssetEntry is one referenced asset.
type AssetEntry struct {
	// AssetRef is the `sha256:<hex>` form the slide layers name.
	AssetRef string `json:"asset_ref"`
	// SizeBytes is the entry's uncompressed length, declared so a reader can
	// refuse an implausible entry before decompressing it.
	SizeBytes int64 `json:"size_bytes"`
}

// Bundle is a read bundle: the manifest plus each asset's verified bytes, keyed
// by `sha256:<hex>`.
type Bundle struct {
	Manifest Manifest
	Assets   map[string][]byte
}

// Errors Read returns for a bundle this reader will not accept. Each is
// distinguishable because the api layer turns them into different operator
// sentences — "that is not a cast bundle" and "that bundle is damaged" send
// somebody to different places.
var (
	ErrNotABundle    = errors.New("castbundle: not a cast bundle")
	ErrWrongFormat   = errors.New("castbundle: unsupported bundle format")
	ErrDamaged       = errors.New("castbundle: the bundle is damaged or incomplete")
	ErrTooLarge      = errors.New("castbundle: the bundle exceeds this reader's limits")
	ErrAssetMismatch = errors.New("castbundle: an asset's bytes do not match the reference it is carried under")
)

// AssetRefOf is the `sha256:<hex>` reference for these bytes — the SAME
// derivation the content origin performs, so a bundle's references and the
// destination's own are the same strings by construction rather than by
// agreement.
func AssetRefOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// entryNameOf is an asset's zip entry name.
func entryNameOf(assetRef string) string {
	return assetPrefix + strings.TrimPrefix(assetRef, "sha256:")
}

// Write writes a bundle to w.
//
// assets maps each `sha256:<hex>` the slides reference to its bytes. Every
// reference must be present: a bundle missing one of its own images is a bundle
// that imports as a cast with a hole in it, and the caller (which has the
// content origin) is the only layer that can tell the difference between "this
// asset is missing" and "this asset was never referenced".
func Write(w io.Writer, m Manifest, assets map[string][]byte) error {
	m.Format = Format
	// The asset list is DERIVED from the slides rather than taken from the
	// caller, so it cannot disagree with what the cast actually references —
	// a manifest listing an asset no layer names, or omitting one that is named,
	// is the failure mode a hand-assembled list has.
	refs := ReferencedAssets(m.Cast.Slides)
	m.Assets = make([]AssetEntry, 0, len(refs))
	for _, ref := range refs {
		body, ok := assets[ref]
		if !ok {
			return fmt.Errorf("castbundle: the cast references %s but no bytes were supplied for it", ref)
		}
		m.Assets = append(m.Assets, AssetEntry{AssetRef: ref, SizeBytes: int64(len(body))})
	}

	zw := zip.NewWriter(w)
	mf, err := zw.Create(ManifestName)
	if err != nil {
		return fmt.Errorf("castbundle: create the manifest entry: %w", err)
	}
	enc := json.NewEncoder(mf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return fmt.Errorf("castbundle: write the manifest: %w", err)
	}
	for _, a := range m.Assets {
		ew, err := zw.Create(entryNameOf(a.AssetRef))
		if err != nil {
			return fmt.Errorf("castbundle: create the entry for %s: %w", a.AssetRef, err)
		}
		if _, err := ew.Write(assets[a.AssetRef]); err != nil {
			return fmt.Errorf("castbundle: write %s: %w", a.AssetRef, err)
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("castbundle: finish the bundle: %w", err)
	}
	return nil
}

// ReferencedAssets returns every distinct asset_ref the slides name, sorted.
//
// Sorted and de-duplicated so a bundle written twice from the same cast is
// byte-identical, which is what lets an operator tell "this is the design I
// already have" from "this is a different one" by comparing files.
func ReferencedAssets(slides []datamodel.CastSlide) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range slides {
		for _, l := range s.Layers {
			if l.AssetRef == "" {
				continue
			}
			if _, ok := seen[l.AssetRef]; ok {
				continue
			}
			seen[l.AssetRef] = struct{}{}
			out = append(out, l.AssetRef)
		}
	}
	sort.Strings(out)
	return out
}

// Read parses and VERIFIES a bundle from raw bytes.
//
// Verification is the whole reason a caller may trust what comes back:
//
//   - the manifest's `format` is this reader's;
//   - every asset entry the manifest declares is present, and no entry is
//     present that the manifest does not declare (a smuggled entry is a file
//     that would be written to the destination's content origin without ever
//     appearing in the manifest a reviewer read);
//   - every asset's bytes hash to the reference it is carried under, so the
//     `asset_ref` a slide layer names cannot be pointed at different bytes;
//   - every reference the slides name is carried, so an imported cast never has
//     an image the destination cannot serve.
//
// It does NOT validate the slides themselves. That is the platform's own
// authoring validation (datamodel + wire.ValidateAuthoredSlideLayers), which the
// import runs by writing through the ordinary create path — one copy of those
// rules, not two.
func Read(raw []byte) (Bundle, error) {
	if int64(len(raw)) > MaxBundleBytes {
		return Bundle{}, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(raw))
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return Bundle{}, fmt.Errorf("%w: %v", ErrNotABundle, err)
	}

	var manifestFile *zip.File
	assetFiles := map[string]*zip.File{}
	var total int64
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			continue // a directory entry carries no content
		}
		total += int64(f.UncompressedSize64)
		if total > MaxBundleBytes {
			return Bundle{}, fmt.Errorf("%w: declared content exceeds %d bytes", ErrTooLarge, int64(MaxBundleBytes))
		}
		switch {
		case f.Name == ManifestName:
			manifestFile = f
		case strings.HasPrefix(f.Name, assetPrefix):
			// The entry name is not trusted as a path — it is only ever used as a
			// map key, and the asset's real identity comes from hashing its bytes
			// below. Nothing here ever opens a file by this name, which is what
			// makes a `../` in it inert rather than dangerous.
			assetFiles[f.Name] = f
		default:
			return Bundle{}, fmt.Errorf("%w: unexpected entry %q", ErrNotABundle, f.Name)
		}
	}
	if manifestFile == nil {
		return Bundle{}, fmt.Errorf("%w: no %s", ErrNotABundle, ManifestName)
	}
	if len(assetFiles) > MaxAssets {
		return Bundle{}, fmt.Errorf("%w: %d asset entries", ErrTooLarge, len(assetFiles))
	}

	mbytes, err := readEntry(manifestFile, MaxManifestBytes)
	if err != nil {
		return Bundle{}, fmt.Errorf("%w: %s: %v", ErrDamaged, ManifestName, err)
	}
	var m Manifest
	if err := json.Unmarshal(mbytes, &m); err != nil {
		return Bundle{}, fmt.Errorf("%w: %s is not valid JSON: %v", ErrNotABundle, ManifestName, err)
	}
	if m.Format != Format {
		return Bundle{}, fmt.Errorf("%w: %q (this reader accepts %q)", ErrWrongFormat, m.Format, Format)
	}

	out := Bundle{Manifest: m, Assets: map[string][]byte{}}
	for _, a := range m.Assets {
		name := entryNameOf(a.AssetRef)
		f, ok := assetFiles[name]
		if !ok {
			return Bundle{}, fmt.Errorf("%w: the manifest declares %s but the bundle carries no %s", ErrDamaged, a.AssetRef, name)
		}
		delete(assetFiles, name)
		body, err := readEntry(f, MaxBundleBytes)
		if err != nil {
			return Bundle{}, fmt.Errorf("%w: %s: %v", ErrDamaged, name, err)
		}
		if got := AssetRefOf(body); got != a.AssetRef {
			return Bundle{}, fmt.Errorf("%w: %s carries bytes that hash to %s", ErrAssetMismatch, a.AssetRef, got)
		}
		out.Assets[a.AssetRef] = body
	}
	// An entry the manifest never declared would be written to the destination's
	// content origin by an importer that iterated the ZIP instead of the
	// manifest. Refusing it here means the manifest a reviewer reads IS the list
	// of what an import will write.
	for name := range assetFiles {
		return Bundle{}, fmt.Errorf("%w: the bundle carries %q, which its manifest does not declare", ErrNotABundle, name)
	}

	// Finally, the direction that matters to the SCREEN: every image the slides
	// name must be in the bundle. A bundle that verified but was missing one
	// would import as a cast whose layer points at bytes the destination has
	// never seen — a slide that renders a blank while the import reports success.
	for _, ref := range ReferencedAssets(m.Cast.Slides) {
		if _, ok := out.Assets[ref]; !ok {
			return Bundle{}, fmt.Errorf("%w: a slide layer references %s, which the bundle does not carry", ErrDamaged, ref)
		}
	}
	return out, nil
}

// readEntry reads one zip entry, refusing to read more than limit bytes.
//
// The limit is applied to the READ, not to the declared UncompressedSize64: a
// zip header is attacker-supplied and a reader that trusted it would allocate
// whatever the header claimed. io.LimitReader over limit+1 is what makes an
// over-long entry detectable rather than silently truncated.
func readEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("entry exceeds %d bytes", limit)
	}
	return body, nil
}

// FileName is the download name for a bundle of the named cast: the operator's
// own title, reduced to something every filesystem accepts, plus `.cast`.
//
// A name is reduced rather than rejected. An operator's cast is called "Lunch —
// Tuesday (v2)"; refusing to export it because of the punctuation would be the
// tool telling them their title is wrong, and quoting it raw into a
// Content-Disposition header is a header-injection question nobody should have
// to think about twice.
func FileName(castName string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range castName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "cast"
	}
	if len(name) > 64 {
		name = strings.Trim(name[:64], "-")
	}
	return name + ".cast"
}
