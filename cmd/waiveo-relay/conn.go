// conn.go is the relay binary's side of the relay/1 persistent connection:
// the dial-with-retry boot handshake (relayconn.Dial subsumes challenge →
// hello → hello-ack, REL-030–039), the frames pull that feeds the shared
// verify chain (desiredstate.VerifyAndApply) and acknowledges the outcome on
// the wire (state.ack, REL-054), and the small holders that let the
// reconnect supervisor hand a fresh connection to the nudge-driven live
// apply path (livepull.go) without either racing the other.
package main

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// dialWithRetry performs the relay/1 connection handshake against the
// co-located app peer over the persistent transport, retrying a transport
// failure (e.g. the feeder's listener not up yet) until enrollRetryBudget
// elapses — mirroring enrollWithRetry's tolerance of the dev harness
// starting both binaries with no ordering. A typed *relayconn.Refusal (a
// channel-binding, revocation, or protocol-version refusal) is decisive and
// returned immediately, never retried within this bounded budget: the app
// peer answered and declined. (The reconnect supervisor in main separately
// decides whether that decisive refusal is worth retrying indefinitely in
// the background — relayconn.RefusalIsRecoverable.)
func dialWithRetry(dial func() (*relayconn.Client, error)) (*relayconn.Client, error) {
	deadline := time.Now().Add(enrollRetryBudget)
	var lastErr error
	for {
		client, err := dial()
		if err == nil {
			return client, nil
		}
		var refused *relayconn.Refusal
		if errors.As(err, &refused) {
			return nil, err // decisive refusal, not a transport hiccup
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		time.Sleep(enrollRetryInterval)
	}
}

// pullOverFrames performs one state.pull over the live connection (REL-050)
// and runs the reply through the SAME verify chain the HTTP-era pull used
// (desiredstate.VerifyAndApply — REL-060 structural gate, hash recompute,
// signature verification against the enrollment-anchored trust anchor,
// generation monotonicity, atomic persist). since > 0 rides as the REL-050
// since_generation claim, so an unchanged generation answers state.unchanged
// — surfaced as an Applied carrying exactly that generation, which the
// caller's monotonic guard treats as the no-op it is. After a snapshot
// apply, the outcome is acknowledged on the wire with state.ack correlated
// to the pull exchange (REL-054): the advanced applied_generation on
// success, or the taxonomy error plus the UNADVANCED prior generation on a
// rejected snapshot (REL-072). Ack delivery is best-effort — the apply
// outcome, not the ack, is what the relay's own serving state follows.
func pullOverFrames(client *relayconn.Client, store *identity.Store, since int64) (desiredstate.Applied, error) {
	var sincePtr *int64
	if since > 0 {
		sincePtr = &since
	}
	reply, err := client.Pull("", sincePtr)
	if err != nil {
		return desiredstate.Applied{}, err
	}

	switch reply.Type {
	case wire.FrameTypeStateUnchanged:
		var body wire.StateUnchangedBody
		if err := reply.DecodeBody(&body); err != nil {
			return desiredstate.Applied{}, err
		}
		return desiredstate.Applied{Generation: body.Generation}, nil

	case wire.FrameTypeStateSnapshot:
		body, rawSections, err := relayconn.SnapshotFromFrame(reply)
		if err != nil {
			return desiredstate.Applied{}, err
		}
		applied, verifyErr := desiredstate.VerifyAndApply(store, body, rawSections)
		if verifyErr != nil {
			// REL-054/072: acknowledge the REJECTED snapshot with the taxonomy
			// error and the unadvanced last-applied generation.
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

	default:
		return desiredstate.Applied{}, fmt.Errorf("state.pull answered with unexpected frame type %q", reply.Type)
	}
}

// ackErrorCode maps a desiredstate verify failure to the relay/1 Error-
// taxonomy code a rejected snapshot's state.ack error carries (REL-054/072).
func ackErrorCode(err error) string {
	switch {
	case errors.Is(err, desiredstate.ErrSnapshotSignatureInvalid):
		return "SNAPSHOT_SIGNATURE_INVALID"
	case errors.Is(err, desiredstate.ErrSnapshotHashMismatch),
		errors.Is(err, desiredstate.ErrSectionsIncomplete):
		// The snapshot failed its own type's minimum shape/integrity —
		// the taxonomy's MALFORMED_MESSAGE row (REL-002).
		return "MALFORMED_MESSAGE"
	default:
		return "INTERNAL"
	}
}

// relayDialConfig is the ONE relay/1 connection configuration this binary
// dials with — the boot dial and every supervisor redial alike. It is a named
// function rather than a literal inside run() for a reason the device plane
// paid for: a Config assembled inline can silently omit a callback, and
// omitting OnDeviceCommand meant every operator command the app peer dispatched
// was answered "this relay has no device plane wired" by a relay that had one.
// Assembled here, the set of callbacks a dial carries is a thing a test can
// hold and assert on (conn_test.go), and adding a callback to the connection is
// a change to one function rather than to a literal in the middle of boot.
//
// Both callbacks are indirection sinks rather than the handlers themselves,
// because both handlers are built AFTER the first dial: nudges routes REL-057's
// state.changed to the live apply path, and commands routes REL-112's
// device.command to the automation host's command surface.
func relayDialConfig(cfg config, store *identity.Store, nudges *nudgeSink, commands *deviceCommandSink) relayconn.Config {
	return relayconn.Config{
		URL:                 cfg.feederURL,
		Store:               store,
		Declaration:         relayHelloDeclaration(cfg),
		OnGenerationAdvance: nudges.deliver,
		OnDeviceCommand:     commands.execute,
	}
}

// connHolder hands the reconnect supervisor's CURRENT live connection to the
// pull path: OnConnected stores each freshly authenticated client here, and
// rePuller's pull closure reads whatever is stored at pull time — nil while
// disconnected, which a tick logs as a non-fatal pull failure (REL-055).
type connHolder struct {
	mu sync.Mutex
	c  *relayconn.Client
}

func (h *connHolder) set(c *relayconn.Client) {
	h.mu.Lock()
	h.c = c
	h.mu.Unlock()
}

func (h *connHolder) get() *relayconn.Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.c
}

// nudgeSink decouples the connection's state.changed dispatcher (REL-057,
// wired into relayconn.Config at dial time — before the live apply path
// exists) from the live apply path itself (installed once the boot pull and
// serving stacks are up). A nudge arriving before a handler is installed is
// dropped harmlessly: the supervisor's pull-on-reconnect and the handler's
// own first tick recover it (REL-057 best-effort delivery).
type nudgeSink struct {
	mu sync.Mutex
	fn func(generation int64)
}

func (n *nudgeSink) set(fn func(int64)) {
	n.mu.Lock()
	n.fn = fn
	n.mu.Unlock()
}

// deliver is the relayconn.Config.OnGenerationAdvance callback: it runs on
// the connection's dedicated nudge-dispatcher goroutine, so the handler may
// pull synchronously.
func (n *nudgeSink) deliver(generation int64) {
	n.mu.Lock()
	fn := n.fn
	n.mu.Unlock()
	if fn != nil {
		fn(generation)
	}
}

// deviceCommandSink is the nudgeSink of the device plane: it decouples the
// connection's inbound `device.command` handler (REL-112, wired into
// relayconn.Config at dial time) from the automation host that executes one,
// which is built later in boot — after enrollment, after the first pull, and
// after the operational store is open.
//
// It exists because that ordering is exactly what left OnDeviceCommand nil in
// the shipped binary while every test wired it: at the one place Config is
// built there is no host to point at yet, and a callback that cannot be written
// at construction is a callback that quietly never gets written. The seam turns
// "wire it later" from a thing a reader has to notice into a thing the type
// system asks for — Dial always gets a non-nil handler, and `set` is the only
// way anything ever answers a command.
//
// Handing the connection this sink rather than the host directly has a second
// payoff: every redial reuses the SAME sink, so a reconnect cannot lose the
// device plane the way it would if each Dial had to re-close over the host.
type deviceCommandSink struct {
	mu sync.Mutex
	fn func(wire.DeviceCommandBody) wire.DeviceCommandResultBody
}

func (d *deviceCommandSink) set(fn func(wire.DeviceCommandBody) wire.DeviceCommandResultBody) {
	d.mu.Lock()
	d.fn = fn
	d.mu.Unlock()
}

// execute is the relayconn.Config.OnDeviceCommand callback. It runs on the
// connection's own per-command goroutine (never the read loop), so a handler
// that waits on a physical device cannot stall the connection.
//
// A command that arrives before the host is up — the boot window between the
// first Dial and bootAutomationStack, and the whole of a boot whose automation
// stack failed — is answered with a typed refusal rather than dropped. REL-112
// requires a result for every command, and "the device plane is not up yet" is
// a true, retryable thing to say; silence would leave the app peer's own
// operation hanging until its timeout with nothing to report.
func (d *deviceCommandSink) execute(body wire.DeviceCommandBody) wire.DeviceCommandResultBody {
	d.mu.Lock()
	fn := d.fn
	d.mu.Unlock()
	if fn == nil {
		return wire.NewDeviceCommandError("INTERNAL",
			"this relay's device plane is not up yet; retry once it has finished starting")
	}
	return fn(body)
}
