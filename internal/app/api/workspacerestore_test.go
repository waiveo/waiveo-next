package api_test

import (
	"bytes"
	"net/http"
	"os"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/restoreswap"
	"github.com/maaxton/waiveo-next/internal/archive"
)

// The restore path, driven against a container this deployment really produced.
//
// A restore test that hand-builds its input proves the code can read what the
// test writes. These export first and restore THAT, so the two halves are held
// against each other — the one disagreement that matters (an export whose
// snapshot a restore cannot use) is invisible to any test that fakes either
// side.

// exportedArchiveName runs an export to completion and returns the container's
// file name, which is what the restore request names.
func exportedArchiveName(t *testing.T, e *workspaceEnv) string {
	t.Helper()
	e.seedWorkspace(t)
	resp, raw := e.postWorkspace(t, e.auth.Credential(), "export", map[string]any{"passphrase": testExportPassphrase})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("export = %d, want 202 (body %s)", resp.StatusCode, raw)
	}
	job := decodeJob(t, raw)
	e.runJobs()
	done := e.polledJob(t, e.auth.Credential(), job.ID)
	if done.State != "succeeded" {
		t.Fatalf("export job state = %q, want succeeded (%+v)", done.State, done.Targets)
	}
	return "workspace-" + job.ID + ".waiveo-archive"
}

// TestRestoreStagesTheArchivedStoreWithoutTouchingTheLiveOne is the whole
// property of the offline swap, end to end.
func TestRestoreStagesTheArchivedStoreWithoutTouchingTheLiveOne(t *testing.T) {
	e := newWorkspaceEnv(t)
	name := exportedArchiveName(t, e)

	// A live store on disk, so "untouched" is a claim with something behind it.
	const liveBytes = "the store that is serving"
	if err := os.WriteFile(e.storePath, []byte(liveBytes), 0o600); err != nil {
		t.Fatalf("seed live store: %v", err)
	}

	resp, raw := e.postWorkspace(t, e.auth.Credential(), "restore",
		map[string]any{"archive": name, "passphrase": testExportPassphrase})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restore = %d, want 202 (body %s)", resp.StatusCode, raw)
	}
	job := decodeJob(t, raw)
	e.runJobs()
	done := e.polledJob(t, e.auth.Credential(), job.ID)
	if done.State != "succeeded" {
		t.Fatalf("restore job state = %q, want succeeded (%+v)", done.State, done.Targets)
	}

	// The live store is byte-for-byte what it was. This is ARC-107 without a
	// rollback step: the restore succeeded and still replaced nothing.
	got, err := os.ReadFile(e.storePath)
	if err != nil {
		t.Fatalf("read live store: %v", err)
	}
	if string(got) != liveBytes {
		t.Fatalf("a SUCCEEDED restore changed the live store to %q — the swap must happen at the next boot, not while "+
			"this process is serving from it", got)
	}

	if !restoreswap.Pending(e.storePath) {
		t.Fatal("a succeeded restore staged nothing — the next boot would adopt nothing and the operator's restore " +
			"would silently not have happened")
	}

	// The staged store is the archive's own snapshot, not something rebuilt.
	_, staged, _, _ := restoreswap.Paths(e.storePath)
	stagedBytes, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read staged store: %v", err)
	}
	_, entries := e.openArchive(t, jobIDOfExport(t, name))
	var want []byte
	for _, en := range entries {
		if en.Name == archive.SnapshotEntryName {
			want = en.Body
		}
	}
	if want == nil {
		t.Fatal("the exported container carries no snapshot entry, so this comparison proves nothing")
	}
	if !bytes.Equal(stagedBytes, want) {
		t.Errorf("the staged store is %d bytes and the archive's snapshot is %d — the restore staged something other "+
			"than what the container carried", len(stagedBytes), len(want))
	}

	// And the swap really completes: adopting puts the archived store live and
	// keeps what was there.
	adopted, err := restoreswap.Adopt(e.storePath)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !adopted {
		t.Fatal("the staged restore was not adopted at the simulated next boot")
	}
	live, err := os.ReadFile(e.storePath)
	if err != nil {
		t.Fatalf("read adopted store: %v", err)
	}
	if !bytes.Equal(live, want) {
		t.Error("after adoption the live store is not the archive's snapshot")
	}
	_, _, previous, _ := restoreswap.Paths(e.storePath)
	if prev, err := os.ReadFile(previous); err != nil || string(prev) != liveBytes {
		t.Errorf("the pre-restore copy is %q (err %v), want the store that was serving", prev, err)
	}
}

// jobIDOfExport recovers an export job's id from the container name the export
// wrote, so the assertion above can reopen that exact container.
func jobIDOfExport(t *testing.T, name string) string {
	t.Helper()
	const prefix, suffix = "workspace-", ".waiveo-archive"
	if len(name) <= len(prefix)+len(suffix) {
		t.Fatalf("unexpected container name %q", name)
	}
	return name[len(prefix) : len(name)-len(suffix)]
}

// TestARestoreOfAnAbsentArchiveFailsTheTargetAndStagesNothing: the job reports
// the failure, and the live store is still untouched.
func TestARestoreOfAnAbsentArchiveFailsTheTargetAndStagesNothing(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	const liveBytes = "the store that is serving"
	if err := os.WriteFile(e.storePath, []byte(liveBytes), 0o600); err != nil {
		t.Fatalf("seed live store: %v", err)
	}

	resp, raw := e.postWorkspace(t, e.auth.Credential(), "restore",
		map[string]any{"archive": "no-such-container.waiveo-archive", "passphrase": testExportPassphrase})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restore = %d, want 202 (body %s)", resp.StatusCode, raw)
	}
	job := decodeJob(t, raw)
	e.runJobs()
	done := e.polledJob(t, e.auth.Credential(), job.ID)
	if done.State != "failed" {
		t.Fatalf("restore job state = %q, want failed", done.State)
	}
	if restoreswap.Pending(e.storePath) {
		t.Error("a FAILED restore left a pending swap — the next boot would adopt a store no restore produced")
	}
	if got, _ := os.ReadFile(e.storePath); string(got) != liveBytes {
		t.Errorf("a failed restore changed the live store to %q", got)
	}
}

// TestARestoreWithTheWrongPassphraseFailsBeforeStaging: the container never
// opens, so nothing reaches the staging step.
func TestARestoreWithTheWrongPassphraseFailsBeforeStaging(t *testing.T) {
	e := newWorkspaceEnv(t)
	name := exportedArchiveName(t, e)

	resp, raw := e.postWorkspace(t, e.auth.Credential(), "restore",
		map[string]any{"archive": name, "passphrase": "not-the-export-passphrase"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restore = %d, want 202 (body %s)", resp.StatusCode, raw)
	}
	job := decodeJob(t, raw)
	e.runJobs()
	done := e.polledJob(t, e.auth.Credential(), job.ID)
	if done.State != "failed" {
		t.Fatalf("restore job state = %q, want failed", done.State)
	}
	if restoreswap.Pending(e.storePath) {
		t.Error("a restore whose container never decrypted still staged something")
	}
}

// TestARestoreCannotNameAPathOutsideTheArchiveDirectory.
//
// The archive name reaches os.Open. A caller able to send a path would be
// choosing which file this process opens and decrypts — a file-read primitive
// wearing a restore's clothes — so the refusal is at the request, before a Job
// is spent, and it is a 422 rather than the 404 a missing container gets,
// because the request is malformed rather than pointing at something absent.
func TestARestoreCannotNameAPathOutsideTheArchiveDirectory(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	for _, name := range []string{
		"../../etc/passwd",
		"sub/dir/container.waiveo-archive",
		"/etc/passwd",
		"",
		"   ",
	} {
		t.Run(name, func(t *testing.T) {
			resp, raw := e.postWorkspace(t, e.auth.Credential(), "restore",
				map[string]any{"archive": name, "passphrase": testExportPassphrase})
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("restore with archive %q = %d, want 422 (body %s)", name, resp.StatusCode, raw)
			}
		})
	}
}
