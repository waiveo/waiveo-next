package eventingest

import (
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/events"
)

// clock_test.go defends the one property New's required clock argument exists to
// establish: an ingested envelope's ts is the clock the DEPLOYMENT chose, never
// a reading this package took for itself.
//
// This layer used to stamp ts from time.Now directly. That looked equivalent,
// because the reading a deployment passes — the app's persisted monotonic clock
// floor (internal/app/auth.ClockFloor.Now, security-model/1 SEC-066) — reports
// max(host wall clock, persisted floor), and the floor is 0 until a time source
// advances it. The two separate the moment one does, and separate in the
// direction that corrupts a reconstruction: a relay's telemetry would enter the
// same durable log as the audit records describing the same request, stamped
// from a clock running BEHIND them, so a reader would see the effect before the
// cause.

// ingestFloorClampedMs is a reading no host clock in this test could produce: it
// is what ClockFloor.Now returns once a floor is established above the host's
// reading. Roughly the year 4000 in epoch milliseconds.
const ingestFloorClampedMs int64 = 64_060_588_800_000

// TestIngestStampsTSFromTheInjectedClock: the envelope's ts is the injected
// reading, taken PER RECORD.
//
// The injected value is centuries from any wall clock, so a regression to
// time.Now cannot coincidentally satisfy this — the assertion is on the reading
// itself, not merely on ts being non-zero (which the existing cases already
// cover, and which a hardcoded host clock satisfies just as well).
func TestIngestStampsTSFromTheInjectedClock(t *testing.T) {
	reading := ingestFloorClampedMs
	log := events.NewEventLog(0)
	h := New(log, siteScope, seqIDs(), func() int64 { return reading }, testRelay().Authorizer(), nil)

	postBatch(t, h, pushBatch(autoEntry(1, validAutomationRunPayload())))
	got := log.After("")
	if len(got) != 1 {
		t.Fatalf("exactly one envelope must be appended; got %d", len(got))
	}
	if got[0].TS != ingestFloorClampedMs {
		t.Fatalf("envelope ts = %d, want the injected reading %d — the ingest stamped from a clock the deployment did not choose",
			got[0].TS, ingestFloorClampedMs)
	}

	// The clock is READ per record rather than captured at construction: a floor
	// that advances between pushes must move the next envelope's ts with it.
	reading = ingestFloorClampedMs + 60_000
	postBatch(t, h, pushBatch(autoEntry(2, validAutomationRunPayload())))
	got = log.After("")
	if len(got) != 2 {
		t.Fatalf("two envelopes must be appended; got %d", len(got))
	}
	if got[1].TS != reading {
		t.Fatalf("second envelope ts = %d, want %d — the ingest cached a reading instead of taking one", got[1].TS, reading)
	}
}

// TestNewRefusesWithoutAClock: the clock is REQUIRED. There is deliberately no
// fallback — a default that silently reads the host clock is the defect this
// argument removes, and the refusal is at construction because this handler is
// built once, at boot, from a single call site. A nil that survived construction
// would surface as a recovered nil-deref inside net/http: one 500 per push, the
// batch lost, and nothing naming the wiring bug.
func TestNewRefusesWithoutAClock(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with a nil clock returned a handler; an ingest that can be built without naming its clock will eventually be built without one")
		}
	}()
	var h http.Handler = New(events.NewEventLog(0), siteScope, seqIDs(), nil, testRelay().Authorizer(), nil)
	_ = h
}
