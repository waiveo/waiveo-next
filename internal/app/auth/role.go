package auth

import "net/http"

// Role is an authorization level bound to a (principal, scope node) pair
// (SEC-010). The four values are fixed by the contract; their ordering below is
// this package's own, and is a TOTAL order — every role strictly contains the
// authority of the one beneath it — which is what makes "at least operator" a
// meaningful check.
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
	RoleOwner    Role = "owner"
)

// roleRank orders the four roles for the "at least" comparisons authorization
// is expressed in. A role absent from this map is not a role.
var roleRank = map[Role]int{
	RoleViewer:   1,
	RoleOperator: 2,
	RoleAdmin:    3,
	RoleOwner:    4,
}

// ValidRole reports whether r is one of SEC-010's four role names.
func ValidRole(r Role) bool {
	_, ok := roleRank[r]
	return ok
}

// AtLeast reports whether r carries at least the authority of min. An invalid r
// is never sufficient for anything — an unrecognized role name is treated as no
// authority at all, never as an unknown that might be permissive (SEC-005: a
// decision that cannot be resolved refuses).
func (r Role) AtLeast(min Role) bool {
	have, ok := roleRank[r]
	if !ok {
		return false
	}
	want, ok := roleRank[min]
	if !ok {
		return false
	}
	return have >= want
}

// RootScopeNode is the sentinel scope node standing for the workspace root: the
// implicit ancestor of every real `org → site → group → screen` node. A binding
// placed here applies platform-wide, which is what the first-boot owner
// (SEC-120) needs before any org node necessarily exists. It is deliberately not
// a ULID — `*` cannot collide with a real scope-node id (DAT-005a requires
// those be ULIDs), so a real node can never be mistaken for the root and vice
// versa.
const RootScopeNode = "*"

// Binding is one role-binding record (SEC-010): a principal holds Role at
// ScopeNode, and — absent a nearer binding — at every descendant of it.
type Binding struct {
	BindingID   string
	PrincipalID string
	ScopeNode   string
	Role        Role
	CreatedAt   int64
}

// Resolve returns the role a principal holds AT node, given that principal's
// own bindings and the ancestor chain of node (nearest first, self included —
// exactly datamodel.ScopeTree.AncestorChain's output order), and whether any
// binding applies at all.
//
// SEC-010's rule is "a role bound at a scope node applies to that node and,
// absent a more specific binding, to its descendants" — so the walk goes from
// the node itself OUTWARD and stops at the first level that carries a binding.
// A nearer binding therefore fully REPLACES a farther one, including when it is
// weaker: an `admin` at the org who is deliberately bound `viewer` at one site
// is a viewer there, not an admin. Narrowing is the point of a nearer binding;
// taking the max across levels would make it impossible to express.
//
// RootScopeNode is consulted last, as the implicit outermost ancestor, so a
// platform-wide binding applies wherever no more specific one does.
func Resolve(bindings []Binding, ancestors []string) (Role, bool) {
	at := make(map[string]Role, len(bindings))
	for _, b := range bindings {
		// Two bindings for one principal at one node cannot both be authoritative;
		// the store's unique index makes that unrepresentable, and keeping the
		// STRONGER here means a hypothetical duplicate fails safe toward the
		// binding an operator most recently intended rather than toward silence.
		if cur, ok := at[b.ScopeNode]; ok && cur.AtLeast(b.Role) {
			continue
		}
		at[b.ScopeNode] = b.Role
	}
	for _, node := range ancestors {
		if r, ok := at[node]; ok {
			return r, true
		}
	}
	if r, ok := at[RootScopeNode]; ok {
		return r, true
	}
	return "", false
}

// CanRead reports whether a principal holding bindings may READ a resource
// placed at the scope node whose ancestor chain is ancestors — nearest first,
// self included, exactly datamodel.ScopeTree.AncestorChain's output order and
// exactly what Resolve consumes.
//
// It is deliberately a thin composition of Resolve rather than a second walk.
// SEC-010's inheritance rule — a role bound at a scope node applies to that
// node and, absent a more specific binding, to its descendants — is implemented
// ONCE, in Resolve; every "can this principal see this node?" question in the
// platform resolves through that one implementation. A second walk is how two
// answers to the same question start disagreeing, and a visibility rule that
// disagrees with the authorization rule is a leak by construction.
//
// `viewer` is the floor because it is the weakest of SEC-010's four roles and
// the one RequiredRole already names for a safe method: every valid role is at
// least a viewer. A binding carrying a role name outside the four carries no
// authority at all (Role.AtLeast returns false for an unrecognized role), so a
// malformed binding fails CLOSED — it grants sight of nothing — rather than
// being treated as an unknown that might be permissive (SEC-005).
//
// An EMPTY ancestors chain — what AncestorChain returns for a node absent from
// the tree it was built over — still resolves through Resolve's RootScopeNode
// fallback: a platform-wide binding can read a row placed at a node the tree
// does not (yet) carry, while a principal bound only somewhere inside the tree
// cannot. That is the fail-closed direction on an unknown placement, and it is
// what keeps a row whose scope_node dangles from becoming universally visible.
func CanRead(bindings []Binding, ancestors []string) bool {
	role, bound := Resolve(bindings, ancestors)
	return bound && role.AtLeast(RoleViewer)
}

// CanWrite reports whether a principal holding bindings may WRITE at the scope
// node whose ancestor chain is ancestors — create a resource placed there, move
// one there, or mutate one already placed there. ancestors is nearest first,
// self included: exactly datamodel.ScopeTree.AncestorChain's output order and
// exactly what Resolve consumes.
//
// It is CanRead's twin in every structural respect, and for the same reason:
// SEC-010's inheritance rule is implemented ONCE, in Resolve, so "may I see
// this node?" and "may I write at this node?" can never answer from two
// different walks of the same tree. The only difference is the floor.
//
// The floor is `operator`, and it is deliberately COARSE. The contract's own
// draft-note beneath SEC-012 leaves "the complete permission matrix for
// admin/operator/viewer against every individual api/1 operation ... as
// per-operation api/1 configuration, not a security-model requirement" — so
// this package does not invent one. What it does implement is the split
// RequiredRole already fixes at the method level: a safe method needs `viewer`,
// anything that can change platform state needs `operator`. This function is
// that same split asked of a PLACE rather than of a method, which is the half
// RequiredRole structurally cannot answer — a request is addressed by a
// resource, not by a scope node, so a principal who is `admin` at one site and
// `viewer` at another clears RequiredRole on either and must still be refused a
// write at the second.
//
// One consequence is load-bearing well beyond this function, so it is stated
// here where the floors are chosen: because `operator` outranks `viewer` in
// roleRank's TOTAL order, CanWrite at a node IMPLIES CanRead at that node.
// Every row a write-time uniqueness or cross-reference check scans within the
// node it was just authorized at is therefore a row the same caller could have
// enumerated by listing it. A guard that must scan its whole placement to be
// correct (api/1 API-101's external_id uniqueness) consequently discloses
// nothing, provided the placement was authorized FIRST — which is why the
// placement check is ordered ahead of it rather than the guard being narrowed.
//
// A binding carrying a role name outside the four carries no authority at all
// (Role.AtLeast refuses an unrecognized role), and an EMPTY ancestors chain —
// what AncestorChain returns for a node the tree does not contain, including
// the empty node id — resolves only through Resolve's RootScopeNode fallback.
// Both are the fail-closed direction (SEC-005): a write aimed at a node that
// does not exist is refused for exactly the same reason, and with exactly the
// same answer, as one aimed at a node the caller has no authority over.
func CanWrite(bindings []Binding, ancestors []string) bool {
	role, bound := Resolve(bindings, ancestors)
	return bound && role.AtLeast(RoleOperator)
}

// Effective returns the strongest role a principal holds ANYWHERE in the tree,
// and whether they hold any binding at all.
//
// This is the deliberately COARSE authority the request middleware evaluates
// against, and the reason it is coarse is in the contract: security-model/1's
// own draft-note beneath SEC-012 states that "the complete permission matrix for
// admin/operator/viewer against every individual api/1 operation is not
// enumerated here and is left as per-operation api/1 configuration, not a
// security-model requirement." That matrix is therefore DEFERRED, not
// implemented-and-approximated here.
//
// What this package does implement, exactly and testably, is the part the
// contract does fix: the scope-node inheritance rule (SEC-010, Resolve above)
// and the refuse-never-default-permit rule (SEC-005 — a principal with no
// binding gets no authority from this function). The middleware uses Effective
// rather than Resolve because a REQUEST is not addressed by a scope node — it
// names a resource, and which role level each individual operation demands is
// the deferred per-operation configuration.
//
// What is no longer deferred is the other half: which ROWS a request may see.
// That question IS addressed by a scope node — every resource carries the node
// it is placed under — so it is answered by CanRead/Resolve per row, not by
// this function. Effective is now strictly an admission floor ("may this
// caller reach this method at all"), and the visible set does the rest.
func Effective(bindings []Binding) (Role, bool) {
	best := Role("")
	found := false
	for _, b := range bindings {
		if !ValidRole(b.Role) {
			continue
		}
		if !found || b.Role.AtLeast(best) {
			best = b.Role
			found = true
		}
	}
	return best, found
}

// RequiredRole is the coarse per-method authority floor the middleware enforces
// (see Effective's doc for why it is method-grained rather than
// operation-grained): a safe, non-mutating method needs `viewer`; anything that
// can change platform state needs `operator`.
//
// The two roles this mapping does NOT distinguish, `admin` and `owner`, are
// exactly the two whose separation the contract pins to specific ACTS rather
// than to HTTP verbs — issuing a credential-reset grant (SEC-012, admin),
// acknowledging a capability-widening pack update / issuing a `--new-owner`
// break-glass grant / toggling developer mode (SEC-011, owner-exclusive). None
// of those acts is routed under /api/v1 in this increment; SEC-011's one rule
// that IS reachable today — the last owner binding is not deletable through
// ordinary api/1 mutation — is enforced where the deletion actually happens
// (Store.DeleteRoleBinding), not by a verb-level check that could not see it.
func RequiredRole(method string) Role {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return RoleViewer
	default:
		return RoleOperator
	}
}

// isMutatingMethod reports whether a method can change platform state — the
// predicate SEC-024's double-submit CSRF requirement keys off ("every mutating
// api/1 route reachable from a browser session").
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
