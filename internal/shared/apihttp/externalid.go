package apihttp

import (
	"fmt"
	"net/http"
)

// ExternalRef is the minimal projection of an existing resource this file's
// helpers need: its server-assigned id, its own external_id (empty when
// unset), the resource type it is an instance of, and the scope node it is
// placed under. A caller assembles this projection from whatever store it
// queries — like this package's other helpers (concurrency.go, pagination.go)
// and apiselector, this file holds no persistence of its own.
type ExternalRef struct {
	ID           string
	ExternalID   string
	ResourceType string
	ScopeNode    string
}

// ExternalIDError describes the Problem a caller MUST emit when
// CheckExternalIDUnique or ResolveRef rejects a request: the HTTP Status, the
// closed-registry Code (API-011: EXTERNAL_ID_CONFLICT or NOT_FOUND), a human
// Title, and a human-readable Detail. It is returned by pointer — a nil
// *ExternalIDError means the request is accepted (CheckExternalIDUnique) or
// the reference resolved (ResolveRef).
type ExternalIDError struct {
	Status int
	Code   string
	Title  string
	Detail string
}

// Error lets a *ExternalIDError satisfy the error interface; the message is
// the Problem Detail.
func (e *ExternalIDError) Error() string { return e.Detail }

// CheckExternalIDUnique enforces api/1's external_id uniqueness rule
// (API-101/102) before a create or update executes its write. A non-empty
// externalID that already names a DIFFERENT resource (an existing record
// whose ID != selfID) of the same resourceType placed under the same
// scopeNode is a collision, rejected 400 / EXTERNAL_ID_CONFLICT (API-102) —
// the caller MUST NOT perform the write when the return is non-nil, and the
// check MUST run before any write (there is no unconditional-write path, the
// same discipline API-022 imposes on concurrency).
//
// The same externalID value used by a different resourceType, or by the same
// resourceType under a different scopeNode, is NOT a collision (API-101):
// placement together with type — not the bare string alone — is the
// uniqueness scope. An empty externalID never collides, since setting it is
// always optional (API-100).
//
// resourceType is the matching key API-101 scopes uniqueness by (a family
// tag like "scope-nodes", shared by every kind of that family); displayName
// is the human noun the rejection's Detail names instead — e.g. "scope node"
// — so a Problem a client reads never leaks the bare, possibly-plural or
// possibly-ungrammatical tag (e.g. "another scope-nodes under this parent").
//
// selfID names the resource being created or updated: empty for a create (so
// no existing record's ID can equal it, and the check can never suppress
// itself), or that resource's own id for an update — so an update that
// leaves its own external_id unchanged is never mistaken for a collision
// with itself. selfID only excuses the one record it names; a colliding
// value already claimed by a DIFFERENT existing record is still rejected
// even when selfID is supplied for some other resource entirely.
func CheckExternalIDUnique(existing []ExternalRef, resourceType, displayName, scopeNode, externalID, selfID string) *ExternalIDError {
	if externalID == "" {
		return nil
	}
	for _, ref := range existing {
		if ref.ResourceType != resourceType || ref.ScopeNode != scopeNode {
			continue // a different type or scope node is not a collision (API-101)
		}
		if ref.ExternalID != externalID {
			continue
		}
		if ref.ID == selfID {
			continue // the same resource, not a DIFFERENT one — not a collision
		}
		return &ExternalIDError{
			Status: http.StatusBadRequest,
			Code:   "EXTERNAL_ID_CONFLICT",
			Title:  "Bad Request",
			Detail: fmt.Sprintf("external_id %q is already in use by another %s under this parent.", externalID, displayName),
		}
	}
	return nil
}

// ResolveRef resolves value — as supplied by a field that MAY accept either a
// resource's id or its external_id (API-103) — against existing, narrowing
// the search to records of resourceType placed under scopeNode: the SAME
// (type, scope node) unit API-101 scopes uniqueness by, so the fallback below
// never crosses into a different placement's or type's namesake. displayName
// is the human noun the NOT_FOUND Detail names — see CheckExternalIDUnique's
// doc for why this is kept separate from resourceType, the matching key.
//
// value is tried as an id FIRST; only when no candidate's ID matches does it
// fall back to external_id — so a value that happens to equal one record's id
// resolves to that record even when it would also match a different record's
// external_id (id resolution always takes precedence). A value resolving to
// neither is rejected with the same code: NOT_FOUND an unresolvable id
// produces elsewhere in this contract (API-103) — this helper does not
// distinguish "no such id" from "no such external_id" in its Problem, since
// the contract requires a caller be unable to tell the two apart.
func ResolveRef(value string, existing []ExternalRef, resourceType, displayName, scopeNode string) (id string, prob *ExternalIDError) {
	for _, ref := range existing {
		if ref.ResourceType == resourceType && ref.ScopeNode == scopeNode && ref.ID == value {
			return ref.ID, nil
		}
	}
	for _, ref := range existing {
		if ref.ResourceType == resourceType && ref.ScopeNode == scopeNode && ref.ExternalID != "" && ref.ExternalID == value {
			return ref.ID, nil
		}
	}
	return "", &ExternalIDError{
		Status: http.StatusNotFound,
		Code:   "NOT_FOUND",
		Title:  "Not Found",
		Detail: fmt.Sprintf("No %s exists with this identifier.", displayName),
	}
}
