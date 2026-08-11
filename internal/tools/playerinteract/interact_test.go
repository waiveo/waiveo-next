package playerinteract

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

var (
	scenePath = filepath.Join("..", "..", "..", "player-v3", "components", "PhotonScene.brs")
	taskPath  = filepath.Join("..", "..", "..", "player-v3", "components", "InteractionTask.brs")
	sceneXML  = filepath.Join("..", "..", "..", "player-v3", "components", "PhotonScene.xml")
)

// read loads a shipped player source with its comments stripped, so a guard can
// never be satisfied by a sentence in a comment that says the code does the
// thing. Comment stripping is quote-aware — a `'` inside a string literal is not
// a comment marker, and treating it as one would corrupt the line.
func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		out = append(out, strings.TrimSpace(stripComment(line)))
	}
	joined := strings.Join(out, "\n")
	if strings.TrimSpace(joined) == "" {
		t.Fatalf("%s parsed to no code", path)
	}
	return joined
}

func stripComment(text string) string {
	inString := false
	for i, r := range text {
		switch r {
		case '"':
			inString = !inString
		case '\'':
			if !inString {
				return text[:i]
			}
		}
	}
	return text
}

// TestSceneTakesFocusSoKeyEventsArrive is the guard on the most silent failure
// available here. A SceneGraph Scene receives onKeyEvent only while it holds
// focus; without setFocus(true) the whole interactive layer class is inert and
// absolutely nothing else looks different.
func TestSceneTakesFocusSoKeyEventsArrive(t *testing.T) {
	src := read(t, scenePath)
	if !strings.Contains(src, "m.top.setFocus(true)") {
		t.Error("PhotonScene must call m.top.setFocus(true): a Scene that does not hold focus receives no key events, and every interactive layer is inert with no other symptom")
	}
	if !regexp.MustCompile(`(?m)^function onKeyEvent\(key as String, press as Boolean\) as Boolean`).MatchString(src) {
		t.Error("PhotonScene must define onKeyEvent(key, press) — the only entry point a remote press has")
	}
}

// TestOnlyKeyDownIsActedOn: a remote delivers press AND release for every key.
// Acting on both dispatches every interaction twice, so one human press runs two
// automations.
func TestOnlyKeyDownIsActedOn(t *testing.T) {
	body := functionBody(t, read(t, scenePath), "onKeyEvent")
	if !strings.Contains(body, "if not press then return false") {
		t.Error("onKeyEvent must ignore key RELEASE (`if not press then return false`), or every press dispatches twice")
	}
}

// TestUnhandledKeysAreNotSwallowed: returning true for a key this scene did
// nothing with takes it away from the platform. On a wall-mounted panel with no
// keyboard, a scene that swallows every key is a channel nobody can exit.
func TestUnhandledKeysAreNotSwallowed(t *testing.T) {
	body := functionBody(t, read(t, scenePath), "onKeyEvent")
	if !strings.HasSuffix(strings.TrimSpace(body), "return false\nend function") {
		t.Error("onKeyEvent must fall through to `return false` so an unhandled key keeps its platform meaning")
	}
	if !strings.Contains(body, "if m.focusTargets = invalid or m.focusTargets.Count() = 0 then return false") {
		t.Error("onKeyEvent must handle nothing on a slide with no interactive regions")
	}
}

// TestFocusTargetsCoverBothArmsOfInteractive is the mirror-direction guard, and
// the one a kind-only implementation fails.
//
// wire.LayerIsInteractive has two arms: a `nav` layer (whose items are the
// targets), and ANY layer carrying a ping_name (which is what makes an ordinary
// widget an interactive one — tracker row 3.7). The player must register targets
// for both. Registering only the `ping` KIND would leave every interactive
// widget drawing perfectly and never taking focus.
func TestFocusTargetsCoverBothArmsOfInteractive(t *testing.T) {
	body := functionBody(t, read(t, scenePath), "renderSlide")

	if !strings.Contains(body, `pingName = wvSlideStr(layer.ping_name)`) {
		t.Fatal("renderSlide must read every layer's ping_name")
	}
	// The ping-name target registration must sit OUTSIDE the kind chain. The
	// chain's branches are `if kind = "…"` / `else if kind = "…"`; a registration
	// inside one of them would appear before the chain's terminating `end if`.
	chainEnd := strings.Index(body, `print "[player-v3] slide layer with unsupported kind`)
	pingReg := strings.Index(body, `pingName = wvSlideStr(layer.ping_name)`)
	if chainEnd < 0 {
		t.Fatal("renderSlide's unknown-kind fallthrough is missing; the structure this guard reads has changed")
	}
	if pingReg < chainEnd {
		t.Error("the ping_name focus target is registered INSIDE the per-kind chain, so only the `ping` kind becomes focusable — every interactive widget (row 3.7) would be inert")
	}
	if !strings.Contains(body, `action: "ping"`) || !strings.Contains(body, `action: "nav"`) {
		t.Error("renderSlide must register both a ping action and a nav action")
	}
	if !strings.Contains(body, `rects = wvNavItemRects(`) {
		t.Error("a nav layer's items must each get their own focus region, laid out by wvNavItemRects")
	}
}

// TestClearSlideDropsFocusState: focus targets that outlive their slide let the
// next OK dispatch the PREVIOUS slide's action — a button the viewer can no
// longer see firing an automation they did not choose. Same teardown discipline
// the tick timer and the video children already have.
func TestClearSlideDropsFocusState(t *testing.T) {
	body := functionBody(t, read(t, scenePath), "clearSlide")
	for _, want := range []string{"m.focusTargets = []", "m.focusIndex = -1", "hideFocusRing()"} {
		if !strings.Contains(body, want) {
			t.Errorf("clearSlide must %s — a stale focus target dispatches the previous slide's action", want)
		}
	}
}

// TestExactlyOneInteractionTaskIsEverCreated is this fleet's own thread-leak
// class. Task threads outlive the node that owns them; the legacy player created
// a fresh ApiTask per press, and a panel pressed a few hundred times a day
// reaches the firmware thread cap. The Task must be created in init and nowhere
// else, and it must be stopped at shutdown.
func TestExactlyOneInteractionTaskIsEverCreated(t *testing.T) {
	src := read(t, scenePath)
	if n := strings.Count(src, `CreateObject("roSGNode", "InteractionTask")`); n != 1 {
		t.Fatalf("InteractionTask is created %d time(s); exactly one may ever exist (Task threads outlive their owning node)", n)
	}
	if !strings.Contains(functionBody(t, src, "init"), `CreateObject("roSGNode", "InteractionTask")`) {
		t.Error("the single InteractionTask must be created in init, not on a press")
	}
	shutdown := functionBody(t, src, "shutdown")
	// The two stops AND the guard they sit behind, as one contiguous block. The
	// strings alone are satisfied by a block that can never execute (`if false`,
	// or a guard on something always-invalid), which is a stranded Task with a
	// shutdown path that reads correct — so the guard condition is pinned too.
	wantBlock := strings.Join([]string{
		"if m.interactionTask <> invalid",
		"m.interactionTask.stopFlag = true",
		`m.interactionTask.control = "STOP"`,
		"end if",
	}, "\n")
	if !strings.Contains(shutdown, wantBlock) {
		t.Errorf("shutdown must stop the interaction Task cooperatively AND at the firmware level, behind an `m.interactionTask <> invalid` guard; got:\n%s", shutdown)
	}
}

// TestInteractionTaskLoopCanEnd: a Task blocked forever in wait() is a stranded
// thread. Its loop must consult both exits and use a BOUNDED wait so they are
// reached even when nobody is pressing anything.
func TestInteractionTaskLoopCanEnd(t *testing.T) {
	src := read(t, taskPath)
	body := functionBody(t, src, "runInteraction")
	if !strings.Contains(body, "m.top.stopFlag = true") {
		t.Error("runInteraction must honour stopFlag")
	}
	if !strings.Contains(body, `LCase(m.top.control) <> "run"`) {
		t.Error("runInteraction must honour a control-field stop (compared case-insensitively — a Task's control reads back lower-cased)")
	}
	if !strings.Contains(body, "wait(wvInteractionWaitMs(), port)") {
		t.Error("runInteraction must wait with a BOUNDED timeout, or neither exit is reachable while idle")
	}
}

// TestPressIsPostedOverThePinnedConnection: the request carries the channel
// token, which is the credential the relay resolves the SCREEN identity from.
// Posting it to an unverified peer would hand a screen's credential to whatever
// answered.
func TestPressIsPostedOverThePinnedConnection(t *testing.T) {
	body := functionBody(t, read(t, taskPath), "wvPostInteraction")
	if !strings.Contains(body, "peerVerify: true") {
		t.Error("a press must be posted with peer verification ON against the pinned relay trust anchor")
	}
	if !strings.Contains(body, "/player/v1/interaction") {
		t.Error("a press must be posted to /player/v1/interaction")
	}
	if !strings.Contains(body, "bearer: state.channelToken") {
		t.Error("a press must present the channel token, which is what resolves the screen identity")
	}
}

// TestFocusRingIsDeclaredOnceInTheScene: the ring is repositioned, never
// recreated. A per-slide ring would be four more nodes to remember to remove on
// the teardown path that already has to stop timers and videos — and the one
// that got forgotten would be an outline drawn around a layer that no longer
// exists. It must also paint ON TOP of the slide layers.
func TestFocusRingIsDeclaredOnceInTheScene(t *testing.T) {
	raw, err := os.ReadFile(sceneXML)
	if err != nil {
		t.Fatalf("read PhotonScene.xml: %v", err)
	}
	xml := string(raw)
	if !strings.Contains(xml, `<Group id="focusRing"`) {
		t.Fatal("PhotonScene.xml must declare the focusRing group")
	}
	for _, id := range []string{"focusRingTop", "focusRingBottom", "focusRingLeft", "focusRingRight"} {
		if !strings.Contains(xml, id) {
			t.Errorf("PhotonScene.xml must declare %s — a SceneGraph Rectangle has no border, so the ring is four bars", id)
		}
	}
	if strings.Index(xml, `id="slideLayers"`) > strings.Index(xml, `id="focusRing"`) {
		t.Error("focusRing must be declared AFTER slideLayers so it paints on top of every layer, including a full-canvas image")
	}
	if strings.Contains(read(t, scenePath), `CreateObject("roSGNode", "Rectangle")`+"\n"+`    ring`) {
		t.Error("the focus ring must not be created per slide")
	}
}

// TestNavItemRectsMatchesTheGoDefinition pins the player's transcription of
// wire.NavItemRects against the Go function itself.
//
// Three implementations compute these rects — the Go one (the authority, which
// the Studio's TypeScript mirror also follows), and this BrightScript one — and
// they must agree, because the Go one decides where a menu item is DRAWN in the
// editor and this one decides where the player FOCUSES it. A drift puts the
// focus ring somewhere other than the label it belongs to.
//
// BrightScript cannot be executed here, so the guard asserts the three decisions
// that make the layout what it is, extracted from the real source: the axis test,
// the equal-cell division, and the last item absorbing the remainder. Each is
// checked against the Go function's own behaviour so a change to the Go side that
// nobody transcribed is caught from either direction.
func TestNavItemRectsMatchesTheGoDefinition(t *testing.T) {
	body := functionBody(t, read(t, scenePath), "wvNavItemRects")

	if !strings.Contains(body, "if w >= h") {
		t.Error("the player must pick the layout axis with `w >= h` — the same test wire.NavItemRects makes; any other spelling is a square laid out the other way round from the editor")
	}
	if !strings.Contains(body, "cell = Int(w / n)") || !strings.Contains(body, "cell = Int(h / n)") {
		t.Error("the player must divide the box into n equal cells along the chosen axis")
	}
	if !strings.Contains(body, "iw = w - cell * (n - 1)") || !strings.Contains(body, "ih = h - cell * (n - 1)") {
		t.Error("the LAST item must absorb the integer-division remainder, exactly as wire.NavItemRects does, or the menu ends short of the box it was drawn in")
	}

	// And the Go side must still behave the way that transcription describes —
	// checked from the other direction so a change to the Go function alone is
	// caught here rather than only on a television.
	horizontal := wire.Layer{Kind: wire.LayerKindNav, X: 0, Y: 0, W: 100, H: 10,
		Items: []wire.NavItem{{Label: "a"}, {Label: "b"}, {Label: "c"}}}
	rects := wire.NavItemRects(horizontal)
	if len(rects) != 3 {
		t.Fatalf("wire.NavItemRects returned %d rects", len(rects))
	}
	if rects[0][2] != 33 || rects[2][2] != 34 {
		t.Errorf("wire.NavItemRects widths = %d/%d/%d; the transcription above assumes equal cells with the remainder on the last",
			rects[0][2], rects[1][2], rects[2][2])
	}
	vertical := wire.Layer{Kind: wire.LayerKindNav, X: 0, Y: 0, W: 10, H: 100,
		Items: []wire.NavItem{{Label: "a"}, {Label: "b"}}}
	if v := wire.NavItemRects(vertical); v[1][1] != 50 {
		t.Errorf("wire.NavItemRects stacks vertically from y=%d, want 50", v[1][1])
	}
}

// TestDwellIsRearmedByInteraction: a slide carrying a menu is still a slide with
// a dwell time. Without re-arming, a viewer three items into a menu has it
// replaced mid-thought.
func TestDwellIsRearmedByInteraction(t *testing.T) {
	src := read(t, scenePath)
	body := functionBody(t, src, "onKeyEvent")
	if strings.Count(body, "wvRestartDwell()") < 2 {
		t.Error("both the OK path and the focus-move path must re-arm the slide's dwell timer")
	}
	rearm := functionBody(t, src, "wvRestartDwell")
	if !strings.Contains(rearm, "if m.currentDwellMs = invalid or m.currentDwellMs <= 0 then return") {
		t.Error("wvRestartDwell must refuse a zero/absent dwell — arming a SceneGraph Timer with 0 saturates the render thread")
	}
}

// functionBody extracts one `sub`/`function` body from comment-stripped source.
func functionBody(t *testing.T, src, name string) string {
	t.Helper()
	start := regexp.MustCompile(`(?m)^(sub|function) ` + regexp.QuoteMeta(name) + `\(`)
	loc := start.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("no sub/function %q in the shipped source", name)
	}
	rest := src[loc[0]:]
	end := regexp.MustCompile(`(?m)^end (sub|function)`)
	e := end.FindStringIndex(rest)
	if e == nil {
		t.Fatalf("sub/function %q is not terminated", name)
	}
	return rest[:e[1]]
}

// TestGuardReadsRealSource proves this package is pointed at the shipped files
// rather than at nothing — a path typo would make every assertion above vacuous
// in the direction that passes.
func TestGuardReadsRealSource(t *testing.T) {
	for _, p := range []string{scenePath, taskPath, sceneXML} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if info.Size() < 500 {
			t.Fatalf("%s is only %s bytes; this guard is reading the wrong file", p, strconv.FormatInt(info.Size(), 10))
		}
	}
}

// TestOnlyDrawnLayersBecomeFocusTargets: an unrecognized kind is SKIPPED (the
// player's forward-compatibility rule), and a skipped layer must not become a
// focus region. A ring around empty space the viewer cannot see or press is a
// focus trap on exactly the path that is supposed to degrade gracefully.
func TestOnlyDrawnLayersBecomeFocusTargets(t *testing.T) {
	body := functionBody(t, read(t, scenePath), "renderSlide")
	if !strings.Contains(body, `if pingName <> "" and kind <> "nav" and drawn`) {
		t.Error("the ping_name focus target must be gated on the layer having actually been drawn")
	}
	if !strings.Contains(body, "drawn = false") {
		t.Error("the unknown-kind branch must clear `drawn`, or a skipped layer still takes focus")
	}
}
