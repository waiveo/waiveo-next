package eventingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
)

// delivery_test.go covers the POST-APPEND handoff (EventDeliverer): what the app
// peer acts on an ingested durable event through.
//
// The placement of that handoff is the whole subject. It used to be folded into
// the EventSink, so it ran with this ingest's own mutex held — and what the app
// peer does there is fire every matching rules/1 `event` trigger, which lists
// and parses the deployment's automations and then dispatches device commands
// over a relay connection with a 15-second timeout. One unreachable device
// therefore head-of-line blocked the ONE durable telemetry ingest every relay in
// the deployment shares: `device.heartbeat`, `entity.state_changed` and
// `content.played` delivery stalled fleet-wide behind a single screen's button
// press. This is a liveness hazard on exactly the channel REL-093/103 exist to
// protect.
//
// Moving it off the lock must not cost the other property, which is why both are
// asserted here: the ACK CURSOR must not outrun the work. REL-092's ack is what
// makes the relay discard its retained entries, so a cursor covering an entry
// still being delivered lets a crash lose a press with the ack already given.
//
// That property is about the CURSOR, not about the request, and the difference
// is the whole of what this file's second test was rewritten to measure. The
// first attempt held the response open until delivery finished — while advancing
// the cursor under the lock BEFORE delivery ran. So the guarantee held for the
// push that did the work and failed for every other reader of the cursor,
// including the one that matters most: the relay's own retry, which it sends 2
// seconds after its 5-second client deadline elapses and which was answered 200
// with an ack covering an in-flight entry.

// blockingDeliverer is an EventDeliverer whose FIRST call parks until released.
// Only the first, so a second, unrelated push is delivered normally and can
// complete — which is the thing being measured.
type blockingDeliverer struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}

	mu   sync.Mutex
	seen []events.Envelope
}

func newBlockingDeliverer() *blockingDeliverer {
	return &blockingDeliverer{entered: make(chan struct{}), release: make(chan struct{})}
}

func (d *blockingDeliverer) deliver(_ context.Context, env events.Envelope) {
	d.mu.Lock()
	d.seen = append(d.seen, env)
	d.mu.Unlock()
	if d.calls.Add(1) == 1 {
		close(d.entered)
		<-d.release
	}
}

func (d *blockingDeliverer) delivered() []events.Envelope {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]events.Envelope(nil), d.seen...)
}

// TestDeliveryDoesNotHoldTheIngestLock is the liveness assertion, in the shape
// the hazard actually takes: one relay's push is inside its deliverer, and a
// DIFFERENT relay's push must still be ingested and acked.
//
// Without the fix this deadlocks until the first deliverer returns — which on a
// real box is up to deviceCommandTimeout (15s) per matching rule, per entry.
func TestDeliveryDoesNotHoldTheIngestLock(t *testing.T) {
	log := events.NewEventLog(0)
	d := newBlockingDeliverer()
	h := New(log, siteScope, seqIDs(), testWallMs, testRelay().Authorizer(), d.deliver)

	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, pushRequest(t, pushBatch(autoEntry(1, validAutomationRunPayload()))))
	}()
	<-d.entered
	defer close(d.release)

	// A second, unrelated push — a heartbeat, the highest-volume durable schema
	// on a real box — while the first is still inside its deliverer.
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, pushRequest(t, pushBatch(telemetry.Entry{
			Seq: 2, Schema: events.SchemaDeviceHeartbeat, Payload: validDeviceHeartbeatPayload(),
		})))
		done <- rec.Code
	}()

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("the second push answered %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second relay's telemetry push could not be ingested while an unrelated event was being delivered — " +
			"delivery is holding the ingest lock, so one slow rule run stalls durable telemetry for the whole deployment (REL-093/103)")
	}
}

// TestTheAckCursorDoesNotOutrunDelivery is the other half, measured through the
// path the previous version of this test did not model: a SECOND push arriving
// while the first push's delivery is still in flight.
//
// That is not a contrived case, it is the relay's ordinary behaviour. Its
// telemetry client gives up on a push after 5 seconds (telemetryPushTimeout) and
// the flush loop re-pushes every 2 (telemetryFlushInterval), while ONE device
// command inside a fired rule is bounded at 15 seconds and applied serially per
// target. So a rule run reliably outlives the request that started it, and the
// retry is what the ingest answers next.
//
// The measured probe against the previous implementation was:
//
//	RETRY RESPONSE: status=200 ack_through_seq=1 — delivery of seq 1 is STILL IN FLIGHT
//
// The relay applies that ack to its buffer (Channel.applyAck → Buffer.applyAck →
// PruneTelemetry) and discards the retained entry. A crash during the remainder
// of the delivery then loses the press with the ack already given — exactly what
// REL-103 forbids.
func TestTheAckCursorDoesNotOutrunDelivery(t *testing.T) {
	log := events.NewEventLog(0)
	d := newBlockingDeliverer()
	h := New(log, siteScope, seqIDs(), testWallMs, testRelay().Authorizer(), d.deliver)

	// The first push. It returns promptly — the request is bounded, which is the
	// other half of the fix — so this is not backgrounded.
	if ack := postBatch(t, h, pushBatch(autoEntry(1, validAutomationRunPayload()))); ack.AckThroughSeq != 0 {
		t.Fatalf("the push that STARTED a delivery acked through %d, want 0 — "+
			"the entry it appended has not been delivered yet, so nothing may be acked", ack.AckThroughSeq)
	}
	<-d.entered // delivery of seq 1 is now in flight and parked

	// THE RETRY. Same batch, re-pushed because the relay never saw a response it
	// was willing to wait for. Its seq is already known, so nothing is
	// re-appended and nothing is re-delivered — but the ack it is answered with
	// must still not cover seq 1.
	retry := postBatch(t, h, pushBatch(autoEntry(1, validAutomationRunPayload())))
	if retry.AckThroughSeq >= 1 {
		t.Fatalf("a retry was answered ack_through_seq=%d while delivery of seq 1 was still in flight — "+
			"the relay prunes its retained entry on that value, so a crash during the rest of the delivery "+
			"loses the press with the ack already given (REL-092/103)", retry.AckThroughSeq)
	}

	// An UNRELATED, later entry must not drag the cursor over the pending one
	// either: the cursor is a prefix promise, and acking 2 would tell the relay
	// it may discard 1.
	if ack := postBatch(t, h, pushBatch(telemetry.Entry{
		Seq: 2, Schema: events.SchemaDeviceHeartbeat, Payload: validDeviceHeartbeatPayload(),
	})); ack.AckThroughSeq >= 1 {
		t.Fatalf("a later entry advanced the cursor to %d over a pending seq 1 — the ack is a prefix promise", ack.AckThroughSeq)
	}

	// Release it: once delivery concludes the cursor may move, and the next push
	// carries the advanced value to the relay.
	close(d.release)
	if err := h.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if ack := postBatch(t, h, pushBatch()); ack.AckThroughSeq != 2 {
		t.Fatalf("after every delivery concluded the cursor is %d, want 2 — "+
			"an ack that never advances leaves the relay retaining forever", ack.AckThroughSeq)
	}
}

// TestTheRequestIsBoundedByTheDeliveryQueue: a reconnect batch larger than the
// queue is taken in part, not in whole. The rest is left un-acked, which is what
// makes the relay re-push it — the relay's own 1024-entry durable buffer is the
// backpressure sink, and it is built for exactly this.
//
// Without a bound, a reconnect after an outage delivers the whole backlog inside
// one request: the case where the work is largest is the case where the client
// deadline is guaranteed to elapse.
func TestTheRequestIsBoundedByTheDeliveryQueue(t *testing.T) {
	log := events.NewEventLog(0)
	d := newBlockingDeliverer()
	h := New(log, siteScope, seqIDs(), testWallMs, testRelay().Authorizer(), d.deliver)

	// One more entry than the queue can hold, all in one push.
	entries := make([]telemetry.Entry, 0, deliveryQueueDepth+8)
	for i := 1; i <= deliveryQueueDepth+8; i++ {
		entries = append(entries, autoEntry(int64(i), validAutomationRunPayload()))
	}
	postBatch(t, h, pushBatch(entries...))
	<-d.entered
	defer close(d.release)

	h.mu.Lock()
	taken := h.receivedThrough
	pending := len(h.pending)
	h.mu.Unlock()

	if pending > deliveryQueueDepth {
		t.Fatalf("%d envelopes are pending delivery, want at most %d — the queue's depth is the request's only bound", pending, deliveryQueueDepth)
	}
	if taken >= int64(len(entries)) {
		t.Fatalf("the whole %d-entry batch was taken in one request (received through %d); "+
			"a reconnect backlog must be taken a bounded prefix at a time", len(entries), taken)
	}
	if taken == 0 {
		t.Fatal("no entry at all was taken from the batch; backpressure must not wedge the channel")
	}
}

// TestOnlyAppendedEnvelopesAreDelivered: an EVT-013 drop is delivered to nobody.
// The ordering — append first, deliver after — is what makes "what fired a rule"
// and "what the durable log holds" the same set; a rule that ran on a record no
// subsequent reader can find is unexplainable from the log.
func TestOnlyAppendedEnvelopesAreDelivered(t *testing.T) {
	log := events.NewEventLog(0)
	var seen []events.Envelope
	h := New(log, siteScope, seqIDs(), testWallMs, testRelay().Authorizer(),
		func(_ context.Context, env events.Envelope) { seen = append(seen, env) })

	postBatch(t, h, pushBatch(
		autoEntry(1, validAutomationRunPayload()),
		// Unclassed schema: dropped and logged (EVT-013), never appended.
		telemetry.Entry{Seq: 2, Schema: "not.a.registered.schema", Payload: json.RawMessage(`{}`)},
		telemetry.Entry{Seq: 3, Schema: events.SchemaContentPlayed, Payload: validContentPlayedPayload()},
	))
	// Delivery runs off the request now, so the observation point is the drain,
	// not the response. Drain returning is the same instant the ack cursor is
	// allowed to cover the batch.
	if err := h.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("%d envelope(s) delivered, want 2 — the dropped record must reach no consumer", len(seen))
	}
	if seen[0].Schema != events.SchemaAutomationRun || seen[1].Schema != events.SchemaContentPlayed {
		t.Fatalf("delivered %q then %q, want automation.run then content.played — delivery must follow append order",
			seen[0].Schema, seen[1].Schema)
	}
	if got := log.After(""); len(got) != 2 {
		t.Fatalf("the log holds %d envelope(s) but %d were delivered; the two sets must be identical", len(got), len(seen))
	}
}

// TestARedeliveredSeqIsNotDeliveredTwice: the ingest is idempotent on the
// telemetry seq (REL-097/EVT-135), and that idempotence has to cover the
// consumer too. A relay that retried a push whose ack it never saw would
// otherwise fire every matching automation a second time — one press, two runs.
func TestARedeliveredSeqIsNotDeliveredTwice(t *testing.T) {
	log := events.NewEventLog(0)
	var seen []events.Envelope
	h := New(log, siteScope, seqIDs(), testWallMs, testRelay().Authorizer(),
		func(_ context.Context, env events.Envelope) { seen = append(seen, env) })

	batch := pushBatch(autoEntry(1, validAutomationRunPayload()))
	postBatch(t, h, batch)
	postBatch(t, h, batch)
	if err := h.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if len(seen) != 1 {
		t.Fatalf("%d deliveries for one seq pushed twice, want 1 — a redelivered press must not run its automation again", len(seen))
	}
}

// TestANilDelivererIsLegalAndSilent: an ingest with no app-side consumer is a
// legitimate deployment (every conformance driver is one), and it must ingest
// exactly as it did before the seam existed rather than panicking on a nil.
func TestANilDelivererIsLegalAndSilent(t *testing.T) {
	log := events.NewEventLog(0)
	h := New(log, siteScope, seqIDs(), testWallMs, testRelay().Authorizer(), nil)

	if ack := postBatch(t, h, pushBatch(autoEntry(1, validAutomationRunPayload()))); ack.AckThroughSeq != 1 {
		t.Fatalf("ack_through_seq = %d, want 1", ack.AckThroughSeq)
	}
	if got := log.After(""); len(got) != 1 {
		t.Fatalf("the log holds %d envelope(s), want 1", len(got))
	}
}

// poisonDeliverer is an EventDeliverer that PANICS on the nth envelope it is
// handed and records every one. It is a named type with a named method on
// purpose: its symbol is what the stack assertion below looks for, and a
// closure's would be the test function's.
type poisonDeliverer struct {
	poisonNth int32

	calls atomic.Int32
	mu    sync.Mutex
	seen  []string
}

func (d *poisonDeliverer) deliver(_ context.Context, env events.Envelope) {
	n := d.calls.Add(1)
	d.mu.Lock()
	d.seen = append(d.seen, env.ID)
	d.mu.Unlock()
	if n == d.poisonNth {
		panic("deliverer blew up")
	}
}

func (d *poisonDeliverer) delivered() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

// TestAPanickingDelivererDoesNotKillTheRunner is the containment assertion, and
// it is the half of round 2's async-delivery change that was not built.
//
// Moving delivery off the request removed the only recover that was ever above
// it: net/http installs one per connection, so on the synchronous design a
// panicking deliverer cost one HTTP 500 and a logged stack. On a goroutine
// spawned by startDeliveryLocked there is nothing above it at all, so the same
// panic terminates the feeder — relay connection, SSE hub and signage control
// plane with it. Measured before the fix, verbatim:
//
//	panic: deliverer blew up
//	  …eventingest.(*Ingest).drainQueue(…) eventingest.go:603
//	  created by …startDeliveryLocked in goroutine 42  eventingest.go:572
//
// And it does not stop at one crash: the poisoned entry is never acked, so the
// relay retains it (REL-097) and re-pushes 2 s after the restart, where both
// in-memory cursors have been reset — it is appended to the durable log a second
// time and panics again. A crash loop that duplicates an envelope per cycle.
//
// What this asserts, in the order the failure would show up: the process
// survives; the runner keeps serving the entries BEHIND the poisoned one in the
// same batch; the ack cursor advances past it rather than being pinned; the
// envelope is in the durable log either way; and the log line names the panic
// value AND carries real frames.
//
// The deliverer panics on its SECOND call, which is seq 2 — delivery follows
// append order globally because one runner performs it (drainQueue).
func TestAPanickingDelivererDoesNotKillTheRunner(t *testing.T) {
	log := events.NewEventLog(0)
	d := &poisonDeliverer{poisonNth: 2}
	h := New(log, siteScope, seqIDs(), testWallMs, testRelay().Authorizer(), d.deliver)

	var logMu sync.Mutex
	var logged []string
	h.logf = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	postBatch(t, h, pushBatch(
		autoEntry(1, validAutomationRunPayload()),
		autoEntry(2, validAutomationRunPayload()), // this one's delivery panics
		autoEntry(3, validAutomationRunPayload()),
	))
	if err := h.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// The runner survived the panic AND kept draining: seq 3 sits BEHIND the
	// poisoned entry in the same queue, so it is delivered only if the runner is
	// still alive after the panic.
	if got := d.delivered(); len(got) != 3 {
		t.Fatalf("%d envelope(s) delivered, want 3 — a panic in one delivery must not lose the runner, "+
			"and the entries queued behind it must still be served", len(got))
	}

	// A LATER push, after the runner has exited on an empty queue, must start a
	// fresh one and deliver normally: containment must not have left `draining`
	// or the queue in a state no subsequent batch can restart.
	postBatch(t, h, pushBatch(telemetry.Entry{
		Seq: 4, Schema: events.SchemaDeviceHeartbeat, Payload: validDeviceHeartbeatPayload(),
	}))
	if err := h.Drain(context.Background()); err != nil {
		t.Fatalf("Drain after the later push: %v", err)
	}
	if got := d.delivered(); len(got) != 4 {
		t.Fatalf("%d envelope(s) delivered after a later push, want 4 — the ingest must still deliver "+
			"batches that arrive after a poisoned entry", len(got))
	}

	// The poisoned seq is CONCLUDED, not held. Held would pin the ack cursor one
	// below it for the process's lifetime — the cursor is a prefix promise — so
	// seqs 3 and 4 would never be acked either and the relay would retain its
	// whole stream until REL-096 drop-oldest started discarding good entries.
	if ack := postBatch(t, h, pushBatch()); ack.AckThroughSeq != 4 {
		t.Fatalf("ack_through_seq = %d after a poisoned delivery, want 4 — a permanently-pending seq pins "+
			"the cursor for every later seq too, which is unbounded loss traded for one lost action", ack.AckThroughSeq)
	}

	// The EVENT is not lost, only its action: processOne appended it before it
	// was ever queued for delivery, which is what makes "concluded" honest.
	if got := log.After(""); len(got) != 4 {
		t.Fatalf("the durable log holds %d envelope(s), want 4 — the poisoned entry was appended before "+
			"delivery and must still be readable", len(got))
	}

	// And the panic is loud, with frames. A bare %v of the recovered value names
	// no function, and the only reader of this line is someone trying to find
	// which rule action did it.
	logMu.Lock()
	defer logMu.Unlock()
	var panicLine string
	for _, l := range logged {
		if strings.Contains(l, "PANIC") {
			panicLine = l
		}
	}
	if panicLine == "" {
		t.Fatalf("a panicking delivery must be logged; got %v", logged)
	}
	if !strings.Contains(panicLine, "deliverer blew up") {
		t.Fatalf("the panic log must carry the panic value; got %q", panicLine)
	}
	if !strings.Contains(panicLine, "poisonDeliverer") {
		t.Fatalf("the panic log must carry debug.Stack() frames naming the function that panicked — "+
			"a bare %%v of the panic value is not diagnosable; got %q", panicLine)
	}
	if !strings.Contains(panicLine, "seq 2") {
		t.Fatalf("the panic log must name the telemetry seq it was delivering; got %q", panicLine)
	}
}
