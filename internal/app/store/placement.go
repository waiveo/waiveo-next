package store

import (
	"context"
	"fmt"
	"strings"
)

// placement.go answers one question the tree cannot answer about itself: which
// rows, anywhere in this store, are PLACED at a given scope node — data-model/1
// DAT-006's `scope_node`, the column every row but a scope node itself carries.
//
// It exists for the deletion rules (DAT-021): a scope node still named as some
// row's scope_node MUST NOT be removed, and nothing else in this package looks
// at placement from the referenced side. The forward direction (ListFilter.
// ScopeNode) has always been here; this is the reverse.

// Placement names one row found placed at a scope node: the table it lives in
// (a Kind's string value IS its table name — tableFor's own identity) and the
// row's own id. It is deliberately not a Kind: pack_rows carries a placement and
// is not a resource Kind (it has no `id` column and no api/1 resource baseline),
// and a type that could not name it would leave a real reference invisible.
type Placement struct {
	Table string
	ID    string
}

// placementTable is one table the reverse lookup scans: its name and the column
// its rows are identified by.
type placementTable struct {
	table    string
	idColumn string
}

// packRowsTable is the declarative-packs collection-row table (packs.go). It is
// named here as a literal because it is not a Kind — see Placement.
const packRowsTable = "pack_rows"

// placementTables is every table whose rows carry a `scope_node` placement, in
// scan order.
//
// It is DERIVED from allKinds rather than listed by hand, minus scope_nodes
// itself: a scope node's link to the tree is its parent_id, not a scope_node
// column (its own column is always empty), and DAT-006 says as much — "every row
// this contract defines OTHER THAN a scope node itself". Deriving it is what
// makes a resource kind added later automatically visible to the reverse lookup;
// a hand-written list is how the eleventh kind ends up silently unreferenced.
//
// pack_rows is appended because a pack's collection rows carry the same
// placement in the same role and are authorized and visibility-filtered by it
// exactly as every resource row is (internal/app/api/packs_data.go). A node they
// sit under is genuinely in use, whoever declared the collection.
//
// Deliberately NOT here, each for a reason rather than an oversight:
//   - jobs / job_targets — an execution record over other rows, carrying no
//     scope_node of its own (jobs.go).
//   - events — an immutable historical record. Blocking a deletion because
//     history MENTIONS a node would make every node that ever did anything
//     permanently undeletable.
//   - pairing_grants — minted, consumed and swept on expiry; never a durable
//     placement.
var placementTables = func() []placementTable {
	out := make([]placementTable, 0, len(allKinds))
	for _, k := range allKinds {
		if k == KindScopeNode {
			continue
		}
		out = append(out, placementTable{table: string(k), idColumn: "id"})
	}
	return append(out, placementTable{table: packRowsTable, idColumn: "entity_id"})
}()

// placedAtQuery is the single compound statement the reverse lookup runs: one
// arm per placement table, `LIMIT 1` over the whole compound (SQLite applies a
// compound's LIMIT to the concatenation, and produces the arms in order, so the
// scan stops at the first hit rather than materializing every match).
//
// It is built once at init from placementTables. Every interpolated name comes
// from that closed, package-owned set — the same property tableFor establishes
// for the CRUD statements — and the scope node itself is a bound parameter, one
// per arm.
var placedAtQuery = func() string {
	var sb strings.Builder
	for i, pt := range placementTables {
		if i > 0 {
			sb.WriteString(" UNION ALL ")
		}
		fmt.Fprintf(&sb, "SELECT '%s' AS tbl, %s AS row_id FROM %s WHERE scope_node = ?", pt.table, pt.idColumn, pt.table)
	}
	sb.WriteString(" LIMIT 1")
	return sb.String()
}()

// placedAt returns one row placed at scopeNode and whether any exists.
//
// The cost is one statement and, worst case, one full scan per placement table —
// the tables carry no scope_node index, and none is added for this: a scope-node
// deletion is a rare operation on an appliance-scale store, and the guard that
// runs on EVERY create and update already reads a whole table into Go structs
// (readResources, the external_id guard's snapshot). This reads at most one row
// out of the database and stops.
func placedAt(ctx context.Context, q queryer, scopeNode string) (Placement, bool, error) {
	if scopeNode == "" {
		// Every row's scope_node column defaults to the empty string, so an empty
		// argument would match every unplaced row in the store and report a
		// reference that does not exist. No scope node has an empty id.
		return Placement{}, false, nil
	}
	args := make([]any, len(placementTables))
	for i := range args {
		args[i] = scopeNode
	}
	rows, err := q.QueryContext(ctx, placedAtQuery, args...)
	if err != nil {
		return Placement{}, false, fmt.Errorf("store: rows placed at %s: %w", scopeNode, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return Placement{}, false, rows.Err()
	}
	var p Placement
	if err := rows.Scan(&p.Table, &p.ID); err != nil {
		return Placement{}, false, fmt.Errorf("store: scan row placed at %s: %w", scopeNode, err)
	}
	return p, true, nil
}
