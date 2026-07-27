// apicommand_e2e_test.go is the device-command plane's whole-stack proof: an
// HTTP request to the REAL api/1 handler travels the REAL relay/1 persistent
// connection and comes back with the relay's own answer. Nothing between the
// two is simulated —
//
//	http.Client
//	  -> internal/app/api (the real handler, real routes, real conventions)
//	    -> internal/feeder/relayconn (the real connection server)
//	      == the real authenticated WS connection established by enrollment
//	         + challenge/hello/hello-ack ==
//	    -> internal/relay/relayconn (the real dialer's inbound handler)
//	  -> internal/relay/automationhost + internal/relay/deviceplane (the real
//	     command surface, over the canonical device-class registry)
//	-> the physical-device adapter
//
// The api-layer conventions (pagination, selectors, validation, idempotency,
// error mapping) are covered against a fake dispatcher in internal/app/api;
// this file exists to prove the seam BETWEEN the layers, which no
// single-layer test can.
package relayconn_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// apiFixedNowMs is the injected clock the api layer's idempotency store is
// timestamped with — the api layer reads no wall clock of its own, and neither
// does this test.
const apiFixedNowMs int64 = 1752537600000

// apiContentBase is the content-origin base URL the api handler is built with.
// The device plane never touches it; it is a required constructor argument.
const apiContentBase = "https://origin.invalid"

// commandResponse is the openapi EntityCommandResult shape as the caller sees
// it over HTTP.
type commandResponse struct {
	OK    bool `json:"ok"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// newAPIOverConnection builds the app peer's api/1 handler wired to the SAME
// connection server the relay is connected to, with a device registry naming
// the connected relay as the owner of the fixture entity. The returned server
// is the caller's HTTP door into the whole stack.
func newAPIOverConnection(t *testing.T, h *harness, relayID string) *httptest.Server {
	t.Helper()

	registry := devices.New()
	if err := registry.PutDevice(devices.Device{
		ID: fixtureDevice1, RelayID: relayID, DeviceClass: fixtureDevClass,
		Name: "Lobby TV", ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
	}); err != nil {
		t.Fatalf("PutDevice: %v", err)
	}
	if err := registry.PutEntity(devices.Entity{
		ID: fixtureEntityA, DeviceID: fixtureDevice1, RelayID: relayID,
		DeviceClass: fixtureDevClass, Name: "Lobby TV player",
		ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
	}); err != nil {
		t.Fatalf("PutEntity: %v", err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clock := func() int64 { return apiFixedNowMs }
	// api.New requires an authenticator: the surface refuses a request whose
	// principal cannot be resolved (security-model/1 SEC-005), so this
	// whole-stack test drives a real seeded principal rather than an
	// unauthenticated request the shipped handler would reject.
	fixture, err := authtest.New(authtest.Config{NowMs: clock})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	t.Cleanup(fixture.Close)
	apiAuth = fixture
	handler := api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, ulid.Monotonic(),
		origin.New(), apiContentBase, fixture.Auth,
		// The connection server IS the dispatcher: no adapter, no shim — the
		// api layer's CommandDispatcher is exactly its SendDeviceCommand.
		api.WithDevicePlane(registry, h.connSrv))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// apiAuth is the fixture the most recently started api server was mounted with;
// postCommand presents its credential. A package-level handoff keeps the two
// helpers' existing signatures, which every case in this file already calls.
var apiAuth *authtest.Fixture

// postCommand issues the real HTTP request an operator's client would.
func postCommand(t *testing.T, ts *httptest.Server, entityID, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/entities/"+entityID+"/commands", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	apiAuth.Authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST command: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, raw
}

// TestEntityCommandTravelsFromTheAPIToTheDevice is the whole-stack proof: a
// command POSTed to /api/v1/entities/{id}/commands reaches the physical-device
// adapter behind the connected relay, and its result is observable to the HTTP
// caller.
func TestEntityCommandTravelsFromTheAPIToTheDevice(t *testing.T) {
	h := newHarness(t)
	_, controller, relayID := connectedDevicePlane(t, h)
	ts := newAPIOverConnection(t, h, relayID)

	resp, raw := postCommand(t, ts, fixtureEntityA,
		`{"command":"launch","params":{"channel":"dev"}}`,
		map[string]string{"Trace-Id": "01J8Z4K4N5P6Q7R8S9T0V1W3B0"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST command = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	var result commandResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode result: %v (body %s)", err, raw)
	}
	if !result.OK || result.Error != nil {
		t.Fatalf("result = %s, want {ok:true}", raw)
	}
	if got := resp.Header.Get("Trace-Id"); got != "01J8Z4K4N5P6Q7R8S9T0V1W3B0" {
		t.Fatalf("Trace-Id response header = %q, want the request's own", got)
	}

	// The command genuinely reached the device behind the relay — not a
	// fabricated ok from anywhere in between.
	calls := controller.dispatched()
	if len(calls) != 1 {
		t.Fatalf("the device adapter saw %d dispatch(es), want 1: %+v", len(calls), calls)
	}
	if calls[0].EntityID != fixtureEntityA || calls[0].Command != "launch" {
		t.Fatalf("device adapter saw %+v, want launch against %s", calls[0], fixtureEntityA)
	}
	if calls[0].Params["channel"] != "dev" {
		t.Fatalf("device adapter saw params %v, want channel=dev — params did not survive the full path", calls[0].Params)
	}

	// The relay's own typed refusal surfaces to the HTTP caller intact, and the
	// device is never touched for it (REL-113).
	resp, raw = postCommand(t, ts, fixtureEntityA, `{"command":"blast"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refused command = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode refusal: %v (body %s)", err, raw)
	}
	if result.OK || result.Error == nil || result.Error.Code != "COMMAND_UNRESOLVED" {
		t.Fatalf("result = %s, want {ok:false, COMMAND_UNRESOLVED}", raw)
	}
	if n := len(controller.dispatched()); n != 1 {
		t.Fatalf("the device adapter saw %d dispatch(es) after an unresolved command, want the original 1", n)
	}

	// Every exchange completed: no correlation entry survives.
	if n := h.connSrv.PendingCommandCount(); n != 0 {
		t.Fatalf("PendingCommandCount = %d after two completed exchanges, want 0", n)
	}
}

// TestEntityCommandIsRefusedWhenTheRelayDisconnects proves the offline path all
// the way out to HTTP: once the relay's connection is gone, the command is
// refused with a retryable UNAVAILABLE Problem rather than dropped or answered
// with a fabricated result.
func TestEntityCommandIsRefusedWhenTheRelayDisconnects(t *testing.T) {
	h := newHarness(t)
	client, controller, relayID := connectedDevicePlane(t, h)
	ts := newAPIOverConnection(t, h, relayID)

	_ = client.Close()
	waitFor(t, 10*time.Second, func() bool { return h.connSrv.ConnCount() == 0 },
		"the app peer never dropped the closed connection")

	resp, raw := postCommand(t, ts, fixtureEntityA, `{"command":"home"}`, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("command to a disconnected relay = %d, want 503 (body %s)", resp.StatusCode, raw)
	}
	var problem map[string]any
	if err := json.Unmarshal(raw, &problem); err != nil {
		t.Fatalf("decode problem: %v (body %s)", err, raw)
	}
	if problem["code"] != "UNAVAILABLE" {
		t.Fatalf("problem code = %v, want UNAVAILABLE (body %s)", problem["code"], raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != apihttp.ProblemContentType {
		t.Fatalf("Content-Type = %q, want %q", ct, apihttp.ProblemContentType)
	}
	if n := len(controller.dispatched()); n != 0 {
		t.Fatalf("the device adapter saw %d dispatch(es) while the relay was offline, want 0", n)
	}
}
