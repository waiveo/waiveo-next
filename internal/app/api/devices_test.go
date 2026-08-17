package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// devices_test.go covers the device plane's api/1 surface against a fake
// dispatcher: the list conventions on both families, the command operation's
// request validation and error mapping, and Idempotency-Key on the command.
// The end-to-end proof over the REAL relay/1 transport lives beside the
// connection server (internal/feeder/relayconn); these are the api-layer
// conventions the transport proof does not re-derive.

// Fixture ids: canonical ULIDs (DAT-005a), chosen so device/entity ids sort in
// a known order for the pagination assertions.
const (
	devRelayA  = "01J8Z4K4N5P6Q7R8S9T0V1W3A1"
	devRelayB  = "01J8Z4K4N5P6Q7R8S9T0V1W3A2"
	devScopeA  = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	devScopeB  = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z6"
	devDevice1 = "01J8Z3K4N5P6Q7R8S9T0V1W2D1"
	devDevice2 = "01J8Z3K4N5P6Q7R8S9T0V1W2D2"
	devEntity1 = "01J8Z3K4N5P6Q7R8S9T0V1W2E1"
	devEntity2 = "01J8Z3K4N5P6Q7R8S9T0V1W2E2"
	devEntity3 = "01J8Z3K4N5P6Q7R8S9T0V1W2E3"
	devMissing = "01J8Z3K4N5P6Q7R8S9T0V1W2ZZ"
)

// fakeDispatcher is a CommandDispatcher test double: it records every dispatch
// and answers with a scripted result or error.
type fakeDispatcher struct {
	mu     sync.Mutex
	calls  []dispatchCall
	result wire.DeviceCommandResultBody
	err    error

	// The scan half: every discovery.scan asked of a relay, and the scripted
	// answer each draws.
	scans      []scanCall
	scanResult wire.DiscoveryScanResultBody
	scanErr    error
}

// scanCall is one recorded discovery.scan request.
type scanCall struct {
	relayID string
	traceID string
	body    wire.DiscoveryScanBody
}

// dispatchCall is one recorded dispatch, including the relay it was routed to
// and the trace id it carried.
type dispatchCall struct {
	relayID string
	traceID string
	body    wire.DeviceCommandBody
}

func (d *fakeDispatcher) SendDeviceCommand(ctx context.Context, relayID, traceID string, body wire.DeviceCommandBody) (wire.DeviceCommandResultBody, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, dispatchCall{relayID: relayID, traceID: traceID, body: body})
	return d.result, d.err
}

func (d *fakeDispatcher) SendDiscoveryScan(ctx context.Context, relayID, traceID string, body wire.DiscoveryScanBody) (wire.DiscoveryScanResultBody, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scans = append(d.scans, scanCall{relayID: relayID, traceID: traceID, body: body})
	return d.scanResult, d.scanErr
}

func (d *fakeDispatcher) scanned() []scanCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]scanCall, len(d.scans))
	copy(out, d.scans)
	return out
}

func (d *fakeDispatcher) dispatched() []dispatchCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]dispatchCall, len(d.calls))
	copy(out, d.calls)
	return out
}

// devicePlaneEnv is a testEnv whose api handler carries a populated device
// registry and a scripted dispatcher.
type devicePlaneEnv struct {
	*testEnv
	registry   *devices.Registry
	dispatcher *fakeDispatcher
}

// newDevicePlaneEnv builds the api handler with a device plane holding two
// devices (one per relay) and three entities — two of them on the SAME device,
// the shape that makes the device-vs-entity distinction observable.
func newDevicePlaneEnv(t *testing.T) *devicePlaneEnv {
	t.Helper()
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	registry := devices.New(devScopeA, func() int64 { return 0 })
	mustPutDevice(t, registry, devices.Device{
		ID: devDevice1, RelayID: devRelayA, DeviceClass: "media-player",
		Name: "Lobby TV", ScopeNode: devScopeA, Labels: map[string]string{"env": "prod"},
	})
	mustPutDevice(t, registry, devices.Device{
		ID: devDevice2, RelayID: devRelayB, DeviceClass: "media-player",
		Name: "Cafe TV", ScopeNode: devScopeB, Labels: map[string]string{"env": "staging"},
	})
	mustPutEntity(t, registry, devices.Entity{
		ID: devEntity1, DeviceID: devDevice1, RelayID: devRelayA, DeviceClass: "media-player",
		Name: "Lobby TV player", ScopeNode: devScopeA, Labels: map[string]string{"env": "prod"}, State: "on",
	})
	mustPutEntity(t, registry, devices.Entity{
		ID: devEntity2, DeviceID: devDevice1, RelayID: devRelayA, DeviceClass: "media-player",
		Name: "Lobby TV power", ScopeNode: devScopeA, Labels: map[string]string{"env": "prod"},
	})
	mustPutEntity(t, registry, devices.Entity{
		ID: devEntity3, DeviceID: devDevice2, RelayID: devRelayB, DeviceClass: "media-player",
		Name: "Cafe TV player", ScopeNode: devScopeB, Labels: map[string]string{"env": "staging"},
	})

	dispatcher := &fakeDispatcher{result: wire.DeviceCommandResultBody{OK: true}}
	clock := func() int64 { return fixedNowMs }
	idem := apihttp.NewIdempotencyStore(clock, 0)
	content := origin.New()
	fixture := newAuthFixture(t)
	ts := httptest.NewServer(api.New(st, idem, clock, ulid.Monotonic(), content, testContentBase, fixture.Auth,
		api.WithDevicePlane(registry, dispatcher)))
	t.Cleanup(ts.Close)

	return &devicePlaneEnv{
		testEnv:    &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture},
		registry:   registry,
		dispatcher: dispatcher,
	}
}

func mustPutDevice(t *testing.T, r *devices.Registry, d devices.Device) {
	t.Helper()
	if err := r.PutDevice(d); err != nil {
		t.Fatalf("PutDevice: %v", err)
	}
}

func mustPutEntity(t *testing.T, r *devices.Registry, e devices.Entity) {
	t.Helper()
	if err := r.PutEntity(e); err != nil {
		t.Fatalf("PutEntity: %v", err)
	}
}

// TestDeviceAndEntityListsAreMounted is the mounting bar: both families answer
// a list request with the api/1 page envelope, carrying every registry row in
// id (keyset) order.
func TestDeviceAndEntityListsAreMounted(t *testing.T) {
	e := newDevicePlaneEnv(t)

	resp, raw := e.do(t, http.MethodGet, "/api/v1/devices", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /devices = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	p := decodePage(t, raw)
	if len(p.Items) != 2 {
		t.Fatalf("devices page carried %d item(s), want 2: %s", len(p.Items), raw)
	}
	if p.Items[0]["id"] != devDevice1 || p.Items[1]["id"] != devDevice2 {
		t.Fatalf("devices are not in id order: %s", raw)
	}
	if p.Items[0]["relay_id"] != devRelayA {
		t.Fatalf("device relay_id = %v, want %s", p.Items[0]["relay_id"], devRelayA)
	}
	if p.Cursor != nil {
		t.Fatalf("cursor = %v on a complete page, want null", *p.Cursor)
	}

	resp, raw = e.do(t, http.MethodGet, "/api/v1/entities", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /entities = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	p = decodePage(t, raw)
	if len(p.Items) != 3 {
		t.Fatalf("entities page carried %d item(s), want 3: %s", len(p.Items), raw)
	}
	if p.Items[0]["device_id"] != devDevice1 || p.Items[1]["device_id"] != devDevice1 {
		t.Fatalf("the first two entities should share device %s: %s", devDevice1, raw)
	}
	if p.Items[0]["state"] != "on" {
		t.Fatalf("entity state = %v, want on", p.Items[0]["state"])
	}
	if _, present := p.Items[1]["state"]; present {
		t.Fatalf("an entity with no reported state should omit `state`: %s", raw)
	}
}

// TestDeviceListsWithoutADevicePlaneServeAnEmptyPage pins the
// mounted-either-way rule: an api handler built without WithDevicePlane still
// answers both list routes — with an empty page, never a 404 for an unmounted
// path.
func TestDeviceListsWithoutADevicePlaneServeAnEmptyPage(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/api/v1/devices", "/api/v1/entities"} {
		resp, raw := e.do(t, http.MethodGet, path, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 (body %s)", path, resp.StatusCode, raw)
		}
		p := decodePage(t, raw)
		if len(p.Items) != 0 || p.Cursor != nil {
			t.Fatalf("GET %s = %s, want an empty page", path, raw)
		}
	}
}

// TestDeviceListPaginationAndSelector proves both list families run the same
// api/1 conventions the store-backed families do: keyset paging through an
// opaque cursor, selector filtering, and the query-parameter failure modes.
func TestDeviceListPaginationAndSelector(t *testing.T) {
	e := newDevicePlaneEnv(t)

	// Page 1 of 3 entities at limit=2 yields a cursor; page 2 completes the set
	// with no repeats and no skips (API-032/034).
	resp, raw := e.do(t, http.MethodGet, "/api/v1/entities?limit=2", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /entities?limit=2 = %d (body %s)", resp.StatusCode, raw)
	}
	first := decodePage(t, raw)
	if len(first.Items) != 2 || first.Cursor == nil {
		t.Fatalf("first page = %s, want 2 items and a cursor", raw)
	}
	resp, raw = e.do(t, http.MethodGet, "/api/v1/entities?limit=2&cursor="+*first.Cursor, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /entities page 2 = %d (body %s)", resp.StatusCode, raw)
	}
	second := decodePage(t, raw)
	if len(second.Items) != 1 || second.Cursor != nil {
		t.Fatalf("second page = %s, want the final item and a null cursor", raw)
	}
	if second.Items[0]["id"] != devEntity3 {
		t.Fatalf("second page item = %v, want %s", second.Items[0]["id"], devEntity3)
	}

	// A cursor minted by the entities list is meaningless on the devices list
	// and is refused rather than silently paged from (API-033/035).
	resp, raw = e.do(t, http.MethodGet, "/api/v1/devices?cursor="+*first.Cursor, nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("cross-resource cursor = %d, want 400 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "CURSOR_INVALID")

	// A selector filters on the row's own labels and scope-node placement.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/devices?selector=env%3Dstaging", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("selector list = %d (body %s)", resp.StatusCode, raw)
	}
	p := decodePage(t, raw)
	if len(p.Items) != 1 || p.Items[0]["id"] != devDevice2 {
		t.Fatalf("selector env=staging matched %s, want only %s", raw, devDevice2)
	}

	resp, raw = e.do(t, http.MethodGet, "/api/v1/entities?selector=scope_node%3D"+devScopeB, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scope_node selector list = %d (body %s)", resp.StatusCode, raw)
	}
	p = decodePage(t, raw)
	if len(p.Items) != 1 || p.Items[0]["id"] != devEntity3 {
		t.Fatalf("scope_node selector matched %s, want only %s", raw, devEntity3)
	}

	// API-031: an out-of-range limit is a QUERY-PARAMETER failure — 400, not
	// the 422 a body failure carries (API-013a).
	resp, raw = e.do(t, http.MethodGet, "/api/v1/entities?limit=0", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("limit=0 = %d, want 400 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")

	// API-045: an unparseable selector is 400 / SELECTOR_INVALID.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/devices?selector=env%20%3D%20prod", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed selector = %d, want 400 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "SELECTOR_INVALID")
}

// TestSendEntityCommandRoutesToTheOwningRelay is the command operation's happy
// path: the addressed entity resolves to its own relay, the dispatch carries
// the entity id and the request's trace id, and the relay's result is returned.
func TestSendEntityCommandRoutesToTheOwningRelay(t *testing.T) {
	e := newDevicePlaneEnv(t)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/entities/"+devEntity3+"/commands",
		mustJSON(t, map[string]any{"command": "launch", "params": map[string]any{"channel": "dev"}}),
		map[string]string{"Trace-Id": "01J8Z4K4N5P6Q7R8S9T0V1W3B0"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST command = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result["ok"] != true {
		t.Fatalf("result = %s, want {ok:true}", raw)
	}

	calls := e.dispatcher.dispatched()
	if len(calls) != 1 {
		t.Fatalf("dispatcher saw %d call(s), want 1", len(calls))
	}
	// devEntity3 belongs to the device on relay B: the command must not be sent
	// to relay A just because it is another live relay.
	if calls[0].relayID != devRelayB {
		t.Fatalf("dispatched to relay %s, want %s — the command went to the wrong relay", calls[0].relayID, devRelayB)
	}
	if calls[0].body.EntityID != devEntity3 || calls[0].body.Command != "launch" {
		t.Fatalf("dispatched %+v, want launch against %s", calls[0].body, devEntity3)
	}
	if calls[0].body.Params["channel"] != "dev" {
		t.Fatalf("dispatched params = %v, want channel=dev", calls[0].body.Params)
	}
	if calls[0].traceID != "01J8Z4K4N5P6Q7R8S9T0V1W3B0" {
		t.Fatalf("dispatched trace_id = %q, want the request's own (REL-006)", calls[0].traceID)
	}
}

// TestSendEntityCommandReturnsTheRelaysTypedRefusal pins the 200-with-ok:false
// shape: a relay that refuses the command has ANSWERED, so the exchange is a
// success carrying the relay's own taxonomy code, not an api/1 Problem.
func TestSendEntityCommandReturnsTheRelaysTypedRefusal(t *testing.T) {
	e := newDevicePlaneEnv(t)
	e.dispatcher.result = wire.NewDeviceCommandError("COMMAND_UNRESOLVED", `"blast" is not a command media-player declares`)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/entities/"+devEntity1+"/commands",
		mustJSON(t, map[string]any{"command": "blast"}), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refused command = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	var result struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.OK || result.Error == nil || result.Error.Code != "COMMAND_UNRESOLVED" {
		t.Fatalf("result = %s, want {ok:false, COMMAND_UNRESOLVED}", raw)
	}
}

// TestSendEntityCommandUnknownEntityIs404 proves the target is resolved before
// anything is dispatched, and that the Problem names the resource by its own
// noun.
func TestSendEntityCommandUnknownEntityIs404(t *testing.T) {
	e := newDevicePlaneEnv(t)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/entities/"+devMissing+"/commands",
		mustJSON(t, map[string]any{"command": "home"}), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown entity = %d, want 404 (body %s)", resp.StatusCode, raw)
	}
	problem := assertProblem(t, resp, raw, "NOT_FOUND")
	if detail, _ := problem["detail"].(string); detail != "No entity exists with this identifier." {
		t.Fatalf("detail = %q, want the entity's own display name", detail)
	}
	if n := len(e.dispatcher.dispatched()); n != 0 {
		t.Fatalf("dispatcher saw %d call(s) for an unknown entity, want 0", n)
	}
}

// TestSendEntityCommandBodyValidation covers API-013a's body half: every
// rejection here is 422, never the 400 a query-parameter failure carries. The
// unknown-member rejection is what enforces the schema's own
// `additionalProperties: false` at runtime — and what stops a client smuggling
// an `id` or `entity_id` into the body to redirect the dispatch.
func TestSendEntityCommandBodyValidation(t *testing.T) {
	e := newDevicePlaneEnv(t)
	path := "/api/v1/entities/" + devEntity1 + "/commands"

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"unparseable", []byte(`{"command":`)},
		{"not an object", []byte(`"launch"`)},
		{"command absent", []byte(`{"params":{}}`)},
		{"command blank", []byte(`{"command":"  "}`)},
		{"client-supplied id", []byte(`{"command":"home","id":"01J8Z3K4N5P6Q7R8S9T0V1W2E2"}`)},
		{"client-supplied entity_id", []byte(`{"command":"home","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2E2"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := e.do(t, http.MethodPost, path, tc.body, nil)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("%s = %d, want 422 (body %s)", tc.name, resp.StatusCode, raw)
			}
			assertProblem(t, resp, raw, "VALIDATION_FAILED")
		})
	}
	if n := len(e.dispatcher.dispatched()); n != 0 {
		t.Fatalf("dispatcher saw %d call(s) for rejected bodies, want 0", n)
	}
}

// TestSendEntityCommandRelayOfflineIs503 is the never-silently-drop rule at the
// api boundary: a relay with no live connection produces a retryable
// UNAVAILABLE Problem, not a fabricated ok:false result.
func TestSendEntityCommandRelayOfflineIs503(t *testing.T) {
	e := newDevicePlaneEnv(t)
	e.dispatcher.err = feederrelayconn.ErrRelayNotConnected

	resp, raw := e.do(t, http.MethodPost, "/api/v1/entities/"+devEntity1+"/commands",
		mustJSON(t, map[string]any{"command": "home"}), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("offline relay = %d, want 503 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "UNAVAILABLE")
}

// TestSendEntityCommandWithoutADispatcherIs503 covers the other unavailable
// shape: the entity is known but no connection plane exists to carry a command
// to it.
func TestSendEntityCommandWithoutADispatcherIs503(t *testing.T) {
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	registry := devices.New(devScopeA, func() int64 { return 0 })
	mustPutEntity(t, registry, devices.Entity{
		ID: devEntity1, DeviceID: devDevice1, RelayID: devRelayA,
		DeviceClass: "media-player", Name: "Lobby TV player", ScopeNode: devScopeA,
	})
	clock := func() int64 { return fixedNowMs }
	fixture := newAuthFixture(t)
	ts := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock,
		ulid.Monotonic(), origin.New(), testContentBase, fixture.Auth, api.WithDevicePlane(registry, nil)))
	t.Cleanup(ts.Close)
	e := &testEnv{ts: ts, store: st, content: origin.New(), contentBase: testContentBase, auth: fixture}

	resp, raw := e.do(t, http.MethodPost, "/api/v1/entities/"+devEntity1+"/commands",
		mustJSON(t, map[string]any{"command": "home"}), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no dispatcher = %d, want 503 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "UNAVAILABLE")
}

// TestSendEntityCommandTimeoutIs503 maps a relay that never answers onto the
// retryable UNAVAILABLE code rather than a 500.
func TestSendEntityCommandTimeoutIs503(t *testing.T) {
	e := newDevicePlaneEnv(t)
	e.dispatcher.err = context.DeadlineExceeded

	resp, raw := e.do(t, http.MethodPost, "/api/v1/entities/"+devEntity1+"/commands",
		mustJSON(t, map[string]any{"command": "home"}), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unanswered command = %d, want 503 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "UNAVAILABLE")
}

// TestSendEntityCommandRefusalIsInternal covers the remaining transport
// failure: a relay that refuses the request frame itself is an app-peer-side
// defect the client cannot correct, so it is a 500, not a retryable 503.
func TestSendEntityCommandRefusalIsInternal(t *testing.T) {
	e := newDevicePlaneEnv(t)
	e.dispatcher.err = &feederrelayconn.RefusalError{Code: "MALFORMED_MESSAGE", Message: "device.command body did not decode"}

	resp, raw := e.do(t, http.MethodPost, "/api/v1/entities/"+devEntity1+"/commands",
		mustJSON(t, map[string]any{"command": "home"}), nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("refused frame = %d, want 500 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "INTERNAL")
}

// TestSendEntityCommandIdempotencyKeyReplays proves a retry-on-timeout cannot
// fire the same command at the physical device twice: the retained response is
// replayed verbatim and the dispatcher is called exactly once (API-050/052).
func TestSendEntityCommandIdempotencyKeyReplays(t *testing.T) {
	e := newDevicePlaneEnv(t)
	path := "/api/v1/entities/" + devEntity1 + "/commands"
	body := mustJSON(t, map[string]any{"command": "home"})
	headers := map[string]string{"Idempotency-Key": "retry-after-timeout"}

	resp, first := e.do(t, http.MethodPost, path, body, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first command = %d, want 200 (body %s)", resp.StatusCode, first)
	}
	resp, second := e.do(t, http.MethodPost, path, body, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replayed command = %d, want 200 (body %s)", resp.StatusCode, second)
	}
	if string(first) != string(second) {
		t.Fatalf("replay body %s does not match the original %s", second, first)
	}
	if n := len(e.dispatcher.dispatched()); n != 1 {
		t.Fatalf("dispatcher saw %d call(s) across a keyed retry, want 1 — the device was commanded twice", n)
	}

	// A different body under the same key is a reuse conflict, and still does
	// not dispatch (API-053).
	resp, raw := e.do(t, http.MethodPost, path, mustJSON(t, map[string]any{"command": "power"}), headers)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("key reuse = %d, want 409 (body %s)", resp.StatusCode, raw)
	}
	if n := len(e.dispatcher.dispatched()); n != 1 {
		t.Fatalf("dispatcher saw %d call(s) after a key-reuse conflict, want 1", n)
	}
}

// TestRegistryRejectsMalformedIdentifiers pins DAT-005a at the registry
// boundary — a row whose own id (or an entity's device_id) is not a canonical
// ULID is refused and not stored, so it can never become an out-of-order page
// boundary — and pins the deliberate exception: relay_id is minted by the
// enrollment path (relay/1 REL-012/014), is NOT a ULID in the running system,
// and is required only to be present.
func TestRegistryRejectsMalformedIdentifiers(t *testing.T) {
	r := devices.New(devScopeA, func() int64 { return 0 })
	if err := r.PutDevice(devices.Device{ID: "not-a-ulid", RelayID: devRelayA}); err == nil {
		t.Fatal("PutDevice accepted a non-ULID id")
	}
	if err := r.PutDevice(devices.Device{ID: devDevice1}); err == nil {
		t.Fatal("PutDevice accepted a device with no relay_id — it would be uncommandable")
	}
	if err := r.PutEntity(devices.Entity{ID: devEntity1, DeviceID: "dev-1", RelayID: devRelayA}); err == nil {
		t.Fatal("PutEntity accepted a non-ULID device_id")
	}
	if err := r.PutEntity(devices.Entity{ID: devEntity1, DeviceID: devDevice1}); err == nil {
		t.Fatal("PutEntity accepted an entity with no relay_id")
	}
	if n := len(r.Devices()) + len(r.Entities()); n != 0 {
		t.Fatalf("registry holds %d row(s) after rejected writes, want 0", n)
	}

	// An enrollment-shaped relay id — the form the enrollment path actually
	// mints — is accepted, since relay_id is not a row id of this platform's.
	mustPutDevice(t, r, devices.Device{ID: devDevice2, RelayID: "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0"})

	// The accepting path still works, and reads back in id order.
	mustPutEntity(t, r, devices.Entity{ID: devEntity2, DeviceID: devDevice1, RelayID: devRelayA})
	mustPutEntity(t, r, devices.Entity{ID: devEntity1, DeviceID: devDevice1, RelayID: devRelayA})
	got := r.Entities()
	if len(got) != 2 || got[0].ID != devEntity1 || got[1].ID != devEntity2 {
		t.Fatalf("Entities() = %+v, want %s then %s", got, devEntity1, devEntity2)
	}
	if _, ok := r.Entity(devMissing); ok {
		t.Fatal("Entity() resolved an id that was never registered")
	}
}
