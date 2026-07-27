package api

import (
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// scopeview.go computes ONE request's view of the scope-node tree: which nodes
// lie under which (the selector's `scope_node subtree` term, API-044) and which
// nodes the authenticated caller may READ at all (SEC-010's binding
// inheritance). Both come from the same tree read, so a request never answers
// "is B under A?" and "may I see B?" from two differently-timed snapshots.
//
// # The visible set
//
// SEC-010: "a role bound at a scope node applies to that node and, absent a
// more specific binding, to its descendants." A principal's visible set is
// therefore every node whose ancestor chain reaches one of their bindings
// before it reaches a nearer one that overrides it — which is exactly
// auth.Resolve, already implemented and tested in the auth package. This file
// deliberately calls it per node instead of re-deriving inheritance here: two
// implementations of one inheritance rule is how a visibility check and an
// authorization check start disagreeing, and the disagreement is always a leak.
//
// # Why an out-of-reach row is 404, never 403
//
// events/1 EVT-122 states the posture and its reason outright: a selector term
// resolving outside the principal's readable set "MUST simply match nothing for
// that term ... it MUST NOT be surfaced as an error. Treating an out-of-reach
// scope node as an error would let a selector probe for the existence of scope
// nodes the principal cannot read." EVT-120 ties that set to this surface —
// the subscriber's visible set is "computed the same way any other api/1-governed
// read is scoped" — so events/1 is describing api/1's posture, not inventing a
// separate one, and api/1 owes the same answer.
//
// api/1's own error taxonomy makes the choice concrete. FORBIDDEN is "the
// principal is authenticated but not authorized for this operation"; NOT_FOUND
// is "no resource exists at the identifier named by the request." Answering a
// single-resource read of an out-of-reach row with FORBIDDEN would confirm that
// SOMETHING exists at that id — the precise fact EVT-122 forbids a selector
// from discovering, differing only in how many requests it takes to learn it.
// So an out-of-reach row is answered exactly as a nonexistent id is: 404 /
// NOT_FOUND, named by the resource's own displayName, indistinguishable from an
// id that was never minted. API-103 already applies this equivalence in the
// other direction — an external_id resolving to nothing "MUST reject with the
// same code: NOT_FOUND an unresolvable id produces."
//
// 403 keeps the refusals that reveal nothing a caller does not already know
// about ITSELF: no binding anywhere, an insufficient role for the method, a
// missing CSRF token (auth's middleware). Those describe the caller. A 404 here
// describes a row, so it must describe every unreachable row identically.
//
// # Why an unauthorized PLACEMENT is 403, and why that leaks nothing
//
// A write names its own scope node: a create supplies the node it places the
// row under, a patch supplies the node it moves the row to. So the refusal is
// answering a question the caller ASKED about a node the caller NAMED, and
// FORBIDDEN's own definition fits it exactly — "the principal is authenticated
// but not authorized for this operation." Pretending the node does not exist
// would additionally be a claim api/1 has no id to attach it to: the 404 the
// read side returns is scoped to "no resource exists at the identifier named by
// the request", and on a create there is no such identifier yet.
//
// The anti-probing property the read side buys with 404 is preserved here by
// ORDERING rather than by status code, and it is stronger for it. canWrite
// consults only the caller's own bindings and the named node's ancestor chain;
// datamodel.ScopeTree.AncestorChain returns nil for any node the tree does not
// contain, and auth.Resolve resolves a nil chain solely through the
// workspace-root fallback. A caller not bound at the root therefore receives the
// SAME 403, with the same body, whether the node they named is real-but-out-of-
// reach or was never minted at all — and because the check runs before anything
// that consults stored state, no later stage gets the chance to tell them apart.
//
// That ordering is what closes the external_id oracle specifically. API-101
// scopes uniqueness by (resource type, scope node), so the guard MUST scan every
// row under the target node, including rows the caller cannot see; narrowing it
// to the visible subset would break the rule the guard exists to enforce. It
// does not need narrowing. auth.CanWrite's floor (`operator`) outranks
// auth.CanRead's (`viewer`) in one total order, so write authority at a node
// implies read authority at it: once the placement is authorized, every row the
// guard scans under that placement is a row the same caller could have
// enumerated with a list. The guard only ever runs for a caller already entitled
// to the answer it could give.
//
// # Selectors narrow, never widen
//
// events/1 EVT-121 states the rule for its own binding: a selector "MUST only
// narrow the default visible set — it MUST NOT be able to widen delivery to a
// scope node the principal cannot otherwise read." The api/1 selector is the
// SAME grammar (API-046 makes this contract's grammar the platform's sole
// definition), so it gets the same treatment structurally rather than by
// promise: every list intersects the visible set with the selector's own
// predicate, and intersection cannot widen. A selector naming a node the caller
// cannot reach therefore yields an empty page — never a 403, and never a 400,
// which would be the same existence oracle in a different status code. A
// selector that does not PARSE is still 400 / SELECTOR_INVALID (API-045): that
// is a statement about the request's syntax, and it reveals nothing about which
// nodes exist.
type scopeView struct {
	// inSubtree reports whether node lies STRICTLY below ancestor — the
	// predicate apiselector.Selector.Matches consults for a `scope_node subtree`
	// term (equality with the named node itself is the selector's own business).
	inSubtree func(ancestor, node string) bool
	// canRead reports whether the request's principal may read a resource placed
	// at node. It is the visible-set membership test every read on this surface
	// funnels through.
	canRead func(node string) bool
	// canWrite reports whether the request's principal may WRITE at node —
	// create a resource placed there, move one there, or mutate one already
	// there. It is the membership test every write on this surface funnels
	// through, and it is answered from the SAME tree read and the SAME binding
	// set canRead is, so a single request cannot decide "may I see this row?"
	// and "may I write it here?" against two differently-timed snapshots.
	canWrite func(node string) bool
	// roleAt resolves the actual ROLE the request's principal holds at node
	// (SEC-010's inheritance, auth.Resolve), and whether any binding applies.
	//
	// canRead/canWrite answer the two coarse floors every ordinary read and
	// write on this surface funnels through, and that is all any of them needs:
	// security-model/1's own draft-note beneath SEC-012 leaves "the complete
	// permission matrix for admin/operator/viewer against every individual
	// api/1 operation ... as per-operation api/1 configuration". roleAt is what
	// an operation configured ABOVE those floors asks — today the two
	// data-subject operations (workspace.go), which SEC-011's owner-exclusivity
	// argument places at `owner`. It resolves through the SAME auth.Resolve the
	// two floors do, over the same tree read and the same bindings, so a third
	// answer to "what authority does this caller hold here?" cannot disagree
	// with the first two.
	roleAt func(node string) (auth.Role, bool)
}

// scopeView builds r's view of the scope-node tree. The tree is read fresh per
// request (POC) from the same DesiredStateRows snapshot the feeder derives
// desired state from, so subtree containment and visibility are both answered
// against one consistent set of nodes.
//
// The principal comes from the request context, where the auth middleware put
// it. A request that carries NO principal yields a view that can read nothing:
// every route on this surface except the two credential-exchange operations
// hangs behind that middleware (api.go's authExemptPaths), so an absent
// principal is a wiring bug, and the fail-closed answer to a wiring bug is an
// empty world rather than the whole one (SEC-005: never default-permit).
func (srv *server) scopeView(r *http.Request) (scopeView, error) {
	nodes, _, _, _, err := srv.store.DesiredStateRows(r.Context())
	if err != nil {
		return scopeView{}, err
	}
	tree, _ := datamodel.BuildScopeTree(nodes)

	// FromContext's ok is deliberately discarded onto the zero Principal: a zero
	// Principal carries no bindings, and no bindings is exactly "reads nothing"
	// (auth.CanRead). Branching on ok to produce a different denial would be the
	// same outcome by a longer route.
	p, _ := auth.FromContext(r.Context())
	bindings := p.Bindings

	// One list can ask about the same placement node hundreds of times (every
	// row under one screen). The chain walk and role resolution are pure over a
	// fixed tree and a fixed binding set, so the answer is memoized for the
	// request's lifetime. The maps are confined to this request's goroutine, and
	// are kept separate per question rather than caching a resolved Role: the
	// two answers have different floors (auth.CanRead / auth.CanWrite) and a
	// shared cache keyed only by node would invite one to be read for the other.
	seen := make(map[string]bool)
	seenWrite := make(map[string]bool)

	return scopeView{
		inSubtree: func(ancestor, node string) bool {
			if ancestor == node {
				return false
			}
			for _, id := range tree.AncestorChain(node) {
				if id == ancestor {
					return true
				}
			}
			return false
		},
		canRead: func(node string) bool {
			if v, ok := seen[node]; ok {
				return v
			}
			v := auth.CanRead(bindings, tree.AncestorChain(node))
			seen[node] = v
			return v
		},
		canWrite: func(node string) bool {
			if v, ok := seenWrite[node]; ok {
				return v
			}
			v := auth.CanWrite(bindings, tree.AncestorChain(node))
			seenWrite[node] = v
			return v
		},
		roleAt: func(node string) (auth.Role, bool) {
			return auth.Resolve(bindings, tree.AncestorChain(node))
		},
	}, nil
}
