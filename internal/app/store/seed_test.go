package store_test

import (
	"context"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// seedAssetRef is a fixture content id the demo playlist item points at; the
// exact digest is immaterial to the store (ValidateRows does not parse asset_ref
// bytes), it only needs the sha256: grammar shape.
const seedAssetRef = "sha256:3a5439d0a1f4b2c6e7889900aabbccddeeff00112233445566778899aabbccdd"

// TestSeedDemoInsertsResolvableProgram: SeedDemo lands the whole demo as store
// rows that validate cleanly, advancing the generation once per row (a fresh
// store reaches generation 7), and the derived desired-state read carries a
// scope tree that validates and a site_effective taken from the site node's own
// tz/lat/long (DAT-033), never box-local state.
func TestSeedDemoInsertsResolvableProgram(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if g := gen(t, s); g != 0 {
		t.Fatalf("fresh store generation = %d, want 0", g)
	}
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	if g := gen(t, s); g != 7 {
		t.Fatalf("generation after seed = %d, want 7 (2 scope nodes + playlist + schedule + preset + 2 dayparts)", g)
	}

	nodes, rows, se, generation, err := s.DesiredStateRows(ctx)
	if err != nil {
		t.Fatalf("DesiredStateRows: %v", err)
	}
	if generation != 7 {
		t.Fatalf("DesiredStateRows generation = %d, want 7", generation)
	}

	// The scheduling rows validate as a complete, referentially-sound set.
	if _, errs := datamodel.ValidateRows(rows); len(errs) != 0 {
		t.Fatalf("seeded scheduling rows failed validation: %+v", errs)
	}
	// The scope tree validates.
	if _, errs := datamodel.BuildScopeTree(nodes); len(errs) != 0 {
		t.Fatalf("seeded scope tree failed validation: %+v", errs)
	}

	if len(nodes) != 2 {
		t.Fatalf("seeded scope nodes = %d, want 2 (site + screen)", len(nodes))
	}
	if len(rows.Playlists) != 1 || len(rows.Schedules) != 1 || len(rows.Dayparts) != 2 || len(rows.PresetBatches) != 1 {
		t.Fatalf("seeded rows = playlists:%d schedules:%d dayparts:%d presets:%d, want 1/1/2/1",
			len(rows.Playlists), len(rows.Schedules), len(rows.Dayparts), len(rows.PresetBatches))
	}

	// site_effective comes from the site node's own placement columns (DAT-033).
	if se.TZ != siteTZ || se.Lat != siteLat || se.Long != siteLong {
		t.Fatalf("site_effective = %+v, want tz=%s lat=%v long=%v from the site node (DAT-033)", se, siteTZ, siteLat, siteLong)
	}
}

// TestSeedDemoSiteMatchesFeederBinding locks the invariant that the seeded site
// node is the SAME scope node the feeder's hello site_binding names — so the
// authored demo and the relay's adopted site never disagree.
func TestSeedDemoSiteMatchesFeederBinding(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	// siteNodeID (the store test's fixture) IS the id the feeder's firstPhotonSite
	// binds to; a hit here proves SeedDemo used exactly that id for its site node.
	got, ok, err := s.Get(ctx, store.KindScopeNode, siteNodeID)
	if err != nil || !ok {
		t.Fatalf("Get seeded site node %q: ok=%v err=%v (SeedDemo must key the site node on firstPhotonSite.ScopeNode)", siteNodeID, ok, err)
	}
	if got.ID != siteNodeID {
		t.Fatalf("seeded site node id = %q, want %q", got.ID, siteNodeID)
	}
}

// TestDesiredStateBundlesTheRows: DesiredState returns exactly what
// DesiredStateRows does, as one value.
func TestDesiredStateBundlesTheRows(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	ds, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	if ds.Generation != 7 {
		t.Fatalf("DesiredState.Generation = %d, want 7", ds.Generation)
	}
	if len(ds.ScopeNodes) != 2 || len(ds.Rows.Dayparts) != 2 {
		t.Fatalf("DesiredState bundled nodes:%d dayparts:%d, want 2/2", len(ds.ScopeNodes), len(ds.Rows.Dayparts))
	}
	if ds.SiteEffective.TZ != siteTZ {
		t.Fatalf("DesiredState.SiteEffective.TZ = %q, want %q", ds.SiteEffective.TZ, siteTZ)
	}
}
