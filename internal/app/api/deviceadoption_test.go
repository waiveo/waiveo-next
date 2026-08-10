package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// deviceadoption_test.go covers the two halves of the device plane that make a
// discovered TV actionable from the console: the list carrying an ADDRESS an
// operator (and a command) can reach it at, and the adopt operation that files
// the durable record the relay is then sent.
//
// Everything here drives the real HTTP surface against the real store — the
// adopt path writes an authored row, and a test that stubbed the store would
// prove only that a handler was called.

const (
	adoptDeviceID  = "01J8Z3AD0PTDEV1CE0000000A1"
	adoptDeviceID2 = "01J8Z3AD0PTDEV1CE0000000B2"
	adoptRelayID   = "relay-adopt-fixture"
)

// seedAdoptable places a device in the read model at a REAL scope node and
// mirrors it durably, which is the state a relay's report leaves behind. It
// returns the node so a caller can assert placement-scoped behaviour.
func seedAdoptable(t *testing.T, e *devicePlaneEnv, deviceID, nativeID string) string {
	t.Helper()
	node := e.mintOrgNode(t)
	mustPutDevice(t, e.registry, devices.Device{
		ID: deviceID, RelayID: adoptRelayID, DeviceClass: "media-player",
		Name: "Lobby TV", ScopeNode: node, Labels: map[string]string{},
		Address: "192.168.50.31:8060", Model: "Roku Ultra", Serial: "X00500ABC123",
	})
	if err := e.store.ReplaceDiscoveredDevices(context.Background(), adoptRelayID, []store.DiscoveredDevice{{
		DeviceID: deviceID, RelayID: adoptRelayID, ScopeNode: node,
		Driver: "roku-ecp", NativeID: nativeID, DeviceClass: "media-player",
		Name: "Lobby TV", Address: "192.168.50.31:8060", Model: "Roku Ultra", Serial: "X00500ABC123",
		FirstSeen: 1000, LastSeen: 2000,
		Entities: []wire.CandidateEntity{{Key: "main", DeviceClass: "media-player"}},
	}}); err != nil {
		t.Fatalf("mirror the discovered device: %v", err)
	}
	return node
}

// mintOrgNode creates the org node a fixture row is placed at, through the API
// itself — a row's scope_node is a reference the store resolves, so a fixture
// placed at an id no node carries would be refused for its PLACEMENT and the
// case would pass on the wrong refusal.
func (e *devicePlaneEnv) mintOrgNode(t *testing.T) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes",
		mustJSON(t, map[string]any{"kind": "org", "name": "Adopt Fixture Org", "account_state": "active", "entitlements": map[string]any{}}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint org node: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// TestDeviceListCarriesTheDiscoveredAddress is the API-visible end of the
// address capture: an operator's device list must show WHERE each device is,
// because a row with no address is one nothing can ever command.
func TestDeviceListCarriesTheDiscoveredAddress(t *testing.T) {
	e := newDevicePlaneEnv(t)
	seedAdoptable(t, e, adoptDeviceID, "uuid:roku:ecp:X1")

	resp, raw := e.do(t, http.MethodGet, "/api/v1/devices", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /devices = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var row map[string]any
	for _, item := range decodePage(t, raw).Items {
		if item["id"] == adoptDeviceID {
			row = item
		}
	}
	if row == nil {
		t.Fatalf("the seeded device is not in the list: %s", raw)
	}
	if got := row["address"]; got != "192.168.50.31:8060" {
		t.Errorf("address = %v, want 192.168.50.31:8060", got)
	}
	if got := row["model"]; got != "Roku Ultra" {
		t.Errorf("model = %v, want Roku Ultra", got)
	}
	if got := row["serial"]; got != "X00500ABC123" {
		t.Errorf("serial = %v, want X00500ABC123", got)
	}
	if got := row["adopted"]; got != false {
		t.Errorf("adopted = %v, want false — discovery alone never adopts", got)
	}

	// A device discovered with no usable address must OMIT the member rather
	// than serve an empty string: absent says "the relay found it and cannot
	// reach it", which is a different and honest answer.
	for _, item := range decodePage(t, raw).Items {
		if item["id"] != devDevice1 {
			continue
		}
		if _, present := item["address"]; present {
			t.Errorf("a device discovered with no address carried `address`: %v", item["address"])
		}
	}
}

// TestAdoptDeviceFilesTheAdoptionRecord is the operation's real subject: not
// the 200, but the durable row that reaches the relay in desired state.
func TestAdoptDeviceFilesTheAdoptionRecord(t *testing.T) {
	e := newDevicePlaneEnv(t)
	seedAdoptable(t, e, adoptDeviceID, "uuid:roku:ecp:X1")

	resp, raw := e.do(t, http.MethodPost, "/api/v1/devices/"+adoptDeviceID+"/adopt", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST adopt = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode adopt response: %v", err)
	}
	if body["id"] != adoptDeviceID {
		t.Errorf("response id = %v, want the adopted device %s", body["id"], adoptDeviceID)
	}
	if body["adopted"] != true {
		t.Errorf("response adopted = %v, want true", body["adopted"])
	}

	// The durable half. An adopt that only flipped an in-memory flag would pass
	// every assertion above and control nothing.
	res, ok, err := e.store.Get(context.Background(), store.KindAdoptedDevice, adoptDeviceID)
	if err != nil || !ok {
		t.Fatalf("adopted row not written (ok=%v, err=%v)", ok, err)
	}
	var row datamodel.Device
	if err := json.Unmarshal(res.Body, &row); err != nil {
		t.Fatalf("decode adopted row: %v", err)
	}
	if row.Driver != "roku-ecp" || row.NativeID != "uuid:roku:ecp:X1" {
		t.Errorf("adopted identity = %q/%q, want the discovered tuple", row.Driver, row.NativeID)
	}

	// And the list now reports it, which is what the console reads.
	_, listRaw := e.do(t, http.MethodGet, "/api/v1/devices", nil, nil)
	for _, item := range decodePage(t, listRaw).Items {
		if item["id"] == adoptDeviceID && item["adopted"] != true {
			t.Errorf("listed adopted = %v after adopting, want true", item["adopted"])
		}
	}
}

// TestAdoptDeviceIsIdempotent: an operator double-click, or a client retrying a
// request whose response was lost, must not be an error.
func TestAdoptDeviceIsIdempotent(t *testing.T) {
	e := newDevicePlaneEnv(t)
	seedAdoptable(t, e, adoptDeviceID, "uuid:roku:ecp:X1")

	for i := range 2 {
		resp, raw := e.do(t, http.MethodPost, "/api/v1/devices/"+adoptDeviceID+"/adopt", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("adopt #%d = %d, want 200 (%s)", i+1, resp.StatusCode, raw)
		}
	}
	rows, err := e.store.List(context.Background(), store.KindAdoptedDevice, store.ListFilter{})
	if err != nil {
		t.Fatalf("List adopted devices: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("adopted rows = %d after two adopts, want 1", len(rows))
	}
}

// TestAdoptDeviceHonorsIdempotencyKey pins API-050/052 on this operation: a
// retry under the same key replays the recorded response rather than re-running
// the write.
func TestAdoptDeviceHonorsIdempotencyKey(t *testing.T) {
	e := newDevicePlaneEnv(t)
	seedAdoptable(t, e, adoptDeviceID, "uuid:roku:ecp:X1")

	headers := map[string]string{"Idempotency-Key": "adopt-key-1"}
	resp1, raw1 := e.do(t, http.MethodPost, "/api/v1/devices/"+adoptDeviceID+"/adopt", nil, headers)
	resp2, raw2 := e.do(t, http.MethodPost, "/api/v1/devices/"+adoptDeviceID+"/adopt", nil, headers)
	if resp1.StatusCode != http.StatusOK || resp2.StatusCode != http.StatusOK {
		t.Fatalf("adopt statuses = %d/%d, want 200/200 (%s | %s)", resp1.StatusCode, resp2.StatusCode, raw1, raw2)
	}
	if string(raw1) != string(raw2) {
		t.Errorf("replayed body differs:\n first: %s\nsecond: %s", raw1, raw2)
	}
}

// TestAdoptUnknownDeviceIs404: an id no relay has reported names nothing, and
// the answer must be the same 404 any unresolvable identifier draws.
func TestAdoptUnknownDeviceIs404(t *testing.T) {
	e := newDevicePlaneEnv(t)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/devices/"+devMissing+"/adopt", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("adopt unknown = %d, want 404 (%s)", resp.StatusCode, raw)
	}
	rows, err := e.store.List(context.Background(), store.KindAdoptedDevice, store.ListFilter{})
	if err != nil {
		t.Fatalf("List adopted devices: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused adopt wrote %d row(s), want 0", len(rows))
	}
}

// TestAdoptADeviceWithNoDurableMirrorIsRetryable covers the narrow window
// between a relay's first report reaching the read model and the mirror write
// landing. The device demonstrably exists — the caller just read it out of the
// list — so reporting it absent would be a lie a client would cache.
func TestAdoptADeviceWithNoDurableMirrorIsRetryable(t *testing.T) {
	e := newDevicePlaneEnv(t)
	node := e.mintOrgNode(t)
	mustPutDevice(t, e.registry, devices.Device{
		ID: adoptDeviceID2, RelayID: adoptRelayID, DeviceClass: "media-player",
		Name: "Unmirrored TV", ScopeNode: node, Labels: map[string]string{},
	})

	resp, raw := e.do(t, http.MethodPost, "/api/v1/devices/"+adoptDeviceID2+"/adopt", nil, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("adopt un-mirrored = %d, want 503 (%s)", resp.StatusCode, raw)
	}
}
