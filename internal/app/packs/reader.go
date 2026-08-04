package packs

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// Limits bounds an untrusted pack artifact so a malicious or malformed zip
// cannot exhaust the host: the whole artifact's byte size, each entry's
// uncompressed size, the total uncompressed bytes across all entries (a
// decompression-bomb guard the per-file cap alone does not close), and the entry
// count. Dev-POC values — generous for a declarative pack (a manifest, a handful
// of small JSON page docs, and locale catalogs) yet firmly bounded.
type Limits struct {
	MaxArtifactBytes     int64
	MaxFileBytes         int64
	MaxTotalUncompressed int64
	MaxFiles             int
}

// DefaultLimits are the install-path caps. A declarative pack is tiny; these
// leave comfortable headroom while refusing anything that could be a resource
// attack.
var DefaultLimits = Limits{
	MaxArtifactBytes:     8 << 20,  // 8 MiB compressed artifact
	MaxFileBytes:         1 << 20,  // 1 MiB per entry, uncompressed
	MaxTotalUncompressed: 16 << 20, // 16 MiB total, uncompressed
	MaxFiles:             256,
}

// ArtifactError is a malformed, unsafe, or oversize artifact — everything that
// makes a zip un-openable or un-trustworthy BEFORE the manifest engine ever sees
// it. It carries a stable Code and a human Message; the api layer renders it as
// a 422 Problem (a bad artifact is a client error, never a panic and never a
// 500). It is deliberately distinct from a manifest ValidationError (which
// carries per-field errors[]): an artifact this broken has no fields to report.
type ArtifactError struct {
	Code    string
	Message string
}

func (e *ArtifactError) Error() string { return e.Code + ": " + e.Message }

func artifactErr(code, format string, args ...any) *ArtifactError {
	return &ArtifactError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Bundle is a validated pack artifact read fully into memory: the set of entry
// paths → their bytes. Every path is a clean, relative, forward-slash path
// (fs.ValidPath) — path-traversal, absolute, and symlink entries were refused at
// read time, so a consumer may key on and store these names without re-checking.
type Bundle struct {
	files map[string][]byte
}

// Has reports whether the bundle carries an entry at name.
func (b *Bundle) Has(name string) bool { _, ok := b.files[name]; return ok }

// drop removes an entry from the bundle. Install uses it for exactly one entry:
// the signature envelope, once verification is done with it. The envelope is
// the only entry the content digest cannot cover — it carries that digest — so
// leaving it in the bundle would leave one signature-exempt byte region inside
// an artifact the rest of the pipeline treats as verified, and MAN-063 would
// let a manifest legally point a surface at it. Dropping it makes "every entry
// this pipeline still holds is covered by the signature" true by construction
// rather than by every future consumer remembering the exception.
func (b *Bundle) drop(name string) { delete(b.files, name) }

// File returns the bytes of the entry at name, and whether it exists.
func (b *Bundle) File(name string) ([]byte, bool) { v, ok := b.files[name]; return v, ok }

// Names returns every entry path in the bundle (unordered).
func (b *Bundle) Names() []string {
	out := make([]string, 0, len(b.files))
	for n := range b.files {
		out = append(out, n)
	}
	return out
}

// FileSet is the bundle's entry-path set — the exact input manifest/1's MAN-063
// (a ui.surfaces[].entry MUST name a bundled file) and MAN-111 (the bundle MUST
// include messages/en.json) are validated against, so those checks are real.
func (b *Bundle) FileSet() map[string]bool {
	set := make(map[string]bool, len(b.files))
	for n := range b.files {
		set[n] = true
	}
	return set
}

// ReadBundle reads a raw zip artifact into a Bundle, enforcing lim and refusing
// every unsafe entry. It NEVER panics on hostile input: archive/zip returns
// errors for malformed data, and a defensive recover at this untrusted-parse
// boundary converts any residual panic into an ArtifactError rather than
// crashing the host. Refusals (all → a 422 ArtifactError, never a 500):
//
//   - the artifact exceeds MaxArtifactBytes, or is not a readable zip;
//   - more than MaxFiles entries;
//   - a path-traversal (../), absolute, or otherwise non-clean entry name
//     (anything fs.ValidPath rejects), or a name containing a backslash or NUL;
//   - a symlink or any non-regular, non-directory entry (a symlink is a classic
//     zip-slip write-outside primitive — refused, never followed);
//   - a duplicate entry name;
//   - an entry whose uncompressed size exceeds MaxFileBytes, or whose bytes
//     (read under a hard limit, so a lying header cannot beat the cap) push the
//     running total past MaxTotalUncompressed.
//
// Directory entries are skipped (a normal zip carries them); only regular files
// land in the Bundle.
func ReadBundle(artifact []byte, lim Limits) (b *Bundle, err error) {
	defer func() {
		if r := recover(); r != nil {
			b = nil
			err = artifactErr("PACK_ARTIFACT_INVALID", "the artifact could not be read as a zip: %v", r)
		}
	}()

	if int64(len(artifact)) > lim.MaxArtifactBytes {
		return nil, artifactErr("PACK_ARTIFACT_TOO_LARGE",
			"artifact is %d bytes, over the %d-byte limit", len(artifact), lim.MaxArtifactBytes)
	}

	zr, err := zip.NewReader(bytes.NewReader(artifact), int64(len(artifact)))
	if err != nil {
		return nil, artifactErr("PACK_ARTIFACT_INVALID", "not a readable zip: %v", err)
	}
	if len(zr.File) > lim.MaxFiles {
		return nil, artifactErr("PACK_ARTIFACT_TOO_MANY_FILES",
			"artifact has %d entries, over the %d-entry limit", len(zr.File), lim.MaxFiles)
	}

	files := make(map[string][]byte, len(zr.File))
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if err := safeEntryName(f.Name); err != nil {
			return nil, err
		}
		if f.Mode()&fs.ModeSymlink != 0 {
			return nil, artifactErr("PACK_ARTIFACT_UNSAFE_ENTRY",
				"entry %q is a symlink, which is not allowed in a pack artifact", f.Name)
		}
		if !f.Mode().IsRegular() {
			return nil, artifactErr("PACK_ARTIFACT_UNSAFE_ENTRY",
				"entry %q is not a regular file", f.Name)
		}
		if _, dup := files[f.Name]; dup {
			return nil, artifactErr("PACK_ARTIFACT_INVALID", "duplicate entry %q", f.Name)
		}
		if f.UncompressedSize64 > uint64(lim.MaxFileBytes) {
			return nil, artifactErr("PACK_ARTIFACT_FILE_TOO_LARGE",
				"entry %q is %d bytes, over the %d-byte per-file limit", f.Name, f.UncompressedSize64, lim.MaxFileBytes)
		}

		data, err := readCapped(f, lim.MaxFileBytes)
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		if total > lim.MaxTotalUncompressed {
			return nil, artifactErr("PACK_ARTIFACT_TOO_LARGE",
				"artifact uncompresses past the %d-byte total limit", lim.MaxTotalUncompressed)
		}
		files[f.Name] = data
	}

	return &Bundle{files: files}, nil
}

// safeEntryName refuses any entry whose name could escape the extraction root or
// is otherwise not a clean relative path. fs.ValidPath is the single source of
// truth for "clean, relative, forward-slash, no . or .. element, no leading or
// trailing slash"; a backslash or NUL (which fs.ValidPath tolerates as ordinary
// bytes) is refused explicitly so a Windows-style ..\ traversal cannot sneak
// through.
func safeEntryName(name string) error {
	if strings.ContainsAny(name, "\\\x00") {
		return artifactErr("PACK_ARTIFACT_UNSAFE_ENTRY",
			"entry %q contains a backslash or NUL, which is not allowed", name)
	}
	if !fs.ValidPath(name) {
		return artifactErr("PACK_ARTIFACT_UNSAFE_ENTRY",
			"entry %q is not a safe relative path (absolute, ../ traversal, or non-clean)", name)
	}
	return nil
}

// readCapped opens a zip entry and reads at most cap bytes, refusing an entry
// that yields more.
//
// The over-read check is a BACKSTOP that cannot currently fire, and saying so is
// more useful than implying a defence it is not the one providing. Two layers
// already bound the stream: the caller refuses a declared UncompressedSize64
// over the same cap before calling here, and archive/zip's own reader fails with
// ErrFormat as soon as the decompressed stream exceeds that declared size. So a
// header that overstates its content is refused above, and one that understates
// it is refused by the stdlib — either way len(data) <= cap on return.
//
// It is kept because it is the only bound that does not depend on either of
// those holding: the LimitReader caps the allocation whatever the archive claims
// and whatever a future stdlib or a second caller with a different cap does.
func readCapped(f *zip.File, cap int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, artifactErr("PACK_ARTIFACT_INVALID", "entry %q could not be opened: %v", f.Name, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(io.LimitReader(rc, cap+1))
	if err != nil {
		return nil, artifactErr("PACK_ARTIFACT_INVALID", "entry %q could not be read: %v", f.Name, err)
	}
	if int64(len(data)) > cap {
		return nil, artifactErr("PACK_ARTIFACT_FILE_TOO_LARGE",
			"entry %q exceeds the %d-byte per-file limit when decompressed", f.Name, cap)
	}
	return data, nil
}
