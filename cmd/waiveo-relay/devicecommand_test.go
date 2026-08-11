package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/ecp"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// devicecommand_test.go is the app→hardware path this binary is supposed to
// provide, tested through the objects the binary itself builds.
//
// The defect it exists to prevent is not a logic error — every piece below was
// individually correct and individually tested. relayconn.Config.OnDeviceCommand
// simply had no production assignment, so the shipped relay answered every
// operator command "this relay has no device plane wired" while its own
// automation host sat next to it able to execute one. The gap was invisible to
// `go build`, to `go vet`, and to every unit test, because each half worked.
//
// So the cases here are deliberately end-to-end-shaped: they build the real
// dial config, the real device plane, and a real automation host, and assert
// that a command entering at the wire seam comes out at an HTTP server standing
// in for a Roku. A future edit that unwires the callback, drops the adoption
// gate, or changes what the controller resolves fails here rather than on a TV.

const (
	dcSite     = "01J8Z3K4N5P6Q7R8S9T0V1SITE"
	dcRelayID  = "relay-devicecommand-test"
	dcDriver   = "roku-ecp"
	dcNativeID = "uuid:roku:ecp:AA11"
	dcOtherNID = "uuid:roku:ecp:LEGACY"
	dcDeviceID = "01J8Z3K4N5P6Q7R8S9T0V1DEV1"
)

// fakeRoku is an HTTP server standing in for a Roku's ECP surface. It records
// every request path it is asked for, which is how these tests tell "the
// command reached hardware" from "the command was accepted and dropped".
type fakeRoku struct {
	srv *httptest.Server

	mu    sync.Mutex
	paths []string
}

func newFakeRoku(t *testing.T) *fakeRoku {
	t.Helper()
	f := &fakeRoku{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.EscapedPath())
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// location renders the fake as the SSDP LOCATION URL a real Roku answers with,
// so the candidate store learns its address exactly as it would on a LAN.
func (f *fakeRoku) location() string { return f.srv.URL + "/" }

func (f *fakeRoku) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func (f *fakeRoku) reset() {
	f.mu.Lock()
	f.paths = nil
	f.mu.Unlock()
}

// sighting is one SSDP observation of a device, as the discovery lane makes it.
func sighting(nativeID, location string) deviceplane.Observation {
	return deviceplane.Observation{
		Match:       deviceplane.Match{SSDP: rokuSearchTarget},
		Provenance:  deviceplane.ProvenanceDiscovered,
		Driver:      dcDriver,
		NativeID:    nativeID,
		DeviceClass: mediaPlayerClass,
		Entities:    []deviceplane.CandidateEntity{{Key: mainEntityKey, DeviceClass: mediaPlayerClass}},
		Address:     location,
	}
}

// entityIDOf derives the entity id both peers derive for a device's main entity
// (REL-110b) — the id an operator's command is addressed to.
func entityIDOf(nativeID string) string {
	return deviceid.Entity(dcSite, dcDriver, nativeID, mainEntityKey)
}

// adoptionFor is the `device_inventory.devices` array (REL-063) the app peer
// ships down when an operator adopts nativeID's main entity.
func adoptionFor(t *testing.T, nativeID string, enabled bool) wire.DeviceInventory {
	t.Helper()
	raw, err := json.Marshal(wire.DeviceEntry{
		DeviceID: dcDeviceID,
		Driver:   dcDriver,
		NativeID: nativeID,
		Entities: []wire.DeviceEntity{{
			EntityID:    entityIDOf(nativeID),
			DeviceClass: mediaPlayerClass,
			Enabled:     enabled,
			DisplayName: "Main",
			Category:    "primary",
		}},
	})
	if err != nil {
		t.Fatalf("marshaling the adopted device entry: %v", err)
	}
	return wire.DeviceInventory{Devices: []json.RawMessage{raw}}.Normalized()
}

// dcFixture is the relay's real device plane plus a real automation host over
// it, and the command sink the connection would hand a `device.command` to.
type dcFixture struct {
	plane    *devicePlane
	store    *deviceplane.Store
	commands *deviceCommandSink
}

// newDCFixture builds the same object graph run() builds, with no connection
// and no listeners: candidate store → device plane (newDevicePlane) →
// bootAutomationStack, which arms the command sink with the host it builds.
//
// It deliberately does NOT arm the sink itself. Arming is the property under
// test in TestBootAutomationStackArmsTheCommandSink, and a fixture that armed
// it by hand would keep every case below passing after the production wiring
// was deleted — which is precisely how the original defect survived a full test
// suite.
func newDCFixture(t *testing.T, overrides map[string]ecp.Target) *dcFixture {
	t.Helper()

	candStore := deviceplane.NewStore(dcRelayID)
	candStore.SetSite(dcSite)

	plane := newDevicePlane(overrides, candStore)

	opStore, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = opStore.Close() })

	commands := &deviceCommandSink{}
	if _, err := bootAutomationStack(
		opStore,
		identity.RelayIdentity{RelayID: dcRelayID},
		desiredstate.Applied{},
		hello.SiteBinding{TZ: "UTC"},
		deviceclass.Builtin(),
		plane.controller,
		plane.resolve,
		commands,
	); err != nil {
		t.Fatalf("bootAutomationStack: %v", err)
	}

	return &dcFixture{plane: plane, store: candStore, commands: commands}
}

// TestBootAutomationStackArmsTheCommandSink is the other half of the
// regression guard. relayDialConfig proves the connection HAS a handler;
// this proves the handler is connected to something that can execute a
// command — the assignment whose absence was the original defect.
//
// Booting the automation stack and leaving the wire unable to reach it is the
// failure being prevented, so the assertion is made against a sink nothing else
// touched: before the call it refuses everything, after it the host answers.
func TestBootAutomationStackArmsTheCommandSink(t *testing.T) {
	opStore, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = opStore.Close() })

	commands := &deviceCommandSink{}
	if res := commands.execute(wire.DeviceCommandBody{EntityID: "ent-1", Command: "home"}); res.OK {
		t.Fatal("an unarmed sink reported ok before the automation stack booted")
	}

	candStore := deviceplane.NewStore(dcRelayID)
	plane := newDevicePlane(nil, candStore)
	if _, err := bootAutomationStack(
		opStore,
		identity.RelayIdentity{RelayID: dcRelayID},
		desiredstate.Applied{},
		hello.SiteBinding{TZ: "UTC"},
		deviceclass.Builtin(),
		plane.controller,
		plane.resolve,
		commands,
	); err != nil {
		t.Fatalf("bootAutomationStack: %v", err)
	}

	// The sink now reaches the host's own command surface, which resolves the
	// command against the media-player vocabulary. An unknown entity draws
	// COMMAND_UNRESOLVED — the surface's answer, not the sink's "device plane is
	// not up yet" INTERNAL, which is how this distinguishes armed from unarmed.
	res := commands.execute(wire.DeviceCommandBody{EntityID: "ent-1", Command: "home"})
	if res.OK {
		t.Fatal("commanding an unknown entity reported ok")
	}
	if res.Error == nil || res.Error.Code != "COMMAND_UNRESOLVED" {
		t.Fatalf("result = %+v, want the command surface's COMMAND_UNRESOLVED — the sink is still unarmed", res)
	}
}

// TestRelayDialConfigWiresTheDeviceCommandHandler is the regression guard for
// the exact defect: the production dial configuration MUST carry a
// device.command handler, and that handler MUST be the sink the boot path later
// points at the automation host.
//
// It is written against relayDialConfig rather than against a comment, because
// a nil OnDeviceCommand is not a compile error and not a test failure anywhere
// else — relayconn answers the command with a typed INTERNAL refusal and the
// relay looks healthy. Deleting the assignment must break something, and this
// is the something.
func TestRelayDialConfigWiresTheDeviceCommandHandler(t *testing.T) {
	commands := &deviceCommandSink{}
	cfg := relayDialConfig(config{feederURL: "https://127.0.0.1:7420"}, nil, &nudgeSink{}, commands)

	if cfg.OnDeviceCommand == nil {
		t.Fatal("relayconn.Config.OnDeviceCommand is nil — every app-issued device command would be answered \"this relay has no device plane wired\"")
	}

	// And it is genuinely THIS sink, not some other closure: what the sink is
	// later set to is what the connection calls.
	var got wire.DeviceCommandBody
	commands.set(func(body wire.DeviceCommandBody) wire.DeviceCommandResultBody {
		got = body
		return wire.DeviceCommandResultBody{OK: true}
	})
	res := cfg.OnDeviceCommand(wire.DeviceCommandBody{EntityID: "ent-1", Command: "home"})
	if !res.OK {
		t.Fatalf("dial config's handler returned %+v, want ok", res)
	}
	if got.EntityID != "ent-1" || got.Command != "home" {
		t.Fatalf("handler received %+v, want the dispatched body", got)
	}
}

// TestDeviceCommandSinkRefusesBeforeTheHostIsUp: a command arriving in the boot
// window (the connection opens several steps before the automation host is
// built) must draw a typed refusal, never silence. REL-112 requires a result
// for every command, and a dropped one hangs the app peer's operation until its
// own timeout with nothing to report.
func TestDeviceCommandSinkRefusesBeforeTheHostIsUp(t *testing.T) {
	var sink deviceCommandSink

	res := sink.execute(wire.DeviceCommandBody{EntityID: "ent-1", Command: "home"})
	if res.OK {
		t.Fatal("an unset sink reported ok — a command nothing executed must never report success")
	}
	if res.Error == nil || res.Error.Code != "INTERNAL" {
		t.Fatalf("result = %+v, want a typed INTERNAL refusal", res)
	}
}

// TestAdoptedDeviceCommandsReachHardware is the parity surface, verb by verb: a
// command entering at the wire seam must arrive at the device as the right ECP
// request. Every one of these is a button on the legacy remote.
func TestAdoptedDeviceCommandsReachHardware(t *testing.T) {
	roku := newFakeRoku(t)
	fx := newDCFixture(t, nil)
	fx.store.Observe(sighting(dcNativeID, roku.location()), 1000)
	fx.plane.targets.SetInventory(adoptionFor(t, dcNativeID, true).Devices)

	entityID := entityIDOf(dcNativeID)
	cases := []struct {
		name     string
		command  string
		params   map[string]any
		wantPath string
	}{
		{"power on", "power", map[string]any{"state": "on"}, "POST /keypress/PowerOn"},
		{"power off", "power", map[string]any{"state": "off"}, "POST /keypress/PowerOff"},
		{"home", "home", nil, "POST /keypress/Home"},
		{"launch app", "launch", map[string]any{"channel": "782875"}, "POST /launch/782875"},
		{"dpad", "keypress", map[string]any{"key": "Select"}, "POST /keypress/Select"},
		{"transport", "keypress", map[string]any{"key": "Play"}, "POST /keypress/Play"},
		{"volume", "keypress", map[string]any{"key": "VolumeUp"}, "POST /keypress/VolumeUp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			roku.reset()
			res := fx.commands.execute(wire.DeviceCommandBody{
				EntityID: entityID,
				Command:  tc.command,
				Params:   tc.params,
			})
			if !res.OK {
				t.Fatalf("%s = %+v, want ok:true", tc.command, res.Error)
			}
			seen := roku.seen()
			if len(seen) != 1 || seen[0] != tc.wantPath {
				t.Fatalf("device saw %v, want exactly [%s]", seen, tc.wantPath)
			}
		})
	}
}

// TestUnadoptedDeviceIsRefused is the guardrail, at the seam an operator's
// command actually enters through. A Roku this relay discovered but nobody
// adopted must be refused, and — the part that matters on a shared LAN — must
// receive no HTTP request at all: the legacy stack still drives it, and a
// command from here would be two controllers fighting over one TV.
func TestUnadoptedDeviceIsRefused(t *testing.T) {
	adoptedRoku := newFakeRoku(t)
	legacyRoku := newFakeRoku(t)

	fx := newDCFixture(t, nil)
	fx.store.Observe(sighting(dcNativeID, adoptedRoku.location()), 1000)
	fx.store.Observe(sighting(dcOtherNID, legacyRoku.location()), 1000)
	// Only the first is adopted.
	fx.plane.targets.SetInventory(adoptionFor(t, dcNativeID, true).Devices)

	res := fx.commands.execute(wire.DeviceCommandBody{
		EntityID: entityIDOf(dcOtherNID),
		Command:  "home",
	})
	if res.OK {
		t.Fatal("commanding a discovered-but-unadopted device reported ok")
	}
	if res.Error == nil || res.Error.Code != "COMMAND_UNRESOLVED" {
		t.Fatalf("result = %+v, want COMMAND_UNRESOLVED", res)
	}
	if seen := legacyRoku.seen(); len(seen) != 0 {
		t.Fatalf("the unadopted device received %v — an unadopted device must never be touched", seen)
	}

	// The adopted one still works, so the refusal is the gate and not a
	// wholesale outage.
	if res := fx.commands.execute(wire.DeviceCommandBody{EntityID: entityIDOf(dcNativeID), Command: "home"}); !res.OK {
		t.Fatalf("the adopted device was refused too: %+v", res.Error)
	}
}

// TestDisabledEntityIsRefused: `enabled:false` is the app peer stating "do not
// act on this device" (REL-063). Adoption plus a known address is not enough.
func TestDisabledEntityIsRefused(t *testing.T) {
	roku := newFakeRoku(t)
	fx := newDCFixture(t, nil)
	fx.store.Observe(sighting(dcNativeID, roku.location()), 1000)
	fx.plane.targets.SetInventory(adoptionFor(t, dcNativeID, false).Devices)

	res := fx.commands.execute(wire.DeviceCommandBody{EntityID: entityIDOf(dcNativeID), Command: "home"})
	if res.OK {
		t.Fatal("commanding a disabled entity reported ok")
	}
	if seen := roku.seen(); len(seen) != 0 {
		t.Fatalf("a disabled entity's device received %v", seen)
	}
}

// TestUnadoptionStopsControlLive: an operator releasing a device in the console
// ships a generation whose inventory no longer names it, and control must stop
// at that apply — not at the next relay restart.
func TestUnadoptionStopsControlLive(t *testing.T) {
	roku := newFakeRoku(t)
	fx := newDCFixture(t, nil)
	fx.store.Observe(sighting(dcNativeID, roku.location()), 1000)
	fx.plane.targets.SetInventory(adoptionFor(t, dcNativeID, true).Devices)

	entityID := entityIDOf(dcNativeID)
	if res := fx.commands.execute(wire.DeviceCommandBody{EntityID: entityID, Command: "home"}); !res.OK {
		t.Fatalf("adopted device refused: %+v", res.Error)
	}

	fx.plane.targets.SetInventory(nil) // the next generation adopts nothing
	roku.reset()

	res := fx.commands.execute(wire.DeviceCommandBody{EntityID: entityID, Command: "home"})
	if res.OK {
		t.Fatal("a released device is still controllable")
	}
	if seen := roku.seen(); len(seen) != 0 {
		t.Fatalf("a released device received %v", seen)
	}
}

// TestUnknownCommandIsRefusedWithoutTouchingTheDevice pins REL-113 through the
// whole stack: a command outside the media-player vocabulary is rejected by the
// command surface, and the device is never contacted.
func TestUnknownCommandIsRefusedWithoutTouchingTheDevice(t *testing.T) {
	roku := newFakeRoku(t)
	fx := newDCFixture(t, nil)
	fx.store.Observe(sighting(dcNativeID, roku.location()), 1000)
	fx.plane.targets.SetInventory(adoptionFor(t, dcNativeID, true).Devices)

	res := fx.commands.execute(wire.DeviceCommandBody{
		EntityID: entityIDOf(dcNativeID),
		Command:  "self_destruct",
	})
	if res.OK || res.Error == nil || res.Error.Code != "COMMAND_UNRESOLVED" {
		t.Fatalf("result = %+v, want COMMAND_UNRESOLVED", res)
	}
	if seen := roku.seen(); len(seen) != 0 {
		t.Fatalf("device received %v for an out-of-vocabulary command", seen)
	}
}

// TestUnreachableAdoptedDeviceReportsTargetUnreachable: an adopted device at an
// address that no longer answers must produce the transient, retryable code —
// distinguishable from "you may not drive this", which is what an operator
// needs to tell a network problem from a permissions one.
func TestUnreachableAdoptedDeviceReportsTargetUnreachable(t *testing.T) {
	dead := newFakeRoku(t)
	deadURL := dead.location()
	dead.srv.Close() // the device goes away, its address does not

	fx := newDCFixture(t, nil)
	fx.store.Observe(sighting(dcNativeID, deadURL), 1000)
	fx.plane.targets.SetInventory(adoptionFor(t, dcNativeID, true).Devices)

	res := fx.commands.execute(wire.DeviceCommandBody{EntityID: entityIDOf(dcNativeID), Command: "home"})
	if res.OK {
		t.Fatal("commanding an unreachable device reported ok")
	}
	if res.Error == nil || res.Error.Code != "COMMAND_TARGET_UNREACHABLE" {
		t.Fatalf("result = %+v, want COMMAND_TARGET_UNREACHABLE", res)
	}
}

// TestOverrideTargetReachesHardwareWithoutAdoption: the deployment escape
// hatch still works end to end, with no candidate store entry and no inventory
// — the bring-up path, and the only path that existed before this track.
func TestOverrideTargetReachesHardwareWithoutAdoption(t *testing.T) {
	roku := newFakeRoku(t)
	host, port := hostPortOf(t, roku.srv.URL)

	fx := newDCFixture(t, map[string]ecp.Target{"ent-pinned": {Host: host, Port: port}})

	res := fx.commands.execute(wire.DeviceCommandBody{EntityID: "ent-pinned", Command: "home"})
	if !res.OK {
		t.Fatalf("an override-configured entity was refused: %+v", res.Error)
	}
	if seen := roku.seen(); len(seen) != 1 || seen[0] != "POST /keypress/Home" {
		t.Fatalf("device saw %v, want [POST /keypress/Home]", seen)
	}
}

// TestPollTargetsFollowTheAdoptedSet proves the poll set and the command set
// are one set. They are derived from the same gate on purpose: a relay that
// commanded a device it did not observe (or observed one it could not command)
// would have two adoption answers, and the drift would only show up as an
// automation that never fires.
func TestPollTargetsFollowTheAdoptedSet(t *testing.T) {
	roku := newFakeRoku(t)
	fx := newDCFixture(t, nil)
	fx.store.Observe(sighting(dcNativeID, roku.location()), 1000)
	fx.store.Observe(sighting(dcOtherNID, newFakeRoku(t).location()), 1000)

	if got := len(pollTargetsFor(fx.plane.targets)); got != 0 {
		t.Fatalf("%d poll target(s) before any adoption, want 0", got)
	}

	fx.plane.targets.SetInventory(adoptionFor(t, dcNativeID, true).Devices)

	got := pollTargetsFor(fx.plane.targets)
	if len(got) != 1 {
		t.Fatalf("poll targets = %v, want exactly the one adopted entity", got)
	}
	if _, ok := got[entityIDOf(dcNativeID)]; !ok {
		t.Fatalf("poll targets = %v, want the adopted entity's id", got)
	}
}

// hostPortOf splits an httptest server URL into the host and port an
// ecp.Target names.
func hostPortOf(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing %q: %v", rawURL, err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("splitting %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing port %q: %v", portStr, err)
	}
	return host, port
}
