package wire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The INTERACTIVE slide layers (`ping`, `nav`) and the interactive members that
// can ride any layer (`ping_name`, `items`) — parity milestones 1.5/3.7.
//
// lease_test.go and slidewidgets_test.go pin the drawn kinds; this file covers
// the class that is new in kind: layers whose value flows from the screen BACK
// to the box, and the focus geometry two other implementations transcribe.

func interactiveBase(kind string) Layer {
	return Layer{Kind: kind, X: 100, Y: 100, W: 400, H: 120}
}

// TestPingLayerRequiresBothHalves pins the `ping` rule. A ping with only one of
// its two halves is exactly half a button — an unlabelled hotspot nobody knows
// to press, or a labelled control that reports nothing — and either is a surface
// that accepts a press and performs no work.
func TestPingLayerRequiresBothHalves(t *testing.T) {
	full := interactiveBase(LayerKindPing)
	full.Text = "Call for service"
	full.PingName = "call_service"
	if err := ValidateAuthoredSlideLayers([]Layer{full}); err != nil {
		t.Fatalf("a complete ping layer must validate, got %v", err)
	}

	noLabel := full
	noLabel.Text = ""
	if err := ValidateAuthoredSlideLayers([]Layer{noLabel}); err == nil {
		t.Error("a ping with no label must be refused")
	}

	noName := full
	noName.PingName = ""
	if err := ValidateAuthoredSlideLayers([]Layer{noName}); err == nil {
		t.Error("a ping with no ping_name must be refused")
	}
}

// TestPingNameGrammarIsCheckedOnEveryKind is the MIRROR-DIRECTION guard, and it
// is the one this repo's past half-fixes would have failed.
//
// `ping_name` is required on `ping` and legal on every other kind — that is the
// whole interactive-widget mechanism. A validator that checked the grammar only
// inside its `ping` case would let a malformed name through on all ten other
// kinds, where it would reach a rules/1 `event` trigger's match constraint and
// never fire, with nothing anywhere saying why. The required direction and the
// optional-everywhere direction are both asserted here.
func TestPingNameGrammarIsCheckedOnEveryKind(t *testing.T) {
	bad := []string{"Front Desk", "UPPER", "_leading", "front desk", strings.Repeat("a", 65), "emoji☺"}
	for _, name := range bad {
		l := interactiveBase(LayerKindText)
		l.Text = "hello"
		l.PingName = name
		if err := ValidateAuthoredSlideLayers([]Layer{l}); err == nil {
			t.Errorf("ping_name %q on a text layer must be refused", name)
		}
	}

	good := []string{"a", "call_service", "front-desk.1", "x9"}
	for _, name := range good {
		for _, kind := range []string{LayerKindText, LayerKindRect, LayerKindEntity} {
			l := interactiveBase(kind)
			switch kind {
			case LayerKindText:
				l.Text = "hello"
			case LayerKindRect:
				l.Color = "#112233"
			case LayerKindEntity:
				l.EntityID = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"
			}
			l.PingName = name
			if err := ValidateAuthoredSlideLayers([]Layer{l}); err != nil {
				t.Errorf("ping_name %q on a %s layer must be accepted (that is what makes an ordinary widget interactive), got %v", name, kind, err)
			}
		}
	}
}

// TestNavLayerRules pins the menu's own rules, including the bound.
func TestNavLayerRules(t *testing.T) {
	ok := interactiveBase(LayerKindNav)
	ok.Items = []NavItem{{Label: "Menu", TargetSlideID: "s2"}}
	if err := ValidateAuthoredSlideLayers([]Layer{ok}); err != nil {
		t.Fatalf("a one-item nav must validate, got %v", err)
	}

	empty := interactiveBase(LayerKindNav)
	if err := ValidateAuthoredSlideLayers([]Layer{empty}); err == nil {
		t.Error("a nav with no items must be refused — a menu nobody can choose from")
	}

	tooMany := interactiveBase(LayerKindNav)
	for i := 0; i < maxNavItems+1; i++ {
		tooMany.Items = append(tooMany.Items, NavItem{Label: "x", TargetSlideID: "s"})
	}
	if err := ValidateAuthoredSlideLayers([]Layer{tooMany}); err == nil {
		t.Errorf("a nav with %d items must be refused (max %d)", maxNavItems+1, maxNavItems)
	}

	noLabel := interactiveBase(LayerKindNav)
	noLabel.Items = []NavItem{{TargetSlideID: "s2"}}
	if err := ValidateAuthoredSlideLayers([]Layer{noLabel}); err == nil {
		t.Error("a nav item with no label must be refused")
	}

	noTarget := interactiveBase(LayerKindNav)
	noTarget.Items = []NavItem{{Label: "Menu"}}
	if err := ValidateAuthoredSlideLayers([]Layer{noTarget}); err == nil {
		t.Error("a nav item with no target must be refused — it would accept a press and perform nothing")
	}
}

// TestItemsRejectedOnNonNavKinds is the OTHER mirror direction: `items` are
// legal only on `nav`, so their absence is checked in the per-kind switch and
// their PRESENCE ELSEWHERE can only be caught outside it. A rect that silently
// dropped its menu items would draw a rectangle where the producer believed it
// had authored a menu.
func TestItemsRejectedOnNonNavKinds(t *testing.T) {
	for _, kind := range []string{LayerKindText, LayerKindRect, LayerKindPing} {
		l := interactiveBase(kind)
		l.Text = "hello"
		l.Color = "#112233"
		l.PingName = "p"
		l.Items = []NavItem{{Label: "Menu", TargetSlideID: "s2"}}
		if err := ValidateAuthoredSlideLayers([]Layer{l}); err == nil {
			t.Errorf("menu items on a %s layer must be refused", kind)
		}
	}
}

// TestLayerIsInteractiveNamesBothArms pins the shared focusability predicate.
// A consumer that tested the KIND alone would give no focus region to an
// ordinary widget an author made interactive, which is tracker row 3.7 entirely.
func TestLayerIsInteractiveNamesBothArms(t *testing.T) {
	if !LayerIsInteractive(Layer{Kind: LayerKindNav}) {
		t.Error("a nav layer is interactive by kind")
	}
	if !LayerIsInteractive(Layer{Kind: LayerKindEntity, PingName: "x"}) {
		t.Error("ANY layer carrying a ping_name is interactive — that is the interactive-widget mechanism")
	}
	if LayerIsInteractive(Layer{Kind: LayerKindText}) {
		t.Error("a plain text layer with no ping_name is not interactive")
	}
	if !LayerIsInteractive(Layer{Kind: LayerKindPing, PingName: "x"}) {
		t.Error("a ping layer is interactive")
	}
}

// TestNavItemRectsLayout pins the geometry three implementations share.
func TestNavItemRectsLayout(t *testing.T) {
	horizontal := Layer{Kind: LayerKindNav, X: 100, Y: 200, W: 900, H: 100,
		Items: []NavItem{{Label: "a"}, {Label: "b"}, {Label: "c"}}}
	want := [][4]int{{100, 200, 300, 100}, {400, 200, 300, 100}, {700, 200, 300, 100}}
	if got := NavItemRects(horizontal); !reflect.DeepEqual(got, want) {
		t.Errorf("horizontal layout = %v, want %v", got, want)
	}

	vertical := Layer{Kind: LayerKindNav, X: 10, Y: 20, W: 200, H: 600,
		Items: []NavItem{{Label: "a"}, {Label: "b"}}}
	wantV := [][4]int{{10, 20, 200, 300}, {10, 320, 200, 300}}
	if got := NavItemRects(vertical); !reflect.DeepEqual(got, wantV) {
		t.Errorf("vertical layout = %v, want %v", got, wantV)
	}

	// The remainder lands on the LAST item, so the items exactly fill the box —
	// 100/3 is 33 each with 1px left over, and a layout that dropped it would
	// leave a visible sliver of unclaimed, unfocusable space at the far edge.
	remainder := Layer{Kind: LayerKindNav, X: 0, Y: 0, W: 100, H: 10,
		Items: []NavItem{{Label: "a"}, {Label: "b"}, {Label: "c"}}}
	got := NavItemRects(remainder)
	if len(got) != 3 {
		t.Fatalf("got %d rects, want 3", len(got))
	}
	if end := got[2][0] + got[2][2]; end != 100 {
		t.Errorf("last item ends at %d, want the box's own far edge 100", end)
	}

	if NavItemRects(Layer{Kind: LayerKindNav}) != nil {
		t.Error("a nav with no items lays out nothing")
	}
}

// TestInteractiveLayerRoundTrip pins the wire shape: the two new members
// marshal under their contract names and survive a round trip, and a layer that
// carries neither emits no new keys at all (so every pre-existing slide's signed
// bytes are byte-identical to before these fields existed).
func TestInteractiveLayerRoundTrip(t *testing.T) {
	in := Layer{
		Kind: LayerKindNav, X: 1, Y: 2, W: 300, H: 100,
		Items: []NavItem{{Label: "Rooms", TargetSlideID: "rooms"}},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `"items":[{"label":"Rooms","target_slide_id":"rooms"}]`; !strings.Contains(string(b), want) {
		t.Fatalf("missing %s in %s", want, b)
	}
	var out Layer
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip changed the layer: %+v vs %+v", out, in)
	}

	plain, err := json.Marshal(Layer{Kind: LayerKindText, X: 1, Y: 2, W: 3, H: 4, Text: "hi"})
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	for _, key := range []string{"ping_name", "items"} {
		if strings.Contains(string(plain), key) {
			t.Errorf("a layer carrying no %s must not emit the key: %s", key, plain)
		}
	}
}

// TestLeaseContentSlideIDIsOmittedWhenAbsent pins the same additive-compatibility
// property for the item-level id a nav target resolves against.
func TestLeaseContentSlideIDIsOmittedWhenAbsent(t *testing.T) {
	b, err := json.Marshal(LeaseContent{Type: "image", AssetRef: "sha256:aa", URL: "u", ExpiresAt: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "slide_id") {
		t.Errorf("an item with no cast-local slide id must not emit the key: %s", b)
	}
	withID, err := json.Marshal(LeaseContent{Type: "slide", SlideID: "intro"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(withID), `"slide_id":"intro"`) {
		t.Errorf("a cast slide item must carry its id: %s", withID)
	}
}

// TestInteractiveKindsAreInTheClosedSet guards the enumeration the rejection
// message names: a kind added to the constants and forgotten in slideLayerKinds
// would be refused while the error claimed to list every legal value.
func TestInteractiveKindsAreInTheClosedSet(t *testing.T) {
	for _, kind := range []string{LayerKindPing, LayerKindNav} {
		if !isSlideLayerKind(kind) {
			t.Errorf("%s must be a member of the closed kind set", kind)
		}
		if !strings.Contains(strings.Join(slideLayerKinds, "/"), kind) {
			t.Errorf("%s must appear in the rejection message's enumeration", kind)
		}
	}
}

// TestValidPingName pins the slug grammar itself — the one an automation author
// retypes into a trigger's match constraint.
func TestValidPingName(t *testing.T) {
	for _, s := range []string{"a", "0", "call_service", "a.b-c", "a1_2.3-4"} {
		if !ValidPingName(s) {
			t.Errorf("%q must be a valid ping name", s)
		}
	}
	for _, s := range []string{"", "A", "a b", "-lead", ".lead", "_lead", "a/b", strings.Repeat("a", 65)} {
		if ValidPingName(s) {
			t.Errorf("%q must not be a valid ping name", s)
		}
	}
}

// TestFocusRegionsMustBeLegible pins the legibility floor over BOTH arms of
// LayerIsInteractive — which is where the rule is easy to get half right.
//
// The canvas is a wall seen from across a room and driven by a remote. A focus
// outline below MinInteractiveSide cannot be told apart at that distance, so the
// viewer cannot see what pressing OK would do. The Studio's canvas is scaled
// DOWN, which is exactly what makes the mistake invisible while authoring.
func TestFocusRegionsMustBeLegible(t *testing.T) {
	// Arm one: any layer made pressable by a ping name. Checking only the `ping`
	// KIND here would leave every interactive widget unguarded.
	tiny := Layer{Kind: LayerKindRect, X: 0, Y: 0, W: MinInteractiveSide - 1, H: 200,
		Color: "#112233", PingName: "hotspot"}
	if err := ValidateAuthoredSlideLayers([]Layer{tiny}); err == nil {
		t.Errorf("a %dpx-wide pressable layer must be refused (floor is %d)", tiny.W, MinInteractiveSide)
	}
	ok := tiny
	ok.W = MinInteractiveSide
	if err := ValidateAuthoredSlideLayers([]Layer{ok}); err != nil {
		t.Errorf("a layer exactly at the floor must be accepted, got %v", err)
	}
	// The SAME box with no ping name is not focusable and is perfectly legal —
	// the floor applies to focusability, not to geometry in general.
	notPressable := tiny
	notPressable.PingName = ""
	if err := ValidateAuthoredSlideLayers([]Layer{notPressable}); err != nil {
		t.Errorf("a non-interactive layer of the same size must still be accepted, got %v", err)
	}

	// Arm two: a nav whose BOX is comfortably large and whose CELLS are not.
	// A whole-layer check misses this case entirely, and it is the likelier one:
	// nobody draws a tiny menu, they add one item too many to a normal one.
	crowded := Layer{Kind: LayerKindNav, X: 0, Y: 0, W: 300, H: 120}
	for i := 0; i < maxNavItems; i++ {
		crowded.Items = append(crowded.Items, NavItem{Label: "x", TargetSlideID: "s"})
	}
	if err := ValidateAuthoredSlideLayers([]Layer{crowded}); err == nil {
		t.Errorf("a %dpx menu of %d items gives %dpx cells and must be refused",
			crowded.W, len(crowded.Items), crowded.W/len(crowded.Items))
	}
	roomy := crowded
	roomy.W = MinInteractiveSide * maxNavItems
	if err := ValidateAuthoredSlideLayers([]Layer{roomy}); err != nil {
		t.Errorf("the same menu in a box wide enough for its items must be accepted, got %v", err)
	}
}
