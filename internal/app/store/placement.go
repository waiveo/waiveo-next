package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// placement.go owns data-model/1 DAT-006's `scope_node` — the column every row
// but a scope node itself carries — as a REFERENCE, in both of the directions the
// tree cannot answer about itself:
//
//   - placedAt — the REFERENCED side: which rows, anywhere in this store, are
//     placed AT a given scope node. It exists for the deletion rules (DAT-021): a
//     scope node still named as some row's scope_node MUST NOT be removed.
//   - scopeNodeExists / checkPlacementResolves — the REFERRING side: does the node
//     a row is being placed at exist at all? DAT-006 says a row's scope_node is
//     "the id of the scope node it is placed under", and a row placed under nothing
//     is the identical dangling state a delete is refused for, reached from the
//     other end.
//
// One state, two entrances, and the entrances need different shapes: a deletion
// asks about ONE node across EVERY table (so placedAt is a compound scan over
// placementTables), while a write asks about ONE node against the tree (so the
// forward check is a single lookup and needs no table list at all).
//
// What they DO share is placementTables' subject matter: the set of tables whose
// scope_node is a LIVE placement is both the set that must block a deletion and
// the set whose writes must resolve. A table on one list and not the other would be
// a store where a node cannot be deleted but a row may point at nothing, or the
// reverse. placementTables is that one inventory;
// TestEveryScopeNodeColumnIsScannedOrExcluded holds it against the live schema, and
// TestEveryPlacementTableRefusesAnUnresolvablePlacement holds every table on it to
// the forward refusal.
//
// The plain forward read (ListFilter.ScopeNode) has always been here; neither of
// these is that.

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
// Deliberately NOT here, each for a reason rather than an oversight. Three of
// the four DO carry a `scope_node` column, so "no such column" is never the
// reason — what disqualifies them is what the column MEANS there:
//   - job_targets — a FROZEN AUTHORIZATION SNAPSHOT, not a placement: the node
//     each target sat at when the job was accepted, recorded so a Job's
//     readability cannot drift or be widened afterwards (JobRecord.ScopeNodes,
//     consumed by the api layer's jobVisible). It is the same "historical record"
//     rationale as events, one row per target instead of per event, and treating
//     it as a live reference would make every node any job ever touched
//     permanently undeletable. Deleting a node is fail-closed here rather than
//     dangling: the deleted id resolves through no ancestor chain, so
//     jobVisible's canRead answers only via the workspace-root fallback and the
//     Job NARROWS to root-bound callers and its own submitter. (jobs is the
//     parent row and has no `scope_node` column at all.)
//   - events — an immutable historical record. Blocking a deletion because
//     history MENTIONS a node would make every node that ever did anything
//     permanently undeletable.
//   - pairing_grants — minted, consumed and swept on expiry; never a durable
//     placement.
//
// TestEveryScopeNodeColumnIsScannedOrExcluded (placement_internal_test.go) reads
// the live schema and fails on a table carrying a `scope_node` column that is
// neither scanned above nor named in that test's exclusion list, so this
// completeness argument is checked rather than asserted.
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

// scopeNodeExistsQuery answers the one question a WRITE has to ask about a
// placement: is the node there? It is the forward direction of placedAt's reverse
// scan, and it needs no table list at all — the whole tree lives in one table.
const scopeNodeExistsQuery = `SELECT 1 FROM scope_nodes WHERE id = ? LIMIT 1`

// scopeNodeExists reports whether scopeNode names a stored scope node.
//
// An empty argument is reported as existing, and that is not a shortcut. Every
// row's scope_node column defaults to the empty string, so "" means UNPLACED, not
// "placed at a node whose id is empty" — the same distinction placedAt draws from
// its own side. Whether a given kind MAY be unplaced is DAT-006's presence half,
// enforced per kind by the datamodel validators; a reference check that refused an
// absent reference would be deciding that rule for every kind at once, silently.
//
// That claim used to be true of the identity rows ONLY, and it said so as though it
// were true of everything — which is how six kinds came to accept an unplaced row
// at 201 while this comment asserted otherwise. The presence half is now enforced
// where DAT-006 requires it: the six scheduling-core kinds DAT-005 enumerates
// (checkRowPlacement) plus the two identity kinds (checkPlacementAndName). A kind
// this contract does NOT define — an automation, a webhook endpoint, a pack row —
// is governed by its own contract and is deliberately not covered, which is why
// the rule has to stay per-kind rather than becoming a blanket refusal here.
func scopeNodeExists(ctx context.Context, q queryer, scopeNode string) (bool, error) {
	if scopeNode == "" {
		return true, nil
	}
	rows, err := q.QueryContext(ctx, scopeNodeExistsQuery, scopeNode)
	if err != nil {
		return false, fmt.Errorf("store: resolve scope node %s: %w", scopeNode, err)
	}
	defer rows.Close()
	if rows.Next() {
		return true, nil
	}
	return false, rows.Err()
}

// checkPlacementResolves is the refusal scopeNodeExists feeds: a *ValidationError
// naming DAT-006's `scope_node` field, for the api layer to render as
// VALIDATION_FAILED with the per-field code. kind names the row's own family, so
// the message says which table refused rather than only which column.
//
// # Which rows are checked, and why that is enough
//
// This judges the placement the write itself carries, not the table it landed in.
// The invariant — no row references a scope node that is not there — has exactly
// two entrances, and both are now closed: a row arrives carrying an unresolvable
// scope_node (here), or the node a resolving reference names is removed underneath
// it (DAT-021, refused by the delete guard reading placedAt). There is no third,
// so a whole-table sweep would add no coverage and would refuse a caller's
// perfectly good write because some OTHER row is wrong — with a `scope_node`
// error naming a body member the request does not have.
//
// # The code
//
// data-model/1 publishes NO error code for this refusal. Its Error taxonomy has
// SCOPE_NODE_PARENT_INVALID for a scope node's own parent_id (DAT-002/003) and
// SCOPE_NODE_IN_USE / _NOT_EMPTY / _ORG_UNDELETABLE for the deletion side
// (DAT-020-022) — none of which is a row's own placement — and DAT-006 states the
// requirement without naming a refusal for it. So this uses REFERENCE_INVALID,
// which is what every OTHER unresolvable cross-row reference in the data model is
// already spelled as (DAT-050/060/070/075/080's schedule_id, playlist_id,
// fallback_id and preset_batch_id, and PLY-124's optional device_id — see
// internal/datamodel/validate.go and identityrows.go). It is not itself published;
// it is this data model's established per-field vocabulary for "this reference
// names no row", and a row's scope_node is that, exactly. Borrowing
// SCOPE_NODE_PARENT_INVALID instead would tell a caller their parent_id was wrong
// on a row that has no parent_id.
//
// The published code a caller branches on first is api/1's own, and it is the one
// this becomes: VALIDATION_FAILED, 422.
func checkPlacementResolves(ctx context.Context, q queryer, kind, scopeNode string) error {
	found, err := scopeNodeExists(ctx, q, scopeNode)
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	return &ValidationError{Errors: []datamodel.Error{{
		Field: "scope_node",
		Code:  "REFERENCE_INVALID",
		Message: "a row's scope_node MUST be the id of an existing scope node (DAT-006); " +
			scopeNode + " names none (table " + kind + ")",
	}}}
}
