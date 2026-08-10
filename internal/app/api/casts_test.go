package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// casts_test.go drives the cast family over the REAL mounted mux — the same
// handler, auth middleware, declared-schema gate, store and validators a running
// feeder serves — because everything this family gets, it gets from being an
// ordinary resource mount, and "ordinary" is a claim only the real surface can
// settle.

// castSlideFixture is one drawable slide: a full-bleed rect under a title.
func castSlideFixture(id string) datamodel.CastSlide {
	return datamodel.CastSlide{
		ID: id, DurationMS: 6000,
		Layers: []wire.Layer{
			{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#101828"},
			{Kind: wire.LayerKindText, X: 120, Y: 120, W: 1200, H: 160, Text: "Today's Special", FontPx: 96, Color: "#FFFFFF", Align: "left"},
		},
	}
}

// castFixture is the create body every happy-path case below posts.
func castFixture(scopeNode string, labels map[string]string) datamodel.Cast {
	return datamodel.Cast{
		ScopeNode: scopeNode,
		Name:      "Lunch Menu",
		Slides:    []datamodel.CastSlide{castSlideFixture("title"), castSlideFixture("photo")},
		Labels:    labels,
	}
}

// createCast POSTs a cast and returns its server-minted id.
func (e *testEnv) createCast(t *testing.T, scopeNode string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, castFixture(scopeNode, nil)), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create cast: status %d, body %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// decodeCast decodes a served cast representation.
func decodeCast(t *testing.T, raw []byte) datamodel.Cast {
	t.Helper()
	var c datamodel.Cast
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("decode cast: %v (body %s)", err, raw)
	}
	return c
}

// TestCastCRUDHappyPath drives create → read → conditional update → delete over
// the mounted surface, asserting the api/1 conventions a family gets for free
// only if it is genuinely mounted as one: a server-assigned id, an ETag that
// tracks the revision, an If-Match-conditioned patch, and a 204 delete.
func TestCastCRUDHappyPath(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, castFixture(screenID, map[string]string{"env": "prod"})), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /casts: status %d, body %s", resp.StatusCode, raw)
	}
	created := decodeCast(t, raw)
	if created.ID == "" {
		t.Fatal("the created cast carries no server-assigned id")
	}
	if len(created.Slides) != 2 || created.Slides[0].ID != "title" || created.Slides[1].ID != "photo" {
		t.Fatalf("the created cast did not round-trip its slides in order: %+v", created.Slides)
	}
	if created.Revision != 1 {
		t.Errorf("created revision = %d, want 1", created.Revision)
	}
	if loc := resp.Header.Get("Location"); loc != "/api/v1/casts/"+created.ID {
		t.Errorf("Location = %q, want /api/v1/casts/%s", loc, created.ID)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("the create response carries no ETag")
	}

	getResp, getRaw := e.do(t, http.MethodGet, "/api/v1/casts/"+created.ID, nil, nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET the cast: status %d, body %s", getResp.StatusCode, getRaw)
	}
	if got := decodeCast(t, getRaw); got.Name != "Lunch Menu" || len(got.Slides) != 2 {
		t.Errorf("the read-back cast is not what was written: %+v", got)
	}

	// A patch REPLACES the slide list — an ordered document has no member-wise
	// merge — and is conditioned on the current ETag.
	patch := mustJSON(t, map[string]any{
		"name":   "Dinner Menu",
		"slides": []datamodel.CastSlide{castSlideFixture("only")},
	})
	patchResp, patchRaw := e.do(t, http.MethodPatch, "/api/v1/casts/"+created.ID, patch, map[string]string{"If-Match": etag})
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH the cast: status %d, body %s", patchResp.StatusCode, patchRaw)
	}
	updated := decodeCast(t, patchRaw)
	if updated.Name != "Dinner Menu" || len(updated.Slides) != 1 || updated.Slides[0].ID != "only" {
		t.Fatalf("the patch did not replace the slide list: %+v", updated)
	}
	if updated.Revision != 2 {
		t.Errorf("updated revision = %d, want 2", updated.Revision)
	}

	delResp, delRaw := e.do(t, http.MethodDelete, "/api/v1/casts/"+created.ID, nil,
		map[string]string{"If-Match": patchResp.Header.Get("ETag")})
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE the cast: status %d, body %s", delResp.StatusCode, delRaw)
	}
	if gone, _ := e.do(t, http.MethodGet, "/api/v1/casts/"+created.ID, nil, nil); gone.StatusCode != http.StatusNotFound {
		t.Errorf("the deleted cast still reads %d, want 404", gone.StatusCode)
	}
}

// TestCastListIsSelectorFilterable proves the family reaches the shared list
// conventions — the label selector and the {items, cursor} envelope — rather
// than a hand-rolled listing.
func TestCastListIsSelectorFilterable(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	e.createOK(t, "/api/v1/casts", rowCreateBody(t, castFixture(screenID, map[string]string{"env": "prod"})))
	e.createOK(t, "/api/v1/casts", rowCreateBody(t, castFixture(screenID, map[string]string{"env": "lab"})))

	resp, raw := e.do(t, http.MethodGet, "/api/v1/casts?selector=env%3Dprod", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /casts: status %d, body %s", resp.StatusCode, raw)
	}
	var page struct {
		Items  []datamodel.Cast `json:"items"`
		Cursor *string          `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode page: %v (body %s)", err, raw)
	}
	if len(page.Items) != 1 || page.Items[0].Labels["env"] != "prod" {
		t.Fatalf("selector env=prod returned %+v, want only the prod cast", page.Items)
	}
}

// TestAnUndrawableCastIsRefused is the authoring gate seen from the surface: a
// cast whose slide would not draw never reaches the store, and the refusal names
// the offending slide.
func TestAnUndrawableCastIsRefused(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	bad := castFixture(screenID, nil)
	// Second slide runs off the right edge of the 1920x1080 canvas — a shape the
	// declared schema admits (every member is in range on its own), so this is
	// genuinely the data-model gate answering, not the schema gate.
	bad.Slides[1] = datamodel.CastSlide{ID: "offcanvas", Layers: []wire.Layer{
		{Kind: wire.LayerKindText, X: 1800, Y: 0, W: 400, H: 100, Text: "clipped"},
	}}

	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, bad), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST an undrawable cast: status %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	if !problemNamesField(p, "slides[1].layers") {
		t.Errorf("the refusal does not name the offending slide: %s", raw)
	}
	if e.castCount(t) != 0 {
		t.Error("an undrawable cast was stored despite the 422")
	}
}

// TestACastWithNoSlidesIsRefusedOverHTTP pins the empty-cast rule at the
// surface. Here it is the DECLARED SCHEMA that answers first (`minItems: 1`),
// which is the division of labour casts.go describes: shape is the document's,
// drawability is the data model's.
func TestACastWithNoSlidesIsRefusedOverHTTP(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	empty := castFixture(screenID, nil)
	empty.Slides = []datamodel.CastSlide{}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, empty), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST a slideless cast: status %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")
	if e.castCount(t) != 0 {
		t.Error("a slideless cast was stored despite the 422")
	}
}

// TestAPatchCannotIntroduceAnUndrawableSlide closes the half a create-only check
// would leave open: the effective POST-MERGE row is validated exactly as a
// create is, so an editor cannot save a broken slide onto a cast that was fine.
func TestAPatchCannotIntroduceAnUndrawableSlide(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)
	id := e.createCast(t, screenID)
	etag := e.castETag(t, id)

	patch := mustJSON(t, map[string]any{"slides": []datamodel.CastSlide{{
		ID: "broken", Layers: []wire.Layer{{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 100, H: 100}},
	}}})
	resp, raw := e.do(t, http.MethodPatch, "/api/v1/casts/"+id, patch, map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH an undrawable slide: status %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	// And the stored row is untouched — a rejected write changes nothing.
	if got := decodeCast(t, e.getOK(t, "/api/v1/casts/"+id)); len(got.Slides) != 2 || got.Revision != 1 {
		t.Errorf("the refused patch changed the stored cast: %+v", got)
	}
}

// TestAPlaylistMayReferenceACastAndTheCastThenResistsDeletion is the pair of
// rules that make a cast schedulable AND safe: a playlist item may name it, and
// once one does, deleting the cast is refused rather than leaving a reference
// that plays nothing.
func TestAPlaylistMayReferenceACastAndTheCastThenResistsDeletion(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)
	castID := e.createCast(t, screenID)

	pl := playlistFixture(screenID, nil)
	pl.Items = append(pl.Items, datamodel.PlaylistItem{Source: datamodel.PlaylistSourceCast, CastID: castID})
	e.createOK(t, "/api/v1/playlists", rowCreateBody(t, pl))

	etag := e.castETag(t, castID)
	resp, raw := e.do(t, http.MethodDelete, "/api/v1/casts/"+castID, nil, map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("DELETE a referenced cast: status %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")
	if e.castCount(t) != 1 {
		t.Error("a referenced cast was deleted despite the 422")
	}
}

// TestAPlaylistCannotReferenceACastThatDoesNotExist is the same referential rule
// from the other side, at the moment the playlist is written.
func TestAPlaylistCannotReferenceACastThatDoesNotExist(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	pl := playlistFixture(screenID, nil)
	pl.Items = []datamodel.PlaylistItem{{Source: datamodel.PlaylistSourceCast, CastID: missingPlaylistID}}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/playlists", rowCreateBody(t, pl), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST a playlist naming no cast: status %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	if !problemNamesField(p, "items[0].cast_id") {
		t.Errorf("the refusal does not name the dangling reference: %s", raw)
	}
}

// TestACastRejectsAnUndeclaredMember proves the declared request schema is
// enforced at request time rather than merely published — the gate no data-model
// rule can express, because an undeclared member has already vanished by the
// time a row is decoded.
func TestACastRejectsAnUndeclaredMember(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	body := mustJSON(t, map[string]any{
		"scope_node": screenID,
		"name":       "Lunch Menu",
		"slides":     []datamodel.CastSlide{castSlideFixture("s1")},
		"transition": "crossfade", // not a member this operation declares
	})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", body, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST an undeclared member: status %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")
}

// castETag reads a cast's current ETag as the env's own principal, so a
// patch/delete under test carries a genuinely valid If-Match and can only fail
// on the rule it is about.
func (e *testEnv) castETag(t *testing.T, id string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, "/api/v1/casts/"+id, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET the cast for its ETag: status %d, body %s", resp.StatusCode, raw)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("the cast read returned no ETag")
	}
	return etag
}

// getOK reads path and fails unless it answered 200.
func (e *testEnv) getOK(t *testing.T, path string) []byte {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", path, resp.StatusCode, raw)
	}
	return raw
}

// castCount reports how many cast rows the store holds, so a test can assert
// that a refused write stored NOTHING rather than only that it answered 422.
func (e *testEnv) castCount(t *testing.T) int {
	t.Helper()
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(e.getOK(t, "/api/v1/casts"), &page); err != nil {
		t.Fatalf("decode cast page: %v", err)
	}
	return len(page.Items)
}

// problemNamesField reports whether an api/1 Problem's `errors` extension
// (API-013) carries an entry for the named field — the multi-field answer a
// per-slide validation is required to produce.
func problemNamesField(p map[string]any, field string) bool {
	entries, _ := p["errors"].([]any)
	for _, e := range entries {
		if m, ok := e.(map[string]any); ok && m["field"] == field {
			return true
		}
	}
	return false
}
