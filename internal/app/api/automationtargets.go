package api

import (
	"context"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/rules/model"
)

// automationtargets.go is the AUTHORING half of the bound eventtriggers.go
// enforces at run time: an automation's explicitly-named action targets must lie
// within the automation's own placement subtree.
//
// # Why the write side needs it at all
//
// The run side already refuses an out-of-scope target, per target, and reports
// it. That was believed to be sufficient, and it produced this:
//
//	operator authors a rule at node A naming a screen at node B → 201
//	viewer presses the button on the screen                     → nothing happens
//	operator looks for why                                       → nothing to find
//
// which is verbatim the "accepts work it never performs" defect this codebase
// keeps shipping and that eventtriggers.go's own doc spends three paragraphs
// condemning. A surface cannot condemn that shape in one paragraph and produce
// it in the next. The author is holding the wrong thing at exactly one moment —
// the write — and that is the moment to say so.
//
// # What is checked, and what deliberately is not
//
// Checked: a target named EXPLICITLY BY ID that RESOLVES to a scope node — a
// signage action's `screen_id` (RUL-233) and a device-affecting action's
// `entity_id` (RUL-010). Those are the two shapes that bypass selector
// resolution entirely, and they are exactly the two the run authorizes
// individually.
//
// NOT checked, each for its own reason:
//
//   - a `selector` or `device_class` target. It resolves at run time against
//     what the rule can READ, which is already bounded by the same subtree
//     (automationScopeView.canRead), so a selector cannot widen past the bound
//     and there is nothing to refuse at authoring. It also names no fixed set:
//     refusing one at authoring would be refusing a prediction.
//   - a target id that resolves to NOTHING. An operator may legitimately author
//     a rule before adopting the device it names, and refusing that would make
//     the order of two independent operations load-bearing. An unresolvable
//     target is reported by the run instead, which is where it becomes true.
//   - anything at all when the device plane is not wired. `entity_id` targets
//     are then unresolvable by construction; screens are still checked, because
//     they come from the store.
//
// # Why the same predicate as the run
//
// automationSubtreeBound is shared with automationScopeView rather than
// re-derived here. A write-time check that admitted something the run refuses
// would reintroduce exactly the gap it exists to close, one transcription later,
// and the two rules are not independently interesting: they are one rule asked
// at two times.
//
// # This does not make the run-time refusal redundant
//
// Scope changes after authoring. A screen is moved to another site, a group is
// re-parented, the rule itself is re-placed, a node is deleted. None of those
// re-runs this check, and none of them may turn a bounded rule into an unbounded
// one — so the run keeps checking, per target, and now REPORTS what it refused
// (fireEventTriggers' automation.run record). Authoring-time refusal removes the
// case an operator can see coming; run-time refusal plus a report covers the
// case nobody could.

// automationTargetsInScope is the automations family's pre-write `validate`
// hook. It reports one error per explicitly-named action target that resolves
// outside the automation's own placement subtree.
//
// It runs over the EFFECTIVE body — a create body, or a patch merged onto the
// stored row — so re-placing an automation at a narrower node is checked against
// the targets it will then carry, and adding a target to an existing rule is
// checked against the placement it already has.
func automationTargetsInScope(srv *server, body []byte) []datamodel.Error {
	scopeNode := parseFields(body).ScopeNode
	if scopeNode == "" {
		// DAT-006 requires the placement and the store refuses a row without one;
		// with nothing to bound against there is no question to answer here, and
		// inventing one would report a second error for one missing field.
		return nil
	}
	rule, err := model.ParseRule(body)
	if err != nil {
		// The compile gate owns malformed rule documents and answers first with a
		// message about the actual defect. Reporting a scope error over a body
		// nothing could parse would bury it.
		return nil
	}

	ctx := context.Background()
	tree, terr := srv.scopeTree(ctx)
	if terr != nil {
		// A tree this surface cannot read is not an authoring error. The write
		// proceeds and the run's own per-target check still bounds it — refusing
		// every automation write because a read failed would be a worse answer
		// than the one the run already gives.
		return nil
	}
	within := automationSubtreeBound(tree, scopeNode)

	refs := automationTargetRefs("actions", rule.Actions)
	if len(refs) == 0 {
		return nil
	}

	var errs []datamodel.Error
	screens := srv.screenScopeNodes(ctx)
	for _, ref := range refs {
		node, resolved := "", false
		switch ref.kind {
		case targetKindScreen:
			node, resolved = screens[ref.id]
		case targetKindEntity:
			if srv.devices != nil {
				if e, found := srv.devices.Entity(ref.id); found {
					node, resolved = e.ScopeNode, true
				}
			}
		}
		if !resolved || within(node) {
			continue
		}
		errs = append(errs, datamodel.Error{
			Field: ref.field,
			Code:  "ACTION_TARGET_OUT_OF_SCOPE",
			Message: fmt.Sprintf(
				"this action names a %s placed at scope node %s, which is outside the automation's own placement subtree (%s); "+
					"a run is bounded by the authority its author holds, so the rule would be stored, fire, and refuse this target every time (SEC-005/SEC-010)",
				ref.kind, node, scopeNode),
		})
	}
	return errs
}

// screenScopeNodes maps every stored screen id to its placement. It is read once
// per validation rather than per target: a rule naming ten screens must not cost
// ten list reads, and every target has to be judged against the same snapshot as
// every other or the answer depends on write timing.
//
// A read failure yields an empty map, which resolves nothing and therefore
// refuses nothing — the same fail-open-at-authoring, fail-closed-at-run posture
// the tree read above takes, and for the same reason.
func (srv *server) screenScopeNodes(ctx context.Context) map[string]string {
	rows, err := srv.store.List(ctx, store.KindScreen, store.ListFilter{})
	if err != nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(rows))
	for _, res := range rows {
		out[res.ID] = parseFields(res.Body).ScopeNode
	}
	return out
}

const (
	targetKindScreen = "screen"
	targetKindEntity = "entity"
)

// automationTargetRef is one action target named explicitly by id, with the
// addressable path of the member that named it (RUL-006 addressing) so a refusal
// sends the author to the control they typed into rather than to "the rule".
type automationTargetRef struct {
	field string
	kind  string
	id    string
}

// automationTargetRefs collects every explicitly-named target in an action
// sequence, descending into `choose` branches and defaults — which are actions
// like any other, and are exactly where a target would hide from a check that
// only walked the top level.
func automationTargetRefs(prefix string, actions []model.Member) []automationTargetRef {
	var out []automationTargetRef
	for i, a := range actions {
		at := fmt.Sprintf("%s[%d]", prefix, i)
		if a.Type == "choose" {
			for bi, b := range a.Branches {
				out = append(out, automationTargetRefs(fmt.Sprintf("%s.branches[%d].actions", at, bi), b.Actions)...)
			}
			out = append(out, automationTargetRefs(at+".default", a.Default)...)
			continue
		}
		if a.EntityRef != nil && a.EntityRef.EntityID != "" {
			out = append(out, automationTargetRef{field: at + ".entity_id", kind: targetKindEntity, id: a.EntityRef.EntityID})
		}
		// A signage action's ScreenRef. DecodeSignage reports a wrong-typed member
		// rather than coercing it; that refusal belongs to the compile gate, which
		// has already run, so an undecodable action simply contributes no target
		// here.
		if sm, terr := model.DecodeSignage(a.Raw); terr == nil && sm.ScreenID != "" {
			out = append(out, automationTargetRef{field: at + ".screen_id", kind: targetKindScreen, id: sm.ScreenID})
		}
	}
	return out
}
