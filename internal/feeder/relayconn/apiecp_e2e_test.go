// apiecp_e2e_test.go extends the device-command whole-stack proof by one link:
// the last one. Its sibling apicommand_e2e_test.go carries an HTTP request all
// the way to a *fake* DeviceController and asserts the seams in between; this
// file replaces that fake with the REAL ECP adapter (internal/relay/ecp) over
// the REAL adoption gate (internal/relay/devicetargets), and asserts that an
// HTTP POST an operator's console would issue ends as an HTTP request on a
// server standing in for a Roku.
//
//	http.Client
//	  -> internal/app/api                      (real handler)
//	    -> internal/feeder/relayconn           (real connection server)
//	      == real authenticated WS connection ==
//	    -> internal/relay/relayconn            (real inbound handler)
//	      -> internal/relay/automationhost     (real command surface)
//	        -> internal/relay/devicetargets    (real adoption gate)
//	          -> internal/relay/ecp            (real ECP adapter)
//	            -> an HTTP server that is, as far as the stack knows, a Roku
//
// Every link in that chain existed and was tested before this track, and an
// operator's command still reached nothing, because the two halves were never
// joined. The two cases here are the joined statement — the command lands, and
// a device nobody adopted is not touched — which is exactly the pair no
// single-layer test can make.
package relayconn_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/devicetargets"
	"github.com/maaxton/waiveo-next/internal/relay/ecp"
	relayclient "github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// ecpRecorder is an HTTP server standing in for a Roku's ECP surface.
type ecpRecorder struct {
	srv *httptest.Server

	mu    sync.Mutex
	paths []string
}

func newECPRecorder(t *testing.T) *ecpRecorder {
	t.Helper()
	r := &ecpRecorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.paths = append(r.paths, req.Method+" "+req.URL.EscapedPath())
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *ecpRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

// adoptedInventory is the `device_inventory.devices` array (REL-063) the app
// peer ships down when an operator adopts entityID.
func adoptedInventory(t *testing.T, entityID string) []json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(wire.DeviceEntry{
		DeviceID: fixtureDevice1,
		Driver:   "roku-ecp",
		NativeID: "uuid:roku:ecp:E2E",
		Entities: []wire.DeviceEntity{{
			EntityID:    entityID,
			DeviceClass: fixtureDevClass,
			Enabled:     true,
			DisplayName: "Lobby TV player",
			Category:    "primary",
		}},
	})
	if err != nil {
		t.Fatalf("marshaling the adopted device entry: %v", err)
	}
	return []json.RawMessage{raw}
}

// ecpAddresses is the relay's discovered address book, as
// devicetargets.AddressSource sees it — here holding exactly the fake Roku.
type ecpAddresses struct{ addr string }

func (a ecpAddresses) AddressFor(driver, nativeID string) (string, bool) {
	if driver != "roku-ecp" || nativeID != "uuid:roku:ecp:E2E" {
		return "", false
	}
	return a.addr, true
}

// connectRelayWithECP enrolls a relay, builds its real automation host over the
// real ECP adapter reading targets through the real adoption gate, and dials it
// onto the app peer with the host as the inbound device.command handler — the
// production wiring, with an httptest server where the Roku would be.
func connectRelayWithECP(t *testing.T, h *harness, roku *ecpRecorder) (*devicetargets.Registry, string) {
	t.Helper()
	store := enrolledRelay(t, h)
	id, _, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}

	targets := devicetargets.New(nil, ecpAddresses{addr: roku.srv.URL + "/"})
	controller := deviceplane.DeviceController(ecp.New(nil, ecp.WithTargetSource(
		func(entityID string) (ecp.Target, bool) {
			ep, ok := targets.Target(entityID)
			if !ok {
				return ecp.Target{}, false
			}
			return ecp.Target{Host: ep.Host, Port: ep.Port}, true
		})))

	host, err := automationhost.New(store, deviceclass.Builtin(), controller, fixtureResolver, id.RelayID)
	if err != nil {
		t.Fatalf("automationhost.New: %v", err)
	}
	client, err := relayclient.Dial(relayclient.Config{
		URL:             h.ts.URL,
		Store:           store,
		Declaration:     testDeclaration,
		OnDeviceCommand: host.DeviceCommand,
	})
	if err != nil {
		t.Fatalf("relayconn.Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() >= 1 },
		"authenticated connection never registered with the app peer")
	return targets, id.RelayID
}

// TestAPICommandReachesTheDeviceOverECP is the end of the chain: a real HTTP
// POST to /api/v1/entities/{id}/commands returns 200 {"ok":true} AND the device
// received the matching ECP request. Both halves are asserted, because either
// alone is the failure this track existed to fix — a 200 with nothing sent is
// exactly what the relay used to do (a typed refusal, but the same net effect:
// an operator watching an unchanged screen).
func TestAPICommandReachesTheDeviceOverECP(t *testing.T) {
	h := newHarness(t)
	roku := newECPRecorder(t)
	targets, relayID := connectRelayWithECP(t, h, roku)
	targets.SetInventory(adoptedInventory(t, fixtureEntityA))

	ts := newAPIOverConnection(t, h, relayID)

	resp, body := postCommand(t, ts, fixtureEntityA,
		`{"command":"launch","params":{"channel":"782875"}}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST commands = %d, want 200: %s", resp.StatusCode, body)
	}
	var got commandResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decoding EntityCommandResult: %v (%s)", err, body)
	}
	if !got.OK || got.Error != nil {
		t.Fatalf("result = %+v, want ok:true", got)
	}
	if seen := roku.seen(); len(seen) != 1 || seen[0] != "POST /launch/782875" {
		t.Fatalf("device saw %v, want [POST /launch/782875] — a 200 the hardware never heard is the defect this proves closed", seen)
	}
}

// TestAPICommandForAnUnadoptedEntityIsRefused is the guardrail at the operator
// door. The entity resolves (the relay knows it), so this is not a
// "no such entity" answer — it is the adoption gate refusing to drive a device
// nobody adopted, and the device must receive nothing at all. On a LAN shared
// with the legacy stack, a command sent here is two controllers on one TV.
func TestAPICommandForAnUnadoptedEntityIsRefused(t *testing.T) {
	h := newHarness(t)
	roku := newECPRecorder(t)
	_, relayID := connectRelayWithECP(t, h, roku) // no SetInventory: nothing adopted

	ts := newAPIOverConnection(t, h, relayID)

	resp, body := postCommand(t, ts, fixtureEntityA, `{"command":"home"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST commands = %d, want 200 carrying the relay's typed refusal: %s", resp.StatusCode, body)
	}
	var got commandResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decoding EntityCommandResult: %v (%s)", err, body)
	}
	if got.OK {
		t.Fatal("commanding an unadopted entity reported ok")
	}
	if got.Error == nil || got.Error.Code != "COMMAND_UNRESOLVED" {
		t.Fatalf("result = %+v, want COMMAND_UNRESOLVED", got)
	}
	if seen := roku.seen(); len(seen) != 0 {
		t.Fatalf("the unadopted device received %v — it must never be touched", seen)
	}
}
