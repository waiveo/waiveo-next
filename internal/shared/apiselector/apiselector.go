// Package apiselector implements api/1's label-selector grammar (api/1
// API-040–046): the platform's SOLE normative definition of a label selector.
// A selector is a comma-separated list of ANDed terms — there is no OR operator
// and no grouping (API-040). Parse turns a selector string into a Selector that
// evaluates against a resource's labels and (for the reserved scope_node key)
// its scope-node placement; a malformed selector yields a *ParseError naming the
// offending term for a 400 / SELECTOR_INVALID Problem (API-045).
//
// The seven term forms (API-041) are:
//
//	key=value  /  key==value        equality
//	key!=value                      inequality
//	key in (v[,v...])               set-membership
//	key notin (v[,v...])            set-exclusion
//	key                             existence
//	!key                            non-existence
//	scope_node subtree <ulid>       placement at-or-under a scope node (API-044)
//
// plus the equality form applied to the reserved key scope_node
// (scope_node=<ulid> / scope_node==<ulid>), which restricts to that EXACT node
// with no subtree expansion (API-044). scope_node is a reserved placement key,
// never matched against a label.
//
// The key and value charsets (API-042) admit no ',', '(', ')', '=', '!', or
// whitespace, so no term or value ever needs escaping. Whitespace is tolerated
// only immediately inside a set-membership/exclusion term's parentheses
// (API-043, trimmed); required as the separator between a bare-word operator
// (in/notin/subtree) and its operands; and rejected everywhere else.
//
// The package is self-contained: it depends only on the standard library and
// internal/shared/ulid (for API-045's "syntactically valid ULID" check). It
// holds no persistence — a caller supplies each resource's labels and placement.
package apiselector

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// reservedScopeNode is the reserved label-selector key that names a resource's
// scope-node placement rather than one of its labels (API-044). It is matched
// against placement (exact node or subtree), never against the labels map.
const reservedScopeNode = "scope_node"

// keyPattern is API-042's label-key grammar: an optional DNS-subdomain-style
// prefix (each ≤253 chars, no underscore) and '/', then a name segment (≤63
// chars). Neither part admits ',', '(', ')', '=', '!', or whitespace.
var keyPattern = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9.-]{0,251}/)?[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// valuePattern is API-042's label-value grammar: a name segment (≤63 chars). A
// value MAY also be empty (see valueOK); the empty string is not matched by this
// non-empty pattern.
var valuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

func keyOK(s string) bool { return keyPattern.MatchString(s) }

// valueOK reports whether s is a valid label value (API-042) — a name segment
// or the empty string.
func valueOK(s string) bool { return s == "" || valuePattern.MatchString(s) }

// ParseError describes the Problem a caller MUST emit when a selector fails to
// parse (API-045): a 400 / SELECTOR_INVALID whose Detail names the offending
// Term. It mirrors the Status/Code/Title/Detail shape the other api/1 helpers
// return, and is returned by pointer — a nil *ParseError means the selector
// parsed. The type is closed to the api/1 code registry (API-011): Code is
// always SELECTOR_INVALID.
type ParseError struct {
	// Term is the offending selector term, verbatim (unmodified) — the term
	// the Detail names.
	Term   string
	Status int
	Code   string
	Title  string
	Detail string
}

// Error lets a *ParseError satisfy the error interface; the message is the
// Problem Detail.
func (e *ParseError) Error() string { return e.Detail }

// newParseError builds the 400 / SELECTOR_INVALID Problem for an offending term.
// The Detail is `Invalid selector term "<term>": <reason>.` — the term named
// verbatim (API-045). reason MUST NOT end with a period; the trailing period is
// supplied here.
func newParseError(term, reason string) *ParseError {
	return &ParseError{
		Term:   term,
		Status: http.StatusBadRequest,
		Code:   "SELECTOR_INVALID",
		Title:  "Bad Request",
		Detail: fmt.Sprintf("Invalid selector term %q: %s.", term, reason),
	}
}

// termKind enumerates the parsed operator of a single selector term.
type termKind int

const (
	termEquality     termKind = iota // key == value
	termInequality                   // key != value
	termIn                           // key in (values…)
	termNotIn                        // key notin (values…)
	termExists                       // key
	termNotExists                    // !key
	termScopeExact                   // scope_node = <ulid>
	termScopeSubtree                 // scope_node subtree <ulid>
)

// term is one parsed selector term.
type term struct {
	kind   termKind
	key    string   // label key, or reservedScopeNode for the scope terms
	value  string   // equality/inequality operand, or the ULID for scope terms
	values []string // set-membership/exclusion operands
}

// Selector is a parsed label selector: a conjunction (AND) of terms. The zero
// Selector holds no terms and matches every resource (an empty selector imposes
// no restriction). It is produced only by Parse.
type Selector struct {
	terms []term
}

// Parse parses a selector string into a Selector (API-040–046). Terms are split
// on top-level commas (a comma immediately inside a set-membership term's
// parentheses is part of that term, not a term separator) and each is parsed
// under API-041–044. A term or key/value violating the grammar, or a scope_node
// value that is not a syntactically valid ULID, yields a *ParseError naming the
// offending term for a 400 / SELECTOR_INVALID Problem (API-045). An empty or
// whitespace-only selector is valid and matches everything.
func Parse(selector string) (Selector, *ParseError) {
	if strings.TrimFunc(selector, isSelectorSpace) == "" {
		return Selector{}, nil
	}

	rawTerms := splitTopLevel(selector)
	terms := make([]term, 0, len(rawTerms))
	for _, raw := range rawTerms {
		tm, perr := parseTerm(raw)
		if perr != nil {
			return Selector{}, perr
		}
		terms = append(terms, tm)
	}
	return Selector{terms: terms}, nil
}

// splitTopLevel splits a selector on its top-level commas — commas at paren
// depth 0. A comma inside a set-membership term's parentheses is left intact,
// since the key/value charsets never contain '(' or ')' so the only parentheses
// are a set term's own delimiters.
func splitTopLevel(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// parseTerm parses one selector term (already split from its neighbours on a
// top-level comma). It dispatches on structure: a term containing '(' is a
// set-membership/exclusion term; a remaining term containing whitespace is
// either the scope_node subtree form or a whitespace-misplacement error; and a
// term with neither is an equality/inequality/existence/non-existence term.
func parseTerm(raw string) (term, *ParseError) {
	if raw == "" {
		return term{}, newParseError(raw, "an empty term is not permitted")
	}
	if strings.IndexByte(raw, '(') >= 0 {
		return parseSet(raw)
	}
	if strings.IndexFunc(raw, isSelectorSpace) >= 0 {
		return parseSpaced(raw)
	}
	return parseCompact(raw)
}

// parseSet parses a set-membership (`key in (…)`) or set-exclusion
// (`key notin (…)`) term. Whitespace immediately inside the parentheses is
// tolerated and trimmed per value (API-043); the head must be exactly a valid
// key and the keyword `in` or `notin`.
func parseSet(raw string) (term, *ParseError) {
	open := strings.IndexByte(raw, '(')
	if raw[len(raw)-1] != ')' {
		return term{}, newParseError(raw, "a set-membership term must end with a closing parenthesis")
	}
	head := raw[:open]
	inner := raw[open+1 : len(raw)-1]

	fields := strings.FieldsFunc(head, isSelectorSpace)
	if len(fields) != 2 {
		return term{}, newParseError(raw, "a set-membership term must be written key in (…) or key notin (…)")
	}
	key, keyword := fields[0], fields[1]
	if !keyOK(key) {
		return term{}, newParseError(raw, "the set-membership key is not a valid label key")
	}
	if key == reservedScopeNode {
		return term{}, newParseError(raw, "the reserved key scope_node supports only equality (=) or the subtree operator")
	}
	var kind termKind
	switch keyword {
	case "in":
		kind = termIn
	case "notin":
		kind = termNotIn
	default:
		return term{}, newParseError(raw, "a set-membership term must use the operator in or notin")
	}

	parts := strings.Split(inner, ",")
	values := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimFunc(p, isSelectorSpace)
		if !valueOK(v) {
			return term{}, newParseError(raw, "a set-membership value is not a valid label value")
		}
		values = append(values, v)
	}
	return term{kind: kind, key: key, values: values}, nil
}

// parseSpaced parses a term that contains whitespace but no parentheses. The
// only such valid term is the scope_node subtree form `scope_node subtree
// <ulid>` (API-044); any other whitespace is misplaced (API-043) and yields a
// SELECTOR_INVALID error naming the term — with the specific "around an equality
// operator" reason when removing the whitespace would have produced an
// equality/inequality term.
func parseSpaced(raw string) (term, *ParseError) {
	fields := strings.FieldsFunc(raw, isSelectorSpace)
	if len(fields) == 3 && fields[1] == "subtree" {
		key, value := fields[0], fields[2]
		if key != reservedScopeNode {
			return term{}, newParseError(raw, "the subtree operator applies only to the reserved key scope_node")
		}
		if !ulid.Valid(value) {
			return term{}, newParseError(raw, "the scope_node value is not a syntactically valid ULID")
		}
		return term{kind: termScopeSubtree, key: key, value: value}, nil
	}

	// Not the subtree form: the whitespace is misplaced. If deleting it would
	// have produced a valid equality/inequality term, name that specifically —
	// this is the API-045 corpus case (`kind = screen`).
	stripped := strings.Map(func(r rune) rune {
		if isSelectorSpace(r) {
			return -1
		}
		return r
	}, raw)
	if tm, perr := parseCompact(stripped); perr == nil {
		switch tm.kind {
		case termEquality, termInequality, termScopeExact:
			return term{}, newParseError(raw, "whitespace is not permitted around an equality operator")
		}
	}
	return term{}, newParseError(raw, "whitespace is not permitted in this position")
}

// parseCompact parses a term with no parentheses and no whitespace: one of
// non-existence (`!key`), inequality (`key!=value`), equality (`key=value` /
// `key==value`, including the reserved scope_node exact-node form), or existence
// (`key`).
func parseCompact(raw string) (term, *ParseError) {
	if raw == "" {
		return term{}, newParseError(raw, "an empty term is not permitted")
	}

	// Non-existence: a leading '!'. Inequality's '!' is never at index 0 (its
	// key precedes it), so a leading '!' is unambiguously non-existence.
	if raw[0] == '!' {
		key := raw[1:]
		if key == reservedScopeNode {
			return term{}, newParseError(raw, "the reserved key scope_node supports only equality (=) or the subtree operator")
		}
		if !keyOK(key) {
			return term{}, newParseError(raw, "a non-existence term must be ! followed by a valid label key")
		}
		return term{kind: termNotExists, key: key}, nil
	}

	eq := strings.IndexByte(raw, '=')
	if eq < 0 {
		// Existence: a bare key.
		if raw == reservedScopeNode {
			return term{}, newParseError(raw, "the reserved key scope_node supports only equality (=) or the subtree operator")
		}
		if !keyOK(raw) {
			return term{}, newParseError(raw, "not a valid selector term")
		}
		return term{kind: termExists, key: raw}, nil
	}

	// Inequality: `key!=value` — the '=' is preceded by '!'.
	if eq > 0 && raw[eq-1] == '!' {
		key := raw[:eq-1]
		value := raw[eq+1:]
		if !keyOK(key) {
			return term{}, newParseError(raw, "the inequality key is not a valid label key")
		}
		if key == reservedScopeNode {
			return term{}, newParseError(raw, "the reserved key scope_node supports only equality (=) or the subtree operator")
		}
		if !valueOK(value) {
			return term{}, newParseError(raw, "the inequality value is not a valid label value")
		}
		return term{kind: termInequality, key: key, value: value}, nil
	}

	// Equality: `key=value` or `key==value`.
	key := raw[:eq]
	value := raw[eq+1:]
	if eq+1 < len(raw) && raw[eq+1] == '=' {
		value = raw[eq+2:]
	}
	if !keyOK(key) {
		return term{}, newParseError(raw, "the equality key is not a valid label key")
	}
	if key == reservedScopeNode {
		// The exact-node form: scope_node=<ulid> / scope_node==<ulid> (API-044).
		if !ulid.Valid(value) {
			return term{}, newParseError(raw, "the scope_node value is not a syntactically valid ULID")
		}
		return term{kind: termScopeExact, key: key, value: value}, nil
	}
	if !valueOK(value) {
		return term{}, newParseError(raw, "the equality value is not a valid label value")
	}
	return term{kind: termEquality, key: key, value: value}, nil
}

// isSelectorSpace reports whether r is whitespace for the selector grammar.
func isSelectorSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

// Matches reports whether a resource satisfies the selector (API-040): every
// term must hold (AND); an empty selector matches everything. labels is the
// resource's label set; scopeNode is the id of the resource's scope-node
// placement; inSubtree reports whether node lies strictly below ancestor in the
// scope-node tree (it is consulted only by a scope_node subtree term, and may be
// nil when the selector has no such term).
//
// The reserved key scope_node is evaluated against placement, never against
// labels: `scope_node subtree <ulid>` matches when scopeNode equals the named
// ulid OR inSubtree(ulid, scopeNode); `scope_node=<ulid>` matches only the exact
// node. Label terms follow the usual set semantics — an inequality or exclusion
// term also matches a resource lacking the key (the key's value is then never in
// the excluded set).
func (s Selector) Matches(labels map[string]string, scopeNode string, inSubtree func(ancestor, node string) bool) bool {
	for _, t := range s.terms {
		if !t.matches(labels, scopeNode, inSubtree) {
			return false
		}
	}
	return true
}

// matches evaluates a single term.
func (t term) matches(labels map[string]string, scopeNode string, inSubtree func(ancestor, node string) bool) bool {
	switch t.kind {
	case termEquality:
		v, ok := labels[t.key]
		return ok && v == t.value
	case termInequality:
		v, ok := labels[t.key]
		return !ok || v != t.value
	case termIn:
		v, ok := labels[t.key]
		return ok && containsValue(t.values, v)
	case termNotIn:
		v, ok := labels[t.key]
		return !ok || !containsValue(t.values, v)
	case termExists:
		_, ok := labels[t.key]
		return ok
	case termNotExists:
		_, ok := labels[t.key]
		return !ok
	case termScopeExact:
		return scopeNode == t.value
	case termScopeSubtree:
		if scopeNode == t.value {
			return true
		}
		return inSubtree != nil && inSubtree(t.value, scopeNode)
	}
	return false
}

// containsValue reports whether v is in values.
func containsValue(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
