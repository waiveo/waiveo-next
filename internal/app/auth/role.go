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
// rather than Resolve because no api/1 route in this increment is addressed BY a
// scope node — a request names a resource, and the mapping from resource to
// governing scope node is part of the same deferred per-operation configuration.
// When that mapping lands, the middleware switches from Effective to Resolve and
// this function's callers shrink to zero; Resolve is already the real rule.
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
