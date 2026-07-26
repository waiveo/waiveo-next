package ecppoll

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// fixtureServer is a canned ECP double: an httptest server whose
// /query/active-app and /query/device-info bodies can be mutated mid-test.
type fixtureServer struct {
	mu         sync.Mutex
	activeApp  string
	deviceInfo string

	srv *httptest.Server
}

func newFixtureServer() *fixtureServer {
	fx := &fixtureServer{
		activeApp:  `<active-app><app id="12" type="appl" version="1.0">TestApp</app></active-app>`,
		deviceInfo: `<device-info><power-mode>PowerOn</power-mode></device-info>`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/query/active-app", func(w http.ResponseWriter, r *http.Request) {
		fx.mu.Lock()
		body := fx.activeApp
		fx.mu.Unlock()
		w.Write([]byte(body))
	})
	mux.HandleFunc("/query/device-info", func(w http.ResponseWriter, r *http.Request) {
		fx.mu.Lock()
		body := fx.deviceInfo
		fx.mu.Unlock()
		w.Write([]byte(body))
	})
	fx.srv = httptest.NewServer(mux)
	return fx
}

func (fx *fixtureServer) setActiveApp(s string) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.activeApp = s
}

func (fx *fixtureServer) setDeviceInfo(s string) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.deviceInfo = s
}

func (fx *fixtureServer) target(t *testing.T) Target {
	t.Helper()
	u, err := url.Parse(fx.srv.URL)
	if err != nil {
		t.Fatalf("parse fixture URL: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split fixture host/port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse fixture port: %v", err)
	}
	return Target{Host: host, Port: port}
}

func (fx *fixtureServer) Close() { fx.srv.Close() }

// --- first-observation semantics ---

func TestFirstSnapshotSeedsWithoutEmitting(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()

	p := New(map[string]Target{"tv1": fx.target(t)}, time.Hour)
	ctx := context.Background()
	p.pollAll(ctx)

	seeds := p.Seeds()
	if len(seeds) != 1 {
		t.Fatalf("Seeds() len = %d, want 1", len(seeds))
	}
	if seeds[0].ID != "tv1" || seeds[0].State != "on" {
		t.Fatalf("Seeds()[0] = %+v, want ID=tv1 State=on", seeds[0])
	}

	select {
	case obs := <-p.ch:
		t.Fatalf("expected no emitted Observation on first snapshot, got %+v", obs)
	default:
	}
}

func TestSecondIdenticalPollEmitsNothing(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()

	p := New(map[string]Target{"tv1": fx.target(t)}, time.Hour)
	ctx := context.Background()
	p.pollAll(ctx) // seed
	p.pollAll(ctx) // identical snapshot: quiet tick

	select {
	case obs := <-p.ch:
		t.Fatalf("expected no emitted Observation on an unchanged tick, got %+v", obs)
	default:
	}
}

func TestAppChangeEmitsObservationWithChangedAttrsOnly(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()

	p := New(map[string]Target{"tv1": fx.target(t)}, time.Hour)
	ctx := context.Background()
	p.pollAll(ctx) // seed: app id=12 TestApp v1.0

	fx.setActiveApp(`<active-app><app id="99" type="appl" version="2.0">OtherApp</app></active-app>`)
	p.pollAll(ctx)

	obs, ok := p.Next()
	if !ok {
		t.Fatalf("expected an emitted Observation after an app change")
	}
	if obs.StateChanged {
		t.Fatalf("StateChanged = true, want false (on -> on)")
	}
	want := []string{"active_app", "active_app_id"}
	if !equalStrings(obs.ChangedAttrs, want) {
		t.Fatalf("ChangedAttrs = %v, want %v (app_version is cosmetic, app_type unchanged)", obs.ChangedAttrs, want)
	}
	if obs.Entity.State != "on" {
		t.Fatalf("Entity.State = %q, want on", obs.Entity.State)
	}
	if obs.Entity.Attributes["active_app"] != "OtherApp" {
		t.Fatalf("active_app = %v, want OtherApp", obs.Entity.Attributes["active_app"])
	}
	if obs.Entity.Attributes["active_app_id"] != "99" {
		t.Fatalf("active_app_id = %v, want 99", obs.Entity.Attributes["active_app_id"])
	}
	if obs.Entity.Attributes["app_version"] != "2.0" {
		t.Fatalf("app_version = %v, want 2.0 (attribute itself still updates)", obs.Entity.Attributes["app_version"])
	}
}

// --- state derivation ---

func TestStandbyDerivation(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()

	p := New(map[string]Target{"tv1": fx.target(t)}, time.Hour)
	ctx := context.Background()
	p.pollAll(ctx) // seed: PowerOn / on

	fx.setDeviceInfo(`<device-info><power-mode>Suspend</power-mode></device-info>`)
	p.pollAll(ctx)

	obs, ok := p.Next()
	if !ok {
		t.Fatalf("expected an emitted Observation on power-mode change")
	}
	if !obs.StateChanged || obs.Entity.State != "standby" {
		t.Fatalf("Entity = %+v StateChanged=%v, want State=standby StateChanged=true", obs.Entity, obs.StateChanged)
	}
	if obs.Entity.Attributes["power_mode"] != "Suspend" {
		t.Fatalf("power_mode = %v, want Suspend", obs.Entity.Attributes["power_mode"])
	}
}

func TestScreensaverDerivation(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()

	p := New(map[string]Target{"tv1": fx.target(t)}, time.Hour)
	ctx := context.Background()
	p.pollAll(ctx) // seed: app on

	fx.setActiveApp(`<active-app><screensaver id="7" type="ss">MyScreensaver</screensaver></active-app>`)
	p.pollAll(ctx)

	obs, ok := p.Next()
	if !ok {
		t.Fatalf("expected an emitted Observation on screensaver transition")
	}
	if !obs.StateChanged || obs.Entity.State != "idle" {
		t.Fatalf("Entity = %+v StateChanged=%v, want State=idle StateChanged=true", obs.Entity, obs.StateChanged)
	}
	attrs := obs.Entity.Attributes
	if attrs["is_screensaver"] != true {
		t.Fatalf("is_screensaver = %v, want true", attrs["is_screensaver"])
	}
	if attrs["app_type"] != "screensaver" {
		t.Fatalf("app_type = %v, want screensaver", attrs["app_type"])
	}
	if attrs["active_app"] != "MyScreensaver" {
		t.Fatalf("active_app = %v, want MyScreensaver", attrs["active_app"])
	}
	if attrs["active_app_id"] != "7" {
		t.Fatalf("active_app_id = %v, want 7", attrs["active_app_id"])
	}
}

func TestHomeScreenDynamicMenuDerivesIdle(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()

	// A firmware that reports the Home screen as an <app> with no type
	// attribute, identified only by its well-known id (package doc step 5).
	fx.setActiveApp(`<active-app><app id="562859">Roku</app></active-app>`)

	p := New(map[string]Target{"tv1": fx.target(t)}, time.Hour)
	ctx := context.Background()
	p.pollAll(ctx)

	seeds := p.Seeds()
	if len(seeds) != 1 || seeds[0].State != "idle" {
		t.Fatalf("Seeds()[0] = %+v, want State=idle", seeds[0])
	}
	if seeds[0].Attributes["app_type"] != nil {
		t.Fatalf("app_type = %v, want nil (type attr absent, literal map only)", seeds[0].Attributes["app_type"])
	}
}

func TestMissingAppElementDerivesIdle(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()
	fx.setActiveApp(`<active-app></active-app>`)

	p := New(map[string]Target{"tv1": fx.target(t)}, time.Hour)
	ctx := context.Background()
	p.pollAll(ctx)

	seeds := p.Seeds()
	if len(seeds) != 1 || seeds[0].State != "idle" {
		t.Fatalf("Seeds()[0] = %+v, want State=idle", seeds[0])
	}
}

// --- fetch/parse failure ---

func TestUnreachableDerivesUnavailable(t *testing.T) {
	fx := newFixtureServer()
	target := fx.target(t)
	fx.Close() // nothing is listening on target's port from here on

	p := New(map[string]Target{"tv1": target}, time.Hour)
	ctx := context.Background()
	p.pollAll(ctx)

	seeds := p.Seeds()
	if len(seeds) != 1 {
		t.Fatalf("Seeds() len = %d, want 1", len(seeds))
	}
	assertUnavailable(t, seeds[0])
}

func TestXMLGarbageDerivesUnavailable(t *testing.T) {
	fx := newFixtureServer()
	defer fx.Close()
	fx.setActiveApp(`not xml at all {{{`)

	p := New(map[string]Target{"tv1": fx.target(t)}, time.Hour)
	ctx := context.Background()
	p.pollAll(ctx)

	seeds := p.Seeds()
	if len(seeds) != 1 {
		t.Fatalf("Seeds() len = %d, want 1", len(seeds))
	}
	assertUnavailable(t, seeds[0])
}

func assertUnavailable(t *testing.T, e state.Entity) {
	t.Helper()
	if e.State != "unavailable" {
		t.Fatalf("State = %q, want unavailable", e.State)
	}
	for _, k := range []string{"active_app", "active_app_id", "app_type", "power_mode", "app_version"} {
		if v := e.Attributes[k]; v != nil {
			t.Fatalf("Attributes[%q] = %v, want nil", k, v)
		}
	}
	if e.Attributes["is_screensaver"] != false {
		t.Fatalf("is_screensaver = %v, want false", e.Attributes["is_screensaver"])
	}
}

// --- channel overflow ---

func TestChannelOverflowDropsOldestAndCounts(t *testing.T) {
	var counter atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/query/active-app", func(w http.ResponseWriter, r *http.Request) {
		n := counter.Add(1)
		fmt.Fprintf(w, `<active-app><app id="%d" type="appl">App%d</app></active-app>`, n, n)
	})
	mux.HandleFunc("/query/device-info", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<device-info><power-mode>PowerOn</power-mode></device-info>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)
	target := Target{Host: host, Port: port}

	const chanSize = 2
	p := New(map[string]Target{"tv1": target}, time.Millisecond, WithChannelSize(chanSize))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	<-done

	if p.Dropped() == 0 {
		t.Fatalf("Dropped() = 0, want > 0 (many quick ticks with no reader)")
	}

	var received []string
	for {
		obs, ok := p.Next()
		if !ok {
			break
		}
		received = append(received, fmt.Sprintf("%v", obs.Entity.Attributes["active_app_id"]))
	}
	if len(received) == 0 {
		t.Fatalf("expected at least one buffered Observation to survive")
	}
	if len(received) > chanSize {
		t.Fatalf("received %d Observations, want <= chanSize=%d", len(received), chanSize)
	}
	// Drop-oldest means whatever survived is the tail of the sequence: the
	// first surviving id should be well past 1, proving early observations
	// were the ones discarded, not new ones being rejected.
	firstID := received[0]
	if firstID == "1" || firstID == "2" {
		t.Fatalf("first surviving Observation had active_app_id=%s, want a late id (oldest should have been dropped, not newest)", firstID)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
