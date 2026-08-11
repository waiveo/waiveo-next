package playercontentcache

import (
	"os"
	"strings"
	"testing"
)

// videolayer_test.go guards the video SLIDE LAYER (parity row 1.5-video) across
// the two files it is implemented in: Program.brs fetches and verifies its
// bytes, PhotonScene.brs draws it and — the part that has bitten this player
// twice already in other guises — tears it down.

// TestASlideVideoLayerHasItsBytesFetched: the slide loop resolves BOTH
// content-bearing kinds through the shared predicate, not by testing for
// `image`.
//
// A video layer whose bytes were never fetched reaches the scene with no
// contentUri, so the slide draws with a hole where the video is and reports
// nothing — the exact silent shape wire.LayerFetchesContent exists to make
// impossible on the producer side. The predicate is asserted here in its
// on-device mirror, and the loop is asserted to USE it: an inline `= "image"`
// comparison is the regression, and it is the natural thing to write.
func TestASlideVideoLayerHasItsBytesFetched(t *testing.T) {
	src := readBrs(t, programPath)

	pred := routineBody(t, src, "wvLayerFetchesContent")
	for _, kind := range []string{`"image"`, `"video"`} {
		if !contains(pred, kind) {
			t.Errorf("wvLayerFetchesContent does not name %s — a content-bearing kind it omits is a layer whose bytes are silently never fetched", kind)
		}
	}

	body := routineBody(t, src, "wvDoProgram")
	if indexOfCall(body, "wvLayerFetchesContent") < 0 {
		t.Error("wvDoProgram's slide loop does not ask wvLayerFetchesContent which layers carry content; testing a kind inline is how one of the two gets forgotten")
	}
	// The layer that passes the predicate must be resolved through the same
	// cache-or-fetch-then-verify path everything else uses, and must come back
	// carrying a local uri — a layer left without one draws nothing.
	if !contains(body, "layer.contentUri = fv.path") {
		t.Error("wvDoProgram does not attach the resolved local path to a slide content layer; the scene has nothing to draw from")
	}
	if !contains(body, "layer.streamFormat = wvVideoStreamFormat()") {
		t.Error("wvDoProgram does not give a slide video layer its stream format; the scene would have to guess how the bytes are containerised")
	}
}

// TestTheSceneDrawsAVideoLayerAndLoopsIt: PhotonScene renders a `video` layer as
// a positioned, playing, LOOPING Video node.
//
// Looping is the non-obvious half and it is a real requirement, not a nicety. A
// slide advances on its own dwell timer, not on the video's end of stream —
// unlike a plain video cast item, where end of stream IS the advance signal — so
// a clip shorter than its slide would freeze on its last frame for the
// remainder, which on a wall reads as a crashed screen.
func TestTheSceneDrawsAVideoLayerAndLoopsIt(t *testing.T) {
	src := readBrs(t, photonScenePath)

	render := routineBody(t, src, "renderSlide")
	if !contains(render, `kind = "video"`) {
		t.Fatal("renderSlide has no `video` branch: a video layer would fall through to the unknown-kind skip and simply not be drawn")
	}
	if indexOfCall(render, "renderSlideVideo") < 0 {
		t.Error("renderSlide's video branch does not build the layer's Video node")
	}

	node := routineBody(t, src, "renderSlideVideo")
	for _, want := range []struct{ frag, why string }{
		{"v.translation = [x, y]", "a slide layer is positioned in the canvas, not full-screen — an untranslated node covers the whole slide"},
		{"v.loop = true", "a slide advances on its own dwell, so a shorter clip must loop rather than freeze on its last frame for the rest of the slide"},
		{`v.control = "play"`, "a Video node that is never told to play shows nothing"},
		{"content.url", "the node must be pointed at the already-fetched, already-verified local file"},
	} {
		if !contains(node, want.frag) {
			t.Errorf("renderSlideVideo is missing %q — %s", want.frag, want.why)
		}
	}
}

// TestClearSlideStopsVideoChildrenBeforeRemovingThem is the teardown guard, and
// it is the one this player has the most history with.
//
// Removing a SceneGraph node does NOT stop what it is doing. That is how this
// fleet once leaked Task threads until devices hit their firmware thread cap,
// and it is why clearComposed stops composed video layers explicitly. A slide's
// video is the same hazard a third time: left running, it keeps decoding and
// keeps playing AUDIO underneath whatever replaced it — on a signage wall, a
// voice-over from a slide that is no longer on screen.
//
// The ORDER is asserted too. Stopping after the children are gone is stopping
// nothing: the handles are already dropped.
func TestClearSlideStopsVideoChildrenBeforeRemovingThem(t *testing.T) {
	body := routineBody(t, readBrs(t, photonScenePath), "clearSlide")

	stopAt := -1
	for i, l := range body {
		if strings.Contains(l.text, `subtype() = "Video"`) && strings.Contains(l.text, `control = "stop"`) {
			stopAt = i
			break
		}
	}
	if stopAt < 0 {
		t.Fatal("clearSlide does not stop its Video children: removing the node does not stop playback, so a superseded slide keeps decoding (and keeps its audio playing) behind the next one")
	}

	removeAt := -1
	for i, l := range body {
		if strings.Contains(l.text, "removeChildrenIndex(") {
			removeAt = i
			break
		}
	}
	if removeAt < 0 {
		t.Fatal("clearSlide no longer removes its children")
	}
	if stopAt > removeAt {
		t.Errorf("clearSlide stops Video children at line %d, AFTER removing them at line %d — by then the handles are gone and the stop reaches nothing",
			body[stopAt].n, body[removeAt].n)
	}

	// The tick timer must still be stopped first, ahead of everything: a queued
	// fire reaching a torn-down Label is the same class of fault.
	if stopClock := indexOfCall(body, "stopSlideClock"); stopClock < 0 || stopClock > removeAt {
		t.Error("clearSlide does not stop the slide tick timer before removing its children")
	}
}

// TestTheSceneDeclaresTheGroupItsVideoLayersLiveIn is a small consistency check
// on the component XML: renderSlide appends into `slideLayers`, so the node has
// to exist. A findNode miss is `invalid` at runtime and every append after it
// throws on-device, which the compile gate cannot see.
func TestTheSceneDeclaresTheGroupItsVideoLayersLiveIn(t *testing.T) {
	raw, err := os.ReadFile(photonSceneXML)
	if err != nil {
		t.Fatalf("read PhotonScene.xml: %v", err)
	}
	if !strings.Contains(string(raw), `id="slideLayers"`) {
		t.Error("PhotonScene.xml declares no slideLayers node, but renderSlide appends every slide layer — including a video layer's Video node — into it")
	}
}
