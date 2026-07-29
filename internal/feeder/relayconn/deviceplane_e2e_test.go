// deviceplane_e2e_test.go proves relay/1's device-command plane over the REAL
// persistent connection, both sides live: the app peer's outbound dispatch
// (Server.SendDeviceCommand) travels the same authenticated WS connection the
// handshake established, the relay's REAL command surface
// (internal/relay/automationhost over internal/relay/deviceplane) resolves and
// executes it, and the correlated `device.command_result` comes back to the
// caller (REL-112/113).
//
// Nothing here stands in for the transport: the frames are the production
// frames, sent by the production server and answered by the production client.
//
// Proven behaviors:
//
//	(i)   a resolved command reaches the physical-device adapter and answers
//	      {ok:true}, carrying the caller's trace_id both ways (REL-006);
//	(ii)  a command the target's device class does not declare is answered
//	      {ok:false, COMMAND_UNRESOLVED} and NEVER attempted against the device
//	      (REL-113);
//	(iii) a controller that cannot reach the device surfaces its own taxonomy
//	      code (COMMAND_TARGET_UNREACHABLE) as the result's error;
//	(iv)  an offline relay draws the typed ErrRelayNotConnected refusal — the
//	      command is never silently dropped;
//	(v)   correlation entries are reaped: N commands that are never answered
//	      leave the pending map at zero once their callers give up, and a
//	      connection that dies mid-exchange unblocks its waiters;
//	(vi)  concurrent commands never cross wires — each caller receives its own
//	      command's result.
package relayconn_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/deviceclass"
	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	relayclient "github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// The device-plane fixture: two entities of ONE physical device, plus a second
// device — the shape REL-115's per-device serialization is defined over (a
// device exposes one or more entities).
const (
	fixtureEntityA  = "01J8Z3K4N5P6Q7R8S9T0V1W2Y2"
	fixtureEntityB  = "01J8Z3K4N5P6Q7R8S9T0V1W2Y3"
	fixtureDevice1  = "01J8Z3K4N5P6Q7R8S9T0V1W2D1"
	fixtureDevice2  = "01J8Z3K4N5P6Q7R8S9T0V1W2D2"
	fixtureUnknown  = "01J8Z3K4N5P6Q7R8S9T0V1W2ZZ"
	fixtureDevClass = "media-player"
)

// fixtureResolver maps the fixture entities onto their owning device and the
// canonical media-player class; anything else resolves to nothing, so a command
// against it is COMMAND_UNRESOLVED without touching a device (REL-113).
func fixtureResolver(entityID string) (string, string, bool) {
	switch entityID {
	case fixtureEntityA:
		return fixtureDevice1, fixtureDevClass, true
	case fixtureEntityB:
		return fixtureDevice2, fixtureDevClass, true
	default:
		return "", "", false
	}
}

// recordingController is the physical-device adapter under the real command
// surface: it records every dispatch it is handed and returns whatever the test
// scripted. block, when non-nil, holds every dispatch until it is closed —
// standing in for a device that never answers, with no wall-clock sleep.
type recordingController struct {
	mu    sync.Mutex
	calls []deviceplane.CommandBody
	err   error
	block chan struct{}
}

func (c *recordingController) Dispatch(entityID, command string, params map[string]any) error {
	c.mu.Lock()
	c.calls = append(c.calls, deviceplane.CommandBody{EntityID: entityID, Command: command, Params: params})
	block, err := c.block, c.err
	c.mu.Unlock()
	if block != nil {
		<-block
	}
	return err
}

func (c *recordingController) dispatched() []deviceplane.CommandBody {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]deviceplane.CommandBody, len(c.calls))
	copy(out, c.calls)
	return out
}

func (c *recordingController) setErr(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}

// connectedDevicePlane enrolls a relay, builds its REAL edge-automation host
// (whose command surface carries the canonical device-class registry), and
// dials it onto the app peer with that host wired as the inbound
// device.command handler — the exact production wiring. It waits for the app
// peer to register the connection before returning, so a test that pushes a
// command immediately can never race the registration.
func connectedDevicePlane(t *testing.T, h *harness) (*relayclient.Client, *recordingController, string) {
	t.Helper()
	store := enrolledRelay(t, h)
	id, _, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	controller := &recordingController{}
	client := dialWithDevicePlane(t, h, store, controller, id.RelayID)
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() >= 1 },
		"authenticated connection never registered with the app peer")
	return client, controller, id.RelayID
}

// dialWithDevicePlane builds the relay's real automation host over controller
// and dials, handing the host's DeviceCommand seam to the client as its
// inbound-command handler.
func dialWithDevicePlane(t *testing.T, h *harness, store *identity.Store, controller deviceplane.DeviceController, relayID string) *relayclient.Client {
	t.Helper()
	host, err := automationhost.New(store, deviceclass.Builtin(), controller, fixtureResolver, relayID)
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
	return client
}

// sendCtx bounds one command exchange. It is a deadline on the caller's own
// wait, not a sleep: every test that expects an answer gets one long before it
// expires.
func sendCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// TestDeviceCommandRoundTripOverPersistentConnection is behaviors (i)-(iii) on
// ONE connection: the app peer dispatches, the relay's real command surface
// resolves against the canonical media-player vocabulary and executes, and the
// result travels back correlated to the caller.
func TestDeviceCommandRoundTripOverPersistentConnection(t *testing.T) {
	h := newHarness(t)
	_, controller, relayID := connectedDevicePlane(t, h)

	// (i) a resolved command reaches the device and answers ok.
	ctx, cancel := sendCtx(t)
	defer cancel()
	res, err := h.connSrv.SendDeviceCommand(ctx, relayID, "01J8Z4K4N5P6Q7R8S9T0V1W3B0",
		wire.DeviceCommandBody{
			EntityID: fixtureEntityA,
			Command:  "launch",
			Params:   map[string]any{"channel": "dev"},
		})
	if err != nil {
		t.Fatalf("SendDeviceCommand: %v", err)
	}
	if !res.OK || res.Error != nil {
		t.Fatalf("result = %+v, want {ok:true}", res)
	}
	calls := controller.dispatched()
	if len(calls) != 1 {
		t.Fatalf("controller saw %d dispatch(es), want 1: %+v", len(calls), calls)
	}
	if calls[0].EntityID != fixtureEntityA || calls[0].Command != "launch" {
		t.Fatalf("dispatched %+v, want launch against %s", calls[0], fixtureEntityA)
	}
	if got := calls[0].Params["channel"]; got != "dev" {
		t.Fatalf("dispatched params[channel] = %v, want dev — the command's params did not survive the wire", got)
	}

	// (ii) a command outside the target's device class is refused without ever
	// reaching the device (REL-113).
	res, err = h.connSrv.SendDeviceCommand(ctx, relayID, "", wire.DeviceCommandBody{
		EntityID: fixtureEntityA,
		Command:  "blast",
	})
	if err != nil {
		t.Fatalf("SendDeviceCommand (unresolved): %v", err)
	}
	if res.OK || res.Error == nil || res.Error.Code != "COMMAND_UNRESOLVED" {
		t.Fatalf("result = %+v, want {ok:false, COMMAND_UNRESOLVED}", res)
	}
	if n := len(controller.dispatched()); n != 1 {
		t.Fatalf("controller saw %d dispatch(es) after an unresolved command, want the original 1 — REL-113 forbids attempting it", n)
	}

	// An entity the relay has never adopted resolves against no vocabulary at
	// all — same refusal, still no device touched.
	res, err = h.connSrv.SendDeviceCommand(ctx, relayID, "", wire.DeviceCommandBody{
		EntityID: fixtureUnknown,
		Command:  "home",
	})
	if err != nil {
		t.Fatalf("SendDeviceCommand (unknown entity): %v", err)
	}
	if res.OK || res.Error == nil || res.Error.Code != "COMMAND_UNRESOLVED" {
		t.Fatalf("result = %+v, want {ok:false, COMMAND_UNRESOLVED} for an unadopted entity", res)
	}

	// (iii) the controller's own taxonomy code rides the result's error.
	controller.setErr(&deviceplane.ControllerError{
		Code:    "COMMAND_TARGET_UNREACHABLE",
		Message: "the device did not answer on its LAN address",
	})
	res, err = h.connSrv.SendDeviceCommand(ctx, relayID, "", wire.DeviceCommandBody{
		EntityID: fixtureEntityA,
		Command:  "home",
	})
	if err != nil {
		t.Fatalf("SendDeviceCommand (unreachable): %v", err)
	}
	if res.OK || res.Error == nil || res.Error.Code != "COMMAND_TARGET_UNREACHABLE" {
		t.Fatalf("result = %+v, want {ok:false, COMMAND_TARGET_UNREACHABLE}", res)
	}

	// Every exchange completed, so nothing may be left in the correlation map.
	if n := h.connSrv.PendingCommandCount(); n != 0 {
		t.Fatalf("PendingCommandCount = %d after four completed exchanges, want 0", n)
	}
}

// TestDeviceCommandTraceIDRoundTrips pins REL-006 on this exchange: the
// originating operation's trace_id rides the request and is echoed on the
// result, so one identifier correlates the operation across both peers.
func TestDeviceCommandTraceIDRoundTrips(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	id, _, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}

	// Observe the relay's own wire traffic: the reply frame it emits is what
	// carries the echo, and the app peer's correlation is what routes it.
	log := &frameLog{}
	host, err := automationhost.New(store, deviceclass.Builtin(), &recordingController{}, fixtureResolver, id.RelayID)
	if err != nil {
		t.Fatalf("automationhost.New: %v", err)
	}
	client, err := relayclient.Dial(relayclient.Config{
		URL:             h.ts.URL,
		Store:           store,
		Declaration:     testDeclaration,
		OnDeviceCommand: host.DeviceCommand,
		ObserveFrame:    log.observe,
	})
	if err != nil {
		t.Fatalf("relayconn.Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() >= 1 },
		"authenticated connection never registered with the app peer")

	const traceID = "01J8Z4K4N5P6Q7R8S9T0V1W3B0"
	ctx, cancel := sendCtx(t)
	defer cancel()
	if _, err := h.connSrv.SendDeviceCommand(ctx, id.RelayID, traceID, wire.DeviceCommandBody{
		EntityID: fixtureEntityA,
		Command:  "home",
	}); err != nil {
		t.Fatalf("SendDeviceCommand: %v", err)
	}

	// SendDeviceCommand returning does NOT mean the relay's frame log has
	// recorded the outbound result yet: the log is written by the relay-side
	// goroutine, so there is a window between the call returning and the frame
	// being observable here. Reading the log immediately made this test fail
	// under full-suite load on a slow runner — twice — with an empty result id,
	// because the assertion ran inside that window. Wait for the frame the way
	// this test already waits for the connection, rather than assuming a send
	// that has returned is a send that has been logged.
	var request, result wire.Frame
	collect := func() {
		request, result = wire.Frame{}, wire.Frame{}
		for _, f := range log.Received() {
			if f.Type == wire.FrameTypeDeviceCommand {
				request = f
			}
		}
		for _, f := range log.Sent() {
			if f.Type == wire.FrameTypeDeviceCommandResult {
				result = f
			}
		}
	}
	waitFor(t, 5*time.Second, func() bool {
		collect()
		return request.ID != "" && result.ID != ""
	}, "the relay never logged both a device.command and its device.command_result")
	if request.ID == "" {
		t.Fatal("the relay never received a device.command frame")
	}
	if request.TraceID != traceID {
		t.Fatalf("received device.command trace_id = %q, want %q", request.TraceID, traceID)
	}
	if result.ID != request.ID {
		t.Fatalf("device.command_result id = %q, want the request's %q (REL-006)", result.ID, request.ID)
	}
	if result.TraceID != traceID {
		t.Fatalf("device.command_result trace_id = %q, want %q (REL-006)", result.TraceID, traceID)
	}
	if result.RelayID != id.RelayID {
		t.Fatalf("device.command_result relay_id = %q, want %q (REL-005)", result.RelayID, id.RelayID)
	}
}

// TestDeviceCommandRefusedWhenRelayOffline is behavior (iv): with no live
// connection the command draws the typed ErrRelayNotConnected rather than being
// queued, dropped, or answered with a fabricated result.
func TestDeviceCommandRefusedWhenRelayOffline(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := sendCtx(t)
	defer cancel()

	_, err := h.connSrv.SendDeviceCommand(ctx, "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "", wire.DeviceCommandBody{
		EntityID: fixtureEntityA,
		Command:  "home",
	})
	if err == nil {
		t.Fatal("SendDeviceCommand to an unconnected relay returned no error — a dropped command is never acceptable")
	}
	if err != feederrelayconn.ErrRelayNotConnected {
		t.Fatalf("err = %v, want ErrRelayNotConnected", err)
	}
}

// TestDeviceCommandCorrelationEntriesAreReaped is behavior (v)'s first half:
// N commands the relay never answers (its controller is held) register N
// correlation entries; once every caller's context ends, the map returns to
// zero. The hold is a channel, not a sleep — the test controls exactly when a
// dispatch may complete.
func TestDeviceCommandCorrelationEntriesAreReaped(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	id, _, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	held := make(chan struct{})
	controller := &recordingController{block: held}
	dialWithDevicePlane(t, h, store, controller, id.RelayID)
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() >= 1 },
		"authenticated connection never registered with the app peer")

	const n = 8
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct entities of DIFFERENT devices, so REL-115's per-device
			// lock does not serialize them into a single outstanding dispatch:
			// all n commands must genuinely be in flight at once.
			entity := fixtureEntityA
			if i%2 == 1 {
				entity = fixtureEntityB
			}
			_, err := h.connSrv.SendDeviceCommand(ctx, id.RelayID, "", wire.DeviceCommandBody{
				EntityID: entity,
				Command:  "home",
			})
			errs <- err
		}(i)
	}

	// Every command is armed in the map before any caller gives up.
	waitFor(t, 10*time.Second, func() bool { return h.connSrv.PendingCommandCount() == n },
		fmt.Sprintf("PendingCommandCount never reached %d — commands were not correlated", n))

	// The callers give up. Nothing answers them: the relay is still holding
	// every dispatch inside its controller.
	cancel()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != context.Canceled {
			t.Fatalf("abandoned command returned %v, want context.Canceled", err)
		}
	}
	if n := h.connSrv.PendingCommandCount(); n != 0 {
		t.Fatalf("PendingCommandCount = %d after every caller gave up, want 0 — correlation entries leaked", n)
	}

	// Release the held dispatches: their (now uncorrelated) results arrive and
	// are discarded without re-populating the map or crashing the read loop.
	close(held)
	waitFor(t, 10*time.Second, func() bool { return len(controller.dispatched()) == n },
		"the relay never completed the held dispatches")
	if n := h.connSrv.PendingCommandCount(); n != 0 {
		t.Fatalf("PendingCommandCount = %d after late results arrived, want 0", n)
	}
}

// TestDeviceCommandUnblocksWhenConnectionDies is behavior (v)'s second half: a
// caller waiting on a reply when the connection dies is released with the typed
// offline error, never left hanging until its own deadline.
func TestDeviceCommandUnblocksWhenConnectionDies(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	id, _, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	held := make(chan struct{})
	defer close(held)
	controller := &recordingController{block: held}
	client := dialWithDevicePlane(t, h, store, controller, id.RelayID)
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() >= 1 },
		"authenticated connection never registered with the app peer")

	done := make(chan error, 1)
	go func() {
		ctx, cancel := sendCtx(t)
		defer cancel()
		_, err := h.connSrv.SendDeviceCommand(ctx, id.RelayID, "", wire.DeviceCommandBody{
			EntityID: fixtureEntityA,
			Command:  "home",
		})
		done <- err
	}()
	waitFor(t, 10*time.Second, func() bool { return h.connSrv.PendingCommandCount() == 1 },
		"the command was never correlated")

	_ = client.Close()

	select {
	case err := <-done:
		if err != feederrelayconn.ErrRelayNotConnected {
			t.Fatalf("err = %v, want ErrRelayNotConnected when the connection dies mid-exchange", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the caller was never released when the connection died")
	}
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.PendingCommandCount() == 0 },
		"correlation entries survived the connection's death")
}

// TestConcurrentDeviceCommandsNeverCrossWires is behavior (vi): many commands
// in flight at once, each answered with a result that identifies its own
// command, and every caller receives exactly its own. A crossed wire would show
// up as a caller reading another command's outcome.
func TestConcurrentDeviceCommandsNeverCrossWires(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	id, _, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	// The device plane is the real one, but the ANSWER has to be
	// command-distinguishable for a crossed wire to be detectable, so this
	// relay's controller fails every dispatch with a code naming the command
	// it was handed. The surface still resolves each command against the real
	// media-player vocabulary first (REL-113).
	dialWithDevicePlane(t, h, store, echoingController{}, id.RelayID)
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() >= 1 },
		"authenticated connection never registered with the app peer")

	commands := []string{"home", "launch", "keypress", "power"}
	const rounds = 12
	var wg sync.WaitGroup
	fail := make(chan string, len(commands)*rounds)
	for round := 0; round < rounds; round++ {
		for _, command := range commands {
			wg.Add(1)
			go func(command string, round int) {
				defer wg.Done()
				ctx, cancel := sendCtx(t)
				defer cancel()
				// Alternate the target device so the per-device lock (REL-115)
				// does not fully serialize the fleet.
				entity := fixtureEntityA
				if round%2 == 1 {
					entity = fixtureEntityB
				}
				res, err := h.connSrv.SendDeviceCommand(ctx, id.RelayID, "", wire.DeviceCommandBody{
					EntityID: entity,
					Command:  command,
				})
				if err != nil {
					fail <- fmt.Sprintf("SendDeviceCommand(%s): %v", command, err)
					return
				}
				if res.Error == nil {
					fail <- fmt.Sprintf("SendDeviceCommand(%s): result carried no error: %+v", command, res)
					return
				}
				if res.Error.Message != command {
					fail <- fmt.Sprintf("command %q received the result of %q — correlation crossed wires", command, res.Error.Message)
				}
			}(command, round)
		}
	}
	wg.Wait()
	close(fail)
	for msg := range fail {
		t.Error(msg)
	}
	if n := h.connSrv.PendingCommandCount(); n != 0 {
		t.Fatalf("PendingCommandCount = %d after every exchange completed, want 0", n)
	}
}

// echoingController answers every dispatch with a typed error whose message is
// the command name it was handed — the marker that makes a crossed correlation
// wire observable at the caller.
type echoingController struct{}

func (echoingController) Dispatch(entityID, command string, params map[string]any) error {
	return &deviceplane.ControllerError{Code: "COMMAND_TARGET_UNREACHABLE", Message: command}
}
