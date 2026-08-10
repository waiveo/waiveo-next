// Package slidelive resolves the SERVER-SIDE half of a native slide's live
// widgets: it fills wire.Layer.Value for the `weather` and `entity` layer kinds
// so the player draws a string it never has to compute.
//
// # Why the box resolves these and the player does not
//
// A slide's live widgets split cleanly by where their value comes from, and the
// split decides who computes them (wire's layer-kind doc states the same rule
// from the contract's side):
//
//   - `clock`, `date` and `countdown` are functions of the CURRENT TIME. A
//     player has a clock, so it computes them itself and re-computes them every
//     tick. Resolving them here would make them exactly as stale as the last
//     Lease — the freeze this whole native path exists to avoid.
//   - `weather` and `entity` are functions of data ONLY THE BOX HAS: an upstream
//     forecast service, and the device plane's observed entity state. A player
//     resolving those itself would need per-screen outbound internet, a
//     per-screen credential, and a second implementation of every unit and
//     format decision — on a device whose renderer is BrightScript.
//
// # Where resolution happens, and why there
//
// Exactly one call site: internal/relay/playerserver, as it issues a Lease
// (handleProgram). That is the freshest and the ONLY complete point in the
// pipeline, and both properties matter:
//
//   - Freshest: a Lease is minted per player poll, so a value resolved here is
//     at most one poll old. The two projections upstream — the app-signed
//     baseline (internal/feeder/snapshot) and the relay's daypart re-resolver
//     (internal/relay/schedulehost) — run on AUTHORING and SCHEDULE changes, not
//     on a clock. A weather value baked into the signed baseline would be frozen
//     at the instant the generation was built, which for a stable playlist is
//     indefinitely; it would also make the snapshot `hash` (REL-053) a function
//     of the weather, so every forecast change would churn a re-pull across the
//     fleet.
//   - Complete: BOTH projections converge on playerserver — the baseline path
//     through SetServedProgram, the re-resolved path through SetProgram — so
//     resolving here covers a screen whether or not a carried schedule governs
//     it. Resolving in the projections instead would need the identical logic in
//     two packages, which is the drift wire.ValidateSlideLayers is shared to
//     avoid.
//
// # Degrading
//
// Resolve NEVER fails and never drops a layer. An unavailable source yields
// Unavailable ("—") in the widget's place, because the alternative — refusing
// the layer, or refusing the slide — would take down the title, the image and
// the clock to report that a forecast service timed out. This is also why
// wire.ValidateSlideLayers does not require Value: see its doc.
package slidelive

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// Unavailable is the text a live widget shows when its source cannot answer —
// an em dash, the typographic convention for "no value here", and deliberately
// NOT an error string, a stale value with no indication it is stale, or an empty
// label that reads on a wall as a rendering bug.
//
// It is exported because it is the contract between this package and anything
// that asserts on a degraded slide (tests, and an operator reading a screen).
const Unavailable = "—"

// Conditions is the current weather at one location, in the units a
// WeatherSource has already normalized to — this package does no unit
// conversion of its own, so a source is free to answer from whatever the
// upstream service speaks as long as both temperatures are filled.
//
// Both TempF and TempC are carried rather than one plus a conversion because the
// template chooses (`{temp}` vs `{tempc}`) and a slide authored in one unit must
// not silently re-round through the other.
type Conditions struct {
	// TempF and TempC are the current temperature in whole degrees, already
	// rounded — a slide has no room for decimals and rounding at the source
	// keeps one rounding rule rather than one per template token.
	TempF int
	TempC int
	// Text is the human-readable condition ("Clear", "Light rain"), already
	// localized to the vocabulary the source speaks.
	Text string
}

// WeatherSource answers the current conditions at a location.
//
// Current MUST NOT BLOCK: its one production caller is on the goroutine serving
// a player's program poll, and a Lease that waits on a third-party HTTP request
// turns a forecast service's bad day into a stalled screen. An implementation
// that needs the network therefore answers from a cache and refreshes in the
// background — see OpenMeteo, whose doc states exactly that contract. ok=false
// means "no value to show yet", which the caller renders as Unavailable.
type WeatherSource interface {
	Current(ctx context.Context, lat, long float64) (Conditions, bool)
}

// EntitySource answers a platform entity's current canonical state string
// (rules/1's state.Entity.State — "on", "off", "playing", …), as most recently
// OBSERVED on the device plane. ok=false for an entity nothing has ever reported
// for, which is a genuinely different answer from a state of "" and is rendered
// as Unavailable rather than as a blank.
//
// Like Current it must not block; the production implementation
// (internal/relay/automationhost.Host) answers from the map it already maintains
// as observations flow through it.
type EntitySource interface {
	EntityState(entityID string) (string, bool)
}

// EntitySourceFunc adapts a bare lookup function to EntitySource, the
// http.HandlerFunc pattern.
//
// It exists for a specific and slightly unusual reason, recorded so it is not
// "simplified" away: the production entity source is the relay's automation host
// (*automationhost.Host), a large object with many unrelated methods. Handing
// that WHOLE VALUE to an interface field makes the type flow into an interface,
// and the repo's no-dead-code gate (scripts/validate-deadcode.mjs) uses rapid
// type analysis, which then has to treat every one of that type's methods as
// potentially called. The effect is that a dozen genuinely-unwired engine
// methods stop being reported as unwired — the gate's inventory quietly starts
// lying, in the direction that hides real gaps.
//
// Passing the one bound method instead keeps the coupling honest in both
// directions: this package depends on exactly the one function it uses, and the
// gate keeps seeing the rest of the host's surface for what it is.
type EntitySourceFunc func(entityID string) (string, bool)

// EntityState implements EntitySource.
func (f EntitySourceFunc) EntityState(entityID string) (string, bool) { return f(entityID) }

// Sources is the live data one relay resolves slides against: its two sources
// plus the site coordinates weather is asked for.
//
// The coordinates are relay-level, not per-screen, and that is a deliberate
// simplification with a real boundary: a relay is bound to ONE site by the app
// peer's authoritative `site_binding` (relay/1 REL-036), whose tz/lat/long ARE
// the site node's DAT-033 effective geo, already resolved by the app. Every
// screen this relay serves therefore sits under that site. What this does NOT
// honor is a DAT-032 per-group/per-screen geo OVERRIDE beneath the site: two
// screens in one building that declare different coordinates would both show the
// site's weather. Carrying a per-screen geo would mean threading the screen's
// scope node from the projection into the served program, which is a change to
// the program shape rather than to this package — noted here so the limitation
// is a recorded decision and not an accident.
//
// A zero Sources (both interfaces nil) is valid and resolves every live widget
// to Unavailable, which is what a relay with no configured weather and no device
// plane should show.
//
// # Coordinates a relay does not have
//
// Lat/Long are the ONE input here that can be silently, plausibly wrong, and the
// wrong value is the zero value. A relay boots into offline-serve (REL-055/061)
// whenever the app peer is unreachable — an app restart, a feeder outage, a WAN
// partition to the app alone — and adopts NO site_binding on that path, so the
// coordinates it would ask the weather at are (0, 0). Open-Meteo answers that
// point (the Gulf of Guinea) as happily as any other, so the widget would paint
// a confident, plausible, wrong temperature instead of the placeholder the whole
// degrade path exists for. Nobody notices a wrong temperature on a wall; that is
// exactly what makes it worse than a dash. See HasGeo.
type Sources struct {
	Weather WeatherSource
	Entity  EntitySource
	Lat     float64
	Long    float64
}

// HasGeo reports whether Lat/Long name a location this relay may actually ask
// the weather at — i.e. whether the site's DAT-033 effective geo has been
// adopted at all.
//
// Three things disqualify a coordinate pair, and each is a value that reaches
// here only because something upstream had no answer:
//
//   - (0, 0), the zero value of an unadopted hello.SiteBinding. It is also a
//     real point in the ocean, which is the trap: it is not rejected because it
//     is invalid, it is rejected because a site is never there and an unadopted
//     binding always is. A genuine Null Island deployment would see the
//     unavailable placeholder — a trade taken knowingly, and the only one of the
//     three with any cost at all.
//   - Out of range (|lat| > 90, |long| > 180): not a location, so no answer
//     about it can be right.
//   - NaN or ±Inf: the same, and they would additionally poison OpenMeteo's
//     cache key.
//
// It is exported because a deployment wants to SAY so at boot — a relay that
// will show dashes until its app peer comes up should log that once, rather than
// leave an operator inspecting a blank widget.
func (s Sources) HasGeo() bool {
	if math.IsNaN(s.Lat) || math.IsNaN(s.Long) || math.IsInf(s.Lat, 0) || math.IsInf(s.Long, 0) {
		return false
	}
	if s.Lat == 0 && s.Long == 0 {
		return false
	}
	return math.Abs(s.Lat) <= 90 && math.Abs(s.Long) <= 180
}

// weatherLookup returns the coordinates a weather lookup may be performed at,
// and whether one may be performed at all.
//
// It is the single place the two reasons NOT to ask — no source configured, and
// no location to ask about — are decided, so a caller cannot honor one and
// forget the other.
func (s Sources) weatherLookup() (lat, long float64, ok bool) {
	if s.Weather == nil || !s.HasGeo() {
		return 0, 0, false
	}
	return s.Lat, s.Long, true
}

// ResolveContent returns content with every `slide` item's `weather`/`entity`
// layers resolved — the entry point playerserver calls with the Lease content it
// is about to sign.
//
// It COPIES rather than mutating: the input slice's backing array is the served
// program's own state, held under the server's lock and reused for every
// subsequent poll, so writing a resolved value into it would permanently
// overwrite the authored layer with one poll's answer (and would race the next
// SetProgram). Only items that actually carry layers are copied; a plain
// image/video item is passed through by value as it always was, so the common
// case allocates nothing.
func ResolveContent(content []wire.LeaseContent, src Sources) []wire.LeaseContent {
	if len(content) == 0 {
		return content
	}
	out := make([]wire.LeaseContent, len(content))
	for i, item := range content {
		if len(item.Layers) > 0 {
			item.Layers = ResolveLayers(item.Layers, src)
		}
		out[i] = item
	}
	return out
}

// ResolveLayers returns a copy of layers with Value filled on every `weather`
// and `entity` layer, and every other kind passed through untouched.
//
// The one weather lookup a slide needs is performed AT MOST ONCE per call even
// when a slide carries several weather layers (a temperature and a condition
// drawn as two separately positioned widgets is the normal authoring), so a
// two-widget slide does not ask the source twice for the same instant and cannot
// draw two mutually inconsistent readings.
//
// It is exported alongside ResolveContent because a caller that holds bare
// layers (a conformance driver, a test asserting the resolution rules
// themselves) should not have to wrap them in a LeaseContent to reach the rules.
func ResolveLayers(layers []wire.Layer, src Sources) []wire.Layer {
	out := make([]wire.Layer, len(layers))
	copy(out, layers)

	var conds Conditions
	var condsOK, condsRead bool

	for i := range out {
		switch out[i].Kind {
		case wire.LayerKindWeather:
			if !condsRead {
				condsRead = true
				// A relay with no adopted coordinates asks NOTHING rather than
				// asking about (0,0): see Sources.HasGeo. condsOK stays false,
				// so the widget degrades to Unavailable exactly as it does for a
				// source that cannot answer — which is the honest report, since
				// this relay genuinely does not know where it is yet.
				if lat, long, ok := src.weatherLookup(); ok {
					conds, condsOK = src.Weather.Current(context.Background(), lat, long)
				}
			}
			out[i].Value = weatherValue(out[i].Text, conds, condsOK)
		case wire.LayerKindEntity:
			out[i].Value = entityValue(out[i].Text, out[i].EntityID, src.Entity)
		}
	}
	return out
}

// The template tokens a live widget's `text` is substituted through. They are
// braced words rather than printf verbs or bare `%s` for two reasons: a slide
// template is authored by a person in a UI, and a token that names its own
// meaning ("{temp}") survives being reordered, repeated or dropped, which a
// positional verb does not.
//
// The set is closed and small on purpose. Anything not in it is literal text —
// including a brace that does not open a known token — so an author who types
// "{tmp}" sees "{tmp}" on the wall rather than an empty widget, and can fix it.
const (
	tokenTemp  = "{temp}"  // temperature in whole °F
	tokenTempC = "{tempc}" // temperature in whole °C
	tokenCond  = "{cond}"  // human-readable condition text
	tokenState = "{state}" // an entity's canonical state string
)

// defaultEntityTemplate is what an `entity` layer with no authored template
// shows: just the state. wire.ValidateSlideLayers requires a template on
// `weather` (whose several fields have no single obvious default) but not on
// `entity`, whose ONLY value is its state — so "state and nothing else" is the
// answer a blank template can safely mean there.
const defaultEntityTemplate = tokenState

// weatherValue substitutes a weather template against conditions. An
// unavailable reading substitutes Unavailable into EVERY value token while
// leaving the author's literal text in place, so a template of
// "Now: {temp}°  {cond}" degrades to "Now: —°  —" — still recognizably the
// weather widget, still in its authored position, visibly without data. The
// alternative (replacing the whole widget with a single dash) throws away the
// author's layout to say the same thing.
func weatherValue(template string, c Conditions, ok bool) string {
	tempF, tempC, cond := Unavailable, Unavailable, Unavailable
	if ok {
		tempF = strconv.Itoa(c.TempF)
		tempC = strconv.Itoa(c.TempC)
		if c.Text != "" {
			cond = c.Text
		}
	}
	r := strings.NewReplacer(tokenTemp, tempF, tokenTempC, tempC, tokenCond, cond)
	return r.Replace(template)
}

// entityValue substitutes an entity template against the entity's current
// observed state. A nil source, an unknown entity, and an entity observed with
// an empty state all resolve to Unavailable: from a wall's point of view they
// are the same fact ("this widget has nothing to tell you"), and distinguishing
// them on-screen would be reporting the box's internals to a viewer.
func entityValue(template, entityID string, src EntitySource) string {
	if template == "" {
		template = defaultEntityTemplate
	}
	value := Unavailable
	if src != nil {
		if st, ok := src.EntityState(entityID); ok && st != "" {
			value = st
		}
	}
	return strings.ReplaceAll(template, tokenState, value)
}
