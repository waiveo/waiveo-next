package playerbootretry

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This file is the guard's EXECUTION engine: it turns PlayerTask.brs's control
// flow into a statement tree and walks that tree under a stated scenario,
// reporting how the walk ended — fell out the bottom (the loop goes round
// again), returned, or left the loop.
//
// # Why an executor and not a source-text check
//
// The guard this package exists for — "a screen that fails its first program
// pull retries forever" — was previously asserted by looking for statements
// that BEGIN with `return` inside a branch. That reads as thorough and is not:
// BrightScript's ordinary spelling of a one-shot exit is `if not everSucceeded
// then return`, a single line that begins with `if`, and the file already uses
// that spelling twice for its stop check. Re-introducing the dead-screen defect
// in the most natural way therefore went unnoticed. Patching that one predicate
// would only move the hole (`exit while` ends the loop just as dead, and so
// does an early exit written three levels down inside the branch).
//
// The property being guarded is not "no line says return". It is "under these
// conditions, control reaches the bottom of the loop body". That is a question
// about EXECUTION, so this answers it by executing — over the real shipped
// file, with the conditions supplied by the test.
//
// # Why this is sound to trust
//
// Conditions evaluate to true, false, or UNKNOWN. Anything this small evaluator
// does not model — arithmetic, string comparison, a call it was not told the
// answer for — is unknown, and an unknown condition explores BOTH arms. So the
// approximation only ever considers MORE paths than the Roku would, never
// fewer: an exit this reports might in principle be unreachable on hardware,
// but an exit that exists can never be missed. For a guard whose failure mode
// is a wall of dead TVs, that is the direction to be wrong in.
//
// The engine has its own tests (below), including the exact regression shapes
// it exists to catch, so it is not itself an untested safety net.

// tri is a three-valued condition result: an unmodelled condition is UNKNOWN
// rather than guessed, and an unknown condition means both arms are walked.
type tri int

const (
	triUnknown tri = iota
	triTrue
	triFalse
)

func (t tri) not() tri {
	switch t {
	case triTrue:
		return triFalse
	case triFalse:
		return triTrue
	}
	return triUnknown
}

func triAnd(a, b tri) tri {
	if a == triFalse || b == triFalse {
		return triFalse
	}
	if a == triTrue && b == triTrue {
		return triTrue
	}
	return triUnknown
}

func triOr(a, b tri) tri {
	if a == triTrue || b == triTrue {
		return triTrue
	}
	if a == triFalse && b == triFalse {
		return triFalse
	}
	return triUnknown
}

// stmtKind classifies a parsed statement. Everything that is not control flow
// is a leaf: this engine cares only about where control goes.
type stmtKind int

const (
	stmtLeaf stmtKind = iota
	stmtIf
	stmtWhile
	stmtFor
	stmtReturn
	stmtExitWhile
	stmtExitFor
	stmtHalt // BrightScript's bare `end` / `stop`: the thread stops here
)

// arm is one branch of an if-chain: its condition (empty for `else`) and body.
type arm struct {
	cond string
	body []stmt
}

// stmt is one parsed statement, keeping the source line it came from so a
// failure names the line an operator would have to look at.
type stmt struct {
	kind stmtKind
	at   line
	arms []arm  // stmtIf
	body []stmt // stmtWhile / stmtFor
	// assignTo/assignVal record `x = true` / `x = false`, the only assignments
	// worth modelling: they are how a flag a later condition reads gets its
	// value. Every other assignment CLEARS the name (assignTo set, assignVal
	// unknown) rather than leaving a stale one, so a variable this engine
	// cannot follow becomes unknown and widens the walk instead of narrowing
	// it.
	assignTo  string
	assignVal tri
}

var (
	reSingleLineIf = regexp.MustCompile(`(?i)^if\s+(.+?)\s+then\s+(\S.*)$`)
	reBlockIf      = regexp.MustCompile(`(?i)^if\s+(.+?)(?:\s+then)?$`)
	reElseIf       = regexp.MustCompile(`(?i)^else\s+if\s+(.+?)(?:\s+then)?$`)
	reWhile        = regexp.MustCompile(`(?i)^while\s+(.+)$`)
	reFor          = regexp.MustCompile(`(?i)^for\s+`)
)

// parseStatements parses lines[i:] until one of the terminating keywords, and
// returns the statements plus the index OF the terminator.
func parseStatements(t *testing.T, lines []line, i int) ([]stmt, int) {
	t.Helper()
	var out []stmt
	for i < len(lines) {
		l := lines[i]
		if isBlockTerminator(l.text) {
			return out, i
		}
		s, next := parseStatement(t, lines, i)
		out = append(out, s...)
		i = next
	}
	return out, i
}

func isBlockTerminator(text string) bool {
	lower := strings.ToLower(text)
	switch {
	case lower == "end if", lower == "end while", lower == "end for", lower == "end sub", lower == "end function":
		return true
	case lower == "else" || strings.HasPrefix(lower, "else if ") || strings.HasPrefix(lower, "elseif "):
		return true
	case lower == "next": // `next` closes a for in the older spelling
		return true
	}
	return false
}

// parseStatement parses ONE source line (which may carry several `:`-separated
// statements, or open a block) and returns the statements it contributes plus
// the index of the next unconsumed line.
func parseStatement(t *testing.T, lines []line, i int) ([]stmt, int) {
	t.Helper()
	l := lines[i]

	// A colon-separated line is several statements — `pullFailures = 0 : return`
	// is one more idiomatic way to write the regression, and splitting here is
	// what keeps every one of them visible to the walk. Splitting is depth- and
	// quote-aware so an assoc-array literal's `{ phase: "start" }` is untouched.
	if parts := splitStatements(l.text); len(parts) > 1 {
		var out []stmt
		for _, p := range parts {
			sub, _ := parseStatement(t, []line{{n: l.n, text: p}}, 0)
			out = append(out, atLine(sub, l)...)
		}
		return out, i + 1
	}

	lower := strings.ToLower(l.text)
	switch {
	case lower == "return" || strings.HasPrefix(lower, "return ") || strings.HasPrefix(lower, "return("):
		return []stmt{{kind: stmtReturn, at: l}}, i + 1
	case lower == "exit while":
		return []stmt{{kind: stmtExitWhile, at: l}}, i + 1
	case lower == "exit for":
		return []stmt{{kind: stmtExitFor, at: l}}, i + 1
	case lower == "end" || lower == "stop":
		return []stmt{{kind: stmtHalt, at: l}}, i + 1
	}

	if m := reSingleLineIf.FindStringSubmatch(l.text); m != nil {
		return []stmt{singleLineIfStmt(t, l, m[1], m[2])}, i + 1
	}
	if m := reWhile.FindStringSubmatch(l.text); m != nil {
		body, end := parseStatements(t, lines, i+1)
		requireCloser(t, lines, end, "end while", l)
		return []stmt{{kind: stmtWhile, at: l, arms: []arm{{cond: m[1]}}, body: body}}, end + 1
	}
	if reFor.MatchString(l.text) {
		body, end := parseStatements(t, lines, i+1)
		requireCloser(t, lines, end, "end for", l)
		return []stmt{{kind: stmtFor, at: l, body: body}}, end + 1
	}
	if strings.HasPrefix(lower, "if ") {
		return parseIfChain(t, lines, i)
	}
	return []stmt{leafStmt(l)}, i + 1
}

// singleLineIfStmt builds the if-chain for `if <cond> then <stmt> [else <stmt>]`.
func singleLineIfStmt(t *testing.T, l line, cond, tail string) stmt {
	t.Helper()
	thenPart, elsePart := splitSingleLineElse(tail)
	thenStmts, _ := parseStatement(t, []line{{n: l.n, text: thenPart}}, 0)
	s := stmt{kind: stmtIf, at: l, arms: []arm{{cond: cond, body: atLine(thenStmts, l)}}}
	if elsePart != "" {
		elseStmts, _ := parseStatement(t, []line{{n: l.n, text: elsePart}}, 0)
		s.arms = append(s.arms, arm{cond: "", body: atLine(elseStmts, l)})
	}
	return s
}

// atLine re-attaches statements split out of one source line to that WHOLE
// line, so a failure quotes `if not everSucceeded then return` rather than the
// bare `return` a reader would then have to go find.
func atLine(stmts []stmt, l line) []stmt {
	for i := range stmts {
		stmts[i].at = l
		stmts[i].body = atLine(stmts[i].body, l)
		for j := range stmts[i].arms {
			stmts[i].arms[j].body = atLine(stmts[i].arms[j].body, l)
		}
	}
	return stmts
}

func parseIfChain(t *testing.T, lines []line, i int) ([]stmt, int) {
	t.Helper()
	open := lines[i]
	m := reBlockIf.FindStringSubmatch(open.text)
	if m == nil {
		t.Fatalf("line %d: cannot read the condition of %q", open.n, open.text)
	}
	s := stmt{kind: stmtIf, at: open}
	cond := m[1]
	for {
		body, end := parseStatements(t, lines, i+1)
		s.arms = append(s.arms, arm{cond: cond, body: body})
		if end >= len(lines) {
			t.Fatalf("line %d: `if` chain is never closed by `end if`", open.n)
		}
		next := strings.ToLower(lines[end].text)
		switch {
		case next == "end if":
			return []stmt{s}, end + 1
		case next == "else":
			cond, i = "", end
		case strings.HasPrefix(next, "else if ") || strings.HasPrefix(next, "elseif "):
			em := reElseIf.FindStringSubmatch(lines[end].text)
			if em == nil {
				t.Fatalf("line %d: cannot read the condition of %q", lines[end].n, lines[end].text)
			}
			cond, i = em[1], end
		default:
			t.Fatalf("line %d: unexpected %q while reading the `if` opened at line %d", lines[end].n, lines[end].text, open.n)
		}
	}
}

func requireCloser(t *testing.T, lines []line, end int, want string, open line) {
	t.Helper()
	if end >= len(lines) || !strings.EqualFold(lines[end].text, want) {
		t.Fatalf("line %d: the block opened here is not closed by `%s`", open.n, want)
	}
}

// leafStmt records the one thing a non-control-flow line can do that changes a
// later condition: assign to a name.
func leafStmt(l line) stmt {
	s := stmt{kind: stmtLeaf, at: l}
	toks := tokenize(l.text)
	if len(toks) >= 3 && isPath(toks[0]) && toks[1] == "=" {
		s.assignTo = strings.ToLower(toks[0])
		s.assignVal = triUnknown
		if len(toks) == 3 {
			switch strings.ToLower(toks[2]) {
			case "true":
				s.assignVal = triTrue
			case "false":
				s.assignVal = triFalse
			}
		}
	}
	return s
}

// splitStatements splits a line on top-level `:` separators, ignoring colons
// inside strings, brackets or an assoc-array literal.
func splitStatements(text string) []string {
	var parts []string
	depth, inString, start := 0, false, 0
	for i, r := range text {
		switch {
		case r == '"':
			inString = !inString
		case inString:
		case r == '(' || r == '[' || r == '{':
			depth++
		case r == ')' || r == ']' || r == '}':
			depth--
		case r == ':' && depth == 0:
			parts = append(parts, strings.TrimSpace(text[start:i]))
			start = i + 1
		}
	}
	parts = append(parts, strings.TrimSpace(text[start:]))
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitSingleLineElse splits `<then> else <else>` at the top level.
func splitSingleLineElse(tail string) (thenPart, elsePart string) {
	toks, spans := tokenizeWithSpans(tail)
	depth := 0
	for i, tk := range toks {
		switch tk {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
		}
		if depth == 0 && strings.EqualFold(tk, "else") {
			return strings.TrimSpace(tail[:spans[i]]), strings.TrimSpace(tail[spans[i]+len(tk):])
		}
	}
	return strings.TrimSpace(tail), ""
}

// ---------------------------------------------------------------- evaluation

// env maps a lower-cased name — a variable, a dotted member path, or a
// zero-argument call written with its parentheses ("wvtaskshouldkeeprunning()")
// — to what the scenario says it is. A name that is absent is UNKNOWN.
type env map[string]tri

func (e env) clone() env {
	out := make(env, len(e))
	for k, v := range e {
		out[k] = v
	}
	return out
}

// evalCond evaluates a condition to true/false/unknown.
func evalCond(expr string, e env) tri {
	p := &exprParser{toks: tokenize(expr), env: e}
	v := p.parseOr()
	if !p.done() {
		// Trailing tokens mean the parse did not cover the whole condition, so
		// the result cannot be trusted — widen rather than guess.
		return triUnknown
	}
	return v
}

type exprParser struct {
	toks []string
	i    int
	env  env
}

func (p *exprParser) done() bool { return p.i >= len(p.toks) }
func (p *exprParser) peek() string {
	if p.done() {
		return ""
	}
	return p.toks[p.i]
}
func (p *exprParser) next() string { t := p.peek(); p.i++; return t }

func (p *exprParser) parseOr() tri {
	v := p.parseAnd()
	for strings.EqualFold(p.peek(), "or") {
		p.next()
		v = triOr(v, p.parseAnd())
	}
	return v
}

func (p *exprParser) parseAnd() tri {
	v := p.parseUnary()
	for strings.EqualFold(p.peek(), "and") {
		p.next()
		v = triAnd(v, p.parseUnary())
	}
	return v
}

func (p *exprParser) parseUnary() tri {
	if strings.EqualFold(p.peek(), "not") {
		p.next()
		return p.parseUnary().not()
	}
	return p.parseOperand()
}

// parseOperand reads one operand. If anything follows it other than a boolean
// connective or a close paren — a comparison, arithmetic, an argument list —
// the whole operand is consumed and reported UNKNOWN: this engine models
// control flow, not values, and an unmodelled operand must widen the walk.
func (p *exprParser) parseOperand() tri {
	var v tri
	simple := true
	if p.peek() == "(" {
		p.next()
		v = p.parseOr()
		if p.peek() != ")" {
			return triUnknown
		}
		p.next()
	} else {
		v = p.value(p.next())
	}
	depth := 0
	for !p.done() {
		tk := p.peek()
		if depth == 0 && (strings.EqualFold(tk, "and") || strings.EqualFold(tk, "or") || tk == ")") {
			break
		}
		switch tk {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
		}
		p.next()
		simple = false
	}
	if !simple {
		return triUnknown
	}
	return v
}

func (p *exprParser) value(tok string) tri {
	switch strings.ToLower(tok) {
	case "true":
		return triTrue
	case "false":
		return triFalse
	}
	if !isPath(tok) {
		return triUnknown
	}
	if v, ok := p.env[strings.ToLower(tok)]; ok {
		return v
	}
	return triUnknown
}

// ------------------------------------------------------------------ walking

// walkKind is how one execution path through a body ended.
type walkKind int

const (
	walkFallThrough walkKind = iota // reached the bottom: a loop body goes round again
	walkReturn
	walkExitWhile
	walkExitFor
	walkHalt
)

func (w walkKind) String() string {
	switch w {
	case walkFallThrough:
		return "fell through to the bottom"
	case walkReturn:
		return "returned"
	case walkExitWhile:
		return "left the loop (exit while)"
	case walkExitFor:
		return "left the for loop"
	case walkHalt:
		return "halted the thread"
	}
	return "?"
}

// path is one explored execution path's outcome.
type path struct {
	kind walkKind
	at   line // the statement that ended it; zero for a fall-through
	env  env
}

// walk executes stmts under e and returns every path's outcome. Paths multiply
// only at unknown conditions, so a fully-determined scenario yields one.
func walk(stmts []stmt, e env) []path {
	live := []path{{kind: walkFallThrough, env: e}}
	var done []path
	for _, s := range stmts {
		var next []path
		for _, p := range live {
			for _, r := range walkStmt(s, p.env) {
				if r.kind == walkFallThrough {
					next = append(next, r)
					continue
				}
				done = append(done, r)
			}
		}
		live = next
		if len(live) == 0 {
			break
		}
	}
	return append(done, live...)
}

func walkStmt(s stmt, e env) []path {
	switch s.kind {
	case stmtReturn:
		return []path{{kind: walkReturn, at: s.at, env: e}}
	case stmtExitWhile:
		return []path{{kind: walkExitWhile, at: s.at, env: e}}
	case stmtExitFor:
		return []path{{kind: walkExitFor, at: s.at, env: e}}
	case stmtHalt:
		return []path{{kind: walkHalt, at: s.at, env: e}}
	case stmtLeaf:
		if s.assignTo != "" {
			e = e.clone()
			if s.assignVal == triUnknown {
				delete(e, s.assignTo)
			} else {
				e[s.assignTo] = s.assignVal
			}
		}
		return []path{{kind: walkFallThrough, env: e}}
	case stmtIf:
		var out []path
		noArmTaken := true
		for _, a := range s.arms {
			c := triTrue
			if a.cond != "" {
				c = evalCond(a.cond, e)
			}
			if c != triFalse {
				out = append(out, walk(a.body, e.clone())...)
			}
			if c == triTrue {
				noArmTaken = false
				break
			}
		}
		if noArmTaken {
			out = append(out, path{kind: walkFallThrough, env: e})
		}
		return out
	case stmtWhile, stmtFor:
		// One iteration of the body. `exit while` is absorbed here — it ends
		// THIS loop, not the caller's — while a return or a halt propagates,
		// which is exactly the distinction the guard is about.
		enter := triTrue
		if s.kind == stmtWhile && len(s.arms) == 1 {
			enter = evalCond(s.arms[0].cond, e)
		} else if s.kind == stmtFor {
			enter = triUnknown // a for's trip count is a value question
		}
		var out []path
		if enter != triFalse {
			for _, r := range walk(s.body, e.clone()) {
				switch {
				case r.kind == walkExitWhile && s.kind == stmtWhile,
					r.kind == walkExitFor && s.kind == stmtFor,
					r.kind == walkFallThrough:
					out = append(out, path{kind: walkFallThrough, env: e})
				default:
					out = append(out, r)
				}
			}
		}
		if enter != triTrue {
			out = append(out, path{kind: walkFallThrough, env: e})
		}
		return out
	}
	return []path{{kind: walkFallThrough, env: e}}
}

// exits summarises the non-fall-through outcomes of a walk, deduplicated and in
// line order, for a failure message.
func exits(paths []path) []path {
	seen := map[int]bool{}
	var out []path
	for _, p := range paths {
		if p.kind == walkFallThrough || seen[p.at.n] {
			continue
		}
		seen[p.at.n] = true
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.n < out[j].at.n })
	return out
}

func describeExits(paths []path) string {
	var b []string
	for _, p := range exits(paths) {
		b = append(b, "line "+itoa(p.at.n)+": "+p.at.text+" ("+p.kind.String()+")")
	}
	return strings.Join(b, "\n  ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// ----------------------------------------------------------------- tokenizer

func tokenize(s string) []string {
	toks, _ := tokenizeWithSpans(s)
	return toks
}

// tokenizeWithSpans returns the tokens and each token's byte offset.
func tokenizeWithSpans(s string) ([]string, []int) {
	var toks []string
	var spans []int
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				j++
			}
			if j < len(s) {
				j++
			}
			toks, spans = append(toks, s[i:j]), append(spans, i)
			i = j
		case isIdentStart(c):
			j := i
			for j < len(s) && (isIdentChar(s[j]) || (s[j] == '.' && j+1 < len(s) && isIdentStart(s[j+1]))) {
				j++
			}
			tok := s[i:j]
			if j+1 < len(s) && s[j] == '(' && s[j+1] == ')' {
				tok, j = tok+"()", j+2
			}
			toks, spans = append(toks, tok), append(spans, i)
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == '.') {
				j++
			}
			toks, spans = append(toks, s[i:j]), append(spans, i)
			i = j
		case i+1 < len(s) && (s[i:i+2] == "<>" || s[i:i+2] == "<=" || s[i:i+2] == ">="):
			toks, spans = append(toks, s[i:i+2]), append(spans, i)
			i += 2
		default:
			toks, spans = append(toks, string(c)), append(spans, i)
			i++
		}
	}
	return toks, spans
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

// isPath reports whether tok is a name, member path, or zero-argument call.
func isPath(tok string) bool {
	if tok == "" || !isIdentStart(tok[0]) {
		return false
	}
	tok = strings.TrimSuffix(tok, "()")
	for i := 0; i < len(tok); i++ {
		if !isIdentChar(tok[i]) && tok[i] != '.' {
			return false
		}
	}
	return true
}

// ------------------------------------------------- the engine's own tests

// parseSnippet parses inline BrightScript, so the engine can be tested on the
// exact regression shapes without touching the shipped player.
func parseSnippet(t *testing.T, src string) []stmt {
	t.Helper()
	var lines []line
	for i, raw := range strings.Split(src, "\n") {
		text := strings.TrimSpace(stripComment(raw))
		if text == "" {
			continue
		}
		lines = append(lines, line{n: i + 1, text: text})
	}
	stmts, end := parseStatements(t, lines, 0)
	if end != len(lines) {
		t.Fatalf("snippet parse stopped at line %d (%q)", lines[end].n, lines[end].text)
	}
	return stmts
}

// walkKinds returns the distinct outcomes of walking a snippet, for a compact
// assertion.
func walkKinds(stmts []stmt, e env) map[walkKind]bool {
	out := map[walkKind]bool{}
	for _, p := range walk(stmts, e) {
		out[p.kind] = true
	}
	return out
}

// Every idiomatic spelling of "give up here" must be seen. The single-line
// form is first because it is the one the previous guard missed, and the one a
// developer reaching for the shortest edit writes.
func TestTheEngineSeesEveryIdiomaticEarlyExit(t *testing.T) {
	cases := map[string]string{
		"single-line if then return":   "if not everSucceeded then return",
		"block if then return":         "if not everSucceeded\nreturn\nend if",
		"single-line with a value":     "if not everSucceeded then return false",
		"colon-separated return":       "if not everSucceeded\npullFailures = 0 : return\nend if",
		"single-line else arm returns": "if everSucceeded then print \"x\" else return",
		"exit while ends the loop too": "if not everSucceeded then exit while",
		"nested three deep":            "if not everSucceeded\nif true\nif true\nreturn\nend if\nend if\nend if",
		"bare end halts the thread":    "if not everSucceeded then end",
		"lower-cased keywords":         "IF NOT everSucceeded THEN RETURN",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			got := walkKinds(parseSnippet(t, src), env{"eversucceeded": triFalse})
			if got[walkFallThrough] || len(got) != 1 {
				t.Fatalf("outcomes = %v, want exactly one non-fall-through exit — this shape of giving up must be visible to the guard", keysOf(got))
			}
		})
	}
}

func TestTheEngineDoesNotInventAnExit(t *testing.T) {
	// The other direction: the guard must not fail on code that genuinely
	// retries, or it stops being run.
	cases := map[string]string{
		"a branch that is not taken":       "if everSucceeded then return",
		"a return inside a nested while":   "while n < 3\nexit while\nend while",
		"a word that merely contains it":   "returnCode = 1",
		"the word inside a string literal": "print \"return\"",
		"backing off and going round":      "pullFailures = pullFailures + 1\nwaitMs = wvProgramRetryBackoffMs(pullFailures)",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			got := walkKinds(parseSnippet(t, src), env{"eversucceeded": triFalse})
			if !got[walkFallThrough] || len(got) != 1 {
				t.Fatalf("outcomes = %v, want only a fall-through", keysOf(got))
			}
		})
	}
}

func TestAnUnknownConditionExploresBothArms(t *testing.T) {
	// The soundness property: what the engine cannot evaluate, it does not
	// assume away. `pullFailures >= wvRetryStatusAfterFailures()` is a value
	// question, so both arms are walked and a return hidden in either is found.
	got := walkKinds(parseSnippet(t, "if pullFailures >= wvRetryStatusAfterFailures()\nreturn\nend if"), env{})
	if !got[walkReturn] || !got[walkFallThrough] {
		t.Fatalf("outcomes = %v, want both a return and a fall-through", keysOf(got))
	}
}

func TestTheEngineFollowsAFlagItWasNotGiven(t *testing.T) {
	// An assignment inside the body decides a later condition, so the walk has
	// to follow it rather than treating every read as unknown.
	got := walkKinds(parseSnippet(t, "everSucceeded = true\nif everSucceeded then return"), env{})
	if !got[walkReturn] || got[walkFallThrough] {
		t.Fatalf("outcomes = %v, want only a return", keysOf(got))
	}
	// And an assignment it cannot model must CLEAR the name, not keep a stale
	// value that would hide a branch.
	got = walkKinds(parseSnippet(t, "everSucceeded = somethingElse\nif everSucceeded then return"), env{"eversucceeded": triFalse})
	if !got[walkReturn] || !got[walkFallThrough] {
		t.Fatalf("outcomes = %v, want both arms explored once the flag is no longer known", keysOf(got))
	}
}

func keysOf(m map[walkKind]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k.String())
	}
	sort.Strings(out)
	return out
}
