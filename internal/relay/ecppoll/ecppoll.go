// Package ecppoll is the real DeviceStateSource (internal/relay/automationhost's
// devicestate.go seam): it polls a Roku's ECP HTTP endpoints on an interval,
// derives a media-player state.Entity (rules/1 state.Entity, device-class-registry/1
// REG-060-066) per target, and feeds RUL-330 observations to whatever drives
// automationhost.Host.Run/Observe — on hardware, rather than the SyntheticSource
// the dev-stack demo and tests use today.
//
// # ECP snapshot and state derivation (REG-061/062)
//
// Each poll of a target fetches two ECP queries with a 3s timeout each:
// GET /query/active-app and GET /query/device-info, both plain XML (parsed via
// encoding/xml structs, never regex). A fetch/parse failure on EITHER endpoint —
// unreachable device, non-200 response, or unparsable body — derives the whole
// entity as State "unavailable" with every attribute nil/absent except
// is_screensaver, which is always false (never nil).
//
// Otherwise the six REG-064 attributes are read off the parsed XML:
//   - active_app: the active <app> or <screensaver> element's text (its Name),
//     nil if neither element is present.
//   - active_app_id: that element's id attribute, nil if neither is present.
//   - app_type: "screensaver" when a <screensaver> element is present;
//     otherwise the active <app>'s type attribute mapped onto the REG-064 enum —
//     "home"/"menu" pass through unchanged, any other non-empty value (e.g. ECP's
//     real-world "appl") collapses to "app", and an absent/empty type attribute
//     (as ECP reports for the Home screen's own <app> some firmware versions
//     return) maps to nil, since this attribute is a literal map from the ECP
//     type attr, not a heuristic. Nil when no <app> or <screensaver> element is
//     present at all.
//   - power_mode: device-info's raw <power-mode> text (e.g. "PowerOn", "Ready",
//     "DisplayOff"), nil if the element is absent or empty.
//   - is_screensaver: true iff a <screensaver> element is present. Never nil.
//   - app_version: the active <app>/<screensaver>'s version attribute, nil if
//     absent or empty.
//
// The canonical State string is derived, in order:
//  1. power-mode == "PowerOn" -> continue to steps 2-6 below. Any OTHER raw
//     power-mode value is classified by a WHITELIST, never a blacklist
//     (device-class-registry/1 REG-021): a small set of other raw values
//     this driver has actually observed ("Suspend", "Ready", "DisplayOff" —
//     a driver-distinguishable low-power condition short of "off", REG-061)
//     map to "standby"; every OTHER raw value — including an absent/empty
//     <power-mode>, or a value from a future firmware this package has
//     never seen — maps to the media-player class's own REG-062
//     unknown_state_fallback, "off", never to a permissive default. See
//     standbyPowerModes/unknownPowerModeState below.
//  2. a <screensaver> element is present -> "idle".
//  3. no <app> element is present either -> "idle" (a bare active-app response
//     with neither child is ECP's idle-at-home shape on some firmware).
//  4. the <app>'s type attribute is "home" or "menu" -> "idle".
//  5. the <app>'s id is the well-known Dynamic Menu id "562859", or its name is
//     the literal Home screen name "Roku" (ECP reports the Home screen's own
//     <app> with no type attribute on some firmware, so app_type alone cannot
//     distinguish it — this id/name pair is the only signal available) -> "idle".
//  6. otherwise -> "on".
//
// # First-observation semantics (C2, coordinator-ruled single-stream design)
//
// RUL-330's state.NewObservation classifies a TRANSITION between two real
// snapshots (prev, curr); there is no meaningful prev on an entity's very
// first poll. An earlier revision of this package withheld that first
// snapshot from the stream entirely (a Seeds() method the wiring had to
// drain separately before ever starting to feed Next() into the engine) —
// that design is GONE. Instead, an entity's first successful snapshot —
// "successful" meaning a poll cycle completed and produced an Entity value,
// which includes one derived as "unavailable" from a fetch failure, since
// fetchEntity always returns SOME Entity and never a bare Go error — emits a
// SELF-transition Observation, state.NewObservation(reg, seed, seed) with
// seed used as BOTH prev and curr, through the exact same channel Next()
// serves for every later Observation. Because prev==curr, StateChanged is
// always false and ChangedAttrs is always empty for this one Observation —
// it can never itself cause a trigger to fire.
//
// Feeding this Observation through automationhost.Host.Observe (the normal
// path Host.Run's src.Next() loop drives) still seeds the rules engine's own
// per-trigger baseline (RUL-300/304, internal/rules/eval/trigger_state.go's
// StateTrigger.Observe / eval.TriggerBaseline.Known): engine.Observe
// computes next.State/next.Attr/next.AttrKnown from the passed-in curr
// Entity UNCONDITIONALLY, on every call including a StateChanged=false one —
// so this self-transition Observation seeds State AND every attribute's
// baseline in one call. (A plain engine.SeedEntityState(entityID, state)
// call, by contrast, seeds TriggerBaseline.State only — an
// attribute-scoped trigger's AttrKnown would stay unset. This package's
// self-transition Observation has no such gap.)
//
// Run drains one target at a time in a fixed per-poll (sorted) order and
// observe()/send() execute synchronously inside Run's own single goroutine,
// so an entity's seed Observation is always sent to the channel strictly
// before that same entity's first real transition — per-entity ordering is
// inherent to this single stream, not something the wiring has to defend
// separately (the earlier Seeds()-based design forced exactly that race:
// "has every target's first poll completed yet?").
//
// Consumer wiring is therefore just: feed this Poller's Next() into
// automationhost.Host.Run (or call Host.Observe directly per Next() result).
// There is no separate seeding step, and no new Host method (e.g. a
// Host.SeedEntityState wrapper) is needed. From an entity's second
// successful snapshot on, observe() classifies prev->curr via
// state.NewObservation exactly as before and only sends when it reports
// StateChanged or a non-empty ChangedAttrs; a quiet subsequent tick
// (identical snapshot) still emits nothing.
//
// # Channel semantics
//
// Run polls every interval (plus one immediate poll at start) and sends each
// emitted Observation on a bounded internal channel (default size 16, see
// WithChannelSize) that Next receives from. If the channel is full when Run
// tries to send, the OLDEST buffered Observation is dropped (never the new
// one) and Dropped's counter increments — the poll loop itself never blocks on
// a slow consumer. The channel is closed once Run returns (ctx canceled or, in
// principle, the loop otherwise exiting), so Next continues to drain whatever
// was buffered and only then reports ok=false.
package ecppoll

import (
	"context"
	"encoding/xml"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// mediaPlayerClass is the one REG-060 device class this poller ever derives
// entities for.
const mediaPlayerClass = "media-player"

// defaultPort is the ECP port used when a Target's Port is 0.
const defaultPort = 8060

// fetchTimeout bounds each of the two ECP GETs a snapshot performs.
const fetchTimeout = 3 * time.Second

// defaultChanSize is Next's channel capacity absent WithChannelSize.
const defaultChanSize = 16

// homeScreenID and homeScreenName are the Home screen / Dynamic Menu's
// well-known ECP identity, consulted only when the active <app>'s type
// attribute is absent (state derivation step 5 in the package doc).
const (
	homeScreenID   = "562859"
	homeScreenName = "Roku"
)

// Target is one polled device's ECP address. Port 0 means the standard ECP
// port 8060.
type Target struct {
	Host string
	Port int
}

// addr renders t's http://host:port base URL, applying the default port.
// Uses net.JoinHostPort (M1) so an IPv6 literal host is correctly bracketed
// (e.g. "http://[::1]:8060") rather than producing a malformed authority.
func (t Target) addr() string {
	port := t.Port
	if port == 0 {
		port = defaultPort
	}
	return "http://" + net.JoinHostPort(t.Host, strconv.Itoa(port))
}

// Option configures a Poller at construction. See WithChannelSize,
// WithHTTPClient, and WithRegistry.
type Option func(*Poller)

// WithChannelSize overrides Next's channel capacity (default 16). Non-positive
// values are ignored.
func WithChannelSize(n int) Option {
	return func(p *Poller) {
		if n > 0 {
			p.chanSize = n
		}
	}
}

// WithHTTPClient overrides the *http.Client used for ECP requests (default:
// one with a 3s Timeout). A nil client is ignored.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Poller) {
		if c != nil {
			p.client = c
		}
	}
}

// WithRegistry overrides the registry.Registry consulted for change-emission
// classification (REG-044), default registry.FromDeviceClass(deviceclass.Builtin(),
// registry.Overlay{}) — the platform's one canonical media-player vocabulary,
// the same adapter automationhost.New wires into the engine. A nil registry is
// ignored.
func WithRegistry(reg registry.Registry) Option {
	return func(p *Poller) {
		if reg != nil {
			p.reg = reg
		}
	}
}

// Poller is a DeviceStateSource (internal/relay/automationhost/devicestate.go)
// that polls a fixed set of Roku ECP targets on an interval. See the package
// doc for state derivation and first-observation semantics.
type Poller struct {
	interval time.Duration
	client   *http.Client
	reg      registry.Registry
	chanSize int

	ch      chan state.Observation
	dropped atomic.Uint64

	mu      sync.Mutex
	targets map[string]Target       // the set polled, replaced by SetTargets
	prev    map[string]state.Entity // latest known snapshot per entity ID
}

// New builds a Poller over targets (entityID -> ECP address), polling every
// interval. It does not start polling; call Run to drive it.
func New(targets map[string]Target, interval time.Duration, opts ...Option) *Poller {
	cp := make(map[string]Target, len(targets))
	for id, t := range targets {
		cp[id] = t
	}

	p := &Poller{
		targets:  cp,
		interval: interval,
		client:   &http.Client{Timeout: fetchTimeout},
		reg:      registry.FromDeviceClass(deviceclass.Builtin(), registry.Overlay{}),
		chanSize: defaultChanSize,
		prev:     make(map[string]state.Entity, len(targets)),
	}
	for _, opt := range opts {
		opt(p)
	}
	p.ch = make(chan state.Observation, p.chanSize)
	return p
}

// Run drives the poll loop: one immediate poll of every target, then one more
// every interval, until ctx is done. Each poll's emitted Observations (RUL-330,
// see the package doc's first-observation section) are sent on the internal
// channel Next receives from. Run closes that channel before returning, so
// Next continues to drain whatever is buffered and only then reports
// ok=false — this is the one and only closer of the channel, so Run must never
// be called twice concurrently on the same Poller.
func (p *Poller) Run(ctx context.Context) {
	defer close(p.ch)

	p.pollAll(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollAll(ctx)
		}
	}
}

// Next returns the next emitted Observation, blocking until one is available.
// ok is false once Run has closed the channel and every buffered Observation
// has been drained.
func (p *Poller) Next() (state.Observation, bool) {
	obs, ok := <-p.ch
	return obs, ok
}

// Dropped is how many Observations have been discarded because the internal
// channel was full when Run tried to send (the oldest buffered one is always
// the one dropped, never the newest).
func (p *Poller) Dropped() uint64 {
	return p.dropped.Load()
}

// pollAll snapshots every target once, in a deterministic (sorted) order, and
// classifies/emits per entity via observe. ctx is checked between every
// target (I3): if it is already done, pollAll stops rather than racing
// through every remaining target — each of which would otherwise fail fast
// (ctx already canceled) and enqueue a spurious "unavailable" transition
// right at shutdown.
func (p *Poller) pollAll(ctx context.Context) {
	// Snapshot the target set once per cycle rather than reading it per entity:
	// SetTargets can land mid-cycle (the desired-state re-pull loop calls it),
	// and a cycle that half-used the old set and half the new would poll a
	// device that was just un-adopted while skipping one that was just adopted.
	p.mu.Lock()
	targets := make(map[string]Target, len(p.targets))
	for id, t := range p.targets {
		targets[id] = t
	}
	p.mu.Unlock()

	ids := make([]string, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return
		default:
		}
		curr := p.fetchEntity(ctx, id, targets[id])
		p.observe(id, curr)
	}
}

// SetTargets replaces the polled set (entityID → ECP address) while Run is
// live, so the devices whose state this relay reads track the devices it is
// allowed to command — one adoption gate, applied once, rather than a poll set
// and a command set that can drift apart.
//
// A target that DISAPPEARS also has its remembered snapshot dropped, which is
// what makes a re-adoption behave like a first sighting: the next poll emits a
// self-transition seed Observation (the package doc's first-observation
// contract) and re-seeds the engine's baselines, instead of classifying a
// transition against a snapshot from before the device left the set — which
// could be arbitrarily stale and would fire triggers on a change that happened
// while nobody was watching.
func (p *Poller) SetTargets(targets map[string]Target) {
	cp := make(map[string]Target, len(targets))
	for id, t := range targets {
		cp[id] = t
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targets = cp
	for id := range p.prev {
		if _, still := cp[id]; !still {
			delete(p.prev, id)
		}
	}
}

// Snapshot is the poller's latest derived state per polled entity — the answer
// to "what is this device doing right now?" without issuing a second round of
// ECP requests to ask.
//
// It is the read side of the polling this relay already performs: the derived
// state.Entity carries the media-player State (REG-061) plus every REG-064
// attribute the two ECP queries produced (active_app, active_app_id, app_type,
// power_mode, is_screensaver, app_version). The relay reports the State half
// upward on its `device.candidates` report (REL-110a's per-entity `state`,
// "present only once the relay has observed one"), which is how an operator's
// entity list shows what a screen is actually doing.
//
// An entity that has never completed a poll is absent — never present with a
// fabricated state. The returned map and its attribute maps are copies, so a
// caller cannot mutate the poller's own record.
func (p *Poller) Snapshot() map[string]state.Entity {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]state.Entity, len(p.prev))
	for id, e := range p.prev {
		attrs := make(map[string]any, len(e.Attributes))
		for k, v := range e.Attributes {
			attrs[k] = v
		}
		e.Attributes = attrs
		out[id] = e
	}
	return out
}

// observe folds one freshly-fetched snapshot into id's prior state and emits
// an Observation per the package doc's first-observation contract (C2): an
// entity's first successful snapshot emits a SELF-transition Observation
// (state.NewObservation(reg, curr, curr), so StateChanged is always false and
// ChangedAttrs is always empty) through the same channel every later
// Observation uses; from the second snapshot on, NewObservation classifies
// prev->curr and only a genuine change (StateChanged or a non-empty
// ChangedAttrs) is sent.
func (p *Poller) observe(id string, curr state.Entity) {
	p.mu.Lock()
	prev, existed := p.prev[id]
	p.prev[id] = curr
	p.mu.Unlock()

	if !existed {
		// First successful snapshot: emit a self-transition seed Observation
		// rather than withholding it (see the package doc's First-observation
		// semantics section) — it can never fire a trigger itself, but still
		// seeds the engine's per-entity/per-attribute baseline once fed
		// through Host.Observe.
		p.send(state.NewObservation(p.reg, curr, curr))
		return
	}
	obs := state.NewObservation(p.reg, prev, curr)
	if obs.StateChanged || len(obs.ChangedAttrs) > 0 {
		p.send(obs)
	}
}

// send delivers obs on the internal channel, dropping the OLDEST buffered
// Observation (never obs itself) when the channel is already full, and
// counting the drop in Dropped(). Run is this Poller's sole writer, so the
// only concurrency send.
func (p *Poller) send(obs state.Observation) {
	select {
	case p.ch <- obs:
		return
	default:
	}
	// Full: drop the oldest buffered entry to make room. A concurrent Next()
	// call may win the race and consume it first — in that case our own
	// receive below finds the channel already has room and nothing extra is
	// dropped, which is still correct (something was in fact removed to make
	// room for obs, just by the reader rather than by us).
	select {
	case <-p.ch:
		p.dropped.Add(1)
	default:
	}
	select {
	case p.ch <- obs:
	default:
		// Extremely rare: a concurrent sender is impossible (Run is the sole
		// writer) but a concurrent Next() could in principle refill first;
		// fail closed by dropping obs itself rather than blocking forever.
		p.dropped.Add(1)
	}
}

// fetchEntity snapshots one target's active-app and device-info ECP queries
// and derives its state.Entity (see the package doc). Any fetch or parse
// failure on either query derives the whole entity as State "unavailable"
// (REG-061/062) — fetchEntity itself never returns a bare error.
func (p *Poller) fetchEntity(ctx context.Context, id string, t Target) state.Entity {
	base := t.addr()

	var aa ecpActiveApp
	if err := p.getXML(ctx, base+"/query/active-app", &aa); err != nil {
		return unavailableEntity(id)
	}
	var di ecpDeviceInfo
	if err := p.getXML(ctx, base+"/query/device-info", &di); err != nil {
		return unavailableEntity(id)
	}
	return deriveEntity(id, aa, di)
}

// getXML performs one ECP GET and decodes its XML body into v.
func (p *Poller) getXML(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ecppoll: %s: unexpected status %d", url, resp.StatusCode)
	}
	return xml.NewDecoder(resp.Body).Decode(v)
}

// ecpAppElem is the shared shape of ECP active-app's <app> and <screensaver>
// child elements: an id and (for <app>) type attribute, an optional version
// attribute, and the element's text content as its display name.
type ecpAppElem struct {
	ID      string `xml:"id,attr"`
	Type    string `xml:"type,attr"`
	Version string `xml:"version,attr"`
	Name    string `xml:",chardata"`
}

// ecpActiveApp is GET /query/active-app's response shape: either an <app>
// child (the normal case) or a <screensaver> child (the screensaver-active
// variant), never both in practice, but decoded independently.
type ecpActiveApp struct {
	XMLName     xml.Name    `xml:"active-app"`
	App         *ecpAppElem `xml:"app"`
	Screensaver *ecpAppElem `xml:"screensaver"`
}

// ecpDeviceInfo is the one field of GET /query/device-info this package reads.
type ecpDeviceInfo struct {
	XMLName   xml.Name `xml:"device-info"`
	PowerMode string   `xml:"power-mode"`
}

// unavailableEntity is the State "unavailable" entity a fetch/parse failure
// derives: every attribute nil except is_screensaver, which is always false
// (REG-061/062, never nil per REG-064).
func unavailableEntity(id string) state.Entity {
	return state.Entity{
		ID:          id,
		DeviceClass: mediaPlayerClass,
		State:       "unavailable",
		Attributes: map[string]any{
			"active_app":     nil,
			"active_app_id":  nil,
			"app_type":       nil,
			"power_mode":     nil,
			"is_screensaver": false,
			"app_version":    nil,
		},
	}
}

// deriveEntity classifies a successfully-fetched snapshot into a state.Entity
// per the package doc's derivation and attribute rules (REG-061/062/064).
func deriveEntity(id string, aa ecpActiveApp, di ecpDeviceInfo) state.Entity {
	attrs := map[string]any{
		"active_app":     nil,
		"active_app_id":  nil,
		"app_type":       nil,
		"power_mode":     nil,
		"is_screensaver": false,
		"app_version":    nil,
	}

	if di.PowerMode != "" {
		attrs["power_mode"] = di.PowerMode
	}

	isScreensaver := aa.Screensaver != nil
	attrs["is_screensaver"] = isScreensaver

	var appType string // raw ECP type attr of the active <app>, "" = absent
	switch {
	case isScreensaver:
		attrs["app_type"] = "screensaver"
		attrs["active_app"] = aa.Screensaver.Name
		attrs["active_app_id"] = aa.Screensaver.ID
		if aa.Screensaver.Version != "" {
			attrs["app_version"] = aa.Screensaver.Version
		}
	case aa.App != nil:
		attrs["active_app"] = aa.App.Name
		attrs["active_app_id"] = aa.App.ID
		if aa.App.Version != "" {
			attrs["app_version"] = aa.App.Version
		}
		appType = aa.App.Type
		switch appType {
		case "home":
			attrs["app_type"] = "home"
		case "menu":
			attrs["app_type"] = "menu"
		case "":
			// Absent: app_type is a literal map from the ECP type attr, so it
			// stays nil even though state derivation still special-cases this
			// app below (step 5 in the package doc).
		default:
			attrs["app_type"] = "app"
		}
	}

	st := deriveState(di.PowerMode, isScreensaver, aa.App, appType)
	return state.Entity{ID: id, DeviceClass: mediaPlayerClass, State: st, Attributes: attrs}
}

// standbyPowerModes is the whitelist (I1, device-class-registry/1 REG-021)
// of recognized non-"PowerOn" raw <power-mode> values this driver has
// actually observed that classify to the "standby" state (REG-061: "a
// driver-distinguishable low-power condition short of off"). REG-021
// requires a classifier be implemented as a whitelist, never a blacklist
// that maps known-bad values away from a permissive default — so any raw
// value that is NEITHER "PowerOn" NOR a member of this whitelist (an
// absent/empty <power-mode>, or a future firmware string this package has
// never seen) falls through to unknownPowerModeState instead, never to
// "standby" and never to "on".
var standbyPowerModes = map[string]bool{
	"Suspend":    true,
	"Ready":      true,
	"DisplayOff": true,
}

// unknownPowerModeState is the media-player class's own REG-062
// unknown_state_fallback: what deriveState resolves an unrecognized raw
// power-mode value to (never "on" or "standby" — see standbyPowerModes).
const unknownPowerModeState = "off"

// deriveState applies the package doc's six-step State derivation.
func deriveState(powerMode string, isScreensaver bool, app *ecpAppElem, appType string) string {
	switch {
	case powerMode == "PowerOn":
		// Recognized as powered-on: fall through to the idle/on branches
		// below.
	case standbyPowerModes[powerMode]:
		return "standby"
	default:
		// I1/REG-021: whitelist, not a blacklist — an unrecognized raw value
		// (including absent/empty) resolves to the class's own REG-062
		// unknown_state_fallback, "off".
		return unknownPowerModeState
	}
	if isScreensaver {
		return "idle"
	}
	if app == nil {
		return "idle"
	}
	if appType == "home" || appType == "menu" {
		return "idle"
	}
	if app.ID == homeScreenID || app.Name == homeScreenName {
		return "idle"
	}
	return "on"
}
