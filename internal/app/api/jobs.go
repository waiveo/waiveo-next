package api

import (
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
)

// jobs.go serves GET /api/v1/jobs/{job_id} — the read half of api/1's async
// convention (openapi getJob).
//
// API-112 makes this operation the ONLY completion signal the contract offers:
// "a client determines completion by reading the Job resource again and
// inspecting `state`, not by any signal delivered on the original request." An
// async endpoint's 202 is therefore a promise this route keeps; without it the
// Job a fleet-mutating operation returns is a shape with nothing behind it.
//
// The body is rendered by the shared state machine (apijob.Job.Resource), which
// returns the GENERATED apiv1.Job — so the polled representation is field-for-
// field the same model, produced by the same code, as the one the accepting 202
// emitted, and neither can drift into a hand-rolled parallel of API-112's shape.
//
// # Why a Job carries no ETag
//
// Every resource-family read on this surface returns one (API-020). A Job is not
// a resource a client may write: it has no PATCH, no DELETE and no revision, and
// its state advances from the server's own execution rather than from a
// conditional request. An ETag would offer a precondition for a write that does
// not exist. openapi's getJob declares no ETag response header, and this handler
// emits none.

// jobDisplayName is the human noun a Job's 404 `detail` names it by, matching
// the resourceConfig.displayName convention every other family's not-found
// Problem follows ("No <displayName> exists with this identifier.").
const jobDisplayName = "job"

// getJob serves GET /api/v1/jobs/{job_id}: the current, persisted state of one
// Job (API-112's poll).
//
// A job_id naming nothing, and a job this caller may not see, are answered
// IDENTICALLY — 404 / NOT_FOUND, with the same detail — per the anti-probing
// posture scopeview.go states in full: answering an out-of-reach record with 403
// would confirm that something exists at that id, which is the existence oracle
// EVT-122 forbids a selector from having, differing only in how many requests it
// takes to learn it.
func (srv *server) getJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("job_id")
	rec, found, err := srv.store.GetJob(r.Context(), id)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	if !found {
		jobNotFound(w, r)
		return
	}
	view, err := srv.scopeView(r)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	if !jobVisible(rec, auth.PrincipalID(r), view) {
		jobNotFound(w, r)
		return
	}
	writeJSONValue(w, http.StatusOK, rec.Job.Resource())
}

// jobNotFound writes the one Problem BOTH an unknown job_id and an out-of-reach
// Job draw. It is a single function on purpose: two call sites composing the
// same 404 independently is how the two answers start differing by a word, and a
// Problem body is compared member for member by anyone looking for one.
func jobNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No "+jobDisplayName+" exists with this identifier.")
}

// jobVisible reports whether principal may read this Job at all.
//
// A Job's body ENUMERATES the rows it acts on, by id (API-112's `targets`). So a
// Job read is a read of that row set in another shape, and it inherits the same
// visible-set scoping every other read on this surface enforces (scopeview.go,
// auth.CanRead): showing a caller a Job whose targets they cannot list would
// hand them, one id at a time, exactly what the scoped list refuses them.
//
// Two clauses, in this order:
//
//   - The SUBMITTER may always read the Job they submitted. This is not a
//     weakening: the 202 that accepted their request already handed them the
//     complete target list (that IS the Job resource), so refusing them the same
//     bytes on the poll would withhold nothing they do not already hold, while
//     breaking API-112's polling loop for the one principal it exists to serve.
//   - Anyone else must be able to read EVERY target's placement. Not "some" —
//     the body is all-or-nothing, and a partial view would have to either omit
//     targets (misrepresenting the job, since the job-level state is derived from
//     the full set) or disclose the ones it cannot show. A Job with NO targets is
//     readable by its submitter alone: an empty target set makes the placement
//     test vacuously true, and a vacuous truth is not an authorization.
//
// The placements are the ones recorded at acceptance (store.JobRecord), so this
// answer is stable for the life of the Job and cannot be widened afterwards by
// moving a target row under a node the caller happens to be bound at.
func jobVisible(rec store.JobRecord, principal string, view scopeView) bool {
	if principal != "" && rec.Job.CreatedBy == principal {
		return true
	}
	targets := rec.Job.Targets()
	if len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		if !view.canRead(rec.ScopeNodes[t.ID]) {
			return false
		}
	}
	return true
}
