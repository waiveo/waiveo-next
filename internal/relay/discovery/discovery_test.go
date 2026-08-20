package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Fatal("New() error = nil, want error")
			}
		})
	}
}

// An empty (or SSDP-free) initial watch set is LEGAL: watches follow the
// signed desired state (REL-064), so a lane may start before the first pack
// pattern exists. The sweep and the NOTIFY path must both honor a LIVE swap —
// asserted against the Store, the far side of the whole lane.
func TestWatchesFollowSetWatches(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }

	d, err := New(Config{Watches: nil, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() with no initial watches must construct, got %v", err)
	}
	if got := d.WatchCount(); got != 0 {
		t.Fatalf("WatchCount() = %d before any SetWatches, want 0", got)
	}

	// A sweep with no watches searches nothing.
	searched := 0
	d.search = func(st string, waitSec int) ([]foundService, error) {
		searched++
		return []foundService{{ST: st, USN: "uuid:one", Location: "http://192.0.2.9:8060/"}}, nil
	}
	d.sweep(context.Background())
	if searched != 0 {
		t.Fatalf("an empty watch set swept %d target(s), want none", searched)
	}

	// After a live swap the SAME Discoverer sweeps the new target and the hit
	// lands in the Store under the new watch's declared facts.
	st := "urn:new-pack:device:thing:1"
	n := d.SetWatches([]Watch{{
		Match:       mustMatch(t, `{"ssdp":"`+st+`"}`),
		Driver:      "ssdp",
		DeviceClass: "media-player",
		Entities:    []deviceplane.CandidateEntity{{Key: "main", DeviceClass: "media-player"}},
	}})
	if n != 1 || d.WatchCount() != 1 {
		t.Fatalf("SetWatches installed %d (count %d), want 1", n, d.WatchCount())
	}
	d.sweep(context.Background())
	if searched != 1 {
		t.Fatalf("the swapped-in watch was swept %d time(s), want 1", searched)
	}
	report := store.Report()
	if len(report.Body.Candidates) != 1 {
		t.Fatalf("candidates = %d, want the swept hit observed", len(report.Body.Candidates))
	}

	// The NOTIFY path honors the swap too.
	d.observeAlive(context.Background(), aliveNotice{NT: st, USN: "uuid:two", Location: "http://192.0.2.10:8060/"})
	if got := len(store.Report().Body.Candidates); got != 2 {
		t.Fatalf("candidates after NOTIFY = %d, want 2", got)
	}

	// A full replace with an empty set forgets the watch rather than leaking it.
	if n := d.SetWatches(nil); n != 0 || d.WatchCount() != 0 {
		t.Fatalf("a full replace with no watches must forget the old set; got %d live", d.WatchCount())
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

	d.observeAlive(context.Background(), aliveNotice{NT: "urn:roku-com:device:player:1", USN: "uuid:device:1"})

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

	d.observeAlive(context.Background(), aliveNotice{NT: "urn:some-other:device:1", USN: "uuid:device:1"})

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
	d.newMonitor = func(onAlive func(aliveNotice)) ssdpMonitor { return mon }
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

// TestRunIsPassiveAndNeverSearches is the owner's rule as a test (Discovery
// spec §4, 2026-08-17: nothing active "until you tell it to do a scan"). Run
// starts the NOTIFY monitor and must originate NOTHING on the wire — no
// M-SEARCH at start, and none however long it runs. Before this, Run swept
// immediately and then every Interval, which made it the one lane that probed
// the network automatically at boot.
//
// The Interval is set absurdly short so that a reintroduced auto-sweep ticker
// would fire many times over during the window below; a passive Run fires none.
func TestRunIsPassiveAndNeverSearches(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`)

	d, err := New(Config{
		Watches:   []Watch{watchFor(pattern)},
		Store:     store,
		NowMillis: now,
		Interval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	mon := &fakeMonitor{}
	d.newMonitor = func(onAlive func(aliveNotice)) ssdpMonitor { return mon }

	var searches atomic.Int64
	d.search = func(st string, wait int) ([]foundService, error) {
		searches.Add(1)
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Long enough that a 1ms auto-sweep ticker would have fired hundreds of times.
	time.Sleep(150 * time.Millisecond)
	if n := searches.Load(); n != 0 {
		t.Errorf("Run issued %d M-SEARCH(es) on its own, want 0 — active probing must wait for a scan", n)
	}
	if !mon.started {
		t.Error("Run did not start the passive alive monitor")
	}

	// And the active half still works when explicitly asked.
	d.Scan(ctx)
	if n := searches.Load(); n == 0 {
		t.Error("Scan issued no M-SEARCH — the active half must still run on demand")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after ctx cancel")
	}
	if !mon.closed {
		t.Error("Run did not close the alive monitor before returning")
	}
}

// TestPassiveSightingNeverProbesTheDevice is the second half of the owner's rule
// (2026-08-17), and the half that is invisible without a test: a NOTIFY arriving
// on its own must not draw an outbound identity probe at the device. The
// M-SEARCH was the loud violation; this is the quiet one — every announcing
// device on the LAN drew an HTTP GET from the relay, at rest, with no scan.
//
// A passive sighting may still REPORT what a previous scan learned (that is
// memory, not traffic), which the second half of this case pins.
func TestPassiveSightingNeverProbesTheDevice(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`)

	var probes atomic.Int64
	d, err := New(Config{
		Watches:   []Watch{watchFor(pattern)},
		Store:     store,
		NowMillis: now,
		Identify: func(ctx context.Context, address string) (Identity, bool) {
			probes.Add(1)
			return Identity{Name: "Lobby TV", Model: "Roku Ultra", Serial: "X1"}, true
		},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	d.search = func(st string, wait int) ([]foundService, error) {
		return []foundService{{ST: st, USN: "uuid:device:1", Location: "http://192.168.50.31:8060/"}}, nil
	}

	// A passive NOTIFY for a device nothing has scanned: observed, but NOT probed.
	d.observeAlive(context.Background(), aliveNotice{
		NT: "urn:roku-com:device:player:1", USN: "uuid:device:1",
		Location: "http://192.168.50.31:8060/",
	})
	if n := probes.Load(); n != 0 {
		t.Fatalf("a passive NOTIFY drew %d identity probe(s), want 0 — probing waits for a scan", n)
	}
	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("passive sighting produced %d candidate(s), want 1 — it must still be SEEN, just not probed", len(cands))
	}
	if cands[0].Model != "" {
		t.Errorf("un-scanned passive candidate carries model %q, want empty until a scan enriches it", cands[0].Model)
	}

	// A scan DOES probe, and fills the identity in.
	d.Scan(context.Background())
	if n := probes.Load(); n == 0 {
		t.Fatal("a scan drew no identity probe — the active half must still enrich")
	}
	if got := store.Report().Body.Candidates[0].Model; got != "Roku Ultra" {
		t.Errorf("after a scan model = %q, want the probed value", got)
	}

	// And a LATER passive sighting reports what the scan learned, without
	// probing again — memory, not traffic.
	before := probes.Load()
	d.observeAlive(context.Background(), aliveNotice{
		NT: "urn:roku-com:device:player:1", USN: "uuid:device:1",
		Location: "http://192.168.50.31:8060/",
	})
	if probes.Load() != before {
		t.Errorf("a passive sighting re-probed after a scan had already identified the device")
	}
	if got := store.Report().Body.Candidates[0].Model; got != "Roku Ultra" {
		t.Errorf("passive sighting after a scan lost the known model (%q) — cached identity must still be reported", got)
	}
}

// TestScanStopsPromptlyDuringSlowSearch is C1's regression test, now aimed at
// Scan (the active half that actually searches): it proves a caller is released
// promptly on ctx cancellation even while a real, blocking search is still in
// flight, AND that the search's eventual late result — which arrives well after
// Scan has already returned — is discarded rather than reaching the Store (the
// Important stray-goroutine finding).
func TestScanStopsPromptlyDuringSlowSearch(t *testing.T) {
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
	d.newMonitor = func(onAlive func(aliveNotice)) ssdpMonitor { return mon }

	searchStarted := make(chan struct{})
	releaseSearch := make(chan struct{})
	var searchStartedOnce sync.Once
	d.search = func(st string, wait int) ([]foundService, error) {
		searchStartedOnce.Do(func() { close(searchStarted) })
		<-releaseSearch // held open well past ctx cancellation and Run's return
		return []foundService{{ST: st, USN: "uuid:device:1"}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { d.Scan(ctx); close(done) }()

	<-searchStarted // make sure the scan is blocked mid-search before canceling
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Scan() did not return promptly while a search was still blocked")
	}

	// Scan has returned while the search goroutine is still blocked. Release
	// it now and give it time to (wrongly) reach the Store if the ctx guard
	// in searchPattern were missing.
	close(releaseSearch)
	time.Sleep(100 * time.Millisecond)

	if cands := store.Report().Body.Candidates; len(cands) != 0 {
		t.Fatalf("got %d candidates after Scan returned, want 0 (a late search result must be discarded, not Observed)", len(cands))
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
	d.observeAlive(context.Background(), aliveNotice{NT: "urn:roku-com:device:player:1"})
	if n := len(store.Report().Body.Candidates); n != 0 {
		t.Fatalf("got %d candidate(s) from an alive NOTIFY with no USN, want 0", n)
	}
}

// --- §4.1 resolve-and-merge: one device, one candidate ------------------------

// An SSDP responder the neighbour lane also saw (its IP resolves to a MAC)
// MERGES onto the canonical MAC candidate — one row, carrying the neighbour's
// MAC identity AND the SSDP watch's class/name, not two rows. This is the fix
// for the double-count the neighbour lane would otherwise create.
func TestSSDPMergesOntoTheNeighbourCandidate(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	mac := "b0:a7:37:5d:22:c8"

	// The neighbour lane already minted this host by MAC.
	driver, nativeID, match, ok := deviceplane.MACIdentity(mac)
	if !ok {
		t.Fatal("MACIdentity rejected a valid MAC")
	}
	store.Observe(deviceplane.Observation{
		Match: match, Provenance: deviceplane.ProvenanceDiscovered,
		Driver: driver, NativeID: nativeID, DeviceClass: "unclassified",
		Address: "192.168.50.31",
	}, now())

	d, err := New(Config{
		Watches:   []Watch{watchFor(mustMatch(t, `{"ssdp":"roku:ecp"}`))},
		Store:     store,
		NowMillis: now,
		ResolveMAC: func(ip string) (string, bool) {
			if ip == "192.168.50.31" {
				return mac, true
			}
			return "", false
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.search = func(st string, wait int) ([]foundService, error) {
		return []foundService{{ST: st, USN: "uuid:roku:ecp:X029009", Location: "http://192.168.50.31:8060/"}}, nil
	}
	d.sweep(context.Background())

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1 — the SSDP sighting must MERGE onto the MAC host, not add a row: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.Driver != deviceplane.HostDriver || c.NativeID != nativeID {
		t.Errorf("merged identity = (%q,%q), want the canonical MAC identity", c.Driver, c.NativeID)
	}
	if c.Match.MacOui == "" {
		t.Errorf("merged Match lost its OUI (would thrash against the neighbour sweep): %+v", c.Match)
	}
	if c.DeviceClass != "media-player" {
		t.Errorf("class = %q, want the SSDP watch's media-player to ride the merge", c.DeviceClass)
	}
	if c.Address != "192.168.50.31:8060" {
		t.Errorf("address = %q, want the SSDP LOCATION address", c.Address)
	}
}

// A responder the neighbour table cannot resolve (cross-subnet) keeps its watch
// identity and mints its own candidate — merge is best-effort, never a drop.
func TestSSDPWithoutAMACKeepsItsWatchIdentity(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	d, err := New(Config{
		Watches:    []Watch{watchFor(mustMatch(t, `{"ssdp":"roku:ecp"}`))},
		Store:      store,
		NowMillis:  now,
		ResolveMAC: func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.search = func(st string, wait int) ([]foundService, error) {
		return []foundService{{ST: st, USN: "uuid:far:away", Location: "http://10.9.9.9:8060/"}}, nil
	}
	d.sweep(context.Background())

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	if cands[0].Driver != "roku-ecp" || cands[0].NativeID != "uuid:far:away" {
		t.Errorf("identity = (%q,%q), want the unresolved sighting to keep its watch identity",
			cands[0].Driver, cands[0].NativeID)
	}
}

// A DECLARED CLASS OUTRANKS AN INFERRED ONE, and this lane has to say so
// explicitly or it silently reports the ladder's zero value (#204).
//
// It is not a theoretical collision. canonicalize re-keys a MAC-resolvable SSDP
// sighting onto the MAC identity — the SAME store key hostmdns writes — so a
// pack's declaration and a service-type guess really do meet in keepClass. Left
// unranked, a watch that a human wrote ("a device answering this SSDP pattern is
// a media player") could never displace hostmdns's inference from whatever
// `_matter` or `_airplay` record the host happened to advertise. The rank would
// have INTRODUCED that regression, which is why the lane states its own.
func TestADeclaredWatchStatesProductAuthorityForItsClass(t *testing.T) {
	w := watchFor(mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`))
	obs := w.observation("uuid:roku:ecp:X1", "192.168.50.31:8060", Identity{})

	if obs.DeviceClass != "media-player" {
		t.Fatalf("device_class = %q, want media-player — the declaration's own class", obs.DeviceClass)
	}
	if obs.ClassRank != deviceplane.ClassRankProduct {
		t.Fatalf("class_rank = %d, want %d (product) — a pack's declared watch is the strongest class statement this relay has, and leaving it at the ladder's zero value means a service-type guess outranks a human's declaration on the same store key",
			obs.ClassRank, deviceplane.ClassRankProduct)
	}
}
