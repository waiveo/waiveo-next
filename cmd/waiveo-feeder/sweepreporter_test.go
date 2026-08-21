package main

import (
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/feeder/contentgc"
)

// A sweep that reclaimed nothing used to log nothing, so seven days of silence
// on a real box was indistinguishable between three states: retaining
// correctly, retaining wrongly, and not running at all. Every fact needed to
// tell them apart was already in Result and was being discarded.

func res(scanned, reclaimed int, retained map[contentgc.Reason]int) contentgc.Result {
	return contentgc.Result{
		Generation: 42, FleetKnown: true, FleetConverged: true,
		Scanned: scanned, Reclaimed: reclaimed, Retained: retained, SourceRows: 3,
	}
}

func TestASweepThatKeptSomethingSaysWHY(t *testing.T) {
	got := summarizeSweep(res(4, 0, map[contentgc.Reason]int{contentgc.ReasonMarkTooRecent: 1}))
	if !strings.Contains(got, "mark-too-recent=1") {
		t.Fatalf("the summary does not name the retention reason: %s", got)
	}
	if !strings.Contains(got, "scanned 4") || !strings.Contains(got, "reclaimed 0") {
		t.Errorf("the summary does not say what it looked at and took: %s", got)
	}
}

func TestTheSummaryIsStableForAnUnchangedOutcome(t *testing.T) {
	// Load-bearing for the change detection below: Retained is a map, and an
	// unordered range would make every pass look new, restoring the hourly noise
	// this exists to avoid.
	a := map[contentgc.Reason]int{contentgc.ReasonTooNew: 2, contentgc.ReasonMarkTooRecent: 1, contentgc.ReasonBatchLimit: 3}
	b := map[contentgc.Reason]int{contentgc.ReasonBatchLimit: 3, contentgc.ReasonMarkTooRecent: 1, contentgc.ReasonTooNew: 2}
	for i := 0; i < 20; i++ {
		if summarizeSweep(res(6, 0, a)) != summarizeSweep(res(6, 0, b)) {
			t.Fatal("two identical outcomes produced different summaries — map ordering is leaking " +
				"into the log and every hourly pass will report as a change")
		}
	}
}

func TestAnUnchangedSweepIsNotRepeatedEveryHour(t *testing.T) {
	// Counts LINES EMITTED, not the reporter's internal state. The first version
	// of this test read r.last and therefore passed against a build with the
	// change detection deleted — it was describing its own closure.
	var lines []string
	r := &sweepReporter{logf: func(f string, a ...any) { lines = append(lines, f) }}
	steady := res(4, 0, map[contentgc.Reason]int{contentgc.ReasonMarkTooRecent: 1})
	for i := 0; i < 24; i++ {
		r.report(steady)
	}
	if len(lines) != 1 {
		t.Fatalf("a steady box emitted %d lines in 24 passes, want 1 — an hourly repeat buries the "+
			"pass where something actually moved", len(lines))
	}
}

func TestAChangedOutcomeIsReported(t *testing.T) {
	var lines []string
	r := &sweepReporter{logf: func(f string, a ...any) { lines = append(lines, f) }}
	r.report(res(4, 0, map[contentgc.Reason]int{contentgc.ReasonMarkTooRecent: 1}))
	r.report(res(4, 1, nil))
	if len(lines) != 2 {
		t.Fatal("the pass where an asset was finally reclaimed did not change the summary, so it " +
			"would never be logged — which is the one pass an operator is waiting for")
	}
}

func TestTheFirstPassAlwaysReports(t *testing.T) {
	// "Unchanged since a value nobody has seen" is not something an operator can
	// act on, and an empty origin is a legitimate steady state that must be
	// distinguishable from a sweep that is not running.
	var lines []string
	r := &sweepReporter{logf: func(f string, a ...any) { lines = append(lines, f) }}
	r.report(res(0, 0, nil))
	if len(lines) != 1 {
		t.Fatal("the first pass reported nothing, so a quiet box stays indistinguishable from a dead sweep")
	}
}

func TestAnUnknownFleetIsNamedRatherThanShownAsAGeneration(t *testing.T) {
	// Reporting a generation number for a fleet this process cannot account for
	// would assert knowledge it does not have.
	unknown := res(4, 0, map[contentgc.Reason]int{contentgc.ReasonFleetNotConverged: 4})
	unknown.FleetKnown = false
	got := summarizeSweep(unknown)
	if !strings.Contains(got, "UNKNOWN") {
		t.Fatalf("an unaccountable fleet was summarised as a known generation: %s", got)
	}
}
