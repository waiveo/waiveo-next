package slidelive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// awaitCurrent drives Current until it reports a value or the deadline passes.
//
// The polling is not a test smell, it is the CONTRACT under test: Current never
// performs I/O, so the first call always misses and only schedules a background
// refresh. A caller — the real one included — reaches a value by asking again.
// Failing on the deadline rather than sleeping a fixed span keeps the test fast
// when the stub answers immediately and honest when it does not.
func awaitCurrent(t *testing.T, o *OpenMeteo, lat, long float64) Conditions {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, ok := o.Current(context.Background(), lat, long); ok {
			return c
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no reading became available within the deadline")
	return Conditions{}
}

// forecastStub serves one canned Open-Meteo current-conditions response and
// records every request's query, so a test can assert on what was ASKED as well
// as on what came back.
type forecastStub struct {
	body    string
	status  int
	hits    atomic.Int64
	mu      sync.Mutex
	queries []url.Values
}

func (f *forecastStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.Query())
		f.mu.Unlock()
		if f.status != 0 && f.status != http.StatusOK {
			w.WriteHeader(f.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.body))
	}
}

func (f *forecastStub) lastQuery(t *testing.T) url.Values {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		t.Fatalf("the upstream was never asked")
	}
	return f.queries[len(f.queries)-1]
}

func TestOpenMeteoFetchesAndConvertsAReading(t *testing.T) {
	stub := &forecastStub{body: `{"current":{"time":"2026-08-10T17:00","temperature_2m":21.6,"weather_code":3}}`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	o := NewOpenMeteo(OpenMeteoConfig{BaseURL: srv.URL, Client: srv.Client()})

	// The FIRST call must not block on the network and must not answer yet.
	if _, ok := o.Current(context.Background(), 39.7392, -104.9903); ok {
		t.Fatalf("the first call for a location must report no reading yet, not block for one")
	}

	got := awaitCurrent(t, o, 39.7392, -104.9903)
	// 21.6 °C rounds to 22 °C and 70.88 °F rounds to 71 °F — one reading, one
	// rounding rule, both units derived from it.
	if got.TempC != 22 || got.TempF != 71 {
		t.Fatalf("temperatures = %d°C/%d°F, want 22°C/71°F", got.TempC, got.TempF)
	}
	if got.Text != "Overcast" {
		t.Fatalf("WMO code 3 must read %q, got %q", "Overcast", got.Text)
	}

	q := stub.lastQuery(t)
	if q.Get("latitude") != "39.7392" || q.Get("longitude") != "-104.9903" {
		t.Fatalf("the request must carry the asked-for coordinates, got %v", q)
	}
	if q.Get("current") != "temperature_2m,weather_code" {
		t.Fatalf("the request must ask for exactly the two members decoded, got %q", q.Get("current"))
	}
}

func TestOpenMeteoServesTheCacheWithinTheTTL(t *testing.T) {
	stub := &forecastStub{body: `{"current":{"temperature_2m":10,"weather_code":0}}`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	now := time.Now()
	o := NewOpenMeteo(OpenMeteoConfig{
		BaseURL: srv.URL, Client: srv.Client(),
		TTL: time.Hour, Now: func() time.Time { return now },
	})

	awaitCurrent(t, o, 1, 2)
	after := stub.hits.Load()

	// Many further calls inside the TTL must not touch the network at all: this
	// runs on the Lease-issuance path once per player poll, per screen.
	for i := 0; i < 20; i++ {
		if _, ok := o.Current(context.Background(), 1, 2); !ok {
			t.Fatalf("a cached reading must keep answering")
		}
	}
	if stub.hits.Load() != after {
		t.Fatalf("a cached reading must not re-fetch: %d -> %d upstream hits", after, stub.hits.Load())
	}
}

func TestOpenMeteoRefreshesOnceTheTTLLapses(t *testing.T) {
	stub := &forecastStub{body: `{"current":{"temperature_2m":10,"weather_code":0}}`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	var mu sync.Mutex
	now := time.Now()
	o := NewOpenMeteo(OpenMeteoConfig{
		BaseURL: srv.URL, Client: srv.Client(), TTL: 10 * time.Minute,
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
	})

	awaitCurrent(t, o, 1, 2)
	before := stub.hits.Load()

	mu.Lock()
	now = now.Add(11 * time.Minute)
	mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for stub.hits.Load() == before && time.Now().Before(deadline) {
		// A stale reading is still SERVED while the refresh runs — the widget
		// never blanks — so the observable signal is the upstream hit, not the
		// returned value.
		if _, ok := o.Current(context.Background(), 1, 2); !ok {
			t.Fatalf("a stale reading must keep being served while it refreshes")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if stub.hits.Load() == before {
		t.Fatalf("a lapsed TTL must trigger a refresh")
	}
}

func TestOpenMeteoKeepsServingTheLastReadingWhenARefreshFails(t *testing.T) {
	stub := &forecastStub{body: `{"current":{"temperature_2m":30,"weather_code":0}}`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	var mu sync.Mutex
	now := time.Now()
	o := NewOpenMeteo(OpenMeteoConfig{
		BaseURL: srv.URL, Client: srv.Client(), TTL: time.Minute,
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
	})

	first := awaitCurrent(t, o, 1, 2)
	if first.TempC != 30 {
		t.Fatalf("setup: expected 30°C, got %d", first.TempC)
	}

	// The upstream starts failing and the reading goes stale. A screen must ride
	// this out showing a slightly old temperature, not blank.
	stub.status = http.StatusInternalServerError
	before := stub.hits.Load()
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for stub.hits.Load() == before && time.Now().Before(deadline) {
		c, ok := o.Current(context.Background(), 1, 2)
		if !ok || c.TempC != 30 {
			t.Fatalf("the last good reading must keep being served, got %+v ok=%v", c, ok)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if stub.hits.Load() == before {
		t.Fatalf("setup: the failing refresh never ran")
	}

	// And it must still be serving it after the failure, and still retrying —
	// a failed refresh may not wedge the in-flight flag.
	c, ok := o.Current(context.Background(), 1, 2)
	if !ok || c.TempC != 30 {
		t.Fatalf("after a failed refresh the previous reading must remain, got %+v ok=%v", c, ok)
	}
	retryDeadline := time.Now().Add(5 * time.Second)
	for stub.hits.Load() <= before+1 && time.Now().Before(retryDeadline) {
		o.Current(context.Background(), 1, 2)
		time.Sleep(2 * time.Millisecond)
	}
	if stub.hits.Load() <= before+1 {
		t.Fatalf("a failed refresh must be retried on a later call, hits stuck at %d", stub.hits.Load())
	}
}

func TestOpenMeteoCachesPerLocation(t *testing.T) {
	stub := &forecastStub{body: `{"current":{"temperature_2m":5,"weather_code":61}}`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	o := NewOpenMeteo(OpenMeteoConfig{BaseURL: srv.URL, Client: srv.Client(), TTL: time.Hour})

	awaitCurrent(t, o, 10, 20)
	before := stub.hits.Load()
	// A genuinely different location is a cache MISS even though the first one
	// is fresh — otherwise a second site would silently show the first's weather.
	awaitCurrent(t, o, 50, 60)
	if stub.hits.Load() <= before {
		t.Fatalf("a second location must be fetched, not served from the first's cache")
	}

	// Coordinates that round to the same 3-decimal key are the SAME location:
	// they share the weather, so they must share the entry and the request.
	after := stub.hits.Load()
	if _, ok := o.Current(context.Background(), 10.00001, 20.00001); !ok {
		t.Fatalf("a sub-metre coordinate difference must hit the existing entry")
	}
	if stub.hits.Load() != after {
		t.Fatalf("a rounding-equal location must not trigger its own fetch")
	}
}

func TestWMOConditionTextLeavesAnUnknownCodeUnnamed(t *testing.T) {
	// An unnamed code costs the condition token only — weatherValue renders it
	// as the unavailable placeholder while the temperature still shows. Inventing
	// a phrase, or printing the raw number, would put nonsense on a wall.
	if got := wmoConditionText(4242); got != "" {
		t.Fatalf("an unknown WMO code must be unnamed, got %q", got)
	}
	if got := wmoConditionText(0); got != "Clear" {
		t.Fatalf("code 0 must read Clear, got %q", got)
	}
}
