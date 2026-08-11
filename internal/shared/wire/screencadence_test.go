package wire

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The pins that keep the live/stale window and the player timings it is derived
// from from drifting apart (the defect screencadence.go's doc describes). Each
// test below fails for a DIFFERENT single edit, which is what makes them a fence
// rather than one tautology written three ways.
//
// The fence has one more rail than it used to, and the reason is written down in
// screencadence.go's "correction of 2026-08": the previous version's pins were
// all RELATIVE to a cadence constant that was itself a hand-entered number, so a
// reviewer could hand-edit that number to anything plausible and every pin
// stayed green. TestTheHealthyCadenceIsDerivedFromThePlayersOwnTimings is the
// rail that closes it — the cadence is now an expression over two mirrored
// player timings, and hand-editing it fails.

// TestTheHealthyCadenceIsDerivedFromThePlayersOwnTimings: the cadence bound the
// window rests on must be the player's wait plus the player's own request
// timeout, and nothing else.
//
// This is the pin that makes "just widen it a bit" impossible. Replacing
// HealthyProgramPullCadenceMs with ANY literal — 15 000, 60 000, whatever a
// future incident makes tempting — fails here, and the only way to move the
// window is to change a mirrored player timing, which the two mirror tests below
// then hold against the shipped BrightScript.
func TestTheHealthyCadenceIsDerivedFromThePlayersOwnTimings(t *testing.T) {
	want := ProgramPollIntervalMs + ProgramPullRequestTimeoutMs
	if HealthyProgramPullCadenceMs != want {
		t.Fatalf("HealthyProgramPullCadenceMs = %d, want %d (ProgramPollIntervalMs %d + ProgramPullRequestTimeoutMs %d).\n"+
			"This constant must be DERIVED from the player's own two timings. A hand-entered number here is exactly the "+
			"defect of 2026-08: the withdrawn 60 000 was the player's retry-backoff CAP, entered as if it were a measured "+
			"poll cadence, and every other pin in this file passed anyway because they were all relative to it.",
			HealthyProgramPullCadenceMs, want, ProgramPollIntervalMs, ProgramPullRequestTimeoutMs)
	}
}

// TestLiveWindowIsDerivedFromTheHealthyCadence: the window is a whole multiple
// of that bound, not an independently chosen number that happens to be larger.
// Fails if anyone replaces the computation with a literal.
func TestLiveWindowIsDerivedFromTheHealthyCadence(t *testing.T) {
	if ScreenLiveWindowMs%HealthyProgramPullCadenceMs != 0 {
		t.Fatalf("ScreenLiveWindowMs = %d is not a whole multiple of HealthyProgramPullCadenceMs = %d; the window must be DERIVED from the cadence, not picked beside it",
			ScreenLiveWindowMs, HealthyProgramPullCadenceMs)
	}
	if got := ScreenLiveWindowMs / HealthyProgramPullCadenceMs; got != ScreenLiveWindowCadenceMultiple {
		t.Fatalf("window spans %d cadence(s), ScreenLiveWindowCadenceMultiple says %d", got, ScreenLiveWindowCadenceMultiple)
	}
}

// TestLiveWindowSurvivesAWholeMissedPullPlusALateReport is THE relationship —
// the one W2-18 was opened about. A healthy screen's worst honest age is one
// whole pull cadence (the report was taken just before its next pull) plus one
// whole report interval (that report then sat here until the next one replaced
// it). The window must clear that with a whole missed cycle of headroom on top,
// or a screen that is merely unlucky reads stale.
//
// Concretely at today's numbers: worst honest age 28 000 ms, one missed cycle
// takes it to 46 000 ms, and the window is 54 000 ms.
func TestLiveWindowSurvivesAWholeMissedPullPlusALateReport(t *testing.T) {
	worstHonestAge := HealthyProgramPullCadenceMs + ScreenStatusReportIntervalMs
	if ScreenLiveWindowMs <= worstHonestAge {
		t.Fatalf("ScreenLiveWindowMs = %d does not even cover a HEALTHY screen's worst honest age (%d = one pull cadence %d + one report interval %d): every healthy screen will report stale part of every cycle",
			ScreenLiveWindowMs, worstHonestAge, HealthyProgramPullCadenceMs, ScreenStatusReportIntervalMs)
	}
	oneMissedCycle := worstHonestAge + HealthyProgramPullCadenceMs
	if ScreenLiveWindowMs <= oneMissedCycle {
		t.Fatalf("ScreenLiveWindowMs = %d leaves no margin: a screen that misses ONE pull reaches %d and would flip to stale. The window must exceed one whole missed cycle.",
			ScreenLiveWindowMs, oneMissedCycle)
	}
}

// TestTheLiveWindowClearsTheFIELDMeasuredWorstAge is the same relationship
// checked against reality rather than against the derivation, so the two cannot
// quietly disagree.
//
// The numbers are the re-measurement on The Hanger described in
// screencadence.go's correction: with the content-URL 403 fixed, a healthy
// screen's `last_pull_age_ms` sawtooths between ~9 400 ms and ~19 500 ms. This
// is a LOWER bound on the window and it is deliberately separate from the
// derivation pin above — if a future player retune ever pushes the derived
// cadence BELOW what a real screen presents, the derivation would still be
// internally consistent and this is the test that would notice.
func TestTheLiveWindowClearsTheFIELDMeasuredWorstAge(t *testing.T) {
	// Peak app-side age observed on a healthy screen, 2026-08, The Hanger.
	const measuredWorstAgeMs int64 = 19_500
	if ScreenLiveWindowMs <= measuredWorstAgeMs {
		t.Fatalf("ScreenLiveWindowMs = %d is at or below the %d ms peak a REAL healthy screen presents; the console would call a working wall stale",
			ScreenLiveWindowMs, measuredWorstAgeMs)
	}
	if HealthyProgramPullCadenceMs+ScreenStatusReportIntervalMs < measuredWorstAgeMs {
		t.Fatalf("the derived worst honest age (%d = cadence %d + report %d) is BELOW the %d ms a real screen was measured presenting: the derivation no longer describes the field, and the mirrored player timings are where to look",
			HealthyProgramPullCadenceMs+ScreenStatusReportIntervalMs, HealthyProgramPullCadenceMs, ScreenStatusReportIntervalMs, measuredWorstAgeMs)
	}
}

// TestLiveWindowStillDistinguishesAFailedScreen is the OTHER direction, and it
// is here because the mirror-image mistake — answering flapping by widening the
// window without bound — turns the column into one that says `live` about a
// screen that has been dark all afternoon. Two missed cycles must still go
// stale.
func TestLiveWindowStillDistinguishesAFailedScreen(t *testing.T) {
	twoMissedCycles := HealthyProgramPullCadenceMs*3 + ScreenStatusReportIntervalMs
	if ScreenLiveWindowMs >= twoMissedCycles {
		t.Fatalf("ScreenLiveWindowMs = %d is so wide that a screen which has missed TWO whole pull cycles (age %d) still reads live; the window has stopped being a health signal",
			ScreenLiveWindowMs, twoMissedCycles)
	}
}

// TestAScreenInRetryBackoffIsAllowedToReadStale is the semantic the correction
// turns on, pinned so nobody "fixes" it back.
//
// A screen whose pulls are FAILING waits `wvProgramRetryBackoffMs()`, which
// doubles to a 60 000 ms cap — longer than this window on purpose. Such a screen
// is not healthy: it is failing every pull and rendering nothing new. Reading
// `stale` is the correct report for it, and the withdrawn 180 000 ms window's
// only real effect was to hide exactly that case for three minutes.
func TestAScreenInRetryBackoffIsAllowedToReadStale(t *testing.T) {
	// The player's own backoff ceiling — the number the withdrawn constant
	// mistook for a poll cadence. Mirrored by
	// TestTheRetryCapIsNotThePollInterval below.
	const playerRetryCapMs int64 = 60_000
	if ScreenLiveWindowMs >= playerRetryCapMs {
		t.Fatalf("ScreenLiveWindowMs = %d spans the player's whole retry-backoff cap (%d ms), so a screen failing EVERY pull still reads live.\n"+
			"That is the 2026-08 defect: the window was widened to cover a screen that was in backoff because it was broken. A degraded screen must be allowed to read stale.",
			ScreenLiveWindowMs, playerRetryCapMs)
	}
}

// brsFunctionReturnRe builds a matcher for a whole zero-argument BrightScript
// function that is nothing but a `return <integer>` — the shape player-v3 states
// each of these timings in. Anchored on the function name and its `end function`
// so it cannot match some other literal elsewhere in the file.
func brsFunctionReturnRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)function ` + name + `\(\) as Integer\s*\n\s*return\s+(\d+)\s*\n\s*end function`)
}

// readBrsInt reads one integer out of a player source file, failing with an
// instruction rather than a panic if the shape has moved.
func readBrsInt(t *testing.T, src string, re *regexp.Regexp, what string) int64 {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading the shipped player's source (%s): %v — if the player moved, MOVE THIS TEST, do not delete it: it is the only check that the Go mirror still matches BrightScript", src, err)
	}
	m := re.FindSubmatch(b)
	if m == nil {
		t.Fatalf("could not find %s in %s; the mirror in screencadence.go can no longer be verified", what, src)
	}
	got, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		t.Fatalf("%s is %q, which is not an integer: %v", what, m[1], err)
	}
	return got
}

const playerTaskSrc = "../../../player-v3/components/PlayerTask.brs"

// TestProgramPollIntervalMirrorsTheShippedPlayer reads player-v3's source and
// fails if the Go mirror has drifted from it.
//
// This is the only mechanism that can catch the change that STARTED W2-18: the
// player is retuned, nothing in Go stops compiling, every test stays green, and
// the console's staleness line goes on describing a cadence no screen has. A
// human retuning the player is now forced through this file, where the comments
// tell them what changes with it.
func TestProgramPollIntervalMirrorsTheShippedPlayer(t *testing.T) {
	got := readBrsInt(t, playerTaskSrc, brsFunctionReturnRe("wvProgramPollIntervalMs"), "wvProgramPollIntervalMs()'s return value")
	if got != ProgramPollIntervalMs {
		t.Fatalf("the shipped player polls every %d ms but wire.ProgramPollIntervalMs says %d ms.\nUpdate the mirror — the live window is derived from it, and a retuned player moves the window.",
			got, ProgramPollIntervalMs)
	}
}

// programPullTimeoutRe extracts the `timeoutMs` the player's own
// `/player/v1/program` request passes. Anchored on that URL and matched across a
// run containing no `}`, so it cannot reach past the end of that one call
// literal into the timeout of some other request (Pairing's, or the renewal's,
// which are separate calls that happen to carry the same number today).
var programPullTimeoutRe = regexp.MustCompile(`(?s)url:\s*base \+ "/player/v1/program",[^}]*?timeoutMs:\s*(\d+)`)

// TestProgramPullRequestTimeoutMirrorsTheShippedPlayer is the second mirror, and
// it is load-bearing in the same way as the first: the healthy-cadence bound is
// the poll interval PLUS this, so a player that starts waiting longer on a
// program request widens how long a healthy screen may stay quiet.
func TestProgramPullRequestTimeoutMirrorsTheShippedPlayer(t *testing.T) {
	const src = "../../../player-v3/source/Program.brs"
	got := readBrsInt(t, src, programPullTimeoutRe, "the program pull's timeoutMs")
	if got != ProgramPullRequestTimeoutMs {
		t.Fatalf("the shipped player gives its program pull %d ms but wire.ProgramPullRequestTimeoutMs says %d ms.\nUpdate the mirror — the healthy-cadence bound is the poll interval plus this timeout, and the live window is three of those.",
			got, ProgramPullRequestTimeoutMs)
	}
}

// TestTheRetryCapIsNotThePollInterval pins the specific confusion that produced
// the withdrawn constant: `wvProgramRetryCapMs()` returns 60 000 and
// `wvProgramPollIntervalMs()` returns 10 000, and somebody read the first as the
// second.
//
// It asserts they are DIFFERENT functions with different values, so the two can
// never be conflated silently again — and it reads both from the BrightScript,
// so if a future retune ever makes them equal, a human has to come here and say
// why.
func TestTheRetryCapIsNotThePollInterval(t *testing.T) {
	retryCap := readBrsInt(t, playerTaskSrc, brsFunctionReturnRe("wvProgramRetryCapMs"), "wvProgramRetryCapMs()'s return value")
	poll := readBrsInt(t, playerTaskSrc, brsFunctionReturnRe("wvProgramPollIntervalMs"), "wvProgramPollIntervalMs()'s return value")
	if retryCap == poll {
		t.Fatalf("wvProgramRetryCapMs() and wvProgramPollIntervalMs() both return %d ms. They are different things — a failure backoff ceiling and an ordinary poll wait — and the live window must be derived from the SECOND. Reading the first as the second is what produced the withdrawn ObservedProgramPullCadenceMs = 60 000.", retryCap)
	}
	if ProgramPollIntervalMs == retryCap {
		t.Fatalf("wire.ProgramPollIntervalMs = %d is the player's RETRY BACKOFF CAP, not its poll interval (which is %d). This is the exact substitution of 2026-08.", ProgramPollIntervalMs, poll)
	}
}
