// Package discovery is the relay's SSDP CONTROL-POINT: it periodically
// M-SEARCHes and monitors NOTIFY alive messages for a configured set of
// manifest/1 MAN-071 discovery-match patterns, and feeds every exact
// search-target-string hit into the device plane's candidate Store as a
// ProvenanceDiscovered candidate (contracts/relay-1.md REL-110/111).
//
// A candidate this package Observes is one DEVICE (REL-110a): the responding
// device's own SSDP USN is its `native_id`, and the Watch that matched supplies
// the `driver`, device class, and entity fan-out that identity is completed by.
// A response carrying no USN is DROPPED rather than observed under a synthetic
// identity — two devices answering one search target would otherwise collapse
// into a single candidate, and neither could then be listed or addressed
// (REL-111a, REL-153).
//
// Discoverer plays the SSDP CONTROL-POINT role only: this box searching for
// and listening to other devices on the LAN. The SSDP RESPONDER role — so
// screens or other controllers can find this box — is a separate
// deliverable, not this package.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	ssdp "github.com/koron/go-ssdp"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/lanaddr"
)

// Default sweep timing (Config.Interval / Config.SearchWait zero values).
const (
	defaultInterval   = 60 * time.Second
	defaultSearchWait = 3 * time.Second

	// minSearchWait is the floor a positive Config.SearchWait is clamped to
	// (M5). go-ssdp's Search/MX API only accepts whole seconds — sweep's own
	// waitSec computation truncates via integer division by time.Second — so
	// a sub-second SearchWait would otherwise silently truncate to a
	// 0-second wait and collect zero responses every sweep, with no error or
	// visible degradation.
	minSearchWait = 1 * time.Second

	// identifyTTL is how long a SUCCESSFUL identification is reused before the
	// device is probed again. A model and a serial never change; a
	// self-reported name does (an owner renames the TV), so the probe is not
	// one-shot — but it is also not per-sighting: with a 60s sweep and a LAN
	// that emits NOTIFYs freely, probing every sighting would put a steady HTTP
	// load on every device on the network to re-learn three fields that almost
	// never move.
	identifyTTL = 15 * time.Minute

	// identifyRetryAfter is the same interval for a FAILED probe. Much shorter
	// than identifyTTL because a failure is far more likely to be transient
	// than a success is to be stale: a device mid-reboot, a DHCP lease being
	// renewed, one dropped packet. Caching the failure at all is what stops a
	// host that answers the search target but speaks no ECP — a non-Roku, or a
	// spoofer — from drawing a probe on every single sighting.
	identifyRetryAfter = 2 * time.Minute

	// maxCachedIdentities bounds the identity cache, which is keyed by USN —
	// a string chosen by whoever sent the packet, on a lane nothing
	// authenticates. Unbounded, one LAN host emitting NOTIFYs with a fresh USN
	// each time grows this map forever AND draws one outbound HTTP probe per
	// new USN, because identityOf runs BEFORE Store.Observe and so is not
	// covered by the candidate store's own cap.
	//
	// The value matches deviceplane's maxStoredCandidates deliberately: a USN
	// maps to a candidate, so a cache larger than the store it feeds could only
	// ever hold entries for devices the store has already refused, and a
	// smaller one would starve real devices the store is still willing to hold.
	// The admission RULE matches it too — see admitLocked.
	maxCachedIdentities = 1024
)

// Identity is what an identification probe learned about the device at a
// discovered address: the name it calls itself, what model it is, and the
// serial of the physical unit. Any field may be empty — a probe that ANSWERED
// has already established the device speaks the watch's driver, which is the
// classification; the fields are the enrichment.
type Identity struct {
	Name   string
	Model  string
	Serial string
}

// IdentifyFunc probes a discovered address over the driver's own protocol and
// reports what the device says about itself, or ok=false when the address did
// not answer as that kind of device at all.
//
// It is a SEAM rather than a direct call into a driver package for two reasons.
// The concrete probe is protocol-specific (Roku ECP's `/query/device-info`,
// internal/relay/ecp) and this package is the generic SSDP control point, so a
// direct dependency would point the generic lane at one vendor. And a probe is
// live network I/O against an untrusted LAN host: a test that could not replace
// it would either have to stand up a fake device or skip the enrichment path
// entirely.
//
// ctx bounds the probe. The implementation MUST honor it — a sweep that is
// canceled mid-probe must not be held open by a wedged host.
type IdentifyFunc func(ctx context.Context, address string) (Identity, bool)

// foundService is the minimal shape of an SSDP search response discovery
// cares about: the ST (search-target) it responded under, MAN-071's
// "search-target string", the USN the responder identified itself by —
// SSDP's own Unique Service Name, which is the stable per-device identifier
// this lane reports as REL-110a's `native_id` — and the LOCATION URL it
// published, which is the only part of a search response that says WHERE the
// device is. Decoupled from go-ssdp's own ssdp.Service so unit tests can inject
// fakes without touching multicast.
type foundService struct {
	ST       string
	USN      string
	Location string
}

// searchFn performs one SSDP M-SEARCH round for searchType, waiting waitSec
// seconds for responses, and reports every response received — unfiltered;
// sweep itself applies MAN-071's exact-string match. The default
// (defaultSearch) is real go-ssdp; tests inject a fake.
type searchFn func(searchType string, waitSec int) ([]foundService, error)

// defaultSearch is the real go-ssdp-backed searchFn (REL-110/111): a plain
// ssdp.Search on searchType across all interfaces (localAddr "").
func defaultSearch(searchType string, waitSec int) ([]foundService, error) {
	services, err := ssdp.Search(searchType, waitSec, "")
	if err != nil {
		return nil, err
	}
	found := make([]foundService, 0, len(services))
	for _, svc := range services {
		found = append(found, foundService{ST: svc.Type, USN: svc.USN, Location: svc.Location})
	}
	return found, nil
}

// ssdpMonitor is the lifecycle subset of *ssdp.Monitor Run drives: Start
// begins listening for NOTIFY messages (invoking whatever alive callback
// the monitorFactory wired in), Close stops it. Decoupled to an interface
// so unit tests inject a fake monitor that never touches multicast.
type ssdpMonitor interface {
	Start() error
	Close() error
}

// aliveNotice is one alive NOTIFY, reduced to what this lane reports from:
// the NT (notification-type, MAN-071's search-target string), the announcing
// device's own USN (REL-110a's `native_id`), and the two independent statements
// of where it is — the LOCATION URL the device published, and the source
// address of the packet itself.
//
// From is carried because a NOTIFY, unlike a search response, HAS an observed
// sender: go-ssdp surfaces it, and it is the one address on the message the
// device did not choose. It is the fallback when LOCATION is absent or
// malformed (address.go explains why it is a fallback and not the primary).
type aliveNotice struct {
	NT       string
	USN      string
	Location string
	From     net.Addr
}

// monitorFactory builds an ssdpMonitor that reports every alive NOTIFY to
// onAlive as it arrives. The default (defaultMonitorFactory) is real go-ssdp;
// tests inject a fake factory.
type monitorFactory func(onAlive func(aliveNotice)) ssdpMonitor

// defaultMonitorFactory is the real go-ssdp-backed monitorFactory
// (REL-110/111): an *ssdp.Monitor whose Alive handler reports the NOTIFY's
// NT, USN, LOCATION and observed source address to onAlive.
func defaultMonitorFactory(onAlive func(aliveNotice)) ssdpMonitor {
	return &ssdp.Monitor{
		Alive: func(m *ssdp.AliveMessage) {
			onAlive(aliveNotice{NT: m.Type, USN: m.USN, Location: m.Location, From: m.From})
		},
	}
}

// Watch is one declared discovery watch: a manifest/1 MAN-071 match pattern
// plus the facts a device found by it is reported under (REL-110a).
//
// Driver names the adapter that speaks to such a device and is half of
// REL-153's identity tuple; DeviceClass names the vocabulary its commands
// resolve against (device-class-registry/1 REG-052); Entities are the
// addressable handles the driver exposes for one device of that class.
//
// None of these is discoverable from an SSDP response — they come from the
// declaration that asked for the search (a pack's device contribution,
// manifest/1 MAN-070), which is exactly what a match pattern already is. The
// response supplies only the USN that identifies WHICH device answered.
// DefaultPort is the port this driver's control surface listens on, used to
// complete an address when a response's LOCATION is absent or unusable and only
// the packet's source IP is available. It is declaration-side for the same
// reason Driver and DeviceClass are — "Roku ECP is on 8060" is knowledge the
// pack contributing the watch has, not something a UDP packet states — and a
// watch that leaves it zero simply gets no fallback address rather than a
// guessed port (address.go).
type Watch struct {
	Match       deviceplane.Match
	Driver      string
	DeviceClass string
	DefaultPort int
	Entities    []deviceplane.CandidateEntity
}

// observation builds the Observation one responder with this USN is reported
// as: the watch's declared facts, the responder's own identity, where it can be
// reached, and whatever an identification probe established about it.
func (w Watch) observation(usn, address string, id Identity) deviceplane.Observation {
	return deviceplane.Observation{
		Match:       w.Match,
		Provenance:  deviceplane.ProvenanceDiscovered,
		Driver:      w.Driver,
		NativeID:    usn,
		DeviceClass: w.DeviceClass,
		Name:        id.Name,
		Address:     address,
		Model:       id.Model,
		Serial:      id.Serial,
		Entities:    w.Entities,
	}
}

// Config configures a Discoverer (REL-110/111).
type Config struct {
	// Watches is the set of declared discovery watches this lane sweeps for:
	// each pairs a manifest/1 MAN-071 match with the driver, device class and
	// entity fan-out a device matching it is reported under (REL-110a). Only
	// watches whose Match sets SSDP are used; MDNS/MacOui entries are ignored
	// here — mDNS discovery is its own lane, and MacOui has no SSDP analogue.
	// Multiple watches sharing one SSDP string collapse to one sweep target.
	Watches []Watch

	// Store is the candidate store every matched pattern is Observe'd into
	// (REL-110/111). Required.
	Store *deviceplane.Store

	// NowMillis is the Timestamp-ms clock Observe calls are stamped with.
	// Required — inject a fake in tests rather than reading the wall clock.
	NowMillis func() int64

	// Interval is the sweep period. Zero or negative defaults to 60s.
	Interval time.Duration

	// SearchWait is the per-sweep M-SEARCH wait (the SSDP MX value). Zero
	// or negative defaults to 3s.
	SearchWait time.Duration

	// Identify, when set, probes each newly discovered address over the
	// driver's own protocol and enriches the candidate with what the device
	// says about itself (name/model/serial). Optional: left nil, candidates
	// still carry their address — discovery works, it is simply less
	// informative, which is the honest degradation for a deployment that wires
	// no prober.
	//
	// Results are cached per device (identifyTTL / identifyRetryAfter), so a
	// device is probed on first sight and then rarely, not on every sighting.
	Identify IdentifyFunc

	// ResolveMAC resolves a sighting's IP to the MAC the relay's neighbour table
	// saw it at, so an SSDP responder the neighbour lane ALSO found becomes ONE
	// candidate under the canonical MAC identity (spec §4.1) rather than a second
	// row. Optional: left nil, a sighting keeps its watch identity — correct for
	// a deployment with no neighbour lane, and the honest degradation (it just
	// cannot merge). A miss (cross-subnet host the table cannot see) also keeps
	// the watch identity.
	ResolveMAC func(ip string) (string, bool)
}

// Discoverer periodically SSDP M-SEARCHes a configured set of manifest/1
// MAN-071 patterns and monitors NOTIFY alive messages between sweeps,
// Observing every exact search-target-string match into a
// deviceplane.Store as a ProvenanceDiscovered candidate (REL-110/111). See
// the package doc for the CONTROL-POINT/RESPONDER split.
type Discoverer struct {
	// watches holds the CURRENT watch set as an atomically-swapped map keyed
	// by search-target string. A pointer swap rather than a mutex because the
	// readers are hot and lock-free today: the sweep loop, go-ssdp's
	// per-packet NOTIFY goroutines, and SetWatches (driven by every signed
	// desired-state apply, REL-064) all touch it concurrently, and each reader
	// wants one coherent snapshot of the whole set, never a mid-update view.
	watches    atomic.Pointer[map[string]Watch]
	store      *deviceplane.Store
	nowMillis  func() int64
	interval   time.Duration
	searchWait time.Duration

	search     searchFn
	newMonitor monitorFactory
	identify   IdentifyFunc
	resolveMAC func(ip string) (string, bool)

	// identities caches one probe result per device (keyed by USN), so a device
	// is probed on first sight and then only after its entry ages out. Guarded
	// because both lanes reach it concurrently: the sweep runs each pattern's
	// search in its own goroutine, and go-ssdp delivers NOTIFYs from per-packet
	// goroutines of its own. Bounded at maxCachedIdentities, because the key is
	// attacker-chosen — see that constant and admitLocked.
	idMu       sync.Mutex
	identities map[string]identityEntry
}

// identityEntry is one cached probe result: what was learned, the address it
// was learned at, when, and whether the probe answered at all. The address is
// part of the entry because a device that moved (DHCP) must be re-probed
// immediately rather than serving the old unit's facts under a new address.
type identityEntry struct {
	address string
	atMs    int64
	ok      bool
	id      Identity
}

// New returns a Discoverer for cfg. It errors if cfg.Store or cfg.NowMillis
// is nil. An EMPTY (or SSDP-free) initial watch set is legal, not an error:
// since watches follow the signed desired state (REL-064, SetWatches), a lane
// that starts before the first pack pattern arrives sweeps nothing and then
// picks the patterns up on the first inventory apply — refusing to construct
// would make "no packs installed yet" a boot failure.
func New(cfg Config) (*Discoverer, error) {
	if cfg.Store == nil {
		return nil, errors.New("discovery: Config.Store must not be nil")
	}
	if cfg.NowMillis == nil {
		return nil, errors.New("discovery: Config.NowMillis must not be nil")
	}

	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	searchWait := cfg.SearchWait
	switch {
	case searchWait <= 0:
		searchWait = defaultSearchWait
	case searchWait < minSearchWait:
		// M5: floor rather than silently degrading to a 0-second wait.
		searchWait = minSearchWait
	}

	d := &Discoverer{
		store:      cfg.Store,
		nowMillis:  cfg.NowMillis,
		interval:   interval,
		searchWait: searchWait,
		resolveMAC: cfg.ResolveMAC,
		search:     defaultSearch,
		newMonitor: defaultMonitorFactory,
		identify:   cfg.Identify,
		identities: make(map[string]identityEntry),
	}
	d.SetWatches(cfg.Watches)
	return d, nil
}

// SetWatches REPLACES the whole watch set — the REL-064 join: every signed
// desired-state apply derives the current pack-declared patterns into watches
// and installs them here, so installing or removing a pack changes what this
// lane sweeps for at the next generation, no restart. A full replace rather
// than a diff because the patterns section is itself a full set (REL-064) and
// a replace cannot leak a watch whose pack is gone.
//
// Only SSDP-form watches are usable on this lane (mDNS is its own lane;
// MacOui has no SSDP analogue); others are skipped. Returns how many watches
// are now live, so the caller can log the applied set honestly.
func (d *Discoverer) SetWatches(ws []Watch) int {
	byST := make(map[string]Watch)
	for _, w := range ws {
		if w.Match.SSDP == "" {
			continue
		}
		byST[w.Match.SSDP] = w
	}
	d.watches.Store(&byST)
	return len(byST)
}

// watchSet returns the current watch map — one coherent snapshot; callers
// must not mutate it.
func (d *Discoverer) watchSet() map[string]Watch {
	return *d.watches.Load()
}

// WatchCount reports how many watches are currently live — the far side of
// SetWatches, for callers (and tests) that need to observe what a swap
// actually installed rather than trusting the return value they passed along.
func (d *Discoverer) WatchCount() int {
	return len(d.watchSet())
}

// Run drives the sweep loop and alive monitor until ctx is canceled
// (REL-110/111): it starts the NOTIFY alive monitor once, sweeps
// immediately, then sweeps again every Interval. The monitor is always
// closed before Run returns. Run returns ctx.Err() once ctx is done, or an
// error starting the alive monitor.
//
// Cancellation is honored PROMPTLY even mid-sweep (C1): the production
// searchFn (defaultSearch) is go-ssdp's ssdp.Search, which takes no
// context.Context at all and blocks for the entire SearchWait regardless of
// ctx — so sweep runs each pattern's search in its own goroutine via
// searchPattern and races it against ctx.Done() rather than waiting on it
// synchronously. An abandoned search keeps running in the background (it
// cannot be killed) but its eventual result is discarded, never Observed,
// once ctx is done — searchPattern and the alive-monitor callback below both
// re-check ctx.Err() immediately before every Store.Observe call, so no
// Observe can land on the Store once Run has returned to its caller
// (Important, stray-goroutine finding: go-ssdp's own per-packet NOTIFY
// goroutines can briefly outlive Monitor.Close(), and a blocked Search
// goroutine can outlive Run itself).
func (d *Discoverer) Run(ctx context.Context) error {
	mon := d.newMonitor(func(n aliveNotice) {
		if ctx.Err() != nil {
			// Run has already been asked to stop (or has returned): a NOTIFY
			// from go-ssdp's own detached per-packet goroutine — which can
			// outlive Monitor.Close() — must never reach the Store this
			// late.
			return
		}
		d.observeAlive(ctx, n)
	})
	if err := mon.Start(); err != nil {
		return fmt.Errorf("discovery: starting alive monitor: %w", err)
	}
	defer mon.Close()

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	d.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			d.sweep(ctx)
		}
	}
}

// sweep runs one SSDP M-SEARCH round: for every distinct configured pattern
// string, it searches (via searchPattern) and Observes every response whose
// ST exactly matches that pattern (MAN-071's search-target string) into the
// Store (REL-110/111). Repeated hits — within a sweep or across sweeps —
// dedup via the Store's own Match.Key() dedup, bumping last_seen rather than
// accumulating. A response with a mismatched ST is ignored; a per-pattern
// search error is also ignored so one bad/unreachable pattern never stops
// the sweep from trying the rest.
//
// ctx is checked between every pattern (C1): if it is already done, sweep
// returns immediately rather than searching every remaining pattern in this
// round, each at the already-doomed full SearchWait cost.
func (d *Discoverer) sweep(ctx context.Context) {
	for st, w := range d.watchSet() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		d.searchPattern(ctx, st, w)
	}
}

// searchPattern runs one pattern's M-SEARCH in its own goroutine and races it
// against ctx.Done() (C1): the production searchFn (defaultSearch) has no
// context.Context parameter and blocks for the entire SearchWait regardless
// of cancellation, so this is the only way sweep/Run can return promptly
// when ctx is canceled during a real search.
//
// If ctx is canceled before the search goroutine finishes, searchPattern
// returns immediately without waiting for or acting on the eventual result.
// The goroutine keeps running to completion in the background (it cannot be
// interrupted), but its result-handling closure re-checks ctx.Err()
// immediately before every Store.Observe call, so a late result is
// discarded rather than landing on the Store after Run has already returned
// to its caller.
func (d *Discoverer) searchPattern(ctx context.Context, st string, w Watch) {
	waitSec := int(d.searchWait / time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		found, err := d.search(st, waitSec)
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		for _, f := range found {
			if f.ST != st {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			// A response with no USN identifies no device: Observe would drop
			// it anyway (REL-110a), and skipping here says so at the source.
			if f.USN == "" {
				continue
			}
			// A search response has no observed sender go-ssdp exposes, so
			// LOCATION is the only address available on this lane.
			addr, _ := addressFromLocation(f.Location)
			d.store.Observe(d.canonicalize(w.observation(f.USN, addr, d.identityOf(ctx, f.USN, addr))), d.nowMillis())
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// observeAlive is the alive-monitor callback (REL-110/111): an alive NOTIFY
// whose NT exactly matches a configured pattern (MAN-071's search-target
// string) is Observed into the Store; any other NT is ignored. Run wraps this
// in a ctx.Err() guard (see Run's doc) before wiring it to the real monitor.
//
// The address is taken from LOCATION when it parses and from the packet's own
// source otherwise, paired with the watch's declared control port — the ONLY
// lane where that fallback is available, since a search response exposes no
// sender (address.go).
func (d *Discoverer) observeAlive(ctx context.Context, n aliveNotice) {
	w, ok := d.watchSet()[n.NT]
	if !ok || n.USN == "" {
		return
	}
	addr, ok := addressFromLocation(n.Location)
	if !ok {
		addr, _ = addressFromSource(n.From, w.DefaultPort)
	}
	d.store.Observe(d.canonicalize(w.observation(n.USN, addr, d.identityOf(ctx, n.USN, addr))), d.nowMillis())
}

// canonicalize re-keys a sighting under the canonical MAC identity when the
// relay's neighbour table knows the device's address — the merge that keeps one
// physical device to one candidate (spec §4.1). The service facts, the probed
// name/model/serial, the class and entities the watch supplied all ride along
// unchanged; only the IDENTITY and its Match move to the MAC, so the SSDP
// sighting lands on the SAME store key the neighbour lane minted rather than a
// second row. A device the neighbour table cannot resolve (cross-subnet), or a
// deployment with no resolver wired, keeps the watch identity — the honest
// degradation, not a dropped sighting.
func (d *Discoverer) canonicalize(o deviceplane.Observation) deviceplane.Observation {
	if d.resolveMAC == nil || o.Address == "" {
		return o
	}
	host, _, ok := lanaddr.Split(o.Address)
	if !ok {
		return o
	}
	mac, ok := d.resolveMAC(host)
	if !ok {
		return o
	}
	driver, nativeID, match, ok := deviceplane.MACIdentity(mac)
	if !ok {
		return o
	}
	o.Driver, o.NativeID, o.Match = driver, nativeID, match
	return o
}

// identityOf returns what is known about the device with this USN at this
// address, probing only when nothing usable is cached.
//
// Freshness is decided per outcome: a success is reused for identifyTTL, a
// failure only for identifyRetryAfter, and a CHANGED address invalidates either
// immediately — a device that moved may not be the same physical unit, and
// serving the old one's serial under the new address would misidentify it in an
// operator's adoption list.
//
// A probe that fails yields the zero Identity, and the caller Observes with it:
// blank learned fields never overwrite ones a previous sighting established
// (deviceplane.Store.Observe), so a transient failure costs nothing already
// known. With no Identify wired at all this is a pure zero-value return and no
// cache entry is written — there is no outcome to remember.
func (d *Discoverer) identityOf(ctx context.Context, usn, address string) Identity {
	if d.identify == nil || address == "" {
		return Identity{}
	}
	now := d.nowMillis()

	d.idMu.Lock()
	cached, hit := d.identities[usn]
	d.idMu.Unlock()
	if hit && cached.address == address {
		ttl := identifyRetryAfter
		if cached.ok {
			ttl = identifyTTL
		}
		if now-cached.atMs < ttl.Milliseconds() {
			return cached.id
		}
	}

	// Nothing usable is cached, so this USN is about to cost an outbound HTTP
	// request. Ask the cap first: a probe and a cache entry are the same
	// admission decision, and gating only the WRITE would leave the request
	// itself unbounded, which is the half that reaches the network.
	d.idMu.Lock()
	admitted := d.admitLocked(usn, now)
	d.idMu.Unlock()
	if !admitted {
		return Identity{}
	}

	// Probed OUTSIDE the lock on purpose: this is network I/O against a device
	// that may be wedged, and holding the cache mutex across it would stall
	// every other lane's lookup — including sightings of devices that are
	// perfectly healthy. Two goroutines racing to probe one device is a
	// duplicate request, which is cheap; a serialized cache is not.
	id, ok := d.identify(ctx, address)
	if !ok {
		id = Identity{}
	}

	// Re-asked rather than assumed: the cache was unlocked across the probe, so
	// another lane may have filled the last slot meanwhile. Refusing to record
	// the result costs one re-probe later; recording it past the cap is the
	// unbounded growth this is here to stop.
	d.idMu.Lock()
	if d.admitLocked(usn, now) {
		d.identities[usn] = identityEntry{address: address, atMs: now, ok: ok, id: id}
	}
	d.idMu.Unlock()
	return id
}

// admitLocked reports whether this USN may occupy a cache slot at nowMs,
// evicting entries that have nothing left to offer to make room. The caller
// holds idMu.
//
// The rule is deviceplane.Store.Observe's, for the same reason: when the cache
// is full, a USN already in it is always admitted and a NEW one is REFUSED
// rather than displacing someone. Evicting the oldest would let a flood of
// fresh USNs push out the real devices found first — turning a bounded cache
// into an unbounded probe rate against the actual TVs, which is worse than the
// growth it was meant to stop.
//
// The eviction it does perform costs nothing: an entry past its own freshness
// window (identifyTTL for a success, identifyRetryAfter for a failure) would be
// re-probed on its next sighting anyway, so dropping it discards no knowledge
// and is what lets a relay that once saw a flood recover as those entries age
// out, instead of staying full forever.
func (d *Discoverer) admitLocked(usn string, nowMs int64) bool {
	if _, known := d.identities[usn]; known {
		return true
	}
	if len(d.identities) < maxCachedIdentities {
		return true
	}
	for key, e := range d.identities {
		window := identifyRetryAfter
		if e.ok {
			window = identifyTTL
		}
		if nowMs-e.atMs >= window.Milliseconds() {
			delete(d.identities, key)
		}
	}
	return len(d.identities) < maxCachedIdentities
}
