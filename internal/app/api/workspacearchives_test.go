package api_test

// workspacearchives_test.go covers the two reads that make the backup loop
// usable without a shell (parity row 7.5): the listing an operator picks a
// container from, and the download that gets the bytes off the box.
//
// The listing is exercised end to end in workspaceroundtrip_test.go, where its
// output actually drives a restore. This file holds the properties that round
// trip does not touch: what is EXCLUDED from the listing, what the download
// serves, and what a hostile `name` gets.

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/archive"
)

// seedExportedArchive runs one export to completion and returns the container's
// file name.
func seedExportedArchive(t *testing.T, e *workspaceEnv) string {
	t.Helper()
	resp, raw := e.postWorkspace(t, e.auth.Credential(), "export", map[string]any{"passphrase": testExportPassphrase})
	job := acceptedJob(t, resp, raw)
	e.runJobs()
	if got := e.polledJob(t, e.auth.Credential(), job.ID); got.State != "succeeded" {
		t.Fatalf("export job = %q, want succeeded (%+v)", got.State, got.Targets)
	}
	return "workspace-" + job.ID + ".waiveo-archive"
}

// TestTheArchiveListingOffersOnlyRESTORABLEContainers. The restore takes a file
// name, and every name this listing publishes is a name an operator will hand
// straight back to it. Offering a scratch snapshot or a stray file as a "backup"
// would be offering a restore that is going to fail — or worse, one that
// succeeds against something that is not a workspace.
func TestTheArchiveListingOffersOnlyRESTORABLEContainers(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	name := seedExportedArchive(t, e)

	// Three things that live in an archive directory and are NOT backups: the
	// export's own transient snapshot shape, an unrelated file, and a directory.
	for _, junk := range []string{".01J8ZSCRATCH.snapshot", "notes.txt", "workspace-partial.tmp"} {
		if err := os.WriteFile(filepath.Join(e.archiveDir, junk), []byte("not a container"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", junk, err)
		}
	}
	if err := os.Mkdir(filepath.Join(e.archiveDir, "sub.waiveo-archive"), 0o700); err != nil {
		t.Fatalf("seed directory: %v", err)
	}

	got := archiveNames(t, e)
	if len(got) != 1 || got[0] != name {
		t.Fatalf("listing = %v, want exactly the one container %q", got, name)
	}
}

// TestTheArchiveListingIsNewestFirst — the backup an operator wants is almost
// always the last one, and a directory's own order is the filesystem's, not a
// decision anybody made.
func TestTheArchiveListingIsNewestFirst(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	first := seedExportedArchive(t, e)
	second := seedExportedArchive(t, e)

	// The env's clock is fixed, so both containers would otherwise carry the
	// same mtime and the order would be the tie-break rather than the sort.
	// Aged explicitly so the sort is what is being tested.
	older := time.Unix(0, 0).Add(time.Hour)
	if err := os.Chtimes(filepath.Join(e.archiveDir, first), older, older); err != nil {
		t.Fatalf("age the first container: %v", err)
	}

	got := archiveNames(t, e)
	if len(got) != 2 {
		t.Fatalf("listing = %v, want 2", got)
	}
	if got[0] != second || got[1] != first {
		t.Errorf("listing = %v, want the NEWER container (%s) first", got, second)
	}
}

// TestAFreshBoxListsNoArchivesRatherThanFailing. A deployment that has never
// exported has no archive directory yet; "no backups" is the truth about it, and
// a 500 would make a fresh box look broken on the page that exists to say
// whether it is.
func TestAFreshBoxListsNoArchivesRatherThanFailing(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	if err := os.RemoveAll(e.archiveDir); err != nil {
		t.Fatalf("remove the archive dir: %v", err)
	}
	if got := archiveNames(t, e); len(got) != 0 {
		t.Fatalf("listing on a box that has never exported = %v, want empty", got)
	}
}

// TestDownloadingAnArchiveServesTheVERIFIABLEContainer — the bytes off the box
// must be the container, not a truncated or re-encoded copy of it. Proven by
// opening the DOWNLOADED bytes through archive.Open, which verifies the header
// signature, every frame's authentication tag and the recomputed body digest: a
// stream that lost or reordered a byte cannot survive it.
func TestDownloadingAnArchiveServesTheVERIFIABLEContainer(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	name := seedExportedArchive(t, e)

	resp, raw := e.do(t, http.MethodGet, "/api/v1/workspace/archives/"+name, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}
	if got := resp.Header.Get("Content-Disposition"); got != `attachment; filename="`+name+`"` {
		t.Errorf("Content-Disposition = %q — a backup must SAVE, not render", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: a cached backup restored later is a silent data-loss event", got)
	}

	onDisk, err := os.ReadFile(filepath.Join(e.archiveDir, name))
	if err != nil {
		t.Fatalf("read the container: %v", err)
	}
	if !bytes.Equal(raw, onDisk) {
		t.Fatalf("the download is %d bytes and the container is %d", len(raw), len(onDisk))
	}
	if _, _, err := archive.Open(bytes.NewReader(raw), testExportPassphrase, e.key.Public()); err != nil {
		t.Fatalf("the DOWNLOADED bytes do not verify as an archive/1 container: %v — an operator's off-box backup "+
			"would fail at exactly the moment they needed it", err)
	}
}

// TestADownloadCanOnlyEverServeAContainer.
//
// Two distinct things are being refused here, and the second is the one that
// matters most:
//
//   - A `name` that tries to escape the archive directory. (These are largely
//     refused by the router before the handler sees them — a decoded separator
//     makes the single-segment pattern not match — but the handler refuses them
//     on its own terms too, because a route's shape is not a security control.)
//   - A file that IS in the archive directory and is NOT a container. The
//     archive directory transiently holds the export's own scratch snapshot,
//     which is a CLEARTEXT copy of the entire workspace (workspacerun.go
//     unlinks it within microseconds of opening it, but it exists). Serving
//     that under a download route would hand out unencrypted what the whole
//     archive format exists to encrypt. The suffix-and-dotfile rule is what
//     makes that unreachable, and nothing about the route's path shape would.
func TestADownloadCanOnlyEverServeAContainer(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	seedExportedArchive(t, e)

	// Real, readable files IN the archive directory, so a refusal here is a
	// refusal to serve something that genuinely exists rather than a 404 for
	// want of a target.
	inDir := map[string]string{
		".01J8ZSCRATCH.snapshot": "SQLite format 3\x00 the whole workspace, in the clear",
		"operator-notes.txt":     "the passphrase is on the whiteboard",
		"workspace-partial.tmp":  "half an export",
		// A dotfile that nonetheless carries the suffix: the one shape the
		// suffix rule alone would admit, which is why the dotfile rule is not
		// redundant with it.
		".waiveo-archive": "a hidden file nobody exported",
	}
	for name, body := range inDir {
		if err := os.WriteFile(filepath.Join(e.archiveDir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	// And a real file OUTSIDE it, for the traversal attempts to aim at.
	if _, err := os.Stat(filepath.Join(e.keyDir, "workspace_signing_key.pem")); err != nil {
		t.Fatalf("fixture: expected the workspace signing key outside the archive dir: %v", err)
	}

	bad := []string{
		".01J8ZSCRATCH.snapshot",
		"operator-notes.txt",
		"workspace-partial.tmp",
		".waiveo-archive",
		"..%2f..%2fetc%2fpasswd",
		"..%2fworkspace_signing_key.pem",
		"%2e%2e%2fworkspace_signing_key.pem",
		"nothing-here.waiveo-archive",
	}
	for _, name := range bad {
		resp, raw := e.do(t, http.MethodGet, "/api/v1/workspace/archives/"+name, nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET archives/%s = %d, want 404 (body %s)", name, resp.StatusCode, raw)
			continue
		}
		// And it must be a Problem, not the file: a 404 that still wrote bytes
		// would be the disclosure with a misleading status on it.
		for _, secret := range []string{"the whole workspace", "whiteboard", "hidden file", "PRIVATE KEY"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("GET archives/%s returned content from %q despite its 404", name, secret)
			}
		}
	}
}

// TestNeitherArchiveReadIsOpenToANonOwner. These bytes are the entire workspace
// in one file; an admin bound at one site must not be able to enumerate or take
// them.
func TestNeitherArchiveReadIsOpenToANonOwner(t *testing.T) {
	e := newWorkspaceEnv(t)
	org := e.seedWorkspace(t)
	name := seedExportedArchive(t, e)
	site := e.createNode(t, siteUnder(org))
	admin := e.principalWith(t, roleAt{node: site, role: auth.RoleAdmin})

	for _, path := range []string{"/api/v1/workspace/archives", "/api/v1/workspace/archives/" + name} {
		resp, raw := e.as(t, admin, http.MethodGet, path, nil, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s as a site admin = %d, want 403 (body %s)", path, resp.StatusCode, raw)
		}
	}
}

// archiveNames reads the listing and returns the names in the order served.
func archiveNames(t *testing.T, e *workspaceEnv) []string {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, "/api/v1/workspace/archives", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list archives = %d (body %s)", resp.StatusCode, raw)
	}
	var page struct {
		Items []struct {
			Name         string `json:"name"`
			DownloadPath string `json:"download_path"`
		} `json:"items"`
	}
	mustUnmarshal(t, raw, &page)
	out := make([]string, 0, len(page.Items))
	for _, it := range page.Items {
		if it.DownloadPath != "/api/v1/workspace/archives/"+it.Name {
			t.Errorf("download_path = %q for %q; a client composing it itself is a client re-encoding a file name into a path",
				it.DownloadPath, it.Name)
		}
		out = append(out, it.Name)
	}
	return out
}
