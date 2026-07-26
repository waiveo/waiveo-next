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
	if searchWait <= 0 {
		searchWait = defaultSearchWait
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
func (d *Discoverer) Run(ctx context.Context) error {
	mon := d.newMonitor(d.observeAlive)
	if err := mon.Start(); err != nil {
		return fmt.Errorf("discovery: starting alive monitor: %w", err)
	}
	defer mon.Close()

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	d.sweep()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			d.sweep()
		}
	}
}

// sweep runs one SSDP M-SEARCH round: for every distinct configured pattern
// string, it searches and Observes every response whose ST exactly matches
// that pattern (MAN-071's search-target string) into the Store
// (REL-110/111). Repeated hits — within a sweep or across sweeps — dedup
// via the Store's own Match.Key() dedup, bumping last_seen rather than
// accumulating. A response with a mismatched ST is ignored; a per-pattern
// search error is also ignored so one bad/unreachable pattern never stops
// the sweep from trying the rest.
func (d *Discoverer) sweep() {
	waitSec := int(d.searchWait / time.Second)
	for st, m := range d.byST {
		found, err := d.search(st, waitSec)
		if err != nil {
			continue
		}
		for _, f := range found {
			if f.ST == st {
				d.store.Observe(m, deviceplane.ProvenanceDiscovered, d.nowMillis())
			}
		}
	}
}

// observeAlive is the alive-monitor callback (REL-110/111): an alive NOTIFY
// whose NT exactly matches a configured pattern (MAN-071's search-target
// string) is Observed into the Store; any other NT is ignored.
func (d *Discoverer) observeAlive(nt string) {
	m, ok := d.byST[nt]
	if !ok {
		return
	}
	d.store.Observe(m, deviceplane.ProvenanceDiscovered, d.nowMillis())
}
