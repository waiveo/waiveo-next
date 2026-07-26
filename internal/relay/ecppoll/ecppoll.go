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
//  1. power-mode != "PowerOn" (including absent/empty) -> "standby".
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
// # First-observation semantics — investigated conclusion
//
// RUL-330's state.NewObservation classifies a TRANSITION between two real
// snapshots (prev, curr); there is no meaningful prev on an entity's very first
// poll. Two INDEPENDENT first-observation mechanisms exist in this stack, and
// this package is responsible for exactly one of them:
//
//  1. This package's own seam (RUL-330): Poller withholds emitting any
//     Observation for an entity until it has TWO real snapshots. The first
//     successful poll of an entity — "successful" meaning a poll cycle
//     completed and produced an Entity value, which includes one derived as
//     "unavailable" from a fetch failure, since fetchEntity always returns
//     SOME Entity and never a bare Go error — is stored as that entity's prev
//     and surfaced via Seeds(), never handed to state.NewObservation. From the
//     second poll of that entity onward, NewObservation classifies prev->curr
//     and Poller emits only when it reports StateChanged or a non-empty
//     ChangedAttrs; a quiet tick (identical snapshot) emits nothing.
//
//  2. The rules engine's OWN, SEPARATE first-observation suppression
//     (RUL-300/304, investigated in internal/rules/engine/engine.go's
//     SeedEntityState and internal/rules/eval/trigger_state.go's
//     StateTrigger.Observe): every triggerRuntime carries its own durable
//     eval.TriggerBaseline, independent of anything this package computes.
//     StateTrigger.Observe's own doc is explicit: "First-ever observation (no
//     durable prior): never a transition, even if the value equals to:... Record
//     and do not fire." That guard fires on baseline.Known, which starts false
//     for every trigger and is set true ONLY by a real Observe call or by
//     engine.SeedEntityState(entityID, state string) — which seeds the STATE
//     baseline only, not attributes (eval.TriggerBaseline.Attr/AttrKnown are
//     left zero; there is no attribute-seeding entry point today).
//
//     Consequence: if the wiring starts feeding this Poller's Next() into
//     automationhost.Host.Run/Observe WITHOUT seeding the engine first, the very
//     first Observation this package ever emits for an entity (which already
//     represents a genuine prev->curr transition, by construction #1 above) is
//     STILL the engine's own first-ever Observe call for that entity, and every
//     state trigger on it is unconditionally suppressed by RUL-300/304 — a real,
//     already-classified transition would silently fail to fire anything.
//
//     Investigated existing seam: internal/relay/automationhost has NO exposed
//     seeding wrapper today. engine.SeedEntityState is called from exactly one
//     place in the whole tree (internal/app/api/automations.go), against a
//     throwaway *engine.Engine built for one synchronous test-run of a single
//     automation — entirely decoupled from automationhost.Host, whose `engine`
//     field is unexported and which exposes no Host.SeedEntityState method (unlike
//     SetLocation/SetClockTrust, which DO forward to engine methods under Host's mu).
//
//     Conclusion / required wiring contract: before the binary starts feeding a
//     Poller's Next() into Host.Run, it must walk poller.Seeds() and seed each
//     entity's State into the engine — e.g. a small Host.SeedEntityState(entityID,
//     state string) forwarding wrapper (mirroring SetLocation/SetClockTrust's
//     mu-guarded pattern in host.go) called once per Seeds() entry — so the
//     engine's own per-trigger baseline is Known=true before the FIRST real
//     Observation this package ever emits reaches it. That wrapper is NOT part of
//     this package (it touches automationhost, out of this lane's scope) but IS a
//     precondition for correct wiring; recorded here per this package's own
//     house-rule requirement to document the seam it was built against. A residual
//     gap this investigation surfaces but does not close: engine.SeedEntityState
//     seeds State only, so an attribute-scoped trigger's baseline (AttrKnown) is
//     still unset after seeding, and StateTrigger.Observe's unbounded
//     attribute-scoped case (RUL-023 case 2) fires unconditionally the first time
//     AttrKnown is false — a pre-existing engine-level nuance orthogonal to this
//     package, left to whoever wires attribute-scoped edge rules against this
//     source.
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
	"net/http"
	"sort"
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
func (t Target) addr() string {
	port := t.Port
	if port == 0 {
		port = defaultPort
	}
	return fmt.Sprintf("http://%s:%d", t.Host, port)
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
	targets  map[string]Target
	interval time.Duration
	client   *http.Client
	reg      registry.Registry
	chanSize int

	ch      chan state.Observation
	dropped atomic.Uint64

	mu   sync.Mutex
	prev map[string]state.Entity // latest known snapshot per entity ID
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

// Seeds returns the latest known snapshot of every target that has completed
// at least one poll, sorted by entity ID. See the package doc's
// first-observation section for the wiring contract this exists to support:
// the caller must seed the engine's own baseline from these BEFORE feeding
// this Poller's Next() into it, or the engine's first real Observe call per
// entity is unconditionally suppressed by its own RUL-300/304 mechanism.
func (p *Poller) Seeds() []state.Entity {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.prev))
	for id := range p.prev {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]state.Entity, 0, len(ids))
	for _, id := range ids {
		out = append(out, p.prev[id])
	}
	return out
}

// pollAll snapshots every target once, in a deterministic (sorted) order, and
// classifies/emits per entity via observe.
func (p *Poller) pollAll(ctx context.Context) {
	ids := make([]string, 0, len(p.targets))
	for id := range p.targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		curr := p.fetchEntity(ctx, id, p.targets[id])
		p.observe(id, curr)
	}
}

// observe folds one freshly-fetched snapshot into id's prior state and emits
// an Observation per the package doc's first-observation contract: a first
// snapshot is stored and never classified; from the second on, NewObservation
// classifies prev->curr and only a genuine change (StateChanged or a
// non-empty ChangedAttrs) is sent.
func (p *Poller) observe(id string, curr state.Entity) {
	p.mu.Lock()
	prev, existed := p.prev[id]
	p.prev[id] = curr
	p.mu.Unlock()

	if !existed {
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

// deriveState applies the package doc's six-step State derivation.
func deriveState(powerMode string, isScreensaver bool, app *ecpAppElem, appType string) string {
	if powerMode != "PowerOn" {
		return "standby"
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
