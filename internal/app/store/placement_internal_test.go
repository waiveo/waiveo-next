package store

import (
	"strings"
	"testing"
)

// TestPlacementTablesCoverEveryKindButScopeNodes is the anti-drift assertion the
// reverse lookup rests on: the tables it scans are every resource kind except
// scope_nodes, plus pack_rows.
//
// It is here, in an internal test, rather than expressed through behaviour,
// because the property that matters is the LIST — a kind missing from it reports
// "nothing is placed here" for a node that is demonstrably in use, and the
// deletion guard built on it then allows exactly the orphan it exists to
// prevent. Deriving the list from allKinds is what makes that impossible; this
// pins the derivation so replacing it with a literal fails rather than rots.
func TestPlacementTablesCoverEveryKindButScopeNodes(t *testing.T) {
	scanned := map[string]bool{}
	for _, pt := range placementTables {
		if scanned[pt.table] {
			t.Errorf("placementTables scans %q twice", pt.table)
		}
		scanned[pt.table] = true
	}

	for _, k := range allKinds {
		if k == KindScopeNode {
			if scanned[string(k)] {
				t.Errorf("placementTables scans %q — a scope node's link to the tree is parent_id, and its own "+
					"scope_node column is always empty, so scanning it can only produce false references", k)
			}
			continue
		}
		if !scanned[string(k)] {
			t.Errorf("placementTables does not scan %q — a row of that kind placed at a scope node would be "+
				"invisible to the deletion guard, which would then allow the orphan", k)
		}
	}
	if !scanned[packRowsTable] {
		t.Errorf("placementTables does not scan %q — a pack's collection rows carry the same placement", packRowsTable)
	}
	if len(scanned) != len(allKinds) {
		// allKinds minus scope_nodes, plus pack_rows: the same count.
		t.Errorf("placementTables scans %d tables, want %d (allKinds without scope_nodes, plus pack_rows)", len(scanned), len(allKinds))
	}

	// Every scanned table must appear in the compiled statement, once, filtered
	// on scope_node — a table in the list but not in the SQL is the same gap.
	for _, pt := range placementTables {
		arm := "FROM " + pt.table + " WHERE scope_node = ?"
		if n := strings.Count(placedAtQuery, arm); n != 1 {
			t.Errorf("placedAtQuery contains %d arms for %q, want exactly 1 (query: %s)", n, pt.table, placedAtQuery)
		}
	}
}
