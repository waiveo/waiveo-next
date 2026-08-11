package playercontentcache

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// brsrun_test.go EXECUTES a routine out of the real shipped Program.brs.
//
// # Why this exists
//
// The rest of this package reads the player's source and asserts its STRUCTURE
// — does this function call that one, and on which side of another call. That
// instrument is right for "the trim is called on the success path" and wrong for
// "the trim actually reclaims space", and the difference is not academic: the
// content cache shipped with a one-token defect (`cache.keepPrev` assigned the
// PROTECTED set rather than this Lease's keys) that made both of its caps inert
// forever, and every structural guard passed. A reviewer proved the point by
// inserting `for each k in cache.entries: protectedKeys[k] = true` — a trim
// provably incapable of evicting anything — and the package still went green.
//
// A cache's bound is a claim about what happens after N polls. Nothing short of
// running it can check that, and BrightScript has no off-device runner, so this
// is one: a small interpreter for the subset of the language the cache routines
// are written in, running the SHIPPED file (never a fixture copy).
//
// # Why it is sound to trust
//
// It fails LOUDLY on anything it does not understand. Every unsupported
// construct, unknown identifier, unknown call and type mismatch panics with the
// source line, and the panic surfaces as a test failure — so the failure mode of
// a player edit this engine cannot model is a red test naming the line, never a
// green run over code that was silently skipped. The alternative failure
// direction (quietly approximating, and passing) is the one that produced the
// defect this file exists to catch.
//
// Only three things are stubbed, each because it is I/O rather than logic:
// `CreateObject("roFileSystem")` (a recorder, so a delete is observable),
// `wvSweepContentCacheDir` (a cold-start disk sweep, guarded structurally in
// cache_test.go), and `print`. Everything else — the trim, the caps, the cache
// constructor, the unlink — is the player's own code.

// ───────────────────────────────────────────────────────────── values

// assoc models an roAssociativeArray: case-INSENSITIVE keys (Roku's default
// mode) that remember the case they were first written with, iterated in
// insertion order. Case-insensitivity is modelled rather than ignored because
// the cache reads `cache.entries` with a dotted member and writes it with a
// bracket key, and a guard that disagreed with the device about key identity
// would be asserting on a cache the Roku does not have.
type assoc struct {
	keys []string          // display keys, insertion order
	vals map[string]any    // lower-cased key -> value
	disp map[string]string // lower-cased key -> display key
}

func newAssoc() *assoc {
	return &assoc{vals: map[string]any{}, disp: map[string]string{}}
}

func (a *assoc) get(k string) any  { return a.vals[strings.ToLower(k)] }
func (a *assoc) has(k string) bool { _, ok := a.vals[strings.ToLower(k)]; return ok }
func (a *assoc) count() int        { return len(a.keys) }
func (a *assoc) keyList() []string { return append([]string(nil), a.keys...) }
func (a *assoc) String() string    { return "roAssociativeArray(" + strings.Join(a.keys, ",") + ")" }

func (a *assoc) set(k string, v any) {
	lower := strings.ToLower(k)
	if _, ok := a.vals[lower]; !ok {
		a.keys = append(a.keys, k)
		a.disp[lower] = k
	}
	a.vals[lower] = v
}

func (a *assoc) del(k string) bool {
	lower := strings.ToLower(k)
	if _, ok := a.vals[lower]; !ok {
		return false
	}
	delete(a.vals, lower)
	display := a.disp[lower]
	delete(a.disp, lower)
	for i, existing := range a.keys {
		if existing == display {
			a.keys = append(a.keys[:i], a.keys[i+1:]...)
			break
		}
	}
	return true
}

// hostObj is a stand-in for a Roku component (only roFileSystem is needed).
type hostObj struct {
	name    string
	methods map[string]func(args []any) any
}

// brsPanic is a BrightScript-level runtime error — what `try` can catch, and
// what an unsupported construct raises so the test names the line.
type brsPanic struct {
	msg string
	ln  int
}

func (e *brsPanic) Error() string { return fmt.Sprintf("Program.brs:%d: %s", e.ln, e.msg) }

func fail(ln int, format string, args ...any) {
	panic(&brsPanic{msg: fmt.Sprintf(format, args...), ln: ln})
}

// ───────────────────────────────────────────────────────────── tokens

type tokKind int

const (
	tkIdent tokKind = iota
	tkNumber
	tkString
	tkOp
	tkNewline
	tkEOF
)

type token struct {
	kind tokKind
	text string
	ln   int
}

func (t token) isIdent(word string) bool {
	return t.kind == tkIdent && strings.EqualFold(t.text, word)
}

func (t token) isOp(op string) bool { return t.kind == tkOp && t.text == op }

var twoCharOps = []string{"<>", "<=", ">="}

// lex turns comment-stripped source lines into a token stream with explicit
// newline tokens (BrightScript is line-oriented) and a `:` treated as one more
// statement separator.
func lex(lines []line) []token {
	var out []token
	for _, l := range lines {
		s := l.text
		i := 0
		for i < len(s) {
			c := s[i]
			switch {
			case c == ' ' || c == '\t':
				i++
			case c == '"':
				j := i + 1
				var b strings.Builder
				for j < len(s) {
					if s[j] == '"' {
						if j+1 < len(s) && s[j+1] == '"' { // "" is an escaped quote
							b.WriteByte('"')
							j += 2
							continue
						}
						break
					}
					b.WriteByte(s[j])
					j++
				}
				out = append(out, token{kind: tkString, text: b.String(), ln: l.n})
				i = j + 1
			case isIdentStartByte(c):
				j := i
				for j < len(s) && isIdentByte(s[j]) {
					j++
				}
				out = append(out, token{kind: tkIdent, text: s[i:j], ln: l.n})
				i = j
			case c >= '0' && c <= '9':
				j := i
				for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == '.') {
					j++
				}
				out = append(out, token{kind: tkNumber, text: s[i:j], ln: l.n})
				i = j
			default:
				matched := false
				for _, op := range twoCharOps {
					if strings.HasPrefix(s[i:], op) {
						out = append(out, token{kind: tkOp, text: op, ln: l.n})
						i += 2
						matched = true
						break
					}
				}
				if !matched {
					out = append(out, token{kind: tkOp, text: string(c), ln: l.n})
					i++
				}
			}
		}
		out = append(out, token{kind: tkNewline, text: "\n", ln: l.n})
	}
	return append(out, token{kind: tkEOF, ln: 0})
}

func isIdentStartByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentByte(c byte) bool { return isIdentStartByte(c) || (c >= '0' && c <= '9') }

// ───────────────────────────────────────────────────────────── AST

type expr interface{}

type eLit struct{ v any }
type eVar struct {
	name string
	ln   int
}
type eMember struct {
	obj  expr
	name string
	ln   int
}
type eIndex struct {
	obj, idx expr
	ln       int
}
type eCall struct {
	callee expr
	args   []expr
	ln     int
}
type eUnary struct {
	op string
	x  expr
	ln int
}
type eBinary struct {
	op   string
	l, r expr
	ln   int
}
type eAssoc struct {
	keys []string
	vals []expr
	ln   int
}
type eArray struct {
	items []expr
	ln    int
}

type node interface{}

type sAssign struct {
	target expr
	val    expr
	ln     int
}
type sExpr struct {
	x  expr
	ln int
}
type sIf struct {
	arms []ifArm
	ln   int
}
type ifArm struct {
	cond expr // nil = else
	body []node
}
type sWhile struct {
	cond expr
	body []node
	ln   int
}
type sForEach struct {
	name string
	in   expr
	body []node
	ln   int
}
type sReturn struct {
	x  expr
	ln int
}
type sExit struct {
	what string // "while" | "for"
	ln   int
}
type sPrint struct {
	args []expr
	ln   int
}
type sTry struct {
	body    []node
	catchAs string
	catch   []node
	ln      int
}

// ───────────────────────────────────────────────────────────── parser

type parser struct {
	toks []token
	i    int
}

func (p *parser) cur() token  { return p.toks[p.i] }
func (p *parser) next() token { t := p.toks[p.i]; p.i++; return t }

func (p *parser) skipNewlines() {
	for p.cur().kind == tkNewline {
		p.i++
	}
}

func (p *parser) expectIdent(word string) {
	if !p.cur().isIdent(word) {
		fail(p.cur().ln, "expected %q, found %q", word, p.cur().text)
	}
	p.i++
}

func (p *parser) expectOp(op string) {
	if !p.cur().isOp(op) {
		fail(p.cur().ln, "expected %q, found %q", op, p.cur().text)
	}
	p.i++
}

func (p *parser) endOfStatement() {
	if p.cur().kind == tkNewline || p.cur().isOp(":") {
		p.i++
		return
	}
	if p.cur().kind == tkEOF {
		return
	}
	fail(p.cur().ln, "unexpected %q at the end of a statement", p.cur().text)
}

// atBlockEnd reports whether the cursor sits on a keyword that closes the
// enclosing block, without consuming it.
func (p *parser) atBlockEnd() bool {
	t := p.cur()
	if t.kind == tkEOF {
		return true
	}
	if t.kind != tkIdent {
		return false
	}
	switch strings.ToLower(t.text) {
	case "else", "elseif", "catch", "next":
		return true
	case "end":
		n := p.toks[p.i+1]
		if n.kind != tkIdent {
			return false
		}
		switch strings.ToLower(n.text) {
		case "if", "while", "for", "sub", "function", "try":
			return true
		}
	}
	return false
}

func (p *parser) block() []node {
	var out []node
	for {
		p.skipNewlines()
		if p.atBlockEnd() {
			return out
		}
		out = append(out, p.statement())
	}
}

func (p *parser) statement() node {
	t := p.cur()
	if t.kind == tkIdent {
		switch strings.ToLower(t.text) {
		case "if":
			return p.ifStatement()
		case "while":
			return p.whileStatement()
		case "for":
			return p.forStatement()
		case "return":
			p.i++
			s := &sReturn{ln: t.ln}
			if p.cur().kind != tkNewline && !p.cur().isOp(":") && p.cur().kind != tkEOF {
				s.x = p.expr()
			}
			p.endOfStatement()
			return s
		case "exit":
			p.i++
			what := strings.ToLower(p.next().text)
			if what != "while" && what != "for" {
				fail(t.ln, "unsupported `exit %s`", what)
			}
			p.endOfStatement()
			return &sExit{what: what, ln: t.ln}
		case "print":
			p.i++
			s := &sPrint{ln: t.ln}
			for p.cur().kind != tkNewline && !p.cur().isOp(":") && p.cur().kind != tkEOF {
				s.args = append(s.args, p.expr())
				if p.cur().isOp(";") || p.cur().isOp(",") {
					p.i++
				}
			}
			p.endOfStatement()
			return s
		case "try":
			return p.tryStatement()
		case "stop":
			fail(t.ln, "`stop` reached: the interpreter has no debugger to break into")
		}
	}

	// An assignment or a bare call.
	target := p.postfix()
	if p.cur().isOp("=") {
		p.i++
		val := p.expr()
		p.endOfStatement()
		return &sAssign{target: target, val: val, ln: t.ln}
	}
	if _, ok := target.(*eCall); !ok {
		fail(t.ln, "statement is neither an assignment nor a call (near %q)", t.text)
	}
	p.endOfStatement()
	return &sExpr{x: target, ln: t.ln}
}

func (p *parser) ifStatement() node {
	open := p.cur()
	p.expectIdent("if")
	s := &sIf{ln: open.ln}
	cond := p.expr()
	if p.cur().isIdent("then") {
		p.i++
	}
	// Single-line form: `if <cond> then <stmt> [else <stmt>]`.
	if p.cur().kind != tkNewline {
		body := []node{p.inlineStatement()}
		s.arms = append(s.arms, ifArm{cond: cond, body: body})
		if p.cur().isIdent("else") {
			p.i++
			s.arms = append(s.arms, ifArm{body: []node{p.inlineStatement()}})
		}
		p.endOfStatement()
		return s
	}

	for {
		body := p.block()
		s.arms = append(s.arms, ifArm{cond: cond, body: body})
		t := p.cur()
		switch {
		case t.isIdent("end"):
			p.expectIdent("end")
			p.expectIdent("if")
			return s
		case t.isIdent("else") && p.toks[p.i+1].isIdent("if"):
			p.i += 2
			cond = p.expr()
			if p.cur().isIdent("then") {
				p.i++
			}
		case t.isIdent("elseif"):
			p.i++
			cond = p.expr()
			if p.cur().isIdent("then") {
				p.i++
			}
		case t.isIdent("else"):
			p.i++
			body := p.block()
			s.arms = append(s.arms, ifArm{body: body})
			p.expectIdent("end")
			p.expectIdent("if")
			return s
		default:
			fail(t.ln, "unexpected %q inside the `if` opened at line %d", t.text, open.ln)
		}
	}
}

// inlineStatement parses the single statement that follows `then` (or `else`) on
// one line. It must not consume the trailing newline, which the caller owns.
func (p *parser) inlineStatement() node {
	t := p.cur()
	if t.kind == tkIdent {
		switch strings.ToLower(t.text) {
		case "return":
			p.i++
			s := &sReturn{ln: t.ln}
			if p.cur().kind != tkNewline && !p.cur().isIdent("else") && p.cur().kind != tkEOF {
				s.x = p.expr()
			}
			return s
		case "exit":
			p.i++
			what := strings.ToLower(p.next().text)
			return &sExit{what: what, ln: t.ln}
		case "print":
			p.i++
			s := &sPrint{ln: t.ln}
			for p.cur().kind != tkNewline && !p.cur().isIdent("else") && p.cur().kind != tkEOF {
				s.args = append(s.args, p.expr())
				if p.cur().isOp(";") || p.cur().isOp(",") {
					p.i++
				}
			}
			return s
		}
	}
	target := p.postfix()
	if p.cur().isOp("=") {
		p.i++
		return &sAssign{target: target, val: p.expr(), ln: t.ln}
	}
	if _, ok := target.(*eCall); !ok {
		fail(t.ln, "inline statement is neither an assignment nor a call (near %q)", t.text)
	}
	return &sExpr{x: target, ln: t.ln}
}

func (p *parser) whileStatement() node {
	open := p.cur()
	p.expectIdent("while")
	cond := p.expr()
	p.endOfStatement()
	body := p.block()
	p.expectIdent("end")
	p.expectIdent("while")
	return &sWhile{cond: cond, body: body, ln: open.ln}
}

func (p *parser) forStatement() node {
	open := p.cur()
	p.expectIdent("for")
	if !p.cur().isIdent("each") {
		fail(open.ln, "only `for each` is supported; this engine models the cache routines, not a counted loop")
	}
	p.i++
	name := p.next()
	if name.kind != tkIdent {
		fail(open.ln, "`for each` needs a loop variable, found %q", name.text)
	}
	p.expectIdent("in")
	in := p.expr()
	p.endOfStatement()
	body := p.block()
	if p.cur().isIdent("next") {
		p.i++
	} else {
		p.expectIdent("end")
		p.expectIdent("for")
	}
	return &sForEach{name: strings.ToLower(name.text), in: in, body: body, ln: open.ln}
}

func (p *parser) tryStatement() node {
	open := p.cur()
	p.expectIdent("try")
	p.endOfStatement()
	body := p.block()
	p.expectIdent("catch")
	as := p.next()
	if as.kind != tkIdent {
		fail(open.ln, "`catch` needs a variable, found %q", as.text)
	}
	p.endOfStatement()
	catch := p.block()
	p.expectIdent("end")
	p.expectIdent("try")
	return &sTry{body: body, catchAs: strings.ToLower(as.text), catch: catch, ln: open.ln}
}

// Expression precedence: or < and < not < comparison < +- < */ < unary < postfix.
func (p *parser) expr() expr { return p.parseOr() }

func (p *parser) parseOr() expr {
	l := p.parseAnd()
	for p.cur().isIdent("or") {
		ln := p.next().ln
		l = &eBinary{op: "or", l: l, r: p.parseAnd(), ln: ln}
	}
	return l
}

func (p *parser) parseAnd() expr {
	l := p.parseNot()
	for p.cur().isIdent("and") {
		ln := p.next().ln
		l = &eBinary{op: "and", l: l, r: p.parseNot(), ln: ln}
	}
	return l
}

func (p *parser) parseNot() expr {
	if p.cur().isIdent("not") {
		ln := p.next().ln
		return &eUnary{op: "not", x: p.parseNot(), ln: ln}
	}
	return p.parseCompare()
}

func (p *parser) parseCompare() expr {
	l := p.parseAdd()
	for {
		t := p.cur()
		if t.kind != tkOp {
			return l
		}
		switch t.text {
		case "=", "<>", "<", ">", "<=", ">=":
			p.i++
			l = &eBinary{op: t.text, l: l, r: p.parseAdd(), ln: t.ln}
		default:
			return l
		}
	}
}

func (p *parser) parseAdd() expr {
	l := p.parseMul()
	for {
		t := p.cur()
		if t.kind == tkOp && (t.text == "+" || t.text == "-") {
			p.i++
			l = &eBinary{op: t.text, l: l, r: p.parseMul(), ln: t.ln}
			continue
		}
		return l
	}
}

func (p *parser) parseMul() expr {
	l := p.parseUnary()
	for {
		t := p.cur()
		if t.kind == tkOp && (t.text == "*" || t.text == "/") {
			p.i++
			l = &eBinary{op: t.text, l: l, r: p.parseUnary(), ln: t.ln}
			continue
		}
		return l
	}
}

func (p *parser) parseUnary() expr {
	if p.cur().isOp("-") {
		t := p.next()
		return &eUnary{op: "-", x: p.parseUnary(), ln: t.ln}
	}
	return p.postfix()
}

func (p *parser) postfix() expr {
	x := p.primary()
	for {
		t := p.cur()
		switch {
		case t.isOp("."):
			p.i++
			name := p.next()
			if name.kind != tkIdent {
				fail(t.ln, "expected a member name after `.`, found %q", name.text)
			}
			x = &eMember{obj: x, name: name.text, ln: t.ln}
		case t.isOp("["):
			p.i++
			idx := p.expr()
			p.expectOp("]")
			x = &eIndex{obj: x, idx: idx, ln: t.ln}
		case t.isOp("("):
			p.i++
			call := &eCall{callee: x, ln: t.ln}
			for !p.cur().isOp(")") {
				call.args = append(call.args, p.expr())
				if p.cur().isOp(",") {
					p.i++
				}
			}
			p.expectOp(")")
			x = call
		default:
			return x
		}
	}
}

func (p *parser) primary() expr {
	t := p.next()
	switch t.kind {
	case tkNumber:
		if strings.Contains(t.text, ".") {
			f, err := strconv.ParseFloat(t.text, 64)
			if err != nil {
				fail(t.ln, "bad number %q", t.text)
			}
			return &eLit{v: f}
		}
		n, err := strconv.Atoi(t.text)
		if err != nil {
			fail(t.ln, "bad number %q", t.text)
		}
		return &eLit{v: n}
	case tkString:
		return &eLit{v: t.text}
	case tkIdent:
		switch strings.ToLower(t.text) {
		case "true":
			return &eLit{v: true}
		case "false":
			return &eLit{v: false}
		case "invalid":
			return &eLit{v: nil}
		}
		return &eVar{name: strings.ToLower(t.text), ln: t.ln}
	case tkOp:
		switch t.text {
		case "(":
			x := p.expr()
			p.expectOp(")")
			return x
		case "{":
			return p.assocLiteral(t.ln)
		case "[":
			a := &eArray{ln: t.ln}
			for {
				p.skipNewlines()
				if p.cur().isOp("]") {
					p.i++
					return a
				}
				a.items = append(a.items, p.expr())
				p.skipNewlines()
				if p.cur().isOp(",") {
					p.i++
				}
			}
		}
	}
	fail(t.ln, "cannot read an expression starting at %q", t.text)
	return nil
}

func (p *parser) assocLiteral(ln int) expr {
	a := &eAssoc{ln: ln}
	for {
		p.skipNewlines()
		if p.cur().isOp("}") {
			p.i++
			return a
		}
		key := p.next()
		if key.kind != tkIdent && key.kind != tkString {
			fail(key.ln, "an associative-array key must be a name or a string, found %q", key.text)
		}
		p.expectOp(":")
		a.keys = append(a.keys, key.text)
		a.vals = append(a.vals, p.expr())
		p.skipNewlines()
		if p.cur().isOp(",") {
			p.i++
		}
	}
}

// ───────────────────────────────────────────────────────────── interpreter

type routine struct {
	name   string
	params []string
	start  int // token index of the body's first token
	end    int // token index of the closing `end sub`/`end function`
	body   []node
	parsed bool
}

type interp struct {
	t        *testing.T
	toks     []token
	routines map[string]*routine
	// m is the Task thread's own scope, where wvContentCache keeps its state.
	m *assoc
	// deleted records every path handed to roFileSystem.Delete, in order — the
	// observable proof that an eviction reclaimed bytes rather than merely
	// forgetting an entry.
	deleted []string
	printed []string
	stubs   map[string]func(args []any) any
	steps   int
}

const stepBudget = 2_000_000

// newInterp loads a BrightScript file and indexes its routines. Bodies are
// parsed lazily, so a routine this engine could not read costs nothing until
// something calls it.
func newInterp(t *testing.T, path string) *interp {
	t.Helper()
	in := &interp{
		t:        t,
		toks:     lex(readBrs(t, path)),
		routines: map[string]*routine{},
		m:        newAssoc(),
	}
	in.stubs = map[string]func(args []any) any{
		// The only Roku component the cache routines construct. The recorder is
		// what makes "the bytes were reclaimed" assertable.
		"createobject": func(args []any) any {
			name, _ := args[0].(string)
			if name != "roFileSystem" {
				fail(0, "the engine only models CreateObject(\"roFileSystem\"), not %q", name)
			}
			return &hostObj{name: name, methods: map[string]func([]any) any{
				"delete": func(a []any) any {
					p, _ := a[0].(string)
					in.deleted = append(in.deleted, p)
					return true
				},
			}}
		},
		// A cold-start disk sweep: I/O, not logic, and guarded structurally by
		// TestTheCacheSweepsStaleFilesAtStartup.
		"wvsweepcontentcachedir": func([]any) any { return nil },
	}
	in.index()
	return in
}

// index finds every `function`/`sub` header and the token range of its body.
func (in *interp) index() {
	atLineStart := true
	for i := 0; i < len(in.toks); i++ {
		t := in.toks[i]
		if t.kind == tkNewline {
			atLineStart = true
			continue
		}
		if !atLineStart {
			continue
		}
		atLineStart = false
		if t.kind != tkIdent {
			continue
		}
		kw := strings.ToLower(t.text)
		if kw != "function" && kw != "sub" {
			continue
		}
		name := in.toks[i+1]
		if name.kind != tkIdent {
			continue
		}
		r := &routine{name: strings.ToLower(name.text)}
		j := i + 2
		if !in.toks[j].isOp("(") {
			continue
		}
		j++
		for !in.toks[j].isOp(")") {
			if in.toks[j].kind == tkIdent && !in.toks[j].isIdent("as") {
				if len(r.params) == 0 || in.toks[j-1].isOp(",") {
					r.params = append(r.params, strings.ToLower(in.toks[j].text))
				}
			}
			j++
		}
		j++
		for in.toks[j].kind != tkNewline {
			j++ // `as Object` return annotation
		}
		r.start = j + 1
		closer := "sub"
		if kw == "function" {
			closer = "function"
		}
		depth := 0
		for k := r.start; k < len(in.toks); k++ {
			if in.toks[k].isIdent("end") && in.toks[k+1].isIdent(closer) && depth == 0 {
				r.end = k
				break
			}
		}
		if r.end == 0 {
			continue
		}
		in.routines[r.name] = r
		i = r.end
	}
}

func (in *interp) body(r *routine) []node {
	if !r.parsed {
		p := &parser{toks: append(append([]token(nil), in.toks[r.start:r.end]...), token{kind: tkEOF})}
		r.body = p.block()
		r.parsed = true
	}
	return r.body
}

// call runs a routine from the shipped source. A BrightScript-level error, or
// anything the engine cannot model, fails the test with the source line.
func (in *interp) call(name string, args ...any) (result any) {
	in.t.Helper()
	defer func() {
		if rec := recover(); rec != nil {
			if e, ok := rec.(*brsPanic); ok {
				in.t.Fatalf("executing %s: %v", name, e)
			}
			panic(rec)
		}
	}()
	return in.invoke(name, args, 0)
}

func (in *interp) invoke(name string, args []any, ln int) any {
	lower := strings.ToLower(name)
	if stub, ok := in.stubs[lower]; ok {
		return stub(args)
	}
	r, ok := in.routines[lower]
	if !ok {
		fail(ln, "call to %s, which this file does not define and the engine does not stub", name)
	}
	scope := map[string]any{}
	for i, p := range r.params {
		if i < len(args) {
			scope[p] = args[i]
		} else {
			scope[p] = nil
		}
	}
	f := in.execBlock(in.body(r), scope)
	if f.c == ctrlReturn {
		return f.v
	}
	return nil
}

type ctrl int

const (
	ctrlNone ctrl = iota
	ctrlReturn
	ctrlExitWhile
	ctrlExitFor
)

type flow struct {
	c ctrl
	v any
}

func (in *interp) execBlock(stmts []node, scope map[string]any) flow {
	for _, s := range stmts {
		if f := in.exec(s, scope); f.c != ctrlNone {
			return f
		}
	}
	return flow{}
}

func (in *interp) exec(s node, scope map[string]any) flow {
	in.steps++
	if in.steps > stepBudget {
		fail(0, "the interpreter ran %d statements without finishing — the routine under test does not terminate", stepBudget)
	}
	switch n := s.(type) {
	case *sAssign:
		in.assign(n.target, in.eval(n.val, scope), scope)
		return flow{}
	case *sExpr:
		in.eval(n.x, scope)
		return flow{}
	case *sReturn:
		var v any
		if n.x != nil {
			v = in.eval(n.x, scope)
		}
		return flow{c: ctrlReturn, v: v}
	case *sExit:
		if n.what == "while" {
			return flow{c: ctrlExitWhile}
		}
		return flow{c: ctrlExitFor}
	case *sPrint:
		var b strings.Builder
		for _, a := range n.args {
			b.WriteString(toStr(in.eval(a, scope), n.ln))
		}
		in.printed = append(in.printed, b.String())
		return flow{}
	case *sIf:
		for _, arm := range n.arms {
			if arm.cond == nil || truthy(in.eval(arm.cond, scope), n.ln) {
				return in.execBlock(arm.body, scope)
			}
		}
		return flow{}
	case *sWhile:
		for truthy(in.eval(n.cond, scope), n.ln) {
			f := in.execBlock(n.body, scope)
			switch f.c {
			case ctrlExitWhile:
				return flow{}
			case ctrlReturn:
				return f
			}
			in.steps++
			if in.steps > stepBudget {
				fail(n.ln, "this `while` has run %d iterations without exiting", stepBudget)
			}
		}
		return flow{}
	case *sForEach:
		for _, item := range in.iterable(in.eval(n.in, scope), n.ln) {
			scope[n.name] = item
			f := in.execBlock(n.body, scope)
			switch f.c {
			case ctrlExitFor:
				return flow{}
			case ctrlReturn:
				return f
			}
		}
		return flow{}
	case *sTry:
		return in.execTry(n, scope)
	}
	fail(0, "unsupported statement %T", s)
	return flow{}
}

func (in *interp) execTry(n *sTry, scope map[string]any) (out flow) {
	caught := false
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				if e, ok := rec.(*brsPanic); ok {
					caught = true
					scope[n.catchAs] = e.msg
					return
				}
				panic(rec)
			}
		}()
		out = in.execBlock(n.body, scope)
	}()
	if caught {
		return in.execBlock(n.catch, scope)
	}
	return out
}

// iterable is what `for each` walks: an assoc yields its KEYS (a snapshot, so a
// body that mutates the map cannot corrupt the walk), an array its elements.
func (in *interp) iterable(v any, ln int) []any {
	switch x := v.(type) {
	case *assoc:
		keys := x.keyList()
		out := make([]any, 0, len(keys))
		for _, k := range keys {
			out = append(out, k)
		}
		return out
	case []any:
		return append([]any(nil), x...)
	case nil:
		fail(ln, "`for each` over invalid")
	}
	fail(ln, "`for each` over a %T, which is not iterable", v)
	return nil
}

func (in *interp) assign(target expr, v any, scope map[string]any) {
	switch t := target.(type) {
	case *eVar:
		scope[t.name] = v
	case *eMember:
		obj := in.eval(t.obj, scope)
		a, ok := obj.(*assoc)
		if !ok {
			fail(t.ln, "cannot set .%s on a %T", t.name, obj)
		}
		a.set(t.name, v)
	case *eIndex:
		obj := in.eval(t.obj, scope)
		a, ok := obj.(*assoc)
		if !ok {
			fail(t.ln, "cannot set an index on a %T", obj)
		}
		a.set(toStr(in.eval(t.idx, scope), t.ln), v)
	default:
		fail(0, "cannot assign to a %T", target)
	}
}

func (in *interp) eval(e expr, scope map[string]any) any {
	switch x := e.(type) {
	case *eLit:
		return x.v
	case *eVar:
		if x.name == "m" {
			return in.m
		}
		v, ok := scope[x.name]
		if !ok {
			fail(x.ln, "variable %q is not defined at this point", x.name)
		}
		return v
	case *eMember:
		obj := in.eval(x.obj, scope)
		a, ok := obj.(*assoc)
		if !ok {
			fail(x.ln, "cannot read .%s from a %T", x.name, obj)
		}
		return a.get(x.name)
	case *eIndex:
		obj := in.eval(x.obj, scope)
		key := in.eval(x.idx, scope)
		switch c := obj.(type) {
		case *assoc:
			return c.get(toStr(key, x.ln))
		case []any:
			i, ok := key.(int)
			if !ok || i < 0 || i >= len(c) {
				return nil
			}
			return c[i]
		}
		fail(x.ln, "cannot index a %T", obj)
	case *eAssoc:
		a := newAssoc()
		for i, k := range x.keys {
			a.set(k, in.eval(x.vals[i], scope))
		}
		return a
	case *eArray:
		out := make([]any, 0, len(x.items))
		for _, it := range x.items {
			out = append(out, in.eval(it, scope))
		}
		return out
	case *eUnary:
		return in.evalUnary(x, scope)
	case *eBinary:
		return in.evalBinary(x, scope)
	case *eCall:
		return in.evalCall(x, scope)
	}
	fail(0, "unsupported expression %T", e)
	return nil
}

func (in *interp) evalUnary(x *eUnary, scope map[string]any) any {
	v := in.eval(x.x, scope)
	switch x.op {
	case "not":
		return !truthy(v, x.ln)
	case "-":
		switch n := v.(type) {
		case int:
			return -n
		case float64:
			return -n
		}
		fail(x.ln, "cannot negate a %T", v)
	}
	return nil
}

func (in *interp) evalBinary(x *eBinary, scope map[string]any) any {
	// `and`/`or` are evaluated left-first; BrightScript does not short-circuit,
	// but nothing here depends on the difference and evaluating both is the
	// conservative choice (a side effect in the right operand still happens).
	l := in.eval(x.l, scope)
	r := in.eval(x.r, scope)
	switch x.op {
	case "and":
		return truthy(l, x.ln) && truthy(r, x.ln)
	case "or":
		return truthy(l, x.ln) || truthy(r, x.ln)
	case "=":
		return equal(l, r)
	case "<>":
		return !equal(l, r)
	}
	if ls, ok := l.(string); ok {
		rs, ok2 := r.(string)
		if !ok2 {
			fail(x.ln, "cannot apply %q to a string and a %T", x.op, r)
		}
		switch x.op {
		case "+":
			return ls + rs
		case "<":
			return ls < rs
		case ">":
			return ls > rs
		case "<=":
			return ls <= rs
		case ">=":
			return ls >= rs
		}
		fail(x.ln, "cannot apply %q to strings", x.op)
	}
	lf, lok := asFloat(l)
	rf, rok := asFloat(r)
	if !lok || !rok {
		fail(x.ln, "cannot apply %q to a %T and a %T", x.op, l, r)
	}
	_, li := l.(int)
	_, ri := r.(int)
	bothInt := li && ri
	switch x.op {
	case "+", "-", "*":
		var f float64
		switch x.op {
		case "+":
			f = lf + rf
		case "-":
			f = lf - rf
		case "*":
			f = lf * rf
		}
		if bothInt {
			return int(f)
		}
		return f
	case "/":
		if rf == 0 {
			fail(x.ln, "divide by zero")
		}
		return lf / rf
	case "<":
		return lf < rf
	case ">":
		return lf > rf
	case "<=":
		return lf <= rf
	case ">=":
		return lf >= rf
	}
	fail(x.ln, "unsupported operator %q", x.op)
	return nil
}

func (in *interp) evalCall(x *eCall, scope map[string]any) any {
	args := make([]any, 0, len(x.args))
	for _, a := range x.args {
		args = append(args, in.eval(a, scope))
	}
	switch callee := x.callee.(type) {
	case *eVar:
		return in.invoke(callee.name, args, x.ln)
	case *eMember:
		recv := in.eval(callee.obj, scope)
		return in.method(recv, callee.name, args, x.ln)
	}
	fail(x.ln, "cannot call a %T", x.callee)
	return nil
}

// method implements the handful of BrightScript intrinsics the cache routines
// use. An unknown one FAILS rather than returning invalid: a silently-ignored
// `.Delete()` is precisely the class of defect this engine exists to catch.
func (in *interp) method(recv any, name string, args []any, ln int) any {
	switch r := recv.(type) {
	case *assoc:
		switch strings.ToLower(name) {
		case "delete":
			return r.del(toStr(args[0], ln))
		case "count":
			return r.count()
		case "doesexist":
			return r.has(toStr(args[0], ln))
		case "lookup":
			return r.get(toStr(args[0], ln))
		case "clear":
			*r = *newAssoc()
			return nil
		case "keys":
			out := make([]any, 0, r.count())
			for _, k := range r.keyList() {
				out = append(out, k)
			}
			return out
		}
	case *hostObj:
		if fn, ok := r.methods[strings.ToLower(name)]; ok {
			return fn(args)
		}
		fail(ln, "%s has no %s() in this engine", r.name, name)
	case string:
		switch strings.ToLower(name) {
		case "instr":
			return strings.Index(r, toStr(args[0], ln))
		case "len":
			return len(r)
		case "trim":
			return strings.TrimSpace(r)
		}
	case int, float64, bool:
		if strings.EqualFold(name, "tostr") {
			return toStr(recv, ln)
		}
	case []any:
		switch strings.ToLower(name) {
		case "count":
			return len(r)
		case "push":
			fail(ln, "Push() on an array is not modelled (arrays are read-only here)")
		}
	}
	fail(ln, "no method %s() on a %T", name, recv)
	return nil
}

func truthy(v any, ln int) bool {
	switch b := v.(type) {
	case bool:
		return b
	case nil:
		return false
	case int:
		return b != 0
	}
	fail(ln, "a %T is not a condition", v)
	return false
}

func equal(l, r any) bool {
	if l == nil || r == nil {
		return l == nil && r == nil
	}
	if lf, ok := asFloat(l); ok {
		if rf, ok2 := asFloat(r); ok2 {
			return lf == rf
		}
		return false
	}
	return l == r
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func toStr(v any, ln int) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case nil:
		fail(ln, "invalid used where a string is needed")
	}
	fail(ln, "a %T has no string form", v)
	return ""
}

// ───────────────────────────────────────── the engine's own tests

// The engine is a test dependency, so it gets tests of its own: an engine that
// silently mis-evaluated a condition would report a bounded cache as
// convincingly as it reports an unbounded one.
func TestTheEngineExecutesTheConstructsTheCacheIsWrittenIn(t *testing.T) {
	in := newInterp(t, programPath)

	// Reading the shipped caps proves the whole path — index the file, find the
	// routine, parse its body, evaluate an expression, return a value.
	if got := in.call("wvContentCacheMaxEntries"); got != 16 {
		t.Errorf("wvContentCacheMaxEntries() = %v, want 16", got)
	}
	if got := in.call("wvContentCacheMaxBytes"); got != 96*1024*1024 {
		t.Errorf("wvContentCacheMaxBytes() = %v, want %d", got, 96*1024*1024)
	}

	// The cache constructor: an assoc literal, a member write onto `m`, a
	// stubbed sweep, and the invalid-check that makes it construct once.
	cache := in.call("wvContentCache")
	if _, ok := cache.(*assoc); !ok {
		t.Fatalf("wvContentCache() returned %T, want an associative array", cache)
	}
	if again := in.call("wvContentCache"); again != cache {
		t.Error("wvContentCache() built a second cache: the `m` state is not persisting between calls, so nothing below would be testing one cache")
	}

	// The unlink really reaches a filesystem object.
	in.call("wvDeleteCachedFile", "cachefs:/wv_probe.bin")
	if len(in.deleted) != 1 || in.deleted[0] != "cachefs:/wv_probe.bin" {
		t.Errorf("wvDeleteCachedFile did not reach roFileSystem.Delete: recorded %v", in.deleted)
	}
}
