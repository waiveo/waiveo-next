package snapshot

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// screenoverride_test.go covers the PUSH-NOW override's PROJECTION half (parity
// row 5.7): what the signed snapshot carries for a screen an operator has pushed
// content to, and — just as importantly — that everything else about the section
// is unchanged for a screen nobody has.
//
// The override is the screen ROW's own `override` member (DAT-004c), so these
// tests impose it the way every other surface does: a conditional patch of that
// one member through the ordinary resource update. There is no second store to
// write to.

const (
	// A cast created at the seeded screen's own scope node, so a push has
	// something real to name. Deliberately a different asset from the seed's, so
	// "the pushed content reached the wire" cannot be satisfied by the schedule.
	ovCastID    = "01J8Z0VERR1DECASTF0RP5SH01"
	ovPushAsset = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	ovScopeNode = "01J8Z4DEM0SCREENF1RSTPH0TN"
)

// pushOverride imposes an override on a screen through the ordinary conditional
// resource update — the same single write path the api surface and the run-now
// signage sink both take (there is exactly ONE override store, and this is it).
func pushOverride(t *testing.T, s *store.Store, screenID string, o *datamodel.ScreenOverride) {
	t.Helper()
	ctx := context.Background()
	res, found, err := s.Get(ctx, store.KindScreen, screenID)
	if err != nil || !found {
		t.Fatalf("read screen %s before the push: found=%v err=%v", screenID, found, err)
	}
	patch, err := json.Marshal(map[string]any{"override": o})
	if err != nil {
		t.Fatalf("marshal the override patch: %v", err)
	}
	if _, err := s.Update(ctx, store.KindScreen, screenID, res.Revision, patch); err != nil {
		t.Fatalf("write the override on %s: %v", screenID, err)
	}
}

// alertOn is the emergency push these tests drive: `mode: "alert"`, which is the
// mode DAT-004d assigns `preempt` priority to.
func alertOn(castID string) *datamodel.ScreenOverride {
	return &datamodel.ScreenOverride{Mode: datamodel.ScreenOverrideModeAlert, CastID: castID}
}

// ovSeedWithCast returns a seeded store that additionally holds a two-slide cast
// at the seeded screen's placement, and that cast's id.
func ovSeedWithCast(t *testing.T, assetRef string) *store.Store {
	t.Helper()
	s := seededStore(t, assetRef)
	body, err := json.Marshal(datamodel.Cast{
		ID: ovCastID, ScopeNode: ovScopeNode, Name: "Emergency Notice",
		Slides: []datamodel.CastSlide{
			{ID: "notice", DurationMS: 5000, Layers: []wire.Layer{
				{Kind: "rect", X: 0, Y: 0, W: 1920, H: 1080, Color: "#b00020"},
				{Kind: "text", X: 100, Y: 400, W: 1720, H: 200, Text: "FIRE DRILL", FontPx: 160, Color: "#ffffff"},
			}},
			{ID: "assembly", Layers: []wire.Layer{
				{Kind: "image", X: 0, Y: 0, W: 1920, H: 1080, AssetRef: ovPushAsset},
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal the push cast: %v", err)
	}
	if _, err := s.Create(context.Background(), store.KindCast, body); err != nil {
		t.Fatalf("create the push cast: %v", err)
	}
	return s
}

// TestPushedCastProjectsToAPreemptProgramCarryingItsSlides is the projection
// this whole capability rests on: the screen's entry in the signed snapshot
// stops being the schedule's and becomes the operator's, at `preempt` priority
// so the relay's fence protects it and the player interrupts for it.
func TestPushedCastProjectsToAPreemptProgramCarryingItsSlides(t *testing.T) {
	const seedAsset = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	s := ovSeedWithCast(t, seedAsset)

	// Before: the ordinary scheduled program, so nothing below can pass vacuously.
	before, _ := DeriveScreenPrograms(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, contentInstant(t))
	beforeProg := programForScreen(t, before, store.SeedScreenID)
	if beforeProg.Priority != "scheduled" {
		t.Fatalf("fixture: the un-pushed screen derives priority %q, want scheduled", beforeProg.Priority)
	}
	wantSeededAssetThenSlide(t, beforeProg.Content, seedAsset)

	pushOverride(t, s, store.SeedScreenID, alertOn(ovCastID))

	after, errs := DeriveScreenPrograms(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, contentInstant(t))
	if len(errs) != 0 {
		t.Fatalf("deriving a pushed screen reported %+v, want none", errs)
	}
	prog := programForScreen(t, after, store.SeedScreenID)

	if prog.Priority != "preempt" {
		t.Errorf("pushed program priority = %q, want preempt — at `scheduled` the relay's own re-resolution overwrites it within 30s and the player waits out the current item instead of interrupting",
			prog.Priority)
	}
	if !prog.Pinned {
		t.Error("pushed program is not Pinned — Pinned is what stops the relay re-resolving this screen from the schedule it also carries (DAT-004d), and it is the ONLY such guard for a `play` override, whose priority is deliberately `scheduled`")
	}
	if prog.Display != "content" {
		t.Errorf("pushed program display = %q, want content — an operator pushing a cast is saying show this", prog.Display)
	}
	if len(prog.Content) != 2 {
		t.Fatalf("pushed program carries %d content item(s), want 2 (one per cast slide): %+v", len(prog.Content), prog.Content)
	}
	for i, c := range prog.Content {
		if c.ContentType != "slide" {
			t.Errorf("pushed content[%d].content_type = %q, want slide", i, c.ContentType)
		}
	}
	// The slide's own duration_ms rides through; the second slide states none, so
	// it carries none (the player's own default) — there is no playlist item here
	// to source a DAT-042 override from.
	if prog.Content[0].DurationMS != 5000 {
		t.Errorf("pushed content[0].duration_ms = %d, want 5000 (the slide's own)", prog.Content[0].DurationMS)
	}
	if prog.Content[1].DurationMS != 0 {
		t.Errorf("pushed content[1].duration_ms = %d, want 0 (no override, no playlist item to carry one)", prog.Content[1].DurationMS)
	}
	// The pushed asset actually reached the wire with a fetchable URL — a slide
	// whose image layer had no resolvable URL would have been dropped outright.
	if len(prog.Content[1].Layers) != 1 || prog.Content[1].Layers[0].URL == "" {
		t.Fatalf("pushed content[1] layers = %+v, want one image layer with a resolved URL", prog.Content[1].Layers)
	}
	if prog.ProgramRevision == beforeProg.ProgramRevision {
		t.Error("the pushed program carries the SAME program_revision as the scheduled one — a player only re-fetches when the revision changes, so the screen would never swap")
	}
}

// TestClearingThePushRestoresTheScheduledProjection: the override is reversible,
// and reversing it returns byte-identical scheduling output. A push that left a
// residue in the program the screen falls back to would be an override an
// operator could never fully undo.
func TestClearingThePushRestoresTheScheduledProjection(t *testing.T) {
	const seedAsset = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	s := ovSeedWithCast(t, seedAsset)

	before, _ := DeriveScreenPrograms(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, contentInstant(t))
	beforeProg := programForScreen(t, before, store.SeedScreenID)

	pushOverride(t, s, store.SeedScreenID, alertOn(ovCastID))
	pushOverride(t, s, store.SeedScreenID, nil) // an explicit null clears it

	after, _ := DeriveScreenPrograms(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, contentInstant(t))
	afterProg := programForScreen(t, after, store.SeedScreenID)

	if afterProg.Priority != "scheduled" {
		t.Errorf("after clearing, priority = %q, want scheduled", afterProg.Priority)
	}
	if afterProg.Pinned {
		t.Error("after clearing, the program is still Pinned — the relay would go on refusing to re-resolve a screen with no override on it")
	}
	if afterProg.ProgramRevision != beforeProg.ProgramRevision {
		t.Errorf("after clearing, program_revision = %q, want the pre-push %q — the same schedule at the same instant must derive the same program",
			afterProg.ProgramRevision, beforeProg.ProgramRevision)
	}
}

// TestAPushOverridesEvenABlankedScreen: the override is what an operator reaches
// for when a screen is dark and should not be. Deferring to the schedule's
// display_power here would make the feature useless in exactly the case it is
// most needed (an out-of-hours emergency notice).
func TestAPushOverridesEvenABlankedScreen(t *testing.T) {
	const seedAsset = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	s := ovSeedWithCast(t, seedAsset)

	// The overnight instant, where the seeded schedule resolves to display:blank.
	blank, _ := DeriveScreenPrograms(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, blankInstant(t))
	if got := programForScreen(t, blank, store.SeedScreenID).Display; got != "blank" {
		t.Fatalf("fixture: the un-pushed screen derives display %q overnight, want blank", got)
	}

	pushOverride(t, s, store.SeedScreenID, alertOn(ovCastID))
	prog := programForScreen(t,
		mustDerive(t, desiredState(t, s), blankInstant(t)), store.SeedScreenID)
	if prog.Display != "content" {
		t.Errorf("a push at a blanked screen derives display %q, want content", prog.Display)
	}
	if len(prog.Content) == 0 {
		t.Error("a push at a blanked screen derives no content — the screen would stay dark")
	}
}

// TestAPushOnlyAffectsTheScreenItNames: an override is per screen, and a
// projection that leaked one screen's emergency notice onto its neighbours would
// be the worst possible failure of this feature.
func TestAPushOnlyAffectsTheScreenItNames(t *testing.T) {
	const seedAsset = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	s := ovSeedWithCast(t, seedAsset)
	ctx := context.Background()

	// A second screen row at the same placement, so both are governed by the
	// same seeded schedule and any leak would be invisible in the schedule alone.
	const otherScreen = "01J8Z0VERR1DE0THERSCREEN01"
	body, err := json.Marshal(datamodel.Screen{ID: otherScreen, ScopeNode: ovScopeNode, Name: "Neighbour"})
	if err != nil {
		t.Fatalf("marshal the second screen: %v", err)
	}
	if _, err := s.Create(ctx, store.KindScreen, body); err != nil {
		t.Fatalf("create the second screen: %v", err)
	}
	pushOverride(t, s, store.SeedScreenID, alertOn(ovCastID))

	programs := mustDerive(t, desiredState(t, s), contentInstant(t))
	if got := programForScreen(t, programs, store.SeedScreenID).Priority; got != "preempt" {
		t.Errorf("the PUSHED screen derives priority %q, want preempt", got)
	}
	neighbour := programForScreen(t, programs, otherScreen)
	if neighbour.Pinned {
		t.Error("the NEIGHBOURING screen is Pinned — one screen's push leaked onto another, and the relay would stop re-resolving a screen nobody overrode")
	}
	if neighbour.Priority != "scheduled" {
		t.Fatalf("the NEIGHBOURING screen derives priority %q, want scheduled — one screen's push leaked onto another", neighbour.Priority)
	}
	wantSeededAssetThenSlide(t, neighbour.Content, seedAsset)
}

// mustDerive derives the section or fails on any per-screen degrade.
func mustDerive(t *testing.T, ds store.DesiredStateResult, nowMs int64) []wire.ScreenProgram {
	t.Helper()
	programs, errs := DeriveScreenPrograms(ds, contenturl.Signer{Base: "https://origin.example"}, nowMs)
	if len(errs) != 0 {
		t.Fatalf("DeriveScreenPrograms reported %+v, want none", errs)
	}
	return programs
}
