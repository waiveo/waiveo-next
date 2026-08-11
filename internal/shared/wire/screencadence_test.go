package wire

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The pins that keep the live/stale windows and the player timings they are
// derived from from drifting apart (the defect screencadence.go's doc
// describes). Each test below fails for a DIFFERENT single edit, which is what
// makes them a fence rather than one tautology written three ways.
//
// # Why one of the rails reads the source code
//
// The previous version of this file claimed "a hand-entered literal fails
// here". It did not. The rail was
//
//	want := ProgramPollIntervalMs + ProgramPullRequestTimeoutMs
//	if HealthyProgramPullCadenceMs != want { … }
//
// which compares the constant against the same sum it was written from, so it
// only catches a literal that is not today's total: `const
// HealthyProgramPullCadenceMs int64 = 18_000` passed every test in this file.
// A test cannot tell a derived value from a literal by looking at its value.
// So TestTheHealthyCadenceIsAnEXPRESSIONOverThePlayersTimings reads
// screencadence.go's syntax tree instead and asserts the declaration IS an
// expression over the mirrored identifiers, with no numeric literal in it.
// That is the fence the claim always described.

// derivedConstants are the constants that must be expressions rather than
// numbers, with the identifiers each is allowed to be built from. Anything else
// — a literal, or a term nobody declared — fails.
var derivedConstants = map[string][]string{
	"HealthyProgramPullCadenceMs":   {"ProgramPollIntervalMs", "ProgramPullRequestTimeoutMs", "LeaseAckRequestTimeoutMs"},
	"ScreenLiveWindowMs":            {"HealthyProgramPullCadenceMs", "ScreenLiveWindowCadenceMultiple"},
	"ScreenContentTransferWindowMs": {"ScreenLiveWindowMs", "ProgramContentFetchTimeoutMs"},
}

// TestTheHealthyCadenceIsAnEXPRESSIONOverThePlayersTimings is the rail that
// makes "just widen it a bit" impossible, and it is the one the previous round
// only claimed to have.
//
// Every derived constant must be declared as an expression over named, mirrored
// timings and nothing else. Replacing any of them with a literal — 18 000,
// 60 000, whatever a future incident makes tempting — fails here whether or not
// the literal happens to equal today's sum, and the only way to move a window is
// to change a mirrored player timing, which the mirror tests below then hold
// against the shipped BrightScript.
func TestTheHealthyCadenceIsAnEXPRESSIONOverThePlayersTimings(t *testing.T) {
	decls := constDeclarations(t)
	for name, allowed := range derivedConstants {
		expr, ok := decls[name]
		if !ok {
			t.Errorf("no declaration of %s in screencadence.go; this fence can no longer be applied to it", name)
			continue
		}
		idents, literals := exprLeaves(expr)
		if len(literals) > 0 {
			t.Errorf("%s is declared with the numeric literal(s) %v.\n"+
				"It must be DERIVED from the mirrored player timings, not written down. A hand-entered number here is the "+
				"defect of 2026-08 twice over: the withdrawn 60 000 was the player's retry-backoff CAP entered as if it were a "+
				"poll cadence, and the replacement's own value-equality pin let a hand-entered 18 000 through because 18 000 "+
				"was, that week, the sum.", name, literals)
		}
		if !sameSet(idents, allowed) {
			t.Errorf("%s is built from %v, want exactly %v.\n"+
				"A term that appears here without being declared and mirrored is a term nothing checks against the player; "+
				"a term that DISAPPEARS is how the lease-ack round trip went uncounted for a whole round.", name, idents, allowed)
		}
	}
}

// TestLiveWindowIsAWholeMultipleOfTheCadence: the window is a whole number of
// healthy cadences, and that number is the one ScreenLiveWindowCadenceMultiple
// states.
func TestLiveWindowIsAWholeMultipleOfTheCadence(t *testing.T) {
	if ScreenLiveWindowMs%HealthyProgramPullCadenceMs != 0 {
		t.Fatalf("ScreenLiveWindowMs = %d is not a whole multiple of HealthyProgramPullCadenceMs = %d",
			ScreenLiveWindowMs, HealthyProgramPullCadenceMs)
	}
	if got := ScreenLiveWindowMs / HealthyProgramPullCadenceMs; got != ScreenLiveWindowCadenceMultiple {
		t.Fatalf("window spans %d cadence(s), ScreenLiveWindowCadenceMultiple says %d", got, ScreenLiveWindowCadenceMultiple)
	}
}

// ageAfterFailedPulls is a healthy screen's worst app-side `last_pull_age_ms`
// after its last SUCCESSFUL pull, given n consecutive failed pulls since.
//
// It models what the player actually does rather than assuming a missed pull
// costs a whole cadence: after a failure the wait is `wvProgramRetryBackoffMs()`
// (2 s doubling to a 60 s cap), NOT the ordinary poll interval, so the first
// failure costs one request timeout plus two seconds — not twenty-six. The
// previous version of this file used the pessimistic "one whole cadence per
// missed pull", which is one of the two reasons its arithmetic came out green.
func ageAfterFailedPulls(n int, retryBaseMs, retryCapMs int64) int64 {
	age := HealthyProgramPullCadenceMs
	backoff := retryBaseMs
	for i := 0; i < n; i++ {
		age += ProgramPullRequestTimeoutMs + backoff
		if backoff *= 2; backoff > retryCapMs {
			backoff = retryCapMs
		}
	}
	// Plus the report's own age: the relay's observation sits here until the
	// next report replaces it.
	return age + ScreenStatusReportIntervalMs
}

// TestTheLiveWindowCoversAHealthyScreenAndOneFailedPull is THE relationship —
// the one W2-18 was opened about — and it is now computed from the player's
// behaviour rather than restated from the constants.
//
// A healthy screen's worst honest age is one whole cadence (the report was taken
// just before its next pull) plus one whole report interval. The window must
// clear that, and clear one failed pull on top, or a screen that is merely
// unlucky reads stale.
func TestTheLiveWindowCoversAHealthyScreenAndOneFailedPull(t *testing.T) {
	base := readBrsInt(t, playerTaskSrc, brsFunctionReturnRe("wvProgramRetryBaseMs"), "wvProgramRetryBaseMs()'s return value")
	cap := readBrsInt(t, playerTaskSrc, brsFunctionReturnRe("wvProgramRetryCapMs"), "wvProgramRetryCapMs()'s return value")

	healthy := ageAfterFailedPulls(0, base, cap)
	if ScreenLiveWindowMs <= healthy {
		t.Fatalf("ScreenLiveWindowMs = %d does not even cover a HEALTHY screen's worst honest age (%d = one pull cadence %d + one report interval %d): every healthy screen will report stale part of every cycle",
			ScreenLiveWindowMs, healthy, HealthyProgramPullCadenceMs, ScreenStatusReportIntervalMs)
	}
	oneFailure := ageAfterFailedPulls(1, base, cap)
	if ScreenLiveWindowMs <= oneFailure {
		t.Fatalf("ScreenLiveWindowMs = %d leaves no margin: a screen that fails ONE pull reaches %d (request timeout %d + first backoff %d on top of %d) and flips to stale",
			ScreenLiveWindowMs, oneFailure, ProgramPullRequestTimeoutMs, base, healthy)
	}
	t.Logf("healthy worst age %d, one failed pull %d, two failed pulls %d, window %d",
		healthy, oneFailure, ageAfterFailedPulls(2, base, cap), ScreenLiveWindowMs)
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
// screen that has been dark all afternoon.
//
// Two consecutive failed pulls must go stale. That is the tightest honest
// statement available: with the ack counted, a window wide enough to survive two
// failures would also span the player's retry-backoff cap, which is the test
// below.
func TestLiveWindowStillDistinguishesAFailedScreen(t *testing.T) {
	base := readBrsInt(t, playerTaskSrc, brsFunctionReturnRe("wvProgramRetryBaseMs"), "wvProgramRetryBaseMs()'s return value")
	cap := readBrsInt(t, playerTaskSrc, brsFunctionReturnRe("wvProgramRetryCapMs"), "wvProgramRetryCapMs()'s return value")
	twoFailures := ageAfterFailedPulls(2, base, cap)
	if ScreenLiveWindowMs >= twoFailures {
		t.Fatalf("ScreenLiveWindowMs = %d still reads live for a screen that has failed TWO consecutive pulls (age %d); the window has stopped being a health signal",
			ScreenLiveWindowMs, twoFailures)
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
//
// This is also the constraint that fixes ScreenLiveWindowCadenceMultiple at 2:
// three whole 26 000 ms cadences would be 78 000 ms, past this cap.
func TestAScreenInRetryBackoffIsAllowedToReadStale(t *testing.T) {
	playerRetryCapMs := readBrsInt(t, playerTaskSrc, brsFunctionReturnRe("wvProgramRetryCapMs"), "wvProgramRetryCapMs()'s return value")
	if ScreenLiveWindowMs >= playerRetryCapMs {
		t.Fatalf("ScreenLiveWindowMs = %d spans the player's whole retry-backoff cap (%d ms), so a screen failing EVERY pull still reads live.\n"+
			"That is the 2026-08 defect: the window was widened to cover a screen that was in backoff because it was broken. A degraded screen must be allowed to read stale.",
			ScreenLiveWindowMs, playerRetryCapMs)
	}
}

// TestTheContentTransferWindowCoversAWholeFetchAndNoMore pins the answer to the
// cache-MISS case: a screen downloading newly assigned content gets one whole
// content-fetch timeout of grace past the live window, and that grace EXPIRES.
//
// Both halves matter. Without the grace a working wall mid-transfer reads stale
// (W2-18's symptom with a new cause). Without the expiry, a screen that lost
// power between its pull and its ack sits in `fetching` forever, which is the
// 180 000 ms mistake with a nicer label.
func TestTheContentTransferWindowCoversAWholeFetchAndNoMore(t *testing.T) {
	if ScreenContentTransferWindowMs-ScreenLiveWindowMs != ProgramContentFetchTimeoutMs {
		t.Fatalf("the content-transfer grace is %d ms, want exactly one content-fetch timeout (%d): the grace has to be the player's own statement of how long a single transfer may take, not a number picked here",
			ScreenContentTransferWindowMs-ScreenLiveWindowMs, ProgramContentFetchTimeoutMs)
	}
	// And it must not have quietly become the window for everything: a screen
	// with no outstanding Lease is judged by the live window alone, so this
	// number must stay strictly larger and strictly finite.
	if ScreenContentTransferWindowMs <= ScreenLiveWindowMs {
		t.Fatalf("ScreenContentTransferWindowMs = %d does not exceed ScreenLiveWindowMs = %d; there is no grace at all",
			ScreenContentTransferWindowMs, ScreenLiveWindowMs)
	}
}

// TestFoldingTheFetchIntoTheLiveWindowWouldBeAbsurd is the arithmetic that
// justifies a third state instead of a wider window, kept as a test so the
// alternative cannot be re-proposed without meeting it.
func TestFoldingTheFetchIntoTheLiveWindowWouldBeAbsurd(t *testing.T) {
	folded := (HealthyProgramPullCadenceMs + ProgramContentFetchTimeoutMs) * ScreenLiveWindowCadenceMultiple
	const withdrawnWindowMs int64 = 180_000
	if folded <= withdrawnWindowMs {
		t.Fatalf("folding the content-fetch timeout into the healthy cadence would give a %d ms window, which is no worse than the withdrawn %d ms one.\n"+
			"If that ever becomes true, the reason for a separate `fetching` state has gone and this file should be simplified rather than left with two windows.",
			folded, withdrawnWindowMs)
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

const (
	playerTaskSrc = "../../../player-v3/components/PlayerTask.brs"
	programSrc    = "../../../player-v3/source/Program.brs"
)

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

// brsCallTimeoutRe extracts the `timeoutMs` one of the player's own HTTP calls
// passes, anchored on that call's URL and matched across a run containing no `}`
// so it cannot reach past the end of one call literal into another's timeout.
func brsCallTimeoutRe(path string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)url:\s*base \+ "` + regexp.QuoteMeta(path) + `",[^}]*?timeoutMs:\s*(\d+)`)
}

// TestProgramPullRequestTimeoutMirrorsTheShippedPlayer is the second mirror, and
// it is load-bearing in the same way as the first: the healthy-cadence bound is
// the poll interval PLUS this, so a player that starts waiting longer on a
// program request widens how long a healthy screen may stay quiet.
func TestProgramPullRequestTimeoutMirrorsTheShippedPlayer(t *testing.T) {
	got := readBrsInt(t, programSrc, brsCallTimeoutRe("/player/v1/program"), "the program pull's timeoutMs")
	if got != ProgramPullRequestTimeoutMs {
		t.Fatalf("the shipped player gives its program pull %d ms but wire.ProgramPullRequestTimeoutMs says %d ms.\nUpdate the mirror — the healthy-cadence bound is the poll interval plus this timeout plus the lease ack's.",
			got, ProgramPullRequestTimeoutMs)
	}
}

// TestLeaseAckRequestTimeoutMirrorsTheShippedPlayer is the mirror that did not
// exist, and its absence is the whole of this round's second blocking finding:
// `wvAckLease` is a second synchronous round trip inside the same loop, so a
// healthy iteration is poll + program + ack, and a cadence counting two of those
// three understated the loop by eight seconds.
func TestLeaseAckRequestTimeoutMirrorsTheShippedPlayer(t *testing.T) {
	got := readBrsInt(t, programSrc, brsCallTimeoutRe("/player/v1/lease/ack"), "the lease ack's timeoutMs")
	if got != LeaseAckRequestTimeoutMs {
		t.Fatalf("the shipped player gives its lease ack %d ms but wire.LeaseAckRequestTimeoutMs says %d ms.\nUpdate the mirror — the ack blocks the poll loop whether it succeeds or not, so it bounds the cadence.",
			got, LeaseAckRequestTimeoutMs)
	}
}

// TestTheLeaseAckIsInsideThePollLoop pins the FACT the term rests on, not just
// its value: the ack is called from wvDoProgram, synchronously, before the
// player sleeps. If a future player fires it on its own thread, the term stops
// belonging in the cadence and this test is where that conversation starts.
func TestTheLeaseAckIsInsideThePollLoop(t *testing.T) {
	src, err := os.ReadFile(programSrc)
	if err != nil {
		t.Fatalf("reading %s: %v", programSrc, err)
	}
	// The call site inside wvDoProgram's success path, matched as a bare
	// statement (not inside a Task-spawning construct, of which this player has
	// none on this path).
	if !regexp.MustCompile(`(?m)^\s*wvAckLease\(base, state\.channelToken, pinFile, r\.leaseId\)\s*$`).Match(src) {
		t.Fatalf("could not find the synchronous wvAckLease call in %s.\n"+
			"wire.LeaseAckRequestTimeoutMs is a term in HealthyProgramPullCadenceMs ONLY because that call blocks the poll loop. "+
			"If the ack moved off the loop, remove the term; if it merely moved, re-anchor this test.", programSrc)
	}
}

// TestTheAckFollowsTheContentFetch pins the ORDERING the whole `fetching`
// inference rests on, and it is here because that inference is otherwise an
// unstated dependency on a player another track owns.
//
// internal/app/screens reads "pulled, not yet acknowledged" as "materialising
// content". That is only true because this player fetches and verifies every
// asset BEFORE `wvAckLease` fires. If the ack ever moves ahead of the fetch, the
// gap stops meaning anything and a transferring screen goes back to reading
// `stale`, silently.
//
// That is not hypothetical: player/1's own PLY-088 says a player MUST be able to
// accept and acknowledge a Lease whose assets it cannot yet fetch, which this
// player does not do today (a failed fetch abandons the Lease before the ack).
// A future player closing that gap is exactly the change that would break the
// inference, so it has to break this test on the way past.
func TestTheAckFollowsTheContentFetch(t *testing.T) {
	src, err := os.ReadFile(programSrc)
	if err != nil {
		t.Fatalf("reading %s: %v", programSrc, err)
	}
	// Scoped to wvDoProgram's own body, so the definitions of the two routines
	// further down the file cannot be mistaken for call sites.
	body := regexp.MustCompile(`(?s)function wvDoProgram\(.*?\nend function`).Find(src)
	if body == nil {
		t.Fatalf("could not find wvDoProgram in %s; the ordering the fetching state infers from can no longer be verified", programSrc)
	}
	fetches := regexp.MustCompile(`wvEnsureContent\(`).FindAllIndex(body, -1)
	ack := regexp.MustCompile(`wvAckLease\(`).FindIndex(body)
	if len(fetches) == 0 || ack == nil {
		t.Fatalf("wvDoProgram in %s no longer calls both wvEnsureContent and wvAckLease; the ordering the fetching state infers from can no longer be verified", programSrc)
	}
	if fetches[len(fetches)-1][0] > ack[0] {
		t.Fatalf("wvAckLease is called BEFORE the last wvEnsureContent in %s.\n"+
			"internal/app/screens reads an unacknowledged pull as a content transfer in progress; that is only true while the ack "+
			"comes after the fetch. If the player now acknowledges first (PLY-088), the fetching state stops firing and a screen "+
			"downloading a video reads stale again — change the read model, do not delete this test.", programSrc)
	}
}

// TestContentFetchTimeoutMirrorsTheShippedPlayer pins the number the
// content-transfer grace is one of.
func TestContentFetchTimeoutMirrorsTheShippedPlayer(t *testing.T) {
	got := readBrsInt(t, programSrc, brsFunctionReturnRe("wvContentFetchTimeoutMs"), "wvContentFetchTimeoutMs()'s return value")
	if got != ProgramContentFetchTimeoutMs {
		t.Fatalf("the shipped player allows %d ms per content transfer but wire.ProgramContentFetchTimeoutMs says %d ms.\nUpdate the mirror — the `fetching` grace is exactly one of these.",
			got, ProgramContentFetchTimeoutMs)
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

// constDeclarations maps every constant screencadence.go declares to the
// expression it is declared as.
func constDeclarations(t *testing.T) map[string]ast.Expr {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "screencadence.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing screencadence.go: %v — if this file moved, MOVE THIS FENCE, do not delete it", err)
	}
	out := map[string]ast.Expr{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					out[name.Name] = vs.Values[i]
				}
			}
		}
	}
	return out
}

// exprLeaves splits a constant's declared expression into the identifiers it
// names and the numeric literals it contains.
func exprLeaves(expr ast.Expr) (idents, literals []string) {
	ast.Inspect(expr, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Ident:
			idents = append(idents, v.Name)
		case *ast.BasicLit:
			literals = append(literals, v.Value)
		}
		return true
	})
	return idents, literals
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
