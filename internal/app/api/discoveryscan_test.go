package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// discoveryscan_test.go covers POST /discovery/scan — the operator action the
// passive/active split exists for. Discovery's passive lanes run always and
// originate nothing; every probe waits for THIS call (Discovery spec §4, owner
// 2026-08-17). These cases drive the real HTTP surface against a scripted
// dispatcher, so what is asserted is what a relay would actually be asked.

const (
	scanRelayA = "relay-scan-a"
	scanRelayB = "relay-scan-b"
)

// scanEnv is an api handler with a device plane, a scripted dispatcher, and a
// pairing directory reporting the given relays as connected.
type scanEnv struct {
	*testEnv
	dispatcher *fakeDispatcher
}

func newScanEnv(t *testing.T, connected ...string) *scanEnv {
	t.Helper()
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clock := func() int64 { return fixedNowMs }
	fixture := newAuthFixture(t)
	dispatcher := &fakeDispatcher{scanResult: wire.NewDiscoveryScanAccepted("01J8Z8SCAN0000000000000001")}
	registry := devices.New(devScopeA, clock)

	relays := make([]api.PairingRelay, 0, len(connected))
	for _, id := range connected {
		relays = append(relays, api.PairingRelay{RelayID: id})
	}

	content := origin.New()
	ts := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, ulid.Monotonic(),
		content, testContentBase, fixture.Auth,
		api.WithDevicePlane(registry, dispatcher),
		api.WithPairing(api.PairingRelayDirectory{
			ConnectedRelays: func() []api.PairingRelay { return relays },
			RelaySPKI:       func(string) ([]byte, bool) { return nil, false },
		}),
	))
	t.Cleanup(ts.Close)
	return &scanEnv{
		testEnv:    &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture},
		dispatcher: dispatcher,
	}
}

func decodeScans(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var body struct {
		Scans []map[string]any `json:"scans"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode scan response: %v (%s)", err, raw)
	}
	return body.Scans
}

// TestScanAsksEveryConnectedRelay: "scan the network" means every network this
// deployment can see, and each relay is inherently limited to its own segment —
// so a bare scan reaches all of them.
func TestScanAsksEveryConnectedRelay(t *testing.T) {
	e := newScanEnv(t, scanRelayA, scanRelayB)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/discovery/scan", []byte(`{}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST scan = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	asked := e.dispatcher.scanned()
	if len(asked) != 2 {
		t.Fatalf("asked %d relay(s), want 2 — a bare scan reaches every connected relay", len(asked))
	}
	seen := map[string]bool{asked[0].relayID: true, asked[1].relayID: true}
	if !seen[scanRelayA] || !seen[scanRelayB] {
		t.Errorf("asked %v, want both %s and %s", seen, scanRelayA, scanRelayB)
	}
	if scans := decodeScans(t, raw); len(scans) != 2 {
		t.Errorf("reported %d outcome(s), want 2", len(scans))
	}
}

// TestScanCanTargetOneRelay: an operator scanning one segment must not set every
// other relay probing its own.
func TestScanCanTargetOneRelay(t *testing.T) {
	e := newScanEnv(t, scanRelayA, scanRelayB)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/discovery/scan",
		[]byte(`{"relay_id":"`+scanRelayB+`"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST scan = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	asked := e.dispatcher.scanned()
	if len(asked) != 1 || asked[0].relayID != scanRelayB {
		t.Fatalf("asked %+v, want exactly %s", asked, scanRelayB)
	}
}

// TestScanCarriesTheRequestedSubnet pins that the narrowing reaches the wire —
// the relay is the one that validates it against its own policy, so it has to
// arrive there to be refused there.
func TestScanCarriesTheRequestedSubnet(t *testing.T) {
	e := newScanEnv(t, scanRelayA)

	if resp, raw := e.do(t, http.MethodPost, "/api/v1/discovery/scan",
		[]byte(`{"subnet":"192.168.50.0/24"}`), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("POST scan = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	asked := e.dispatcher.scanned()
	if len(asked) != 1 || asked[0].body.Subnet != "192.168.50.0/24" {
		t.Fatalf("relay was asked %+v, want subnet 192.168.50.0/24 on the wire", asked)
	}
}

// TestScanWithNoConnectedRelayIsAConflict: nothing can scan, and saying so is
// more useful than a 200 with an empty list that reads like success.
func TestScanWithNoConnectedRelayIsAConflict(t *testing.T) {
	e := newScanEnv(t) // none connected

	resp, raw := e.do(t, http.MethodPost, "/api/v1/discovery/scan", []byte(`{}`), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST scan with no relay = %d, want 409 (%s)", resp.StatusCode, raw)
	}
	if len(e.dispatcher.scanned()) != 0 {
		t.Errorf("a refused scan still reached a relay")
	}
}

// TestScanReportsARelaysRefusalWithoutFailingTheCall: one relay refusing (its
// policy forbids the subnet, it is not running discovery) must not stop the
// others — the refusal is that relay's outcome, not the operation's.
func TestScanReportsARelaysRefusalWithoutFailingTheCall(t *testing.T) {
	e := newScanEnv(t, scanRelayA)
	e.dispatcher.scanResult = wire.NewDiscoveryScanError("UNAVAILABLE", "this relay is not running discovery, so it cannot scan")

	resp, raw := e.do(t, http.MethodPost, "/api/v1/discovery/scan", []byte(`{}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST scan = %d, want 200 — a relay's refusal is an outcome, not a failed call (%s)", resp.StatusCode, raw)
	}
	scans := decodeScans(t, raw)
	if len(scans) != 1 {
		t.Fatalf("reported %d outcome(s), want 1", len(scans))
	}
	if scans[0]["ok"] != false || scans[0]["code"] != "UNAVAILABLE" {
		t.Errorf("outcome = %+v, want ok=false with the relay's own code", scans[0])
	}
}

// TestScanReportsAnAlreadyRunningScanAsAcceptedNoOp: an operator double-click
// must not double the probe traffic on a segment, and must not look like an
// error either.
func TestScanReportsAnAlreadyRunningScanAsAcceptedNoOp(t *testing.T) {
	e := newScanEnv(t, scanRelayA)
	e.dispatcher.scanResult = wire.NewDiscoveryScanBusy("01J8Z8SCAN0000000000000009")

	resp, raw := e.do(t, http.MethodPost, "/api/v1/discovery/scan", []byte(`{}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST scan = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	scans := decodeScans(t, raw)
	if len(scans) != 1 || scans[0]["ok"] != true || scans[0]["started"] != false {
		t.Fatalf("outcome = %+v, want ok=true started=false (a benign repeat)", scans)
	}
}
