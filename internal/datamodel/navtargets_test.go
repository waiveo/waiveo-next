package datamodel

import (
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// navSlide is a slide carrying one nav layer pointing at target.
func navSlide(id, target string) CastSlide {
	return CastSlide{
		ID: id,
		Layers: []wire.Layer{{
			Kind: wire.LayerKindNav, X: 0, Y: 0, W: 600, H: 100,
			Items: []wire.NavItem{{Label: "Go", TargetSlideID: target}},
		}},
	}
}

// textSlide is a plain, valid slide.
func textSlide(id string) CastSlide {
	return CastSlide{ID: id, Layers: []wire.Layer{{Kind: wire.LayerKindText, X: 0, Y: 0, W: 100, H: 50, Text: "hi"}}}
}

// TestNavTargetMustNameASlideOfTheSameCast is the CAST-level gate the layer
// shape's own validator structurally cannot apply: it sees one stack at a time
// and has no idea which slides exist.
//
// Without it a menu item whose target names nothing is stored happily and
// reaches a screen as an item that takes focus, highlights, accepts a press —
// and performs nothing, visible only to whoever is standing in front of the TV.
func TestNavTargetMustNameASlideOfTheSameCast(t *testing.T) {
	errs := checkCastSlides([]CastSlide{navSlide("one", "nope"), textSlide("two")})
	found := false
	for _, e := range errs {
		if e.Code == "CAST_NAV_TARGET_UNKNOWN" {
			found = true
			if !strings.Contains(e.Field, "slides[0].layers[0].items[0].target_slide_id") {
				t.Errorf("the error must name the exact item, got field %q", e.Field)
			}
		}
	}
	if !found {
		t.Fatalf("a nav item targeting a slide the cast does not declare must be refused; got %+v", errs)
	}
}

// TestNavTargetMayPointForward is the direction an incremental single-pass check
// gets wrong. A "next" item is the commonest menu there is, and it targets a
// slide declared AFTER the one the menu sits on — a validator reusing the
// running duplicate-detection set would accept only BACKWARD jumps and refuse a
// forward one with a message about a slide that plainly exists.
func TestNavTargetMayPointForward(t *testing.T) {
	errs := checkCastSlides([]CastSlide{navSlide("one", "two"), textSlide("two")})
	for _, e := range errs {
		if e.Code == "CAST_NAV_TARGET_UNKNOWN" {
			t.Fatalf("a forward jump to a later slide must be accepted; got %+v", errs)
		}
	}
}

// TestEmptyNavTargetIsReportedOnce: the layer-shape gate already refuses an
// empty target, so reporting it again here would show an operator two errors for
// one mistake.
func TestEmptyNavTargetIsReportedOnce(t *testing.T) {
	errs := checkCastSlides([]CastSlide{navSlide("one", ""), textSlide("two")})
	for _, e := range errs {
		if e.Code == "CAST_NAV_TARGET_UNKNOWN" {
			t.Fatalf("an EMPTY target is the layer gate's error, not the cast gate's; got %+v", errs)
		}
	}
	if len(errs) == 0 {
		t.Fatal("an empty target must still be refused, by the layer-shape gate")
	}
}
