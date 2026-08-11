package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/apijob"
)

// workspace.go serves api/1's two data-subject operations (API-120–124):
// POST /api/v1/workspace/export and POST /api/v1/workspace/delete.
//
// # Which contract owns what
//
// api/1 owns the OPERATION: its request and response shape, its authorization,
// and the 202 + Job convention both answer with (API-123). It owns nothing about
// what the export produces or what the delete destroys, and this file
// deliberately invents neither:
//
//   - The artifact is archive/1's, entirely. API-121: the export operation "MUST
//     produce exactly the container `archive/1` defines — this operation is the
//     API-facing trigger for that same export, not a distinct export format or a
//     second code path". So the execution below calls internal/archive and
//     assembles that contract's manifest; there is no api/1-side export format,
//     and there must never be one. archive/1's own Scope says the same thing from
//     its side: backup, box migration, and "exporting a workspace's data for the
//     operator who owns it are all the same file operation under this contract."
//   - The destruction is security-model.md's, entirely. API-122: the delete
//     operation "MUST trigger the workspace's key-material destruction path
//     (`security-model.md` SEC-121) — deleting a workspace's data, at the
//     self-hosted realization this section specifies, is that same destruction,
//     not a separate deletion mechanism."
//
// # Who may invoke them: `owner`, and why
//
// The contract does not name a role for either operation, and it says why: the
// draft-note beneath SEC-012 leaves "the complete permission matrix for
// admin/operator/viewer against every individual api/1 operation ... as
// per-operation api/1 configuration, not a security-model requirement." This is
// that configuration, and it is `owner` for both, for two independent reasons:
//
//   - Neither operation is scope-narrowable. Every other write on this surface is
//     authorized at the ONE scope node it acts on, so an admin at Site A can
//     mutate Site A and nothing else (scopeview.go). An export carries the entire
//     workspace and a delete destroys the entire workspace; there is no node to
//     narrow to, so the coarse operator/viewer split canWrite implements has
//     nothing to say here. Admitting an `admin` bound at one site would let a
//     principal with authority over one site exfiltrate or erase every other.
//   - SEC-011 already reserves the workspace-wide, irreversible acts to `owner` —
//     acknowledging a capability-widening pack update, issuing a `--new-owner`
//     break-glass grant, toggling developer mode — and it names factory reset as
//     the ONLY thing that may remove the last owner binding. The delete operation
//     IS that factory reset's destruction path (API-122), so admitting a role
//     that SEC-011 will not let delete one owner binding, to an operation that
//     destroys every binding at once, would be incoherent.
//
// The role is resolved AT the workspace's own org node (scopeView.roleAt), so a
// principal bound `owner` at the workspace root or at the org itself qualifies
// and one bound `owner` at some narrower node does not — SEC-010's inheritance
// runs outward from the node, and the node here is the workspace.
//
// # The delete's safety gate: what the contract requires, and what it does not
//
// Stated plainly because the alternative is silence being read as completeness:
// NO requirement in api/1 or security-model.md imposes a confirmation token, a
// grant redemption, or a two-phase handshake on this operation. API-124 says the
// section "specifies the export and delete operations' request/response shape
// and their self-hosted realization (API-121–122) only", and defers the fuller
// data-subject-request workflow.
//
// Silence on an irreversible operation is not permission to make it a one-shot,
// so this file chooses a conservative default rather than the minimum the
// contract would tolerate: the request body MUST carry `confirm_workspace_id`
// equal to the id of the org node it is about to destroy. That id is not
// guessable and not derivable from the request — a caller learns it only by
// first READING the workspace (GET /api/v1/scope-nodes), so no single blind
// request can erase a deployment even from a fully authorized session. A missing
// or mismatched value is refused 422 before anything is destroyed.
//
// What this deliberately does NOT do is mint a confirmation grant. SEC-030 fixes
// a CLOSED vocabulary of seven grant purposes and none of them covers this;
// adding an eighth to carry a confirmation would be this file inventing a
// security-model mechanism, which is exactly the kind of parallel invention
// API-121/122 rule out for the artifact and the destruction.
//
// # Both operations are async, and that is the contract's own reasoning
//
// API-123: both "MUST respond `202 Accepted` with a Job resource a client polls
// for completion (API-111) — neither a full workspace export nor an irreversible
// key-material destruction completes within its own request/response cycle."
// Each Job carries exactly ONE target, the workspace itself, because the same
// sentence continues: "Each operation's target is the workspace itself, implicit
// in the request path; API-110's selector convention does not apply, since
// neither operation takes a selector-chosen subset."
//
// The workspace's identity is its org-kind scope node. data-model/1 fixes that:
// DAT-010 makes `account_state` an org-only column, DAT-014 admits exactly one
// org node, and DAT-012 names the org node as the row that reaches `purged`
// "once [the data-subject delete operation] has run".

// workspaceDisplayName is the human noun a workspace 404's `detail` names it by,
// matching the resourceConfig.displayName convention every other family's
// not-found Problem follows ("No <displayName> exists with this identifier.").
const workspaceDisplayName = "workspace"

// minExportPassphraseLen is the floor on an export passphrase's length, matching
// the `minLength: 12` the openapi WorkspaceExportRequest schema declares.
//
// It is a length floor and nothing more: archive/1 stretches the passphrase
// through a memory-hard KDF (ARC-010), which is what actually buys resistance to
// guessing, and a composition rule ("one digit, one symbol") would narrow the
// search space it claims to widen. Twelve is the point below which even a
// memory-hard KDF stops helping.
const minExportPassphraseLen = 12

// workspaceExportRequest is the WorkspaceExportRequest body. Passphrase is a
// pointer so an ABSENT field is distinguishable from an explicit "" — both are
// refused, but they are refused for reasons worth keeping distinguishable in the
// code that decides.
type workspaceExportRequest struct {
	Passphrase *string `json:"passphrase"`
}

// workspaceDeleteRequest is the WorkspaceDeleteRequest body.
type workspaceDeleteRequest struct {
	ConfirmWorkspaceID *string `json:"confirm_workspace_id"`
}

// exportWorkspace handles POST /api/v1/workspace/export (openapi
// exportWorkspace): the data-subject export operation (API-120/121).
//
// Like every other mutating mcp:act POST on this surface it honors
// Idempotency-Key through the SAME srv.idempotent seam the bulk-enable and
// per-automation-run operations use (API-050/052/072) — a client's
// retry-on-timeout replays the original 202 + Job verbatim rather than starting a
// second export of the same workspace under a second Job.
func (srv *server) exportWorkspace(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	// See the note on runAutomation: the declared member set, checked before the
	// idempotency record is written.
	if srv.undeclaredMemberRejected(w, r, "WorkspaceExportRequest", body) {
		return
	}
	srv.idempotent(w, r, body, func(w http.ResponseWriter) { srv.exportWorkspaceExec(w, r, body) })
}

// exportWorkspaceExec is the export's acceptance, executed once per fresh
// (non-replayed) request under the Idempotency-Key guard. It writes into the
// response capture that guard owns, so the exact 202 + Job bytes are retained
// for a later retry's verbatim replay.
func (srv *server) exportWorkspaceExec(w http.ResponseWriter, r *http.Request, body []byte) {
	workspaceID, ok := srv.authorizeWorkspaceOwner(w, r)
	if !ok {
		return
	}

	var req workspaceExportRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// API-013a: a body failure is 422, never 400 — the same class, and the
		// same status, POST /automations/bulk-enable already returns for an
		// unparseable body.
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", "The request body could not be parsed.")
		return
	}
	if req.Passphrase == nil || strings.TrimSpace(*req.Passphrase) == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", "`passphrase` is required.")
		return
	}
	if len([]rune(*req.Passphrase)) < minExportPassphraseLen {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"`passphrase` must be at least 12 characters.")
		return
	}
	if srv.workspaceArchive == nil {
		// No archive destination is wired, so there is nowhere for the container
		// API-121 requires to be written. UNAVAILABLE ("temporarily unable to
		// serve the request", retriable) is the honest reading: the operation is
		// declared and authorized, the deployment is not currently able to serve
		// it, and a client MAY retry once it is. Accepting the work with a 202
		// and failing the target later would be worse — it would spend a Job on a
		// promise this process already knows it cannot keep.
		writeProblem(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
			"This deployment is not configured with a workspace archive destination.")
		return
	}

	job, ok := srv.acceptWorkspaceJob(w, r, workspaceID)
	if !ok {
		return
	}
	// The container this export will write is named by the Job that produces it
	// (workspacerun.go), so its name is known HERE, at acceptance, and the claim
	// is taken here rather than when the execution starts. The window between the
	// two is real — the Job runner may queue this work behind another export's
	// argon2id pass — and a delete arriving inside it would unlink the container
	// the moment after it appeared, leaving an export that reported success and
	// produced nothing. Claiming at acceptance closes the whole accepted lifetime.
	release := srv.workspaceArchive.claimArchive(exportedArchiveName(job.ID),
		"An export accepted as job "+job.ID+" is still writing this container.")

	writeJSONValue(w, http.StatusAccepted, job.Resource())

	passphrase := *req.Passphrase
	srv.jobs.submit(func(ctx context.Context) {
		defer release()
		srv.runWorkspaceExport(ctx, job.ID, workspaceID, passphrase)
	})
}

// deleteWorkspace handles POST /api/v1/workspace/delete (openapi
// deleteWorkspace): the data-subject delete operation (API-120/122), which
// triggers SEC-121's key-material destruction.
func (srv *server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	// See the note on runAutomation: the declared member set, checked before the
	// idempotency record is written.
	if srv.undeclaredMemberRejected(w, r, "WorkspaceDeleteRequest", body) {
		return
	}
	srv.idempotent(w, r, body, func(w http.ResponseWriter) { srv.deleteWorkspaceExec(w, r, body) })
}

// deleteWorkspaceExec is the delete's acceptance. The ordering below is the
// whole of its safety, and it is deliberate:
//
//	authorize -> validate the confirmation -> mint the Job -> 202 -> destroy
//
// Authorization runs FIRST so the confirmation check never becomes an oracle: a
// non-owner who could probe `confirm_workspace_id` values and tell a mismatch
// (422) from a match (202) would learn the workspace's own id from an operation
// they may not invoke. Refusing them before the value is examined means every
// non-owner gets the same 403 whatever they sent. For the same reason, the
// mismatch Problem below names no id — it says what is required, never what was
// expected.
func (srv *server) deleteWorkspaceExec(w http.ResponseWriter, r *http.Request, body []byte) {
	workspaceID, ok := srv.authorizeWorkspaceOwner(w, r)
	if !ok {
		return
	}

	var req workspaceDeleteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", "The request body could not be parsed.")
		return
	}
	if req.ConfirmWorkspaceID == nil || *req.ConfirmWorkspaceID != workspaceID {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"`confirm_workspace_id` must name the workspace this request destroys; nothing was destroyed.")
		return
	}

	job, ok := srv.acceptWorkspaceJob(w, r, workspaceID)
	if !ok {
		return
	}
	writeJSONValue(w, http.StatusAccepted, job.Resource())

	srv.jobs.submit(func(ctx context.Context) {
		srv.runWorkspaceDelete(ctx, job.ID, workspaceID)
	})
}

// authorizeWorkspaceOwner resolves the workspace's org node and confirms the
// request's principal holds `owner` AT it, writing the refusal itself and
// reporting ok=false when either step fails — so a caller has one thing to
// check.
//
// The two refusals it can write:
//
//   - 404 / NOT_FOUND when no org node exists. There is no workspace for the
//     operation to act on, and NOT_FOUND is api/1's own code for "no resource
//     exists at the identifier named by the request" — the identifier here being
//     the path itself, which names the workspace implicitly (API-123).
//   - 403 / FORBIDDEN when the caller is not `owner` at that node, carrying the
//     ONE detail every unauthorized-write refusal on this surface carries. It
//     names no node and no role, exactly as unauthorizedWriteDetail does not: a
//     detail worded one way for "you are only an admin" and another for "you have
//     no binding here" reports on the workspace rather than on the caller.
func (srv *server) authorizeWorkspaceOwner(w http.ResponseWriter, r *http.Request) (workspaceID string, ok bool) {
	workspaceID, _, err := srv.store.WorkspaceRoot(r.Context())
	if err != nil {
		if err == store.ErrNoWorkspace {
			writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found",
				"No "+workspaceDisplayName+" exists with this identifier.")
			return "", false
		}
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return "", false
	}
	view, err := srv.scopeView(r)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return "", false
	}
	role, bound := view.roleAt(workspaceID)
	if !bound || role != auth.RoleOwner {
		writeProblem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden", unauthorizedWorkspaceDetail)
		return "", false
	}
	return workspaceID, true
}

// unauthorizedWorkspaceDetail is the ONE `detail` both data-subject operations
// carry when the caller is not the workspace's owner. Like
// unauthorizedWriteDetail it describes the CALLER's authority and nothing about
// the workspace.
const unauthorizedWorkspaceDetail = "This principal is not an owner of this workspace."

// acceptWorkspaceJob mints, persists and returns the single-target Job both
// operations answer with (API-112/123).
//
// The Job is PERSISTED before the 202 is written, and a failure to persist
// refuses the whole operation, for the same reason the bulk-enable path does it:
// API-112 makes a poll the only way a client learns this work finished, so a 202
// naming a Job no later read could resolve would be a promise the surface cannot
// keep. A 5xx also Aborts the Idempotency-Key marker, leaving the key retryable
// (API-054).
//
// The target's recorded scope-node placement is the workspace's org node itself,
// which is what a later poll scopes the read by (jobs.go): the owner who
// submitted it can always read it, and so can any principal whose authority
// reaches the org node — which, for a job whose single target IS the workspace,
// is exactly the set that could have invoked it.
//
// # Neither operation persists a re-appliable operation, and both refuse to
//
// A bulk-enable records what it was applying so a restart can re-apply it
// (API-116, automations.go). These two record the ZERO store.JobOperation, and
// that is a decision rather than an omission — each for its own reason:
//
//   - The EXPORT's only argument is the client's passphrase. Persisting it would
//     put a user-supplied secret at rest in the jobs table, readable by anything
//     that can read the database, to buy the ability to finish an archive the
//     client can simply request again. No archive is worth that trade.
//   - The DELETE's execution is a destructive ordered sequence (assets, rows,
//     signing key, credentials) whose partial completion is invisible from the
//     outside. Re-running it after an interrupted attempt would purge a workspace
//     that is already gone and report the target `failed` NOT_FOUND — describing
//     a successful erasure as a failure, which is worse than declining to guess.
//
// A target either operation strands `running` is therefore reconciled to a
// terminal `failed` by the resume rather than resumed (jobrun.go), so the poll
// API-123 gives the submitter still terminates.
func (srv *server) acceptWorkspaceJob(w http.ResponseWriter, r *http.Request, workspaceID string) (*apijob.Job, bool) {
	// .UTC() so created_at serializes as RFC3339 "Z" (API-112); the id comes from
	// the same injected srv.newID seam every other server-minted id draws from,
	// and created_by from the REAL authenticated caller the middleware resolved.
	job := apijob.New(srv.newID(), auth.PrincipalID(r), time.UnixMilli(srv.nowMs()).UTC(), []string{workspaceID})
	if err := srv.store.CreateJob(r.Context(), job, map[string]string{workspaceID: workspaceID}, store.JobOperation{}); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return nil, false
	}
	return job, true
}
