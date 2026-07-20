package apiselector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// loadCorpus reads a frozen api-1 corpus case relative to this package. The
// corpus JSON is the oracle: the tests read their expectations from the file,
// never from values re-typed here.
func loadCorpus(t *testing.T, name string, into any) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "api-1", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus case %s: %v", name, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("decode corpus case %s: %v", name, err)
	}
}

// subtreeCorpus is the slice of API-044 this test drives: a scope-node tree
// (id/kind/parent_id per row), the list request's selector, and the pinned set
// of ids the selector must select.
type subtreeCorpus struct {
	CaseID string `json:"case_id"`
	Input  struct {
		CollectionState []struct {
			ID       string `json:"id"`
			Kind     string `json:"kind"`
			ParentID string `json:"parent_id"`
		} `json:"collection_state"`
		Request struct {
			Query map[string]string `json:"query"`
		} `json:"request"`
	} `json:"input"`
	Expected struct {
		Status int `json:"status"`
		Body   struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		} `json:"body"`
	} `json:"expected"`
}

// TestSelectorScopeSubtreeCorpus drives API-044: a selector combining an
// ordinary equality term with a scope_node subtree term ANDs them — only
// resources of the named kind AND placed at-or-under the named scope node
// match, even though a same-kind resource exists under a sibling subtree, and a
// wrong-kind resource exists inside the named subtree.
func TestSelectorScopeSubtreeCorpus(t *testing.T) {
	var c subtreeCorpus
	loadCorpus(t, "API-044-valid-selector-scope-subtree.json", &c)

	sel, perr := Parse(c.Input.Request.Query["selector"])
	if perr != nil {
		t.Fatalf("Parse(%q) = error %+v, want a valid selector", c.Input.Request.Query["selector"], perr)
	}

	// The placement tree: id -> parent_id. inSubtree walks a node's ancestry to
	// decide whether it is a (strict) descendant of ancestor — exactly the
	// containment predicate a real handler resolves against the scope-node tree.
	parent := map[string]string{}
	for _, row := range c.Input.CollectionState {
		parent[row.ID] = row.ParentID
	}
	inSubtree := func(ancestor, node string) bool {
		for cur := node; ; {
			p, ok := parent[cur]
			if !ok {
				return false
			}
			if p == ancestor {
				return true
			}
			cur = p
		}
	}

	var got []string
	for _, row := range c.Input.CollectionState {
		// A scope node's placement IS its own id; its labels carry `kind`.
		if sel.Matches(map[string]string{"kind": row.Kind}, row.ID, inSubtree) {
			got = append(got, row.ID)
		}
	}

	want := make([]string, 0, len(c.Expected.Body.Items))
	for _, it := range c.Expected.Body.Items {
		want = append(want, it.ID)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("selected %v, want %v (corpus-pinned)", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("selected %v, want %v (corpus-pinned)", got, want)
		}
	}

	// Spell out the two exclusion reasons the corpus pins: the wrong-kind node
	// inside the subtree, and the right-kind node under the sibling site.
	for _, id := range got {
		for _, row := range c.Input.CollectionState {
			if row.ID == id && row.Kind != "screen" {
				t.Errorf("selected %s of kind %q, want only screen-kind", id, row.Kind)
			}
		}
	}
}

// malformedCorpus is the slice of API-045 this test drives: a selector with
// whitespace around the equality operator and the exact Problem body (status,
// title, detail, code) the corpus pins.
type malformedCorpus struct {
	CaseID string `json:"case_id"`
	Input  struct {
		Request struct {
			Query map[string]string `json:"query"`
		} `json:"request"`
	} `json:"input"`
	Expected struct {
		Status int `json:"status"`
		Body   struct {
			Type   string `json:"type"`
			Title  string `json:"title"`
			Status int    `json:"status"`
			Detail string `json:"detail"`
			Code   string `json:"code"`
		} `json:"body"`
	} `json:"expected"`
}

// TestSelectorMalformedWhitespaceCorpus drives API-045: a selector with
// whitespace around the equality operator is rejected 400 / SELECTOR_INVALID
// naming the offending term in `detail`, verbatim as the corpus pins it —
// whitespace is tolerated only immediately inside set-membership parentheses.
func TestSelectorMalformedWhitespaceCorpus(t *testing.T) {
	var c malformedCorpus
	loadCorpus(t, "API-045-invalid-selector-malformed.json", &c)

	sel, perr := Parse(c.Input.Request.Query["selector"])
	if perr == nil {
		t.Fatalf("Parse(%q) = %+v, nil error; want SELECTOR_INVALID", c.Input.Request.Query["selector"], sel)
	}
	if perr.Status != c.Expected.Status {
		t.Errorf("status = %d, want %d", perr.Status, c.Expected.Status)
	}
	if perr.Code != c.Expected.Body.Code {
		t.Errorf("code = %q, want %q", perr.Code, c.Expected.Body.Code)
	}
	if perr.Title != c.Expected.Body.Title {
		t.Errorf("title = %q, want %q", perr.Title, c.Expected.Body.Title)
	}
	if perr.Detail != c.Expected.Body.Detail {
		t.Errorf("detail = %q, want %q (corpus-pinned, verbatim)", perr.Detail, c.Expected.Body.Detail)
	}
	if perr.Error() != c.Expected.Body.Detail {
		t.Errorf("Error() = %q, want the Detail %q", perr.Error(), c.Expected.Body.Detail)
	}
}

// TestSelectorInnerParenWhitespaceParses confirms API-043: whitespace
// immediately inside a set-membership term's parentheses is tolerated (trimmed)
// — `key in ( a , b )` parses and evaluates as membership in {a, b}.
func TestSelectorInnerParenWhitespaceParses(t *testing.T) {
	sel, perr := Parse("env in ( a , b )")
	if perr != nil {
		t.Fatalf(`Parse("env in ( a , b )") = error %+v, want a valid membership selector`, perr)
	}
	if !sel.Matches(map[string]string{"env": "a"}, "", nil) {
		t.Errorf("membership set {a,b} did not match env=a")
	}
	if !sel.Matches(map[string]string{"env": "b"}, "", nil) {
		t.Errorf("membership set {a,b} did not match env=b")
	}
	if sel.Matches(map[string]string{"env": "c"}, "", nil) {
		t.Errorf("membership set {a,b} matched env=c, want no match")
	}
	if sel.Matches(map[string]string{"other": "a"}, "", nil) {
		t.Errorf("membership set {a,b} matched a resource with no env label, want no match")
	}
}

// TestSelectorTermForms confirms every one of the seven ANDed term forms parses
// and evaluates: equality (= and ==), inequality, membership, exclusion,
// existence, and non-existence.
func TestSelectorTermForms(t *testing.T) {
	cases := []struct {
		selector string
		labels   map[string]string
		want     bool
	}{
		// existence
		{"present", map[string]string{"present": "x"}, true},
		{"present", map[string]string{"absent": "x"}, false},
		// non-existence
		{"!missing", map[string]string{"other": "x"}, true},
		{"!missing", map[string]string{"missing": "x"}, false},
		// equality (single =)
		{"k=v", map[string]string{"k": "v"}, true},
		{"k=v", map[string]string{"k": "w"}, false},
		{"k=v", map[string]string{"other": "v"}, false},
		// equality (double ==)
		{"k==v", map[string]string{"k": "v"}, true},
		{"k==v", map[string]string{"k": "w"}, false},
		// inequality — matches when absent OR different (never in the set)
		{"k!=v", map[string]string{"k": "w"}, true},
		{"k!=v", map[string]string{"other": "z"}, true},
		{"k!=v", map[string]string{"k": "v"}, false},
		// membership
		{"k in (a,b)", map[string]string{"k": "a"}, true},
		{"k in (a,b)", map[string]string{"k": "c"}, false},
		{"k in (a,b)", map[string]string{"other": "a"}, false},
		// exclusion — matches when absent OR not in the set
		{"k notin (x)", map[string]string{"k": "y"}, true},
		{"k notin (x)", map[string]string{"other": "z"}, true},
		{"k notin (x)", map[string]string{"k": "x"}, false},
	}
	for _, tc := range cases {
		sel, perr := Parse(tc.selector)
		if perr != nil {
			t.Errorf("Parse(%q) = error %+v, want a valid selector", tc.selector, perr)
			continue
		}
		if got := sel.Matches(tc.labels, "", nil); got != tc.want {
			t.Errorf("Parse(%q).Matches(%v) = %v, want %v", tc.selector, tc.labels, got, tc.want)
		}
	}
}

// TestSelectorANDSemantics confirms API-040: comma-separated terms are ANDed —
// a resource matches only when EVERY term holds, and no OR/grouping exists.
func TestSelectorANDSemantics(t *testing.T) {
	sel, perr := Parse("kind=screen,tier!=beta,region in (us,eu)")
	if perr != nil {
		t.Fatalf("Parse of a three-term AND selector failed: %+v", perr)
	}
	pass := map[string]string{"kind": "screen", "tier": "prod", "region": "us"}
	if !sel.Matches(pass, "", nil) {
		t.Errorf("all three terms held but Matches returned false")
	}
	// Each of the following flips exactly one term false.
	fails := []map[string]string{
		{"kind": "group", "tier": "prod", "region": "us"},  // wrong kind
		{"kind": "screen", "tier": "beta", "region": "us"}, // excluded tier
		{"kind": "screen", "tier": "prod", "region": "ap"}, // region not in set
	}
	for _, labels := range fails {
		if sel.Matches(labels, "", nil) {
			t.Errorf("Matches(%v) = true, want false — AND requires every term to hold", labels)
		}
	}
}

// TestSelectorScopeExactNode confirms API-044: the equality form applied to the
// reserved key scope_node restricts to that EXACT node only, with no subtree
// expansion — a descendant of the named node does not match.
func TestSelectorScopeExactNode(t *testing.T) {
	node := "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	child := "01J8Z2Q1M8H8N4T0V1W2X3Y4Z6"
	inSubtree := func(ancestor, n string) bool { return ancestor == node && n == child }

	sel, perr := Parse("scope_node=" + node)
	if perr != nil {
		t.Fatalf("Parse of an exact scope_node selector failed: %+v", perr)
	}
	if !sel.Matches(nil, node, inSubtree) {
		t.Errorf("scope_node=%s did not match the exact node", node)
	}
	if sel.Matches(nil, child, inSubtree) {
		t.Errorf("scope_node=%s matched a descendant node, want exact-node only (no subtree expansion)", node)
	}

	// The == spelling is equivalent.
	sel2, perr := Parse("scope_node==" + node)
	if perr != nil {
		t.Fatalf("Parse of scope_node== failed: %+v", perr)
	}
	if !sel2.Matches(nil, node, inSubtree) || sel2.Matches(nil, child, inSubtree) {
		t.Errorf("scope_node==%s did not behave as an exact-node match", node)
	}
}

// TestSelectorNonULIDScopeNode confirms API-045: a scope_node value that is not
// a syntactically valid ULID — in either the subtree or the exact-node form —
// is rejected 400 / SELECTOR_INVALID.
func TestSelectorNonULIDScopeNode(t *testing.T) {
	for _, selector := range []string{
		"scope_node subtree not-a-ulid",
		"scope_node=not-a-ulid",
		"scope_node==xyz",
		"scope_node subtree 01J8Z2Q1M8H8N4T0V1W2X3Y4Z", // 25 chars, too short
	} {
		_, perr := Parse(selector)
		if perr == nil {
			t.Errorf("Parse(%q) accepted a non-ULID scope_node, want SELECTOR_INVALID", selector)
			continue
		}
		if perr.Status != 400 {
			t.Errorf("Parse(%q) status = %d, want 400", selector, perr.Status)
		}
		if perr.Code != "SELECTOR_INVALID" {
			t.Errorf("Parse(%q) code = %q, want SELECTOR_INVALID", selector, perr.Code)
		}
	}
}

// TestSelectorWhitespaceRejectedOutsideParens confirms API-043: whitespace
// anywhere in a term other than immediately inside a set-membership term's
// parentheses is rejected 400 / SELECTOR_INVALID, and the offending term is
// named in the detail.
func TestSelectorWhitespaceRejectedOutsideParens(t *testing.T) {
	for _, selector := range []string{
		"kind = screen",    // around =
		"kind ==screen",    // before ==
		"kind== screen",    // after ==
		"kind != screen",   // around !=
		"! present",        // after !
		"kind=screen, x=y", // after the term separator
	} {
		_, perr := Parse(selector)
		if perr == nil {
			t.Errorf("Parse(%q) accepted misplaced whitespace, want SELECTOR_INVALID", selector)
			continue
		}
		if perr.Code != "SELECTOR_INVALID" || perr.Status != 400 {
			t.Errorf("Parse(%q) = status %d code %q, want 400 SELECTOR_INVALID", selector, perr.Status, perr.Code)
		}
	}
}

// TestSelectorMalformedTerms confirms structurally malformed terms are rejected
// 400 / SELECTOR_INVALID rather than silently parsed.
func TestSelectorMalformedTerms(t *testing.T) {
	for _, selector := range []string{
		"=value",         // empty key
		"key=a=b",        // operator in value
		"key in (a,b",    // missing closing paren
		"key in a,b)",    // missing opening paren
		"kind subtree x", // subtree on a non-scope_node key
		"key,,other",     // empty term
		"key,",           // trailing empty term
		"key in ()x",     // trailing junk after the set
		"!",              // bare non-existence with no key
	} {
		_, perr := Parse(selector)
		if perr == nil {
			t.Errorf("Parse(%q) accepted a malformed selector, want SELECTOR_INVALID", selector)
			continue
		}
		if perr.Code != "SELECTOR_INVALID" || perr.Status != 400 {
			t.Errorf("Parse(%q) = status %d code %q, want 400 SELECTOR_INVALID", selector, perr.Status, perr.Code)
		}
	}
}

// TestSelectorEmptyMatchesEverything confirms an empty (or whitespace-only)
// selector is valid and imposes no restriction — every resource matches.
func TestSelectorEmptyMatchesEverything(t *testing.T) {
	for _, selector := range []string{"", "   "} {
		sel, perr := Parse(selector)
		if perr != nil {
			t.Fatalf("Parse(%q) = error %+v, want the match-everything selector", selector, perr)
		}
		if !sel.Matches(map[string]string{"any": "thing"}, "anynode", nil) {
			t.Errorf("Parse(%q) did not match an arbitrary resource", selector)
		}
	}
}
