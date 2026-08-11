package slidelive

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// stubWeather is a WeatherSource that answers from a fixed reading and counts
// how many times it was asked — the call count is what pins the "one lookup per
// slide" rule, which is not observable from the resolved text alone.
type stubWeather struct {
	conds Conditions
	ok    bool
	calls int
	lat   float64
	long  float64
}

func (s *stubWeather) Current(_ context.Context, lat, long float64) (Conditions, bool) {
	s.calls++
	s.lat, s.long = lat, long
	return s.conds, s.ok
}

// stubEntities is an EntitySource over a fixed map. A key present with an empty
// value is a distinct case from an absent key, so the map is the whole state.
type stubEntities map[string]string

func (s stubEntities) EntityState(id string) (string, bool) {
	v, ok := s[id]
	return v, ok
}

func layer(kind string) wire.Layer {
	return wire.Layer{Kind: kind, X: 0, Y: 0, W: 100, H: 50}
}

// siteLat/siteLong are a real site's coordinates. Every case that expects a
// weather lookup to HAPPEN has to carry them, because a Sources with no adopted
// geo deliberately performs no lookup at all (Sources.HasGeo) — so a fixture
// that left them at zero would be testing the degrade path while claiming to
// test the resolution path.
const (
	siteLat  = 39.7392
	siteLong = -104.9903
)

func TestResolveLayersFillsWeatherFromTheTemplate(t *testing.T) {
	w := &stubWeather{conds: Conditions{TempF: 72, TempC: 22, Text: "Partly cloudy"}, ok: true}
	l := layer(wire.LayerKindWeather)
	l.Text = "{temp}°F / {tempc}°C · {cond}"

	out := ResolveLayers([]wire.Layer{l}, Sources{Weather: w, Lat: 39.7392, Long: -104.9903})

	if got, want := out[0].Value, "72°F / 22°C · Partly cloudy"; got != want {
		t.Fatalf("resolved value = %q, want %q", got, want)
	}
	if w.lat != 39.7392 || w.long != -104.9903 {
		t.Fatalf("the source must be asked at the configured site coordinates, got (%v,%v)", w.lat, w.long)
	}
}

func TestResolveLayersAsksWeatherOnceForTheWholeSlide(t *testing.T) {
	// A temperature and a condition drawn as two separately positioned widgets
	// is normal authoring. Asking twice would both double the load and let one
	// slide draw two readings from different instants.
	w := &stubWeather{conds: Conditions{TempF: 60, TempC: 16, Text: "Fog"}, ok: true}
	temp := layer(wire.LayerKindWeather)
	temp.Text = "{temp}"
	cond := layer(wire.LayerKindWeather)
	cond.Text = "{cond}"

	out := ResolveLayers([]wire.Layer{temp, cond}, Sources{Weather: w, Lat: siteLat, Long: siteLong})

	if w.calls != 1 {
		t.Fatalf("weather must be looked up once per slide, got %d calls", w.calls)
	}
	if out[0].Value != "60" || out[1].Value != "Fog" {
		t.Fatalf("both widgets must resolve from the one reading, got %q and %q", out[0].Value, out[1].Value)
	}
}

func TestResolveLayersDegradesWeatherWithoutLosingLayout(t *testing.T) {
	// An unavailable reading substitutes the placeholder into every VALUE token
	// while leaving the author's literal text alone, so the widget stays
	// recognizable and stays in its authored position.
	cases := map[string]Sources{
		"no source configured": {},
		"source has no answer": {Weather: &stubWeather{ok: false}, Lat: siteLat, Long: siteLong},
		// The relay booted with no app peer, so no site_binding was adopted and
		// the coordinates are still (0,0). The source COULD answer about that
		// point — Open-Meteo answers about the Gulf of Guinea like anywhere
		// else — which is precisely why nothing may be asked.
		"no coordinates adopted": {Weather: &stubWeather{conds: Conditions{TempF: 84, TempC: 29, Text: "Clear"}, ok: true}},
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			l := layer(wire.LayerKindWeather)
			l.Text = "Now: {temp}° {cond}"
			out := ResolveLayers([]wire.Layer{l}, src)
			if got, want := out[0].Value, "Now: "+Unavailable+"° "+Unavailable; got != want {
				t.Fatalf("degraded value = %q, want %q", got, want)
			}
		})
	}
}

func TestResolveLayersFillsEntityState(t *testing.T) {
	l := layer(wire.LayerKindEntity)
	l.EntityID = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"
	l.Text = "Lobby screen is {state}"

	out := ResolveLayers([]wire.Layer{l}, Sources{
		Entity: stubEntities{"01J8Z3K4N5P6Q7R8S9T0V1SCRN": "playing"},
	})

	if got, want := out[0].Value, "Lobby screen is playing"; got != want {
		t.Fatalf("resolved value = %q, want %q", got, want)
	}
}

func TestResolveLayersEntityDefaultsToTheBareState(t *testing.T) {
	// An entity layer's only value IS its state, so a blank template can safely
	// mean "state and nothing else" — unlike weather, whose several fields have
	// no single obvious default and which the validator therefore requires.
	l := layer(wire.LayerKindEntity)
	l.EntityID = "E"
	out := ResolveLayers([]wire.Layer{l}, Sources{Entity: stubEntities{"E": "off"}})
	if out[0].Value != "off" {
		t.Fatalf("value = %q, want %q", out[0].Value, "off")
	}
}

func TestResolveLayersEntityDegrades(t *testing.T) {
	cases := map[string]Sources{
		"no source configured":  {},
		"entity never observed": {Entity: stubEntities{}},
		// Observed, but with an empty canonical state: from a wall's point of
		// view that tells a viewer nothing, so it reads the same as unknown.
		"observed empty state": {Entity: stubEntities{"E": ""}},
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			l := layer(wire.LayerKindEntity)
			l.EntityID = "E"
			l.Text = "Screen: {state}"
			out := ResolveLayers([]wire.Layer{l}, src)
			if got, want := out[0].Value, "Screen: "+Unavailable; got != want {
				t.Fatalf("degraded value = %q, want %q", got, want)
			}
		})
	}
}

func TestResolveLayersLeavesEveryOtherKindAlone(t *testing.T) {
	// The clock/date/countdown kinds are the player's own job, and text/rect/
	// image carry no server-resolved half at all: none of them may acquire a
	// Value here, and none of their authored fields may change.
	in := []wire.Layer{}
	for _, kind := range []string{
		wire.LayerKindText, wire.LayerKindRect, wire.LayerKindImage,
		wire.LayerKindClock, wire.LayerKindDate, wire.LayerKindCountdown,
	} {
		l := layer(kind)
		l.Text = "authored"
		in = append(in, l)
	}

	out := ResolveLayers(in, Sources{
		Weather: &stubWeather{conds: Conditions{TempF: 1, Text: "Clear"}, ok: true},
		Entity:  stubEntities{"E": "on"},
		Lat:     siteLat,
		Long:    siteLong,
	})

	for i := range out {
		if out[i].Value != "" {
			t.Fatalf("layer %d (%s) must not be given a resolved value, got %q", i, out[i].Kind, out[i].Value)
		}
		// reflect.DeepEqual, not ==: wire.Layer gained a slice member (Items,
		// the nav menu) and is no longer comparable. Same assertion.
		if !reflect.DeepEqual(out[i], in[i]) {
			t.Fatalf("layer %d (%s) was modified: %+v vs %+v", i, out[i].Kind, out[i], in[i])
		}
	}
}

func TestResolveContentDoesNotMutateTheServedProgram(t *testing.T) {
	// The input slice is the relay's retained served-program state, reused for
	// every subsequent poll. Writing a resolved value into it would overwrite
	// the authored layer permanently with one poll's answer — and would race the
	// next SetProgram. The input must come back untouched.
	weather := layer(wire.LayerKindWeather)
	weather.Text = "{temp}"
	content := []wire.LeaseContent{
		{Type: "image", AssetRef: "sha256:aa", URL: "https://origin/content/aa"},
		{Type: "slide", Layers: []wire.Layer{weather}},
	}

	out := ResolveContent(content, Sources{
		Weather: &stubWeather{conds: Conditions{TempF: 81}, ok: true},
		Lat:     siteLat,
		Long:    siteLong,
	})

	if content[1].Layers[0].Value != "" {
		t.Fatalf("the input program was mutated: %q", content[1].Layers[0].Value)
	}
	if out[1].Layers[0].Value != "81" {
		t.Fatalf("the returned copy must carry the resolved value, got %q", out[1].Layers[0].Value)
	}
	if !reflect.DeepEqual(out[0], content[0]) {
		t.Fatalf("a plain image item must pass through unchanged: %+v", out[0])
	}
}

func TestResolveContentHandlesAnEmptyProgram(t *testing.T) {
	// A blank screen's Lease carries an empty content array (REL-060's no-null
	// rule), and it must stay an empty array rather than becoming nil.
	out := ResolveContent([]wire.LeaseContent{}, Sources{})
	if out == nil || len(out) != 0 {
		t.Fatalf("an empty content array must survive resolution as an empty array, got %#v", out)
	}
}

func TestUnknownTemplateTokenStaysLiteral(t *testing.T) {
	// An author who mistypes a token must SEE the mistype on the wall — that is
	// how it gets fixed. Swallowing it would leave a blank widget and no clue.
	l := layer(wire.LayerKindWeather)
	l.Text = "{tmp} {temp}"
	out := ResolveLayers([]wire.Layer{l}, Sources{
		Weather: &stubWeather{conds: Conditions{TempF: 55}, ok: true},
		Lat:     siteLat,
		Long:    siteLong,
	})
	if got, want := out[0].Value, "{tmp} 55"; got != want {
		t.Fatalf("value = %q, want %q", got, want)
	}
}

// THE regression guard for the "weather at (0,0)" defect. A wrong-but-plausible
// temperature on a wall is worse than a dash, because nobody notices it is
// wrong — so the source is not merely IGNORED when the relay has no location,
// it is never ASKED, which is also what keeps a boot with no app peer from
// pointing a public weather API at a coordinate the site never authored.
func TestWeatherIsNeverAskedAtCoordinatesTheRelayDoesNotHave(t *testing.T) {
	cases := map[string]struct{ lat, long float64 }{
		// The zero hello.SiteBinding: what a relay that booted into
		// offline-serve (REL-055/061) holds until an app peer answers.
		"no site_binding adopted": {0, 0},
		"latitude out of range":   {91, 10},
		"longitude out of range":  {10, 181},
		"latitude NaN":            {math.NaN(), 10},
		"longitude infinite":      {10, math.Inf(1)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := &stubWeather{conds: Conditions{TempF: 84, TempC: 29, Text: "Clear"}, ok: true}
			l := layer(wire.LayerKindWeather)
			l.Text = "{temp}° {cond}"

			out := ResolveLayers([]wire.Layer{l}, Sources{Weather: w, Lat: c.lat, Long: c.long})

			if w.calls != 0 {
				t.Errorf("the weather source was asked at (%v,%v); a relay that does not know where it is must ask nobody", c.lat, c.long)
			}
			if got, want := out[0].Value, Unavailable+"° "+Unavailable; got != want {
				t.Errorf("value = %q, want the unavailable placeholder %q — a plausible reading for the wrong place is worse than a dash", got, want)
			}
		})
	}
}

func TestHasGeoAcceptsRealSites(t *testing.T) {
	// The range checks must not reject legitimate sites, including the extremes
	// and the axes: a site ON the equator or the prime meridian (but not both)
	// is a real place and keeps its weather.
	for _, s := range []Sources{
		{Lat: siteLat, Long: siteLong},
		{Lat: 0, Long: -104.9903}, // on the equator
		{Lat: 39.7392, Long: 0},   // on the prime meridian
		{Lat: -90, Long: 180},     // the corners of the range are valid
		{Lat: 90, Long: -180},
	} {
		if !s.HasGeo() {
			t.Errorf("(%v,%v) is a real location and must keep its weather", s.Lat, s.Long)
		}
	}
}
