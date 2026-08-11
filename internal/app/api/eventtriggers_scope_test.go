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

// TestAnEventFiredRunCannotActOutsideItsOwnScope is the escalation, in the
// shape an operator could actually reach it: the rule is placed at node A and
// its action names, by id, a screen at node B.
func TestAnEventFiredRunCannotActOutsideItsOwnScope(t *testing.T) {
	dispatcher := &api.EventTriggerDispatcher{}
	e := newEnvWithOptions(t, api.WithEventTriggers(dispatcher))
	nodeA := e.placementNode(t)
	e.seedPlacementNodes(t, eventScopeNodeB)

	// The screen the rule reaches for is at B; the rule itself is at A.
	outsideScreen := mintSignageScreen(t, e, eventScopeNodeB, "Boardroom")
	castID := mintSignageCast(t, e, nodeA, "Service Requested")

	mintEventAutomation(t, e, nodeA, true,
		map[string]any{"interaction": "call_service"},
		map[string]any{"type": "play_cast", "screen_id": outsideScreen, "cast_id": castID})

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
	outside := mintSignageScreen(t, e, eventScopeNodeB, "Boardroom")
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

	dispatcher.Deliver(context.Background(), interactionEnvelope(t, inside, "call_service"))

	if after := screenProgramOf(t, e, outside, fixedNowMs); after.Display != "blank" {
		t.Errorf("the out-of-scope target was acted on (showing %q)", after.Display)
	}
	if after := screenProgramOf(t, e, inside, fixedNowMs); after.Display != "content" {
		t.Errorf("one refused target stopped the rest of the rule: the in-scope screen shows %q, want content", after.Display)
	}
}
