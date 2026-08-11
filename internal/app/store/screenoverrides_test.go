package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// screenoverrides_test.go covers the PUSH-NOW override's storage half (parity
// row 5.7): that setting one is a DESIRED-STATE write (so the relays are nudged
// and the snapshot carries it), that clearing one is too, that neither can be
// made to point at a row that does not exist, and that clearing something that
// was never there is not a write at all.

const (
	soCastID      = "01J8ZSCRE3N0VERR1DECAST001"
	soOtherCastID = "01J8ZSCRE3N0VERR1DECAST002"
	soPlaylistID  = "01J8ZSCRE3N0VERR1DEPAY0001"
	// The seeded demo screen row's own placement (seed.go) — the node every
	// fixture row below is created at, so nothing trips the placement check.
	soScopeNode = "01J8Z4DEM0SCREENF1RSTPH0TN"
)

// soSeed opens a seeded store and adds a cast, a second cast and a playlist at
// the demo screen's own scope node, so a push has something real to name.
func soSeed(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	for _, id := range []string{soCastID, soOtherCastID} {
		body, err := json.Marshal(datamodel.Cast{
			ID: id, ScopeNode: soScopeNode, Name: "Push Fixture " + id,
			Slides: []datamodel.CastSlide{{ID: "s1", Layers: []wire.Layer{
				{Kind: "text", X: 10, Y: 10, W: 500, H: 100, Text: "hi", FontPx: 64, Color: "#ffffff"},
			}}},
		})
		if err != nil {
			t.Fatalf("marshal cast: %v", err)
		}
		if _, err := s.Create(ctx, store.KindCast, body); err != nil {
			t.Fatalf("create cast %s: %v", id, err)
		}
	}
	body, err := json.Marshal(datamodel.Playlist{
		ID: soPlaylistID, ScopeNode: soScopeNode, Name: "Push Fixture Playlist",
		Items: []datamodel.PlaylistItem{{Source: "cast", CastID: soCastID}},
	})
	if err != nil {
		t.Fatalf("marshal playlist: %v", err)
	}
	if _, err := s.Create(ctx, store.KindPlaylist, body); err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	return s, ctx
}

// TestSetScreenOverrideRidesDesiredStateAndBumpsGeneration: a push is desired
// state, not a side channel. The generation advances — which is the ONLY thing
// that causes a live relay to re-pull (REL-057) and therefore the only thing
// that makes "now" mean anything — and the very next DesiredState read carries
// the override for the snapshot projection to act on.
func TestSetScreenOverrideRidesDesiredStateAndBumpsGeneration(t *testing.T) {
	s, ctx := soSeed(t)

	before := gen(t, s)
	written, err := s.SetScreenOverride(ctx, store.SeedScreenID, soCastID, "")
	if err != nil {
		t.Fatalf("SetScreenOverride: %v", err)
	}
	if after := gen(t, s); after != before+1 {
		t.Fatalf("generation = %d after a push, want %d — without a new generation no relay ever re-pulls and the screen never changes", after, before+1)
	}
	if written.ScreenID != store.SeedScreenID || written.CastID != soCastID || written.PlaylistID != "" {
		t.Errorf("SetScreenOverride returned %+v, want the row it wrote", written)
	}
	if written.CreatedAt == 0 {
		t.Error("SetScreenOverride returned created_at 0 — the console shows an operator how long an override has been in force from this")
	}

	ds, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	got, ok := ds.ScreenOverrides[store.SeedScreenID]
	if !ok {
		t.Fatalf("DesiredState carries no override for the pushed screen (%d in total) — the snapshot projection would never see it", len(ds.ScreenOverrides))
	}
	if got.CastID != soCastID {
		t.Errorf("DesiredState override cast = %q, want %q", got.CastID, soCastID)
	}
}

// TestSetScreenOverrideReplacesRatherThanAccumulates: a screen has at most one
// override, and the latest instruction wins. Two operators pushing different
// casts at one screen within a second must leave the screen showing the later
// one — not two overrides to reconcile, and not a 412 for the second operator
// about a row they never read.
func TestSetScreenOverrideReplacesRatherThanAccumulates(t *testing.T) {
	s, ctx := soSeed(t)

	if _, err := s.SetScreenOverride(ctx, store.SeedScreenID, soCastID, ""); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if _, err := s.SetScreenOverride(ctx, store.SeedScreenID, soOtherCastID, ""); err != nil {
		t.Fatalf("second push: %v", err)
	}

	all, err := s.ScreenOverrides(ctx)
	if err != nil {
		t.Fatalf("ScreenOverrides: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("%d override(s) after two pushes at one screen, want 1", len(all))
	}
	if all[store.SeedScreenID].CastID != soOtherCastID {
		t.Errorf("override cast = %q after the second push, want %q (latest instruction wins)", all[store.SeedScreenID].CastID, soOtherCastID)
	}
}

// TestSetScreenOverrideRefusesRowsThatDoNotExist: an override naming a missing
// screen would be unclearable through the screen it names, and one naming a
// missing cast projects to an EMPTY program — a black screen an operator asked
// to show something on, with no error anywhere to explain it.
func TestSetScreenOverrideRefusesRowsThatDoNotExist(t *testing.T) {
	s, ctx := soSeed(t)

	if _, err := s.SetScreenOverride(ctx, "01J8ZN0SUCHSCREENR0W000001", soCastID, ""); !errors.Is(err, store.ErrScreenOverrideScreenUnknown) {
		t.Errorf("pushing at an unknown screen = %v, want ErrScreenOverrideScreenUnknown", err)
	}
	if _, err := s.SetScreenOverride(ctx, store.SeedScreenID, "01J8ZN0SUCHCASTR0W00000001", ""); !errors.Is(err, store.ErrScreenOverrideTargetUnknown) {
		t.Errorf("pushing an unknown cast = %v, want ErrScreenOverrideTargetUnknown", err)
	}
	if _, err := s.SetScreenOverride(ctx, store.SeedScreenID, "", "01J8ZN0SUCHPAY11ST00000001"); !errors.Is(err, store.ErrScreenOverrideTargetUnknown) {
		t.Errorf("pushing an unknown playlist = %v, want ErrScreenOverrideTargetUnknown", err)
	}

	// And nothing was written or generation-bumped by any of the three.
	all, err := s.ScreenOverrides(ctx)
	if err != nil {
		t.Fatalf("ScreenOverrides: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("%d override(s) persisted by refused pushes, want 0", len(all))
	}
}

// TestSetScreenOverrideRequiresExactlyOneTarget: "both" and "neither" are the
// two shapes a caller can be wrong in, and resolving either by precedence would
// turn a client bug into the wrong content on a wall.
func TestSetScreenOverrideRequiresExactlyOneTarget(t *testing.T) {
	s, ctx := soSeed(t)

	if _, err := s.SetScreenOverride(ctx, store.SeedScreenID, "", ""); err == nil {
		t.Error("pushing with neither a cast nor a playlist succeeded, want a refusal")
	}
	if _, err := s.SetScreenOverride(ctx, store.SeedScreenID, soCastID, soPlaylistID); err == nil {
		t.Error("pushing with BOTH a cast and a playlist succeeded, want a refusal")
	}
}

// TestClearScreenOverrideBumpsOnlyWhenSomethingWasCleared: clearing is a
// desired-state write when it changes something (the screen must fall back to
// its schedule, which needs a new generation to reach the relays) and is NOT one
// when it does not — a double-click on "return to schedule" must not make every
// relay on the site re-pull and re-resolve for a change that did not happen.
func TestClearScreenOverrideBumpsOnlyWhenSomethingWasCleared(t *testing.T) {
	s, ctx := soSeed(t)
	if _, err := s.SetScreenOverride(ctx, store.SeedScreenID, soCastID, ""); err != nil {
		t.Fatalf("push: %v", err)
	}

	before := gen(t, s)
	cleared, err := s.ClearScreenOverride(ctx, store.SeedScreenID)
	if err != nil {
		t.Fatalf("ClearScreenOverride: %v", err)
	}
	if !cleared {
		t.Error("ClearScreenOverride reported nothing cleared, want true")
	}
	if after := gen(t, s); after != before+1 {
		t.Fatalf("generation = %d after clearing, want %d — without a new generation the screen never returns to its schedule", after, before+1)
	}
	all, err := s.ScreenOverrides(ctx)
	if err != nil {
		t.Fatalf("ScreenOverrides: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("%d override(s) remain after clearing, want 0", len(all))
	}

	// The second clear: same resulting state, no write.
	before = gen(t, s)
	cleared, err = s.ClearScreenOverride(ctx, store.SeedScreenID)
	if err != nil {
		t.Fatalf("second ClearScreenOverride: %v", err)
	}
	if cleared {
		t.Error("the second clear reported something cleared, want false")
	}
	if after := gen(t, s); after != before {
		t.Errorf("generation = %d after a no-op clear, want %d unchanged", after, before)
	}
}

// TestScreenOverridesSurviveAReopen: the override is durable, which is the whole
// argument for making a push desired state rather than a live frame. A screen
// showing an emergency notice must still be showing it after the box reboots.
func TestScreenOverridesSurviveAReopen(t *testing.T) {
	dir := t.TempDir()
	dsn := dir + "/store.db"

	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	body, err := json.Marshal(datamodel.Cast{
		ID: soCastID, ScopeNode: soScopeNode, Name: "Durable Push",
		Slides: []datamodel.CastSlide{{ID: "s1", Layers: []wire.Layer{
			{Kind: "text", X: 10, Y: 10, W: 500, H: 100, Text: "hi", FontPx: 64, Color: "#ffffff"},
		}}},
	})
	if err != nil {
		t.Fatalf("marshal cast: %v", err)
	}
	if _, err := s.Create(ctx, store.KindCast, body); err != nil {
		t.Fatalf("create cast: %v", err)
	}
	if _, err := s.SetScreenOverride(ctx, store.SeedScreenID, soCastID, ""); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	all, err := reopened.ScreenOverrides(ctx)
	if err != nil {
		t.Fatalf("ScreenOverrides after reopen: %v", err)
	}
	if all[store.SeedScreenID].CastID != soCastID {
		t.Fatalf("override after reopen = %+v, want the pushed cast — a push that evaporates on reboot is a push nobody can rely on", all[store.SeedScreenID])
	}
}
