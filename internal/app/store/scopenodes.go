package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// readScopeNodes loads every scope-node row body, ordered by id, into typed
// datamodel.ScopeNode values — the input BuildScopeTree and the desired-state
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

// DesiredStateRows returns the store's current desired-state inputs the feeder
// derives its signed snapshot from (the schedule section, REL-065): the scope
// nodes, the six scheduling-core row kinds as datamodel.RawRows, the site's
// effective tz/lat/long, and the store generation — all read under one read lock
// so they form a single consistent snapshot at that generation.
//
// site_effective is taken from the SITE scope node's OWN placement columns
// (DAT-033) — never from the feeder's OS locale (the no-box-local-desired-state
// rule). A site node always carries all three geo columns non-null (BuildScopeTree
// enforced it at write time). If no site node is present the zero SiteEffective
// is returned.
func (s *Store) DesiredStateRows(ctx context.Context) (scopeNodes []datamodel.ScopeNode, rows datamodel.RawRows, siteEffective wire.SiteEffective, generation int64, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	scopeNodes, err = readScopeNodes(ctx, s.db)
	if err != nil {
		return nil, datamodel.RawRows{}, wire.SiteEffective{}, 0, err
	}
	rows, err = readRawRows(ctx, s.db)
	if err != nil {
		return nil, datamodel.RawRows{}, wire.SiteEffective{}, 0, err
	}
	generation, err = readGeneration(ctx, s.db)
	if err != nil {
		return nil, datamodel.RawRows{}, wire.SiteEffective{}, 0, err
	}

	siteEffective = deriveSiteEffective(scopeNodes)
	return scopeNodes, rows, siteEffective, generation, nil
}

// DesiredStateResult bundles a single consistent DesiredStateRows read into one
// value — the input the feeder's snapshot.BuildFromStore consumes to derive a
// signed desired-state generation. Its fields are exactly the DesiredStateRows
// tuple (the scope nodes, the six scheduling-core row kinds, the site's effective
// tz/lat/long from the SITE node per DAT-033, and the store generation the read
// was taken at), so BuildFromStore never needs box-local state.
type DesiredStateResult struct {
	ScopeNodes    []datamodel.ScopeNode
	Rows          datamodel.RawRows
	SiteEffective wire.SiteEffective
	Generation    int64
}

// DesiredState is DesiredStateRows returned as one DesiredStateResult value — the
// single-argument form snapshot.BuildFromStore takes. Like DesiredStateRows it
// reads scope nodes, scheduling rows, the site effective placement, and the
// generation under one read lock, so the returned result is a consistent snapshot
// at that generation.
func (s *Store) DesiredState(ctx context.Context) (DesiredStateResult, error) {
	nodes, rows, se, gen, err := s.DesiredStateRows(ctx)
	if err != nil {
		return DesiredStateResult{}, err
	}
	return DesiredStateResult{ScopeNodes: nodes, Rows: rows, SiteEffective: se, Generation: gen}, nil
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
