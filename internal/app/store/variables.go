package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// variables.go is the store half of data-model/1's Variables section
// (DAT-130–138): the two reads a rules evaluation needs, and the one invariant
// only the write transaction can decide.
//
// # Concurrency, stated once, here
//
// Two rules writing the same variable in the same tick is not a race this file
// resolves with a merge policy — it is resolved by SERIALIZATION, which the
// store already imposes: every Create/Update/Delete runs through writeTx under
// Store.mu with SQLite opened `_txlock=immediate`, so the two writes take a
// total order and the LAST one committed is the variable's value. Neither write
// is lost in the sense of being dropped: each commits, each bumps the store
// generation, and each emits its own `variable.changed` (DAT-137, "once per
// committed write, in write order"), so the event log carries both transitions
// in the order they happened and an operator can see that two rules contended.
//
// There is deliberately NO read-modify-write verb (no increment, no
// compare-and-swap). A rule's `variable_write` declares a VALUE (RUL-220), not a
// delta, so nothing in the vocabulary can express an operation whose correctness
// depends on the value it did not observe. Adding one later means adding an
// If-Match-carrying write path, which this row family already has through the
// generic api/1 PATCH — a rule simply does not use it.
//
// # Durability and history
//
// A variable is a row in the same SQLite store as every other resource, under
// WAL with synchronous=FULL, so it survives a restart exactly as a schedule
// does. There is no value HISTORY table and this is a decision, not an
// omission: the previous value is recoverable from the event log, because
// DAT-137 requires a `variable.changed` per committed write and EVT-084 carries
// old_value AND new_value in it. A second, row-shaped history would be a
// duplicate of that log with its own retention story and its own way of
// disagreeing with it.

// VariableRows returns every stored variable row, decoded (ordered by id).
//
// It decodes to datamodel.Variable rather than handing back raw Resources
// because every caller — the effective-value resolution a rule reads, the
// closure environment, the event emitter — needs `name` and `value`, and a
// decode repeated at each call site is a decode that can disagree between them.
func (s *Store) VariableRows(ctx context.Context) ([]datamodel.Variable, error) {
	rows, err := s.List(ctx, KindVariable, ListFilter{})
	if err != nil {
		return nil, err
	}
	return decodeVariables(rows)
}

// decodeVariables projects resource rows onto datamodel.Variable. The baseline
// columns are taken from the Resource (the store's own authoritative copy),
// and only `name`/`value` are read out of the body — so a body whose injected
// baseline somehow disagreed with its columns cannot make a variable resolve
// against the wrong scope node.
func decodeVariables(rows []Resource) ([]datamodel.Variable, error) {
	out := make([]datamodel.Variable, 0, len(rows))
	for _, r := range rows {
		var content struct {
			Name  string `json:"name"`
			Value any    `json:"value"`
		}
		if err := json.Unmarshal(r.Body, &content); err != nil {
			return nil, fmt.Errorf("store: decode variable %s: %w", r.ID, err)
		}
		out = append(out, datamodel.Variable{
			ID:         r.ID,
			Revision:   r.Revision,
			ExternalID: r.ExternalID,
			Labels:     r.Labels,
			ScopeNode:  r.ScopeNode,
			CreatedAt:  r.CreatedAt,
			UpdatedAt:  r.UpdatedAt,
			Name:       content.Name,
			Value:      content.Value,
		})
	}
	return out, nil
}

// EffectiveVariables resolves every variable name visible at nodeID into the
// name→value environment a rules/1 evaluation reads (DAT-134/135, RUL-150).
//
// The scope tree and the variable rows are read under ONE read lock, not
// composed from ScopeNodes() and VariableRows(): resolution walks parent_id, so
// a tree from one read and rows from a later one can resolve a variable through
// an ancestor that had been re-parented in between — a value attributed to a
// node that never held it. The same reasoning DesiredState gives for its
// single-lock read applies here for the same reason.
func (s *Store) EffectiveVariables(ctx context.Context, nodeID string) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodes, err := readScopeNodes(ctx, s.db)
	if err != nil {
		return nil, err
	}
	varRows, err := readResources(ctx, s.db, string(KindVariable))
	if err != nil {
		return nil, err
	}
	vars, err := decodeVariables(varRows)
	if err != nil {
		return nil, err
	}
	tree, _ := datamodel.BuildScopeTree(nodes)
	return datamodel.EffectiveVariables(tree, nodeID, vars), nil
}

// VariableNameUniqueGuard enforces DAT-131: `name` is unique among variable rows
// sharing a `scope_node`. excludeID is the row being UPDATED (empty on create),
// so a patch that does not move the name does not collide with itself.
//
// This is a WriteGuard rather than a pre-write check because the invariant is
// about the OTHER rows, and those can change between a handler's read and its
// write. A pre-write check would leave exactly the window two operators (or two
// rules) creating `store_open` at the same node simultaneously would both pass
// through — and the result is not a 409 either of them sees, it is two rows
// with one name, after which DAT-134's "the node's own row of that name" has two
// answers and a rule's value depends on which the resolver reaches first.
//
// The refusal is a *ValidationError carrying the published field-level code,
// NOT a bespoke error type. That is deliberate rather than lazy: a bespoke type
// would fall through writeStoreError's switch to the 500 INTERNAL default, so
// an operator reusing a name would be told the server broke. Riding
// ValidationError puts it on the 422 / VALIDATION_FAILED path every other
// field-level refusal in this family already takes, with `field: "name"` and
// the code a client branches on.
func VariableNameUniqueGuard(name, scopeNode, excludeID string) WriteGuard {
	return func(existing []Resource) error {
		for _, r := range existing {
			if r.ID == excludeID || r.ScopeNode != scopeNode {
				continue
			}
			var content struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(r.Body, &content); err != nil {
				// A row already in the store that will not decode cannot be
				// compared, and treating it as "does not collide" would let the
				// duplicate through. Refuse the write instead: an undecodable
				// stored row is a store fault, and the write that discovers it is
				// not the write that should paper over it.
				return fmt.Errorf("store: variable name guard: existing row %s: %w", r.ID, err)
			}
			if content.Name == name {
				return &ValidationError{Errors: []datamodel.Error{{
					Field: "name",
					Code:  "VARIABLE_NAME_DUPLICATE",
					Message: fmt.Sprintf(
						"another variable named %q already exists at this scope node (data-model/1 DAT-131); a variable's name is unique among the rows sharing its placement",
						name),
				}}}
			}
		}
		return nil
	}
}
