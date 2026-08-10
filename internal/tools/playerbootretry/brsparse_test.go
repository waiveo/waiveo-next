package playerbootretry

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// playerTaskPath is the real shipped source. Parsing the file the Roku
// actually runs — rather than a fixture copy of it — is the whole point: a
// copy drifts, and a guard reading a drifted copy passes while the shipped
// player regresses.
var playerTaskPath = filepath.Join("..", "..", "..", "player-v3", "components", "PlayerTask.brs")

// line is one source line stripped of comments and indentation, keeping its
// 1-based number so a failure can point at it.
type line struct {
	n    int
	text string
}

// readPlayerTask loads PlayerTask.brs as comment-free, blank-free lines.
//
// Comment stripping is quote-aware: PlayerTask publishes status strings
// containing apostrophes is not something it does today, but a naive strip
// would corrupt any line that ever did, and a corrupted line silently changes
// what this guard believes the control flow is.
func readPlayerTask(t *testing.T) []line {
	t.Helper()
	raw, err := os.ReadFile(playerTaskPath)
	if err != nil {
		t.Fatalf("read PlayerTask.brs: %v", err)
	}
	var out []line
	for i, text := range strings.Split(string(raw), "\n") {
		stripped := strings.TrimSpace(stripComment(text))
		if stripped == "" {
			continue
		}
		out = append(out, line{n: i + 1, text: stripped})
	}
	if len(out) == 0 {
		t.Fatal("PlayerTask.brs parsed to no code lines")
	}
	return out
}

// stripComment removes a trailing BrightScript `'` comment, ignoring
// apostrophes inside double-quoted strings.
func stripComment(text string) string {
	inString := false
	for i, r := range text {
		switch r {
		case '"':
			inString = !inString
		case '\'':
			if !inString {
				return text[:i]
			}
		}
	}
	return text
}

// subBody returns the lines strictly between `sub <name>(` and its `end sub`.
func subBody(t *testing.T, lines []line, name string) []line {
	t.Helper()
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l.text, "sub "+name+"(") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("PlayerTask.brs has no `sub %s(` — the guard cannot read a player it does not recognise", name)
	}
	for i := start; i < len(lines); i++ {
		if lines[i].text == "end sub" {
			return lines[start:i]
		}
	}
	t.Fatalf("`sub %s` is never closed by `end sub`", name)
	return nil
}

// singleLineIf matches `if <cond> then <statement>` — a complete statement
// that opens no block, which the depth tracking below must not count.
var singleLineIf = regexp.MustCompile(`(?i)^if\s+.+\s+then\s+\S`)

// opensBlock reports whether a line opens a control-flow block that a matching
// `end …` closes.
func opensBlock(text string) bool {
	lower := strings.ToLower(text)
	switch {
	case singleLineIf.MatchString(text):
		return false
	case strings.HasPrefix(lower, "if "):
		return true
	case strings.HasPrefix(lower, "while "):
		return true
	case strings.HasPrefix(lower, "for "):
		return true
	default:
		return false
	}
}

// closesBlock reports whether a line closes one.
func closesBlock(text string) bool {
	lower := strings.ToLower(text)
	return lower == "end if" || lower == "end while" || lower == "end for" || lower == "end function"
}

// whileBlocks returns each `while …` block's body, in source order, at the top
// level of the given lines.
func whileBlocks(t *testing.T, lines []line) [][]line {
	t.Helper()
	var blocks [][]line
	depth := 0
	start := -1
	for i, l := range lines {
		lower := strings.ToLower(l.text)
		if strings.HasPrefix(lower, "while ") {
			if depth == 0 {
				start = i + 1
			}
			depth++
			continue
		}
		if opensBlock(l.text) && depth > 0 {
			depth++
			continue
		}
		if lower == "end while" {
			depth--
			if depth == 0 {
				blocks = append(blocks, lines[start:i])
			}
			continue
		}
		if closesBlock(l.text) && depth > 0 {
			depth--
		}
	}
	if depth != 0 {
		t.Fatalf("unbalanced while/end while in PlayerTask.brs (depth %d at end) — the guard cannot read this control flow", depth)
	}
	return blocks
}

// branch is one arm of an if/else-if/else chain: the condition line that
// introduced it (empty for the bare `else`) and the statements inside it.
type branch struct {
	cond  line
	stmts []line
}

// ifChain finds the top-level if-chain in body whose opening condition
// contains needle, and returns its arms in order.
func ifChain(t *testing.T, body []line, needle string) []branch {
	t.Helper()
	start := -1
	depth := 0
	for i, l := range body {
		if depth == 0 && opensBlock(l.text) && strings.HasPrefix(strings.ToLower(l.text), "if ") && strings.Contains(l.text, needle) {
			start = i
			break
		}
		if opensBlock(l.text) {
			depth++
		} else if closesBlock(l.text) {
			depth--
		}
	}
	if start < 0 {
		t.Fatalf("no top-level `if … %s …` chain found — PlayerTask's failure classification has been restructured; re-read this guard before deleting it", needle)
	}

	branches := []branch{{cond: body[start]}}
	depth = 0
	for i := start + 1; i < len(body); i++ {
		l := body[i]
		lower := strings.ToLower(l.text)
		if depth == 0 && (strings.HasPrefix(lower, "else if ") || lower == "else") {
			branches = append(branches, branch{cond: l})
			continue
		}
		if depth == 0 && lower == "end if" {
			return branches
		}
		if opensBlock(l.text) {
			depth++
		} else if closesBlock(l.text) {
			depth--
		}
		branches[len(branches)-1].stmts = append(branches[len(branches)-1].stmts, l)
	}
	t.Fatalf("the `if … %s …` chain is never closed", needle)
	return nil
}

// loopExit is one statement that ends the enclosing loop — or the whole task.
type loopExit struct {
	at   line
	kind string // "return", "exit while", "exit for", "halt"
}

// loopExits reports every statement in these lines that stops going round.
//
// It exists because the two loop guards in this file each used to decide that
// for themselves, differently, and the program-loop one decided it wrong: it
// asked whether a statement BEGAN with `return`, which sees
//
//	return
//
// but not
//
//	if not everSucceeded then return
//
// — BrightScript's ordinary spelling, used twice in PlayerTask.brs itself for
// the stop check, and therefore the exact form a developer re-introducing the
// one-shot boot defect would reach for. One shared definition, applied to
// tokens rather than prefixes, is the fix; `exit while` and a bare `end` are
// included because they end the retry every bit as terminally as a return.
//
// Tokenizing (rather than substring-matching, which the pairing guard used) is
// what keeps `returnCode = 1` and a log line containing the word "return" from
// reading as exits.
func loopExits(stmts []line) []loopExit {
	var out []loopExit
	for _, l := range stmts {
		toks := tokenize(l.text)
		for i, tk := range toks {
			switch {
			case strings.EqualFold(tk, "return"):
				out = append(out, loopExit{at: l, kind: "return"})
			case strings.EqualFold(tk, "exit") && i+1 < len(toks) && strings.EqualFold(toks[i+1], "while"):
				out = append(out, loopExit{at: l, kind: "exit while"})
			case strings.EqualFold(tk, "exit") && i+1 < len(toks) && strings.EqualFold(toks[i+1], "for"):
				out = append(out, loopExit{at: l, kind: "exit for"})
			case len(toks) == 1 && (strings.EqualFold(tk, "end") || strings.EqualFold(tk, "stop")):
				out = append(out, loopExit{at: l, kind: "halt"})
			}
		}
	}
	return out
}

// leavesTheTask reports the exits that end the whole task rather than just the
// enclosing loop — the only kind the pairing loop forbids, since it leaves
// legitimately (into the program poll) via `exit while`.
func leavesTheTask(stmts []line) []loopExit {
	var out []loopExit
	for _, e := range loopExits(stmts) {
		if e.kind == "return" || e.kind == "halt" {
			out = append(out, e)
		}
	}
	return out
}

// containsStatement reports whether any statement in the branch is (or begins
// with) stmt.
func (b branch) containsStatement(stmt string) bool {
	for _, l := range b.stmts {
		if l.text == stmt || strings.HasPrefix(l.text, stmt+" ") || strings.HasPrefix(l.text, stmt+"(") {
			return true
		}
	}
	return false
}

// containsCall reports whether any statement in the branch mentions name as a
// call.
func (b branch) containsCall(name string) bool {
	for _, l := range b.stmts {
		if strings.Contains(l.text, name+"(") {
			return true
		}
	}
	return false
}

// intFunc reads a zero-argument `function <name>() as Integer` whose body is a
// single `return <literal>`, which is how PlayerTask spells its tunables.
func intFunc(t *testing.T, lines []line, name string) int {
	t.Helper()
	for i, l := range lines {
		if !strings.HasPrefix(l.text, "function "+name+"()") {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j].text == "end function" {
				break
			}
			if v, ok := strings.CutPrefix(lines[j].text, "return "); ok {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil {
					t.Fatalf("%s returns %q, which is not an integer literal", name, v)
				}
				return n
			}
		}
		t.Fatalf("%s has no integer `return`", name)
	}
	t.Fatalf("PlayerTask.brs has no `function %s()` — the retry policy has been restructured", name)
	return 0
}

// The structural half of the regression guard: of the program loop's failure
// arms, exactly ONE may leave the loop, and it must be the dead-credential one.
//
// Stated as "the only branch that exits is the dead-credential one" rather than
// as "the first-pull branch does not return", because the one-shot defect can
// be re-introduced in any arm — and because a NEW terminal branch is exactly as
// bad as the old one, whatever it is called.
//
// The behavioural guard in behavior_test.go is the stronger statement of the
// same property (it walks the loop under a stated scenario, so it sees an exit
// wherever it is nested and whatever it is spelled). This one survives beside
// it because it says something the walk does not: WHICH arm the one legal exit
// belongs to. A player that retried a revoked credential forever would satisfy
// every scenario and still be wrong.
func TestProgramLoopOnlyTerminalExitIsTheDeadCredential(t *testing.T) {
	body := subBody(t, readPlayerTask(t), "runPhoton")
	loops := whileBlocks(t, body)
	if len(loops) != 2 {
		t.Fatalf("runPhoton has %d top-level while loop(s), want 2 (the pairing retry loop and the program poll loop)", len(loops))
	}
	program := loops[1]

	var returning []branch
	for _, b := range ifChain(t, program, "prog.ok") {
		// Any spelling of leaving: `return`, `exit while`, a bare `end` — and
		// at any depth inside the arm, not just as its first word. See
		// loopExits for why that breadth is the whole point.
		if len(loopExits(b.stmts)) > 0 {
			returning = append(returning, b)
		}
	}

	if len(returning) != 1 {
		var where []string
		for _, b := range returning {
			for _, e := range loopExits(b.stmts) {
				where = append(where, "branch at line "+strconv.Itoa(b.cond.n)+" ("+b.cond.text+") exits at line "+strconv.Itoa(e.at.n)+": "+e.at.text)
			}
		}
		t.Fatalf("the program loop has %d branch(es) that LEAVE THE LOOP, want exactly 1 (the needsRepair dead-credential exit).\n"+
			"A branch that leaves is a screen that gives up. This is the one-shot boot-retry defect:\n  %s",
			len(returning), strings.Join(where, "\n  "))
	}
	if !strings.Contains(returning[0].cond.text, "needsRepair") {
		t.Fatalf("the program loop's only terminal exit is %q at line %d, want the needsRepair (revoked/invalid credential) branch — "+
			"every other failure must retry", returning[0].cond.text, returning[0].cond.n)
	}
}

// The retry has to actually wait. A failure branch that falls through with no
// backoff turns "never give up" into a tight loop hammering a relay that is
// already struggling — the other way to get this wrong.
func TestProgramLoopFailureBranchBacksOff(t *testing.T) {
	body := subBody(t, readPlayerTask(t), "runPhoton")
	program := whileBlocks(t, body)[1]

	chain := ifChain(t, program, "prog.ok")
	failure := chain[len(chain)-1]
	if strings.HasPrefix(strings.ToLower(failure.cond.text), "else if") && strings.Contains(failure.cond.text, "needsRepair") {
		t.Fatalf("the program loop's last branch is the needsRepair exit at line %d — there is no retry branch at all", failure.cond.n)
	}
	if !failure.containsCall("wvProgramRetryBackoffMs") {
		t.Errorf("the program loop's failure branch (line %d) never computes a backoff; a retry with no wait is a hot loop against a struggling relay", failure.cond.n)
	}
	if !failure.containsStatement("pullFailures = pullFailures + 1") {
		t.Errorf("the program loop's failure branch (line %d) does not advance the consecutive-failure count, so the backoff can never grow", failure.cond.n)
	}
}

// Never-wipe, from the other direction: once content IS rendering, a transient
// poll failure must not publish a not-ok photonResult, because PhotonScene's
// onPhotonResult tears the running cast down on one. The publish is therefore
// only legal inside the "nothing has ever rendered" arm.
func TestProgramLoopDoesNotPublishAFailureOverRenderedContent(t *testing.T) {
	body := subBody(t, readPlayerTask(t), "runPhoton")
	program := whileBlocks(t, body)[1]
	chain := ifChain(t, program, "prog.ok")
	failure := chain[len(chain)-1]

	for _, l := range failure.stmts {
		if !strings.HasPrefix(l.text, "m.top.photonResult =") {
			continue
		}
		if !publishIsGuardedByNeverRendered(failure.stmts, l) {
			t.Fatalf("line %d publishes a failure photonResult from the retry branch without an `everSucceeded` guard — "+
				"PhotonScene tears the running cast down on any not-ok result, so this blanks a working screen on one transient poll error", l.n)
		}
	}
}

// publishIsGuardedByNeverRendered reports whether the publish at target sits
// in the arm of an `everSucceeded` chain that means "nothing has ever
// rendered".
//
// Both spellings are accepted — `if not everSucceeded … end if` and
// `if everSucceeded … else …` — because which one reads better depends on what
// else is in the branch, and pinning one would make this guard a style rule.
// What it will NOT accept is a publish sitting in the arm where content IS on
// screen, or outside the chain entirely.
func publishIsGuardedByNeverRendered(stmts []line, target line) bool {
	const flag = "eversucceeded"
	inChain, neverRenderedArm := false, false
	depth := 0
	for _, l := range stmts {
		lower := strings.ToLower(l.text)
		if l.n == target.n {
			return inChain && neverRenderedArm
		}
		if !inChain {
			if strings.HasPrefix(lower, "if ") && strings.Contains(lower, flag) && opensBlock(l.text) {
				inChain = true
				depth = 1
				neverRenderedArm = strings.Contains(lower, "not "+flag)
			}
			continue
		}
		switch {
		case depth == 1 && lower == "else":
			neverRenderedArm = !neverRenderedArm
		case depth == 1 && strings.HasPrefix(lower, "else if "):
			neverRenderedArm = strings.Contains(lower, "not "+flag)
		case opensBlock(l.text):
			depth++
		case closesBlock(l.text):
			depth--
			if depth == 0 {
				inChain = false
			}
		}
	}
	return false
}

// The pairing path had the identical one-shot shape, and it bites one step
// earlier: a freshly provisioned screen pairs against a relay that may not be
// listening yet. Its loop's ONLY return may be the stop-control check, which
// is the task's owner asking it to stop, not a failure giving up.
func TestPairingLoopRetriesAndNeverGivesUp(t *testing.T) {
	body := subBody(t, readPlayerTask(t), "runPhoton")
	pairing := whileBlocks(t, body)[0]

	// `exit while` is legal here (a successful pairing, and the never-wipe
	// fallback to a persisted one, both leave this loop for the program poll),
	// so only the exits that end the TASK are forbidden.
	for _, e := range leavesTheTask(pairing) {
		if !strings.Contains(e.at.text, "wvTaskShouldKeepRunning") {
			t.Errorf("line %d leaves the task from inside the pairing loop (%q, %s); the only legal exit is the owner's stop request — "+
				"a pairing failure with no persisted state must retry, not strand the screen", e.at.n, e.at.text, e.kind)
		}
	}

	var sawBackoff, sawSuccessExit bool
	for _, l := range pairing {
		if strings.Contains(l.text, "wvProgramRetryBackoffMs(") {
			sawBackoff = true
		}
		if strings.EqualFold(l.text, "exit while") {
			sawSuccessExit = true
		}
	}
	if !sawBackoff {
		t.Error("the pairing loop never backs off between attempts")
	}
	if !sawSuccessExit {
		t.Error("the pairing loop has no `exit while`, so a SUCCESSFUL pairing never reaches the program loop")
	}
}

// The backoff's shape, read out of the player's own tunables. These are the
// numbers that decide how long a wall stays dark after the relay comes back.
func TestRetryBackoffPolicy(t *testing.T) {
	lines := readPlayerTask(t)
	base := intFunc(t, lines, "wvProgramRetryBaseMs")
	cap := intFunc(t, lines, "wvProgramRetryCapMs")
	statusAfter := intFunc(t, lines, "wvRetryStatusAfterFailures")
	poll := intFunc(t, lines, "wvProgramPollIntervalMs")

	if base <= 0 || cap <= 0 {
		t.Fatalf("backoff base %dms / cap %dms must both be positive", base, cap)
	}
	if base >= cap {
		t.Errorf("backoff base %dms is not below the cap %dms, so it never backs off", base, cap)
	}
	if base > poll {
		t.Errorf("the first retry waits %dms, longer than the healthy poll interval %dms — a screen showing NOTHING must not "+
			"retry more slowly than one that is working", base, poll)
	}
	if cap > 60000 {
		t.Errorf("backoff cap is %dms; a screen must recover within a minute of its relay returning, not wait out an exponent", cap)
	}
	if statusAfter < 2 {
		t.Errorf("a status is surfaced after %d failure(s); an ordinary relay restart would put a message on a wall", statusAfter)
	}
	if statusAfter > 30 {
		t.Errorf("a status is surfaced only after %d failures; a genuinely stuck screen stays a silent black rectangle far too long", statusAfter)
	}
}

// The backoff must never hand `sleep` a value it computed by overflowing a
// 32-bit Integer: a negative wait is not a short wait, it is a busy loop. The
// implementation guards this by RETURNING at the cap before the next multiply,
// so the guard has to sit before the doubling, not after it.
func TestBackoffCannotOverflowIntoABusyLoop(t *testing.T) {
	lines := readPlayerTask(t)
	body := subBody(t, lines, "runPhoton") // ensures the file parsed as expected
	_ = body

	var fnLines []line
	collecting := false
	for _, l := range lines {
		if strings.HasPrefix(l.text, "function wvProgramRetryBackoffMs(") {
			collecting = true
			continue
		}
		if collecting {
			if l.text == "end function" {
				break
			}
			fnLines = append(fnLines, l)
		}
	}
	if len(fnLines) == 0 {
		t.Fatal("wvProgramRetryBackoffMs has no body")
	}

	multiplyAt, capReturnAt := -1, -1
	for _, l := range fnLines {
		if multiplyAt < 0 && strings.Contains(l.text, "ms = ms * 2") {
			multiplyAt = l.n
		}
		if capReturnAt < 0 && strings.Contains(l.text, "wvProgramRetryCapMs()") && strings.Contains(l.text, "return") {
			capReturnAt = l.n
		}
	}
	if multiplyAt < 0 {
		t.Fatal("wvProgramRetryBackoffMs does not double; the backoff is not exponential")
	}
	if capReturnAt < 0 {
		t.Fatal("wvProgramRetryBackoffMs never returns the cap; the doubling is unbounded and overflows into a negative sleep")
	}
	if capReturnAt < multiplyAt {
		t.Fatalf("the cap check (line %d) runs before the first doubling (line %d), so the backoff can never grow past the base",
			capReturnAt, multiplyAt)
	}
}
