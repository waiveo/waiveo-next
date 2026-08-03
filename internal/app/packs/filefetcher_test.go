package packs_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// filefetcher_test.go pins the three FileFetcher rules a mutation sweep found
// unheld. Root containment is covered elsewhere; these are the rest of what
// stands between an index author's chosen download_url and this host.
//
// All three are masked by a later failure, which is why the existing coverage
// passes without holding them: delete a rule and the fetch still fails, for a
// reason that describes the symptom rather than the cause.

// TestFileFetcherRefusesANonFileSchemeAsSuch pins the scheme rule by its REASON.
//
// TestFileFetcherRefusesEscapingTheRegistryRoot already passes an https:// URL
// and asserts it is refused — and that assertion survives deleting the scheme
// check, because the URL's path then resolves inside the root, the host is
// simply dropped, and the fetch fails as a missing file instead. The refusal
// looks identical from the outside and means something entirely different: one
// says this deployment does not speak that scheme, the other says it went
// looking on local disk for a path an attacker chose.
func TestFileFetcherRefusesANonFileSchemeAsSuch(t *testing.T) {
	root := t.TempDir()
	// A file that WOULD exist at the resolved path, so a fetcher that ignored
	// the scheme and kept the URL's path would succeed rather than fail on a
	// missing file. Without this the test could not tell the two apart.
	if err := os.WriteFile(filepath.Join(root, "artifact.zip"), []byte("local bytes"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f := packs.FileFetcher{Root: root}

	got, err := f.Fetch(context.Background(), "https://example.invalid/artifact.zip")
	if err == nil {
		t.Fatalf("an https:// download_url was served from local disk, returning %q — an index author's URL "+
			"became a read of this host's filesystem", got)
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("refused with %q, want the scheme rule — a refusal naming anything else means the URL was "+
			"resolved as a local path first", err)
	}
}

// TestFileFetcherRefusesANonRegularTarget pins the stat-then-check shape.
//
// The rule is not merely tidiness about file types. A fifo or a character device
// inside the root would make os.ReadFile BLOCK or stream without end, so relying
// on the read to fail is not an option — the check has to happen before any read
// begins. A directory is used here because it is portable and reaches the same
// rule; the blocking cases are why the rule is written as a pre-check rather
// than as error handling around the read.
func TestFileFetcherRefusesANonRegularTarget(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "notafile"), 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	f := packs.FileFetcher{Root: root}

	_, err := f.Fetch(context.Background(), "file:///notafile")
	if err == nil {
		t.Fatal("a non-regular target was fetched")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("refused with %q, want the regular-file rule", err)
	}
}

// TestFileFetcherCapsTheObjectItReads pins the size cap, and asserts it refuses
// on the STAT rather than after reading: an oversized object must cost a stat,
// not a full read into memory, because its size is chosen by whoever wrote the
// index.
func TestFileFetcherCapsTheObjectItReads(t *testing.T) {
	root := t.TempDir()
	big := bytes.Repeat([]byte("A"), 4096)
	if err := os.WriteFile(filepath.Join(root, "big.zip"), big, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f := packs.FileFetcher{Root: root, MaxBytes: 1024}

	_, err := f.Fetch(context.Background(), "file:///big.zip")
	if err == nil {
		t.Fatal("an object over the cap was fetched")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("refused with %q, want the byte-cap rule", err)
	}

	// The control: the same object under a cap that admits it reads back whole,
	// so the rule above is the cap and not a fetcher that refuses everything.
	ok := packs.FileFetcher{Root: root, MaxBytes: 1 << 20}
	got, err := ok.Fetch(context.Background(), "file:///big.zip")
	if err != nil {
		t.Fatalf("an in-cap object was refused: %v", err)
	}
	if !bytes.Equal(got, big) {
		t.Errorf("in-cap fetch returned %d bytes, want %d", len(got), len(big))
	}
}
