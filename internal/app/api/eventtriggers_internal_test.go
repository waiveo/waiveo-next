package api

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/rules/model"
)

// Unit-level guards for the two decisions the end-to-end cases in
// eventtriggers_e2e_test.go cannot separate: what an automation row's MISSING
// `enabled` flag means, and how a `match` constraint compares values.

// TestAutomationEnabledFailsClosed pins the direction of the default.
//
// Treating an ABSENT flag as enabled would make any row this decoder cannot
// understand FIRE — the wrong way round for a mechanism whose only failure mode
// is acting when it should not. The explicit-false case is what an operator's
// "disable" writes; the absent and unparseable cases are what a row from
// anywhere else looks like.
func TestAutomationEnabledFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"explicitly enabled", `{"enabled":true}`, true},
		{"explicitly disabled", `{"enabled":false}`, false},
		{"flag absent", `{"name":"x"}`, false},
		{"unparseable row", `not json`, false},
		{"flag null", `{"enabled":null}`, false},
	}
	for _, c := range cases {
		if got := automationEnabled([]byte(c.body)); got != c.want {
			t.Errorf("%s: automationEnabled(%s) = %v, want %v", c.name, c.body, got, c.want)
		}
	}
}

// TestMatchConstraintsCompareByJSONValue: a constraint holds when the payload's
// TOP-LEVEL field equals it as a JSON value — so a number written `2` in a rule
// matches `2` in a payload without this file having an opinion about float64,
// and an absent field never satisfies a constraint.
func TestMatchConstraintsCompareByJSONValue(t *testing.T) {
	payload := json.RawMessage(`{"interaction":"call_service","count":2,"nested":{"a":1}}`)

	hold := func(match string) bool {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(match), &m); err != nil {
			t.Fatalf("bad match fixture %s: %v", match, err)
		}
		return matchConstraintsHold(m, payload)
	}

	if !hold(`{}`) {
		t.Error("no constraints must match any event of that name")
	}
	if !hold(`{"interaction":"call_service"}`) {
		t.Error("an exact string constraint must hold")
	}
	if !hold(`{"count":2}`) {
		t.Error("a numeric constraint must hold by JSON value, not by Go type")
	}
	if !hold(`{"nested":{"a":1}}`) {
		t.Error("an object constraint must compare structurally")
	}
	if hold(`{"interaction":"other"}`) {
		t.Error("a different value must not match")
	}
	if hold(`{"interation":"call_service"}`) {
		t.Error("a constraint on a field the payload does not carry must FAIL, not be ignored — ignoring it silently widens the rule to every event of that schema")
	}
	if hold(`{"interaction":"call_service","count":3}`) {
		t.Error("EVERY constraint must hold, not merely one")
	}
}

// TestRuleMatchesEventOnlyOnItsOwnSchema: a rule fires once per matching event,
// and only on the durable event name it names.
func TestRuleMatchesEventOnlyOnItsOwnSchema(t *testing.T) {
	rule, err := model.ParseRule([]byte(`{
	  "id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2",
	  "triggers":[{"type":"event","event":"screen.interaction","match":{"interaction":"call_service"}}],
	  "actions":[{"type":"log","message":"x"}]
	}`))
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	env := func(schema, interaction string) events.Envelope {
		return events.Envelope{Schema: schema, Payload: json.RawMessage(`{"interaction":"` + interaction + `"}`)}
	}
	if !ruleMatchesEvent(rule, env(events.SchemaScreenInteraction, "call_service")) {
		t.Error("the named event with a satisfied match must fire")
	}
	if ruleMatchesEvent(rule, env(events.SchemaScreenInteraction, "other")) {
		t.Error("an unsatisfied match must not fire")
	}
	if ruleMatchesEvent(rule, env(events.SchemaDeviceHeartbeat, "call_service")) {
		t.Error("another schema must not fire a rule that names screen.interaction")
	}

	// A rule with no event trigger at all — the ordinary state-triggered rule
	// every deployment is full of — must never be fired by an event.
	stateRule, err := model.ParseRule([]byte(`{
	  "id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z3",
	  "triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z4","to":["on"]}],
	  "actions":[{"type":"log","message":"x"}]
	}`))
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	if ruleMatchesEvent(stateRule, env(events.SchemaScreenInteraction, "call_service")) {
		t.Error("a rule with no event trigger must never be fired by an event")
	}
}

// TestAutomationScopeViewIsBoundedByTheAutomationsOwnSubtree pins the AUTHORITY
// an event-fired run executes under, member by member.
//
// The end-to-end cases (eventtriggers_scope_test.go) can only observe the
// OUTCOME — did the screen change — and canWrite alone decides that. So a view
// whose canRead said yes everywhere while canWrite was correctly bounded would
// pass all of them, and it is not a harmless difference: canRead is what a
// selector's candidate set is filtered by, so a permissive one lets a rule at
// one node ENUMERATE every screen in the deployment and name the out-of-reach
// ones in its run record. This is the only place that half can be seen.
func TestAutomationScopeViewIsBoundedByTheAutomationsOwnSubtree(t *testing.T) {
	// org
	//  └── site
	//       ├── nodeA  (the automation's placement)
	//       │    └── nodeADeep
	//       └── nodeB  (a sibling)
	const (
		org      = "01J8Z0V1EW00000000000000RG"
		site     = "01J8Z0V1EW0000000000000S1T"
		nodeA    = "01J8Z0V1EW00000000000000AA"
		deep     = "01J8Z0V1EW0000000000000AA1"
		nodeB    = "01J8Z0V1EW00000000000000BB"
		nowhere  = "01J8Z0V1EW00000000000000ZZ" // in no tree at all
		emptyStr = ""
	)
	strp := func(s string) *string { return &s }
	tree, _ := datamodel.BuildScopeTree([]datamodel.ScopeNode{
		{ID: org, Kind: "org", Name: "Org"},
		{ID: site, Kind: "site", ParentID: strp(org), Name: "Site"},
		{ID: nodeA, Kind: "screen", ParentID: strp(site), Name: "A"},
		{ID: deep, Kind: "screen", ParentID: strp(nodeA), Name: "A deep"},
		{ID: nodeB, Kind: "screen", ParentID: strp(site), Name: "B"},
	})

	view := automationScopeView(tree, nodeA)

	for _, c := range []struct {
		node string
		want bool
		why  string
	}{
		{nodeA, true, "the automation's own node — exactly the authority its author had to hold to create the row"},
		{deep, true, "a DESCENDANT: SEC-010 inherits a binding down the tree, so the rule reaches what its author could reach"},
		{nodeB, false, "a SIBLING: authoring a rule at one node must not be a way to act at another"},
		{site, false, "an ANCESTOR: write authority does not travel upwards"},
		{org, false, "the root: likewise"},
		{nowhere, false, "a node the tree does not contain — unknown must fail closed (SEC-005)"},
		{emptyStr, false, "an unplaced row: no scope node authorizes nothing, never everything"},
	} {
		if got := view.canRead(c.node); got != c.want {
			t.Errorf("canRead(%s) = %v, want %v — %s", c.node, got, c.want, c.why)
		}
		if got := view.canWrite(c.node); got != c.want {
			t.Errorf("canWrite(%s) = %v, want %v — %s", c.node, got, c.want, c.why)
		}
		role, ok := view.roleAt(c.node)
		if ok != c.want {
			t.Errorf("roleAt(%s) resolved = %v, want %v — %s", c.node, ok, c.want, c.why)
		}
		// OPERATOR, not owner. The view mirrors the authority the rule's author
		// had to hold, and POST /automations requires canWrite, which auth.CanWrite
		// grants from `operator` upward. Answering `owner` handed the run three
		// levels more than the write that created it was checked for.
		if c.want && role != auth.RoleOperator {
			t.Errorf("roleAt(%s) = %q, want %q inside the subtree — a mirrored authority must not exceed the one it mirrors",
				c.node, role, auth.RoleOperator)
		}
	}

	// A node the tree DOES NOT CONTAIN is refused even when it is the
	// automation's own placement. The `node == scopeNode` arm used to be a
	// string comparison that asked the tree nothing, so a rule whose placement
	// had since been deleted still authorized itself there — "unknown fails
	// closed" with a hole at the one node a rule reaches most.
	deletedPlacement := automationScopeView(tree, nowhere)
	if deletedPlacement.canWrite(nowhere) {
		t.Error("an automation placed at a node the tree does not contain can still write at that node; " +
			"a deleted scope_node must fail closed like any other unknown node (SEC-005)")
	}
	if _, ok := deletedPlacement.roleAt(nowhere); ok {
		t.Error("roleAt resolved a binding at an automation's own deleted placement node")
	}

	// inSubtree is the tree's real containment relation and stays strict, so a
	// `scope_node subtree` selector term still means what it means everywhere
	// else on this surface (the node itself is the selector's own business).
	if !view.inSubtree(site, nodeA) {
		t.Error("inSubtree(site, nodeA) = false; the view is not answering over the real tree")
	}
	if view.inSubtree(nodeA, nodeA) {
		t.Error("inSubtree(nodeA, nodeA) = true; containment is STRICT, and a selector term depends on it")
	}
	if view.inSubtree(nodeB, deep) {
		t.Error("inSubtree(nodeB, deep) = true; the two are in different branches")
	}

	// An automation carrying NO placement authorizes nothing. This is the
	// fail-closed direction for a row shape the surface refuses to create but a
	// seed or a restore could still produce.
	unplaced := automationScopeView(tree, "")
	for _, node := range []string{org, site, nodeA, deep, nodeB} {
		if unplaced.canRead(node) || unplaced.canWrite(node) {
			t.Errorf("an automation with no scope_node can reach %s; an unplaced rule must authorize nothing", node)
		}
	}
}

// TestAutomationSubtreeBoundRefusesEitherEndTheTreeDoesNotHold is the guard that
// shipped without one: the both-ends presence test in automationSubtreeBound.
//
// The view-level case above covers one cell of it — a rule whose own placement
// is gone cannot write AT that placement. This is the whole matrix, asserted
// directly on the predicate both the run view and the authoring check are built
// from, because "a deleted node is not a node this rule may reach" is a claim
// about four situations and the string-comparison bug it replaced was in exactly
// one of them: node == scopeNode, where the old code answered from the two
// strings and asked the tree nothing.
//
// The state is not reachable through the api today — scopeNodeDeleteGuards'
// DAT-021 arm refuses to delete a node an automation is placed at — so this is a
// defence-in-depth rule, and that is precisely why it needs the test. A seed, a
// workspace restore, or a future relaxation of that delete rule reaches it with
// nothing in between, and a fail-closed guard nobody exercises is a guard
// nobody notices the loss of.
func TestAutomationSubtreeBoundRefusesEitherEndTheTreeDoesNotHold(t *testing.T) {
	const (
		org     = "01J8Z0V1EW00000000000000RG"
		site    = "01J8Z0V1EW0000000000000S1T"
		nodeA   = "01J8Z0V1EW00000000000000AA"
		deep    = "01J8Z0V1EW0000000000000AA1"
		deleted = "01J8Z0V1EW00000000000000DL" // never in the tree: a node since removed
	)
	strp := func(s string) *string { return &s }
	tree, _ := datamodel.BuildScopeTree([]datamodel.ScopeNode{
		{ID: org, Kind: "org", Name: "Org"},
		{ID: site, Kind: "site", ParentID: strp(org), Name: "Site"},
		{ID: nodeA, Kind: "screen", ParentID: strp(site), Name: "A"},
		{ID: deep, Kind: "screen", ParentID: strp(nodeA), Name: "A deep"},
	})

	// An orphan: its parent_id names the deleted node, so it is the row a cascade
	// left behind. It must NOT become reachable by a rule placed at the node that
	// is gone — that would make deleting a scope node a way to widen a rule.
	const orphan = "01J8Z0V1EW00000000000000OR"
	orphaned, _ := datamodel.BuildScopeTree([]datamodel.ScopeNode{
		{ID: org, Kind: "org", Name: "Org"},
		{ID: site, Kind: "site", ParentID: strp(org), Name: "Site"},
		{ID: orphan, Kind: "screen", ParentID: strp(deleted), Name: "Orphan"},
	})

	for _, c := range []struct {
		name      string
		tree      datamodel.ScopeTree
		scopeNode string
		node      string
		want      bool
		why       string
	}{
		{"both ends present, same node", tree, nodeA, nodeA, true,
			"the control: the rule's own node is reachable, or the whole feature is a no-op"},
		{"both ends present, descendant", tree, nodeA, deep, true,
			"the other control: SEC-010 inherits downward and the rule mirrors that"},
		{"placement deleted, target present", tree, deleted, nodeA, false,
			"a rule whose placement is gone mirrors no authority at all"},
		{"placement present, target deleted", tree, nodeA, deleted, false,
			"a target row pointing at a node the tree no longer holds is not in anyone's subtree"},
		{"BOTH ends the deleted node", tree, deleted, deleted, false,
			"THE case the string comparison waved through: node == scopeNode answered true from two " +
				"strings without asking the tree, so a rule whose placement had been deleted still " +
				"authorized itself there — unknown-fails-closed with a hole at the node a rule uses most"},
		{"placement deleted, orphan still points at it", orphaned, deleted, orphan, false,
			"a cascade that left a child pointing at a removed parent must not make that child " +
				"reachable from the removed parent"},
	} {
		if got := automationSubtreeBound(c.tree, c.scopeNode)(c.node); got != c.want {
			t.Errorf("%s: bound(scopeNode=%s)(%s) = %v, want %v — %s", c.name, c.scopeNode, c.node, got, c.want, c.why)
		}
	}
}
