package events

// filter.go is events/1's per-subscriber delivery predicate (EVT-120–124): the
// one place a connected subscriber's visible set, its optional narrowing
// selector, and its optional schemas restriction are combined into a single
// "may this envelope be delivered to this connection?" answer.
//
// It is binding-agnostic on purpose. EVT-123 requires scope-node filtering to be
// enforced "server-side, per event, at delivery time" on EVERY binding, so the
// SSE server (internal/app/eventsse) and the still-deferred WS server must reach
// the same verdict for the same envelope. Making the verdict a value here, rather
// than a loop inside one transport, is what keeps the two bindings from drifting
// into two different notions of what a subscriber may see.
//
// Three deliberate properties:
//
//   - The visible set is supplied as a canRead predicate rather than computed
//     here. EVT-120 pins it to "the scope nodes its principal can read, computed
//     the SAME WAY any other api/1-governed read is scoped" — that computation is
//     security-model/1's binding inheritance (SEC-010), already implemented once
//     in internal/app/auth (Resolve/CanRead) and consumed once per request by
//     api/1's own scope view. Re-deriving inheritance here would be the second
//     implementation of one rule, which is how a visibility check and an
//     authorization check start disagreeing. A nil canRead therefore denies
//     everything (SEC-005: never default-permit).
//
//   - The selector is a CONJUNCT, never a substitute. canRead is evaluated
//     independently of the selector and both must hold, so a selector can only
//     ever intersect the visible set. That is EVT-121's "MUST only narrow ... MUST
//     NOT be able to widen" enforced structurally rather than by promise, and it
//     is simultaneously EVT-122: a term naming a scope node outside the readable
//     set matches nothing, because every event under that node fails canRead —
//     an ordinary empty result, never an error, so a selector cannot be used to
//     probe for the existence of scope nodes the principal cannot read.
//
//   - A durable-event envelope carries no labels: EVT-010 fixes its field set,
//     and none of them is a label map. The selector's LABEL terms are therefore
//     evaluated against the empty label set, which is the ordinary reading of the
//     grammar rather than a special case — a positive term (equality,
//     set-membership, existence) matches nothing, and a negative term
//     (inequality, set-exclusion, non-existence) matches everything, exactly as
//     api/1 answers them for a resource carrying no labels. Neither direction can
//     widen delivery, since canRead still has to hold.

import "github.com/maaxton/waiveo-next/internal/shared/apiselector"

// Filter is one connection's delivery predicate over the durable-event envelope
// (EVT-120–124). The zero Filter denies every envelope: its canRead is nil, and
// "no resolvable visible set" is answered with an empty world rather than the
// whole one (SEC-005). Build one with NewFilter.
type Filter struct {
	// canRead reports whether the connection's principal may read a resource
	// placed at a scope node — the visible-set membership test of EVT-120,
	// supplied by the caller from auth.CanRead over the same scope tree api/1's
	// reads are scoped against.
	canRead func(scopeNode string) bool
	// selector is the client's optional narrowing selector (EVT-121). The zero
	// Selector holds no terms and matches everything, which is exactly the
	// "absent a narrowing selector" case EVT-120 describes.
	selector apiselector.Selector
	// inSubtree reports whether node lies STRICTLY below ancestor; the selector's
	// `scope_node subtree` term (API-044) consults it. May be nil when the
	// selector carries no subtree term.
	inSubtree func(ancestor, node string) bool
	// schemas is the optional EVT-124 restriction as a set. Empty means "no
	// schemas restriction" — the filter then imposes none, and scope-node
	// filtering still applies in full (EVT-124: alongside, never in place of).
	schemas map[string]struct{}
}

// NewFilter builds a connection's delivery predicate.
//
// canRead is the visible-set membership test (EVT-120); passing nil yields a
// filter that denies everything. selector is the client's optional narrowing
// selector, already parsed under api/1's grammar (EVT-121 — the zero Selector
// means none was supplied). inSubtree answers the selector's `scope_node
// subtree` term and may be nil when the selector has no such term. schemas is
// the optional registered-schema restriction (EVT-124); an empty or nil slice
// imposes none.
func NewFilter(canRead func(scopeNode string) bool, selector apiselector.Selector, inSubtree func(ancestor, node string) bool, schemas []string) Filter {
	var set map[string]struct{}
	if len(schemas) > 0 {
		set = make(map[string]struct{}, len(schemas))
		for _, s := range schemas {
			set[s] = struct{}{}
		}
	}
	return Filter{canRead: canRead, selector: selector, inSubtree: inSubtree, schemas: set}
}

// Allows reports whether env may be delivered to this connection (EVT-120–124).
//
// The three restrictions are ANDed, and the visible-set test is FIRST and
// unconditional: an envelope whose scope_node falls outside the principal's
// readable set is never delivered, whatever the selector or schemas say
// (EVT-120/123). The remaining two only narrow further.
func (f Filter) Allows(env Envelope) bool {
	// EVT-120/123, per event at delivery time. A nil canRead is a Filter that was
	// never built by NewFilter with a real visible set; it denies everything.
	if f.canRead == nil || !f.canRead(env.ScopeNode) {
		return false
	}
	// EVT-124: an additional restriction ALONGSIDE scope-node filtering, applied
	// only when a list was supplied. A schema not in the list is simply not
	// delivered — an unregistered or misspelled name is a member no event matches,
	// which is an empty stream rather than an error.
	if len(f.schemas) > 0 {
		if _, ok := f.schemas[env.Schema]; !ok {
			return false
		}
	}
	// EVT-121/122: the client's own narrowing selector, evaluated against the
	// envelope's placement and (per this file's header) the empty label set. It
	// runs LAST and can only remove envelopes the two tests above already
	// admitted, so it is incapable of widening delivery.
	return f.selector.Matches(nil, env.ScopeNode, f.inSubtree)
}
