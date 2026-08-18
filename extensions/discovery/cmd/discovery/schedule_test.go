package main

import (
	"strings"
	"testing"
	"time"
)

// schedule_test.go pins the schedule grammar, which decides whether this
// extension puts active traffic on someone's network unprompted. The bias is
// stated once and asserted here: anything not clearly understood schedules
// NOTHING. A misread schedule is a segment probed on a cadence nobody chose.

func TestParseEveryAcceptsTheOneGrammarItDocuments(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"every 30m", 30 * time.Minute},
		{"every 6h", 6 * time.Hour},
		{"EVERY 45M", 45 * time.Minute},
		{"  every 2h  ", 2 * time.Hour},
	} {
		got, ok := parseEvery(tc.in)
		if !ok || got != tc.want {
			t.Errorf("parseEvery(%q) = %v,%v — want %v,true", tc.in, got, ok, tc.want)
		}
	}
}

// Everything else schedules nothing. Cron is included on purpose: it is the
// format an operator is most likely to try, and silently misreading it is the
// failure this grammar exists to avoid.
func TestParseEveryRefusesEverythingElse(t *testing.T) {
	for _, in := range []string{
		"", "never", "daily at 03:00", "0 3 * * *", "every", "every day",
		"every 0m", "every -5m", "hourly", "30m", "every 5x",
	} {
		if got, ok := parseEvery(in); ok {
			t.Errorf("parseEvery(%q) = %v,true — an unclear schedule must schedule nothing", in, got)
		}
	}
}

// The floor exists so "every 1m" cannot turn a relay into a permanent scanner.
// parseEvery reports what was asked; the caller clamps — this asserts the value
// that reaches the clamp is the small one, so the clamp is what protects.
func TestParseEveryReportsSubMinimumIntervalsForTheCallerToClamp(t *testing.T) {
	got, ok := parseEvery("every 1m")
	if !ok || got != time.Minute {
		t.Fatalf("parseEvery(every 1m) = %v,%v — want 1m,true so the caller can clamp it to the floor", got, ok)
	}
	if got >= minScanInterval {
		t.Fatalf("the test's premise is wrong: 1m is not below the %v floor", minScanInterval)
	}
}

// TestSettingsFieldReadsTheRowOutOfThePage pins the shape that cost a box
// round-trip: a collection endpoint answers {"items":[…]} even for a collection
// declared SINGLETON, because a singleton is a collection with room for one row
// rather than a bare document. Decoding the page as the row yields the zero
// value for every field — silently — which reads exactly like "the operator
// configured nothing", so the extension would simply never schedule and never
// say why.
func TestSettingsFieldReadsTheRowOutOfThePage(t *testing.T) {
	page := `{"items":[{"schedule":"every 30m","subnets":"192.168.50.0/24"}],"cursor":null}`
	if got := settingsField(strings.NewReader(page), "schedule"); got != "every 30m" {
		t.Errorf("schedule = %q, want it read out of items[0]", got)
	}
	if got := settingsField(strings.NewReader(page), "subnets"); got != "192.168.50.0/24" {
		t.Errorf("subnets = %q, want it read out of items[0]", got)
	}
}

// An empty collection is an operator who has configured nothing — which must
// read as "no schedule", not as an error and not as a guess.
func TestSettingsFieldOnAnEmptyCollectionIsBlank(t *testing.T) {
	for _, body := range []string{`{"items":[],"cursor":null}`, `{}`, `not json`} {
		if got := settingsField(strings.NewReader(body), "schedule"); got != "" {
			t.Errorf("settingsField(%q) = %q, want blank", body, got)
		}
	}
}
