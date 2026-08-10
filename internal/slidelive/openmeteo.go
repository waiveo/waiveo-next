package slidelive

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// openMeteoDefaultBaseURL is the public Open-Meteo forecast API.
//
// Open-Meteo is the source specifically because it needs NO API KEY and no
// account: a box that ships to a customer site must be able to draw the weather
// widget on the day it is plugged in, and any keyed provider would mean either a
// credential in the image (shared across every deployment, unrotatable) or an
// onboarding step between plugging a screen in and it working. The cost is that
// this depends on a free public service; the cache and the stale-serve below are
// what keep that dependency from being a screen outage.
const openMeteoDefaultBaseURL = "https://api.open-meteo.com"

// openMeteoDefaultTTL is how long a reading is served before a refresh is
// triggered. Ten minutes because the upstream model itself updates on the order
// of an hour and a wall-mounted screen showing a ten-minute-old temperature is
// indistinguishable, to a viewer, from a live one — while polling faster would
// multiply requests by the number of boxes for no visible gain.
const openMeteoDefaultTTL = 10 * time.Minute

// openMeteoFetchTimeout bounds one background fetch. It is generous relative to
// the TTL (a slow answer is still a useful answer) but finite, so a hung
// connection cannot pin the in-flight flag and wedge refreshes forever.
const openMeteoFetchTimeout = 15 * time.Second

// OpenMeteoConfig configures NewOpenMeteo. EVERY field is optional: the zero
// value is the production configuration, which is what cmd/waiveo-relay passes.
// The fields exist so a test can point at an httptest server, drive an explicit
// clock, and shorten the TTL — a plain struct rather than functional options
// precisely so an option nothing in the delivery path passes cannot accumulate
// (the shape scripts/validate-deadcode.mjs exists to catch).
type OpenMeteoConfig struct {
	// BaseURL replaces the public API root (no trailing slash).
	BaseURL string
	// Client replaces the HTTP client used for the background fetch.
	Client *http.Client
	// TTL replaces the freshness window a cached reading is served within.
	TTL time.Duration
	// Now replaces the clock the freshness window is measured against.
	Now func() time.Time
}

// OpenMeteo is a WeatherSource over the keyless Open-Meteo current-conditions
// API, and it is NON-BLOCKING by construction — which is the whole design, not a
// detail.
//
// Its Current runs on the goroutine issuing a player's Lease. A synchronous HTTP
// call there would put a third-party service directly in the path of every
// screen's program poll: a provider timing out would not degrade the weather
// widget, it would stall the poll, and with it the screen's whole program. So
// Current NEVER performs I/O. It answers from cache and, when that answer is
// stale or missing, kicks off ONE background refresh (a second concurrent poll
// for the same location joins the in-flight one rather than starting another).
//
// The consequences, both deliberate:
//
//   - The FIRST poll after boot for a location returns ok=false, so the widget
//     shows Unavailable for one poll interval. A player polls on the order of
//     seconds and the widget then fills in, which is a far better failure shape
//     than a boot that blocks.
//   - A refresh that FAILS leaves the previous reading in place and keeps
//     serving it past its TTL, retrying on each subsequent Current. A screen
//     therefore rides out a network blip showing a slightly old temperature
//     instead of blanking — the same "last known good beats nothing" rule the
//     player's own never-wipe reconnect behavior follows. There is no staleness
//     ceiling on that fallback today: an outage lasting days would keep showing
//     the last reading, which is the trade taken knowingly rather than blanking
//     a widget that is right far more often than not.
type OpenMeteo struct {
	baseURL string
	client  *http.Client
	ttl     time.Duration
	now     func() time.Time

	mu       sync.Mutex
	cache    map[string]openMeteoReading
	inflight map[string]bool
}

// openMeteoReading is one location's last successful reading and when it was
// taken. A location with no successful reading has no map entry at all, which is
// what distinguishes "never fetched" (show Unavailable) from "fetched, now
// stale" (keep showing it).
type openMeteoReading struct {
	conds Conditions
	at    time.Time
}

// NewOpenMeteo builds an OpenMeteo, filling every unset config field with its
// production default.
func NewOpenMeteo(cfg OpenMeteoConfig) *OpenMeteo {
	o := &OpenMeteo{
		baseURL:  cfg.BaseURL,
		client:   cfg.Client,
		ttl:      cfg.TTL,
		now:      cfg.Now,
		cache:    map[string]openMeteoReading{},
		inflight: map[string]bool{},
	}
	if o.baseURL == "" {
		o.baseURL = openMeteoDefaultBaseURL
	}
	if o.client == nil {
		o.client = &http.Client{Timeout: openMeteoFetchTimeout}
	}
	if o.ttl <= 0 {
		o.ttl = openMeteoDefaultTTL
	}
	if o.now == nil {
		o.now = time.Now
	}
	return o
}

// Current implements WeatherSource. It returns the cached reading for
// (lat, long) — serving a stale one rather than nothing — and schedules a
// background refresh when the cached reading is missing or older than the TTL.
//
// ctx is the CALLER's request context and is deliberately not used for the
// fetch: the refresh outlives this call by design, and inheriting a request
// context would cancel it the moment the player's poll returned, so no refresh
// would ever complete. The parameter stays because it is the WeatherSource
// contract and a source that CAN answer synchronously (a fixture, an in-process
// cache with a deadline) should be handed the caller's deadline.
func (o *OpenMeteo) Current(ctx context.Context, lat, long float64) (Conditions, bool) {
	_ = ctx
	key := openMeteoKey(lat, long)

	o.mu.Lock()
	reading, have := o.cache[key]
	stale := !have || o.now().Sub(reading.at) >= o.ttl
	start := stale && !o.inflight[key]
	if start {
		o.inflight[key] = true
	}
	o.mu.Unlock()

	if start {
		go o.refresh(key, lat, long)
	}
	return reading.conds, have
}

// refresh performs one background fetch for a location and stores the result,
// clearing the in-flight flag on every path (including a failed fetch) so a
// transient error cannot permanently block later refreshes.
func (o *OpenMeteo) refresh(key string, lat, long float64) {
	ctx, cancel := context.WithTimeout(context.Background(), openMeteoFetchTimeout)
	defer cancel()

	conds, err := o.fetch(ctx, lat, long)

	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.inflight, key)
	if err != nil {
		// Keep whatever reading is already cached — see the type's doc: a failed
		// refresh serves the previous value rather than blanking the widget.
		return
	}
	o.cache[key] = openMeteoReading{conds: conds, at: o.now()}
}

// openMeteoCurrentResponse is the slice of Open-Meteo's response this reads:
// `{"current":{"temperature_2m":21.3,"weather_code":3}}`. Every other member the
// API returns (elevation, generation time, units echo, the request's own
// coordinates) is ignored rather than modeled — decoding only what is consumed
// keeps a provider adding fields from being a parse change here.
type openMeteoCurrentResponse struct {
	Current struct {
		Temperature float64 `json:"temperature_2m"`
		WeatherCode int     `json:"weather_code"`
	} `json:"current"`
}

// fetch performs the actual HTTP request and maps the response into Conditions.
// Temperatures are requested in Celsius (the API default) and Fahrenheit is
// derived here, so both template tokens come from ONE reading and one rounding
// rule rather than from two requests that could straddle a model update.
func (o *OpenMeteo) fetch(ctx context.Context, lat, long float64) (Conditions, error) {
	q := url.Values{}
	q.Set("latitude", fmt.Sprintf("%g", lat))
	q.Set("longitude", fmt.Sprintf("%g", long))
	q.Set("current", "temperature_2m,weather_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/v1/forecast?"+q.Encode(), nil)
	if err != nil {
		return Conditions{}, err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return Conditions{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Conditions{}, fmt.Errorf("slidelive: open-meteo returned %d", resp.StatusCode)
	}

	var body openMeteoCurrentResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Conditions{}, fmt.Errorf("slidelive: decode open-meteo response: %w", err)
	}

	c := body.Current.Temperature
	return Conditions{
		TempC: int(math.Round(c)),
		TempF: int(math.Round(c*9/5 + 32)),
		Text:  wmoConditionText(body.Current.WeatherCode),
	}, nil
}

// openMeteoKey is the cache key for a location: coordinates rounded to three
// decimals, roughly 100 m. Rounding is what makes the key STABLE — two screens
// at the same site resolve identical coordinates today, but a float formatted
// at full precision would make a future per-screen geo a per-screen cache entry
// (and a per-screen upstream request) for locations that share the same weather.
func openMeteoKey(lat, long float64) string {
	return fmt.Sprintf("%.3f,%.3f", lat, long)
}

// wmoConditionTexts maps a WMO 4677 weather code — the vocabulary Open-Meteo
// reports conditions in — to the short human phrase a slide shows. The phrases
// are deliberately terse: this lands in a fixed-width widget beside a
// temperature, not in a forecast paragraph.
//
// Codes are grouped as the standard groups them (drizzle 51-57, rain 61-67,
// snow 71-77, showers 80-86, thunderstorm 95-99); a code outside the table is
// not an error, just unnamed — see wmoConditionText.
var wmoConditionTexts = map[int]string{
	0:  "Clear",
	1:  "Mainly clear",
	2:  "Partly cloudy",
	3:  "Overcast",
	45: "Fog",
	48: "Rime fog",
	51: "Light drizzle",
	53: "Drizzle",
	55: "Heavy drizzle",
	56: "Freezing drizzle",
	57: "Freezing drizzle",
	61: "Light rain",
	63: "Rain",
	65: "Heavy rain",
	66: "Freezing rain",
	67: "Freezing rain",
	71: "Light snow",
	73: "Snow",
	75: "Heavy snow",
	77: "Snow grains",
	80: "Light showers",
	81: "Showers",
	82: "Heavy showers",
	85: "Snow showers",
	86: "Snow showers",
	95: "Thunderstorm",
	96: "Thunderstorm",
	99: "Thunderstorm",
}

// wmoConditionText names a WMO weather code, or returns "" for a code this
// table does not carry. An empty string is NOT a defect to report: the caller
// (weatherValue) renders it as Unavailable in the condition's place while still
// showing the temperature, so an unrecognized code costs one token, not the
// widget. Returning a made-up phrase, or the raw numeric code, would put
// something meaningless on a wall instead.
func wmoConditionText(code int) string {
	return wmoConditionTexts[code]
}
