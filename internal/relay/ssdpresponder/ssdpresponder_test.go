package ssdpresponder

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	ssdp "github.com/koron/go-ssdp"
)

// fakeAdvertiser is an advertiser test double that never touches multicast:
// it records call counts and can be made to fail, mirroring
// discovery_test.go's fakeMonitor.
type fakeAdvertiser struct {
	mu         sync.Mutex
	aliveCalls int
	byeCalls   int
	closeCalls int
	aliveErr   error
}

func (f *fakeAdvertiser) Alive() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aliveCalls++
	return f.aliveErr
}

func (f *fakeAdvertiser) Bye() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byeCalls++
	return nil
}

func (f *fakeAdvertiser) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return nil
}

func (f *fakeAdvertiser) counts() (alive, bye, close int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aliveCalls, f.byeCalls, f.closeCalls
}

// waitForAliveCount polls fake's alive count until it reaches at least n or
// timeout elapses, failing the test on timeout. Polling (rather than a fixed
// sleep) keeps this race-free without coupling to a particular scheduling
// delay.
func waitForAliveCount(t *testing.T, fake *fakeAdvertiser, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if alive, _, _ := fake.counts(); alive >= n {
			return
		}
		if time.Now().After(deadline) {
			alive, _, _ := fake.counts()
			t.Fatalf("alive calls = %d after %s, want >= %d", alive, timeout, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"empty BaseURL", Config{BaseURL: "", USN: "uuid:x"}},
		{"unparseable BaseURL", Config{BaseURL: "://bad", USN: "uuid:x"}},
		{"BaseURL missing scheme and host", Config{BaseURL: "not-a-url", USN: "uuid:x"}},
		{"empty USN", Config{BaseURL: "https://192.0.2.1:7421/player/v1", USN: ""}},
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
	r, err := New(Config{BaseURL: "https://192.0.2.1:7421/player/v1", USN: "uuid:x"})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if r.maxAgeSec != int(defaultMaxAge/time.Second) {
		t.Errorf("maxAgeSec = %d, want default %d", r.maxAgeSec, int(defaultMaxAge/time.Second))
	}
	if r.aliveEvery != defaultMaxAge/2 {
		t.Errorf("aliveEvery = %v, want default %v", r.aliveEvery, defaultMaxAge/2)
	}
	if r.usn != "uuid:x" {
		t.Errorf("usn = %q, want uuid:x", r.usn)
	}
	if got := r.loc.Location(nil, nil); got != "https://192.0.2.1:7421/player/v1" {
		t.Errorf("initial location = %q, want the configured BaseURL", got)
	}

	r2, err := New(Config{BaseURL: "https://192.0.2.1:7421/player/v1", USN: "uuid:x", MaxAge: 4000 * time.Second})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if r2.maxAgeSec != 4000 {
		t.Errorf("maxAgeSec override = %d, want 4000", r2.maxAgeSec)
	}
	if r2.aliveEvery != 2000*time.Second {
		t.Errorf("aliveEvery override = %v, want 2000s (half of MaxAge)", r2.aliveEvery)
	}
}

// TestNewFloorsAliveIntervalForSmallMaxAge asserts a small configured MaxAge
// (whose half would be well under minAliveInterval) floors Run's periodic
// re-announce cadence rather than turning it into a busy-loop (M5-style
// floor, mirroring discovery.go's minSearchWait).
func TestNewFloorsAliveIntervalForSmallMaxAge(t *testing.T) {
	r, err := New(Config{BaseURL: "https://192.0.2.1:7421/player/v1", USN: "uuid:x", MaxAge: 10 * time.Second})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if r.aliveEvery != minAliveInterval {
		t.Errorf("aliveEvery = %v, want floored to %v", r.aliveEvery, minAliveInterval)
	}
}

func TestRunReturnsAdvertiseError(t *testing.T) {
	r, err := New(Config{BaseURL: "https://192.0.2.1:7421/player/v1", USN: "uuid:x"})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	boom := errors.New("bind failed")
	r.advertise = func(st, usn string, location ssdp.LocationProvider, server string, maxAgeSec int) (advertiser, error) {
		return nil, boom
	}

	if err := r.Run(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Run() error = %v, want wrapping %v", err, boom)
	}
}

// TestRunAdvertisesImmediatelyAndRespondsToCancel asserts Run passes the
// configured ST/USN/location/maxAge to the advertiser, sends an immediate
// ssdp:alive on start, and — on ctx cancellation — sends ssdp:byebye, closes
// the advertiser, and returns ctx.Err() promptly (no announce ever sent
// after Run returns is exercised by TestUpdateBaseURLAfterRunReturnsIsANoOp).
func TestRunAdvertisesImmediatelyAndRespondsToCancel(t *testing.T) {
	r, err := New(Config{BaseURL: "https://192.0.2.1:7421/player/v1", USN: "uuid:waiveo-relay:relay-1", MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	var gotST, gotUSN, gotServer string
	var gotMaxAge int
	var gotLocation string
	fake := &fakeAdvertiser{}
	r.advertise = func(st, usn string, location ssdp.LocationProvider, server string, maxAgeSec int) (advertiser, error) {
		gotST, gotUSN, gotServer, gotMaxAge = st, usn, server, maxAgeSec
		gotLocation = location.Location(nil, nil)
		return fake, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	waitForAliveCount(t, fake, 1, 2*time.Second)

	if gotST != SearchTarget {
		t.Errorf("advertise ST = %q, want %q", gotST, SearchTarget)
	}
	if gotUSN != "uuid:waiveo-relay:relay-1" {
		t.Errorf("advertise USN = %q, want the configured USN", gotUSN)
	}
	if gotServer != "" {
		t.Errorf("advertise server = %q, want empty (no SERVER header configured)", gotServer)
	}
	if gotMaxAge != int((time.Hour)/time.Second) {
		t.Errorf("advertise maxAgeSec = %d, want %d", gotMaxAge, int(time.Hour/time.Second))
	}
	if gotLocation != "https://192.0.2.1:7421/player/v1" {
		t.Errorf("advertise location = %q, want the configured BaseURL", gotLocation)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return promptly after ctx cancel")
	}

	_, bye, closeCalls := fake.counts()
	if bye != 1 {
		t.Errorf("bye calls = %d, want 1", bye)
	}
	if closeCalls != 1 {
		t.Errorf("close calls = %d, want 1", closeCalls)
	}
}

// TestUpdateBaseURLReannouncesAliveWhileRunning is the PLY-022 oracle: while
// Run is active, UpdateBaseURL swaps the served location and immediately
// sends one extra ssdp:alive re-announcement under the new address.
func TestUpdateBaseURLReannouncesAliveWhileRunning(t *testing.T) {
	r, err := New(Config{BaseURL: "https://192.0.2.1:7421/player/v1", USN: "uuid:x", MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	fake := &fakeAdvertiser{}
	r.advertise = func(st, usn string, location ssdp.LocationProvider, server string, maxAgeSec int) (advertiser, error) {
		return fake, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	waitForAliveCount(t, fake, 1, 2*time.Second) // Run's own initial announce

	if err := r.UpdateBaseURL("https://192.0.2.2:7421/player/v1"); err != nil {
		t.Fatalf("UpdateBaseURL: %v", err)
	}

	waitForAliveCount(t, fake, 2, 2*time.Second) // the PLY-022 re-announce

	if got := r.loc.Location(nil, nil); got != "https://192.0.2.2:7421/player/v1" {
		t.Errorf("location after UpdateBaseURL = %q, want the new address", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after ctx cancel")
	}
}

// TestUpdateBaseURLBeforeRunRecordsAddressOnly asserts calling UpdateBaseURL
// before Run has ever started only swaps the served location (Run's
// eventual first announce will carry it) without attempting to reach a
// not-yet-existing advertiser.
func TestUpdateBaseURLBeforeRunRecordsAddressOnly(t *testing.T) {
	r, err := New(Config{BaseURL: "https://192.0.2.1:7421/player/v1", USN: "uuid:x"})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if err := r.UpdateBaseURL("https://192.0.2.9:7421/player/v1"); err != nil {
		t.Fatalf("UpdateBaseURL before Run: %v", err)
	}
	if got := r.loc.Location(nil, nil); got != "https://192.0.2.9:7421/player/v1" {
		t.Errorf("location after pre-Run UpdateBaseURL = %q, want the new address", got)
	}
}

// TestUpdateBaseURLAfterRunReturnsIsANoOp asserts the "no announce after Run
// returns" invariant from the caller's side: once Run has returned (ctx
// canceled), UpdateBaseURL still swaps the recorded address but sends no
// announce (r.adv is nil again), never reaching the now-closed advertiser.
func TestUpdateBaseURLAfterRunReturnsIsANoOp(t *testing.T) {
	r, err := New(Config{BaseURL: "https://192.0.2.1:7421/player/v1", USN: "uuid:x", MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	fake := &fakeAdvertiser{}
	r.advertise = func(st, usn string, location ssdp.LocationProvider, server string, maxAgeSec int) (advertiser, error) {
		return fake, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitForAliveCount(t, fake, 1, 2*time.Second)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after ctx cancel")
	}

	aliveBefore, _, _ := fake.counts()
	if err := r.UpdateBaseURL("https://192.0.2.9:7421/player/v1"); err != nil {
		t.Fatalf("UpdateBaseURL after Run returned: %v", err)
	}
	if got := r.loc.Location(nil, nil); got != "https://192.0.2.9:7421/player/v1" {
		t.Errorf("location after post-return UpdateBaseURL = %q, want the new address recorded", got)
	}
	aliveAfter, _, _ := fake.counts()
	if aliveAfter != aliveBefore {
		t.Errorf("alive calls after Run returned = %d, want unchanged %d (no announce after Run returns)", aliveAfter, aliveBefore)
	}
}

// TestUpdateBaseURLValidationLeavesPriorLocationInEffect asserts a rejected
// UpdateBaseURL (bad URL) never partially swaps the served location.
func TestUpdateBaseURLValidationLeavesPriorLocationInEffect(t *testing.T) {
	r, err := New(Config{BaseURL: "https://192.0.2.1:7421/player/v1", USN: "uuid:x"})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if err := r.UpdateBaseURL("not-a-url"); err == nil {
		t.Fatal("UpdateBaseURL(not-a-url) error = nil, want error")
	}
	if got := r.loc.Location(nil, nil); got != "https://192.0.2.1:7421/player/v1" {
		t.Errorf("location after a rejected UpdateBaseURL = %q, want unchanged original BaseURL", got)
	}
}

// TestLiveAdvertiseSmoke is an optional, env-gated integration check that
// defaultAdvertise can actually bind go-ssdp's real multicast listener and
// tear it down cleanly — skipped by default so CI and ordinary `go test`
// runs never touch multicast; set WAIVEO_HW_LAN=1 to run it, mirroring
// discovery_test.go's TestLiveLANSearch gate.
func TestLiveAdvertiseSmoke(t *testing.T) {
	if os.Getenv("WAIVEO_HW_LAN") != "1" {
		t.Skip("set WAIVEO_HW_LAN=1 to run against real multicast")
	}

	loc := &dynamicLocation{url: "https://192.0.2.1:7421/player/v1"}
	adv, err := defaultAdvertise(SearchTarget, "uuid:waiveo-relay:smoke-test", loc, "", 1800)
	if err != nil {
		t.Fatalf("defaultAdvertise: %v", err)
	}
	if err := adv.Alive(); err != nil {
		t.Errorf("Alive(): %v", err)
	}
	if err := adv.Bye(); err != nil {
		t.Errorf("Bye(): %v", err)
	}
	if err := adv.Close(); err != nil {
		t.Errorf("Close(): %v", err)
	}
}
