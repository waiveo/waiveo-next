package events

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
)

// filter_test.go drives the per-subscriber delivery predicate (EVT-120–124) as a
// pure value. The transport-level proof — that the /events/v1 handler actually
// consults it, on both the replayed backlog and the live tail — lives in
// internal/app/eventsse; what is pinned here is the ALGEBRA, especially the two
// properties EVT-121/122 turn on: the selector is a conjunct that cannot widen,
// and an out-of-reach term is an empty result rather than an error (this type has
// no error channel at all, which is that requirement made structural).

const (
	visibleNode = "01J8Z6A0B1C2D3E4F5G6H7ZV10"
	hiddenNode  = "01J8Z6A0B1C2D3E4F5G6H7ZH10"
	childNode   = "01J8Z6A0B1C2D3E4F5G6H7ZCH0"
)

// visibleOnly is the visible set of a principal that may read visibleNode and
// its child, and nothing else.
func visibleOnly(node string) bool { return node == visibleNode || node == childNode }

// underVisible reports childNode as lying strictly below visibleNode; nothing
// else is below anything.
func underVisible(ancestor, node string) bool {
	return ancestor == visibleNode && node == childNode
}

func placedEnv(scopeNode, schema string) Envelope {
	return Envelope{ID: "01J8Z6A0B1C2D3E4F5G6H7ZEV0", Schema: schema, ScopeNode: scopeNode}
}

func mustParse(t *testing.T, s string) apiselector.Selector {
	t.Helper()
	sel, perr := apiselector.Parse(s)
	if perr != nil {
		t.Fatalf("parsing selector %q: %v", s, perr)
	}
	return sel
}

// TestFilter_ZeroValueDeniesEverything: SEC-005's never-default-permit rule made
// structural. A Filter that was never given a visible set must not be a Filter
// that delivers the whole world.
func TestFilter_ZeroValueDeniesEverything(t *testing.T) {
	var f Filter
	if f.Allows(placedEnv(visibleNode, SchemaAutomationRun)) {
		t.Fatal("the zero Filter carries no visible set and MUST deliver nothing (SEC-005)")
	}
	if NewFilter(nil, apiselector.Selector{}, nil, nil).Allows(placedEnv(visibleNode, SchemaAutomationRun)) {
		t.Fatal("a nil canRead MUST deny every envelope, never permit every one (SEC-005)")
	}
}

// TestFilter_VisibleSetIsTheDefault is EVT-120: absent a narrowing selector, the
// visible set alone decides, and an envelope placed outside it is not delivered.
func TestFilter_VisibleSetIsTheDefault(t *testing.T) {
	f := NewFilter(visibleOnly, apiselector.Selector{}, underVisible, nil)
	if !f.Allows(placedEnv(visibleNode, SchemaAutomationRun)) {
		t.Fatal("an envelope inside the visible set must be delivered (EVT-120)")
	}
	if f.Allows(placedEnv(hiddenNode, SchemaAutomationRun)) {
		t.Fatal("an envelope whose scope_node falls outside the visible set MUST NOT be delivered (EVT-120)")
	}
}

// TestFilter_SelectorOnlyNarrows is EVT-121: a selector may remove envelopes the
// visible set admitted, and may never add one it did not.
func TestFilter_SelectorOnlyNarrows(t *testing.T) {
	// Narrowing: the subtree term names visibleNode, so the child is admitted and
	// nothing else changes.
	narrow := NewFilter(visibleOnly, mustParse(t, "scope_node subtree "+visibleNode), underVisible, nil)
	if !narrow.Allows(placedEnv(childNode, SchemaAutomationRun)) {
		t.Fatal("a subtree term must admit a descendant inside the visible set (API-044)")
	}
	if !narrow.Allows(placedEnv(visibleNode, SchemaAutomationRun)) {
		t.Fatal("a subtree term includes the NAMED node itself (API-044)")
	}

	// Narrowing that actually removes something: an exact-node term inside the
	// visible set excludes the sibling placement the visible set admits.
	exact := NewFilter(visibleOnly, mustParse(t, "scope_node="+childNode), underVisible, nil)
	if exact.Allows(placedEnv(visibleNode, SchemaAutomationRun)) {
		t.Fatal("an exact-node term restricts to that node only, with no subtree expansion (API-044)")
	}
	if !exact.Allows(placedEnv(childNode, SchemaAutomationRun)) {
		t.Fatal("an exact-node term must admit the named node")
	}

	// Widening is structurally impossible: naming the hidden node cannot deliver
	// it, because canRead is an independent conjunct.
	for _, selector := range []string{
		"scope_node=" + hiddenNode,
		"scope_node subtree " + hiddenNode,
	} {
		wide := NewFilter(visibleOnly, mustParse(t, selector), underVisible, nil)
		if wide.Allows(placedEnv(hiddenNode, SchemaAutomationRun)) {
			t.Fatalf("selector %q MUST NOT widen delivery to a scope node the principal cannot read (EVT-121)", selector)
		}
	}
}

// TestFilter_OutOfReachTermMatchesNothing is EVT-122: a term resolving outside the
// readable set matches nothing "exactly as an ordinary empty-result filter
// would". There is nowhere for an error to go — Allows returns a bool — so what
// is pinned here is that it also does not accidentally MATCH.
func TestFilter_OutOfReachTermMatchesNothing(t *testing.T) {
	f := NewFilter(visibleOnly, mustParse(t, "scope_node subtree "+hiddenNode), underVisible, nil)
	for _, node := range []string{visibleNode, childNode, hiddenNode} {
		if f.Allows(placedEnv(node, SchemaAutomationRun)) {
			t.Fatalf("a wholly out-of-reach subtree term must match nothing; it matched an envelope at %s (EVT-122)", node)
		}
	}
}

// TestFilter_PartlyOutOfReachTermKeepsItsInReachPart: EVT-122's "wholly or in
// part" is about the TERM, not about disabling the whole term — the in-reach part
// still matches, which is the intersection api/1's own list surface already
// answers with.
func TestFilter_PartlyOutOfReachTermKeepsItsInReachPart(t *testing.T) {
	// A subtree term naming a node that is itself unreadable but whose descendant
	// the principal CAN read: only the readable descendant is delivered.
	above := "01J8Z6A0B1C2D3E4F5G6H7ZAB0"
	inSubtree := func(ancestor, node string) bool {
		return ancestor == above && (node == visibleNode || node == hiddenNode)
	}
	f := NewFilter(visibleOnly, mustParse(t, "scope_node subtree "+above), inSubtree, nil)
	if !f.Allows(placedEnv(visibleNode, SchemaAutomationRun)) {
		t.Fatal("the IN-REACH part of a partly out-of-reach term must still match (EVT-121/122)")
	}
	if f.Allows(placedEnv(hiddenNode, SchemaAutomationRun)) {
		t.Fatal("the out-of-reach part must still match nothing (EVT-122)")
	}
}

// TestFilter_SchemasIsAnAdditionalRestriction is EVT-124: applied ALONGSIDE
// scope-node filtering, never in place of it.
func TestFilter_SchemasIsAnAdditionalRestriction(t *testing.T) {
	f := NewFilter(visibleOnly, apiselector.Selector{}, underVisible, []string{SchemaAutomationRun, SchemaBoxVitals})
	if !f.Allows(placedEnv(visibleNode, SchemaAutomationRun)) {
		t.Fatal("a listed schema inside the visible set must be delivered (EVT-124)")
	}
	if f.Allows(placedEnv(visibleNode, SchemaContentPlayed)) {
		t.Fatal("a schema outside the supplied list MUST NOT be delivered (EVT-124)")
	}
	if f.Allows(placedEnv(hiddenNode, SchemaAutomationRun)) {
		t.Fatal("a schemas match MUST NOT substitute for scope-node filtering (EVT-124)")
	}

	// An empty list imposes nothing; an unrecognized name is simply a member no
	// event matches.
	none := NewFilter(visibleOnly, apiselector.Selector{}, underVisible, nil)
	if !none.Allows(placedEnv(visibleNode, SchemaContentPlayed)) {
		t.Fatal("an empty schemas list must impose no restriction (EVT-124)")
	}
	unknown := NewFilter(visibleOnly, apiselector.Selector{}, underVisible, []string{"no.such_schema"})
	if unknown.Allows(placedEnv(visibleNode, SchemaAutomationRun)) {
		t.Fatal("a list naming only unrecognized schemas must match nothing")
	}
}

// TestFilter_LabelTermsEvaluateAgainstTheEmptyLabelSet pins the reading of the
// grammar this file's package header states: a durable-event envelope carries no
// labels (EVT-010 fixes its field set), so a POSITIVE label term matches nothing
// and a NEGATIVE one matches everything — the ordinary answer for a resource with
// no labels, and in neither direction able to widen past canRead.
func TestFilter_LabelTermsEvaluateAgainstTheEmptyLabelSet(t *testing.T) {
	positive := map[string]string{
		"equality":       "kind=screen",
		"set-membership": "kind in (screen,site)",
		"existence":      "kind",
	}
	for name, selector := range positive {
		f := NewFilter(visibleOnly, mustParse(t, selector), underVisible, nil)
		if f.Allows(placedEnv(visibleNode, SchemaAutomationRun)) {
			t.Fatalf("%s: a positive label term must match nothing against an envelope that carries no labels", name)
		}
	}
	negative := map[string]string{
		"inequality":     "kind!=screen",
		"set-exclusion":  "kind notin (screen)",
		"non-existence":  "!kind",
		"empty selector": "",
	}
	for name, selector := range negative {
		f := NewFilter(visibleOnly, mustParse(t, selector), underVisible, nil)
		if !f.Allows(placedEnv(visibleNode, SchemaAutomationRun)) {
			t.Fatalf("%s: a negative label term matches an envelope that carries no labels", name)
		}
		if f.Allows(placedEnv(hiddenNode, SchemaAutomationRun)) {
			t.Fatalf("%s: a negative label term still MUST NOT widen past the visible set (EVT-121)", name)
		}
	}
}
