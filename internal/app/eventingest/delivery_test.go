package eventingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
