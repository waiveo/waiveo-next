package playercontentcache

import (
	"strings"
	"testing"
)

// programdegrade_test.go EXECUTES the real wvDoProgram out of the shipped
// player-v3/source/Program.brs and asserts what a Lease actually resolves to.
//
// # Why executed and not structural
//
// Both defects these tests cover are decisions inside one loop, and both of them
// shipped, survived every gate, and were found only by photographing a
// television:
//
//   - HV-2: one slide layer answered 403 and the player rejected the WHOLE
//     program — including a second slide that referenced no assets at all. The
//     screen sat on an hour-old test slide for a whole session while the log
//     said "keeping current content, never-wipe".
//   - HV-4: a valid signed Lease carrying `"display":"blank"` and `"content":[]`
//     was logged as a FAILED pull, so an alert override whose TTL had lapsed
//     never left the wall, and a screen scheduled dark overnight never went
//     dark.
//
// Nothing structural can see either one. "wvDoProgram contains a `return r`" is
// true of a correct player and a broken one alike; what matters is which items
// survive a partial failure and which outcome a blank instruction produces. So
// these run the routine (brsrun_test.go's engine) over a Lease built in Go and
// assert the returned cast — the same instrument, and for the same reason, that
// cache_test.go's trim tests use.
//
// # What is faked, and what is not
//
// Only I/O: the pinned trust file, the two HTTP calls, the file transfer, and
// the sha256 of the transferred bytes. The item loop, the display decision, the
// degrade/drop decision, the per-item cache keying, the trim and the ack call
// are the player's own code, executed.
//
// wvStr is the one exception and it is a cross-FILE dependency, not I/O: it
// lives in Pairing.brs, which this single-file engine does not load.
// TestTheWvStrTranscriptionStillMatchesTheShippedPlayer below pins the three
// lines this stub transcribes, so the stub cannot drift away from the function
// the device actually runs.

// programHarness drives one wvDoProgram call.
type programHarness struct {
	in *interp
	// lease is what ParseJson hands back — the Lease under test.
	lease *assoc
	// resp is what the program GET answers with.
	resp *assoc
	// fetchable decides, per content url, whether the transfer succeeds. A url
	// absent from the map fails with 403 — the exact origin response HV-2 was
	// found through.
	fetchable map[string]bool
	// fetched records every url a transfer was attempted for, in order, and acks
	// records every lease/ack request body posted.
	fetched []string
	acks    []*assoc
}

func newProgramHarness(t *testing.T) *programHarness {
	t.Helper()
	h := &programHarness{in: newInterp(t, programPath), fetchable: map[string]bool{}}

	// The pinned trust anchor, rehydrated to a path. Storage/file I/O.
	h.in.stubs["wvrehydratetrustfile"] = func([]any) any { return "cachefs:/pin.pem" }

	// wvStr's transcription — see this file's own doc, and the fence below.
	h.in.stubs["wvstr"] = func(args []any) any {
		if len(args) == 0 || args[0] == nil {
			return ""
		}
		if s, ok := args[0].(string); ok {
			return s
		}
		return toStr(args[0], 0)
	}

	// FormatJson/ParseJson: the engine models no JSON codec, and neither is
	// where a defect could hide here — the Lease under test is built directly.
	h.in.stubs["formatjson"] = func([]any) any { return "{}" }
	h.in.stubs["parsejson"] = func([]any) any {
		if h.lease == nil {
			return nil
		}
		return h.lease
	}

	// The two HTTP calls wvDoProgram makes, told apart by url so the ack is
	// observable. wvAckLease itself is NOT stubbed: it is the player's own code
	// and whether it runs is one of the things under test.
	h.in.stubs["wvhttpjson"] = func(args []any) any {
		req, ok := args[0].(*assoc)
		if !ok {
			t.Fatalf("wvHttpJson called with a %T, want a request assoc", args[0])
		}
		url := toStr(req.get("url"), 0)
		if strings.HasSuffix(url, "/player/v1/lease/ack") {
			h.acks = append(h.acks, req)
			return okAssoc()
		}
		return h.resp
	}

	// The content transfer and the integrity hash. The transfer is where a real
	// origin says 403; the hash is a whole-file read this engine cannot perform.
	h.in.stubs["wvhttpgettofile"] = func(args []any) any {
		url := toStr(args[0], 0)
		h.fetched = append(h.fetched, url)
		res := newAssoc()
		res.set("startFailed", false)
		if h.fetchable[url] {
			res.set("ok", true)
			res.set("code", 200)
			res.set("failureReason", "")
			return res
		}
		res.set("ok", false)
		res.set("code", 403)
		res.set("failureReason", "Forbidden")
		return res
	}
	h.in.stubs["wvverifystoredbytes"] = func([]any) any {
		v := newAssoc()
		v.set("ok", true)
		v.set("sizeBytes", 4096)
		return v
	}

	h.resp = programResponse(`{"lease_id":"L1"}`)
	return h
}

// run executes wvDoProgram and returns its result assoc.
func (h *programHarness) run(t *testing.T) *assoc {
	t.Helper()
	state := newAssoc()
	state.set("channelToken", "tok")
	state.set("relayHost", "192.168.50.10")
	state.set("relayPort", "7421")
	state.set("trustPem", "-----BEGIN CERTIFICATE-----")
	out, ok := h.in.call("wvDoProgram", state).(*assoc)
	if !ok {
		t.Fatal("wvDoProgram did not return an associative array")
	}
	return out
}

// console is everything the player printed during the run, joined so a test can
// assert that a degrade or a drop was actually REPORTED. Silence is half of both
// defects: a shortened rotation nobody can see is the same operational problem
// as a frozen one.
func (h *programHarness) console() string { return strings.Join(h.in.printed, "\n") }

func okAssoc() *assoc {
	a := newAssoc()
	a.set("ok", true)
	a.set("code", 200)
	a.set("body", "{}")
	a.set("failureReason", "")
	a.set("startFailed", false)
	return a
}

func programResponse(body string) *assoc {
	a := okAssoc()
	a.set("body", body)
	return a
}

// lease builds a Lease assoc with the given display and content items.
func lease(display string, items ...*assoc) *assoc {
	l := newAssoc()
	l.set("lease_id", "01J0LEASE")
	l.set("display", display)
	content := &arr{}
	for _, it := range items {
		content.items = append(content.items, it)
	}
	l.set("content", content)
	return l
}

// slideItem builds a `slide` content item out of already-built layers.
func slideItem(slideID string, layers ...*assoc) *assoc {
	it := newAssoc()
	it.set("type", "slide")
	it.set("slide_id", slideID)
	it.set("duration_ms", 8000)
	ls := &arr{}
	for _, l := range layers {
		ls.items = append(ls.items, l)
	}
	it.set("layers", ls)
	return it
}

func textLayer(text string) *assoc {
	l := newAssoc()
	l.set("kind", "text")
	l.set("text", text)
	return l
}

// contentLayer builds a slide layer of a content-bearing kind (image/video).
func contentLayer(kind, assetRef, url string) *assoc {
	l := newAssoc()
	l.set("kind", kind)
	l.set("asset_ref", assetRef)
	l.set("url", url)
	return l
}

// plainItem builds a plain `image`/`video` content item.
func plainItem(itemType, assetRef, url string) *assoc {
	it := newAssoc()
	it.set("type", itemType)
	it.set("asset_ref", assetRef)
	it.set("url", url)
	return it
}

func items(t *testing.T, result *assoc) *arr {
	t.Helper()
	v := result.get("items")
	if v == nil {
		return nil
	}
	a, ok := v.(*arr)
	if !ok {
		t.Fatalf("wvDoProgram returned items of type %T, want an array", v)
	}
	return a
}

func itemAt(t *testing.T, result *assoc, i int) *assoc {
	t.Helper()
	list := items(t, result)
	if list == nil || i >= len(list.items) {
		t.Fatalf("no cast item %d (the cast holds %d)", i, itemCount(list))
	}
	e, ok := list.items[i].(*assoc)
	if !ok {
		t.Fatalf("cast item %d is a %T, want an associative array", i, list.items[i])
	}
	return e
}

func itemCount(list *arr) int {
	if list == nil {
		return 0
	}
	return len(list.items)
}

func layerAt(t *testing.T, item *assoc, j int) *assoc {
	t.Helper()
	ls, ok := item.get("layers").(*arr)
	if !ok {
		t.Fatalf("cast item has no layers array")
	}
	if j >= len(ls.items) {
		t.Fatalf("no layer %d (the item holds %d)", j, len(ls.items))
	}
	l, ok := ls.items[j].(*assoc)
	if !ok {
		t.Fatalf("layer %d is a %T", j, ls.items[j])
	}
	return l
}

// ─────────────────────────────────────────────────────────── HV-2: degrading

// TestAnUnfetchableSlideLayerDegradesAndTheSlideStillDraws is HV-2, stated as
// the hardware found it.
//
// The Lease carries two slides. The first references no assets at all. The
// second carries a title, an image that fetches, and an image that answers 403.
// Before this fix the 403 returned out of wvDoProgram, the whole Lease was
// discarded, the assetless slide went with it, and the screen kept an hour-old
// program with "never-wipe" written next to it in the log.
//
// PLY-087 is explicit about which way this goes: a player that cannot reach a
// content origin "MUST continue rendering whatever content it already holds
// locally that remains valid under that Lease, rather than treat the fetch
// failure as a reason to stop rendering entirely". wire.Layer.Value argues the
// identical case one level down — an unreachable forecast service must not blank
// a slide and lose its title, its image and its clock.
func TestAnUnfetchableSlideLayerDegradesAndTheSlideStillDraws(t *testing.T) {
	h := newProgramHarness(t)
	h.fetchable["https://origin/good.png"] = true // and NOT bad.png
	h.lease = lease("content",
		slideItem("a", textLayer("no assets on this slide at all")),
		slideItem("b",
			textLayer("Title"),
			contentLayer("image", "sha256:AA", "https://origin/good.png"),
			contentLayer("image", "sha256:BB", "https://origin/bad.png"),
		),
	)

	got := h.run(t)

	if got.get("ok") != true {
		t.Fatalf("wvDoProgram failed the whole Lease over one 403 layer: error = %q\nconsole:\n%s", got.get("error"), h.console())
	}
	if got.get("contentType") != "cast" {
		t.Errorf("contentType = %q, want \"cast\"", got.get("contentType"))
	}
	if n := itemCount(items(t, got)); n != 2 {
		t.Fatalf("the cast holds %d item(s), want 2 — a slide that fetched cleanly was discarded because a DIFFERENT item could not", n)
	}

	// The assetless slide is the one the hardware lost, so it is asserted by
	// identity, not by count.
	if id := itemAt(t, got, 0).get("slideId"); id != "a" {
		t.Errorf("cast item 0 is slide %q, want \"a\" — the slide with no assets at all", id)
	}

	// Within the surviving slide: the layer that fetched carries its local path,
	// the layer that did not carries an EMPTY contentUri, which is the single
	// signal PhotonScene's renderSlide draws its degraded placeholder from.
	slide := itemAt(t, got, 1)
	if uri, _ := layerAt(t, slide, 1).get("contentUri").(string); uri == "" {
		t.Error("the layer that fetched cleanly has no contentUri — the whole slide was degraded, not just the failing layer")
	}
	if uri := layerAt(t, slide, 2).get("contentUri"); uri != "" {
		t.Errorf("the 403 layer's contentUri = %v, want \"\" — PhotonScene draws its unavailable placeholder from exactly that emptiness, so a stale or absent value draws either the wrong picture or a silent hole", uri)
	}

	// And it must be REPORTED. A rotation that silently shows less than it was
	// assigned is the same operational problem as one that shows the wrong
	// thing, just harder to notice.
	if !strings.Contains(h.console(), "DEGRADED") {
		t.Errorf("nothing on the console says a layer was degraded:\n%s", h.console())
	}
}

// TestOneUnfetchableItemDoesNotVetoTheItemsThatFetchedCleanly is the other half
// of HV-2: containment at the ITEM boundary, not just the layer boundary.
//
// A plain image item IS its asset — there is no partial version of it to draw —
// so it is dropped rather than degraded. What must not happen is the drop
// spreading: every other item in the same Lease still presents.
func TestOneUnfetchableItemDoesNotVetoTheItemsThatFetchedCleanly(t *testing.T) {
	h := newProgramHarness(t)
	h.lease = lease("content",
		plainItem("image", "sha256:CC", "https://origin/gone.png"), // unfetchable
		slideItem("b", textLayer("this slide needs nothing from the origin")),
	)

	got := h.run(t)

	if got.get("ok") != true {
		t.Fatalf("one unfetchable item failed the whole Lease: error = %q\nconsole:\n%s", got.get("error"), h.console())
	}
	if n := itemCount(items(t, got)); n != 1 {
		t.Fatalf("the cast holds %d item(s), want 1 (the slide) — the unfetchable image should be dropped and nothing else with it", n)
	}
	if id := itemAt(t, got, 0).get("slideId"); id != "b" {
		t.Errorf("the surviving item is %q, want the slide \"b\"", id)
	}
	if !strings.Contains(h.console(), "DROPPED") {
		t.Errorf("the dropped item was not reported:\n%s", h.console())
	}
}

// TestALeaseThatResolvesToNothingKeepsTheCurrentContent is the MIRROR defect,
// and it is the one that would be worse than the bug.
//
// Degrading per layer and dropping per item must not become "publish an empty
// cast". A Lease whose every item is unfetchable is exactly the unreachability
// never-wipe exists for: report the failure, publish nothing, and let PlayerTask
// keep whatever is already on the wall.
func TestALeaseThatResolvesToNothingKeepsTheCurrentContent(t *testing.T) {
	h := newProgramHarness(t)
	h.lease = lease("content",
		plainItem("image", "sha256:DD", "https://origin/a.png"),
		plainItem("video", "sha256:EE", "https://origin/b.mp4"),
	)

	got := h.run(t)

	if got.get("ok") != false {
		t.Fatal("wvDoProgram reported success for a Lease it could not present a single item of — PlayerTask would publish an empty cast and PhotonScene would tear the screen down")
	}
	if n := itemCount(items(t, got)); n != 0 {
		t.Errorf("a failed pull published %d item(s); it must publish none", n)
	}
	if e, _ := got.get("error").(string); e == "" {
		t.Error("a failed pull carries no error text, so the never-wipe log line would say nothing about why")
	}
	if got.get("contentType") == "blank" {
		t.Error("a failed pull reported itself as display:blank — that is HV-4's mirror defect, and it would blank a wall over an unreachable origin")
	}
}

// ─────────────────────────────────────────────────────── HV-4: display:blank

// TestABlankDisplayLeaseIsASuccessThatShowsNothing is HV-4.
//
// `display` is a first-class contract value (PLY-093, `api/openapi.yaml`'s
// `enum: [content, blank]`), and a blank Lease's own content array MAY be empty
// — so an empty array under display:blank is the NORMAL shape, not a malformed
// one. Before this fix wvDoProgram tested `content.Count() = 0` before it looked
// at `display` at all, so the instruction "show nothing" arrived as the error
// "lease carried an empty or missing content array" and the screen kept an
// expired evacuation notice on the wall.
//
// internal/virtualplayer's AdoptionOf has always honoured `display`, and the
// conformance corpus drives that double — which is exactly why every gate was
// green while the device ignored the contract. This test drives the DEVICE's
// own routine.
func TestABlankDisplayLeaseIsASuccessThatShowsNothing(t *testing.T) {
	h := newProgramHarness(t)
	h.lease = lease("blank")

	got := h.run(t)

	if got.get("ok") != true {
		t.Fatalf("a valid signed display:blank Lease was reported as a FAILED pull (error = %q), so never-wipe keeps whatever is on the wall — an expired alert never leaves it", got.get("error"))
	}
	if got.get("contentType") != "blank" {
		t.Fatalf("contentType = %q, want \"blank\" — PhotonScene branches on exactly this to tear the screen down", got.get("contentType"))
	}
	if n := itemCount(items(t, got)); n != 0 {
		t.Errorf("a blank Lease published %d item(s), want 0", n)
	}
	// PLY-091/104: a blank Lease is still accepted and persisted. An
	// unacknowledged blank is a screen that went dark without telling anyone.
	if len(h.acks) != 1 {
		t.Errorf("the blank Lease was acknowledged %d times, want exactly 1 (PLY-091/104)", len(h.acks))
	}
}

// TestABlankDisplayWinsOverAnyContentTheLeaseHappensToCarry: PLY-093 says a
// blank display shows "none of the Lease's own `content` array (which MAY be
// empty under this value)" — MAY, not MUST. So `display` decides, and content is
// not even fetched: bytes nothing can put on screen are bytes not worth a
// transfer, and on a fleet of screens that is a real link.
func TestABlankDisplayWinsOverAnyContentTheLeaseHappensToCarry(t *testing.T) {
	h := newProgramHarness(t)
	h.fetchable["https://origin/good.png"] = true
	h.lease = lease("blank", plainItem("image", "sha256:FF", "https://origin/good.png"))

	got := h.run(t)

	if got.get("contentType") != "blank" {
		t.Fatalf("contentType = %q, want \"blank\" — a blank Lease that carries content still shows nothing", got.get("contentType"))
	}
	if n := itemCount(items(t, got)); n != 0 {
		t.Errorf("a blank Lease published %d item(s), want 0", n)
	}
	if len(h.fetched) != 0 {
		t.Errorf("a blank Lease fetched %v — nothing it carries can reach the screen", h.fetched)
	}
}

// TestAFailedPullIsNeverReportedAsBlank is HV-4's mirror defect, and it is worse
// than the bug it mirrors: blanking on unreachability turns a WAN blip into a
// dark wall, while the bug merely leaves the last good program up.
//
// Two shapes of "I could not get a program" are checked, because they take
// different routes out of wvDoProgram: the relay was unreachable, and the relay
// answered with a content Lease carrying nothing.
func TestAFailedPullIsNeverReportedAsBlank(t *testing.T) {
	t.Run("relay unreachable", func(t *testing.T) {
		h := newProgramHarness(t)
		down := newAssoc()
		down.set("ok", false)
		down.set("code", 0)
		down.set("body", "")
		down.set("failureReason", "connection refused")
		down.set("startFailed", true)
		h.resp = down
		h.lease = lease("blank") // deliberately blank: it must never be reached

		got := h.run(t)

		if got.get("ok") != false {
			t.Fatal("an unreachable relay was reported as a successful pull")
		}
		if got.get("contentType") == "blank" {
			t.Fatal("an unreachable relay blanked the screen — never-wipe exists for exactly this case")
		}
	})

	t.Run("content display with an empty content array", func(t *testing.T) {
		h := newProgramHarness(t)
		h.lease = lease("content")

		got := h.run(t)

		if got.get("ok") != false {
			t.Fatal("a display:content Lease carrying no content was reported as a successful pull")
		}
		if got.get("contentType") == "blank" {
			t.Fatal("an empty content Lease blanked the screen; only display:blank may do that")
		}
	})
}

// TestAnUnrecognisedDisplayValueKeepsTheContentPath: the display test is a
// positive comparison against "blank", never a negation of "content".
//
// An older or non-conformant relay, or a future vocabulary this player has not
// adopted, must not be able to blank a wall by sending a word this player does
// not understand. The conservative direction here is to keep showing content.
func TestAnUnrecognisedDisplayValueKeepsTheContentPath(t *testing.T) {
	for _, display := range []string{"", "content", "dimmed", "BLANK"} {
		h := newProgramHarness(t)
		h.fetchable["https://origin/good.png"] = true
		h.lease = lease(display, plainItem("image", "sha256:11", "https://origin/good.png"))

		got := h.run(t)

		if got.get("contentType") != "cast" {
			t.Errorf("display %q resolved to contentType %q, want \"cast\" — only the exact value \"blank\" may blank a screen", display, got.get("contentType"))
		}
	}
}

// ───────────────────────────────────────────────────────────────── the fence

// TestTheWvStrTranscriptionStillMatchesTheShippedPlayer pins the ONE piece of
// player code these tests transcribe rather than execute.
//
// wvStr lives in Pairing.brs, which this single-file engine does not load, so
// newProgramHarness stubs it. That stub is a claim about the shipped player, and
// an unpinned claim is exactly how a guard goes on passing while the thing it
// guards changes underneath it. The three statements are named LITERALLY here —
// not counted, not pattern-matched — because the failure this must catch is a
// member being changed or removed, and a test that iterates whatever it finds
// cannot see a deletion.
func TestTheWvStrTranscriptionStillMatchesTheShippedPlayer(t *testing.T) {
	body := routineBody(t, readBrs(t, pairingPath), "wvStr")

	want := []string{
		`if v = invalid then return ""`,
		`if type(v) = "roString" or type(v) = "String" then return v`,
		`return v.toStr()`,
	}
	if len(body) != len(want) {
		t.Fatalf("wvStr is now %d statement(s), not %d — programdegrade_test.go's stub transcribes it and must be re-read:\n%s",
			len(body), len(want), joinLines(body))
	}
	for i, w := range want {
		if body[i].text != w {
			t.Errorf("wvStr line %d is %q, want %q — the stub in newProgramHarness models the second form; update both together",
				body[i].n, body[i].text, w)
		}
	}
}

func joinLines(body []line) string {
	var b strings.Builder
	for _, l := range body {
		b.WriteString(l.text)
		b.WriteString("\n")
	}
	return b.String()
}
