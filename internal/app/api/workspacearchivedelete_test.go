package api_test

// workspacearchivedelete_test.go covers the one operation on this surface that
// destroys a backup: DELETE /api/v1/workspace/archives/{name}.
//
// Every case here asserts BOTH halves — the response, and whether the bytes are
// still on disk. That is not belt-and-braces. A destructive operation has two
// ways to be wrong and they are opposites: it can refuse and delete anyway, and
// it can report success and delete nothing. Asserting only the status code
// catches neither, and a suite that checked only the status would have passed
// against a handler that unlinked the file before evaluating its preconditions.

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/archive"
)

// problemCode reads a Problem's `code` — the sole machine-readable discriminant
// api/1 defines, and therefore the only thing a refusal is asserted on here.
func problemCode(t *testing.T, raw []byte) string {
	t.Helper()
	var p struct {
		Code string `json:"code"`
	}
	mustUnmarshal(t, raw, &p)
	return p.Code
}

// archiveOnDisk reports whether the container is still in the archive directory.
func archiveOnDisk(t *testing.T, e *workspaceEnv, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(e.archiveDir, name))
	return err == nil
}

// archiveETag reads one container's published entity-tag out of the listing —
// the same value a console holds when it offers the Delete button, so every case
// below conditions on what a real client would actually send.
func archiveETag(t *testing.T, e *workspaceEnv, name string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, "/api/v1/workspace/archives", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list archives = %d (body %s)", resp.StatusCode, raw)
	}
	var page struct {
		Items []struct {
			Name string `json:"name"`
			ETag string `json:"etag"`
		} `json:"items"`
	}
	mustUnmarshal(t, raw, &page)
	for _, it := range page.Items {
		if it.Name == name {
			if it.ETag == "" {
				t.Fatalf("the listing published no etag for %q — a client cannot condition a delete on nothing", name)
			}
			return it.ETag
		}
	}
	t.Fatalf("the listing does not carry %q: %s", name, raw)
	return ""
}

// deleteArchive drives the operation with the given If-Match header, or without
// one when etag is empty.
func deleteArchive(t *testing.T, e *workspaceEnv, name, etag string) (*http.Response, []byte) {
	t.Helper()
	var headers map[string]string
	if etag != "" {
		headers = map[string]string{"If-Match": etag}
	}
	return e.do(t, http.MethodDelete, "/api/v1/workspace/archives/"+name, nil, headers)
}

// TestDeletingAnArchiveRECLAIMSIT — the whole point. The container is gone from
// the listing AND from the disk, because the operator's problem is the disk and
// a listing that merely stops mentioning a file has solved nothing.
func TestDeletingAnArchiveRECLAIMSIT(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	name := seedExportedArchive(t, e)

	resp, raw := deleteArchive(t, e, name, archiveETag(t, e, name))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204 (body %s)", resp.StatusCode, raw)
	}
	if got := archiveNames(t, e); len(got) != 0 {
		t.Errorf("listing after the delete = %v, want empty", got)
	}
	if archiveOnDisk(t, e, name) {
		t.Error("the container is still on disk: a delete that only hides a backup from the listing has " +
			"reclaimed nothing, and the reason this operation exists is a disk that is filling up")
	}
}

// TestADeleteWITHOUTIfMatchChangesNothing. API-022 admits no unconditional
// overwrite, and the second assertion is the one that matters: a 428 whose file
// is gone would be a refusal that performed the act it refused.
func TestADeleteWITHOUTIfMatchChangesNothing(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	name := seedExportedArchive(t, e)

	resp, raw := deleteArchive(t, e, name, "")
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("delete with no If-Match = %d, want 428 (body %s)", resp.StatusCode, raw)
	}
	if got := problemCode(t, raw); got != "IF_MATCH_REQUIRED" {
		t.Errorf("code = %q, want IF_MATCH_REQUIRED", got)
	}
	if !archiveOnDisk(t, e, name) {
		t.Fatal("the container was deleted despite the 428")
	}
	// And the refusal SAYS so. An operator looking at an irreversible control
	// must not have to infer from a status code whether their backup survived.
	if !bytes.Contains(raw, []byte("not deleted")) {
		t.Errorf("the refusal does not say the backup survived: %s", raw)
	}
}

// TestADeleteWithASTALEIfMatchChangesNothing.
//
// The scenario is the real disaster-recovery path, not a contrived one: an
// operator copies a container BACK into the archive directory (the documented
// way to restore onto new hardware), reusing a name the console already listed.
// A delete carrying the old validator must not unlink the file that replaced the
// one the operator was looking at.
func TestADeleteWithASTALEIfMatchChangesNothing(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	name := seedExportedArchive(t, e)
	stale := archiveETag(t, e, name)

	// The bytes under that name are replaced — a different container, copied
	// back from off-box storage under the same file name.
	//
	// Deliberately the SAME LENGTH as the container it replaces. A different
	// size would make this pass against a validator derived from size alone, and
	// the whole question the tag has to answer is "are these the same bytes",
	// which two files of equal length routinely are not. Only the modification
	// time separates them here, so the case fails unless the tag carries it.
	path := filepath.Join(e.archiveDir, name)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the container: %v", err)
	}
	replacement := bytes.Repeat([]byte{0x5a}, len(original))
	if bytes.Equal(replacement, original) {
		t.Fatal("fixture: the replacement is byte-identical to the container it replaces")
	}
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatalf("replace the container: %v", err)
	}
	// Written within the same millisecond as the export on a fast machine, which
	// would make the two tags equal by accident. Aged explicitly so the case is
	// testing the validator and not the clock.
	newer := time.UnixMilli(1).Add(48 * time.Hour)
	if err := os.Chtimes(path, newer, newer); err != nil {
		t.Fatalf("age the replacement: %v", err)
	}

	resp, raw := deleteArchive(t, e, name, stale)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("delete with a stale If-Match = %d, want 412 (body %s)", resp.StatusCode, raw)
	}
	if got := problemCode(t, raw); got != "REVISION_CONFLICT" {
		t.Errorf("code = %q, want REVISION_CONFLICT", got)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the replacement container was deleted despite the 412: %v", err)
	}
	if !bytes.Equal(onDisk, replacement) {
		t.Error("the bytes under that name are not the replacement's")
	}

	// The FRESH validator works, so the refusal is a precondition and not a
	// permanent block: an operator who re-reads the list can still reclaim it.
	resp, raw = deleteArchive(t, e, name, archiveETag(t, e, name))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete with the fresh If-Match = %d, want 204 (body %s)", resp.StatusCode, raw)
	}
	if archiveOnDisk(t, e, name) {
		t.Error("the container survived a 204")
	}
}

// TestADeleteCanOnlyEverTargetAContainer — the download's own rule, applied to
// the operation that destroys rather than reads.
//
// The archive directory transiently holds the export's scratch snapshot, which
// is a CLEARTEXT copy of the entire workspace. Neither read nor write may name
// it, and the refusal must be the SAME 404 an absent file gets: a delete that
// answered 428 for a file that exists and 404 for one that does not would be an
// existence oracle over that directory.
func TestADeleteCanOnlyEverTargetAContainer(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	seedExportedArchive(t, e)

	inDir := map[string]string{
		".01J8ZSCRATCH.snapshot": "SQLite format 3\x00 the whole workspace, in the clear",
		"operator-notes.txt":     "the passphrase is on the whiteboard",
		".waiveo-archive":        "a hidden file nobody exported",
	}
	for name, body := range inDir {
		if err := os.WriteFile(filepath.Join(e.archiveDir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	bad := []string{
		".01J8ZSCRATCH.snapshot",
		"operator-notes.txt",
		".waiveo-archive",
		"..%2f..%2fetc%2fpasswd",
		"..%2fworkspace_signing_key.pem",
		"nothing-here.waiveo-archive",
	}
	for _, name := range bad {
		// A REAL validator is presented, so a 404 here cannot be the precondition
		// refusing on the caller's behalf — the handler is genuinely declining to
		// resolve the name.
		resp, raw := deleteArchive(t, e, name, `"1-1"`)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("DELETE archives/%s = %d, want 404 (body %s)", name, resp.StatusCode, raw)
		}
	}
	for name := range inDir {
		if _, err := os.Stat(filepath.Join(e.archiveDir, name)); err != nil {
			t.Errorf("%s was deleted despite its 404: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.keyDir, "workspace_signing_key.pem")); err != nil {
		t.Errorf("the workspace signing key was deleted through a traversal: %v", err)
	}
}

// TestDeletingAnArchiveIsClosedToANonOwner. The container is the entire
// workspace; a site admin must not be able to destroy the deployment's only
// backup, and the refusal must leave it there.
func TestDeletingAnArchiveIsClosedToANonOwner(t *testing.T) {
	e := newWorkspaceEnv(t)
	org := e.seedWorkspace(t)
	name := seedExportedArchive(t, e)
	etag := archiveETag(t, e, name)
	site := e.createNode(t, siteUnder(org))
	admin := e.principalWith(t, roleAt{node: site, role: auth.RoleAdmin})

	resp, raw := e.as(t, admin, http.MethodDelete, "/api/v1/workspace/archives/"+name, nil,
		map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete as a site admin = %d, want 403 (body %s)", resp.StatusCode, raw)
	}
	if !archiveOnDisk(t, e, name) {
		t.Fatal("a site admin's refused delete removed the container anyway")
	}
}

// TestAnArchiveAnACCEPTEDRestoreIsReadingCannotBeDeleted.
//
// The env's Job runner is stopped until runJobs, so this drives the exact window
// the claim exists for: the restore is ACCEPTED and has not opened the file yet.
// Unlinking here would fail that job with "no archive of that name" — a sentence
// about a container the operator did have, in a job they may not connect to the
// button they pressed. Whether the unlink lands before or after the job's own
// `open` is a scheduler race nobody can observe, so the operation refuses across
// the whole accepted lifetime rather than resolving differently each time.
func TestAnArchiveAnACCEPTEDRestoreIsReadingCannotBeDeleted(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	name := seedExportedArchive(t, e)
	etag := archiveETag(t, e, name)

	resp, raw := e.postWorkspace(t, e.auth.Credential(), "restore",
		map[string]any{"archive": name, "passphrase": testExportPassphrase})
	job := acceptedJob(t, resp, raw)

	resp, raw = deleteArchive(t, e, name, etag)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete while a restore holds it = %d, want 409 (body %s)", resp.StatusCode, raw)
	}
	if got := problemCode(t, raw); got != "ARCHIVE_IN_USE" {
		t.Errorf("code = %q, want ARCHIVE_IN_USE", got)
	}
	// The refusal NAMES the job, so the wait has an end the operator can watch.
	if !bytes.Contains(raw, []byte(job.ID)) {
		t.Errorf("the refusal does not name the job holding the container: %s", raw)
	}
	if !archiveOnDisk(t, e, name) {
		t.Fatal("the container was deleted despite the 409")
	}

	// The PRECONDITION still comes first. A caller who omitted If-Match against a
	// busy container gets 428, not 409 — API-022/023 are categorical, and a route
	// that answered its own refusal ahead of the precondition would be the one
	// resource on this surface whose If-Match rule differs from every other.
	resp, raw = deleteArchive(t, e, name, "")
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Errorf("delete with no If-Match on a busy container = %d, want 428 (body %s)", resp.StatusCode, raw)
	}

	// Once the job reaches a terminal state the claim is released and the SAME
	// request succeeds — the refusal is a wait, not a permanent block. A guard
	// that never lifts is the other way to strand an operator on a full disk.
	e.runJobs()
	resp, raw = deleteArchive(t, e, name, etag)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete after the restore finished = %d, want 204 (body %s)", resp.StatusCode, raw)
	}
	if archiveOnDisk(t, e, name) {
		t.Error("the container survived a 204")
	}
}

// TestAnArchiveAnACCEPTEDExportIsWritingCannotBeDeleted — the same guard from the
// other side.
//
// The half-written container IS listed the moment it appears (the listing filters
// on the suffix, not on completeness), so an operator prowling a full disk can
// see and target it. Unlinking it mid-write does not fail the export: the writer
// holds the descriptor, so every byte lands in a file with no name, the job
// reports `succeeded`, and the backup does not exist. That is the precise shape
// this codebase keeps removing — a surface reporting work it did not perform.
func TestAnArchiveAnACCEPTEDExportIsWritingCannotBeDeleted(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)

	resp, raw := e.postWorkspace(t, e.auth.Credential(), "export",
		map[string]any{"passphrase": testExportPassphrase})
	job := acceptedJob(t, resp, raw)
	name := "workspace-" + job.ID + ".waiveo-archive"

	// A real validator is presented — the file does not exist yet, so this must
	// be a 404 rather than a 409: the honest answer about a name with nothing
	// under it, and it means the claim never becomes a way to learn that an
	// export is running from a request that would otherwise 404.
	resp, raw = deleteArchive(t, e, name, `"1-1"`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete before the export wrote anything = %d, want 404 (body %s)", resp.StatusCode, raw)
	}

	// Now the container EXISTS and the export is still accepted — the state a
	// real box is in for the whole minute a large export takes, and the state an
	// operator prowling a full disk can see in the listing. Stood up directly
	// because the export's write is not otherwise pausable, and it is the same
	// state from the delete's point of view: a name that resolves, held by a job.
	if err := os.WriteFile(filepath.Join(e.archiveDir, name), []byte("half an export"), 0o600); err != nil {
		t.Fatalf("seed the half-written container: %v", err)
	}
	resp, raw = deleteArchive(t, e, name, archiveETag(t, e, name))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete while an export is writing = %d, want 409 (body %s)", resp.StatusCode, raw)
	}
	if got := problemCode(t, raw); got != "ARCHIVE_IN_USE" {
		t.Errorf("code = %q, want ARCHIVE_IN_USE", got)
	}
	if !bytes.Contains(raw, []byte(job.ID)) {
		t.Errorf("the refusal does not name the export holding the container: %s", raw)
	}
	if !archiveOnDisk(t, e, name) {
		t.Fatal("the half-written container was deleted despite the 409")
	}

	// Draining the runner releases the claim, whatever the job's own outcome —
	// the export below fails, because the seeded file is in the way of its
	// O_EXCL create, and the guard must still lift. A claim that only lifted on
	// SUCCESS would leave a failed export's name undeletable forever, on the
	// operation whose whole purpose is freeing a disk that is filling up.
	e.runJobs()
	resp, raw = deleteArchive(t, e, name, archiveETag(t, e, name))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete after the export finished = %d, want 204 (body %s)", resp.StatusCode, raw)
	}
	if archiveOnDisk(t, e, name) {
		t.Error("the container survived a 204")
	}
}

// TestAnInFlightDOWNLOADSurvivesTheDelete.
//
// The third design call this operation had to make, and it is deliberately NOT
// a refusal. A download holds an open descriptor, and unlinking a file somebody
// holds open leaves every byte readable through that descriptor — so the
// operator taking the backup off the box gets the WHOLE container, verified,
// while the operator reclaiming the disk gets their space back. Refusing the
// delete for the duration would be a guard bought with no safety at all.
//
// Proven by verifying the DOWNLOADED bytes through archive.Open after the file
// is gone: the header signature, every frame's authentication tag and the
// recomputed body digest all have to hold, so a stream truncated by the unlink
// cannot pass.
func TestAnInFlightDOWNLOADSurvivesTheDelete(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	name := seedExportedArchive(t, e)
	etag := archiveETag(t, e, name)

	// The response is taken but NOT drained — the handler has opened the file and
	// is streaming, exactly the window the delete has to be safe against.
	req, err := http.NewRequest(http.MethodGet, e.ts.URL+"/api/v1/workspace/archives/"+name, nil)
	if err != nil {
		t.Fatalf("build the download request: %v", err)
	}
	e.auth.Authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("start the download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download = %d, want 200", resp.StatusCode)
	}

	del, raw := deleteArchive(t, e, name, etag)
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete during a download = %d, want 204 — a download is not a conflict (body %s)",
			del.StatusCode, raw)
	}
	if archiveOnDisk(t, e, name) {
		t.Fatal("the container is still on disk after its 204")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the in-flight download after the delete: %v", err)
	}
	if _, _, err := archive.Open(bytes.NewReader(body), testExportPassphrase, e.key.Public()); err != nil {
		t.Fatalf("the in-flight download did not survive the delete: %v — an operator's off-box copy would be "+
			"truncated at exactly the moment they were saving it", err)
	}

	// And a download STARTED after the delete is the ordinary 404, not a
	// half-served file.
	after, raw := e.do(t, http.MethodGet, "/api/v1/workspace/archives/"+name, nil, nil)
	if after.StatusCode != http.StatusNotFound {
		t.Errorf("download after the delete = %d, want 404 (body %s)", after.StatusCode, raw)
	}
}
