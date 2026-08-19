package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// orgroot.go re-creates the one row a whole store can NAME and never HOLD: the
// org-kind scope node at the top of the tree.
//
// # The state it repairs
//
// From the commit that introduced the make-dev seed until the one that fixed it
// ten days later, SeedDemo inserted a Demo Site whose parent_id named an "org
// ancestor" it never inserted — a virtual boundary, tolerated because the write
// path validated a SUBTREE, where an unresolvable parent is just the edge of the
// piece you were handed. When the store started judging writes against the whole
// tree, that seed became a store with a permanently dangling parent_id and no
// org row at all. SeedDemo runs only at generation 0, so a box seeded in that
// window can never acquire the row by seeding again: it is stuck with a
// two-node tree forever.
//
// What that costs is out of all proportion to a missing fixture row. The org
// node IS the workspace's identity (workspace.go), so WorkspaceRoot returns
// ErrNoWorkspace, and every owner-gated route authorizes through it —
// /system-health included, which takes relay health down with the whole owner
// surface. The box otherwise looks perfectly healthy: it serves screens, derives
// programs, and 404s the operator.
//
// # Why the store is allowed to repair this, when it is not allowed to repair a row
//
// priorfaults.go states the standing rule in as many words: an inherited fault is
// REPORTED, never hidden and never "repaired"; quarantining an operator's row to
// satisfy a validator written after it was authored is rejected outright. This
// heal does not weaken that rule, because it is the opposite act. Quarantining
// DESTROYS content the store cannot second-guess — "your playlist has a bad
// slide" becomes "your playlist is missing an item". This CREATES the row every
// remaining node already names, edits no authored field, and deletes nothing. The
// store is not guessing what an operator meant; it is materializing the node its
// own rows assert exists.
//
// It also does not go looking for work. It fires on exactly one diagnosis — no
// org row, and a single missing parent that only site-kind nodes name — and
// declines, loudly, on anything it cannot read unambiguously. A repair that
// guessed would be worse than the 404, because the 404 is at least honest.
//
// # Where the mechanics come from
//
// The shape (an idempotent repair on the load path, a fail-closed predicate, a
// row identity everything else pins preserved rather than re-minted, persisted
// so the next boot is a plain load) is the serving-leaf heal in
// internal/feeder/signing. The mechanics for a STORE row (plan under the read
// lock, re-plan inside the write transaction, capturePriorFaults before and
// validateAfterWrite after, bumpGeneration, the commit hook fired by hand
// because runWriteTx does not) are MigrateRowIDs'. The one property of the TLS
// heal deliberately NOT copied is its silence: it repairs without a word, which
// is affordable for a certificate nobody pins by value and is not affordable
// here. A repair nobody is told about is how a store drifts into a state nobody
// knows is broken — half of this very defect was a log line that could not tell
// a real row from a phantom.

// healedOrgRootName is the name an org root re-created here carries when the id
// it is being minted at is not one this build's seed knows.
//
// Deliberately descriptive rather than invented: a scope node MUST carry a
// non-empty name (DAT-001a), the platform has no idea what this workspace is
// called, and a name that claimed otherwise would be the repair asserting
// something it does not know. It is an ordinary, editable field the moment the
// owner surface comes back.
const healedOrgRootName = "Workspace Root"

// OrgRootHeal reports what HealOrgRoot did — or, from PlanOrgRootHeal, what it
// would do. A zero value means the store has an org root (or no rows at all) and
// nothing was written.
type OrgRootHeal struct {
	// Needed is true when the store has no org root and one can be re-created
	// from what the remaining rows name.
	Needed bool
	// Healed is true when the row was actually inserted. PlanOrgRootHeal never
	// sets it; HealOrgRoot sets it whenever Needed is set and the write committed.
	Healed bool

	// OrgID is the id the org row carries (or would). It equals ReferencedID
	// unless that id is not a canonical ULID, in which case the row is minted at
	// its canonical fold and every reference to it is repointed in the SAME
	// transaction.
	OrgID string
	// ReferencedID is the id the orphaned nodes name today.
	ReferencedID string
	// ChildIDs are the orphaned nodes, in id order.
	ChildIDs []string

	// Declined is non-empty when the store has no org root, something names one,
	// and the repair refused to invent it — carrying the reason, in operator
	// language, because this is the case a human has to resolve.
	Declined string

	// PresentID is the id of the org node the store ALREADY has, and it is the
	// conforming answer stated rather than implied. A zero value used to mean
	// three different things at once — the root is there, or the store has no
	// scope nodes and is still awaiting its seed, or the read found nothing to
	// say — so `-store-check`'s org-root section printed NOTHING on a healthy
	// store, which is the one rule the rest of that report keeps ("nothing to
	// report" and "the check did not look" must not read the same).
	PresentID string
}

// orgRootPlan is the diagnosis: an empty orgID means "do nothing", with declined
// distinguishing "nothing is wrong" from "something is wrong and I will not
// guess at it".
type orgRootPlan struct {
	orgID        string
	referencedID string
	children     []string
	declined     string
	// presentID is the org node the store already has — "nothing to do" with the
	// reason, so a reader can distinguish it from "nothing to say".
	presentID string
}

// diagnoseOrgRoot decides, from the scope-node rows alone, whether the store is
// missing an org root the rest of the tree already names — and, if so, under
// which id the row must be re-created.
//
// Every branch that returns without a plan is a deliberate refusal, and each one
// is a state where inserting a root would trade the fault for a different fault
// rather than clear it.
func diagnoseOrgRoot(nodes []datamodel.ScopeNode) orgRootPlan {
	// A store with no scope nodes at all cannot be judged from the tree, because
	// the tree is the thing that is missing: before the first write it is a store
	// waiting for SeedDemo, and after it, it is terminal. That distinction needs
	// the generation, which is not in this function's input — planOrgRootHeal
	// asks diagnoseEmptyStore instead and never reaches here.
	if len(nodes) == 0 {
		return orgRootPlan{}
	}

	stored := make(map[string]datamodel.ScopeNode, len(nodes))
	for _, n := range nodes {
		if n.Kind == orgNodeKind {
			// WorkspaceRoot already resolves, so the surface this exists to
			// restore is up. Anything else the tree carries — including a
			// dangling parent somewhere below — is not this repair's business: a
			// second org row is DAT-002's multiple-org fault, one fault traded
			// for another. The migration reports the dangle; a human resolves it.
			return orgRootPlan{presentID: n.ID}
		}
		stored[n.ID] = n
	}

	orphans := map[string][]string{}
	for _, n := range nodes {
		if n.ParentID == nil {
			continue // a non-org with no parent is its own DAT-002 fault, and names nothing to create
		}
		if _, ok := stored[*n.ParentID]; ok {
			continue
		}
		orphans[*n.ParentID] = append(orphans[*n.ParentID], n.ID)
	}

	missing := make([]string, 0, len(orphans))
	for id := range orphans {
		missing = append(missing, id)
	}
	sort.Strings(missing)

	switch len(missing) {
	case 0:
		// No org row and nothing naming one. There is no id to re-create the root
		// UNDER, and a root minted at a fresh id would have nothing hanging off
		// it — a row that fixes no reference and is not the node anything meant.
		// A store whose nodes are all orphaned by a nil parent_id lands here too,
		// as does a tree whose every parent resolves but into a cycle rather than
		// a root (scopetree.go's group→group case).
		//
		// So it writes nothing — and it DECLINES rather than returning silence.
		// Every branch of this diagnosis is reached only when WorkspaceRoot
		// already fails, and this one is a state no automatic repair can follow;
		// the only thing between the box and a human is whether anything said so.
		// A report that spoke up about the state the boot heals by itself and
		// went quiet on the state that needs a person would have the asymmetry
		// exactly backwards.
		return orgRootPlan{declined: fmt.Sprintf(
			"there is no org-kind scope node, and none of the %d stored scope node(s) names a parent that is missing, so there is no id to re-create the root under; a root minted at a fresh id would have nothing hanging off it",
			len(nodes))}
	case 1:
	default:
		return orgRootPlan{declined: fmt.Sprintf(
			"%d different scope nodes are named as parents but do not exist (%s); which of them was the tree's org root is not something this repair can know",
			len(missing), strings.Join(missing, ", "))}
	}

	referenced := missing[0]
	children := append([]string(nil), orphans[referenced]...)
	sort.Strings(children)

	// DAT-003: an org may parent a SITE and nothing else. A screen or a group
	// naming the missing node means the node was not an org, and re-creating it
	// as one would swap SCOPE_NODE_PARENT_INVALID (DAT-002, parent does not
	// exist) for SCOPE_NODE_PARENT_INVALID (DAT-003, kind not permitted under its
	// parent) — the same broken surface, now with a row in it that lies about
	// what it is.
	for _, id := range children {
		if kind := stored[id].Kind; kind != "site" {
			return orgRootPlan{declined: fmt.Sprintf(
				"the node %q that names the missing parent %q is of kind %q, and an org-kind node may parent only a site (DAT-003); re-creating the parent as an org would replace one validation fault with another",
				id, referenced, kind)}
		}
	}

	orgID := referenced
	if !ulid.Valid(orgID) {
		// The one place a dangling reference may be rewritten: the row it names is
		// being created beside it, in the same transaction, so the reference and
		// its target move together. Folding is what makes the repair possible at
		// all — DAT-005a means no row can ever be created at a non-canonical id,
		// so a root minted at the referenced spelling would be a fresh stored
		// fault, and one minted at an unrelated id would attach to nothing.
		folded, ok := foldToCanonicalULID(orgID)
		if !ok {
			return orgRootPlan{declined: fmt.Sprintf(
				"the missing parent %q is not a canonical ULID and cannot be folded into one (DAT-005a), so no row can be created to satisfy the %d node(s) naming it",
				referenced, len(children))}
		}
		orgID = folded
	}
	if _, taken := stored[orgID]; taken {
		return orgRootPlan{declined: fmt.Sprintf(
			"the missing parent %q folds onto %q, which is already a stored scope node; re-creating the root there would collide with an existing row",
			referenced, orgID)}
	}

	return orgRootPlan{orgID: orgID, referencedID: referenced, children: children}
}

// diagnoseEmptyStore decides what to say about a store holding no scope nodes at
// all — the one state the tree cannot answer, because the tree is what is
// missing. The generation is what separates the two halves of it.
//
// At generation 0 it is not a fault: it is a store before its first write, and
// SeedDemo — the very next step of the same boot — is what fills it. This must
// stay a strictly SILENT no-op there. A row written here would carry the
// generation past 0 and suppress the seed outright, and a warning printed here
// would fire on every fresh box about a state that resolves microseconds later.
//
// Past generation 0 it is terminal and, until this line, invisible: the seed
// gate is `generation == 0` so the seed can never re-fire, nothing names a
// parent so there is no id to re-create a root under, and WorkspaceRoot fails so
// every owner-gated route 404s. It is reachable through supported API calls on a
// box already in the #193 state — with no org row, the remaining site and screen
// nodes are ordinary deletable rows (only an ORG node is undeletable, DAT-022),
// so an operator tidying up the demo can empty the tree one leaf at a time and
// arrive here.
func diagnoseEmptyStore(ctx context.Context, q rowQueryer) (orgRootPlan, error) {
	generation, err := readGeneration(ctx, q)
	if err != nil {
		return orgRootPlan{}, err
	}
	if generation == 0 {
		return orgRootPlan{}, nil
	}
	return orgRootPlan{declined: fmt.Sprintf(
		"this store holds no scope nodes at all and its generation is %d, so the demo seed (which fills a store only at generation 0) will never run again and nothing names an id a root could be re-created under; restoring a scope node is the only way back",
		generation)}, nil
}

// rowQueryer is queryer plus the single-row read diagnoseEmptyStore needs: the
// generation lives in `meta`, not in the scope tree. Both *sql.DB and *sql.Tx
// satisfy it, so the read-lock plan and the in-transaction re-plan run the same
// code, as everywhere else in this file.
type rowQueryer interface {
	queryer
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// planOrgRootHeal is diagnoseOrgRoot plus the two checks that need the rest of
// the store rather than the scope tree: the empty-store case, which turns on the
// generation, and — on the folding path — that the referenced id is not also
// some other row's id.
//
// That check exists because of how the rewrite is applied. applyIDRewrites
// remaps the id COLUMN of every table it walks, not only the table its entry
// names — which is sound for the migration, whose plan comes from a full scan of
// every table (a shared id is planned once per table holding it, onto the same
// deterministic fold), and is NOT sound for the single entry this repair
// synthesizes. A playlist that happened to carry the missing node's id would be
// renamed by a repair that had no business touching it. So the repair refuses
// instead: the whole of its license is that it creates a row and changes nothing
// else.
func planOrgRootHeal(ctx context.Context, q rowQueryer) (orgRootPlan, error) {
	nodes, err := readScopeNodes(ctx, q)
	if err != nil {
		return orgRootPlan{}, err
	}
	if len(nodes) == 0 {
		return diagnoseEmptyStore(ctx, q)
	}
	plan := diagnoseOrgRoot(nodes)
	if plan.orgID == "" || plan.orgID == plan.referencedID {
		return plan, nil // nothing to do, or nothing to rewrite
	}
	for _, kind := range allKinds {
		ids, err := readIDs(ctx, q, string(kind))
		if err != nil {
			return orgRootPlan{}, err
		}
		for _, id := range ids {
			if id == plan.referencedID {
				return orgRootPlan{declined: fmt.Sprintf(
					"the missing parent %q is also the id of a stored %s row; repointing the reference at %q would rename that row too",
					plan.referencedID, kind, plan.orgID)}, nil
			}
		}
	}
	return plan, nil
}

// PlanOrgRootHeal reports what HealOrgRoot would do, writing nothing — the
// read-only half an operator gets from `waiveo-feeder -store-check` before
// restarting a box.
func (s *Store) PlanOrgRootHeal(ctx context.Context) (OrgRootHeal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	plan, err := planOrgRootHeal(ctx, s.db)
	if err != nil {
		return OrgRootHeal{}, err
	}
	return plan.report(false), nil
}

// report turns a diagnosis into the caller-facing value. healed says whether the
// write actually happened, which is the only thing the plan and the repair
// disagree about.
func (p orgRootPlan) report(healed bool) OrgRootHeal {
	if p.orgID == "" {
		return OrgRootHeal{Declined: p.declined, PresentID: p.presentID}
	}
	return OrgRootHeal{
		Needed: true, Healed: healed,
		OrgID: p.orgID, ReferencedID: p.referencedID, ChildIDs: p.children,
	}
}

// HealOrgRoot re-creates the missing org-kind scope node the store's remaining
// nodes already name, and returns what it did.
//
// It is safe to call on every boot: a store that has an org root — every store
// this build seeds — is not written to at all. No transaction is opened, no
// generation is bumped, no commit hook fires. It is idempotent by construction
// rather than by bookkeeping: the row it writes is the very thing its own
// predicate tests for, so the next call sees an org node and stands down.
//
// The generation IS advanced when a row is written, for the same reason
// MigrateRowIDs advances it: the scope tree is part of the desired state a relay
// caches by generation, and a store that gained a node while keeping its old
// generation would leave connected relays on the tree without it.
func (s *Store) HealOrgRoot(ctx context.Context) (OrgRootHeal, error) {
	// Diagnosed under the read lock first, purely so a healthy store never even
	// takes the write lock at boot. The write path re-diagnoses under its own
	// lock, so this is an optimization and not the decision.
	s.mu.RLock()
	plan, err := planOrgRootHeal(ctx, s.db)
	s.mu.RUnlock()
	if err != nil {
		return OrgRootHeal{}, err
	}
	if plan.orgID == "" {
		return plan.report(false), nil
	}

	var out OrgRootHeal
	if err := s.runWriteTx(ctx, func(tx *sql.Tx) error {
		plan, err := planOrgRootHeal(ctx, tx)
		if err != nil {
			return err
		}
		if plan.orgID == "" {
			out = plan.report(false)
			return nil
		}
		// Captured BEFORE the insert, so the repair is judged on what IT
		// introduced (priorfaults.go). That matters here more than anywhere: a
		// box that has been running without its root has had time to accumulate
		// unrelated inherited faults, and a heal that had to wait for a
		// spotlessly clean store would never run on the boxes that need it.
		prior, err := capturePriorFaults(ctx, tx, KindScopeNode)
		if err != nil {
			return err
		}
		if err := insertOrgRoot(ctx, tx, plan.orgID, s.nowMs()); err != nil {
			return err
		}
		if plan.orgID != plan.referencedID {
			// One rewrite, in the same transaction as the row it names — the
			// invariant idmigrate.go's planner now refuses to violate, kept here
			// by satisfying it rather than by exempting this path from it.
			// applyIDRewrites is reused rather than a hand-written UPDATE so the
			// reference is repointed everywhere identifiers are stored, not just
			// in the children this diagnosis happened to look at.
			if err := applyIDRewrites(ctx, tx, []IDRewrite{{
				Kind: KindScopeNode, From: plan.referencedID, To: plan.orgID,
			}}); err != nil {
				return err
			}
		}
		// The write path's OWN validator, not a second opinion written here: if
		// the row this inserted does not produce a conformant tree, nothing
		// commits and the store is exactly as it was.
		if err := validateAfterWrite(ctx, tx, KindScopeNode, "", prior); err != nil {
			return fmt.Errorf("store: the re-created org scope node did not validate: %w", err)
		}
		if err := bumpGeneration(ctx, tx); err != nil {
			return err
		}
		out = plan.report(true)
		return nil
	}); err != nil {
		return OrgRootHeal{}, err
	}

	if out.Healed {
		// runWriteTx does not fire the commit hook (writeTx does), so a repair
		// that changed the desired state nudges connected relays itself — and one
		// that changed nothing stays silent.
		s.hookMu.Lock()
		hook := s.onCommit
		s.hookMu.Unlock()
		if hook != nil {
			hook()
		}
	}
	return out, nil
}

// insertOrgRoot writes the org row at id, in the caller's transaction.
//
// The body is completed by the SAME two steps Create runs — injectDeclaredMembers
// (every member api/openapi.yaml declares required, materialized) and
// injectBaseline (revision/created_at/updated_at) — rather than hand-assembled,
// so the row a GET serves after a repair is the same representation an authored
// one would be. reinsertPurgedOrg's tombstone hand-assembles its body and is a
// standing reminder of what that costs: it carries no `labels`, a member the
// schema declares required.
func insertOrgRoot(ctx context.Context, tx *sql.Tx, id string, now int64) error {
	body, err := json.Marshal(orgRootScopeNode(id, orgRootName(id)))
	if err != nil {
		return fmt.Errorf("store: build the org scope node: %w", err)
	}
	if body, err = injectDeclaredMembers(KindScopeNode, body); err != nil {
		return fmt.Errorf("store: build the org scope node: %w", err)
	}
	if body, err = injectBaseline(body, 1, now, now); err != nil {
		return fmt.Errorf("store: build the org scope node: %w", err)
	}
	// scope_nodes is the one resource table that never carries a placement
	// (DAT-006), so its scope_node column is empty rather than a parent pointer —
	// the parent lives in the body.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO scope_nodes (id, revision, external_id, labels, scope_node, created_at, updated_at, body)
		 VALUES (?, 1, '', '{}', '', ?, ?, ?)`,
		id, now, now, string(body),
	); err != nil {
		return fmt.Errorf("store: insert the org scope node %q: %w", id, err)
	}
	return nil
}

// orgRootName names a re-created root. When the id is the one this build's seed
// gives its own org ancestor, the seed's name is used: that store IS a make-dev
// seed missing its root — the whole population this repair exists for — and a
// healed box should end up with the tree a freshly seeded box has, not a
// near-miss of it.
func orgRootName(id string) string {
	if id == seedOrgAncestorScopeNodeID {
		return seedOrgName
	}
	return healedOrgRootName
}
