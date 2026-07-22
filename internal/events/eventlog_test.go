package events

import (
	"reflect"
	"testing"
)

// env is a minimal envelope carrying only the id — the ordering/retention key
// the log is defined by (EVT-140 bounds a subscriber stream by envelope id).
func env(id string) Envelope { return Envelope{ID: id} }

func ids(evs []Envelope) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.ID)
	}
	return out
}

// TestEventLog_OrderedAppendAndQueries: Append keeps the log ordered by id even
// when events arrive out of order; OldestRetainedID names the front; After
// returns strictly-greater ids in id order; Has answers membership.
func TestEventLog_OrderedAppendAndQueries(t *testing.T) {
	var l EventLog // zero value = unbounded
	// append deliberately out of id order.
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y8"))
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y6"))
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y7"))

	if got := l.OldestRetainedID(); got != "01J8Z3K4N5P6Q7R8S9T0V1W2Y6" {
		t.Fatalf("OldestRetainedID = %q; want the earliest id Y6", got)
	}
	// After(Y6) is Y7, Y8 — strictly greater, in id order (no gap, no dup).
	if got := ids(l.After("01J8Z3K4N5P6Q7R8S9T0V1W2Y6")); !reflect.DeepEqual(got, []string{"01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "01J8Z3K4N5P6Q7R8S9T0V1W2Y8"}) {
		t.Fatalf("After(Y6) = %v; want [Y7 Y8] in id order", got)
	}
	if !l.Has("01J8Z3K4N5P6Q7R8S9T0V1W2Y7") {
		t.Fatal("Has(Y7) must be true after Append")
	}
	if l.Has("01J8Z3K4N5P6Q7R8S9T0V1W2ZZ") {
		t.Fatal("Has(never-appended) must be false")
	}
}

// TestEventLog_AppendDedup (EVT-135): appending the same id twice is idempotent
// — at-least-once delivery uses id as the dedup key, so a redelivered id must
// not create a duplicate log entry.
func TestEventLog_AppendDedup(t *testing.T) {
	var l EventLog
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y7"))
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y7"))
	if got := ids(l.After("")); len(got) != 1 {
		t.Fatalf("appending the same id twice must not duplicate it (EVT-135); got %v", got)
	}
}

// TestEventLog_EvictedAfter (EVT-142/143) is the precise mid-stream loss check a
// live subscriber's drain relies on: EvictedAfter(id) reports whether any event
// with an id strictly greater than id has aged out of retention — i.e. an event
// past the subscriber's last-delivered point was dropped before delivery. It
// must be exact, never spuriously true: a subscriber caught up to the last
// aged-out id (or beyond) has NO gap, because everything after its point is still
// retained. A log that has never evicted always answers false.
func TestEventLog_EvictedAfter(t *testing.T) {
	y5 := "01J8Z3K4N5P6Q7R8S9T0V1W2Y5"
	y6 := "01J8Z3K4N5P6Q7R8S9T0V1W2Y6"
	y7 := "01J8Z3K4N5P6Q7R8S9T0V1W2Y7"
	y8 := "01J8Z3K4N5P6Q7R8S9T0V1W2Y8"
	y9 := "01J8Z3K4N5P6Q7R8S9T0V1W2Y9"

	l := NewEventLog(2)
	// Nothing evicted yet — no id can have aged out, so EvictedAfter is always
	// false, even from the very start ("").
	l.Append(env(y5))
	l.Append(env(y6))
	if l.EvictedAfter("") {
		t.Fatal("a log that has evicted nothing must answer EvictedAfter(\"\") false")
	}

	// A slow-consumer burst: Y5, Y6, Y7, Y8, Y9 with no drain. Retention 2 keeps
	// [Y8, Y9] and ages out Y5, Y6, Y7 — the highest aged-out id is Y7.
	l.Append(env(y7)) // → [Y6 Y7], evicted through Y5
	l.Append(env(y8)) // → [Y7 Y8], evicted through Y6
	l.Append(env(y9)) // → [Y8 Y9], evicted through Y7

	// A subscriber stuck before the highest aged-out id lost undelivered events.
	if !l.EvictedAfter("") {
		t.Fatal("a subscriber at the start of a log that has since aged out entries must see a gap")
	}
	if !l.EvictedAfter(y5) {
		t.Fatal("a subscriber last at Y5 lost Y6/Y7 (aged out before delivery) — EvictedAfter must be true")
	}
	if !l.EvictedAfter(y6) {
		t.Fatal("a subscriber last at Y6 lost Y7 (aged out before delivery) — EvictedAfter must be true")
	}
	// Boundary: a subscriber caught up EXACTLY to the highest aged-out id has no
	// loss — everything strictly after Y7 (namely Y8, Y9) is still retained. This
	// is the false-positive the naive `lastID < OldestRetainedID` check gets wrong
	// (Y7 < Y8), so it must NOT mark a gap here.
	if l.EvictedAfter(y7) {
		t.Fatal("a subscriber caught up to the highest aged-out id Y7 has NO gap — Y8/Y9 are retained; EvictedAfter must be false (no spurious gap)")
	}
	if l.EvictedAfter(y8) {
		t.Fatal("a subscriber already at a retained id must never see a gap")
	}
}

// TestEventLog_RetentionHorizon (EVT-141): a bounded log drops its oldest
// entries once it exceeds the retention horizon, which advances
// OldestRetainedID — the substrate a retention_expired resume resolves against.
// A dropped id is no longer retained (Has false), so a resume from it will land
// in the aged-out branch, never a silent hole.
func TestEventLog_RetentionHorizon(t *testing.T) {
	l := NewEventLog(2)
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y6"))
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y7"))
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y8")) // overflows retention 2 → drops Y6

	if got := l.OldestRetainedID(); got != "01J8Z3K4N5P6Q7R8S9T0V1W2Y7" {
		t.Fatalf("after overflow OldestRetainedID must advance to Y7; got %q", got)
	}
	if l.Has("01J8Z3K4N5P6Q7R8S9T0V1W2Y6") {
		t.Fatal("the aged-out id Y6 must no longer be retained")
	}
	if got := ids(l.After("")); !reflect.DeepEqual(got, []string{"01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "01J8Z3K4N5P6Q7R8S9T0V1W2Y8"}) {
		t.Fatalf("retained set must be exactly [Y7 Y8]; got %v", got)
	}
}
