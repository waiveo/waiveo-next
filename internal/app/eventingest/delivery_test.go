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
// asserted here: the ack MUST NOT outrun the work. REL-092's ack is what makes
// the relay discard its retained entries, so delivering in a detached goroutine
// would let a crash between the two lose a press with the ack already given.

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

// TestTheAckDoesNotOutrunDelivery is the other half. The ack is REL-092's
// promise that the batch was processed and is what lets the relay drop its
// retained copy, so it must not be written while the work it acknowledges is
// still in flight.
func TestTheAckDoesNotOutrunDelivery(t *testing.T) {
	log := events.NewEventLog(0)
	d := newBlockingDeliverer()
	h := New(log, siteScope, seqIDs(), testWallMs, testRelay().Authorizer(), d.deliver)

	answered := make(chan struct{})
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, pushRequest(t, pushBatch(autoEntry(1, validAutomationRunPayload()))))
		close(answered)
	}()

	<-d.entered
	select {
	case <-answered:
		t.Fatal("the push was acked while its delivery was still in flight — a crash between the two loses the event with the relay already told it was received (REL-092/103)")
	case <-time.After(150 * time.Millisecond):
	}
	close(d.release)

	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("the push never completed after its delivery was released")
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
