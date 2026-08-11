package main

import (
	"log"
	"sync"

	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/slidelive"
)

// siteGeo re-points every LIVE consumer of the site's effective geo at the
// site_binding this relay currently holds.
//
// # What "effective geo" is, and who consumes it
//
// A relay is bound to ONE site, and the app peer states that binding
// authoritatively in every hello-ack (relay/1 REL-036): scope node, tz, lat,
// long — the site scope node's DAT-033 effective geo, already resolved down the
// scope tree by the app. Three things on the box are functions of it:
//
//   - the automation engine's location, which decides when `sunset` is (rules/1
//     schedule + sun triggers);
//   - the coordinates a native slide's `weather` widget is resolved at
//     (internal/slidelive);
//   - the schedule resolvers' site (rePuller.driver.site), which adoptSite
//     already handled on its own.
//
// # Why this is a type rather than two lines at boot
//
// The first two used to be installed exactly once, from the value `site` held
// at boot, and never again. That is wrong in the one case the relay is BUILT to
// survive: an offline boot (REL-055/061) adopts no site_binding at all, so both
// were installed from the zero hello.SiteBinding — tz "" and coordinates
// (0, 0) — and stayed there for the life of the process, even after the app
// peer came back and handed this relay its real site seconds later. The weather
// half of that is the worst shape a defect can take on a wall: Open-Meteo
// answers about (0, 0) as readily as anywhere, so a screen painted a confident,
// plausible temperature for the Gulf of Guinea and no one could tell by looking.
//
// So adoption is a THING that happens, repeatedly, rather than a value copied at
// startup: every fresh hello-ack calls adopt (through rePuller.adoptSite, the
// one seam the reconnect supervisor already drives with each new binding), and
// every consumer is re-pointed together from that one place. A consumer added
// later is added here, and inherits re-adoption for free.
type siteGeo struct {
	// setSlideLive installs the live-widget sources — playerserver.Server's
	// own SetSlideLive, which takes the server lock and is safe to re-call.
	setSlideLive func(slidelive.Sources)
	// setLocation adopts tz/lat/long into the automation engine
	// (automationhost.Host.SetLocation). Optional: a relay with no automation
	// stack has nothing to adopt into.
	//
	// Both are bound METHOD VALUES rather than the objects behind an interface,
	// for the reason slidelive.EntitySourceFunc records: handing a large type
	// to an interface field makes every one of its methods look potentially
	// called to scripts/validate-deadcode.mjs's type analysis, and the gate's
	// inventory quietly stops reporting genuinely unwired ones.
	setLocation func(tz string, lat, long float64) error

	// weather and entity are the sources themselves — identical across every
	// adoption, since only the LOCATION changes. Carrying them here is what
	// lets adopt rebuild a whole slidelive.Sources: the Sources value is
	// immutable by design (installed wholesale under the server's lock), so
	// changing the coordinates means installing a new one.
	weather slidelive.WeatherSource
	entity  slidelive.EntitySource

	mu      sync.Mutex
	site    hello.SiteBinding
	adopted bool
}

// adopt installs site's geo into every consumer. It is idempotent — an
// unchanged binding (every reconnect to the same site, which is nearly all of
// them) does no work and logs nothing — and safe on a nil receiver, so a
// construction with no live-widget surface at all can leave the field nil.
//
// A failure to adopt the timezone is logged, never fatal: this runs on the
// reconnect path, where the alternative to a stale location is no relay.
func (g *siteGeo) adopt(site hello.SiteBinding) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.adopted && g.site == site {
		return
	}
	g.site, g.adopted = site, true

	src := slidelive.Sources{Weather: g.weather, Entity: g.entity, Lat: site.Lat, Long: site.Long}
	if g.setSlideLive != nil {
		g.setSlideLive(src)
	}
	if g.setLocation != nil {
		if err := g.setLocation(site.TZ, site.Lat, site.Long); err != nil {
			log.Printf("waiveo-relay: adopt site_binding tz %q into the automation engine failed (%v); the engine keeps its previous location, so sun triggers stay where they were", site.TZ, err)
		}
	}

	// Say it out loud in the one state an operator would otherwise diagnose by
	// staring at a widget: no coordinates means every weather widget on this
	// site shows a dash, and it will keep doing so until an app peer answers.
	if !src.HasGeo() {
		log.Printf("waiveo-relay: no site coordinates adopted (site %q, lat %v, long %v) — weather widgets show the unavailable placeholder until the app peer's site_binding (REL-036) supplies the site's effective geo", site.ScopeNode, site.Lat, site.Long)
		return
	}
	log.Printf("waiveo-relay: site geo adopted (site %q, tz %q, lat %v, long %v) — weather widgets resolve at the site's own coordinates", site.ScopeNode, site.TZ, site.Lat, site.Long)
}
