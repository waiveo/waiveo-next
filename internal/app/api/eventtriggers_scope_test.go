package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
)

// eventtriggers_scope_test.go is the AUTHORITY half of the `event` trigger path:
// what an event-fired automation run is allowed to touch.
//
// It exists because the first implementation ran every event-fired rule under an
// all-permissive view of the scope tree — canRead and canWrite true for every
// node in the deployment. That is a privilege escalation with a very short path
// to it: `POST /automations` authorizes only the automation's OWN scope node at
// write time, and states in as many words that per-target authorization inside
// the run is what covers the entities and screens its actions name. So an
// operator with write authority at one node could author a rule whose action
// names a screen at another node — or `selector: "*"` — and the first viewer
// press executed it deployment-wide. The SAME rule run by hand through
// `POST /automations/{id}/run` was refused per target.
//
// The bound is the automation's own placement subtree, inclusive: exactly the
// authority its author had to hold to create the row. All three directions are
// asserted here, because a fix that only closed the first would be the familiar
// half-fix — and the third is the one that would break the feature.

// eventScopeNodeB is a SIBLING placement node: same fixture site, no ancestor
// relationship to autoScopeNode in either direction.
const eventScopeNodeB = "01J8Z0B000000000000000000B"

// TestAuthoringRefusesAnOutOfScopeActionTarget is the WRITE-side half, and it
// is the one that was missing: authoring accepted a rule the run would refuse
// every time.
//
// The sequence that produced was: operator authors → 201 → viewer presses →
// nothing happens → no evidence an operator can read. That is verbatim the
// "accepts a press and performs nothing" defect eventtriggers.go's own doc
// spends three paragraphs condemning, produced by the surface that condemns it.
// The author is holding the wrong thing at exactly one moment, and it is this
// one.
func TestAuthoringRefusesAnOutOfScopeActionTarget(t *testing.T) {
	e := newEnv(t)
	nodeA := e.placementNode(t)
	e.seedPlacementNodes(t, eventScopeNodeB)

	outsideScreen := mintSignageScreen(t, e, eventScopeNodeB, "Boardroom")
	castID := mintSignageCast(t, e, nodeA, "Service Requested")

	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", mustJSON(t, map[string]any{
		"name":       "Reaches Too Far",
		"scope_node": nodeA,
		"enabled":    true,
		"mode":       "single",
		"triggers":   []any{map[string]any{"type": "event", "event": "screen.interaction"}},
		"actions": []any{
			map[string]any{"type": "play_cast", "screen_id": outsideScreen, "cast_id": castID},
		},
	}), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST an automation at %s naming a screen at %s: status %d, want 422 — "+
			"the run refuses this target every time, so accepting the write stores a rule that fires and performs nothing (body %s)",
			nodeA, eventScopeNodeB, resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	if !problemCarriesCode(p, "actions[0].screen_id", "ACTION_TARGET_OUT_OF_SCOPE") {
		t.Errorf("want ACTION_TARGET_OUT_OF_SCOPE on actions[0].screen_id, got %s", raw)
	}

	// The control, and it is not decoration: a check that refused every target
	// would make signage automations unauthorable. A screen inside the rule's own
	// subtree is accepted.
	insideScreen := mintSignageScreen(t, e, nodeA, "Lobby A")
	ok, okRaw := e.do(t, http.MethodPost, "/api/v1/automations", mustJSON(t, map[string]any{
		"name":       "Reaches Its Own",
		"scope_node": nodeA,
		"enabled":    true,
		"mode":       "single",
		"triggers":   []any{map[string]any{"type": "event", "event": "screen.interaction"}},
		"actions": []any{
			map[string]any{"type": "play_cast", "screen_id": insideScreen, "cast_id": castID},
		},
	}), nil)
	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("an automation naming a screen at its OWN node was refused: %d %s", ok.StatusCode, okRaw)
	}
}

// TestAnEventFiredRunCannotActOutsideItsOwnScope is the escalation, in the only
// shape that can still reach a stored rule now that authoring refuses the
// obvious one: the rule and its target start out together at node A, and the
// SCREEN IS THEN MOVED to node B.
//
// This is why the per-target run check is not made redundant by the authoring
// check. Placement changes after authoring — a screen is moved, a group is
// re-parented, a node is deleted — and none of those re-run the write-time gate.
// A bound that only held at authoring would hold until the first reorganisation
// and then silently stop.
func TestAnEventFiredRunCannotActOutsideItsOwnScope(t *testing.T) {
	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher))
	nodeA := e.placementNode(t)
	e.seedPlacementNodes(t, eventScopeNodeB)

	// Authored in scope: the screen is at A, with the rule.
	outsideScreen := mintSignageScreen(t, e, nodeA, "Boardroom")
	castID := mintSignageCast(t, e, nodeA, "Service Requested")

	mintEventAutomation(t, e, nodeA, true,
		map[string]any{"interaction": "call_service"},
		map[string]any{"type": "play_cast", "screen_id": outsideScreen, "cast_id": castID})

	// …and then reorganised out of it, which nothing re-validates the rule on.
	moveScreenToNode(t, e, outsideScreen, eventScopeNodeB)

	dispatcher.Deliver(context.Background(), interactionEnvelope(t, outsideScreen, "call_service"))

	if after := screenProgramOf(t, e, outsideScreen, fixedNowMs); after.Display != "blank" {
		t.Fatalf("a rule placed at %s changed a screen placed at %s (now showing %q). An event-fired run must be "+
			"bounded by the automation's own scope subtree — the same authority its author had to hold to create it — "+
			"or authoring a rule at one node becomes a way to command the whole deployment.",
			nodeA, eventScopeNodeB, after.Display)
	}

	// The control. Without it this test would also pass on a run that acted on
	// nothing at all — a dispatcher that stopped firing, a rule that stopped
	// matching, a projection that stopped resolving.
	insideScreen := mintSignageScreen(t, e, nodeA, "Lobby A")
	mintEventAutomation(t, e, nodeA, true,
		map[string]any{"interaction": "call_service"},
		map[string]any{"type": "play_cast", "screen_id": insideScreen, "cast_id": castID})

	dispatcher.Deliver(context.Background(), interactionEnvelope(t, insideScreen, "call_service"))

	if after := screenProgramOf(t, e, insideScreen, fixedNowMs); after.Display != "content" {
		t.Fatalf("the in-scope screen shows %q, want content — the bound is refusing what it must allow", after.Display)
	}
}

// TestAnEventFiredSelectorResolvesOnlyWithinItsOwnScope is the same escalation
// through the other door, and the wider one: a selector's candidate set is
// filtered by canRead, so under an all-permissive view `selector: "*"` on a rule
// at one node resolved against EVERY screen in the deployment.
//
// It is a separate case from the one above because it reaches the out-of-scope
// screen through target RESOLUTION rather than through a named id, and those are
// two different code paths (screenOverrideSink.targets versus its per-target
// write). Note the honest limit: from OUTSIDE, canWrite alone decides whether a
// screen changes, so a view that read everywhere and wrote only in-subtree would
// still pass this. The read half is pinned where it can actually be seen, in
// TestAutomationScopeViewIsBoundedByTheAutomationsOwnSubtree.
func TestAnEventFiredSelectorResolvesOnlyWithinItsOwnScope(t *testing.T) {
	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher))
	nodeA := e.placementNode(t)
	e.seedPlacementNodes(t, eventScopeNodeB)

	inside := mintSignageScreen(t, e, nodeA, "Lobby A")
	outside := mintSignageScreen(t, e, eventScopeNodeB, "Boardroom")
	castID := mintSignageCast(t, e, nodeA, "Service Requested")

	mintEventAutomation(t, e, nodeA, true,
		map[string]any{"interaction": "call_service"},
		map[string]any{"type": "play_cast", "selector": "zone=lobby", "cast_id": castID})

	dispatcher.Deliver(context.Background(), interactionEnvelope(t, inside, "call_service"))

	if after := screenProgramOf(t, e, inside, fixedNowMs); after.Display != "content" {
		t.Fatalf("the selector did not reach the screen at the rule's own node (showing %q)", after.Display)
	}
	if after := screenProgramOf(t, e, outside, fixedNowMs); after.Display != "blank" {
		t.Fatalf("a label selector on a rule placed at %s reached a screen placed at %s (now showing %q); "+
			"a selector must narrow the rule's own subtree, never widen to the deployment", nodeA, eventScopeNodeB, after.Display)
	}
}

// TestAnEventFiredRunReachesItsOwnDescendants is the MIRROR direction, and it is
// the one a too-eager fix breaks. SEC-010 inherits a binding down the tree, so
// the author of a rule placed at a SITE could have written every screen beneath
// it by hand; the rule must be able to do the same. A bound that only admitted
// the rule's exact node would silently turn every site-wide rule into a no-op —
// a refusal reported per target and visible nowhere but a run report nobody
// reads on the event path.
func TestAnEventFiredRunReachesItsOwnDescendants(t *testing.T) {
	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher))
	child := e.placementNode(t) // a screen-kind node under the fixture site
	site := e.fixtureSite(t)

	screenID := mintSignageScreen(t, e, child, "Lobby A")
	castID := mintSignageCast(t, e, child, "Service Requested")

	// The rule is placed at the SITE, one level above the screen it names.
	mintEventAutomation(t, e, site, true,
		map[string]any{"interaction": "call_service"},
		map[string]any{"type": "play_cast", "screen_id": screenID, "cast_id": castID})

	dispatcher.Deliver(context.Background(), interactionEnvelope(t, screenID, "call_service"))

	if after := screenProgramOf(t, e, screenID, fixedNowMs); after.Display != "content" {
		t.Fatalf("a rule placed at the site could not act on a screen beneath it (screen shows %q); "+
			"the scope bound must inherit down the tree exactly as SEC-010 does, or every site-wide rule is inert", after.Display)
	}
}

// TestAnEventFiredRunStillPerformsItsInScopeActions: the run still
// happens, the target is still resolved, and the refusal is recorded — the same
// per-target independence RUL-171/236 requires of a hand-initiated run. Asserted
// through the store rather than a report because an event-fired run has no
// caller to answer: a SECOND action of the same rule, in scope, must still run.
func TestAnEventFiredRunStillPerformsItsInScopeActions(t *testing.T) {
	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher))
	nodeA := e.placementNode(t)
	e.seedPlacementNodes(t, eventScopeNodeB)

	inside := mintSignageScreen(t, e, nodeA, "Lobby A")
	// Authored at A alongside the other target, then moved away — the same
	// after-the-fact reorganisation TestAnEventFiredRunCannotActOutsideItsOwnScope
	// uses, because authoring now refuses the direct spelling.
	outside := mintSignageScreen(t, e, nodeA, "Boardroom")
	castID := mintSignageCast(t, e, nodeA, "Service Requested")

	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", mustJSON(t, map[string]any{
		"name":       "Two Targets",
		"scope_node": nodeA,
		"enabled":    true,
		"mode":       "single",
		"triggers":   []any{map[string]any{"type": "event", "event": "screen.interaction"}},
		"actions": []any{
			map[string]any{"type": "play_cast", "screen_id": outside, "cast_id": castID},
			map[string]any{"type": "play_cast", "screen_id": inside, "cast_id": castID},
		},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create two-target automation: %d %s", resp.StatusCode, raw)
	}
	moveScreenToNode(t, e, outside, eventScopeNodeB)

	dispatcher.Deliver(context.Background(), interactionEnvelope(t, inside, "call_service"))

	if after := screenProgramOf(t, e, outside, fixedNowMs); after.Display != "blank" {
		t.Errorf("the out-of-scope target was acted on (showing %q)", after.Display)
	}
	if after := screenProgramOf(t, e, inside, fixedNowMs); after.Display != "content" {
		t.Errorf("one refused target stopped the rest of the rule: the in-scope screen shows %q, want content", after.Display)
	}
}

// moveScreenToNode re-places a screen row at another scope node, through the
// ordinary conditional PATCH an operator's own reorganisation takes.
//
// It is how these tests reach a state authoring now refuses to create directly:
// a stored rule naming a target outside its subtree. That state is reachable in
// production by exactly this route and several others (re-parenting a group,
// deleting a node, moving the rule) — none of which re-validate the rules that
// point at what moved, which is the whole argument for keeping the run's own
// per-target check.
func moveScreenToNode(t *testing.T, e *testEnv, screenID, node string) {
	t.Helper()
	get, raw := e.do(t, http.MethodGet, "/api/v1/screens/"+screenID, nil, nil)
	if get.StatusCode != http.StatusOK {
		t.Fatalf("read screen %s before moving it: %d %s", screenID, get.StatusCode, raw)
	}
	resp, body := e.do(t, http.MethodPatch, "/api/v1/screens/"+screenID,
		mustJSON(t, map[string]any{"scope_node": node}),
		map[string]string{"If-Match": get.Header.Get("ETag")})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("move screen %s to %s: %d %s", screenID, node, resp.StatusCode, body)
	}
}
