package relay1

import (
	"errors"
	"fmt"
	"sync"

	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
	"github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// RealRelayClient is the first-photon RelayClient: it delegates straight to
// the real relay-side implementations (internal/relay/enroll for the HTTP
// bootstrap; internal/relay/relayconn + internal/relay/desiredstate for
// everything on the persistent connection) — the code that ships. It is the
// concrete target the relay/1 driver validates now; the teeth meta-test
// swaps in a broken client to prove the oracle can fail.
type RealRelayClient struct{}

// NewRealRelayClient returns the first-photon relay client.
func NewRealRelayClient() RealRelayClient { return RealRelayClient{} }

// Name implements RelayClient.
func (RealRelayClient) Name() string { return "real-relay" }

// Enroll implements RelayClient via internal/relay/enroll.Run — the HTTP
// bootstrap that CANNOT ride the authenticated connection (REL-010/026).
func (RealRelayClient) Enroll(feederBaseURL string, store *identity.Store) error {
	return relayenroll.Run(feederBaseURL, store)
}

// driverDeclaration is the standard hello declaration this client's
// connection-based operations dial with when the case under drive does not
// supply its own (Pull) — matching the relay binary's own defaults.
var driverDeclaration = hello.Declaration{
	ProtocolVersion: "1.0",
	Features:        []string{"telemetry.latest_only_v1"},
	ClockState:      hello.ClockState{State: "untrusted", Source: "cold_boot"},
}

// Pull implements RelayClient over the persistent connection: one fresh
// authenticated dial (challenge → hello → hello-ack inside relayconn.Dial),
// one state.pull, and the shared verify-then-apply gate under test
// (desiredstate.VerifyAndApply). The apply outcome is acknowledged on the
// wire with a correlated state.ack (REL-054) — the advanced generation on
// success, the taxonomy error plus the UNADVANCED prior generation on a
// rejected snapshot (REL-072) — exactly as cmd/waiveo-relay does, so the
// driver can diff the wire acknowledgment through Feeder.LastStateAck.
func (RealRelayClient) Pull(connectURL string, store *identity.Store) (desiredstate.Applied, error) {
	client, err := relayconn.Dial(relayconn.Config{
		URL: connectURL, Store: store, Declaration: driverDeclaration,
	})
	if err != nil {
		return desiredstate.Applied{}, fmt.Errorf("real-relay: Pull: dial: %w", err)
	}
	defer client.Close()

	reply, err := client.Pull("", nil)
	if err != nil {
		return desiredstate.Applied{}, fmt.Errorf("real-relay: Pull: state.pull: %w", err)
	}
	body, rawSections, err := relayconn.SnapshotFromFrame(reply)
	if err != nil {
		return desiredstate.Applied{}, fmt.Errorf("real-relay: Pull: %w", err)
	}
	applied, verifyErr := desiredstate.VerifyAndApply(store, body, rawSections)
	if verifyErr != nil {
		priorGen, _, _, _ := store.LastAppliedGeneration()
		_ = client.SendStateAck(reply.ID, reply.TraceID, wire.StateAckBody{
			AppliedGeneration: priorGen,
			Error:             &wire.AckErrorBody{Code: ackErrorCode(verifyErr), Message: verifyErr.Error()},
		})
		return desiredstate.Applied{}, verifyErr
	}
	_ = client.SendStateAck(reply.ID, reply.TraceID, wire.StateAckBody{
		AppliedGeneration: applied.Generation,
	})
	return applied, nil
}

// ackErrorCode maps a desiredstate verify failure to the relay/1
// Error-taxonomy code a rejected snapshot's state.ack error carries
// (REL-054/072) — mirroring cmd/waiveo-relay's own mapping.
func ackErrorCode(err error) string {
	switch {
	case errors.Is(err, desiredstate.ErrSnapshotSignatureInvalid):
		return "SNAPSHOT_SIGNATURE_INVALID"
	case errors.Is(err, desiredstate.ErrSnapshotHashMismatch),
		errors.Is(err, desiredstate.ErrSectionsIncomplete):
		return "MALFORMED_MESSAGE"
	default:
		return "INTERNAL"
	}
}

// Hello implements RelayClient over the persistent connection: one fresh
// authenticated dial whose handshake IS the hello exchange under test
// (REL-030–041). The hello-ack is reconstructed from the observed wire
// frame — envelope relay_id included (REL-005), never inferred from local
// state — so the driver diffs what actually crossed the wire. A typed
// refusal surfaces as *relayconn.Refusal carrying the taxonomy code.
func (RealRelayClient) Hello(connectURL string, store *identity.Store, decl hello.Declaration) (hello.HelloAck, error) {
	capture := &helloAckCapture{}
	client, err := relayconn.Dial(relayconn.Config{
		URL: connectURL, Store: store, Declaration: decl,
		ObserveFrame: capture.observe,
	})
	if err != nil {
		return hello.HelloAck{}, err
	}
	defer client.Close()

	frame, ok := capture.frame()
	if !ok {
		return hello.HelloAck{}, fmt.Errorf("real-relay: Hello: dial succeeded but no hello-ack frame was observed")
	}
	var body hello.HelloAckBody
	if err := frame.DecodeBody(&body); err != nil {
		return hello.HelloAck{}, fmt.Errorf("real-relay: Hello: %w", err)
	}
	return hello.HelloAck{Type: frame.Type, RelayID: frame.RelayID, Body: body}, nil
}

// helloAckCapture records the handshake's hello-ack frame off the client's
// frame-observation seam.
type helloAckCapture struct {
	mu  sync.Mutex
	ack wire.Frame
	ok  bool
}

func (c *helloAckCapture) observe(sent bool, f wire.Frame) {
	if sent || f.Type != wire.FrameTypeHelloAck {
		return
	}
	c.mu.Lock()
	c.ack, c.ok = f, true
	c.mu.Unlock()
}

func (c *helloAckCapture) frame() (wire.Frame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ack, c.ok
}

// ReEnroll implements RelayClient via internal/relay/reenroll.ReEnroll — the
// Expired-certificate re-enrollment client under test (REL-020/027), on the
// HTTP bootstrap surface by design (a relay whose certificate expired cannot
// authenticate the mTLS connection, REL-026).
func (RealRelayClient) ReEnroll(feederBaseURL string, store *identity.Store) error {
	return reenroll.ReEnroll(feederBaseURL, store)
}
