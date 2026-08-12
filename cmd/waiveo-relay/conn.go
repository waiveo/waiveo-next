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

	"github.com/maaxton/waiveo-next/internal/relay/clocktrust"
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

// clear drops the held connection the moment the supervisor reports it dead
// (OnDisconnected), so every reader — the pull closure, the candidate
// reporter, the screen-status reporter, the redemption drainer — sees the
// nil each of them already documents as "the ordinary offline case" instead
// of a corpse.
//
// Its absence is HV-22's most operator-visible symptom. This holder was only
// ever written by OnConnected, so a dead client stayed here until a
// SUCCESSFUL redial replaced it — which is exactly the case that does not
// happen when the app peer cannot be reached. The reporters went on writing
// to a socket the supervisor had already abandoned, at 10s intervals, and
// the only thing in the whole log was 1074 broken-pipe lines naming a port
// nothing was still supervising. The supervisor cleared ITS reference
// (clearClient) and this second holder of the same fact was never told: two
// records of one truth, one of them updated.
func (h *connHolder) clear() {
	h.mu.Lock()
	h.c = nil
	h.mu.Unlock()
}

func (h *connHolder) get() *relayconn.Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.c
}

// connReporter turns the reconnect supervisor's lifecycle edges into an
// account of the app-peer connection an operator can act on.
//
// It exists because HV-22's real damage was not the disconnection — the
// supervisor handled that correctly and redialled on schedule — but that
// NOTHING SAID SO. A relay cut off from its app peer for two and a half
// hours, with its wall dark, logged not one line about the connection: not
// the loss, not a single one of the ~300 redials, not the reason each was
// refused, not that the reason was one no amount of waiting can fix. The
// only trace anywhere was write errors from an unrelated goroutine aimed at
// a socket the supervisor had already given up on, which is worse than
// silence: it pointed at the wrong thing.
//
// Two rules govern what it prints, both learned from that log:
//
//   - VOLUME IS THE ENEMY OF ATTENTION. 1074 identical lines is not
//     reporting, it is noise that buries the report. Failures are logged in
//     full at first and then thinned (reportAttempt), so an outage of any
//     length costs a handful of lines that each say how long it has been
//     going on.
//   - A CONDITION THAT RETRYING CANNOT FIX MUST SAY SO. A trust-pin
//     mismatch means the peer answering this address is not the one this
//     relay enrolled with; REL-137 says the remedy is re-anchoring at
//     enrollment, never waiting. Reporting it as one more retryable
//     hiccup — which is how the supervisor's taxonomy classifies it, and
//     rightly, since the wrong peer may yet be replaced by the right one —
//     would leave an operator watching a counter climb toward nothing.
type connReporter struct {
	logf func(format string, v ...any)
	now  func() time.Time

	mu sync.Mutex
	// downSince is when the current outage began — the boot time for a
	// relay that has never connected, so the first recovery line can still
	// say how long the fleet went unmanaged. Zeroed while connected.
	downSince time.Time
	attempts  int
	// pinToldOnce keeps the REL-137 remedy block to one printing per
	// outage: it is several lines of instruction, and repeating it on every
	// thinned attempt would recreate the volume problem it exists inside.
	pinToldOnce bool
}

func newConnReporter(logf func(string, ...any), now func() time.Time) *connReporter {
	return &connReporter{logf: logf, now: now, downSince: now()}
}

// disconnected reports a live connection's death, naming which half of the
// transport noticed and how long the connection had lasted. err comes
// straight from relayconn.Client.Err, which labels the half (read, write, or
// heartbeat) precisely so this line can be specific.
// One outage has exactly two edges, and each owns its own bookkeeping:
// disconnected STARTS the outage clock, connected ENDS it and clears
// everything the outage accumulated. Resetting the counters here as well
// would be redundant — every disconnect is preceded by a connect, which has
// already cleared them — and a redundant reset is a line no test can
// falsify. (Confirmed by mutation: deleting a duplicate reset here survived
// the whole suite, because the real one lives in connected.)
func (r *connReporter) disconnected(err error, connectedFor time.Duration) {
	r.mu.Lock()
	r.downSince = r.now()
	r.mu.Unlock()
	r.logf("waiveo-relay: app-peer connection LOST after %s up (%v) — screens keep playing the last applied generation offline (REL-055/061), but nothing new reaches them until it is back; re-dialling", connectedFor.Round(time.Second), err)
}

// connectFailed reports one failed redial, thinned by reportAttempt.
func (r *connReporter) connectFailed(err error, consecutive int, retryIn time.Duration) {
	r.mu.Lock()
	r.attempts = consecutive
	down := r.now().Sub(r.downSince)
	tellPin := isTrustPinMismatch(err) && !r.pinToldOnce
	if tellPin {
		r.pinToldOnce = true
	}
	r.mu.Unlock()

	if tellPin {
		// Ahead of the thinning check, and unconditional: this is the one
		// cause where the attempt count is beside the point.
		// Opens with "REFUSED" deliberately. Beyond being the accurate word
		// for a handshake this relay declined, it is one of
		// internal/app/platformlog's errorMarkers — so the day a relay's log
		// reaches the console's log page (it does not today; the buffer tees
		// only the FEEDER's own stderr), the one line here that needs an
		// operator does not arrive filed as info. The rest of this block
		// contains no marker at all: "does not match the pin" reads as
		// perfectly calm text to a substring heuristic.
		r.logf("waiveo-relay: app-peer connection REFUSED at TLS — the app peer at this address is NOT the one this relay enrolled with; its TLS leaf key does not match the enrollment-anchored pin (REL-137).\n"+
			"    Re-dialling will not fix this: REL-137 requires re-anchoring at enrollment, so either the app peer's identity was replaced (re-enroll this relay), or WAIVEO_FEEDER_URL is reaching a DIFFERENT app peer than the intended one — a second process holding the same address answers the dial and presents its own identity.\n"+
			"    Detail: %v", err)
	}
	if !reportAttempt(consecutive) {
		return
	}
	r.logf("waiveo-relay: app peer unreachable — connection attempt %d failed, %s offline so far, retrying in %s: %v",
		consecutive, down.Round(time.Second), retryIn.Round(time.Millisecond), err)
}

// connected reports recovery, and deliberately says nothing when there was
// no outage to recover from: the boot connection is not news, and a line
// that prints on every healthy start is a line an operator learns to skip.
func (r *connReporter) connected() {
	r.mu.Lock()
	attempts := r.attempts
	down := r.now().Sub(r.downSince)
	r.downSince = time.Time{}
	r.attempts = 0
	r.pinToldOnce = false
	r.mu.Unlock()

	if attempts == 0 {
		return
	}
	r.logf("waiveo-relay: app-peer connection RE-ESTABLISHED after %d failed attempt(s), %s offline — pulling desired state and re-reporting screens",
		attempts, down.Round(time.Second))
}

// reportAttempt decides which failed attempts get a line: the first three,
// then powers of two.
//
// The shape matters more than the constants. A fixed interval cannot serve
// both ends of the range — report every attempt and a two-hour outage buys
// 300 identical lines (the disease HV-22 exhibited at 1074), report one in
// fifty and a brief blip goes unrecorded entirely. Doubling gives full
// detail exactly where an operator is watching (the first seconds) and a
// logarithmic tail that still marks the passage of hours: ~10 lines for the
// 2h33m outage that produced this defect, each carrying the elapsed time.
func reportAttempt(n int) bool {
	if n <= 3 {
		return true
	}
	return n&(n-1) == 0
}

// isTrustPinMismatch reports whether a failed connection attempt died on
// REL-137's SPKI pin.
//
// errors.Is against the sentinel rather than a string match: the error
// travels out of crypto/tls's VerifyPeerCertificate hook and through
// relayconn's %w wrapping, and both preserve the chain. (The supervisor's
// renewOnExpiredLeafHandshake next door DOES match on a string, because the
// expired-leaf refusal it classifies is an unexported *tls.permanentError
// minted inside the standard library with no sentinel to match — the
// difference is worth noticing, not copying.)
func isTrustPinMismatch(err error) bool {
	return errors.Is(err, clocktrust.ErrAppPeerKeyMismatch)
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
