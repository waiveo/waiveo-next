package apihttp

import (
	"fmt"
	"net/http"
	"strconv"
)

// ETag returns the strong entity-tag for a mutable resource at the given
// revision (api/1 API-020): the decimal revision as a quoted string, e.g.
// ETag(3) == `"3"`. api/1 derives the validator *solely* from `revision`, so a
// client may treat the `ETag` response header and the resource's `revision`
// field as interchangeable. The value is a strong validator — no `W/` prefix.
func ETag(revision int64) string {
	return `"` + strconv.FormatInt(revision, 10) + `"`
}

// ConcurrencyOutcome is the verdict of CheckIfMatch. When OK is false it fully
// describes the Problem the caller MUST emit — Status/Code/Title/Detail — and
// for a REVISION_CONFLICT it carries CurrentRevision so the response can
// surface the resource's current `revision` (API-023). A non-OK outcome means
// the caller MUST NOT perform the write: the precondition check runs before
// any state change, and there is no unconditional-overwrite path (API-022).
type ConcurrencyOutcome struct {
	OK              bool
	Status          int
	Code            string
	Title           string
	Detail          string
	CurrentRevision *int64
}

// CheckIfMatch enforces api/1's optimistic-concurrency precondition (API-021–
// 023) for a state-changing request against an existing mutable resource at
// currentRevision.
//
//   - headerPresent false → 428 Precondition Required / IF_MATCH_REQUIRED
//     (API-022): every update/delete MUST condition on If-Match; there is no
//     unconditional-overwrite path.
//   - a supplied If-Match whose value does not equal ETag(currentRevision) →
//     412 Precondition Failed / REVISION_CONFLICT (API-023), with
//     CurrentRevision set so the Problem can carry `current_revision`.
//   - a supplied If-Match equal to ETag(currentRevision) → OK.
//
// Surrounding double-quotes on the supplied validator are tolerated (API-020:
// the header carries an entity-tag, but the quoted and bare-decimal forms name
// the same revision). A non-OK outcome NEVER authorizes a write.
func CheckIfMatch(ifMatchHeader string, headerPresent bool, currentRevision int64) ConcurrencyOutcome {
	if !headerPresent {
		return ConcurrencyOutcome{
			Status: http.StatusPreconditionRequired, // 428
			Code:   "IF_MATCH_REQUIRED",
			Title:  "Precondition Required",
			Detail: "This operation requires If-Match.",
		}
	}

	supplied := unquoteETag(ifMatchHeader)
	current := strconv.FormatInt(currentRevision, 10)
	if supplied != current {
		rev := currentRevision
		return ConcurrencyOutcome{
			Status:          http.StatusPreconditionFailed, // 412
			Code:            "REVISION_CONFLICT",
			Title:           "Precondition Failed",
			Detail:          fmt.Sprintf("If-Match %q does not match the current revision, %d.", supplied, currentRevision),
			CurrentRevision: &rev,
		}
	}

	return ConcurrencyOutcome{OK: true}
}

// unquoteETag strips one layer of surrounding double-quotes from an entity-tag
// so `"3"` and `3` compare equal against a revision's canonical decimal form.
func unquoteETag(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}
