package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// workspacearchives.go completes the operator's BACKUP loop (parity row 7.5).
//
// The export operation already produced a real archive/1 container and the
// restore operation already consumed one. What sat between them was a hole an
// operator could not cross without a shell:
//
//   - Nothing LISTED the containers. The restore names an archive by file name,
//     and the only way to learn a name was to know that an export writes
//     `workspace-<job id>.waiveo-archive` and to remember the job id. An
//     operation whose one required argument cannot be discovered through the
//     API is an operation the console cannot offer.
//   - Nothing let the bytes LEAVE THE BOX. A backup that only ever exists on the
//     machine it backs up is not a backup — it protects against a bad edit and
//     against nothing else. The single failure an operator buys a backup for is
//     losing the box.
//
// So: two reads.
//
//	GET /api/v1/workspace/archives          the containers on this box
//	GET /api/v1/workspace/archives/{name}   the bytes of one, as a download
//
// Both are `owner`, exactly as the export and delete are and for the same
// reason (workspace.go's header): the subject is the whole workspace, there is
// no scope node to narrow to, and these particular bytes are the entire
// workspace in one file.
//
// # Downloading an archive is not a confidentiality decision this route makes
//
// A container is encrypted under the operator's own export passphrase (ARC-010)
// and signed by the workspace key. Serving its bytes to the workspace owner
// discloses nothing they did not already choose to create; without the
// passphrase the bytes are opaque even to this process. What the route must not
// do is let the NAME choose a file — see archiveNameOf.

// archiveSuffix is the file extension an archive/1 container is written under
// (workspacerun.go's `workspace-<job id>.waiveo-archive`). The listing filters
// on it so a scratch snapshot, a partial write, or anything else an operator
// dropped in the directory is not offered as a restorable backup.
const archiveSuffix = ".waiveo-archive"

// workspaceArchiveEntry is one container on the wire (openapi WorkspaceArchive).
type workspaceArchiveEntry struct {
	// Name is the file name, which is also the value POST /workspace/restore
	// takes — the whole point of the listing is that a console can offer the
	// restore's one required argument as a choice rather than as free text.
	Name        string `json:"name"`
	SizeBytes   int64  `json:"size_bytes"`
	CreatedAtMs int64  `json:"created_at_ms"`
	// DownloadPath is the path this container's bytes are served from,
	// published rather than left for a client to compose. A client that built
	// the URL itself would be re-encoding a file name into a path, which is the
	// one thing about this family that has a traversal question attached to it.
	DownloadPath string `json:"download_path"`
}

// workspaceArchiveList is the response (openapi WorkspaceArchiveList).
type workspaceArchiveList struct {
	Items []workspaceArchiveEntry `json:"items"`
	// Directory is where these live on the box. Published because a restore that
	// names a container an operator placed there by hand (scp'd back from
	// off-box storage, which is the actual disaster-recovery path) needs the
	// operator to know where "there" is.
	Directory string `json:"directory"`
}

// mountWorkspaceArchives registers both reads.
func (srv *server) mountWorkspaceArchives(rt *router) {
	rt.HandleFunc("GET "+apiPrefix+"/workspace/archives", srv.listWorkspaceArchives)
	rt.HandleFunc("GET "+apiPrefix+"/workspace/archives/{name}", srv.downloadWorkspaceArchive)
}

// listWorkspaceArchives handles GET /api/v1/workspace/archives.
func (srv *server) listWorkspaceArchives(w http.ResponseWriter, r *http.Request) {
	if _, ok := srv.authorizeWorkspaceOwner(w, r); !ok {
		return
	}
	if srv.workspaceArchive == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
			"This deployment is not configured with a workspace archive destination.")
		return
	}
	dir := srv.workspaceArchive.Dir
	out := workspaceArchiveList{Items: []workspaceArchiveEntry{}, Directory: dir}

	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// A deployment that has never exported has no directory yet. That is
			// an empty list, not a failure: the honest answer is "no backups",
			// and a 500 here would make a fresh box look broken on the page whose
			// job is to tell an operator whether their box is healthy.
			writeJSONValue(w, http.StatusOK, out)
			return
		}
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), archiveSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// Vanished between the read and the stat (a sweep, an operator with a
			// shell). Skipped rather than reported: it is genuinely not there.
			continue
		}
		out.Items = append(out.Items, workspaceArchiveEntry{
			Name:         e.Name(),
			SizeBytes:    info.Size(),
			CreatedAtMs:  info.ModTime().UnixMilli(),
			DownloadPath: apiPrefix + "/workspace/archives/" + e.Name(),
		})
	}
	// Newest first: the backup an operator wants is almost always the last one,
	// and a directory listing's own order is the filesystem's, not a decision.
	sort.Slice(out.Items, func(i, j int) bool {
		if out.Items[i].CreatedAtMs != out.Items[j].CreatedAtMs {
			return out.Items[i].CreatedAtMs > out.Items[j].CreatedAtMs
		}
		return out.Items[i].Name < out.Items[j].Name
	})
	writeJSONValue(w, http.StatusOK, out)
}

// downloadWorkspaceArchive handles GET /api/v1/workspace/archives/{name}: the
// container's bytes, so a backup can leave the box it backs up.
func (srv *server) downloadWorkspaceArchive(w http.ResponseWriter, r *http.Request) {
	if _, ok := srv.authorizeWorkspaceOwner(w, r); !ok {
		return
	}
	if srv.workspaceArchive == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
			"This deployment is not configured with a workspace archive destination.")
		return
	}
	name, ok := archiveNameOf(r.PathValue("name"))
	if !ok {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found",
			"No archive of that name is present in this deployment's archive directory.")
		return
	}
	f, err := os.Open(filepath.Join(srv.workspaceArchive.Dir, name))
	if err != nil {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found",
			"No archive of that name is present in this deployment's archive directory.")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found",
			"No archive of that name is present in this deployment's archive directory.")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	// The browser saves rather than renders. The name is already known safe
	// (archiveNameOf), so it can be quoted straight into the header without a
	// second encoding scheme that would have to be got right too.
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	// A backup is the one response on this surface that must never be served
	// from an intermediary's cache: it is the entire workspace, and a stale copy
	// restored later is a silent data-loss event.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// A copy rather than a read-all: a container is as large as the workspace,
	// and buffering it doubles this process's peak memory at exactly the moment
	// an operator is backing up a box they are worried about.
	_, _ = io.Copy(w, f)
}

// archiveNameOf validates a path-supplied archive name and reports whether it
// may be opened.
//
// The rule is the restore's rule, and it is deliberately the SAME rule stated in
// the same shape (workspacerestore.go): the value must be a bare file name, not
// a path. A caller who could send `..%2f..%2fetc%2fshadow` would be choosing
// which file this process reads and streams back — a file-read primitive wearing
// a download's clothes.
//
// It additionally requires the archive suffix, which the restore does not: the
// restore has to tolerate an operator naming a container they copied back from
// off-box storage under whatever name it arrived with, whereas a download can
// only ever be offered for something this listing published, and the listing
// only publishes the suffix. Narrower is correct where it costs nothing.
func archiveNameOf(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	switch {
	case name == "",
		name != filepath.Base(name),
		strings.Contains(name, "/"),
		strings.Contains(name, `\`),
		strings.Contains(name, ".."),
		strings.HasPrefix(name, "."),
		!strings.HasSuffix(name, archiveSuffix):
		return "", false
	}
	return name, true
}
