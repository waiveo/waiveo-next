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

// The size limits, which are ONE set of numbers for both directions.
//
// # Why they are declared together, here
//
// They started apart, and that is the defect: the reader advertised 512 MiB, the
// import route capped the request body at the content-upload ceiling of 64 MiB,
// and the export was bounded by nothing at all. So this box could produce a
// bundle — two video layers is enough — that this box would then refuse to
// import, permanently, with a message about a limit the reader claimed not to
// have. A `.cast` file whose whole purpose is to move between boxes has to have
// ONE size it can be, and every surface that touches it has to use that number:
// the reader below, the export's refusal and the import route's body limit
// (internal/app/api/castbundles.go, pinned equal by a test there).
//
// # Where the number comes from
//
// It is derived, not picked. Every asset in a bundle got onto its source box
// through `POST /content`, so no legitimate asset exceeds that route's ceiling —
// MaxAssetBytes mirrors it. A slidecast is a DESIGN and not a media library: its
// images are a handful, and the only layers that approach the per-asset ceiling
// are video. Two of those at full size, plus room for the manifest describing
// them, is the whole bundle.
//
// The ceiling is low for a reason that is not aesthetic. An import holds the
// request body, the decompressed assets AND the content origin's own resident
// copy simultaneously (origin.Store keeps every asset in memory), so the real
// cost on a Pi-class appliance is roughly three times this number. As with
// maxContentUploadBytes, the honest reading is that moving genuinely large media
// wants a streaming ingest rather than a bigger constant; until then this bounds
// the buffer instead of leaving it to whoever mailed the file.
const (
	// MaxAssets bounds how many entries a bundle may carry, independently of
	// their size: a decompression-bomb guard on entry COUNT, which the total
	// alone would not catch (a million empty entries).
	MaxAssets = 512
	// MaxAssetBytes bounds ONE asset, and equals this platform's per-upload
	// ceiling — see the api layer's TestTheCastBundleLimitsMatchTheUploadCeiling.
	MaxAssetBytes = 64 << 20
	// MaxBundleContentBytes bounds what the ASSETS may total. It is what the
	// export refuses against, because it is the part an export controls.
	MaxBundleContentBytes = 2 * MaxAssetBytes
	// MaxBundleOverheadBytes is the room reserved for everything that is not
	// asset bytes: the manifest and the zip's own framing. It exists so that a
	// bundle whose assets are exactly at MaxBundleContentBytes still fits under
	// MaxBundleBytes — i.e. so an export this box permits is always an import
	// this box permits. TestTheOverheadReserveCoversTheWorstManifestAndFraming
	// checks the arithmetic rather than trusting it.
	MaxBundleOverheadBytes = 8 << 20
	// MaxBundleBytes bounds the whole file: what the import route accepts and
	// what the reader's running total is checked against.
	MaxBundleBytes = MaxBundleContentBytes + MaxBundleOverheadBytes
	// MaxManifestBytes bounds cast.json alone. A manifest is JSON describing a
	// few dozen layers; anything approaching this is not one. It is HALF the
	// overhead reserve so that the manifest plus the zip framing for a maximal
	// entry count both fit inside it.
	//
	// It was 8 MiB before the overhead reserve existed, and halving it to 4 MiB
	// is a DELIBERATE tightening of what this reader accepts, called out here
	// because a reader limit that moves quietly is how an operator's file stops
	// importing for a reason nobody can find. Two things make it safe:
	//
	//   - Nothing this box authors can approach it. A cast's create body is
	//     capped at maxJSONBodyBytes (1 MiB, internal/app/api), and the manifest
	//     is that body's slides plus ~100 bytes per asset entry — 512 assets is
	//     51 KiB. A manifest over 4 MiB is not a cast; it is something else.
	//   - It has to be at most half the reserve for the reserve to hold, since
	//     the framing for MaxAssets entries has to fit beside it —
	//     TestTheOverheadReserveCoversTheWorstManifestAndFraming checks that
	//     arithmetic rather than trusting this sentence.
	MaxManifestBytes = MaxBundleOverheadBytes / 2
)

// zipFramingBytesFor bounds the zip container's own overhead for n entries: a
// local file header and a central-directory record per entry, each carrying the
// entry name, plus the end-of-central-directory record.
//
// It is deliberately an over-estimate. Its only job is to prove the overhead
// reserve is big enough, and a bound that is too generous fails safe (a reserve
// larger than it needs to be) while one that is too tight reopens the exact
// export/import disagreement this whole block exists to close.
func zipFramingBytesFor(entries int) int64 {
	// 30-byte local header + 46-byte central record + a data descriptor, and an
	// entry name that is `assets/` plus a 64-character hex digest.
	const perEntry = 30 + 46 + 24 + 2*(len(assetPrefix)+64)
	const endOfCentralDirectory = 22
	return int64(entries)*int64(perEntry) + endOfCentralDirectory
}

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
	// ErrIncomplete is the one PRODUCER-side sentinel: NewPlan was asked to
	// bundle a cast whose bytes the caller did not supply in full. It is a
	// sentinel rather than a bare error so the api layer can tell it from a size
	// refusal — the two send an operator to completely different places ("this
	// box no longer holds one of these images" versus "this design is too big to
	// travel this way").
	ErrIncomplete = errors.New("castbundle: bytes were not supplied for every image the cast references")
)

// Refusal is one reason NewPlan would not produce a bundle, carrying BOTH the
// sentinel a caller branches on and the sentence an operator reads.
//
// The sentence is a field rather than something a caller reconstructs from the
// sentinel, because a caller reconstructing it is a caller that has to be kept
// in step with the refusal set — which is the defect the two-phase API exists to
// close, wearing different clothes. The api layer adds ADVICE ("move this design
// with a workspace archive instead") and nothing else; a refusal added here
// arrives at an operator already worded.
type Refusal struct {
	// Kind is the sentinel (ErrTooLarge, ErrIncomplete) errors.Is matches.
	Kind error
	// Detail is the operator's sentence: capitalised, ending in a full stop,
	// naming the actual numbers.
	Detail string
}

func (r *Refusal) Error() string { return r.Kind.Error() + ": " + r.Detail }

// Unwrap makes errors.Is(err, ErrTooLarge) work on a Refusal.
func (r *Refusal) Unwrap() error { return r.Kind }

// refuse builds a Refusal, so every refusal site in NewPlan is one line and
// they are visibly the same kind of thing.
func refuse(kind error, format string, args ...any) error {
	return &Refusal{Kind: kind, Detail: fmt.Sprintf(format, args...)}
}

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

// A bundle is produced in TWO phases, and the split is the whole point of this
// section.
//
// # Why a Plan exists at all
//
// An export is an HTTP response. The moment a 200 and the zip's first bytes are
// on the wire there is no way left to say no: the only remaining vocabulary is
// truncating the stream, and a truncated zip arrives at the destination as
// "this bundle is damaged" — a sentence that sends an operator to the wrong
// box entirely.
//
// The first version of this API had one function, and the export handler
// hand-copied a SUBSET of its refusals into a pre-flight before committing the
// header. It copied one of five. The other four — asset count, per-asset size,
// manifest size, overhead reserve — all fire before the first zip byte, i.e.
// after the 200 was already sent, and the handler logged them and hung up. A
// cast with 513 referenced images (which the create path accepts: nothing caps
// slides or layers) answered `200 application/zip` with a zero-byte body.
//
// A hand-copied subset of another function's refusals is the defect, not the
// four misses. So the refusals are not copyable any more:
//
//	NewPlan  EVERY refusal. Returns an error, or a Plan.
//	Stream   bytes onto a writer. Cannot refuse anything.
//
// A caller that can still write a Problem document calls NewPlan; a caller that
// has committed a header calls Stream. Adding a new refusal has exactly one
// place to go, and every calling side gets it for free.
// TestNothingCanBeRefusedOnceTheBundleIsStreaming is the fence that keeps the
// second half unable to refuse: it reads Stream's own syntax tree and fails on
// a second return statement, on any use of this package's refusal sentinels, and
// on any error this package MINTS rather than propagates.

// Plan is a bundle that has already passed every refusal — a value whose
// existence is the proof. It holds slice headers into the caller's asset bytes,
// not copies: the export handler's map comes straight off the content origin's
// resident store, and a Plan that copied it would put a second whole bundle in
// memory on a box that already holds one.
type Plan struct {
	manifest []byte
	entries  []plannedEntry
	// ContentBytes is what the assets total, the figure the size refusal was
	// decided against. Published so a caller can report or log it without
	// re-deriving it from the same map.
	ContentBytes int64
}

// plannedEntry is one asset, resolved to the entry name it will be written as.
type plannedEntry struct {
	name string
	body []byte
}

// NewPlan runs EVERY refusal this package makes about a bundle it is asked to
// produce, and returns the plan for writing one if none fire.
//
// assets maps each `sha256:<hex>` the slides reference to its bytes. Every
// reference must be present: a bundle missing one of its own images is a bundle
// that imports as a cast with a hole in it, and the caller (which has the
// content origin) is the only layer that can tell the difference between "this
// asset is missing" and "this asset was never referenced".
//
// Every refusal below is one Read would apply to the finished file, so a bundle
// this box produces is always a bundle this box accepts — the export/import
// disagreement the size block above exists to make impossible. The messages are
// written as sentences an operator reads, because they reach one: the api layer
// puts them in the Problem document's `detail` rather than translating them.
func NewPlan(m Manifest, assets map[string][]byte) (*Plan, error) {
	m.Format = Format
	// The asset list is DERIVED from the slides rather than taken from the
	// caller, so it cannot disagree with what the cast actually references —
	// a manifest listing an asset no layer names, or omitting one that is named,
	// is the failure mode a hand-assembled list has.
	refs := ReferencedAssets(m.Cast.Slides)
	if len(refs) > MaxAssets {
		return nil, refuse(ErrTooLarge, "This cast references %d images, more than the %d a cast bundle can carry.", len(refs), MaxAssets)
	}
	plan := &Plan{entries: make([]plannedEntry, 0, len(refs))}
	m.Assets = make([]AssetEntry, 0, len(refs))
	for _, ref := range refs {
		body, ok := assets[ref]
		if !ok {
			return nil, refuse(ErrIncomplete, "This cast references the image %s, which this box no longer holds.", ref)
		}
		if int64(len(body)) > MaxAssetBytes {
			return nil, refuse(ErrTooLarge, "The image %s is %d bytes, more than the %d-byte limit one bundle entry may be.", ref, len(body), int64(MaxAssetBytes))
		}
		plan.ContentBytes += int64(len(body))
		m.Assets = append(m.Assets, AssetEntry{AssetRef: ref, SizeBytes: int64(len(body))})
		plan.entries = append(plan.entries, plannedEntry{name: entryNameOf(ref), body: body})
	}
	// The ASSET TOTAL, against the same number the import route accepts and the
	// reader enforces. It lives here rather than in the caller for the reason
	// this whole section exists: a refusal a caller has to remember to make is a
	// refusal one caller will forget.
	//
	// Without it an export is bounded by nothing at all: MaxAssets assets at the
	// per-upload ceiling is 32 GiB, and an authenticated caller could ask a
	// Pi-class appliance to marshal that with one GET.
	if plan.ContentBytes > MaxBundleContentBytes {
		return nil, refuse(ErrTooLarge, "This cast's images total %d bytes, more than the %d-byte limit a cast bundle carries.",
			plan.ContentBytes, int64(MaxBundleContentBytes))
	}

	// The manifest is encoded to memory, because its SIZE has to be checked
	// before anything is committed to a stream: a manifest over MaxManifestBytes
	// is one Read refuses, and discovering that halfway through a response is
	// discovering it too late. It is JSON describing slides, so this buffer is
	// kilobytes.
	var manifest bytes.Buffer
	enc := json.NewEncoder(&manifest)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("castbundle: encode the manifest: %w", err)
	}
	if int64(manifest.Len()) > MaxManifestBytes {
		return nil, refuse(ErrTooLarge, "This cast's manifest is %d bytes, more than the %d a bundle reader will accept.", manifest.Len(), int64(MaxManifestBytes))
	}
	// The other half of "an export this box permits is an import this box
	// permits": the asset total above bounds the content, and this bounds
	// everything else, so the two together cannot add up past MaxBundleBytes.
	if overhead := int64(manifest.Len()) + zipFramingBytesFor(len(m.Assets)); overhead > MaxBundleOverheadBytes {
		return nil, refuse(ErrTooLarge, "This cast's manifest and container framing come to %d bytes, past the %d reserved for them, so the bundle could exceed the %d-byte limit a reader accepts.",
			overhead, int64(MaxBundleOverheadBytes), int64(MaxBundleBytes))
	}
	plan.manifest = manifest.Bytes()
	return plan, nil
}

// Stream writes the planned bundle onto w. It cannot refuse anything: every
// error it can return came out of w (or out of the zip writer wrapping w), and
// by then the caller has committed a header anyway.
//
// Streaming is the point. The export handler passes the http.ResponseWriter
// directly, so the only whole copy of a bundle that ever exists on the exporting
// box is the one going out over the socket — an earlier version assembled the
// zip in a bytes.Buffer first, which put a second copy of every asset in memory
// on a box that already holds them all resident.
//
// It is deliberately NOT named WriteTo: io.WriterTo's contract is
// `(int64, error)` and a byte count is not something any caller here wants,
// while a `WriteTo(io.Writer) error` is a method go vet's stdmethods check
// correctly refuses.
//
// The shape — one accumulated `failure`, one return, no error minted here — is
// load-bearing and is enforced by TestNothingCanBeRefusedOnceTheBundleIsStreaming.
// It is what makes "the calling side has already seen every refusal" a property
// of the code rather than a promise in a comment.
func (p *Plan) Stream(w io.Writer) error {
	var failure error
	zw := zip.NewWriter(w)
	// STORED for assets, deflated for the manifest. A bundle's assets are
	// already-compressed media — JPEG, PNG, H.264 — so deflate spends a Pi's CPU
	// on every export to achieve nothing, and it makes the file's size
	// unpredictable from the content's, which is exactly the property the size
	// refusals above need. The manifest is JSON, so that one is worth deflating.
	write := func(name string, body []byte, method uint16) {
		if failure != nil {
			return
		}
		ew, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		if err == nil {
			_, err = ew.Write(body)
		}
		failure = err
	}
	write(ManifestName, p.manifest, zip.Deflate)
	for _, e := range p.entries {
		write(e.name, e.body, zip.Store)
	}
	if err := zw.Close(); failure == nil {
		failure = err
	}
	return failure
}

// Write plans a bundle and streams it, for a caller with nothing committed yet
// and nothing to say to an operator — the tests, and any future non-HTTP
// producer. An HTTP handler must NOT use it: it cannot tell a refusal from a
// broken socket, which is the distinction the two-phase API exists to preserve.
func Write(w io.Writer, m Manifest, assets map[string][]byte) error {
	plan, err := NewPlan(m, assets)
	if err != nil {
		return err
	}
	return plan.Stream(w)
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
		// Bounded by the PER-ASSET ceiling, not by the whole-bundle one: an
		// entry larger than any upload this platform accepts is not an asset
		// that could have come from a box, however much room the bundle has
		// left.
		body, err := readEntry(f, MaxAssetBytes)
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
