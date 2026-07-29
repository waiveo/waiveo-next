// devicediscovery_e2e_test.go is the discovery pipeline's whole-stack proof:
// a device a relay discovers becomes a listable device and a commandable entity
// on the app peer's api/1 surface, with no registration step anywhere.
//
//	the relay's own candidate store (what its discovery lanes Observe into)
//	  -> internal/relay/relayconn (the real dialer's SendDeviceCandidates)
//	    == the real authenticated WS connection established by enrollment
//	       + challenge/hello/hello-ack ==
//	  -> internal/feeder/relayconn (the real connection server's read loop)
//	    -> internal/app/devices (the real intake and read model)
//	      -> internal/app/api (the real handler: GET /devices, GET /entities,
//	         POST /entities/{id}/commands)
//	        -> back down the same connection to the relay's real command
//	           surface and the physical-device adapter
//
// NOTHING in this file calls the intake. The only way a row appears is a real
// relay putting a real `device.candidates` frame on a real connection — which
// is the one arrangement that can catch either half of the failure this
// pipeline invites: a read model with a populate path nothing reaches, or a
// connection that accepts the report and drops it.
//
// The one link above this file's scope is the discovery lane -> candidate store
// step (an SSDP response or an mDNS PTR record becoming an Observation). That
// is proven against the real parsers in internal/relay/discovery and
// internal/relay/mdns, which is where the packet shapes live; from the candidate
// store onward, everything here is production code.
package relayconn_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	relayclient "github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// The discovered fixture: one Roku the relay's SSDP lane would have found, plus
// a second one behind the same declared search target — the pair that makes
// identity-keyed reporting observable end to end (REL-111a).
const (
	discDriver     = "roku-ecp"
	discNativeA    = "uuid:roku:ecp:X10001"
	discNativeB    = "uuid:roku:ecp:X10002"
	discClass      = "media-player"
	discEntityKey  = "main"
	discObservedMs = 1752537600000
)

// discoveredRoku is the Observation an SSDP sweep produces for one responder:
// the watch's declared driver/class/entities plus the USN that responder
// identified itself by (internal/relay/discovery.Watch.observation).
func discoveredRoku(t *testing.T, nativeID, name string) deviceplane.Observation {
	t.Helper()
	m, err := deviceplane.ParseMatch(json.RawMessage(`{"ssdp":"urn:roku-com:device:player:1"}`))
	if err != nil {
		t.Fatalf("ParseMatch: %v", err)
	}
	return deviceplane.Observation{
		Match:       m,
		Provenance:  deviceplane.ProvenanceDiscovered,
		Driver:      discDriver,
		NativeID:    nativeID,
		DeviceClass: discClass,
		Name:        name,
		Entities:    []deviceplane.CandidateEntity{{Key: discEntityKey, DeviceClass: discClass}},
	}
}

// discoveryStack is one relay connected to one app peer, with the app peer's
// real api/1 handler mounted over the same connection server.
type discoveryStack struct {
	h          *harness
	registry   *devices.Registry
	candStore  *deviceplane.Store
	client     *relayclient.Client
	controller *recordingController
	relayID    string
	ts         *httptest.Server
}

// newDiscoveryStack wires the production arrangement: the read model IS the
// connection server's candidate sink, and the relay's candidate store IS its
// command surface's entity resolver — the two seams that make a discovered
// device reachable in both directions without either peer sending the other an
// identifier.
func newDiscoveryStack(t *testing.T) *discoveryStack {
	t.Helper()

	registry := devices.New(apiSiteScopeNode)
	h := newHarness(t, feederrelayconn.WithCandidateSink(registry))

	identStore := enrolledRelay(t, h)
	id, _, err := identStore.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}

	candStore := deviceplane.NewStore(id.RelayID)
	controller := &recordingController{}
	host, err := automationhost.New(identStore, deviceclass.Builtin(), controller, candStore.ResolveEntity, id.RelayID)
	if err != nil {
		t.Fatalf("automationhost.New: %v", err)
	}
	client, err := relayclient.Dial(relayclient.Config{
		URL:             h.ts.URL,
		Store:           identStore,
		Declaration:     testDeclaration,
		OnDeviceCommand: host.DeviceCommand,
	})
	if err != nil {
		t.Fatalf("relayconn.Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() >= 1 },
		"authenticated connection never registered with the app peer")

	// The relay adopts the app peer's authoritative site from its own hello-ack
	// (REL-036) — exactly what cmd/waiveo-relay does on every connect. Neither
	// peer states an id to the other; they derive the same ones from this.
	candStore.SetSite(client.HelloAck().SiteBinding.ScopeNode)

	return &discoveryStack{
		h: h, registry: registry, candStore: candStore, client: client,
		controller: controller, relayID: id.RelayID,
		ts: newAPIOverRegistry(t, h, registry),
	}
}

// report sends the relay's full current candidate set upward exactly as
// cmd/waiveo-relay's own reportCandidates does, and waits for the app peer to
// hold the expected number of devices. It never touches the registry's writer.
func (s *discoveryStack) report(t *testing.T, wantDevices int) {
	t.Helper()
	rep := s.candStore.Report()
	cands := make([]wire.DeviceCandidate, 0, len(rep.Body.Candidates))
	for _, c := range rep.Body.Candidates {
		match, err := json.Marshal(c.Match)
		if err != nil {
			t.Fatalf("marshal candidate match: %v", err)
		}
		ents := make([]wire.CandidateEntity, 0, len(c.Entities))
		for _, e := range c.Entities {
			ents = append(ents, wire.CandidateEntity{Key: e.Key, DeviceClass: e.DeviceClass, State: e.State})
		}
		cands = append(cands, wire.DeviceCandidate{
			Match: match, Provenance: string(c.Provenance), Status: string(c.Status),
			IgnoredUntil: c.IgnoredUntil, FirstSeen: c.FirstSeen, LastSeen: c.LastSeen,
			Driver: c.Driver, NativeID: c.NativeID, DeviceClass: c.DeviceClass,
			Name: c.Name, Entities: ents,
		})
	}
	if err := s.client.SendDeviceCandidates(wire.DeviceCandidatesBody{Candidates: cands}); err != nil {
		t.Fatalf("SendDeviceCandidates: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(s.registry.Devices()) == wantDevices },
		fmt.Sprintf("the app peer never held %d device(s) after a device.candidates report", wantDevices))
}

// newAPIOverRegistry mounts the real api/1 handler over an EXISTING registry —
// deliberately without seeding a single row, so every row a case observes got
// there through the connection.
func newAPIOverRegistry(t *testing.T, h *harness, registry *devices.Registry) *httptest.Server {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clock := func() int64 { return apiFixedNowMs }
	fixture, err := authtest.New(authtest.Config{NowMs: clock})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	t.Cleanup(fixture.Close)
	apiAuth = fixture

	handler := api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, ulid.Monotonic(),
		origin.New(), apiContentBase, fixture.Auth,
		api.WithDevicePlane(registry, h.connSrv))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// listResponse is the openapi list envelope, decoded generically so a case can
// assert on whatever member it cares about without a per-family type.
type listResponse struct {
	Items  []map[string]any `json:"items"`
	Cursor *string          `json:"cursor"`
}

// getList issues the real HTTP GET an operator's client would.
func getList(t *testing.T, ts *httptest.Server, path string) listResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	apiAuth.Authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body %s)", path, resp.StatusCode, raw)
	}
	var out listResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v (body %s)", path, err, raw)
	}
	return out
}

// TestDiscoveredDeviceListsAndCommandsWithNoRegistration is the done-bar case:
// with the app peer's lists EMPTY and no registration call made anywhere, a
// device a relay discovered appears in GET /devices, its entity appears in
// GET /entities, and a command POSTed to that entity reaches the physical-device
// adapter behind the relay.
func TestDiscoveredDeviceListsAndCommandsWithNoRegistration(t *testing.T) {
	s := newDiscoveryStack(t)

	// Precondition, asserted rather than assumed: nothing is registered, and the
	// live routes say so truthfully rather than 404-ing.
	if got := getList(t, s.ts, "/api/v1/devices"); len(got.Items) != 0 {
		t.Fatalf("GET /devices before any report returned %d item(s), want 0 — the fixture pre-seeded a row", len(got.Items))
	}

	// The relay's discovery lane observes two Rokus behind ONE declared search
	// target, then reports its full set upward.
	s.candStore.Observe(discoveredRoku(t, discNativeA, "Lobby Roku"), discObservedMs)
	s.candStore.Observe(discoveredRoku(t, discNativeB, "Break Room Roku"), discObservedMs)
	s.report(t, 2)

	// (a) both devices list, identity-keyed rather than pattern-keyed.
	devList := getList(t, s.ts, "/api/v1/devices")
	if len(devList.Items) != 2 {
		t.Fatalf("GET /devices returned %d item(s), want 2 — two responders behind one pattern are two devices (REL-111a)", len(devList.Items))
	}
	wantDeviceID := deviceid.Device(apiSiteScopeNode, discDriver, discNativeA)
	var lobby map[string]any
	for _, item := range devList.Items {
		if item["id"] == wantDeviceID {
			lobby = item
		}
	}
	if lobby == nil {
		t.Fatalf("no device carries the derived id %q; got %v", wantDeviceID, devList.Items)
	}
	if lobby["relay_id"] != s.relayID {
		t.Errorf("device relay_id = %v, want the reporting relay %q", lobby["relay_id"], s.relayID)
	}
	if lobby["name"] != "Lobby Roku" {
		t.Errorf("device name = %v, want the device's own self-reported name", lobby["name"])
	}
	if lobby["scope_node"] != apiSiteScopeNode {
		t.Errorf("device scope_node = %v, want the relay's site node %q", lobby["scope_node"], apiSiteScopeNode)
	}
	// relay_id is NOT a ULID and must not be treated as one (REL-012/014): the
	// enrollment path mints it, and a row-id rule applied to it would refuse
	// every real relay.
	if ulid.Valid(s.relayID) {
		t.Fatalf("the enrolled relay_id %q happens to be a valid ULID — this fixture can no longer prove relay_id is not typed as one", s.relayID)
	}
	if !ulid.Valid(wantDeviceID) {
		t.Errorf("derived device id %q is not a canonical ULID (DAT-005a)", wantDeviceID)
	}

	// (b) the entity lists, owned by the derived device.
	entList := getList(t, s.ts, "/api/v1/entities")
	if len(entList.Items) != 2 {
		t.Fatalf("GET /entities returned %d item(s), want 2", len(entList.Items))
	}
	wantEntityID := deviceid.Entity(apiSiteScopeNode, discDriver, discNativeA, discEntityKey)
	var lobbyEntity map[string]any
	for _, item := range entList.Items {
		if item["id"] == wantEntityID {
			lobbyEntity = item
		}
	}
	if lobbyEntity == nil {
		t.Fatalf("no entity carries the derived id %q; got %v", wantEntityID, entList.Items)
	}
	if lobbyEntity["device_id"] != wantDeviceID {
		t.Errorf("entity device_id = %v, want the derived device id %q", lobbyEntity["device_id"], wantDeviceID)
	}
	if !ulid.Valid(wantEntityID) {
		t.Errorf("derived entity id %q is not a canonical ULID (DAT-005a)", wantEntityID)
	}

	// (c) a command against the discovered entity reaches the device. The relay
	// resolves the id by deriving it from what IT discovered — nothing told it
	// this id, and it accepted no identifier from the app peer's registry.
	resp, raw := postCommand(t, s.ts, wantEntityID,
		`{"command":"launch","params":{"channel":"dev"}}`, nil)
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
	calls := s.controller.dispatched()
	if len(calls) != 1 {
		t.Fatalf("the device adapter saw %d dispatch(es), want 1: %+v", len(calls), calls)
	}
	if calls[0].EntityID != wantEntityID || calls[0].Command != "launch" {
		t.Fatalf("device adapter saw %+v, want launch against %s", calls[0], wantEntityID)
	}
}

// TestReportIsAFullSetReplaceForThatRelayOnly drives REL-111's replace semantics
// over the real connection: a relay that stops seeing a device drops it from the
// app peer's view on its NEXT report, without any delete call — and a report is
// a replace of that relay's own view, not an append.
func TestReportIsAFullSetReplaceForThatRelayOnly(t *testing.T) {
	s := newDiscoveryStack(t)

	s.candStore.Observe(discoveredRoku(t, discNativeA, "Lobby Roku"), discObservedMs)
	s.candStore.Observe(discoveredRoku(t, discNativeB, "Break Room Roku"), discObservedMs)
	s.report(t, 2)

	// The relay now sees only one of them. A fresh store standing in for the
	// relay's next sweep is exactly what a full-set report is: the current view,
	// not a delta describing what left.
	s.candStore = deviceplane.NewStore(s.relayID)
	s.candStore.SetSite(apiSiteScopeNode)
	s.candStore.Observe(discoveredRoku(t, discNativeA, "Lobby Roku"), discObservedMs+1000)
	s.report(t, 1)

	list := getList(t, s.ts, "/api/v1/devices")
	if len(list.Items) != 1 {
		t.Fatalf("GET /devices = %d item(s) after a report omitting one device, want 1", len(list.Items))
	}
	if want := deviceid.Device(apiSiteScopeNode, discDriver, discNativeA); list.Items[0]["id"] != want {
		t.Fatalf("surviving device id = %v, want %q", list.Items[0]["id"], want)
	}
	// The entity of the departed device went with it: a command to it is now the
	// same 404 an id nobody ever reported draws.
	goneEntity := deviceid.Entity(apiSiteScopeNode, discDriver, discNativeB, discEntityKey)
	resp, raw := postCommand(t, s.ts, goneEntity, `{"command":"home"}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("command to a no-longer-reported entity = %d, want 404 (body %s)", resp.StatusCode, raw)
	}
}

// TestAForgedRelayIdInTheFrameCannotReplaceAnotherRelaysView is the hostile
// case the intake's authenticated-identity argument exists for, driven over a
// real authenticated connection: relay B enrolls, completes a valid handshake,
// then sends a `device.candidates` frame whose envelope `relay_id` CLAIMS to be
// relay A.
//
// The frame's own relay_id is a self-assertion; the app peer keys the replace by
// the mTLS identity the connection was authenticated as (REL-041/150). A report
// is a full-set REPLACE, so honouring the claim would let any enrolled relay
// wipe another relay's entire device view — and re-point its entities' commands
// at itself — with one frame.
//
// Change handleDeviceCandidates to pass f.RelayID instead of the connection's
// own relayID and this test fails: relay A's device disappears, replaced by the
// forged report's device, still attributed to relay A.
func TestAForgedRelayIdInTheFrameCannotReplaceAnotherRelaysView(t *testing.T) {
	a := newDiscoveryStack(t)
	a.candStore.Observe(discoveredRoku(t, discNativeA, "Lobby Roku"), discObservedMs)
	a.report(t, 1)

	// A second relay enrolls onto the SAME app peer, using the raw client so its
	// frames can carry an envelope the production dialer would never emit.
	bIdent := enrolledRelay(t, a.h)
	bID, _, err := bIdent.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	if bID.RelayID == a.relayID {
		t.Fatal("the two relays enrolled under one identity — this case cannot separate them")
	}
	ws, err := rawDial(t, a.h, bIdent, bID.CertPEM, []string{wire.Subprotocol})
	if err != nil {
		t.Fatalf("rawDial (relay B): %v", err)
	}
	defer ws.CloseNow()
	rawHandshake(t, ws, bIdent)

	forged, err := wire.NewFrame(wire.FrameTypeDeviceCandidates, "forged-report-1",
		a.relayID, // the lie: relay B stamping relay A's identity
		wire.DeviceCandidatesBody{Candidates: []wire.DeviceCandidate{{
			Match:      json.RawMessage(`{"ssdp":"urn:roku-com:device:player:1"}`),
			Provenance: wire.CandidateProvenanceDiscovered,
			Status:     wire.CandidateStatusPending,
			FirstSeen:  discObservedMs, LastSeen: discObservedMs,
			Driver: discDriver, NativeID: "uuid:roku:ecp:ATTACKER", DeviceClass: discClass,
			Name:     "Attacker Roku",
			Entities: []wire.CandidateEntity{{Key: discEntityKey, DeviceClass: discClass}},
		}}})
	if err != nil {
		t.Fatalf("NewFrame(device.candidates): %v", err)
	}
	if err := wsSend(t, ws, forged); err != nil {
		t.Fatalf("send forged report: %v", err)
	}

	// The forged report lands under relay B's OWN identity — an addition, never a
	// replacement — so both devices exist and each is attributed to its real
	// reporter.
	attacker := deviceid.Device(apiSiteScopeNode, discDriver, "uuid:roku:ecp:ATTACKER")
	waitFor(t, 5*time.Second, func() bool {
		for _, d := range a.registry.Devices() {
			if d.ID == attacker {
				return true
			}
		}
		return false
	}, "the forged report never reached the intake at all")

	byID := map[string]devices.Device{}
	for _, d := range a.registry.Devices() {
		byID[d.ID] = d
	}
	aDevice := deviceid.Device(apiSiteScopeNode, discDriver, discNativeA)
	got, ok := byID[aDevice]
	if !ok {
		t.Fatalf("relay A's device %q vanished when relay B forged A's relay_id — the frame's claim was honoured", aDevice)
	}
	if got.RelayID != a.relayID {
		t.Errorf("relay A's device is attributed to %q, want %q", got.RelayID, a.relayID)
	}
	forgedRow, ok := byID[attacker]
	if !ok {
		t.Fatalf("the forged report's device %q never landed", attacker)
	}
	if forgedRow.RelayID != bID.RelayID {
		t.Errorf("the forged report's device is attributed to %q, want the AUTHENTICATED reporter %q", forgedRow.RelayID, bID.RelayID)
	}
}

// TestSameDeviceSeenByTwoRelaysIsOneRow is REL-153 over the real connection: a
// device's identity is (site, driver, native_id), never the relay reporting it,
// so two relays serving one site that both see one device hold ONE row between
// them — attributed to whichever reported most recently — and it survives either
// one of them ceasing to report it.
func TestSameDeviceSeenByTwoRelaysIsOneRow(t *testing.T) {
	a := newDiscoveryStack(t)
	a.candStore.Observe(discoveredRoku(t, discNativeA, "Lobby Roku"), discObservedMs)
	a.report(t, 1)

	bIdent := enrolledRelay(t, a.h)
	bID, _, err := bIdent.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	bClient, err := relayclient.Dial(relayclient.Config{
		URL: a.h.ts.URL, Store: bIdent, Declaration: testDeclaration,
	})
	if err != nil {
		t.Fatalf("relayconn.Dial (relay B): %v", err)
	}
	t.Cleanup(func() { _ = bClient.Close() })
	waitFor(t, 5*time.Second, func() bool { return a.h.connSrv.ConnCount() >= 2 }, "relay B never connected")

	bStore := deviceplane.NewStore(bID.RelayID)
	bStore.SetSite(apiSiteScopeNode)
	bStore.Observe(discoveredRoku(t, discNativeA, "Lobby Roku"), discObservedMs+1)
	sendReport(t, bClient, bStore)

	wantID := deviceid.Device(apiSiteScopeNode, discDriver, discNativeA)
	waitFor(t, 5*time.Second, func() bool {
		rows := a.registry.Devices()
		return len(rows) == 1 && rows[0].RelayID == bID.RelayID
	}, "the same device seen by two relays did not settle to one row attributed to the most recent reporter")

	rows := a.registry.Devices()
	if rows[0].ID != wantID {
		t.Fatalf("row id = %q, want the identity-derived %q (REL-153)", rows[0].ID, wantID)
	}

	// Relay B stops seeing it. Relay A still does, so the row survives — and
	// comes back to A, because A's view still holds it.
	empty := deviceplane.NewStore(bID.RelayID)
	sendReport(t, bClient, empty)
	waitFor(t, 5*time.Second, func() bool {
		rows := a.registry.Devices()
		return len(rows) == 1 && rows[0].RelayID == a.relayID
	}, "the device vanished (or stayed attributed to B) when B stopped reporting it, though A still reports it")
}

// TestMalformedReportLeavesThePriorViewIntact drives an untrusted report over
// the real connection: a candidate violating REL-110's own shape is refused
// whole, the relay is told, and the app peer still holds exactly what it held
// before — never a half-applied view whose missing rows look like devices that
// went away.
func TestMalformedReportLeavesThePriorViewIntact(t *testing.T) {
	s := newDiscoveryStack(t)
	s.candStore.Observe(discoveredRoku(t, discNativeA, "Lobby Roku"), discObservedMs)
	s.report(t, 1)
	before := s.registry.Devices()

	// One good candidate and one with no native_id: applying the good half
	// would silently delete nothing here, but WOULD in the general case, so the
	// whole report is refused.
	err := s.client.SendDeviceCandidates(wire.DeviceCandidatesBody{Candidates: []wire.DeviceCandidate{
		{
			Match:      json.RawMessage(`{"ssdp":"urn:roku-com:device:player:1"}`),
			Provenance: wire.CandidateProvenanceDiscovered, Status: wire.CandidateStatusPending,
			Driver: discDriver, NativeID: "uuid:roku:ecp:GOOD", DeviceClass: discClass,
			Entities: []wire.CandidateEntity{{Key: discEntityKey, DeviceClass: discClass}},
		},
		{
			Match:      json.RawMessage(`{"ssdp":"urn:roku-com:device:player:1"}`),
			Provenance: wire.CandidateProvenanceDiscovered, Status: wire.CandidateStatusPending,
			Driver: discDriver, DeviceClass: discClass, // no native_id
		},
	}})
	if err != nil {
		t.Fatalf("SendDeviceCandidates: %v", err)
	}

	// The refusal is asynchronous; give the app peer a window in which it COULD
	// have corrupted the view, then assert it did not.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		rows := s.registry.Devices()
		if len(rows) != len(before) {
			t.Fatalf("a refused report changed the view: %d device(s), want the prior %d", len(rows), len(before))
		}
		if len(rows) == 1 && rows[0].ID != before[0].ID {
			t.Fatalf("a refused report replaced the prior device %q with %q", before[0].ID, rows[0].ID)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The connection survives a refused report — a bad report is not a protocol
	// violation — and a subsequent good report still applies.
	if n := s.h.connSrv.ConnCount(); n != 1 {
		t.Fatalf("ConnCount = %d after a refused report, want the connection still up", n)
	}
	s.candStore.Observe(discoveredRoku(t, discNativeB, "Break Room Roku"), discObservedMs+1)
	s.report(t, 2)
}

// TestRelaySuppliedIdentifiersAreNotHeard proves the structural half of the
// intake's identity argument: a report carrying `id`, `device_id` and
// `entity_id` members — the fields a relay would use to name a row it does not
// own — changes nothing about the ids the app peer assigns, because nothing on
// this side decodes them.
func TestRelaySuppliedIdentifiersAreNotHeard(t *testing.T) {
	s := newDiscoveryStack(t)

	// Hand-built frame body, so the injected members really are on the wire
	// rather than dropped by the sending type.
	hostile := fmt.Sprintf(`{"candidates":[{
		"id":"01J8Z3K4N5P6Q7R8S9T0V1W2Y2",
		"device_id":"01J8Z3K4N5P6Q7R8S9T0V1W2D1",
		"match":{"ssdp":"urn:roku-com:device:player:1"},
		"provenance":"discovered","status":"pending","ignored_until":null,
		"first_seen":%d,"last_seen":%d,
		"driver":%q,"native_id":%q,"device_class":%q,"name":"Lobby Roku",
		"entities":[{"key":%q,"device_class":%q,"entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Y2"}]
	}]}`, discObservedMs, discObservedMs, discDriver, discNativeA, discClass, discEntityKey, discClass)

	var body wire.DeviceCandidatesBody
	if err := json.Unmarshal([]byte(hostile), &body); err != nil {
		t.Fatalf("decode hostile body: %v", err)
	}
	if err := s.client.SendDeviceCandidates(body); err != nil {
		t.Fatalf("SendDeviceCandidates: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(s.registry.Devices()) == 1 },
		"the report never landed")

	wantDevice := deviceid.Device(apiSiteScopeNode, discDriver, discNativeA)
	wantEntity := deviceid.Entity(apiSiteScopeNode, discDriver, discNativeA, discEntityKey)
	rows := s.registry.Devices()
	if rows[0].ID != wantDevice {
		t.Fatalf("device id = %q, want the DERIVED %q — an id was taken off the wire", rows[0].ID, wantDevice)
	}
	ents := s.registry.Entities()
	if len(ents) != 1 || ents[0].ID != wantEntity {
		t.Fatalf("entity ids = %v, want the single derived %q", ents, wantEntity)
	}
	if _, found := s.registry.Entity("01J8Z3K4N5P6Q7R8S9T0V1W2Y2"); found {
		t.Fatal("the relay-supplied entity_id resolves — a relay named a row")
	}
}

// sendReport ships a candidate store's full current set down a client, the same
// projection cmd/waiveo-relay's reportCandidates performs.
func sendReport(t *testing.T, c *relayclient.Client, store *deviceplane.Store) {
	t.Helper()
	rep := store.Report()
	cands := make([]wire.DeviceCandidate, 0, len(rep.Body.Candidates))
	for _, cand := range rep.Body.Candidates {
		match, err := json.Marshal(cand.Match)
		if err != nil {
			t.Fatalf("marshal candidate match: %v", err)
		}
		ents := make([]wire.CandidateEntity, 0, len(cand.Entities))
		for _, e := range cand.Entities {
			ents = append(ents, wire.CandidateEntity{Key: e.Key, DeviceClass: e.DeviceClass, State: e.State})
		}
		cands = append(cands, wire.DeviceCandidate{
			Match: match, Provenance: string(cand.Provenance), Status: string(cand.Status),
			IgnoredUntil: cand.IgnoredUntil, FirstSeen: cand.FirstSeen, LastSeen: cand.LastSeen,
			Driver: cand.Driver, NativeID: cand.NativeID, DeviceClass: cand.DeviceClass,
			Name: cand.Name, Entities: ents,
		})
	}
	if err := c.SendDeviceCandidates(wire.DeviceCandidatesBody{Candidates: cands}); err != nil {
		t.Fatalf("SendDeviceCandidates: %v", err)
	}
}
