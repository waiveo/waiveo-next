package packs_test

import (
	"errors"
	"io/fs"
	"math/rand"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// tinyLimits are cheap caps so the size/count refusals are exercised without
// building megabytes of fixture.
var tinyLimits = packs.Limits{
	MaxArtifactBytes:     4096,
	MaxFileBytes:         64,
	MaxTotalUncompressed: 256,
	MaxFiles:             3,
}

// artifactCode asserts err is an *ArtifactError with the wanted code.
func artifactCode(t *testing.T, err error, want string) {
	t.Helper()
	var aerr *packs.ArtifactError
	if !errors.As(err, &aerr) {
		t.Fatalf("error = %v (%T), want *packs.ArtifactError", err, err)
	}
	if aerr.Code != want {
		t.Fatalf("artifact code = %q, want %q (msg %q)", aerr.Code, want, aerr.Message)
	}
}

// TestReadBundleValid reads a plain multi-file zip back byte-for-byte.
func TestReadBundleValid(t *testing.T) {
	z := filesZip(t, map[string]string{
		"manifest.json":      `{"id":"a/b"}`,
		"ui/menu-items.json": menuItemsDoc,
		"messages/en.json":   enCatalog,
	})
	b, err := packs.ReadBundle(z, packs.DefaultLimits)
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}
	got, ok := b.File("ui/menu-items.json")
	if !ok || string(got) != menuItemsDoc {
		t.Fatalf("File(ui/menu-items.json) = %q,%v; want the doc", got, ok)
	}
	if !b.FileSet()["messages/en.json"] {
		t.Fatalf("FileSet missing messages/en.json: %v", b.Names())
	}
}

// TestReadBundleSkipsDirectories: a directory entry is skipped, not refused, and
// does not appear in the bundle.
func TestReadBundleSkipsDirectories(t *testing.T) {
	z := buildZip(t,
		zentry{name: "ui/", mode: fs.ModeDir | 0o755},
		zentry{name: "ui/x.json", body: `{}`},
	)
	b, err := packs.ReadBundle(z, packs.DefaultLimits)
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}
	if b.Has("ui/") {
		t.Fatalf("directory entry leaked into the bundle: %v", b.Names())
	}
	if !b.Has("ui/x.json") {
		t.Fatalf("regular file missing: %v", b.Names())
	}
}

// TestReadBundleZipSlip: a ../ traversal entry is refused.
func TestReadBundleZipSlip(t *testing.T) {
	z := buildZip(t, zentry{name: "../escape.json", body: `{}`})
	_, err := packs.ReadBundle(z, packs.DefaultLimits)
	artifactCode(t, err, "PACK_ARTIFACT_UNSAFE_ENTRY")
}

// TestReadBundleNestedZipSlip: a non-leading ../ escaping a subdir is refused.
func TestReadBundleNestedZipSlip(t *testing.T) {
	z := buildZip(t, zentry{name: "ui/../../escape.json", body: `{}`})
	_, err := packs.ReadBundle(z, packs.DefaultLimits)
	artifactCode(t, err, "PACK_ARTIFACT_UNSAFE_ENTRY")
}

// TestReadBundleAbsolutePath: an absolute entry name is refused.
func TestReadBundleAbsolutePath(t *testing.T) {
	z := buildZip(t, zentry{name: "/etc/passwd", body: `x`})
	_, err := packs.ReadBundle(z, packs.DefaultLimits)
	artifactCode(t, err, "PACK_ARTIFACT_UNSAFE_ENTRY")
}

// TestReadBundleBackslash: a backslash (Windows ..\ traversal) is refused even
// though fs.ValidPath tolerates the byte.
func TestReadBundleBackslash(t *testing.T) {
	z := buildZip(t, zentry{name: `..\evil.json`, body: `x`})
	_, err := packs.ReadBundle(z, packs.DefaultLimits)
	artifactCode(t, err, "PACK_ARTIFACT_UNSAFE_ENTRY")
}

// TestReadBundleSymlink: a symlink entry is refused, never followed.
func TestReadBundleSymlink(t *testing.T) {
	z := buildZip(t, zentry{name: "link.json", body: "/etc/passwd", mode: fs.ModeSymlink | 0o777})
	_, err := packs.ReadBundle(z, packs.DefaultLimits)
	artifactCode(t, err, "PACK_ARTIFACT_UNSAFE_ENTRY")
}

// TestReadBundleArtifactTooLarge: an artifact whose compressed size is over the
// byte cap is refused before it is even parsed. The cap is set below any real
// zip's own container size so the check fires on the artifact, not a per-file
// or count rule.
func TestReadBundleArtifactTooLarge(t *testing.T) {
	lim := packs.Limits{MaxArtifactBytes: 50, MaxFileBytes: 1 << 20, MaxTotalUncompressed: 1 << 20, MaxFiles: 100}
	z := filesZip(t, map[string]string{"a.json": "hello"})
	_, err := packs.ReadBundle(z, lim)
	artifactCode(t, err, "PACK_ARTIFACT_TOO_LARGE")
}

// TestReadBundleTooManyFiles: more entries than the count cap is refused.
func TestReadBundleTooManyFiles(t *testing.T) {
	z := filesZip(t, map[string]string{
		"a.json": "1", "b.json": "2", "c.json": "3", "d.json": "4",
	})
	_, err := packs.ReadBundle(z, tinyLimits)
	artifactCode(t, err, "PACK_ARTIFACT_TOO_MANY_FILES")
}

// TestReadBundleFileTooLarge: a single entry over the per-file cap is refused.
func TestReadBundleFileTooLarge(t *testing.T) {
	z := filesZip(t, map[string]string{"big.json": strings.Repeat("y", 100)})
	_, err := packs.ReadBundle(z, tinyLimits)
	artifactCode(t, err, "PACK_ARTIFACT_FILE_TOO_LARGE")
}

// TestReadBundleTotalUncompressed: many small entries whose sum passes the total
// cap are refused even though each is under the per-file cap.
func TestReadBundleTotalUncompressed(t *testing.T) {
	lim := packs.Limits{MaxArtifactBytes: 1 << 20, MaxFileBytes: 64, MaxTotalUncompressed: 100, MaxFiles: 100}
	z := filesZip(t, map[string]string{
		"a.json": strings.Repeat("a", 60),
		"b.json": strings.Repeat("b", 60),
	})
	_, err := packs.ReadBundle(z, lim)
	artifactCode(t, err, "PACK_ARTIFACT_TOO_LARGE")
}

// TestReadBundleDuplicateName: two entries with the same name are refused.
func TestReadBundleDuplicateName(t *testing.T) {
	z := buildZip(t,
		zentry{name: "dup.json", body: "1"},
		zentry{name: "dup.json", body: "2"},
	)
	_, err := packs.ReadBundle(z, packs.DefaultLimits)
	artifactCode(t, err, "PACK_ARTIFACT_INVALID")
}

// TestReadBundleMalformed: arbitrary non-zip bytes yield an ArtifactError, never
// a panic.
func TestReadBundleMalformed(t *testing.T) {
	_, err := packs.ReadBundle([]byte("this is definitely not a zip file"), packs.DefaultLimits)
	artifactCode(t, err, "PACK_ARTIFACT_INVALID")
}

// TestReadBundleFuzzNeverPanics: random and truncated byte strings never panic
// the reader — every input either reads or returns an ArtifactError.
func TestReadBundleFuzzNeverPanics(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	valid := basePackZip(t, baseManifest())
	for i := 0; i < 500; i++ {
		var in []byte
		switch i % 3 {
		case 0: // random bytes
			in = make([]byte, rng.Intn(200))
			rng.Read(in)
		case 1: // a truncated real zip
			n := rng.Intn(len(valid) + 1)
			in = valid[:n]
		case 2: // a real zip with one byte flipped
			in = append([]byte(nil), valid...)
			if len(in) > 0 {
				in[rng.Intn(len(in))] ^= byte(1 + rng.Intn(255))
			}
		}
		b, err := packs.ReadBundle(in, packs.DefaultLimits)
		if err == nil && b == nil {
			t.Fatalf("case %d: nil bundle and nil error", i)
		}
		// No assertion on which outcome — the contract is only "never panics".
	}
}
