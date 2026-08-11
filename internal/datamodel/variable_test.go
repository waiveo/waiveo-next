package datamodel

import (
	"encoding/json"
	"testing"
)

// variable_test.go exercises data-model/1's Variables section (DAT-130–138).
//
// Every case here asserts on the ERROR CODE, never on message prose: the codes
// are the published field-level register a client branches on, and a test that
// matched the sentence would pass a change that renamed the code.

func ptr(s string) *string { return &s }

// tree builds a scope tree from (id, parent) pairs. Parent "" means root.
// Kind/name/geo are filled with values BuildScopeTree accepts so the tree the
// resolution cases walk is a real validated one, not a hand-stuffed index.
func tree(t *testing.T, pairs ...[2]string) ScopeTree {
	t.Helper()
	nodes := make([]ScopeNode, 0, len(pairs))
	for _, p := range pairs {
		n := ScopeNode{ID: p[0], Name: "n-" + p[0], Kind: "group"}
		if p[1] == "" {
			n.Kind = "org"
		} else {
			n.ParentID = ptr(p[1])
		}
		nodes = append(nodes, n)
	}
	tr, _ := BuildScopeTree(nodes)
	return tr
}

func v(id, name, node string, value any) Variable {
	return Variable{ID: id, Name: name, ScopeNode: node, Value: value}
}

// --- DAT-131a: the name grammar ---------------------------------------------

func TestVariableName_GrammarDAT131a(t *testing.T) {
	valid := []string{"a", "guest_mode", "store_open", "x9", "a_1_b", stringOfLen(64)}
	for _, n := range valid {
		if !ValidVariableName(n) {
			t.Errorf("ValidVariableName(%q) = false; DAT-131a admits it", n)
		}
	}
	invalid := []string{
		"",              // absent
		"Guest_mode",    // uppercase
		"9lives",        // leading digit
		"_leading",      // leading underscore
		"has-hyphen",    // hyphen is a label-key char, deliberately not this grammar
		"has space",     // whitespace would need quoting in pack text
		"has.dot",       // would read as a path expression in a closure
		stringOfLen(65), // one over the bound
		"trailingé",     // non-ASCII
	}
	for _, n := range invalid {
		if ValidVariableName(n) {
			t.Errorf("ValidVariableName(%q) = true; DAT-131a refuses it", n)
		}
	}
}

func stringOfLen(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

// --- DAT-132/133: the value rule --------------------------------------------

// A string, a number and a boolean are settable; an object, an array and null
// are not. The null case is DAT-133 and is the one a reader is most likely to
// get wrong, because null IS a JSON scalar — it is refused because events/1
// EVT-084 already spent it on "unset".
func TestVariableValue_ScalarsOnly_DAT132_DAT133(t *testing.T) {
	cases := []struct {
		body     string
		wantCode string // "" = accepted
	}{
		{`{"name":"a","value":"open"}`, ""},
		{`{"name":"a","value":42}`, ""},
		{`{"name":"a","value":-1.5}`, ""},
		{`{"name":"a","value":true}`, ""},
		{`{"name":"a","value":false}`, ""},
		{`{"name":"a","value":null}`, "VARIABLE_VALUE_INVALID"},
		{`{"name":"a","value":{"k":1}}`, "VARIABLE_VALUE_INVALID"},
		{`{"name":"a","value":[1,2]}`, "VARIABLE_VALUE_INVALID"},
		{`{"name":"a"}`, "VARIABLE_VALUE_INVALID"}, // absent is the same request as null
	}
	for _, c := range cases {
		errs := ValidateVariableBody([]byte(c.body))
		got := ""
		for _, e := range errs {
			if e.Field == "value" || e.Field == "" {
				got = e.Code
			}
		}
		if got != c.wantCode {
			t.Errorf("ValidateVariableBody(%s): value code = %q, want %q (errs=%+v)", c.body, got, c.wantCode, errs)
		}
	}
}

// A bad name and a bad value are reported TOGETHER, not one at a time. An
// operator fixing one and resubmitting to discover the other is the shape a
// per-field register exists to avoid.
func TestVariableValidate_ReportsEveryViolatedRuleAtOnce(t *testing.T) {
	errs := ValidateVariableBody([]byte(`{"name":"Bad Name","value":{"o":1}}`))
	codes := map[string]bool{}
	for _, e := range errs {
		codes[e.Code] = true
	}
	if !codes["VARIABLE_NAME_INVALID"] || !codes["VARIABLE_VALUE_INVALID"] {
		t.Fatalf("both rules must be reported at once; got %+v", errs)
	}
}

// A name at the wrong JSON TYPE is refused naming itself, never read as absent
// — the same rule model.DecodeSignage enforces for a signage member.
func TestVariableValidate_WrongTypedNameIsRefusedNotIgnored(t *testing.T) {
	errs := ValidateVariableBody([]byte(`{"name":7,"value":"x"}`))
	if len(errs) != 1 || errs[0].Code != "VARIABLE_NAME_INVALID" || errs[0].Field != "name" {
		t.Fatalf("a numeric name must be VARIABLE_NAME_INVALID on field name; got %+v", errs)
	}
}

// --- DAT-134: nearest-ancestor resolution -----------------------------------

// The node's OWN row wins over an ancestor's.
func TestEffectiveVariable_OwnRowBeatsAncestor_DAT134(t *testing.T) {
	tr := tree(t, [2]string{"org", ""}, [2]string{"site", "org"}, [2]string{"grp", "site"})
	rows := []Variable{
		v("01A", "store_open", "site", false),
		v("01B", "store_open", "grp", true),
	}
	got, ok := EffectiveVariable(tr, "grp", "store_open", rows)
	if !ok || got.ID != "01B" || got.Value != true {
		t.Fatalf("the node's own row must win (DAT-134); got %+v ok=%v", got, ok)
	}
}

// With no row of its own, a node resolves through the NEAREST ancestor that has
// one — not the root, and not the first row in the slice.
func TestEffectiveVariable_NearestAncestorWins_DAT134(t *testing.T) {
	tr := tree(t, [2]string{"org", ""}, [2]string{"site", "org"}, [2]string{"grp", "site"}, [2]string{"leaf", "grp"})
	rows := []Variable{
		v("01A", "store_open", "org", "org-value"),
		v("01B", "store_open", "site", "site-value"),
	}
	got, ok := EffectiveVariable(tr, "leaf", "store_open", rows)
	if !ok || got.Value != "site-value" {
		t.Fatalf("the NEAREST ancestor must win, not the root (DAT-134); got %+v ok=%v", got, ok)
	}
}

// A name nothing on the chain declares does not resolve. It must not fall back
// to a row placed on a DIFFERENT branch, which is the failure a "search all
// rows" implementation has.
func TestEffectiveVariable_OffChainRowNeverResolves_DAT134(t *testing.T) {
	tr := tree(t, [2]string{"org", ""}, [2]string{"a", "org"}, [2]string{"b", "org"})
	rows := []Variable{v("01A", "store_open", "b", "b-value")}
	if got, ok := EffectiveVariable(tr, "a", "store_open", rows); ok {
		t.Fatalf("a sibling branch's row must not resolve at a; got %+v", got)
	}
}

// --- DAT-135: resolution is PER NAME ----------------------------------------

// The rule DAT-135 exists to state, as an assertion: a node overriding ONE name
// must not shadow an ancestor's OTHER names. An implementation that generalized
// DAT-033's resolve-together-as-a-unit rule would return only `store_open` here.
func TestEffectiveVariables_PerNameNotPerNode_DAT135(t *testing.T) {
	tr := tree(t, [2]string{"org", ""}, [2]string{"site", "org"}, [2]string{"grp", "site"})
	rows := []Variable{
		v("01A", "store_open", "site", false),
		v("01B", "holiday_mode", "site", "none"),
		v("01C", "store_open", "grp", true),
	}
	got := EffectiveVariables(tr, "grp", rows)

	if got["store_open"] != true {
		t.Errorf("store_open must resolve to the grp override; got %v", got["store_open"])
	}
	if got["holiday_mode"] != "none" {
		t.Errorf("DAT-135: overriding store_open at grp must NOT shadow the site's holiday_mode; got %v (whole env %v)", got["holiday_mode"], got)
	}
	if len(got) != 2 {
		t.Errorf("both names must be visible at grp; got %v", got)
	}
}

// --- DAT-136: deleting an override re-exposes the ancestor ------------------

// Stated as a before/after over the SAME resolver rather than as a comment: the
// deletion removes an override, it does not unset the name in the subtree.
func TestEffectiveVariables_DeletingAnOverrideReExposesTheAncestor_DAT136(t *testing.T) {
	tr := tree(t, [2]string{"org", ""}, [2]string{"site", "org"}, [2]string{"grp", "site"})
	withOverride := []Variable{
		v("01A", "store_open", "site", "site-value"),
		v("01B", "store_open", "grp", "grp-value"),
	}
	if got := EffectiveVariables(tr, "grp", withOverride)["store_open"]; got != "grp-value" {
		t.Fatalf("precondition: the override must be effective; got %v", got)
	}

	// The delete: the grp row is gone, the site row is not.
	afterDelete := []Variable{v("01A", "store_open", "site", "site-value")}
	got := EffectiveVariables(tr, "grp", afterDelete)
	if _, present := got["store_open"]; !present {
		t.Fatalf("DAT-136: deleting the override must RE-EXPOSE the ancestor's row, not unset the name; got %v", got)
	}
	if got["store_open"] != "site-value" {
		t.Fatalf("DAT-136: the re-exposed value must be the ancestor's; got %v", got["store_open"])
	}
}

// --- determinism -------------------------------------------------------------

// Two rows sharing a name AND a node cannot both be stored (DAT-131 is enforced
// at write), but a resolver handed them anyway must answer the SAME way every
// time — a value that varied with slice order would make one rule fire
// differently on two boxes holding identical rows.
func TestEffectiveVariable_DuplicateRowsResolveDeterministically(t *testing.T) {
	tr := tree(t, [2]string{"org", ""})
	forward := []Variable{v("01B", "n", "org", "second"), v("01A", "n", "org", "first")}
	reverse := []Variable{v("01A", "n", "org", "first"), v("01B", "n", "org", "second")}
	a, _ := EffectiveVariable(tr, "org", "n", forward)
	b, _ := EffectiveVariable(tr, "org", "n", reverse)
	if a.ID != b.ID {
		t.Fatalf("resolution must not depend on slice order; got %s vs %s", a.ID, b.ID)
	}
	if a.ID != "01A" {
		t.Fatalf("the lowest id must win deterministically; got %s", a.ID)
	}
}

// An unknown node yields an EMPTY, NON-NIL map — a caller passes it straight
// into an eval.ActionContext with no nil check.
func TestEffectiveVariables_UnknownNodeYieldsEmptyNonNilMap(t *testing.T) {
	tr := tree(t, [2]string{"org", ""})
	got := EffectiveVariables(tr, "nosuchnode", []Variable{v("01A", "n", "org", 1)})
	if got == nil {
		t.Fatal("must be non-nil so a caller needs no nil check")
	}
	if len(got) != 0 {
		t.Fatalf("an unknown node resolves nothing; got %v", got)
	}
}

// The round trip a store read performs: a value decoded out of JSON must still
// satisfy the scalar rule, so a number arriving as float64 is not refused.
func TestVariableValue_SurvivesAJSONRoundTrip(t *testing.T) {
	var decoded any
	if err := json.Unmarshal([]byte(`42`), &decoded); err != nil {
		t.Fatal(err)
	}
	if !ValidVariableValue(decoded) {
		t.Fatalf("a JSON-decoded number (%T) must be a settable value", decoded)
	}
}
