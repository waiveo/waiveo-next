package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// readScopeNodes loads every scope-node row body, ordered by id, into typed
// datamodel.ScopeNode values — the input the tree builders and the desired-state
// derivation consume.
func readScopeNodes(ctx context.Context, q queryer) ([]datamodel.ScopeNode, error) {
	bodies, err := readBodies(ctx, q, string(KindScopeNode))
	if err != nil {
		return nil, err
	}
	nodes := make([]datamodel.ScopeNode, 0, len(bodies))
	for _, b := range bodies {
		var n datamodel.ScopeNode
		if err := json.Unmarshal(b, &n); err != nil {
			return nil, fmt.Errorf("store: decode scope node: %w", err)
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// readScreens loads every screen identity row body, ordered by id, into typed
// datamodel.Screen values — the input the desired-state derivation resolves ONE
// screen_programs entry per (internal/feeder/snapshot).
//
// It is the screen half of readIdentityRows (scheduling.go), typed rather than
// raw: the write path validates the two identity tables together as raw bundles,
// while the read path here wants the screen rows alone, already parsed, because
// the only thing derivation needs from a screen is its id and its scope_node
// placement. Every row in this table has already passed ValidateIdentityRows at
// write time, so a decode failure here is a corrupted store, not authoring input.
func readScreens(ctx context.Context, q queryer) ([]datamodel.Screen, error) {
	bodies, err := readBodies(ctx, q, string(KindScreen))
	if err != nil {
		return nil, err
	}
	screens := make([]datamodel.Screen, 0, len(bodies))
	for _, b := range bodies {
		var s datamodel.Screen
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("store: decode screen row: %w", err)
		}
		screens = append(screens, s)
	}
	return screens, nil
}

// desiredStateRows is the unlocked core of DesiredStateRows: it reads the scope
// nodes and the six scheduling-core row kinds and derives site_effective, but
// takes no lock itself — every caller wraps it in its OWN read-lock section, so
// it can be composed with FURTHER reads (DesiredState adds the edge-rule-bodies
// read) inside a single critical section rather than stacking separately-locked
// calls.
//
// site_effective is taken from the SITE scope node's OWN placement columns
// (DAT-033) — never from the feeder's OS locale (the no-box-local-desired-state
// rule). A site node always carries all three geo columns non-null (the tree builder
// enforced it at write time). If no site node is present the zero SiteEffective
// is returned.
func desiredStateRows(ctx context.Context, q queryer) (scopeNodes []datamodel.ScopeNode, rows datamodel.RawRows, siteEffective wire.SiteEffective, err error) {
	scopeNodes, err = readScopeNodes(ctx, q)
	if err != nil {
		return nil, datamodel.RawRows{}, wire.SiteEffective{}, err
	}
	rows, err = readRawRows(ctx, q)
	if err != nil {
		return nil, datamodel.RawRows{}, wire.SiteEffective{}, err
	}
	siteEffective = deriveSiteEffective(scopeNodes)
	return scopeNodes, rows, siteEffective, nil
}

// DesiredStateRows returns the store's current desired-state inputs the feeder
// derives its signed snapshot from (the schedule section, REL-065): the scope
// nodes, the six scheduling-core row kinds as datamodel.RawRows, the site's
// effective tz/lat/long, and the store generation — all read under one read lock
// so they form a single consistent snapshot at that generation.
func (s *Store) DesiredStateRows(ctx context.Context) (scopeNodes []datamodel.ScopeNode, rows datamodel.RawRows, siteEffective wire.SiteEffective, generation int64, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	scopeNodes, rows, siteEffective, err = desiredStateRows(ctx, s.db)
	if err != nil {
		return nil, datamodel.RawRows{}, wire.SiteEffective{}, 0, err
	}
	generation, err = readGeneration(ctx, s.db)
	if err != nil {
		return nil, datamodel.RawRows{}, wire.SiteEffective{}, 0, err
	}
	return scopeNodes, rows, siteEffective, generation, nil
}

// ScopeNodes returns just the scope-node rows, read under the store's read lock
// so they form one consistent snapshot of the tree.
//
// It exists beside DesiredStateRows because two callers want the TREE and
// nothing else: SEC-010's binding inheritance (which node inherits from which)
// and events/1's per-subscriber visible set (EVT-120). Both would otherwise pull
// the six scheduling-core row kinds and the generation on every read purely to
// discard them — on a long-lived /events/v1 connection that is a whole
// desired-state read to answer a question about a handful of nodes.
func (s *Store) ScopeNodes(ctx context.Context) ([]datamodel.ScopeNode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readScopeNodes(ctx, s.db)
}

// DesiredStateResult bundles a single consistent DesiredStateRows read into one
// value — the input the feeder's snapshot.BuildFromStore consumes to derive a
// signed desired-state generation. Its fields are exactly the DesiredStateRows
// tuple (the scope nodes, the six scheduling-core row kinds, the site's effective
// tz/lat/long from the SITE node per DAT-033, and the store generation the read
// was taken at) PLUS EdgeRules — the store's edge-classified automations
// (Store.EdgeRuleBodies, REL-062) wire-shaped for BuildFromStore's edge_rules
// section — and DeviceInventory, the adopted-device rows projected into REL-063
// entries alongside the installed packs' declared discovery-match patterns
// (REL-064, deviceinventory.go) — and Screens, the screen identity rows whose
// placements the screen_programs section is resolved per (REL-061, DAT-004a) —
// so BuildFromStore never needs box-local state nor a hardcoded rule, device,
// pattern, or screen.
type DesiredStateResult struct {
	ScopeNodes      []datamodel.ScopeNode
	Rows            datamodel.RawRows
	SiteEffective   wire.SiteEffective
	EdgeRules       wire.EdgeRules
	DeviceInventory wire.DeviceInventory

	// Screens is every screen identity row (DAT-004/DAT-004a), ordered by id —
	// the rows a `screen_id` NAMES, each carrying the scope_node placement the
	// schedule-applicability cascade (DAT-051) is walked from to resolve THAT
	// screen's program. It is deliberately the screen ROWS and not the
	// `screen`-kind scope nodes: a screen row may hang off a node of any kind
	// and two screen rows may share one node, so the nodes cannot stand in for
	// the screens without losing (or duplicating) one.
	Screens []datamodel.Screen

	// PairingGrants is every stored pending pairing-grant record
	// (pairinggrants.go), wire-shaped for the snapshot's `pairing_grants`
	// section (relay/1 REL-067/REL-121a). Expiry and screen-row existence
	// are applied at derivation time (snapshot.BuildFromStore) — per-instant
	// concerns that do not belong in a plain read.
	PairingGrants []wire.PairingGrant

	// Revoked is every revoked screen id (revocations.go), for the snapshot's
	// `revocation_and_site.revoked` (relay/1 REL-066). Read inside DesiredState's
	// own lock section with everything else: a revocation bound to a different
	// generation than the content around it is the one section where being one
	// generation behind means a relay keeps honouring a credential an operator
	// has just withdrawn.
	Revoked []string

	// ScreenOverrides is every active push-now override, keyed by screen_id
	// (screenoverrides.go) — the "show this here now" assignment that outranks
	// whatever the schedule would otherwise resolve for that screen, projected
	// by snapshot.DeriveScreenPrograms into a `preempt`-priority entry
	// (REL-061/PLY-108).
	//
	// Read inside DesiredState's own lock section with everything else, for the
	// reason Revoked is: an override bound to a different generation than the
	// content around it is a screen whose emergency notice and whose schedule
	// come from two different instants.
	ScreenOverrides map[string]ScreenOverride

	Generation int64
}

// DesiredState is DesiredStateRows returned as one DesiredStateResult value — the
// single-argument form snapshot.BuildFromStore takes. It reads scope nodes,
// scheduling rows, the site effective placement, the store's edge-classified
// automations (the same query readEdgeRuleBodies/EdgeRuleBodies runs) so the
// result's EdgeRules field carries them wire-shaped (REL-062) — an app-classified
// rule is never included, only edge rules ride edge_rules — AND the device
// inventory (deviceInventory: the adopted-device rows projected into REL-063
// entries, plus the installed packs' declared discovery-match patterns, REL-064)
// AND the screen identity rows (readScreens) the screen_programs section is
// resolved one entry per (REL-061).
//
// All seven reads (scope nodes, scheduling rows, edge rule bodies, device
// inventory, screens, pairing grants, generation) happen inside ONE s.mu.RLock() section — not composed
// from DesiredStateRows, EdgeRuleBodies and DeviceInventory's own separate lock
// sections. Composing those public, independently-locked methods would let a
// write commit (and bump the shared generation) in the gap between one RUnlock
// and the next RLock: the result would then carry one read's generation alongside
// a LATER read's content, binding a stale generation to fresher content and
// breaking the (generation, hash) signing invariant REL-053/075 depends on — a
// bug this composed form does not have, since no lock is ever released mid-read
// here.
func (s *Store) DesiredState(ctx context.Context) (DesiredStateResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodes, rows, se, err := desiredStateRows(ctx, s.db)
	if err != nil {
		return DesiredStateResult{}, err
	}
	bodies, err := readEdgeRuleBodies(ctx, s.db)
	if err != nil {
		return DesiredStateResult{}, err
	}
	inv, err := deviceInventory(ctx, s.db)
	if err != nil {
		return DesiredStateResult{}, err
	}
	screens, err := readScreens(ctx, s.db)
	if err != nil {
		return DesiredStateResult{}, err
	}
	grants, err := readPairingGrants(ctx, s.db)
	if err != nil {
		return DesiredStateResult{}, err
	}
	revoked, err := s.revokedSubjectsLocked(ctx, RevocationSubjectScreen)
	if err != nil {
		return DesiredStateResult{}, err
	}
	overrides, err := readScreenOverrides(ctx, s.db)
	if err != nil {
		return DesiredStateResult{}, err
	}
	generation, err := readGeneration(ctx, s.db)
	if err != nil {
		return DesiredStateResult{}, err
	}
	return DesiredStateResult{
		ScopeNodes:      nodes,
		Rows:            rows,
		SiteEffective:   se,
		EdgeRules:       wire.EdgeRules{RulesMinorVersion: rulesMinorVersion, Rules: bodies},
		DeviceInventory: inv,
		Screens:         screens,
		PairingGrants:   grants,
		Revoked:         revoked,
		ScreenOverrides: overrides,
		Generation:      generation,
	}, nil
}

// deriveSiteEffective reads the site node's own tz/lat/long (DAT-033). The first
// site node in id order is used; a store with no site node yields the zero value.
func deriveSiteEffective(nodes []datamodel.ScopeNode) wire.SiteEffective {
	for _, n := range nodes {
		if n.Kind == "site" && n.TZ != nil && n.Lat != nil && n.Long != nil {
			return wire.SiteEffective{TZ: *n.TZ, Lat: *n.Lat, Long: *n.Long}
		}
	}
	return wire.SiteEffective{}
}
