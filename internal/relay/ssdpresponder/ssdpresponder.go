// Package ssdpresponder is the relay's SSDP RESPONDER role (contracts/
// player-1.md PLY-021/022/023): it advertises this relay's player/1 pairing
// surface on the LAN, answering a player's SSDP M-SEARCH for the search
// target SearchTarget with a response carrying this relay's own resolvable
// /player/v1 base URL (PLY-001, PLY-021), and re-announcing immediately
// whenever that base URL changes (PLY-022) so a player relying on discovery
// is never left holding a stale address with no signal that it changed.
//
// This is the OTHER SSDP role from internal/relay/discovery, which plays the
// CONTROL-POINT side instead — this box searching for and listening to other
// devices on the LAN (REL-110/111). Discovery reach is not a normative
// property either role can guarantee (PLY-023, context only): pairing-code
// and manual-entry address acquisition (playerserver.FormPairingCode,
// PLY-024/025) exist as first-class paths independent of this package, not a
// fallback exercised only when discovery fails.
package ssdpresponder

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	ssdp "github.com/koron/go-ssdp"
)

// SearchTarget is the player/1 SSDP search target this package answers
// M-SEARCH requests for and advertises alive notifications under (PLY-021).
const SearchTarget = "urn:waiveo:service:player:1"

// defaultMaxAge is Config.MaxAge's zero-value default (CACHE-CONTROL
// max-age, in seconds once converted): matches go-ssdp's own advertise
// example default of 1800s (30 minutes).
const defaultMaxAge = 1800 * time.Second

// minAliveInterval floors the periodic ssdp:alive re-announce cadence Run
// derives from Config.MaxAge (see aliveInterval) so a very small configured
// MaxAge cannot turn Run's background loop into an Alive()-send busy-loop —
// mirroring discovery.go's own minSearchWait floor (M5).
const minAliveInterval = 30 * time.Second

// Config configures a Responder (PLY-021/022).
type Config struct {
	// BaseURL is the resolvable /player/v1 base a discovery response
	// carries (PLY-001, PLY-021), e.g.
	// "https://192.168.50.12:7421/player/v1". Must be a non-empty, absolute
	// (scheme+host) URL. Required.
	BaseURL string

	// USN is the unique service name go-ssdp sends as every response's and
	// alive notification's USN header. Callers pass something
	// relay-identity-derived, e.g. "uuid:waiveo-relay:<relayID>", so a
	// player can tell which relay answered. Required.
	USN string

	// MaxAge is the CACHE-CONTROL max-age basis a response/alive
	// notification carries; Run's own periodic re-announce interval is
	// derived from it (half of MaxAge, floored at minAliveInterval). Zero
	// or negative defaults to 1800s (30 minutes).
	MaxAge time.Duration
}

// advertiser is the lifecycle subset of *ssdp.Advertiser Run drives: Alive
// sends one ssdp:alive re-announcement (PLY-022), Bye sends ssdp:byebye, and
// Close stops the underlying M-SEARCH-answering listener. Decoupled to an
// interface — the same seam shape as discovery.go's ssdpMonitor — so unit
// tests inject a fake that never touches multicast.
type advertiser interface {
	Alive() error
	Bye() error
	Close() error
}

// advertiseFn starts a (real, or in tests fake) advertiser that answers
// M-SEARCH for st/usn with location, carrying server and a maxAgeSec
// CACHE-CONTROL in every response and alive/bye announcement. The default
// (defaultAdvertise) is real go-ssdp; tests inject a fake constructor so
// New/Run never touch multicast.
type advertiseFn func(st, usn string, location ssdp.LocationProvider, server string, maxAgeSec int) (advertiser, error)

// defaultAdvertise is the real go-ssdp-backed advertiseFn (PLY-021):
// ssdp.Advertise binds a multicast listener and starts answering M-SEARCH
// for st under usn/location immediately, on construction — before Run ever
// calls Alive. *ssdp.Advertiser satisfies this package's advertiser
// interface directly (Alive/Bye/Close), so no adapter is needed.
func defaultAdvertise(st, usn string, location ssdp.LocationProvider, server string, maxAgeSec int) (advertiser, error) {
	return ssdp.Advertise(st, usn, location, server, maxAgeSec)
}

// dynamicLocation is a ssdp.LocationProvider whose URL can be swapped live
// (PLY-022). go-ssdp's Advertiser calls Location fresh on every M-SEARCH
// response and Alive announcement it builds, so once installed at Advertise
// time, a later UpdateBaseURL swap is visible to every subsequent
// response/announcement without recreating the Advertiser — which would
// drop its bound listener and reset the M-SEARCH responder mid-flight.
type dynamicLocation struct {
	mu  sync.RWMutex
	url string
}

func (l *dynamicLocation) Location(net.Addr, *net.Interface) string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.url
}

func (l *dynamicLocation) set(u string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.url = u
}

// Responder is the relay's SSDP RESPONDER (PLY-021/022): once Run is
// started it answers a player's M-SEARCH for SearchTarget with a response
// carrying Config.BaseURL, and periodically re-announces ssdp:alive until
// ctx is done. See the package doc for the CONTROL-POINT/RESPONDER split.
type Responder struct {
	usn        string
	maxAgeSec  int
	aliveEvery time.Duration
	loc        *dynamicLocation

	advertise advertiseFn

	mu  sync.Mutex
	adv advertiser // set only while Run is active; nil before Run starts and after it returns
}

// New returns a Responder for cfg. It errors if cfg.BaseURL does not parse
// as a non-empty absolute URL, or cfg.USN is empty.
func New(cfg Config) (*Responder, error) {
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	if cfg.USN == "" {
		return nil, errors.New("ssdpresponder: Config.USN must not be empty")
	}

	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = defaultMaxAge
	}

	return &Responder{
		usn:        cfg.USN,
		maxAgeSec:  int(maxAge / time.Second),
		aliveEvery: aliveInterval(maxAge),
		loc:        &dynamicLocation{url: cfg.BaseURL},
		advertise:  defaultAdvertise,
	}, nil
}

// validateBaseURL requires a non-empty, absolute (scheme+host) URL — the
// minimum shape PLY-021 needs for "a resolvable base URL for its own
// /player/v1 path".
func validateBaseURL(raw string) error {
	if raw == "" {
		return errors.New("ssdpresponder: Config.BaseURL must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("ssdpresponder: Config.BaseURL %q does not parse as a URL: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("ssdpresponder: Config.BaseURL %q is not an absolute URL (needs a scheme and host)", raw)
	}
	return nil
}

// aliveInterval derives Run's periodic ssdp:alive re-announce cadence from
// maxAge: half of maxAge, so a cache built from one announcement's max-age
// never expires before the next re-announcement under ordinary operation,
// floored at minAliveInterval.
func aliveInterval(maxAge time.Duration) time.Duration {
	interval := maxAge / 2
	if interval < minAliveInterval {
		return minAliveInterval
	}
	return interval
}

// Run starts advertising SearchTarget — go-ssdp's Advertiser begins
// answering M-SEARCH the instant r.advertise constructs it (PLY-021), before
// this method sends its first Alive — sends one immediate ssdp:alive
// announcement, then keeps re-announcing on a background ticker until ctx
// is done, at which point it sends ssdp:byebye, closes the advertiser, and
// returns ctx.Err(). No announce is ever sent after Run returns (prompt
// ctx-cancel semantics: a goroutine + select, mirroring
// discovery.Discoverer.Run).
//
// An error starting the advertiser is returned immediately, before anything
// is advertised. A failed periodic Alive or the final Bye is otherwise
// ignored (this package has no logger to report it to): best-effort
// re-announcement never stops Run, since the underlying M-SEARCH responder
// — PLY-021's actual mechanism — keeps answering regardless of whether a
// given Alive/Bye send succeeded.
func (r *Responder) Run(ctx context.Context) error {
	adv, err := r.advertise(SearchTarget, r.usn, r.loc, "", r.maxAgeSec)
	if err != nil {
		return fmt.Errorf("ssdpresponder: starting advertiser: %w", err)
	}

	r.mu.Lock()
	r.adv = adv
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.adv = nil
		r.mu.Unlock()
	}()

	_ = adv.Alive() // best-effort initial announce; the M-SEARCH responder above already answers regardless of this succeeding

	ticker := time.NewTicker(r.aliveEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = adv.Bye()
			_ = adv.Close()
			return ctx.Err()
		case <-ticker.C:
			_ = adv.Alive()
		}
	}
}

// UpdateBaseURL swaps the base URL this responder's M-SEARCH responses and
// alive announcements carry and, if Run is currently active, immediately
// sends one ssdp:alive re-announcement under the new address (PLY-022) —
// so a player relying on discovery is never left holding a stale address
// with no signal that it changed. Called before Run has started, or after
// it has already returned, it only records the new address (r.adv is nil in
// both cases): the swap takes effect on Run's own next announce, if any,
// rather than sending an announce with nothing listening to answer for it.
//
// It errors if u does not parse as a non-empty absolute URL, leaving the
// previously configured BaseURL in effect unchanged (no partial swap).
func (r *Responder) UpdateBaseURL(u string) error {
	if err := validateBaseURL(u); err != nil {
		return err
	}
	r.loc.set(u)

	r.mu.Lock()
	adv := r.adv
	r.mu.Unlock()
	if adv == nil {
		return nil
	}
	return adv.Alive()
}
