package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	_ "modernc.org/sqlite"
)

// seededOrgNodeID is the org root a store seeded by THIS build carries — and,
// on a store seeded before the seed inserted it, the id its Demo Site names and
// nothing holds.
const seededOrgNodeID = "01J8Z0DEM00RGANCEST0RB0VND"

// seededSiteNodeID is the Demo Site: the node orphaned when the root is missing.
const seededSiteNodeID = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"

// dropOrgRow deletes the org-kind scope node straight out of the file, forging
// the state a box seeded in the window before the demo seed inserted its own
// root is stuck in: a site whose parent_id names a node that is not there, and no
// org row anywhere. The store package can no longer PRODUCE that state — the
// full-tree validator refuses the write — so the fixture is made past it.
func dropOrgRow(t *testing.T, dsn, orgID string) {
	t.Helper()
	withRawDB(t, dsn, func(db *sql.DB) {
		res, err := db.Exec(`DELETE FROM scope_nodes WHERE id = ?`, orgID)
		if err != nil {
			t.Fatalf("delete org row %q: %v", orgID, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("deleted %d row(s) for org %q, want 1", n, orgID)
		}
	})
}

// repointSiteParent rewrites the Demo Site's parent_id in place, so a test can
// choose which missing id the tree names.
func repointSiteParent(t *testing.T, dsn, from, to string) {
	t.Helper()
	withRawDB(t, dsn, func(db *sql.DB) {
		if _, err := db.Exec(`UPDATE scope_nodes SET body = replace(body, ?, ?) WHERE id = ?`,
			`"`+from+`"`, `"`+to+`"`, seededSiteNodeID); err != nil {
			t.Fatalf("repoint the site's parent_id: %v", err)
		}
	})
}

// TestHealOrgRootLeavesAStoreWithARootUntouched is the safety claim that lets
// this run unattended at every boot: a store that has its org root is not written
// to at all — not a row, not the generation, not even a commit hook, which would
// otherwise nudge every connected relay on every restart for no reason. It is the
// same shape TestMigrateRowIDsLeavesAConformingStoreUntouched asserts of the
// migration, for the same reason.
func TestHealOrgRootLeavesAStoreWithARootUntouched(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)
	before := dumpStore(t, dsn)

	s := openStoreAt(t, dsn)
	var hookFired int
	s.OnCommit(func() { hookFired++ })

	heal, err := s.HealOrgRoot(ctx)
	if err != nil {
		t.Fatalf("HealOrgRoot: %v", err)
	}
	if heal.Needed || heal.Healed || heal.Declined != "" {
		t.Fatalf("HealOrgRoot on a store with a root = %+v, want a zero report", heal)
	}
	if hookFired != 0 {
		t.Fatalf("the commit hook fired %d time(s) on a store with a root, want 0", hookFired)
	}
	assertSameStore(t, before, dumpStore(t, dsn))

	// And the read-only report agrees with the repair that ran.
	plan, err := s.PlanOrgRootHeal(ctx)
	if err != nil {
		t.Fatalf("PlanOrgRootHeal: %v", err)
	}
	if plan.Needed || plan.Declined != "" {
		t.Fatalf("PlanOrgRootHeal on a store with a root = %+v, want a zero report", plan)
	}
}

// TestHealOrgRootLeavesAnEmptyStoreForTheSeed pins the one ordering hazard: this
// runs at boot BEFORE SeedDemo, and SeedDemo runs only while the generation is
// still 0. A repair that wrote anything into an empty store would suppress the
// seed outright and leave a fresh box with no demo at all.
func TestHealOrgRootLeavesAnEmptyStoreForTheSeed(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")
	s := openStoreAt(t, dsn)

	heal, err := s.HealOrgRoot(ctx)
	if err != nil {
		t.Fatalf("HealOrgRoot: %v", err)
	}
	if heal.Needed || heal.Healed || heal.Declined != "" {
		t.Fatalf("HealOrgRoot on an empty store = %+v, want a zero report", heal)
	}
	if g := gen(t, s); g != 0 {
		t.Fatalf("generation after HealOrgRoot on an empty store = %d, want 0 — the seed runs only at 0", g)
	}
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo after HealOrgRoot: %v", err)
	}
}

// TestHealOrgRootRecreatesTheRootTheTreeAlreadyNames is the end-to-end proof:
// the exact shape a live box was stuck in, repaired at the id its own rows name,
// with the owner-facing surface's own lookup (WorkspaceRoot) working afterwards.
func TestHealOrgRootRecreatesTheRootTheTreeAlreadyNames(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)
	dropOrgRow(t, dsn, seededOrgNodeID)

	s := openStoreAt(t, dsn)
	var hookFired int
	s.OnCommit(func() { hookFired++ })

	// BEFORE: the whole owner surface authorizes through this call, so this is
	// what a 404 on every owner-gated route looks like from underneath.
	if _, _, err := s.WorkspaceRoot(ctx); !errors.Is(err, store.ErrNoWorkspace) {
		t.Fatalf("WorkspaceRoot on a store with no org node = %v, want ErrNoWorkspace", err)
	}
	faults, err := s.StoredFaults(ctx)
	if err != nil {
		t.Fatalf("StoredFaults: %v", err)
	}
	if len(faults) != 1 || faults[0].Code != "SCOPE_NODE_PARENT_INVALID" {
		t.Fatalf("StoredFaults before the heal = %+v, want exactly one SCOPE_NODE_PARENT_INVALID", faults)
	}
	before := gen(t, s)

	heal, err := s.HealOrgRoot(ctx)
	if err != nil {
		t.Fatalf("HealOrgRoot: %v", err)
	}
	if !heal.Healed || heal.OrgID != seededOrgNodeID || heal.ReferencedID != seededOrgNodeID {
		t.Fatalf("HealOrgRoot = %+v, want the root re-created at %q", heal, seededOrgNodeID)
	}
	if strings.Join(heal.ChildIDs, " ") != seededSiteNodeID {
		t.Fatalf("HealOrgRoot healed children %v, want just the orphaned site %q", heal.ChildIDs, seededSiteNodeID)
	}

	// AFTER: the workspace has an identity again, and the tree is clean.
	id, state, err := s.WorkspaceRoot(ctx)
	if err != nil {
		t.Fatalf("WorkspaceRoot after the heal: %v", err)
	}
	if id != seededOrgNodeID {
		t.Fatalf("WorkspaceRoot id = %q, want the id the tree already named (%q)", id, seededOrgNodeID)
	}
	if state != "active" {
		t.Fatalf("WorkspaceRoot account_state = %q, want active — a re-created root is a live workspace, not a purged one", state)
	}
	faults, err = s.StoredFaults(ctx)
	if err != nil {
		t.Fatalf("StoredFaults: %v", err)
	}
	if len(faults) != 0 {
		t.Fatalf("StoredFaults after the heal = %+v, want none", faults)
	}
	if g := gen(t, s); g != before+1 {
		t.Fatalf("generation after the heal = %d, want %d — the tree changed and every relay caches it by generation", g, before+1)
	}
	if hookFired != 1 {
		t.Fatalf("the commit hook fired %d time(s) for the heal, want 1", hookFired)
	}

	// The row it wrote is a row the api can serve: every member the schema
	// declares required is present, and it carries the account fields DAT-010/013
	// make mandatory on an org.
	res, ok, err := s.Get(ctx, store.KindScopeNode, seededOrgNodeID)
	if err != nil || !ok {
		t.Fatalf("Get the healed org node: ok=%v err=%v", ok, err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(res.Body, &body); err != nil {
		t.Fatalf("the healed org body is not JSON: %v", err)
	}
	for key, want := range map[string]string{
		"kind":          `"org"`,
		"name":          `"Demo Org"`,
		"parent_id":     `null`,
		"account_state": `"active"`,
		"entitlements":  `{}`,
		"labels":        `{}`,
		"revision":      `1`,
	} {
		got, present := body[key]
		if !present {
			t.Errorf("the healed org body has no %q; a client generated from the schema expects one", key)
			continue
		}
		if string(got) != want {
			t.Errorf("the healed org %q = %s, want %s", key, got, want)
		}
	}

	// A store seeded by THIS build and a store healed into having a root are the
	// same tree, not two dialects.
	fresh := seededStore(t)
	if want, got := dumpStore(t, fresh)["scope_nodes"], dumpStore(t, dsn)["scope_nodes"]; len(want) != len(got) {
		t.Fatalf("the healed store holds %d scope node(s), a freshly seeded one holds %d", len(got), len(want))
	}
}

// TestHealOrgRootIsIdempotentAcrossBoots: the repair persists, so the next boot
// is a plain no-op — the property that lets it sit unconditionally on the boot
// path rather than behind a flag someone has to know to set.
func TestHealOrgRootIsIdempotentAcrossBoots(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)
	dropOrgRow(t, dsn, seededOrgNodeID)

	first := openStoreAt(t, dsn)
	if heal, err := first.HealOrgRoot(ctx); err != nil || !heal.Healed {
		t.Fatalf("first HealOrgRoot = %+v, %v; want a repair", heal, err)
	}
	after := dumpStore(t, dsn)

	// A second boot: a new Store over the same file, exactly as a restart is.
	second := openStoreAt(t, dsn)
	heal, err := second.HealOrgRoot(ctx)
	if err != nil {
		t.Fatalf("second HealOrgRoot: %v", err)
	}
	if heal.Needed || heal.Healed {
		t.Fatalf("second HealOrgRoot = %+v, want nothing to do", heal)
	}
	assertSameStore(t, after, dumpStore(t, dsn))
}

// TestHealOrgRootFoldsAReferenceItCreatesTheRowFor is the one case where a
// dangling reference IS rewritten, and the reason it is allowed: the row it names
// is created in the same transaction, so the reference and its target move
// together. A store written before DAT-005a names its root in a spelling no row
// could ever legally carry, so minting the root at that spelling would trade the
// dangle for a permanent ROW_ID_INVALID.
func TestHealOrgRootFoldsAReferenceItCreatesTheRowFor(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)
	regressToPreRuleIDs(t, dsn)
	const legacyOrgID = "01J8Z0DEMOORGANCESTORBOUND"
	dropOrgRow(t, dsn, legacyOrgID)

	s := openStoreAt(t, dsn)
	// The boot order: canonicalize first (which leaves the dangle alone), then
	// repair the missing row.
	if _, err := s.MigrateRowIDs(ctx); err != nil {
		t.Fatalf("MigrateRowIDs: %v", err)
	}
	heal, err := s.HealOrgRoot(ctx)
	if err != nil {
		t.Fatalf("HealOrgRoot: %v", err)
	}
	if !heal.Healed || heal.OrgID != seededOrgNodeID || heal.ReferencedID != legacyOrgID {
		t.Fatalf("HealOrgRoot = %+v, want %q created for the reference %q", heal, seededOrgNodeID, legacyOrgID)
	}

	nodes, err := s.ScopeNodes(ctx)
	if err != nil {
		t.Fatalf("ScopeNodes: %v", err)
	}
	var sawSite bool
	for _, n := range nodes {
		if n.ID != seededSiteNodeID {
			continue
		}
		sawSite = true
		if n.ParentID == nil || *n.ParentID != seededOrgNodeID {
			t.Fatalf("the site's parent_id = %v, want it repointed at the row that was just created (%q)", n.ParentID, seededOrgNodeID)
		}
	}
	if !sawSite {
		t.Fatalf("the site scope node is gone; the heal creates rows, it does not move them")
	}
	faults, err := s.StoredFaults(ctx)
	if err != nil {
		t.Fatalf("StoredFaults: %v", err)
	}
	if len(faults) != 0 {
		t.Fatalf("StoredFaults after canonicalize + heal = %+v, want none", faults)
	}
	if id, _, err := s.WorkspaceRoot(ctx); err != nil || id != seededOrgNodeID {
		t.Fatalf("WorkspaceRoot = %q, %v; want %q", id, err, seededOrgNodeID)
	}
}

// TestHealOrgRootDeclinesWhenTheReferenceIsAlsoARowsID covers the one hazard the
// folding path carries: the rewrite it performs remaps the id column of every
// table, so a row elsewhere carrying the missing node's id would be renamed by a
// repair with no business touching it. The repair's whole license is that it
// creates a row and changes nothing else, so it refuses rather than widen.
func TestHealOrgRootDeclinesWhenTheReferenceIsAlsoARowsID(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)
	regressToPreRuleIDs(t, dsn)
	const legacyOrgID = "01J8Z0DEMOORGANCESTORBOUND"
	dropOrgRow(t, dsn, legacyOrgID)
	withRawDB(t, dsn, func(db *sql.DB) {
		if _, err := db.Exec(
			`INSERT INTO playlists (id, revision, external_id, labels, scope_node, created_at, updated_at, body)
			 VALUES (?, 1, '', '{}', ?, 1, 1, ?)`,
			legacyOrgID, seededSiteNodeID,
			`{"id":"`+legacyOrgID+`","scope_node":"`+seededSiteNodeID+`","name":"Namesake","items":[],"revision":1,"created_at":1,"updated_at":1}`,
		); err != nil {
			t.Fatalf("plant the namesake playlist: %v", err)
		}
	})
	before := dumpStore(t, dsn)

	s := openStoreAt(t, dsn)
	heal, err := s.HealOrgRoot(ctx)
	if err != nil {
		t.Fatalf("HealOrgRoot: %v", err)
	}
	if heal.Healed || heal.Needed {
		t.Fatalf("HealOrgRoot = %+v, want a refusal", heal)
	}
	if !strings.Contains(heal.Declined, "playlists") {
		t.Fatalf("HealOrgRoot declined with %q, want it to name the row it would have renamed", heal.Declined)
	}
	assertSameStore(t, before, dumpStore(t, dsn))
}

// TestHealOrgRootNamesARootItDoesNotRecognise: when the missing id is not the one
// this build's seed knows, the row still gets created — the reference is what
// makes it needed — but it is given a descriptive name rather than the demo's,
// because the platform has no idea what this workspace is called.
func TestHealOrgRootNamesARootItDoesNotRecognise(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)
	dropOrgRow(t, dsn, seededOrgNodeID)
	const otherOrgID = "01JBBBBBBBBBBBBBBBBBBBBBBB"
	repointSiteParent(t, dsn, seededOrgNodeID, otherOrgID)

	s := openStoreAt(t, dsn)
	heal, err := s.HealOrgRoot(ctx)
	if err != nil {
		t.Fatalf("HealOrgRoot: %v", err)
	}
	if !heal.Healed || heal.OrgID != otherOrgID {
		t.Fatalf("HealOrgRoot = %+v, want the root created at %q", heal, otherOrgID)
	}
	res, ok, err := s.Get(ctx, store.KindScopeNode, otherOrgID)
	if err != nil || !ok {
		t.Fatalf("Get the healed org node: ok=%v err=%v", ok, err)
	}
	var node struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(res.Body, &node); err != nil {
		t.Fatalf("the healed org body is not JSON: %v", err)
	}
	if node.Name != "Workspace Root" {
		t.Fatalf("the healed org name = %q, want a descriptive one — the repair must not claim to know whose workspace this is", node.Name)
	}
}

// TestHealOrgRootDeclinesWhenTheOrphanCannotBeAnOrgsChild: an org may parent only
// a site (DAT-003). Creating the missing parent as an org for a group or a screen
// would swap SCOPE_NODE_PARENT_INVALID for SCOPE_NODE_PARENT_INVALID — the same
// broken surface, now with a row in it that lies about what it is.
func TestHealOrgRootDeclinesWhenTheOrphanCannotBeAnOrgsChild(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)
	dropOrgRow(t, dsn, seededOrgNodeID)
	withRawDB(t, dsn, func(db *sql.DB) {
		if _, err := db.Exec(`UPDATE scope_nodes SET body = replace(body, ?, ?) WHERE id = ?`,
			`"kind":"site"`, `"kind":"group"`, seededSiteNodeID); err != nil {
			t.Fatalf("re-kind the orphan: %v", err)
		}
	})
	before := dumpStore(t, dsn)

	s := openStoreAt(t, dsn)
	heal, err := s.HealOrgRoot(ctx)
	if err != nil {
		t.Fatalf("HealOrgRoot: %v", err)
	}
	if heal.Healed || heal.Needed {
		t.Fatalf("HealOrgRoot = %+v, want a refusal", heal)
	}
	if !strings.Contains(heal.Declined, "DAT-003") {
		t.Fatalf("HealOrgRoot declined with %q, want it to name the rule it is keeping", heal.Declined)
	}
	assertSameStore(t, before, dumpStore(t, dsn))
}

// TestOrgRootReportIsNeverSilentAboutAMissingWorkspace is the property the
// individual cases below are instances of, and the one that matters most: there
// is no store where WorkspaceRoot fails and this report says nothing.
//
// The section was added because a box can 404 every owner-gated route while
// looking perfectly healthy. A version of it that spoke up about the state the
// boot repairs by itself and went quiet on the states only a human can resolve
// would have the asymmetry exactly backwards — the silent ones are the ones where
// a sentence in the log is the whole remedy. Two states were silent: a tree with
// no org and nothing naming a missing parent, and a store emptied of scope nodes
// after the seed gate has closed behind it.
//
// It is written as an enumeration rather than as six separate tests so that a
// seventh state added to diagnoseOrgRoot has somewhere obvious to be listed, and
// so that "every no-workspace store" is asserted rather than sampled.
func TestOrgRootReportIsNeverSilentAboutAMissingWorkspace(t *testing.T) {
	ctx := context.Background()
	const otherMissingID = "01JCCCCCCCCCCCCCCCCCCCCCCC"

	for _, tc := range []struct {
		name        string
		build       func(t *testing.T, dsn string)
		wantNeeded  bool
		wantPhrases []string
	}{{
		name:       "the root the tree already names",
		build:      func(t *testing.T, dsn string) { dropOrgRow(t, dsn, seededOrgNodeID) },
		wantNeeded: true,
	}, {
		// No org row, and no node naming one — every parent_id either resolves or
		// is null. There is no id a root could be re-created under, so nothing is
		// written, but a store in this state never recovers on its own.
		name: "nothing names a missing parent",
		build: func(t *testing.T, dsn string) {
			dropOrgRow(t, dsn, seededOrgNodeID)
			withRawDB(t, dsn, func(db *sql.DB) {
				if _, err := db.Exec(`UPDATE scope_nodes SET body = replace(body, ?, ?) WHERE id = ?`,
					`"parent_id":"`+seededOrgNodeID+`"`, `"parent_id":null`, seededSiteNodeID); err != nil {
					t.Fatalf("null the orphan's parent: %v", err)
				}
			})
		},
		wantPhrases: []string{"no org-kind scope node", "no id to re-create the root under"},
	}, {
		// Reachable through supported API calls on a box already missing its org
		// row: only an ORG node is undeletable, so the remaining nodes can be
		// deleted one leaf at a time. Past generation 0 the seed can never re-fire,
		// so this is terminal.
		name: "no scope nodes at all, past the seed gate",
		build: func(t *testing.T, dsn string) {
			withRawDB(t, dsn, func(db *sql.DB) {
				if _, err := db.Exec(`DELETE FROM scope_nodes`); err != nil {
					t.Fatalf("empty the tree: %v", err)
				}
			})
		},
		wantPhrases: []string{"no scope nodes at all", "generation"},
	}, {
		name: "two candidate parents",
		build: func(t *testing.T, dsn string) {
			dropOrgRow(t, dsn, seededOrgNodeID)
			withRawDB(t, dsn, func(db *sql.DB) {
				if _, err := db.Exec(`UPDATE scope_nodes SET body = replace(body, ?, ?) WHERE id = ?`,
					`"`+seededSiteNodeID+`"`, `"`+otherMissingID+`"`, "01J8Z4DEM0SCREENF1RSTPH0TN"); err != nil {
					t.Fatalf("repoint the screen node: %v", err)
				}
			})
		},
		wantPhrases: []string{otherMissingID},
	}, {
		name: "the orphan cannot be an org's child",
		build: func(t *testing.T, dsn string) {
			dropOrgRow(t, dsn, seededOrgNodeID)
			withRawDB(t, dsn, func(db *sql.DB) {
				if _, err := db.Exec(`UPDATE scope_nodes SET body = replace(body, ?, ?) WHERE id = ?`,
					`"kind":"site"`, `"kind":"group"`, seededSiteNodeID); err != nil {
					t.Fatalf("re-kind the orphan: %v", err)
				}
			})
		},
		wantPhrases: []string{"DAT-003"},
	}, {
		name: "the missing id is also another table's row id",
		build: func(t *testing.T, dsn string) {
			regressToPreRuleIDs(t, dsn)
			const legacyOrgID = "01J8Z0DEMOORGANCESTORBOUND"
			dropOrgRow(t, dsn, legacyOrgID)
			withRawDB(t, dsn, func(db *sql.DB) {
				if _, err := db.Exec(
					`INSERT INTO playlists (id, revision, external_id, labels, scope_node, created_at, updated_at, body)
					 VALUES (?, 1, '', '{}', ?, 1, 1, ?)`,
					legacyOrgID, seededSiteNodeID,
					`{"id":"`+legacyOrgID+`","scope_node":"`+seededSiteNodeID+`","name":"Namesake","items":[],"revision":1,"created_at":1,"updated_at":1}`,
				); err != nil {
					t.Fatalf("plant the namesake playlist: %v", err)
				}
			})
		},
		wantPhrases: []string{"playlists"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := seededStore(t)
			tc.build(t, dsn)
			s := openStoreAt(t, dsn)

			// The premise: every one of these is a store whose owner surface is down.
			if _, _, err := s.WorkspaceRoot(ctx); !errors.Is(err, store.ErrNoWorkspace) {
				t.Fatalf("WorkspaceRoot = %v, want ErrNoWorkspace — this fixture is not a no-workspace store", err)
			}

			plan, err := s.PlanOrgRootHeal(ctx)
			if err != nil {
				t.Fatalf("PlanOrgRootHeal: %v", err)
			}
			if !plan.Needed && plan.Declined == "" {
				t.Fatalf("PlanOrgRootHeal = %+v: a store with no workspace and nothing said about it — "+
					"the boot log and -store-check are silent, and only a human can fix this one", plan)
			}
			if plan.Needed != tc.wantNeeded {
				t.Fatalf("PlanOrgRootHeal Needed = %v, want %v (%+v)", plan.Needed, tc.wantNeeded, plan)
			}
			for _, want := range tc.wantPhrases {
				if !strings.Contains(plan.Declined, want) {
					t.Errorf("the refusal %q does not carry %q", plan.Declined, want)
				}
			}

			// And the repair itself agrees with its own dry run, which is the only
			// reason an operator can trust the check before a restart.
			heal, err := s.HealOrgRoot(ctx)
			if err != nil {
				t.Fatalf("HealOrgRoot: %v", err)
			}
			if heal.Needed != plan.Needed || heal.Declined != plan.Declined {
				t.Fatalf("HealOrgRoot = %+v but PlanOrgRootHeal said %+v", heal, plan)
			}
		})
	}
}

// TestOrgRootReportStaysSilentBeforeTheFirstWrite is the other half of that
// property, and the reason it cannot simply be "always say something": a store at
// generation 0 has no scope nodes because SeedDemo has not run yet, and SeedDemo
// is the very next step of the same boot. A warning here would fire on every
// fresh box about a state that resolves microseconds later.
func TestOrgRootReportStaysSilentBeforeTheFirstWrite(t *testing.T) {
	ctx := context.Background()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "feeder-store.db"))

	if _, _, err := s.WorkspaceRoot(ctx); !errors.Is(err, store.ErrNoWorkspace) {
		t.Fatalf("WorkspaceRoot on an empty store = %v, want ErrNoWorkspace", err)
	}
	plan, err := s.PlanOrgRootHeal(ctx)
	if err != nil {
		t.Fatalf("PlanOrgRootHeal: %v", err)
	}
	if plan.Needed || plan.Declined != "" {
		t.Fatalf("PlanOrgRootHeal on a store awaiting its first write = %+v, want a zero report", plan)
	}
	if g := gen(t, s); g != 0 {
		t.Fatalf("generation = %d, want 0 — the seed runs only at 0", g)
	}
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	// And once seeded, it has a root and stays silent for the opposite reason.
	if plan, err := s.PlanOrgRootHeal(ctx); err != nil || plan.Needed || plan.Declined != "" {
		t.Fatalf("PlanOrgRootHeal after the seed = %+v, %v; want a zero report", plan, err)
	}
}

// TestHealOrgRootDeclinesWhenTwoParentsAreMissing: with two candidates there is
// no way to tell which one was the root, and a repair that guessed would attach
// half the tree to a node nobody meant. It refuses and says so, which is what
// leaves a human something to act on.
func TestHealOrgRootDeclinesWhenTwoParentsAreMissing(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)
	dropOrgRow(t, dsn, seededOrgNodeID)
	// A second orphan: the demo screen node, repointed at a node that is not there
	// either.
	const otherMissingID = "01JCCCCCCCCCCCCCCCCCCCCCCC"
	withRawDB(t, dsn, func(db *sql.DB) {
		if _, err := db.Exec(`UPDATE scope_nodes SET body = replace(body, ?, ?) WHERE id = ?`,
			`"`+seededSiteNodeID+`"`, `"`+otherMissingID+`"`, "01J8Z4DEM0SCREENF1RSTPH0TN"); err != nil {
			t.Fatalf("repoint the screen node: %v", err)
		}
	})
	before := dumpStore(t, dsn)

	s := openStoreAt(t, dsn)
	heal, err := s.HealOrgRoot(ctx)
	if err != nil {
		t.Fatalf("HealOrgRoot: %v", err)
	}
	if heal.Healed || heal.Needed {
		t.Fatalf("HealOrgRoot = %+v, want a refusal", heal)
	}
	for _, want := range []string{seededOrgNodeID, otherMissingID} {
		if !strings.Contains(heal.Declined, want) {
			t.Errorf("HealOrgRoot declined with %q, want it to name the candidate %q", heal.Declined, want)
		}
	}
	assertSameStore(t, before, dumpStore(t, dsn))
}
