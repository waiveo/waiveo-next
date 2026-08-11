package api_test

import (
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// casts_widgets_test.go drives the three things a cast row gained for parity
// rows 1.7/1.8/3.6 over the REAL mounted mux: the LIVE widget layer kinds, the
// cast-level default dwell time, and the template flavor.
//
// All three exist to close the same shape of defect from opposite ends. Wave 1
// taught the wire, the live-value resolver (internal/slidelive) and the player
// to render `date`/`countdown`/`weather`/`entity` layers — and then left
// `POST /casts` answering 422 for every one of them, because the declared
// request schema's kind enum still listed four. A capability nothing can author
// is not a capability, and the only test that can tell the difference is one
// that goes through the door an operator goes through.

// widgetSlide is one slide carrying ALL FOUR live widget kinds at once. Together
// rather than one per case on purpose: the enum is a single closed list, and a
// test that admitted three of the four while the fourth was still refused would
// be reporting a green that the console could not reproduce.
func widgetSlide(id string) datamodel.CastSlide {
	return datamodel.CastSlide{
		ID: id, DurationMS: 8000,
		Layers: []wire.Layer{
			{Kind: wire.LayerKindDate, X: 80, Y: 60, W: 700, H: 120, Text: "Monday, January 2", FontPx: 72, Color: "#FFFFFF"},
			{Kind: wire.LayerKindCountdown, X: 80, Y: 220, W: 700, H: 160, Text: "DD:HH:MM:SS", TargetMS: 1893456000000, FontPx: 96},
			{Kind: wire.LayerKindWeather, X: 1100, Y: 60, W: 700, H: 140, Text: "{temp}° {cond}", FontPx: 80, Align: "right"},
			{Kind: wire.LayerKindEntity, X: 1100, Y: 240, W: 700, H: 120, EntityID: "01J8ZENT1TY000000000000001", Text: "Lobby TV: {state}", FontPx: 64},
		},
	}
}

// TestACastCanAuthorEveryLiveWidgetKind is the regression that names the wave-1
// gap: every kind the player renders MUST be a kind the authoring API accepts,
// and each one must survive the round trip with its OWN kind-specific field
// intact (a countdown's target, an entity's subject) rather than being accepted
// and then silently flattened.
func TestACastCanAuthorEveryLiveWidgetKind(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	c := datamodel.Cast{ScopeNode: screenID, Name: "Widget Board", Slides: []datamodel.CastSlide{widgetSlide("widgets")}}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, c), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST a cast of live widget layers: status %d, want 201 (body %s)", resp.StatusCode, raw)
	}

	got := decodeCast(t, e.getOK(t, "/api/v1/casts/"+decodeCast(t, raw).ID))
	if len(got.Slides) != 1 || len(got.Slides[0].Layers) != 4 {
		t.Fatalf("the widget slide did not round-trip its four layers: %+v", got.Slides)
	}
	layers := got.Slides[0].Layers
	wantKinds := []string{wire.LayerKindDate, wire.LayerKindCountdown, wire.LayerKindWeather, wire.LayerKindEntity}
	for i, want := range wantKinds {
		if layers[i].Kind != want {
			t.Errorf("layers[%d].kind = %q, want %q", i, layers[i].Kind, want)
		}
	}
	// The two kind-specific members. Neither is expressible as any other kind's
	// field, so an enum widened without them would 201 and store a countdown
	// that counts to the epoch and an entity that names nothing.
	if layers[1].TargetMS != 1893456000000 {
		t.Errorf("the countdown's target_ms did not round-trip: %d", layers[1].TargetMS)
	}
	if layers[3].EntityID != "01J8ZENT1TY000000000000001" {
		t.Errorf("the entity layer's entity_id did not round-trip: %q", layers[3].EntityID)
	}
}

// TestAWidgetLayerMissingItsOwnRequiredFieldIsRefused proves the widened enum
// did not widen into a hole: each live kind still has to satisfy the SHARED
// layer gate (wire.ValidateAuthoredSlideLayers), reported per-slide as
// CAST_SLIDE_LAYERS_INVALID rather than accepted and dropped at serve time.
func TestAWidgetLayerMissingItsOwnRequiredFieldIsRefused(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	for _, tc := range []struct {
		name  string
		layer wire.Layer
	}{
		{"a date with no format", wire.Layer{Kind: wire.LayerKindDate, X: 0, Y: 0, W: 400, H: 100}},
		{"a countdown with no target", wire.Layer{Kind: wire.LayerKindCountdown, X: 0, Y: 0, W: 400, H: 100, Text: "HH:MM:SS"}},
		{"a weather with no template", wire.Layer{Kind: wire.LayerKindWeather, X: 0, Y: 0, W: 400, H: 100}},
		{"an entity naming nothing", wire.Layer{Kind: wire.LayerKindEntity, X: 0, Y: 0, W: 400, H: 100, Text: "{state}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := datamodel.Cast{ScopeNode: screenID, Name: "Broken", Slides: []datamodel.CastSlide{{ID: "s1", Layers: []wire.Layer{tc.layer}}}}
			resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, c), nil)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422 (body %s)", resp.StatusCode, raw)
			}
			p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
			if !problemNamesField(p, "slides[0].layers") {
				t.Errorf("the refusal does not name the offending slide's layers: %s", raw)
			}
		})
	}
}

// TestACastCarriesAndClearsItsOwnDefaultDuration drives the cast-level playback
// setting end to end, INCLUDING the clear.
//
// The clear is the half that would otherwise ship broken. `default_duration_ms`
// is `omitempty`, a PATCH shallow-merges over the stored body, and the schema's
// `minimum: 1` (correctly) refuses a zero — so an update that omitted the member
// would mean "leave it alone" and there would be no way at all to turn a
// cast-wide default back off. Explicit `null` is that way, and this is what
// proves it is not merely documented.
func TestACastCarriesAndClearsItsOwnDefaultDuration(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	c := castFixture(screenID, nil)
	c.DefaultDurationMS = 7000
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, c), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST a cast with a default duration: status %d (body %s)", resp.StatusCode, raw)
	}
	id := decodeCast(t, raw).ID
	if got := decodeCast(t, e.getOK(t, "/api/v1/casts/"+id)); got.DefaultDurationMS != 7000 {
		t.Fatalf("default_duration_ms read back as %d, want 7000", got.DefaultDurationMS)
	}

	patch := mustJSON(t, map[string]any{"default_duration_ms": nil})
	pResp, pRaw := e.do(t, http.MethodPatch, "/api/v1/casts/"+id, patch, map[string]string{"If-Match": e.castETag(t, id)})
	if pResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH clearing the default duration: status %d, want 200 (body %s)", pResp.StatusCode, pRaw)
	}
	if got := decodeCast(t, e.getOK(t, "/api/v1/casts/"+id)); got.DefaultDurationMS != 0 {
		t.Errorf("default_duration_ms survived an explicit null clear as %d", got.DefaultDurationMS)
	}
}

// TestANonPositiveDefaultDurationIsRefused pins the data-model rule beneath the
// schema's own minimum: a negative is a dwell time nothing can honour, and it is
// reported against the cast's own field rather than against a slide, because
// that is the control an operator has to go and fix.
func TestANonPositiveDefaultDurationIsRefused(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	body := mustJSON(t, map[string]any{
		"scope_node":          screenID,
		"name":                "Lunch Menu",
		"slides":              []datamodel.CastSlide{castSlideFixture("s1")},
		"default_duration_ms": -1,
	})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", body, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST a negative default duration: status %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")
}

// TestATemplateCastCannotBeScheduled is the whole reason `template` is a server
// field and not a console-side convention: a template exists to be EDITED as the
// source of future casts, so a screen playing one would change every time
// somebody improved the starting point.
//
// It also pins the code. The reference resolves perfectly well — the row is
// right there in the operator's template gallery — so REFERENCE_INVALID would be
// an actively misleading answer, sending them to look for a cast that is not
// missing.
func TestATemplateCastCannotBeScheduled(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	tpl := castFixture(screenID, nil)
	tpl.Name = "Title + Clock"
	tpl.Template = true
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, tpl), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST a template cast: status %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	templateID := decodeCast(t, raw).ID
	if got := decodeCast(t, e.getOK(t, "/api/v1/casts/"+templateID)); !got.Template {
		t.Fatal("the template flag did not round-trip; a template that reads back as an ordinary cast is not a template")
	}

	pl := playlistFixture(screenID, nil)
	pl.Items = []datamodel.PlaylistItem{{Source: datamodel.PlaylistSourceCast, CastID: templateID}}
	plResp, plRaw := e.do(t, http.MethodPost, "/api/v1/playlists", rowCreateBody(t, pl), nil)
	if plResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST a playlist scheduling a template: status %d, want 422 (body %s)", plResp.StatusCode, plRaw)
	}
	p := assertProblem(t, plResp, plRaw, "VALIDATION_FAILED")
	if !problemNamesField(p, "items[0].cast_id") {
		t.Errorf("the refusal does not name the offending item: %s", plRaw)
	}
	if !problemCarriesCode(p, "items[0].cast_id", "CAST_TEMPLATE_NOT_SCHEDULABLE") {
		t.Errorf("want CAST_TEMPLATE_NOT_SCHEDULABLE on items[0].cast_id, got %s", plRaw)
	}
}

// TestAnAlreadyScheduledCastCannotBeFlippedToATemplate is the rule from the
// direction a one-way check at playlist-write time would miss entirely.
//
// It works because the rule lives in the whole-row-set validator, which the
// store re-runs over the set a write would LEAVE BEHIND — the same mechanism
// that already refuses DELETING a scheduled cast. Without it, "save as template"
// on a cast three screens are playing would quietly take those screens' content
// away at the next projection, with a 200 on the console.
func TestAnAlreadyScheduledCastCannotBeFlippedToATemplate(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	castID := e.createCast(t, screenID)
	pl := playlistFixture(screenID, nil)
	pl.Items = []datamodel.PlaylistItem{{Source: datamodel.PlaylistSourceCast, CastID: castID}}
	if plResp, plRaw := e.do(t, http.MethodPost, "/api/v1/playlists", rowCreateBody(t, pl), nil); plResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST a playlist scheduling the cast: status %d (body %s)", plResp.StatusCode, plRaw)
	}

	patch := mustJSON(t, map[string]any{"template": true})
	resp, raw := e.do(t, http.MethodPatch, "/api/v1/casts/"+castID, patch, map[string]string{"If-Match": e.castETag(t, castID)})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH template:true onto a scheduled cast: status %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	if got := decodeCast(t, e.getOK(t, "/api/v1/casts/"+castID)); got.Template {
		t.Error("the cast was marked a template despite the 422")
	}
}

// problemCarriesCode reports whether an api/1 Problem's `errors` extension
// (API-013) carries an entry for the named field WITH the named code. The field
// alone is not enough for a rule whose whole point is being distinguishable from
// the REFERENCE_INVALID that shares its field.
func problemCarriesCode(p map[string]any, field, code string) bool {
	entries, _ := p["errors"].([]any)
	for _, e := range entries {
		if m, ok := e.(map[string]any); ok && m["field"] == field && m["code"] == code {
			return true
		}
	}
	return false
}
