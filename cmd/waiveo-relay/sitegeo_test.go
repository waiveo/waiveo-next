package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
	"github.com/maaxton/waiveo-next/internal/slidelive"
)

// recordingWeather answers a fixed reading and records the coordinates it was
// asked at — which is the whole subject here. A source that is never asked is
// the CORRECT outcome for a relay with no adopted site, and the only way to see
// the difference between "not asked" and "asked about the wrong place" is to
// watch the source rather than the rendered string.
type recordingWeather struct {
	asked []string
}

func (w *recordingWeather) Current(_ context.Context, lat, long float64) (slidelive.Conditions, bool) {
	w.asked = append(w.asked, coords(lat, long))
	return slidelive.Conditions{TempF: 84, TempC: 29, Text: "Clear"}, true
}

// coords renders a coordinate pair as one comparable, readable string, so a
// failure message shows the place the source was asked about rather than two
// floats.
func coords(lat, long float64) string {
	return fmt.Sprintf("%g,%g", lat, long)
}

// installedSources captures what siteGeo hands the player server, so a case can
// resolve a real slide through it exactly as a Lease would.
type installedSources struct {
	src   slidelive.Sources
	count int
}

func (i *installedSources) set(src slidelive.Sources) {
	i.src = src
	i.count++
}

// resolveWeather runs the captured Sources through the real resolution path and
// returns the string a screen would paint.
func (i *installedSources) resolveWeather(t *testing.T) string {
	t.Helper()
	if i.count == 0 {
		t.Fatal("no slidelive.Sources was ever installed, so nothing would resolve a weather widget at all")
	}
	out := slidelive.ResolveLayers([]wire.Layer{{
		Kind: wire.LayerKindWeather, X: 0, Y: 0, W: 400, H: 100, Text: "{temp}° {cond}",
	}}, i.src)
	return out[0].Value
}

func newGeoFixture() (*siteGeo, *installedSources, *recordingWeather, *[]hello.SiteBinding) {
	installed := &installedSources{}
	weather := &recordingWeather{}
	var locations []hello.SiteBinding
	g := &siteGeo{
		setSlideLive: installed.set,
		setLocation: func(tz string, lat, long float64) error {
			locations = append(locations, hello.SiteBinding{TZ: tz, Lat: lat, Long: long})
			return nil
		},
		weather: weather,
	}
	return g, installed, weather, &locations
}

// theHanger is a real site_binding — the shape an app peer answers a hello with.
var theHanger = hello.SiteBinding{ScopeNode: "01J8Z3K4N5P6Q7R8S9T0V1SITE", TZ: "America/Denver", Lat: 39.7392, Long: -104.9903}

func TestAdoptingASiteResolvesWeatherAtItsOwnCoordinates(t *testing.T) {
	g, installed, weather, locations := newGeoFixture()

	g.adopt(theHanger)

	if got := installed.resolveWeather(t); got != "84° Clear" {
		t.Errorf("weather = %q, want %q", got, "84° Clear")
	}
	if len(weather.asked) != 1 || weather.asked[0] != coords(theHanger.Lat, theHanger.Long) {
		t.Errorf("the source was asked at %v, want one lookup at the site's own coordinates %s", weather.asked, coords(theHanger.Lat, theHanger.Long))
	}
	if len(*locations) != 1 || (*locations)[0].TZ != "America/Denver" {
		t.Errorf("the automation engine's location was set to %+v, want the site's tz — a slide's weather and a rule's sunset must agree about where this relay is", *locations)
	}
}

// THE regression guard, in the shape the box actually fails: the relay boots
// while its app peer is down (offline-serve, REL-055/061), so it adopts the
// ZERO site_binding and has no idea where it is. It must paint a dash — not the
// Gulf of Guinea's temperature, which is what asking at (0,0) returns and which
// nobody looking at the wall could tell was wrong.
func TestABootWithNoAppPeerAsksTheWeatherAtNoCoordinatesAtAll(t *testing.T) {
	g, installed, weather, _ := newGeoFixture()

	g.adopt(hello.SiteBinding{})

	if len(weather.asked) != 0 {
		t.Errorf("the weather source was asked at %v; a relay that adopted no site_binding knows no location and must ask nobody", weather.asked)
	}
	if got, want := installed.resolveWeather(t), slidelive.Unavailable+"° "+slidelive.Unavailable; got != want {
		t.Errorf("weather = %q, want the unavailable placeholder %q", got, want)
	}
}

// The other half of the same defect: adopting late must WORK. The boot value is
// not the last word — the supervisor reconnects seconds or hours later with the
// real site, and the screen has to correct itself without the process being
// restarted.
func TestASiteAdoptedAfterAnOfflineBootTakesEffectWithoutARestart(t *testing.T) {
	g, installed, weather, locations := newGeoFixture()

	g.adopt(hello.SiteBinding{}) // boot: app peer down
	g.adopt(theHanger)           // the supervisor reconnects and hands over the real site

	if got := installed.resolveWeather(t); got != "84° Clear" {
		t.Errorf("weather after the site was adopted = %q, want %q — a relay that boots offline must self-correct, not stay wrong until someone restarts it", got, "84° Clear")
	}
	if len(weather.asked) != 1 || weather.asked[0] != coords(theHanger.Lat, theHanger.Long) {
		t.Errorf("the source was asked at %v, want exactly one lookup, at the adopted site's coordinates", weather.asked)
	}
	if len(*locations) != 2 || (*locations)[1].TZ != "America/Denver" {
		t.Errorf("engine locations = %+v, want the second adoption to carry the real tz", *locations)
	}
}

func TestReAdoptingTheSameSiteIsANoOp(t *testing.T) {
	// Every reconnect re-adopts, and nearly all of them carry the binding
	// already held. Re-installing on each would churn the player server's lock
	// and log a line per reconnect for a fact that did not change.
	g, installed, _, locations := newGeoFixture()

	g.adopt(theHanger)
	g.adopt(theHanger)
	g.adopt(theHanger)

	if installed.count != 1 {
		t.Errorf("slidelive sources installed %d times for one unchanged site, want 1", installed.count)
	}
	if len(*locations) != 1 {
		t.Errorf("engine location set %d times for one unchanged site, want 1", len(*locations))
	}
}

func TestARelayReboundToAnotherSiteFollowsIt(t *testing.T) {
	// A site REBIND is the case adoptSite's own doc names: the binding changed
	// while this relay was offline, and the first hello-ack after reconnecting
	// is where it learns. Every geo consumer has to move with it.
	g, installed, weather, _ := newGeoFixture()
	elsewhere := hello.SiteBinding{ScopeNode: "01J8Z3K4N5P6Q7R8S9T0V1ELSE", TZ: "Europe/Lisbon", Lat: 38.7223, Long: -9.1393}

	g.adopt(theHanger)
	installed.resolveWeather(t) // a Lease issued while bound to the first site
	g.adopt(elsewhere)
	installed.resolveWeather(t) // and one issued after the rebind

	if len(weather.asked) != 2 || weather.asked[1] != coords(elsewhere.Lat, elsewhere.Long) {
		t.Errorf("the source was asked at %v, want the second lookup at the rebound site's coordinates", weather.asked)
	}
}

// The wiring half: the geo is re-adopted from rePuller.adoptSite, which is what
// the reconnect supervisor already calls with each fresh hello-ack's binding. A
// siteGeo nothing drives is a fix that never runs.
func TestAdoptSiteReAdoptsTheGeoAndTheScheduleSite(t *testing.T) {
	g, installed, weather, _ := newGeoFixture()
	p := &rePuller{driver: &scheduleDriver{}, geo: g}

	p.adoptSite(theHanger)

	if p.driver.site != theHanger {
		t.Errorf("the schedule driver's site = %+v, want the adopted binding", p.driver.site)
	}
	if got := installed.resolveWeather(t); got != "84° Clear" {
		t.Errorf("weather after adoptSite = %q, want %q — the site's geo must be re-adopted by the same call that re-adopts the site", got, "84° Clear")
	}
	if len(weather.asked) != 1 || weather.asked[0] != coords(theHanger.Lat, theHanger.Long) {
		t.Errorf("the source was asked at %v, want the adopted site's coordinates", weather.asked)
	}
}

func TestAdoptSiteToleratesNoGeoConsumers(t *testing.T) {
	// Several tests construct a rePuller for the serving side alone. A nil geo
	// must stay a supported construction rather than a panic waiting for the
	// first reconnect.
	p := &rePuller{driver: &scheduleDriver{}}
	p.adoptSite(theHanger)
	if p.driver.site != theHanger {
		t.Fatalf("the schedule driver's site = %+v, want the adopted binding", p.driver.site)
	}
}
