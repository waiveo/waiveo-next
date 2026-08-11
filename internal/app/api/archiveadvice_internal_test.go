package api

// The disk grading's archive half — the sentence that turns "prune old images
// and archives" from a category into an amount and a place.
//
// Internal because the disk cannot be made short of space from outside the
// package: `storageHealth` measures a real filesystem, and a test that filled
// one to below 5 GiB to observe a string would be a test nobody could run twice.
// What IS testable, and is where every way this can be wrong lives, is the two
// pieces the grade appends: what the footprint counts, and what the sentence
// says about it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheArchiveFootprintCountsWhatTheListingPublishes.
//
// Two different answers to "how much are the backups using" — one on the health
// page, one implied by the Backup page's own list — is how an operator deletes
// every container they have and finds the number unmoved, because the difference
// was a scratch snapshot nothing offered them.
func TestTheArchiveFootprintCountsWhatTheListingPublishes(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	write("workspace-A.waiveo-archive", 1000)
	write("workspace-B.waiveo-archive", 2000)
	// Everything the listing excludes, each of which would otherwise inflate the
	// number an operator is asked to act on.
	write(".01J8ZSCRATCH.snapshot", 9_000_000)
	write("operator-notes.txt", 500)
	if err := os.Mkdir(filepath.Join(dir, "sub.waiveo-archive"), 0o700); err != nil {
		t.Fatalf("seed directory: %v", err)
	}

	count, bytes := archiveFootprint(dir)
	if count != 2 || bytes != 3000 {
		t.Errorf("footprint = (%d, %d), want (2, 3000) — the two containers the listing publishes and nothing else",
			count, bytes)
	}
}

// TestAnUnreadableArchiveDirectoryIsZeroRatherThanAFailure. This feeds one
// advisory clause appended to a grade already computed from a real measurement;
// failing the health read because a directory was missing would take away the
// page an operator opened to find that out.
func TestAnUnreadableArchiveDirectoryIsZeroRatherThanAFailure(t *testing.T) {
	count, bytes := archiveFootprint(filepath.Join(t.TempDir(), "never-exported"))
	if count != 0 || bytes != 0 {
		t.Errorf("footprint of a missing directory = (%d, %d), want (0, 0)", count, bytes)
	}
}

// TestTheDiskAdviceNamesTheBackupsAndWhereToDeleteThem.
func TestTheDiskAdviceNamesTheBackupsAndWhereToDeleteThem(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "workspace-A.waiveo-archive"), make([]byte, 3<<20), 0o600); err != nil {
		t.Fatalf("seed a container: %v", err)
	}
	srv := &server{workspaceArchive: &WorkspaceArchive{Dir: dir}}

	got := srv.archiveAdvice()
	for _, want := range []string{"1 backup is", "3.0 MiB", "Backup page"} {
		if !strings.Contains(got, want) {
			t.Errorf("advice = %q, want it to contain %q", got, want)
		}
	}
}

// TestTheDiskAdviceIsSILENTWhenThereAreNoBackups.
//
// "0 backups are using 0 B" on a box with none would send an operator to a page
// that cannot help them — the same defect as the advice this replaces, one step
// along. A deployment with no archive destination at all says nothing for the
// same reason.
func TestTheDiskAdviceIsSILENTWhenThereAreNoBackups(t *testing.T) {
	empty := &server{workspaceArchive: &WorkspaceArchive{Dir: t.TempDir()}}
	if got := empty.archiveAdvice(); got != "" {
		t.Errorf("advice on a box with no containers = %q, want empty", got)
	}
	unwired := &server{}
	if got := unwired.archiveAdvice(); got != "" {
		t.Errorf("advice on a deployment with no archive destination = %q, want empty", got)
	}
}
