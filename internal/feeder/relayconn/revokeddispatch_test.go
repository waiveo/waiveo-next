package relayconn_test

import (
	"errors"
	"testing"
	"time"

	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// TestRevokedRelayIsNotHandedADeviceCommand pins REL-016 on the dispatch path.
//
// The connection-time and mid-session halves are already covered. This is the
// third place the check runs, and it was held by nothing: an operator's command
// dispatched to a relay whose certificate has been revoked.
//
// Asserting the error alone would prove very little — SendDeviceCommand answers
// ErrRelayNotConnected here, which is the same answer it gives for a relay that
// simply is not connected, and that path is well covered. What distinguishes
// this rule is that the command MUST NOT REACH THE RELAY: a revoked relay is
// "refused and disconnected rather than handed an operator's command to
// execute". So the test asserts the device controller saw no dispatch.
func TestRevokedRelayIsNotHandedADeviceCommand(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	id, _, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	controller := &recordingController{}
	client := dialWithDevicePlane(t, h, store, controller, id.RelayID)
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() >= 1 },
		"authenticated connection never registered with the app peer")

	// The control comes FIRST, so the fixture is proven to dispatch before
	// revocation is the reason it does not.
	ctx, cancel := sendCtx(t)
	defer cancel()
	if _, err := h.connSrv.SendDeviceCommand(ctx, id.RelayID, "01J8Z4K4N5P6Q7R8S9T0V1W3C0",
		wire.DeviceCommandBody{EntityID: fixtureEntityA, Command: "launch"}); err != nil {
		t.Fatalf("pre-revocation command: %v", err)
	}
	if n := len(controller.dispatched()); n != 1 {
		t.Fatalf("pre-revocation dispatch count = %d, want 1", n)
	}

	relayID, serial := certSerial(t, store)
	if !h.enrollSrv.Revoke(relayID, serial) {
		t.Fatalf("Revoke(%s, %s) found no issuance on record", relayID, serial)
	}

	ctx2, cancel2 := sendCtx(t)
	defer cancel2()
	_, err = h.connSrv.SendDeviceCommand(ctx2, id.RelayID, "01J8Z4K4N5P6Q7R8S9T0V1W3C1",
		wire.DeviceCommandBody{EntityID: fixtureEntityA, Command: "launch"})
	if err == nil {
		t.Fatal("a command was dispatched to a relay whose certificate is revoked (REL-016)")
	}
	if !errors.Is(err, feederrelayconn.ErrRelayNotConnected) {
		t.Errorf("refused with %v, want ErrRelayNotConnected — a revoked relay is reported the same way an "+
			"absent one is, so a caller cannot learn from the difference", err)
	}

	// The assertion that separates this rule from "the relay was not connected":
	// the command never reached the device.
	if n := len(controller.dispatched()); n != 1 {
		t.Errorf("the device controller saw %d dispatch(es) after revocation, want the pre-revocation 1 — the "+
			"command was handed to a relay the app peer had already revoked", n)
	}

	// And the connection is dropped rather than left usable.
	select {
	case <-client.Done():
	case <-time.After(5 * time.Second):
		t.Error("the connection stayed open after a command was refused for revocation (REL-016 disconnects)")
	}
}
