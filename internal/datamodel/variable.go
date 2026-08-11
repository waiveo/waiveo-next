package datamodel

// This file implements data-model/1's Variables section (DAT-130–138): the
// named, scope-placed scalar a `rules/1` `variable` condition reads (RUL-150),
// a `variable_write` action writes (RUL-220), and whose every committed change
// emits `variable.changed` (events/1 EVT-084/085).
//
// # Why a variable is not just another scheduling row
//
// Every other row this package validates is addressed by its `id`. A variable
// is addressed by its `name` (DAT-131) — the ULID remains the row's identity
// for revision, referential integrity and audit, but a rule authored against
// `store_open` must keep resolving after the row is deleted and recreated, and
// a server-minted ULID cannot survive that. So this file carries two
// identifier rules, not one, and neither substitutes for the other.
//
// # The resolution rule, and the one it is deliberately NOT
//
// EffectiveVariable walks parent_id toward the root and takes the nearest node
// that declares the name (DAT-134) — the same ancestor walk EffectiveGeo does
// for tz/lat/long (DAT-033), reusing ScopeTree.AncestorChain rather than
// re-deriving it.
//
// It differs from EffectiveGeo in exactly one way, and it is the difference
// DAT-135 exists to state: geo resolves as a UNIT from one node, so a consumer
// may not mix an overriding node's tz with an ancestor's lat. Variables resolve
// PER NAME. A node overriding `store_open` must NOT thereby shadow an
// ancestor's `holiday_mode`. Written the other way — resolve every name from
// the single nearest node that declares any variable — setting one variable at
// a node hides every other, which is why DAT-135 says so out loud and why this
// file's loop is per name rather than per node.
//
// # What secrets would need, and why none of it is here
//
// DAT-138 forbids storing secret material as a variable, and the reason is
// structural rather than advisory: a variable is rule-readable by construction
// (RUL-150), emitted in full on every change (DAT-137), and returned by an
// api/1 read to any principal whose visible set covers its placement. Each of
// those three is individually disqualifying.
//
// A later secret tier is therefore a DIFFERENT row family beside this one, not
// a flag on this one, and this file is shaped so that adding it needs no
// reshaping here: nothing in Variable is optional-by-tier, no field means
// "redact me", and no consumer of EffectiveVariable branches on a kind. The
// concrete list a secret tier would have to add — encryption at rest, a
// no-read-back read contract, redaction in the event payload and the audit
// record, and a delivery path to the relay and the player — has no partial
// form that could ride this row safely, which is precisely why the contract
// separates them rather than parameterizing one.

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// variableNameRe is DAT-131a's name grammar, verbatim. It is deliberately
// narrower than api/1's label-key grammar (API-042): a variable name is
// compiled into a rule's closure and read from pack-authored text, so it must
// not require quoting or escaping in either.
var variableNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// The two field-level codes this file raises (data-model/1 Field-level error
// register) are spelled as LITERALS at each emit site rather than bound to
// named constants here. That is the convention every other row validator in
// this package follows (identityrows.go, validate.go), and it is not merely
// stylistic: scripts/validate-error-codes.mjs decides "this code is emitted" by
// finding its literal in a `Code: "…"` position, so a constant would make three
// published codes read as unimplemented while they are in fact raised on every
// bad write. The third, VARIABLE_NAME_DUPLICATE, is raised by the store's
// write-transaction guard (internal/app/store/variables.go), because it is a
// statement about the other rows rather than about this one.

// Variable is a variable row (DAT-130): the api/1 resource baseline plus the
// two members this contract adds. ID/Revision/ExternalID/Labels/ScopeNode/
// CreatedAt/UpdatedAt are the baseline DAT-005 fixes; Name and Value are the
// row's own content.
//
// Value is `any` rather than a typed union because DAT-132 admits exactly three
// JSON scalars and a Go union of them is either an interface (this) or a
// four-field struct with three of them always zero. Callers narrow it through
// ValidVariableValue, which is the ONE place the admitted set is decided.
type Variable struct {
	ID         string            `json:"id"`
	Revision   int64             `json:"revision"`
	ExternalID string            `json:"external_id,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	ScopeNode  string            `json:"scope_node"`
	CreatedAt  int64             `json:"created_at"`
	UpdatedAt  int64             `json:"updated_at"`

	Name  string `json:"name"`
	Value any    `json:"value"`
}

// ValidVariableName reports whether name satisfies DAT-131a.
func ValidVariableName(name string) bool { return variableNameRe.MatchString(name) }

// ValidVariableValue reports whether v is a settable variable value: a JSON
// string, number, or boolean (DAT-132), and NOT null (DAT-133).
//
// The null rule is a rule about SETTABILITY, not about representability. `null`
// is a perfectly good JSON scalar and events/1 EVT-084 already publishes it in
// old_value/new_value — with a meaning of its own, "unset beforehand" / "unset
// by this change". Admitting it as a value here would make `new_value: null`
// ambiguous between a variable set to null and a variable deleted, two facts a
// rule must tell apart arriving as the same bytes. The already-published
// sibling reading wins (DAT-133), so a set of null is refused here.
//
// A json.Number is accepted alongside float64 because a decoder configured with
// UseNumber produces one and the value is the same number either way; refusing
// it would make the check depend on how the caller happened to decode.
func ValidVariableValue(v any) bool {
	switch v.(type) {
	case string, bool, float64, json.Number:
		return true
	case int, int64:
		// Not reachable from encoding/json, but reachable from a Go caller
		// constructing a Variable directly (the rules evaluator's literal path).
		return true
	default:
		// nil (DAT-133), map, slice — all refused (DAT-132/133).
		return false
	}
}

// ValidateVariableBody validates a variable row's authored members against
// DAT-131a (name grammar) and DAT-132/133 (scalar, non-null value), returning
// one Error per violated rule. It does NOT check name uniqueness (DAT-131):
// that is a statement about the OTHER rows sharing a scope node, so it can only
// be decided under the write lock those rows are read beneath — see the store's
// variable-name write guard.
//
// body is the effective request body — a create body, or a patch shallow-merged
// onto the current row — so a patch that changes only `value` is still judged
// against the name it will have after the write, not against the absent one it
// sent.
func ValidateVariableBody(body []byte) []Error {
	var row struct {
		Name  json.RawMessage `json:"name"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(body, &row); err != nil {
		return []Error{{
			Field:   "",
			Code:    "VARIABLE_VALUE_INVALID",
			Message: fmt.Sprintf("a variable row must be a JSON object (data-model/1 DAT-130): %v", err),
		}}
	}

	var errs []Error

	// name (DAT-131/131a). An absent or non-string name is the same refusal as a
	// badly-spelled one: DAT-131 makes name REQUIRED, and the code that names the
	// grammar is the one an operator needs to see either way.
	var name string
	if len(row.Name) == 0 {
		errs = append(errs, Error{
			Field:   "name",
			Code:    "VARIABLE_NAME_INVALID",
			Message: "a variable MUST carry a name (data-model/1 DAT-131); none was declared",
		})
	} else if err := json.Unmarshal(row.Name, &name); err != nil {
		errs = append(errs, Error{
			Field:   "name",
			Code:    "VARIABLE_NAME_INVALID",
			Message: "a variable's name MUST be a string matching ^[a-z][a-z0-9_]{0,63}$ (data-model/1 DAT-131a); this name is not a string",
		})
	} else if !ValidVariableName(name) {
		errs = append(errs, Error{
			Field:   "name",
			Code:    "VARIABLE_NAME_INVALID",
			Message: fmt.Sprintf("a variable's name MUST match ^[a-z][a-z0-9_]{0,63}$ (data-model/1 DAT-131a); %q does not", name),
		})
	}

	// value (DAT-132/133). An ABSENT value and a null value are both refused, and
	// with the same code: DAT-133 makes null unsettable and DAT-132 admits only
	// the three scalars, so "no value" and "the null value" are the same request
	// — a row that would exist without being set to anything.
	if len(row.Value) == 0 {
		errs = append(errs, Error{
			Field:   "value",
			Code:    "VARIABLE_VALUE_INVALID",
			Message: "a variable MUST carry a value — a string, number, or boolean (data-model/1 DAT-132); none was declared. To unset a variable, delete the row (DAT-133)",
		})
	} else {
		var v any
		if err := json.Unmarshal(row.Value, &v); err != nil {
			errs = append(errs, Error{
				Field:   "value",
				Code:    "VARIABLE_VALUE_INVALID",
				Message: fmt.Sprintf("a variable's value MUST be a JSON scalar (data-model/1 DAT-132): %v", err),
			})
		} else if !ValidVariableValue(v) {
			errs = append(errs, Error{
				Field:   "value",
				Code:    "VARIABLE_VALUE_INVALID",
				Message: "a variable's value MUST be a string, number, or boolean (data-model/1 DAT-132); an object, an array, and null are all refused. To unset a variable, delete the row (DAT-133)",
			})
		}
	}

	return errs
}

// EffectiveVariable resolves one variable NAME at nodeID (DAT-134): the node's
// own row of that name when one exists, else the nearest ancestor on the
// parent_id chain that declares it. It returns the resolved row and true, or
// false when no node on the chain declares the name.
//
// rows may be in any order and may contain rows placed at nodes outside the
// chain; only placement is consulted. Two rows sharing a name AND a scope_node
// cannot both be stored (DAT-131, enforced at write), so the nearest match is
// unambiguous — but should a caller hand this function such a pair anyway (a
// hand-edited store, a snapshot built before that guard existed), the LOWEST id
// wins, deterministically, rather than whichever the slice happened to hold
// first. A resolution that varied with map iteration order would make a rule
// fire differently on two boxes holding identical rows.
func EffectiveVariable(tree ScopeTree, nodeID, name string, rows []Variable) (Variable, bool) {
	chain := tree.AncestorChain(nodeID)
	for _, ancestor := range chain {
		var best Variable
		found := false
		for _, r := range rows {
			if r.Name != name || r.ScopeNode != ancestor {
				continue
			}
			if !found || r.ID < best.ID {
				best, found = r, true
			}
		}
		if found {
			return best, true
		}
	}
	return Variable{}, false
}

// EffectiveVariables resolves EVERY variable name visible at nodeID into the
// name→value map a rules/1 evaluation reads (RUL-150's `vars`), applying DAT-134
// independently per name (DAT-135).
//
// The per-name independence is the whole point and is why this is not "find the
// nearest node that declares anything and take its rows": a group node that
// overrides `store_open` must still see the site's `holiday_mode`. The
// implementation makes that structural rather than incidental — it collects the
// candidate names first, then resolves each one through EffectiveVariable's own
// walk, so there is no code path in which one name's override can affect
// another's resolution.
//
// An unknown nodeID yields an empty (non-nil) map: AncestorChain returns nil for
// a node not in the tree, so nothing resolves. A non-nil empty map is returned
// rather than nil so a caller can pass the result straight into an
// eval.ActionContext without a nil check — an absent variable and an empty
// environment already mean the same thing to a `variable` condition (it fails
// closed, RUL-150).
func EffectiveVariables(tree ScopeTree, nodeID string, rows []Variable) map[string]any {
	out := map[string]any{}
	chain := tree.AncestorChain(nodeID)
	if len(chain) == 0 {
		return out
	}
	// onChain narrows the CANDIDATE NAMES to those some node on the chain
	// declares. It is an optimization, not a correctness guard, and saying so
	// matters: EffectiveVariable below re-walks the chain per name and answers
	// ok=false for a name nothing on it declares, so deleting this filter changes
	// only how many walks happen. A mutation test confirmed that — removing it
	// broke nothing. The correctness of "an off-chain row never resolves" is
	// EffectiveVariable's, and it is asserted there.
	onChain := make(map[string]bool, len(chain))
	for _, id := range chain {
		onChain[id] = true
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if !onChain[r.ScopeNode] || seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		if resolved, ok := EffectiveVariable(tree, nodeID, r.Name, rows); ok {
			out[r.Name] = resolved.Value
		}
	}
	return out
}
