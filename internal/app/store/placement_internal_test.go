package store

import (
	"sort"
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

// placementExcludedTables are the tables that DO carry a `scope_node` column and
// are deliberately not scanned by the reverse lookup, each with the one-line
// reason. placement.go's own doc comment holds the full argument for each; this is
// the machine-checkable form of it.
//
// It is a list in a test rather than in placement.go because it is an ASSERTION
// about the schema, not an input to any production code path — nothing at runtime
// needs to know which tables were considered and rejected. What makes it safe is
// that TestEveryScopeNodeColumnIsScannedOrExcluded holds it to the live schema in
// both directions: a table can neither be excluded without appearing here, nor
// appear here without existing and carrying the column.
var placementExcludedTables = map[string]string{
	"scope_nodes": "a scope node's link to the tree is parent_id; its own scope_node column is always empty, " +
		"and DAT-006 scopes the placement rule to \"every row this contract defines OTHER THAN a scope node itself\"",
	"job_targets": "a frozen acceptance-time authorization snapshot, not a live placement — deleting a node " +
		"narrows the Job's readability (fail-closed) rather than dangling a reference",
	"events": "an immutable historical record; blocking a delete because history mentions a node would make " +
		"every node that ever did anything permanently undeletable",
	"pairing_grants": "minted, consumed and swept on expiry; never a durable placement",
}

// TestEveryScopeNodeColumnIsScannedOrExcluded closes the gap the kind-derivation
// above cannot: a NON-Kind table carrying a `scope_node` column.
//
// The test above proves the scan covers every resource Kind, because the scan is
// derived from allKinds. It says nothing about a table that is not a Kind —
// exactly what pack_rows is, which is why pack_rows had to be appended to
// placementTables BY HAND. A second such table added later (a `pack_rows` sibling,
// a new subsystem holding rows under a node) would carry a real placement, be
// invisible to the reverse lookup, and pass every existing assertion: the derived
// set would still equal allKinds, and the count would still match.
//
// So this one asks the DATABASE instead of the type system — every table in the
// live schema that actually has a `scope_node` column, via pragma_table_info over
// sqlite_master — and requires each to be either scanned or named as a deliberate
// exclusion. That is what turns the hand-maintained pair (the appended pack_rows,
// the excluded four) into a checked claim: a new table carrying a placement fails
// here until someone decides, in writing, which side of the line it is on.
func TestEveryScopeNodeColumnIsScannedOrExcluded(t *testing.T) {
	s, err := Open(":memory:", WallClockMs)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Every table the live schema gives a `scope_node` column, asked of the
	// database rather than assembled from this package's own declarations — a
	// table created by a DDL string nobody thought to cross-reference still
	// shows up here.
	rows, err := s.db.Query(`
		SELECT m.name
		FROM sqlite_master AS m
		JOIN pragma_table_info(m.name) AS p
		WHERE m.type = 'table' AND p.name = 'scope_node'
		ORDER BY m.name`)
	if err != nil {
		t.Fatalf("enumerate tables carrying a scope_node column: %v", err)
	}
	defer rows.Close()
	var carrying []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		carrying = append(carrying, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table names: %v", err)
	}
	// A silent empty result would make every assertion below vacuous, and this
	// query is the whole test — pragma_table_info returning nothing (a driver
	// without the table-valued function, say) must fail loudly, not pass.
	if len(carrying) == 0 {
		t.Fatal("no table in the live schema carries a scope_node column — the enumeration itself is broken, " +
			"since every resource table does")
	}

	scanned := map[string]bool{}
	for _, pt := range placementTables {
		scanned[pt.table] = true
	}

	for _, table := range carrying {
		_, excluded := placementExcludedTables[table]
		switch {
		case scanned[table] && excluded:
			t.Errorf("%q is both scanned by the reverse lookup and listed as a deliberate exclusion — it cannot be both", table)
		case !scanned[table] && !excluded:
			t.Errorf("table %q carries a scope_node column but is neither scanned by placedAt nor listed in "+
				"placementExcludedTables. A row of it placed at a scope node is invisible to the deletion guard, "+
				"so DAT-021 would allow exactly the dangling reference it exists to prevent. Add it to "+
				"placementTables, or exclude it there with a reason.", table)
		}
	}

	// The other direction: an exclusion naming a table that does not exist, or
	// that no longer carries the column, is a claim about nothing. Failing on it
	// keeps the list from outliving its subject — the same "inventory, not
	// suppression list" discipline the error-code gate uses.
	have := map[string]bool{}
	for _, table := range carrying {
		have[table] = true
	}
	var stale []string
	for table := range placementExcludedTables {
		if !have[table] {
			stale = append(stale, table)
		}
	}
	sort.Strings(stale)
	for _, table := range stale {
		t.Errorf("placementExcludedTables names %q, but no such table carries a scope_node column in the live "+
			"schema — delete the entry rather than leaving an exclusion for something that is not there", table)
	}
}
