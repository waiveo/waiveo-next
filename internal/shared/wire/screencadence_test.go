package wire

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The pins that keep the live/stale window and the cadences it is derived from
// from drifting apart again (the defect screencadence.go's doc describes). Each
// test below fails for a DIFFERENT single-constant edit, which is what makes
// them a fence rather than one tautology written three ways.

// TestLiveWindowIsDerivedFromTheObservedCadence: the window is a whole multiple
// of the measured pull cadence, not an independently chosen number that happens
// to be larger. Fails if anyone replaces the computation with a literal.
func TestLiveWindowIsDerivedFromTheObservedCadence(t *testing.T) {
	if ScreenLiveWindowMs%ObservedProgramPullCadenceMs != 0 {
		t.Fatalf("ScreenLiveWindowMs = %d is not a whole multiple of ObservedProgramPullCadenceMs = %d; the window must be DERIVED from the cadence, not picked beside it",
			ScreenLiveWindowMs, ObservedProgramPullCadenceMs)
	}
	if got := ScreenLiveWindowMs / ObservedProgramPullCadenceMs; got != ScreenLiveWindowCadenceMultiple {
		t.Fatalf("window spans %d cadence(s), ScreenLiveWindowCadenceMultiple says %d", got, ScreenLiveWindowCadenceMultiple)
	}
}

// TestLiveWindowSurvivesAWholeMissedPullPlusALateReport is THE relationship —
// the one W2-18 violated. A healthy screen's worst honest age is one whole pull
// cadence (the report was taken just before its next pull) plus one whole
// report interval (that report then sat here until the next one replaced it).
// The window must clear that with a whole missed cycle of headroom on top, or a
// screen that is merely unlucky reads stale.
//
// Concretely at today's numbers: worst honest age 70 000 ms, one missed cycle
// takes it to 130 000 ms, and the window is 180 000 ms.
func TestLiveWindowSurvivesAWholeMissedPullPlusALateReport(t *testing.T) {
	worstHonestAge := ObservedProgramPullCadenceMs + ScreenStatusReportIntervalMs
	if ScreenLiveWindowMs <= worstHonestAge {
		t.Fatalf("ScreenLiveWindowMs = %d does not even cover a HEALTHY screen's worst honest age (%d = one pull cadence %d + one report interval %d): every healthy screen will report stale part of every cycle",
			ScreenLiveWindowMs, worstHonestAge, ObservedProgramPullCadenceMs, ScreenStatusReportIntervalMs)
	}
	oneMissedCycle := worstHonestAge + ObservedProgramPullCadenceMs
	if ScreenLiveWindowMs <= oneMissedCycle {
		t.Fatalf("ScreenLiveWindowMs = %d leaves no margin: a screen that misses ONE pull reaches %d and would flip to stale. The window must exceed one whole missed cycle.",
			ScreenLiveWindowMs, oneMissedCycle)
	}
}

// TestLiveWindowStillDistinguishesAFailedScreen is the OTHER direction, and it
// is here because the mirror-image mistake — answering flapping by widening the
// window without bound — turns the column into one that says `live` about a
// screen that has been dark all afternoon. Two missed cycles must still go
// stale.
func TestLiveWindowStillDistinguishesAFailedScreen(t *testing.T) {
	twoMissedCycles := ObservedProgramPullCadenceMs*3 + ScreenStatusReportIntervalMs
	if ScreenLiveWindowMs >= twoMissedCycles {
		t.Fatalf("ScreenLiveWindowMs = %d is so wide that a screen which has missed TWO whole pull cycles (age %d) still reads live; the window has stopped being a health signal",
			ScreenLiveWindowMs, twoMissedCycles)
	}
}

// TestObservedCadenceIsAtLeastTheNominalWait: the measured pull-to-pull cadence
// cannot be shorter than the wait the player performs between pulls — a value
// below it is a transcription error, and it would silently shrink the window.
func TestObservedCadenceIsAtLeastTheNominalWait(t *testing.T) {
	if ObservedProgramPullCadenceMs < ProgramPollIntervalMs {
		t.Fatalf("ObservedProgramPullCadenceMs = %d is below the player's own nominal wait ProgramPollIntervalMs = %d; a pull-to-pull cadence cannot be shorter than the wait inside it",
			ObservedProgramPullCadenceMs, ProgramPollIntervalMs)
	}
}

// playerPollIntervalRe extracts the shipped player's own poll interval from its
// BrightScript source. Anchored on the function it is the whole body of, so it
// cannot match some other `return 10000` elsewhere in the file.
var playerPollIntervalRe = regexp.MustCompile(`(?s)function wvProgramPollIntervalMs\(\) as Integer\s*\n\s*return\s+(\d+)\s*\n\s*end function`)

// TestProgramPollIntervalMirrorsTheShippedPlayer reads player-v3's source and
// fails if the Go mirror has drifted from it.
//
// This is the only mechanism that can catch the change that STARTED W2-18: the
// player is retuned, nothing in Go stops compiling, every test stays green, and
// the console's staleness line goes on describing a cadence no screen has. A
// human retuning the player is now forced through this file, where the comments
// tell them what to re-measure.
func TestProgramPollIntervalMirrorsTheShippedPlayer(t *testing.T) {
	const src = "../../../player-v3/components/PlayerTask.brs"
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading the shipped player's source (%s): %v — if the player moved, MOVE THIS TEST, do not delete it: it is the only check that the Go mirror still matches BrightScript", src, err)
	}
	m := playerPollIntervalRe.FindSubmatch(b)
	if m == nil {
		t.Fatalf("could not find wvProgramPollIntervalMs()'s return value in %s; the mirror in screencadence.go can no longer be verified", src)
	}
	got, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		t.Fatalf("wvProgramPollIntervalMs() returns %q, which is not an integer: %v", m[1], err)
	}
	if got != ProgramPollIntervalMs {
		t.Fatalf("the shipped player polls every %d ms but wire.ProgramPollIntervalMs says %d ms.\nUpdate the mirror AND re-measure ObservedProgramPullCadenceMs — the live window is derived from the observed cadence, and a retuned player changes it.",
			got, ProgramPollIntervalMs)
	}
}
