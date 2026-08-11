package api

// What the audit trail CALLS a backup deletion.
//
// This exists because the generic reading got it dangerously wrong. A
// three-segment DELETE that names no enumerated shape falls to
// unmountedAuditRoute's default arm, which spells the act `<family>.<verb>` —
// and for `DELETE /workspace/archives/{name}` that is `workspace.delete`, the
// identical action name the DATA-SUBJECT DESTRUCTION records
// (`POST /workspace/delete`, which erases the whole deployment).
//
// One is an operator reclaiming a few megabytes. The other is the end of the
// workspace. An audit trail that spells them the same way cannot answer the only
// question anybody asks it afterwards, and nothing about the route's shape would
// have made that visible — which is why it is asserted rather than reasoned
// about.

import (
	"net/http"
	"testing"
)

func TestABackupDeletionIsNotSpelledLikeErasingTheWorkspace(t *testing.T) {
	srv := &server{}

	req := func(method, path string) *http.Request {
		r, err := http.NewRequest(method, "https://box.invalid"+path, nil)
		if err != nil {
			t.Fatalf("build %s %s: %v", method, path, err)
		}
		return r
	}

	container := "workspace-01J8ZJOB000000000000000001.waiveo-archive"
	got, audited := srv.auditRouteOf(req(http.MethodDelete, apiPrefix+"/workspace/archives/"+container))
	if !audited {
		t.Fatal("deleting a backup is not audited at all — the one irreversible act on this family")
	}
	if got.action == "workspace.delete" {
		t.Fatal("a backup deletion records `workspace.delete`, which is what erasing the ENTIRE deployment " +
			"records: the trail cannot tell a few reclaimed megabytes from the end of the workspace")
	}
	if got.action != "workspace.archive-delete" {
		t.Errorf("action = %q, want workspace.archive-delete", got.action)
	}
	// The subject is the container, so the record names WHICH backup went.
	if got.id != container {
		t.Errorf("id = %q, want the container's name %q", got.id, container)
	}

	// And the destruction it must not be confused with still records its own act.
	erase, audited := srv.auditRouteOf(req(http.MethodPost, apiPrefix+"/workspace/delete"))
	if !audited || erase.action != "workspace.delete" {
		t.Errorf("the data-subject delete records %q (audited=%v), want workspace.delete", erase.action, audited)
	}
	if erase.action == got.action {
		t.Error("the two acts still share one action name")
	}
}
