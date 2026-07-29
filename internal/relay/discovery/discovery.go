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
	"time"

	ssdp "github.com/koron/go-ssdp"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
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
)

// foundService is the minimal shape of an SSDP search response discovery
// cares about: the ST (search-target) it responded under, MAN-071's
// "search-target string", and the USN the responder identified itself by —
// SSDP's own Unique Service Name, which is the stable per-device identifier
// this lane reports as REL-110a's `native_id`. Decoupled from go-ssdp's own
// ssdp.Service so unit tests can inject fakes without touching multicast.
type foundService struct {
	ST  string
	USN string
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
		found = append(found, foundService{ST: svc.Type, USN: svc.USN})
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

// monitorFactory builds an ssdpMonitor that reports every alive NOTIFY's NT
// (notification-type, MAN-071's search-target string) and USN (the announcing
// device's own identity, REL-110a's `native_id`) to onAlive as it arrives. The
// default (defaultMonitorFactory) is real go-ssdp; tests inject a fake factory.
type monitorFactory func(onAlive func(nt, usn string)) ssdpMonitor

// defaultMonitorFactory is the real go-ssdp-backed monitorFactory
// (REL-110/111): an *ssdp.Monitor whose Alive handler reports the NOTIFY's
// NT and USN to onAlive.
func defaultMonitorFactory(onAlive func(nt, usn string)) ssdpMonitor {
	return &ssdp.Monitor{
		Alive: func(m *ssdp.AliveMessage) {
			onAlive(m.Type, m.USN)
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
type Watch struct {
	Match       deviceplane.Match
	Driver      string
	DeviceClass string
	Entities    []deviceplane.CandidateEntity
}

// observation builds the Observation one responder with this USN is reported
// as: the watch's declared facts plus the responder's own identity.
func (w Watch) observation(usn string) deviceplane.Observation {
	return deviceplane.Observation{
		Match:       w.Match,
		Provenance:  deviceplane.ProvenanceDiscovered,
		Driver:      w.Driver,
		NativeID:    usn,
		DeviceClass: w.DeviceClass,
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
}

// Discoverer periodically SSDP M-SEARCHes a configured set of manifest/1
// MAN-071 patterns and monitors NOTIFY alive messages between sweeps,
// Observing every exact search-target-string match into a
// deviceplane.Store as a ProvenanceDiscovered candidate (REL-110/111). See
// the package doc for the CONTROL-POINT/RESPONDER split.
type Discoverer struct {
	byST       map[string]Watch
	store      *deviceplane.Store
	nowMillis  func() int64
	interval   time.Duration
	searchWait time.Duration

	search     searchFn
	newMonitor monitorFactory
}

// New returns a Discoverer for cfg. It errors if cfg.Store or cfg.NowMillis
// is nil, or if cfg.Patterns contains no usable (SSDP-set) pattern.
func New(cfg Config) (*Discoverer, error) {
	if cfg.Store == nil {
		return nil, errors.New("discovery: Config.Store must not be nil")
	}
	if cfg.NowMillis == nil {
		return nil, errors.New("discovery: Config.NowMillis must not be nil")
	}

	byST := make(map[string]Watch)
	for _, w := range cfg.Watches {
		if w.Match.SSDP == "" {
			continue
		}
		byST[w.Match.SSDP] = w
	}
	if len(byST) == 0 {
		return nil, errors.New("discovery: Config.Watches has no usable SSDP watch")
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

	return &Discoverer{
		byST:       byST,
		store:      cfg.Store,
		nowMillis:  cfg.NowMillis,
		interval:   interval,
		searchWait: searchWait,
		search:     defaultSearch,
		newMonitor: defaultMonitorFactory,
	}, nil
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
	mon := d.newMonitor(func(nt, usn string) {
		if ctx.Err() != nil {
			// Run has already been asked to stop (or has returned): a NOTIFY
			// from go-ssdp's own detached per-packet goroutine — which can
			// outlive Monitor.Close() — must never reach the Store this
			// late.
			return
		}
		d.observeAlive(nt, usn)
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
	for st, w := range d.byST {
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
			d.store.Observe(w.observation(f.USN), d.nowMillis())
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// observeAlive is the alive-monitor callback (REL-110/111): an alive NOTIFY
// whose NT exactly matches a configured pattern (MAN-071's search-target
// string) is Observed into the Store; any other NT is ignored. Pure lookup
// logic only — Run wraps this in a ctx.Err() guard (see Run's doc) before
// wiring it to the real monitor, so this method itself stays directly
// testable without a context.
func (d *Discoverer) observeAlive(nt, usn string) {
	w, ok := d.byST[nt]
	if !ok || usn == "" {
		return
	}
	d.store.Observe(w.observation(usn), d.nowMillis())
}
