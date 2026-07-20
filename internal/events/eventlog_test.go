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
