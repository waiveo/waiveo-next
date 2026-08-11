package api

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
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
//   - Nothing RECLAIMED one. Containers accumulated forever, and the box's own
//     disk grading told the operator to clear them: `GET /system-health` answers
//     `low` with "prune old images and archives" (diagnostics.go), advice the
//     API gave no operation for and the console gave no control for. A full disk
//     is not a hypothetical on this appliance — it is the state its predecessor
//     died in, twice — so a surface that names the remedy and withholds it is the
//     worst of the two possible failures: the operator does the diagnosis and
//     then has nowhere to go.
//
// So: two reads and one destruction.
//
//	GET    /api/v1/workspace/archives          the containers on this box
//	GET    /api/v1/workspace/archives/{name}   the bytes of one, as a download
//	DELETE /api/v1/workspace/archives/{name}   reclaim one, irreversibly
//
// All three are `owner`, exactly as the export and delete are and for the same
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
//
// # RETENTION: this file deliberately implements NO automatic sweep
//
// The obvious companion to a delete is a keep-N / keep-for-D policy swept on a
// timer, and the feeder already runs a retention ticker three other sweeps ride
// (cmd/waiveo-feeder/main.go), so it would cost almost nothing to add a fourth.
// It is not here, and the reason is a fact about this deployment rather than a
// deferral:
//
//   - NOTHING PRODUCES AN ARCHIVE UNATTENDED. There is no backup scheduler, no
//     cron, and no `rules/1` action that exports — the only caller of POST
//     /workspace/export in the entire tree is the console's Export button. Every
//     container on the box is therefore a deliberate human act, one at a time.
//     A retention sweep exists to bound growth that happens WITHOUT a decision;
//     against a producer that is itself a decision, it has nothing to bound.
//   - AND IT WOULD BE A DATA-LOSS MACHINE. A container is encrypted under a
//     passphrase this box does not keep, so a deleted archive is unrecoverable
//     by anyone including its author. A sweep with a keep-N default would one
//     day silently unlink the archive whose passphrase the operator still has,
//     to reclaim space on a box that was not short of any — trading a bounded,
//     visible problem (a disk filling, which the health page grades and now
//     offers a control for) for an unbounded, invisible one.
//
// A policy belongs with the scheduled-backup engine that would create the churn
// it bounds, and it should be authored there, together, so "keep 7 dailies" is
// one decision rather than a sweep and a schedule agreeing by luck. What this
// wave owes the operator instead is the thing the box already told them to do:
// see one container, see what it costs, and delete it on purpose.
//
// The health page carries the other half — storageHealth() now names how much
// the archive directory is actually holding, so the advice points at a number
// and a place rather than at a category.

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
	// ETag is the `If-Match` value the delete requires, quoted (API-020's shape).
	// See archiveTagOf for what it is derived from and why a file needs one.
	ETag string `json:"etag"`
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

// mountWorkspaceArchives registers the two reads and the destruction.
func (srv *server) mountWorkspaceArchives(rt *router) {
	rt.HandleFunc("GET "+apiPrefix+"/workspace/archives", srv.listWorkspaceArchives)
	rt.HandleFunc("GET "+apiPrefix+"/workspace/archives/{name}", srv.downloadWorkspaceArchive)
	rt.HandleFunc("DELETE "+apiPrefix+"/workspace/archives/{name}", srv.deleteWorkspaceArchive)
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
			ETag:         apihttp.TagETag(archiveTagOf(info)),
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
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", archiveNotFoundDetail)
		return
	}
	f, err := os.Open(filepath.Join(srv.workspaceArchive.Dir, name))
	if err != nil {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", archiveNotFoundDetail)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", archiveNotFoundDetail)
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

// deleteWorkspaceArchive handles DELETE /api/v1/workspace/archives/{name}:
// reclaim one container's bytes, irreversibly.
//
// # The order of the checks IS the safety, and it is the surface's own order
//
// The resource delete every CRUD family shares (api.go's resource.delete) runs:
// not-found → unreadable → unwritable → If-Match precondition → a per-kind
// refusal no retry can clear. This follows it exactly, including the part that
// looks backwards:
//
//	authorize → 404 → If-Match → ARCHIVE_IN_USE → unlink
//
// ARCHIVE_IN_USE sits BEHIND the precondition even though a caller who omitted
// If-Match will be told to retry with one and then meet a second refusal. That
// costs one round trip, and it is what api.go's own note concluded for the same
// question: API-022/023 are categorical, so a route that answered its own
// refusal before the precondition would be the ONE resource on this surface
// whose 428 rule differs from every other, which a strict conformance sweep is
// entitled to fail.
//
// # Why the whole thing runs under one lock
//
// Everything from the stat to the unlink happens inside WorkspaceArchive.dir.
// Without it the check and the act are two moments an accepted job can slip
// between: a restore that claimed this container a microsecond after the busy
// check reads a file that is about to vanish, and an export that created it a
// microsecond after the stat gets its half-written container unlinked while it
// is still streaming into it. Both are races the operator cannot see and the
// logs cannot explain. Holding the lock across the sequence makes the answer
// deterministic: either the job has the name and the delete refuses, or the
// delete unlinked first and the job fails on its own `open` with a message about
// a container that is genuinely not there.
//
// The lock is held for a stat and an unlink and nothing else — no HTTP write, no
// container read — so it cannot be held across a slow client.
func (srv *server) deleteWorkspaceArchive(w http.ResponseWriter, r *http.Request) {
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
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", archiveNotFoundDetail)
		return
	}

	ifMatch, present := r.Header["If-Match"]
	verdict := srv.workspaceArchive.deleteArchive(name, func(etag string) apihttp.ConcurrencyOutcome {
		return apihttp.CheckIfMatchTag(headerValue(ifMatch), present, etag)
	})

	switch {
	case !verdict.Found:
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", archiveNotFoundDetail)
	case !verdict.Precondition.OK:
		// The container is untouched. Said explicitly rather than left to be
		// inferred from the status: this is a DESTRUCTIVE operation, and a
		// refusal an operator cannot tell from a partial success is the one
		// thing an irreversible control must never produce.
		writeProblem(w, r, verdict.Precondition.Status, verdict.Precondition.Code, verdict.Precondition.Title,
			verdict.Precondition.Detail+" The backup was not deleted.")
	case verdict.BusyWhy != "":
		writeProblem(w, r, http.StatusConflict, "ARCHIVE_IN_USE", "Conflict",
			verdict.BusyWhy+" The backup was not deleted; try again once that job reaches a terminal state.")
	case verdict.Err != nil:
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// archiveNotFoundDetail is the ONE `detail` every archive 404 carries, whether
// the name was hostile, unsuffixed, or simply absent. One sentence, because
// three would let a caller tell "there is no such file" from "that file is not
// a container" — a directory-probing oracle over a directory that transiently
// holds a cleartext snapshot of the whole workspace.
const archiveNotFoundDetail = "No archive of that name is present in this deployment's archive directory."

// archiveDeletion is deleteArchive's verdict — one value the handler switches
// on, so the lock's critical section holds no branching about HTTP.
type archiveDeletion struct {
	// Found reports whether a container of that name was there at all.
	Found bool
	// Precondition is the If-Match verdict, evaluated against the container's
	// tag as it was under the lock. Zero (OK false, Status 0) when Found is
	// false, which the handler's switch reaches only after the not-found arm.
	Precondition apihttp.ConcurrencyOutcome
	// BusyWhy is the operator-facing sentence naming the accepted job holding
	// this container, or empty.
	BusyWhy string
	// Err is an unlink that failed for a reason other than absence.
	Err error
}

// deleteArchive resolves, precondition-checks and unlinks one container with the
// archive directory's lock held across all of it.
//
// precondition is called with the container's CURRENT entity-tag, under the
// lock, and must be pure: it computes a verdict and writes nothing. That is what
// lets the check be made against the same bytes the unlink then destroys,
// without the lock ever spanning a response write.
func (a *WorkspaceArchive) deleteArchive(name string, precondition func(etag string) apihttp.ConcurrencyOutcome) archiveDeletion {
	a.dir.Lock()
	defer a.dir.Unlock()

	path := filepath.Join(a.Dir, name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return archiveDeletion{}
	}
	out := archiveDeletion{Found: true, Precondition: precondition(archiveTagOf(info))}
	if !out.Precondition.OK {
		return out
	}
	if claim, held := a.busy[name]; held {
		out.BusyWhy = claim.why
		return out
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		out.Err = err
	}
	return out
}

// archiveFootprint reports how many containers are in dir and what they occupy
// — the number the disk-headroom advice needs to stop being a category and start
// being an amount (diagnostics.go).
//
// It counts exactly what the LISTING publishes, suffix filter and all, rather
// than the directory's whole size. Two different answers to "how much are the
// backups using" — one on the health page, one implied by the Backup page — is
// how an operator deletes every container they have and finds the number
// unmoved, because the difference was a scratch snapshot nothing offered them.
//
// A directory that cannot be read is zero, not an error: this feeds one advisory
// sentence appended to a disk grade that has already been computed from a real
// measurement, and failing the health read because a subdirectory was
// unreadable would take away the page an operator opened to find that out.
func archiveFootprint(dir string) (count int, bytes int64) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), archiveSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		count++
		bytes += info.Size()
	}
	return count, bytes
}

// archiveTagOf derives a container's entity-tag from the bytes' own identity:
// its size and its last-modified instant, the two facts a filesystem keeps about
// a file that change when its content does.
//
// A container has no `revision` to derive a validator from the way every other
// resource on this surface does (apihttp.ETag) — it is a file, written once and
// never updated. That is precisely why the tag is derived rather than skipped:
// the disaster-recovery path is an operator copying a container BACK into this
// directory, possibly under a name the console has already listed, and a delete
// with no precondition would then unlink the container that REPLACED the one the
// operator was looking at when they pressed the button.
//
// Not a content hash: a container is as large as the workspace, and hashing
// gigabytes to answer "is this still the file you saw" would make listing the
// backups cost more than taking one. (size, mtime) is the same validator every
// static-file server has used for thirty years, and it is strong enough for the
// question actually being asked — did these bytes get replaced between the read
// and the write — because a replacement writes a new mtime.
func archiveTagOf(info os.FileInfo) string {
	return strconv.FormatInt(info.Size(), 10) + "-" + strconv.FormatInt(info.ModTime().UnixMilli(), 10)
}

// archiveClaim is one container's in-use registration: how many accepted jobs
// hold it, and the sentence a delete refuses with.
//
// Counted rather than a bare flag because two restores of the same container are
// legal — the operation is idempotent and an operator may simply press twice —
// and a flag would have the first job's completion release a name the second is
// still reading.
type archiveClaim struct {
	holders int
	why     string
}

// claimArchive registers name as in use by an accepted job and returns the
// release, which the job's execution defers.
//
// # Why an in-memory registry is the RIGHT durability here, not a shortcut
//
// The claim describes a fact about THIS PROCESS: a goroutine is part-way through
// reading or writing this file. That fact cannot survive a restart, because the
// goroutine cannot — and the platform already agrees: an export or a restore
// stranded `running` by a crash is reconciled to a terminal `failed` on the next
// boot rather than resumed (jobrun.go), because neither persists a re-appliable
// operation. So a claim persisted to the store would outlive the only thing it
// describes and would refuse deletes forever for a job that is already recorded
// as failed — a lock nothing can release, on the operation whose entire purpose
// is freeing a disk that is filling up.
//
// The one leak this admits is stated rather than hidden: a claim taken at
// acceptance is released by the job's execution, and JobRunner.submit drops work
// accepted after Shutdown. That claim is never released — during a shutdown, in
// a process that is exiting, taking the map with it.
func (a *WorkspaceArchive) claimArchive(name, why string) func() {
	a.dir.Lock()
	defer a.dir.Unlock()
	if a.busy == nil {
		a.busy = map[string]*archiveClaim{}
	}
	if c, held := a.busy[name]; held {
		c.holders++
	} else {
		a.busy[name] = &archiveClaim{holders: 1, why: why}
	}
	return func() {
		a.dir.Lock()
		defer a.dir.Unlock()
		c, held := a.busy[name]
		if !held {
			return
		}
		if c.holders--; c.holders <= 0 {
			delete(a.busy, name)
		}
	}
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
