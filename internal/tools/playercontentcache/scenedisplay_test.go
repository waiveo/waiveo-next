package playercontentcache

import (
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/slidelive"
)

// scenedisplay_test.go guards the RENDER half of the two defects
// programdegrade_test.go covers the resolve half of: what PhotonScene puts on
// the screen when a layer degraded (HV-2) and when a Lease says
// `display: "blank"` (HV-4).
//
// These are structural, deliberately, and the reason is the one this package's
// own doc already states: the instrument follows the property. Program.brs's
// half is a decision over N items, so it is EXECUTED; PhotonScene's half is a
// small set of calls against SceneGraph nodes — "does the blank path tear down
// all four render surfaces", "does an empty contentUri draw something" — which
// is exactly the shape a structural assertion answers, and modelling
// roSGNode/appendChild/Font well enough to run it would be a far larger fake
// than the code under test.
//
// Every assertion below therefore names its members LITERALLY rather than
// iterating whatever it finds. A list-driven check protects additions and is
// blind to deletions, and a deletion is precisely the regression here: the blank
// path forgetting one of the four surfaces leaves a Poster on a wall that was
// told to go dark.

// TestABlankDisplayTearsTheScreenDownRatherThanShowingAStatus is HV-4's scene
// half.
//
// Program.brs reports a `display: "blank"` Lease as ok:true / contentType
// "blank" (TestABlankDisplayLeaseIsASuccessThatShowsNothing). Reporting it is
// useless unless the scene acts on it, and the two obvious wrong ways to act are
// both silent:
//
//   - falling through to the cast path. startCast on an empty items array
//     returns EARLY, before it hides the Poster or stops the Video, so the
//     outgoing image simply stays on the wall — a blank that changes nothing,
//     which is byte-for-byte the screenshot HV-4 was found in.
//   - falling through to the not-ok path. That one tears down correctly but
//     writes a status line across the screen, and an intentionally blanked wall
//     is black, not a wall with an explanation on it.
func TestABlankDisplayTearsTheScreenDownRatherThanShowingAStatus(t *testing.T) {
	src := readBrs(t, photonScenePath)
	body := routineBody(t, src, "onPhotonResult")

	blankAt := -1
	for i, l := range body {
		if strings.Contains(l.text, `= "blank"`) && strings.HasPrefix(l.text, "if ") {
			blankAt = i
			break
		}
	}
	if blankAt < 0 {
		t.Fatal("onPhotonResult has no branch on a \"blank\" contentType: a display:blank Lease (PLY-093) falls through to the cast path, where startCast returns early on an empty items array WITHOUT hiding the Poster or stopping the Video — the screen keeps showing whatever it was showing, which is the defect")
	}

	// It has to come before the cast path, not merely exist: startCast is what
	// would otherwise swallow it.
	startAt := indexOfCall(body, "startCast")
	if startAt >= 0 && startAt < blankAt {
		t.Errorf("onPhotonResult reaches startCast (line %d) before it tests for a blank display (line %d)", body[startAt].n, body[blankAt].n)
	}

	tearAt := indexOfCall(body, "tearDownContent")
	if tearAt < 0 || tearAt < blankAt {
		t.Fatal("the blank branch does not call tearDownContent — something must actually remove what is on screen")
	}
	// PLY-158/PLY-155: an intentionally blanked screen is not assigned content,
	// so the idle-defeat hold comes off with it.
	if !contains(body[blankAt:], "setIdleDefeat(false)") {
		t.Error("the blank branch does not disengage idle defeat; PLY-158's obligation is tied to being assigned NON-blank content, and PLY-155 has a relay treat a blanked screen the same way")
	}
	// And no status text: black, not explained.
	if !contains(body[blankAt:], "m.status.visible = false") {
		t.Error("the blank branch does not hide the status label — a blanked wall must be black, not a wall with a message written across it")
	}

	// The FAILED-pull branch shares the same teardown, so the two can never
	// diverge on which surface one of them forgot.
	if countCalls(body, "tearDownContent") < 2 {
		t.Error("onPhotonResult does not share one teardown between its blank path and its failed-pull path; two copies are two places to forget the Poster")
	}
}

// TestTheTeardownClearsEveryRenderSurface names the four surfaces a program can
// occupy, literally, plus the two pieces of remembered state.
//
// This scene can have a Poster, a Video, a composed Group and a slide Group on
// screen at once, and the teardown is the only place that fact is written down.
// Forgetting one of them shows up as "the screen went dark except for the
// video", which nobody will connect to a display:blank Lease.
//
// m.castSignature is in the list for a subtler reason: the signature exists to
// answer "is this program already on screen?", and after a teardown the honest
// answer for every program is no. A stale signature makes the very next Lease
// carrying that same program look already-showing, startCast is skipped, and the
// screen stays black under a program it was told to display.
func TestTheTeardownClearsEveryRenderSurface(t *testing.T) {
	body := routineBody(t, readBrs(t, photonScenePath), "tearDownContent")

	for _, want := range []struct{ frag, why string }{
		{"stopCastTimer()", "a dwell timer left armed fires an advance against a cast that is no longer there"},
		{"clearComposed()", "a composed item's layers, including a decoding Video, stay on screen and keep playing"},
		{"clearSlide()", "the slide's children stay drawn and its tick timer keeps firing at them"},
		{`m.video.control = "stop"`, "removing or hiding a Video node does not stop playback — the audio keeps going"},
		{"m.video.visible = false", "a stopped Video node is still a visible surface"},
		{"m.poster.visible = false", "the image on the wall is a Poster, and this is the line that takes it off"},
		{"m.castItems = []", "a press or a nav jump would still resolve against the cast that is no longer showing"},
		{`m.castSignature = ""`, "the next Lease carrying this same program would be recognised as already-showing and never re-rendered"},
	} {
		if !contains(body, want.frag) {
			t.Errorf("tearDownContent is missing %q — %s", want.frag, want.why)
		}
	}
}

// TestBlankingIsIdempotentAcrossPolls: the poll loop delivers a fresh blank
// Lease every ten seconds for as long as a screen is dark, and each one arrives
// as a NEW photonResult. Tearing down and logging on every one of them is a
// console line every ten seconds all night, and repeated teardown churn against
// SceneGraph for no reason.
//
// The gate is the same castSignature the content path already dedupes with,
// which is why there is no second flag to keep in sync.
func TestBlankingIsIdempotentAcrossPolls(t *testing.T) {
	src := readBrs(t, photonScenePath)
	body := routineBody(t, src, "onPhotonResult")

	// The GATE, not merely a mention of it: an `if` that consults
	// wvBlankSignature(), standing before the teardown it guards. Asserting that
	// the routine "contains wvBlankSignature()" would be satisfied by the
	// assignment alone, which is exactly what a condition rewritten to `if true`
	// leaves behind — the mutation this test exists to catch.
	gate := -1
	for i, l := range body {
		if strings.HasPrefix(l.text, "if ") && strings.Contains(l.text, "wvBlankSignature()") {
			gate = i
			break
		}
	}
	if gate < 0 {
		t.Fatal("nothing in onPhotonResult GATES on wvBlankSignature(): every poll while a screen is dark re-runs the teardown and re-prints the line, which is a console line every ten seconds all night")
	}
	tearAt := indexOfCall(body, "tearDownContent")
	if tearAt < 0 || tearAt < gate {
		t.Fatalf("the blank teardown at line %d is not behind the wvBlankSignature() gate at line %d", body[tearAt].n, body[gate].n)
	}
	sig := routineBody(t, src, "wvBlankSignature")
	if len(sig) != 1 || !strings.HasPrefix(sig[0].text, "return ") {
		t.Fatalf("wvBlankSignature is no longer a single constant return: %s", joinLines(sig))
	}
	// It must not be a value castSignature could ever produce. castSignature
	// builds every real value starting with the item count, so a numeric-leading
	// sentinel would be a collision waiting for the right cast.
	value := strings.TrimSpace(strings.TrimPrefix(sig[0].text, "return "))
	if strings.Trim(value, `"`) == "" || (value[1] >= '0' && value[1] <= '9') {
		t.Errorf("wvBlankSignature returns %s, which castSignature could produce (it starts every signature with the item count) — a collision would make a real cast look already-blank", value)
	}
}

// TestADegradedContentLayerDrawsAVisiblePlaceholder is HV-2's scene half.
//
// Program.brs degrades a slide image/video layer whose bytes it could not fetch
// by leaving its contentUri empty. If the scene simply skipped such a layer the
// fix would be half-done in the worst way: the slide would draw, and the missing
// picture would be invisible — indistinguishable, on a wall, from a slide
// authored without one. Nobody would ever see that content was being lost.
//
// wvLayerFetchesContent's own doc names this failure exactly ("the slide draws
// with a blank hole where the video is, and nothing anywhere reports an error"),
// and this branch closes it for BOTH causes at once: a fetch that failed, and a
// producer that forgot to resolve the layer at all.
func TestADegradedContentLayerDrawsAVisiblePlaceholder(t *testing.T) {
	src := readBrs(t, photonScenePath)
	render := routineBody(t, src, "renderSlide")

	// Both content-bearing kinds, not just image: the pair is the thing that
	// gets half-implemented (that is why wvLayerFetchesContent exists at all).
	for _, kind := range []string{"image", "video"} {
		branch := branchAfter(t, render, `kind = "`+kind+`"`)
		if !contains(branch, `wvSlideStr(layer.contentUri) = ""`) {
			t.Errorf("renderSlide's %q branch never tests for an empty contentUri, so a degraded layer draws nothing at all and the loss is invisible", kind)
		}
		if indexOfCall(branch, "createDegradedLayer") < 0 {
			t.Errorf("renderSlide's %q branch does not draw the degraded placeholder", kind)
		}
	}

	node := routineBody(t, src, "createDegradedLayer")
	for _, want := range []struct{ frag, why string }{
		{`CreateObject("roSGNode", "Rectangle")`, "the placeholder needs a visible fill; a bare Label on a dark slide is not visibly anything"},
		{`CreateObject("roSGNode", "Label")`, "the placeholder needs the glyph that says 'no value here'"},
		{"wvDegradedLayerText()", "the glyph must come from the one place it is defined, so it cannot drift from slidelive's"},
		{"g.translation = [x, y]", "the placeholder must sit exactly where the missing content would have, or it reports the loss in the wrong place"},
	} {
		if !contains(node, want.frag) {
			t.Errorf("createDegradedLayer is missing %q — %s", want.frag, want.why)
		}
	}
}

// TestTheDegradedGlyphIsTheSameOneTheBoxUses pins the player's placeholder text
// to slidelive.Unavailable across the language boundary.
//
// A slide can degrade in two places — the box cannot resolve a weather or entity
// value, or the device cannot fetch an image's bytes — and a viewer should not
// have to learn that those look different. slidelive.Unavailable is exported for
// exactly this reason ("the contract between this package and anything that
// asserts on a degraded slide"), and nothing but a test can hold a Go constant
// and a BrightScript literal equal.
func TestTheDegradedGlyphIsTheSameOneTheBoxUses(t *testing.T) {
	body := routineBody(t, readBrs(t, photonScenePath), "wvDegradedLayerText")
	if len(body) != 1 {
		t.Fatalf("wvDegradedLayerText is no longer a single return: %s", joinLines(body))
	}
	got := strings.Trim(strings.TrimSpace(strings.TrimPrefix(body[0].text, "return ")), `"`)
	if got != slidelive.Unavailable {
		t.Errorf("the player draws %q for a degraded layer but slidelive.Unavailable is %q — one slide would then degrade two different ways depending on which half failed", got, slidelive.Unavailable)
	}
}

// branchAfter returns the lines of the `else if <cond>` / `if <cond>` arm that
// begins at the first line containing cond, up to the next arm at the same
// nesting. It is deliberately crude — it stops at the next line beginning with
// `else if` or `else` at column 0 of the body — which is enough to keep an
// assertion scoped to one kind's branch rather than to the whole of renderSlide.
func branchAfter(t *testing.T, body []line, cond string) []line {
	t.Helper()
	start := -1
	for i, l := range body {
		if strings.Contains(l.text, cond) && (strings.HasPrefix(l.text, "if ") || strings.HasPrefix(l.text, "else if ")) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("no branch on %q — this guard no longer recognises the routine it reads", cond)
	}
	for i := start; i < len(body); i++ {
		if strings.HasPrefix(body[i].text, "else if ") || body[i].text == "else" {
			return body[start:i]
		}
	}
	return body[start:]
}
