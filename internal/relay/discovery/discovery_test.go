package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
)

// mustMatch parses raw into a deviceplane.Match via ParseMatch (match.go's
// documented construction path), failing the test on a parse error.
func mustMatch(t *testing.T, raw string) deviceplane.Match {
	t.Helper()
	m, err := deviceplane.ParseMatch(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("parse match %s: %v", raw, err)
	}
	return m
}

// fakeMonitor is an ssdpMonitor test double that never touches multicast:
// it just records whether Start/Close were called.
type fakeMonitor struct {
	started bool
	closed  bool
}

func (f *fakeMonitor) Start() error { f.started = true; return nil }
func (f *fakeMonitor) Close() error { f.closed = true; return nil }

// watchFor wraps a bare match in the declaration-side facts a Watch carries
// (REL-110a) — the driver, class and entity handle a device found by this
// pattern is reported under. Every case in this file uses one media-player
// watch, so the facts are constant and the identity a response supplies (its
// USN) is the only thing that varies.
func watchFor(m deviceplane.Match) Watch {
	return Watch{
		Match:       m,
		Driver:      "roku-ecp",
		DeviceClass: "media-player",
		Entities:    []deviceplane.CandidateEntity{{Key: "main", DeviceClass: "media-player"}},
	}
}

func TestNewValidation(t *testing.T) {
	rokuWatch := []Watch{watchFor(mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`))}
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }

	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "nil Store",
			cfg:  Config{Watches: rokuWatch, Store: nil, NowMillis: now},
		},
		{
			name: "nil NowMillis",
			cfg:  Config{Watches: rokuWatch, Store: store, NowMillis: nil},
		},
		{
			name: "no patterns at all",
			cfg:  Config{Watches: nil, Store: store, NowMillis: now},
		},
		{
			name: "only non-SSDP patterns",
			cfg: Config{
				Watches:   []Watch{watchFor(mustMatch(t, `{"mdns":"_googlecast._tcp"}`))},
				Store:     store,
				NowMillis: now,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Fatal("New() error = nil, want error")
			}
		})
	}
}

func TestNewDefaultsAndOverrides(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pat := []Watch{watchFor(mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`))}

	d, err := New(Config{Watches: pat, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if d.interval != defaultInterval {
		t.Errorf("interval = %v, want default %v", d.interval, defaultInterval)
	}
	if d.searchWait != defaultSearchWait {
		t.Errorf("searchWait = %v, want default %v", d.searchWait, defaultSearchWait)
	}

	d2, err := New(Config{
		Watches:    pat,
		Store:      store,
		NowMillis:  now,
		Interval:   5 * time.Second,
		SearchWait: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if d2.interval != 5*time.Second {
		t.Errorf("interval override not applied: got %v", d2.interval)
	}
	if d2.searchWait != 1*time.Second {
		t.Errorf("searchWait override not applied: got %v", d2.searchWait)
	}
}

// TestSweepObservesMatchingST asserts a search response whose ST exactly
// matches the configured pattern is Observed into the Store (REL-110/111,
// MAN-071 exact search-target-string match).
func TestSweepObservesMatchingST(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`)

	d, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	d.search = func(st string, wait int) ([]foundService, error) {
		return []foundService{{ST: st, USN: "uuid:device:1"}}, nil
	}

	d.sweep(context.Background())

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(cands), cands)
	}
	if cands[0].Match != pattern {
		t.Errorf("candidate match = %+v, want %+v", cands[0].Match, pattern)
	}
	if cands[0].Provenance != deviceplane.ProvenanceDiscovered {
		t.Errorf("provenance = %q, want %q", cands[0].Provenance, deviceplane.ProvenanceDiscovered)
	}
	if cands[0].FirstSeen != 1000 || cands[0].LastSeen != 1000 {
		t.Errorf("first/last seen = %d/%d, want 1000/1000", cands[0].FirstSeen, cands[0].LastSeen)
	}
}

// TestSweepIgnoresNonMatchingST asserts a search response whose ST does not
// exactly equal the configured pattern is never Observed.
func TestSweepIgnoresNonMatchingST(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`)

	d, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	d.search = func(st string, wait int) ([]foundService, error) {
		return []foundService{{ST: "urn:some-other:device:1", USN: "uuid:device:1"}}, nil
	}

	d.sweep(context.Background())

	if cands := store.Report().Body.Candidates; len(cands) != 0 {
		t.Fatalf("got %d candidates, want 0: %+v", len(cands), cands)
	}
}

// TestSweepRepeatedHitsBumpLastSeenNotDuplicate asserts two responses within
// one sweep, and a second sweep round, both dedup to a single candidate
// with last_seen bumped and first_seen left alone (Store.Observe's own
// Match.Key() dedup, REL-110/111).
func TestSweepRepeatedHitsBumpLastSeenNotDuplicate(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	nowVal := int64(1000)
	now := func() int64 { return nowVal }
	pattern := mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`)

	d, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	d.search = func(st string, wait int) ([]foundService, error) {
		// Two responses in the same sweep round.
		return []foundService{{ST: st, USN: "uuid:device:1"}, {ST: st, USN: "uuid:device:1"}}, nil
	}

	d.sweep(context.Background())
	nowVal = 2000
	d.sweep(context.Background())

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1 (dedup by device identity): %+v", len(cands), cands)
	}
	if cands[0].FirstSeen != 1000 {
		t.Errorf("first_seen = %d, want 1000 (must not move)", cands[0].FirstSeen)
	}
	if cands[0].LastSeen != 2000 {
		t.Errorf("last_seen = %d, want 2000 (must bump on re-observe)", cands[0].LastSeen)
	}
}

// TestSweepSearchErrorDoesNotStopOtherPatterns asserts a search error on one
// pattern never prevents another configured pattern from being tried and
// observed in the same sweep.
func TestSweepSearchErrorDoesNotStopOtherPatterns(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	bad := mustMatch(t, `{"ssdp":"urn:bad:device:1"}`)
	good := mustMatch(t, `{"ssdp":"urn:good:device:1"}`)

	d, err := New(Config{Watches: []Watch{watchFor(bad), watchFor(good)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	d.search = func(st string, wait int) ([]foundService, error) {
		if st == "urn:bad:device:1" {
			return nil, errors.New("boom")
		}
		return []foundService{{ST: st, USN: "uuid:device:1"}}, nil
	}

	d.sweep(context.Background())

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1 (only the good pattern): %+v", len(cands), cands)
	}
	if cands[0].Match != good {
		t.Errorf("candidate = %+v, want good pattern %+v", cands[0].Match, good)
	}
}

// TestObserveAliveMatchingNT asserts an alive NOTIFY whose NT exactly
// matches a configured pattern is Observed into the Store.
func TestObserveAliveMatchingNT(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 5000 }
	pattern := mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`)

	d, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	d.observeAlive("urn:roku-com:device:player:1", "uuid:device:1")

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	if cands[0].Match != pattern {
		t.Errorf("candidate match = %+v, want %+v", cands[0].Match, pattern)
	}
	if cands[0].FirstSeen != 5000 {
		t.Errorf("first_seen = %d, want 5000", cands[0].FirstSeen)
	}
}

// TestObserveAliveNonMatchingNTIgnored asserts an alive NOTIFY whose NT does
// not match any configured pattern is never Observed.
func TestObserveAliveNonMatchingNTIgnored(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 5000 }
	pattern := mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`)

	d, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	d.observeAlive("urn:some-other:device:1", "uuid:device:1")

	if cands := store.Report().Body.Candidates; len(cands) != 0 {
		t.Fatalf("got %d candidates, want 0: %+v", len(cands), cands)
	}
}

// TestRunStopsPromptlyOnContextCancel asserts Run starts the alive monitor,
// then returns ctx.Err() promptly once ctx is canceled — even with a long
// sweep Interval configured, since cancellation must win the select
// regardless of the ticker period — and always closes the monitor before
// returning.
func TestRunStopsPromptlyOnContextCancel(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`)

	d, err := New(Config{
		Watches:   []Watch{watchFor(pattern)},
		Store:     store,
		NowMillis: now,
		Interval:  time.Hour,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	mon := &fakeMonitor{}
	d.newMonitor = func(onAlive func(string, string)) ssdpMonitor { return mon }
	d.search = func(st string, wait int) ([]foundService, error) { return nil, nil }

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return promptly after ctx cancel")
	}

	if !mon.started {
		t.Error("Run() did not start the alive monitor")
	}
	if !mon.closed {
		t.Error("Run() did not close the alive monitor before returning")
	}
}

// TestRunStopsPromptlyDuringSlowSearch is C1's regression test: it proves Run
// returns promptly on ctx cancellation even while a real, blocking search is
// still in flight (the earlier TestRunStopsPromptlyOnContextCancel only ever
// exercised a zero-latency fake search, which could never detect this), AND
// that the search's eventual late result — which arrives well after Run has
// already returned — is discarded rather than reaching the Store (the
// Important stray-goroutine finding).
func TestRunStopsPromptlyDuringSlowSearch(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`)

	d, err := New(Config{
		Watches:   []Watch{watchFor(pattern)},
		Store:     store,
		NowMillis: now,
		Interval:  time.Hour,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	mon := &fakeMonitor{}
	d.newMonitor = func(onAlive func(string, string)) ssdpMonitor { return mon }

	searchStarted := make(chan struct{})
	releaseSearch := make(chan struct{})
	var searchStartedOnce sync.Once
	d.search = func(st string, wait int) ([]foundService, error) {
		searchStartedOnce.Do(func() { close(searchStarted) })
		<-releaseSearch // held open well past ctx cancellation and Run's return
		return []foundService{{ST: st, USN: "uuid:device:1"}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	<-searchStarted // make sure sweep is blocked mid-search before canceling
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return promptly while a search was still blocked")
	}

	// Run has returned while the search goroutine is still blocked. Release
	// it now and give it time to (wrongly) reach the Store if the ctx guard
	// in searchPattern were missing.
	close(releaseSearch)
	time.Sleep(100 * time.Millisecond)

	if cands := store.Report().Body.Candidates; len(cands) != 0 {
		t.Fatalf("got %d candidates after Run returned, want 0 (a late search result must be discarded, not Observed)", len(cands))
	}
}

// TestNewClampsSubSecondSearchWaitToOneSecond asserts M5's fix: a positive
// but sub-second Config.SearchWait is floored to 1s rather than silently
// truncating (via sweep's int(searchWait/time.Second)) to a 0-second wait
// that would collect zero responses every sweep.
func TestNewClampsSubSecondSearchWaitToOneSecond(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pat := []Watch{watchFor(mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`))}

	d, err := New(Config{Watches: pat, Store: store, NowMillis: now, SearchWait: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if d.searchWait != time.Second {
		t.Errorf("searchWait = %v, want floored to 1s", d.searchWait)
	}
}

// TestLiveLANSearch is an optional, env-gated integration check against the
// real dev LAN's multicast SSDP traffic (the dev LAN has Rokus that respond
// to roku:ecp). Skipped by default so CI and ordinary `go test` runs never
// touch multicast; set WAIVEO_HW_LAN=1 to run it on a box with real Rokus
// on the LAN.
func TestLiveLANSearch(t *testing.T) {
	if os.Getenv("WAIVEO_HW_LAN") != "1" {
		t.Skip("set WAIVEO_HW_LAN=1 to run against the real dev LAN (multicast SSDP)")
	}

	found, err := defaultSearch("roku:ecp", 3)
	if err != nil {
		t.Fatalf("defaultSearch(roku:ecp, 3): %v", err)
	}
	if len(found) == 0 {
		t.Fatal("expected >= 1 roku:ecp response on the dev LAN, got 0")
	}
}

// TestSweepReportsResponderIdentity is REL-110a at the SSDP lane: the candidate
// a sweep produces names WHICH device answered, not merely that the pattern was
// hit. Two responders under one search target must produce two candidates, each
// carrying its own USN as its native_id — the case a lane that discarded the USN
// gets wrong, and the reason a device discovered here can be listed and
// addressed at all.
func TestSweepReportsResponderIdentity(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`)

	d, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.search = func(st string, wait int) ([]foundService, error) {
		return []foundService{
			{ST: st, USN: "uuid:roku:X1"},
			{ST: st, USN: "uuid:roku:X2"},
		}, nil
	}

	d.sweep(context.Background())

	cands := store.Report().Body.Candidates
	if len(cands) != 2 {
		t.Fatalf("got %d candidate(s), want 2 — two responders under one search target are two devices", len(cands))
	}
	seen := map[string]deviceplane.Candidate{}
	for _, c := range cands {
		seen[c.NativeID] = c
	}
	for _, want := range []string{"uuid:roku:X1", "uuid:roku:X2"} {
		c, ok := seen[want]
		if !ok {
			t.Fatalf("no candidate carries native_id %q; got %v", want, seen)
		}
		if c.Driver != "roku-ecp" || c.DeviceClass != "media-player" {
			t.Errorf("candidate %q = driver %q class %q, want the watch's declared roku-ecp/media-player", want, c.Driver, c.DeviceClass)
		}
		if len(c.Entities) != 1 || c.Entities[0].Key != "main" {
			t.Errorf("candidate %q entities = %+v, want the watch's declared single main handle", want, c.Entities)
		}
	}
}

// TestResponseWithoutUSNIsNotObserved: a response that identifies no device
// cannot be reported as one (REL-110a). Observing it under a synthetic identity
// would let two unidentifiable responders collapse into one candidate that
// neither of them is.
func TestResponseWithoutUSNIsNotObserved(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	pattern := mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`)
	d, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: func() int64 { return 1 }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.search = func(st string, wait int) ([]foundService, error) {
		return []foundService{{ST: st}}, nil // no USN
	}
	d.sweep(context.Background())
	if n := len(store.Report().Body.Candidates); n != 0 {
		t.Fatalf("got %d candidate(s) from a response with no USN, want 0", n)
	}
	d.observeAlive("urn:roku-com:device:player:1", "")
	if n := len(store.Report().Body.Candidates); n != 0 {
		t.Fatalf("got %d candidate(s) from an alive NOTIFY with no USN, want 0", n)
	}
}
