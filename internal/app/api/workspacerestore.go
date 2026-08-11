package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/restoreswap"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/archive"
	"github.com/maaxton/waiveo-next/internal/shared/apijob"
)

// The restore half of archive/1's install path, applied through the offline
// swap (internal/app/restoreswap).
//
// # What this job does and deliberately does not do
//
// It verifies a container, preflights it against THIS destination, and stages
// the restored relational store beside the live one. It does not replace
// anything. The swap happens at the next boot, which is what makes ARC-107's
// rollback guarantee hold without a rollback step: a restore that fails at any
// point here has changed nothing an operator can observe.
//
// A restore therefore completes in two parts — an accepted job that stages, and
// a restart that adopts — and the job's success means "staged and ready", never
// "your workspace is now the archived one". Reporting otherwise would tell an
// operator their data had moved while the process was still serving the old
// store from memory.
//
// # ARC-105 holds by construction here rather than by a step
//
// "Restore MUST NOT carry forward any credential, session, or cryptographic
// identity the source workspace held live at export time." The container's
// snapshot is the APP store — scope nodes and scheduling rows. Credentials,
// sessions and grants live in a separate auth store this job never touches, and
// the relay's own enrollment identity is not in either. So there is no
// filtering step that could be forgotten: the material ARC-105 forbids carrying
// forward is not in what is carried.

// workspaceRestoreRequest is POST /workspace/restore's body.
//
// The archive is named rather than uploaded. An upload would mean holding a
// multi-gigabyte container in the request path before anything about it has
// been verified, and the operator performing a restore is one who can already
// place a file in the archive directory — it is where their exports land.
type workspaceRestoreRequest struct {
	Archive    string  `json:"archive"`
	Passphrase *string `json:"passphrase"`
}

// restoreWorkspace is POST /api/v1/workspace/restore.
func (srv *server) restoreWorkspace(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if srv.undeclaredMemberRejected(w, r, "WorkspaceRestoreRequest", body) {
		return
	}
	srv.idempotent(w, r, body, func(w http.ResponseWriter) { srv.restoreWorkspaceExec(w, r, body) })
}

func (srv *server) restoreWorkspaceExec(w http.ResponseWriter, r *http.Request, body []byte) {
	workspaceID, ok := srv.authorizeWorkspaceOwner(w, r)
	if !ok {
		return
	}

	var req workspaceRestoreRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", "The request body could not be parsed.")
		return
	}
	if req.Passphrase == nil || strings.TrimSpace(*req.Passphrase) == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", "`passphrase` is required.")
		return
	}
	// The archive name is a bare filename, never a path. A caller that could
	// send `../../etc/something` would be choosing which file this process
	// opens and decrypts, which is a file-read primitive wearing a restore's
	// clothes — and the check is here rather than at the join, because a join
	// that looks safe is exactly how this class survives review.
	name := strings.TrimSpace(req.Archive)
	if name == "" || name != filepath.Base(name) || strings.Contains(name, string(filepath.Separator)) {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"`archive` must be the name of a container in this deployment's archive directory, not a path.")
		return
	}
	if srv.workspaceArchive == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
			"This deployment is not configured with a workspace archive destination.")
		return
	}
	if srv.storePath == "" {
		// Nothing to stage beside. Refusing up front rather than accepting a Job
		// that already cannot finish, for the same reason the export refuses
		// without an archive destination.
		writeProblem(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
			"This deployment cannot stage a restore because it is not configured with a store path.")
		return
	}

	job, ok := srv.acceptWorkspaceJob(w, r, workspaceID)
	if !ok {
		return
	}
	// This container is now off-limits to the delete for as long as the restore
	// is accepted (workspacearchives.go's claimArchive).
	//
	// The claim is taken at ACCEPTANCE rather than when stageRestore opens the
	// file, and the gap between the two is the whole reason. Unlinking a file
	// somebody holds open is safe on this platform, so a delete landing AFTER the
	// open is invisible and the restore finishes; one landing BEFORE it fails the
	// restore with "no archive of that name", a sentence about a container the
	// operator did have and did choose to delete, in a job they may not connect
	// to the button they pressed. Which of the two happens is a scheduler race
	// nobody can observe. Refusing across the whole accepted lifetime replaces a
	// coin-flip with one answer.
	release := srv.workspaceArchive.claimArchive(name,
		"A restore accepted as job "+job.ID+" is still reading this container.")

	writeJSONValue(w, http.StatusAccepted, job.Resource())

	passphrase := *req.Passphrase
	// Submitted through the same runner the export uses, so a restore is
	// resumable across a restart on the same terms (API-116) rather than being
	// a goroutine whose loss nothing records.
	srv.jobs.submit(func(ctx context.Context) {
		defer release()
		srv.runWorkspaceRestore(ctx, job.ID, workspaceID, name, passphrase)
	})
}

// runWorkspaceRestore executes one accepted restore Job.
func (srv *server) runWorkspaceRestore(ctx context.Context, jobID, workspaceID, name, passphrase string) {
	if _, err := srv.store.AdvanceJob(ctx, jobID, func(j *apijob.Job) error {
		return j.StartTarget(workspaceID)
	}); err != nil {
		return
	}
	code, detail := srv.stageRestore(ctx, name, passphrase)
	_, _ = srv.store.AdvanceJob(ctx, jobID, func(j *apijob.Job) error {
		if code != "" {
			return j.FailTarget(workspaceID, code, detail)
		}
		return j.SucceedTarget(workspaceID)
	})
}

// stageRestore verifies the container, preflights it, and stages the restored
// store. It returns an api/1 error code and detail, or two empty strings.
//
// # Why an archive/1 code never becomes the target's code
//
// API-115 requires a Job's per-target failure to be typed from api/1's OWN
// registry, "never a parallel vocabulary" — and archive/1 publishes its own
// registry, whose codes (DECRYPT_FAILED, EPOCH_TOO_NEW, ASSET_UNAVAILABLE, …)
// are not in it. Returning one directly is exactly the parallel vocabulary that
// rule forbids, and apijob refuses it rather than storing it, which is how this
// was caught rather than shipped.
//
// So every archive-side refusal becomes VALIDATION_FAILED — the request named a
// container this destination cannot apply — and the archive/1 code is carried
// at the FRONT of the detail, where an operator and a support transcript both
// still see exactly which check refused. Nothing is lost but the ability to
// branch on it programmatically, and a client that could branch on an
// unpublished code was never conformant anyway.
func (srv *server) stageRestore(ctx context.Context, name, passphrase string) (string, string) {
	cfg := srv.workspaceArchive
	f, err := os.Open(filepath.Join(cfg.Dir, name))
	if err != nil {
		return "NOT_FOUND", "No archive of that name is present in this deployment's archive directory."
	}
	defer f.Close()

	// Open performs archive/1's whole read-side check order — format, major
	// version, header signature, per-frame authentication, frame-sequence
	// completeness, recomputed body digest, manifest shape, and every embedded
	// asset's hash. A failure here is ARC-082's "before any of it is applied",
	// and nothing has been staged yet, so there is nothing to undo.
	manifest, entries, err := archive.Open(f, passphrase, cfg.Key.Public())
	if err != nil {
		var coded *archive.Error
		if errors.As(err, &coded) {
			return "VALIDATION_FAILED", coded.ErrCode + ": " + coded.Detail
		}
		return "VALIDATION_FAILED", "The archive could not be opened."
	}

	result := archive.Preflight(manifest, srv.restoreDestination())
	if !result.OK() {
		// Every fatal refusal, not the first: ARC's own preflight populates them
		// all from one attempt precisely so an operator is not made to discover
		// them one restore at a time, and collapsing that here would spend that
		// design.
		reasons := make([]string, 0, len(result.Fatal))
		for _, e := range result.Fatal {
			reasons = append(reasons, e.ErrCode+": "+e.Detail)
		}
		return "VALIDATION_FAILED", strings.Join(reasons, "; ")
	}

	snapshot, ok := snapshotEntry(entries)
	if !ok {
		return "VALIDATION_FAILED", "The archive carries no workspace snapshot."
	}

	// The EMBEDDED ASSETS go back into the content origin, before the snapshot
	// is staged.
	//
	// This was the hole in the restore. The snapshot carries the authored rows —
	// casts, playlists, screens — and every image layer in them names an
	// `asset_ref`. On a FRESH box (the whole point of a restore: new hardware,
	// a wiped appliance, a migration) those bytes are not present, so a restore
	// that staged only the snapshot brought back a workspace whose every slide
	// referenced an image the origin had never heard of. Nothing failed: the
	// restore reported success, the console listed the casts, and the screens
	// rendered blanks. That is exactly the shape this codebase keeps having to
	// remove — a surface that accepts work it does not perform.
	//
	// Written NOW rather than staged for the swap, because the content origin is
	// content-addressed and append-only: adding bytes under their own hash
	// cannot disturb the live workspace (nothing in it names those refs) and is
	// idempotent if the restore is retried. It is also safe against the content
	// sweep, whose first guard is a minimum asset age measured from arrival
	// (internal/feeder/contentgc) — freshly written bytes are not reclaimable
	// for days, far longer than the reboot this restore is waiting for.
	//
	// Every asset's hash was already verified against its `asset_ref` by
	// archive.Open, and origin.Add re-derives the ref from the bytes, so a
	// mismatch is impossible to store rather than merely unlikely.
	restored, code, detail := srv.restoreAssets(entries)
	if code != "" {
		return code, detail
	}

	if err := restoreswap.Stage(srv.storePath, func(staged string) error {
		return os.WriteFile(staged, snapshot, 0o600)
	}); err != nil {
		return "INTERNAL", "The restored store could not be staged."
	}
	if restored > 0 {
		log.Printf("waiveo-feeder: restore staged %d byte snapshot and returned %d asset(s) to the content origin",
			len(snapshot), restored)
	}
	return "", ""
}

// restoreAssets writes every embedded asset entry into the content origin,
// returning how many were written.
//
// A destination with NO content origin is refused rather than skipped. Skipping
// would produce the exact silent half-restore this exists to end: a staged
// store full of rows naming bytes that will never arrive.
func (srv *server) restoreAssets(entries []archive.Entry) (int, string, string) {
	assets := 0
	for _, e := range entries {
		ref, ok := archive.AssetRefFromEntryName(e.Name)
		if !ok {
			continue
		}
		assets++
		if srv.content == nil {
			return 0, "UNAVAILABLE", "This archive carries assets but this deployment has no content origin to restore them into; nothing was staged."
		}
		got, err := srv.content.Add(e.Body)
		if err != nil {
			return 0, "INTERNAL", "An asset in this archive could not be written to the content origin; nothing was staged."
		}
		if got != ref {
			// Unreachable via archive.Open, which verifies each embedded asset's
			// hash against its ref before returning it. Checked anyway, because
			// the cost of being wrong is a workspace whose rows point at bytes
			// that are not the bytes they name.
			return 0, "INTERNAL", "An asset in this archive did not hash to the reference it was carried under; nothing was staged."
		}
	}
	return assets, "", ""
}

// snapshotEntry finds the relational snapshot among a container's entries.
func snapshotEntry(entries []archive.Entry) ([]byte, bool) {
	for _, e := range entries {
		if e.Name == archive.SnapshotEntryName {
			return e.Body, true
		}
	}
	return nil, false
}

// restoreDestination describes THIS deployment to archive/1's preflight.
//
// Every callback that is nil fails closed by the preflight's own definition, so
// the shape of this function is a series of deliberate answers rather than a
// series of omissions. Where this deployment genuinely cannot answer — pack
// trust, base-archive resolution — nil IS the answer, and it refuses.
func (srv *server) restoreDestination() archive.Destination {
	return archive.Destination{
		SchemaEpoch: store.PlatformSchemaEpoch,
		// Developer mode is not a setting this deployment carries yet, so the
		// honest answer is the strict one: a dev-channel pack is refused
		// (ARC-052/103) rather than admitted by a default nobody chose.
		DeveloperMode: false,
		// HasAsset answers ARC-063 from the content origin, which is exactly the
		// question "can this destination resolve a by-reference asset" — it
		// either holds those bytes or it does not.
		HasAsset: func(assetRef string) bool {
			if srv.content == nil {
				return false
			}
			return srv.content.Has(strings.TrimPrefix(assetRef, "sha256:"))
		},
		// PackTrust, BootCritical and ResolveBaseArchive are left nil, which the
		// preflight treats as no-trust-state, no-boot-critical-packs and
		// no-base-archives respectively. The first two fail closed; the third
		// refuses any incremental archive. That is correct for a deployment with
		// no pack lockfile and no archive index, and each becomes a real answer
		// when those exist.
	}
}

// WithStorePath tells the server which live store a restore stages beside.
//
// Without it a restore is refused UNAVAILABLE rather than accepted and failed
// later — a Job spent on a promise this process already knows it cannot keep is
// worse than a refusal a client can act on.
func WithStorePath(path string) Option {
	return func(srv *server) { srv.storePath = path }
}
