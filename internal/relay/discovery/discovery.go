// Package discovery is the relay's SSDP CONTROL-POINT: it periodically
// M-SEARCHes and monitors NOTIFY alive messages for a configured set of
// manifest/1 MAN-071 discovery-match patterns, and feeds every exact
// search-target-string hit into the device plane's candidate Store as a
// ProvenanceDiscovered candidate (contracts/relay-1.md REL-110/111).
//
// A candidate this package Observes is a MATCH-PATTERN hit — the pattern
// that matched, not a resolved device identity (REL-110's frozen Candidate
// shape carries only the Match, provenance, lifecycle status, and
// first/last-seen marks).
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
// cares about: just the ST (search-target) it responded under, MAN-071's
// "search-target string". Decoupled from go-ssdp's own ssdp.Service so unit
// tests can inject fakes without touching multicast.
type foundService struct {
	ST string
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
		found = append(found, foundService{ST: svc.Type})
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
// (notification-type, MAN-071's search-target string) to onAlive as it
// arrives. The default (defaultMonitorFactory) is real go-ssdp; tests
// inject a fake factory.
type monitorFactory func(onAlive func(nt string)) ssdpMonitor

// defaultMonitorFactory is the real go-ssdp-backed monitorFactory
// (REL-110/111): an *ssdp.Monitor whose Alive handler reports the NOTIFY's
// NT to onAlive.
func defaultMonitorFactory(onAlive func(nt string)) ssdpMonitor {
	return &ssdp.Monitor{
		Alive: func(m *ssdp.AliveMessage) {
			onAlive(m.Type)
		},
	}
}

// Config configures a Discoverer (REL-110/111).
type Config struct {
	// Patterns is the manifest/1 MAN-071 match set to watch for. Only
	// entries with SSDP set are used; MDNS/MacOui entries are ignored here
	// — mDNS discovery is a separate, later lane, and MacOui has no SSDP
	// analogue. Multiple patterns sharing the same SSDP string collapse to
	// one sweep target.
	Patterns []deviceplane.Match

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
	byST       map[string]deviceplane.Match
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

	byST := make(map[string]deviceplane.Match)
	for _, p := range cfg.Patterns {
		if p.SSDP == "" {
			continue
		}
		byST[p.SSDP] = p
	}
	if len(byST) == 0 {
		return nil, errors.New("discovery: Config.Patterns has no usable SSDP pattern")
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
	mon := d.newMonitor(func(nt string) {
		if ctx.Err() != nil {
			// Run has already been asked to stop (or has returned): a NOTIFY
			// from go-ssdp's own detached per-packet goroutine — which can
			// outlive Monitor.Close() — must never reach the Store this
			// late.
			return
		}
		d.observeAlive(nt)
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
	for st, m := range d.byST {
		select {
		case <-ctx.Done():
			return
		default:
		}
		d.searchPattern(ctx, st, m)
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
func (d *Discoverer) searchPattern(ctx context.Context, st string, m deviceplane.Match) {
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
			d.store.Observe(m, deviceplane.ProvenanceDiscovered, d.nowMillis())
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
func (d *Discoverer) observeAlive(nt string) {
	m, ok := d.byST[nt]
	if !ok {
		return
	}
	d.store.Observe(m, deviceplane.ProvenanceDiscovered, d.nowMillis())
}
