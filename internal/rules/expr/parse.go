package expr

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Parser-surfaced failure codes. CodeUnknownFilter maps directly to the
// rules/1 UNKNOWN_FILTER taxonomy code (contracts/rules-1.md Error taxonomy);
// the compile front-end maps the others. CodeInvalidSource covers a source
// that is not state/attr/now (RUL-281/292); CodeMalformedExpr covers any other
// shape/syntax violation of the Expression grammar (RUL-280).
const (
	CodeUnknownFilter = "UNKNOWN_FILTER"
	CodeInvalidSource = "INVALID_SOURCE"
	CodeMalformedExpr = "MALFORMED_EXPRESSION"
)

// ParseError is a typed Expression-grammar parse failure. Code is one of the
// Code* constants above; Message is human-readable. The compile front-end wraps
// it into a CompileError with a JSON field path (RUL-006 addressing).
type ParseError struct {
	Code    string
	Message string
}

func (e *ParseError) Error() string { return e.Code + ": " + e.Message }

// sourceNames are the only names permitted in a pipeline's source position
// (RUL-281/292).
var sourceNames = map[string]bool{"state": true, "attr": true, "now": true}

// filterNames is the RUL-290 closed filter list minus the three source-only
// names (state/attr/now, RUL-292) — i.e. exactly the names permitted in a
// non-source filter stage. Any other name in a filter position is
// UNKNOWN_FILTER.
var filterNames = map[string]bool{
	"default":  true,
	"upper":    true,
	"lower":    true,
	"trim":     true,
	"round":    true,
	"abs":      true,
	"int":      true,
	"float":    true,
	"elapsed":  true,
	"duration": true,
	"convert":  true,
}

// Parse turns a raw rules/1 Expression (RUL-280) into an Expr: a JSON literal
// (string/number/boolean/null, used as-is) or an {"expr": "<pipeline>"} object
// whose pipeline string is parsed into a source + filter AST. It surfaces
// UNKNOWN_FILTER for a filter outside RUL-290, INVALID_SOURCE for a source
// outside state/attr/now (RUL-281/292), and MALFORMED_EXPRESSION otherwise.
func Parse(raw json.RawMessage) (Expr, *ParseError) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return Expr{}, &ParseError{CodeMalformedExpr, "empty Expression"}
	}
	switch trimmed[0] {
	case '{':
		return parseObject(trimmed)
	case '[':
		return Expr{}, &ParseError{CodeMalformedExpr, "an array is not a valid Expression literal (RUL-280)"}
	default:
		var v any
		if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
			return Expr{}, &ParseError{CodeMalformedExpr, "not a valid JSON literal: " + err.Error()}
		}
		// json.Unmarshal into any yields string/float64/bool/nil for the four
		// literal forms RUL-280 admits; '{' and '[' are handled above.
		return Expr{Literal: &v}, nil
	}
}

// parseObject handles the {"expr": "<pipeline>"} form (RUL-280): exactly one
// key, "expr", whose value is the pipeline string.
func parseObject(s string) (Expr, *ParseError) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return Expr{}, &ParseError{CodeMalformedExpr, "not a valid JSON object: " + err.Error()}
	}
	rawExpr, ok := obj["expr"]
	if !ok || len(obj) != 1 {
		return Expr{}, &ParseError{CodeMalformedExpr, `an Expression object must have exactly the key "expr" (RUL-280)`}
	}
	var pipeStr string
	if err := json.Unmarshal(rawExpr, &pipeStr); err != nil {
		return Expr{}, &ParseError{CodeMalformedExpr, `"expr" must be a pipeline string (RUL-280)`}
	}
	pl, perr := parsePipeline(pipeStr)
	if perr != nil {
		return Expr{}, perr
	}
	return Expr{Pipeline: pl}, nil
}

// parsePipeline parses `source (| filter(args...))*` (RUL-280): the first stage
// is the source (must be state/attr/now), each later stage a non-source filter
// from the RUL-290 list.
func parsePipeline(s string) (*Pipeline, *ParseError) {
	toks, perr := lex(s)
	if perr != nil {
		return nil, perr
	}
	p := &parser{toks: toks}

	src, perr := p.parseStage()
	if perr != nil {
		return nil, perr
	}
	if !sourceNames[src.Name] {
		return nil, &ParseError{CodeInvalidSource,
			fmt.Sprintf("a pipeline source must be state, attr, or now, not %q (RUL-281/292)", src.Name)}
	}

	pl := &Pipeline{Source: src}
	for p.peek().kind == tPipe {
		p.next() // consume '|'
		st, perr := p.parseStage()
		if perr != nil {
			return nil, perr
		}
		if sourceNames[st.Name] {
			return nil, &ParseError{CodeInvalidSource,
				fmt.Sprintf("%q may appear only as a pipeline source, not as a filter stage (RUL-292)", st.Name)}
		}
		if !filterNames[st.Name] {
			return nil, &ParseError{CodeUnknownFilter,
				fmt.Sprintf("%q is not a filter in the closed list (RUL-290)", st.Name)}
		}
		pl.Filters = append(pl.Filters, st)
	}
	if p.peek().kind != tEOF {
		return nil, &ParseError{CodeMalformedExpr, "unexpected trailing tokens after pipeline"}
	}
	return pl, nil
}

// parseStage parses one stage: an identifier, optionally followed by a
// parenthesized comma-separated literal argument list.
func (p *parser) parseStage() (Call, *ParseError) {
	t := p.next()
	if t.kind != tIdent {
		return Call{}, &ParseError{CodeMalformedExpr, "expected a source/filter name"}
	}
	if isLiteralKeyword(t.text) {
		return Call{}, &ParseError{CodeMalformedExpr, fmt.Sprintf("a literal (%s) cannot be a pipeline stage", t.text)}
	}
	call := Call{Name: t.text}
	if p.peek().kind != tLParen {
		return call, nil // bare stage, e.g. `upper`
	}
	p.next() // consume '('
	if p.peek().kind != tRParen {
		for {
			a, perr := p.parseArg()
			if perr != nil {
				return Call{}, perr
			}
			call.Args = append(call.Args, a)
			if p.peek().kind == tComma {
				p.next()
				continue
			}
			break
		}
	}
	if p.next().kind != tRParen {
		return Call{}, &ParseError{CodeMalformedExpr, fmt.Sprintf("expected ) closing %s(...)", call.Name)}
	}
	return call, nil
}

// parseArg parses one literal argument (RUL-281): a string, number, bool, or
// null. Identifiers other than true/false/null are rejected — args are literals.
func (p *parser) parseArg() (Arg, *ParseError) {
	t := p.next()
	switch t.kind {
	case tString, tNumber:
		return Arg{Value: t.val}, nil
	case tIdent:
		switch t.text {
		case "true":
			return Arg{Value: true}, nil
		case "false":
			return Arg{Value: false}, nil
		case "null":
			return Arg{Value: nil}, nil
		}
		return Arg{}, &ParseError{CodeMalformedExpr, fmt.Sprintf("argument must be a literal, got identifier %q", t.text)}
	default:
		return Arg{}, &ParseError{CodeMalformedExpr, "expected a literal argument"}
	}
}

func isLiteralKeyword(s string) bool {
	return s == "true" || s == "false" || s == "null"
}

// ---- lexer ----

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tString
	tNumber
	tLParen
	tRParen
	tComma
	tPipe
)

type token struct {
	kind tokKind
	text string // identifier text (tIdent)
	val  any    // decoded literal value (tString -> string, tNumber -> float64)
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func lex(s string) ([]token, *ParseError) {
	var toks []token
	n := len(s)
	for i := 0; i < n; {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '|':
			toks = append(toks, token{kind: tPipe})
			i++
		case c == '(':
			toks = append(toks, token{kind: tLParen})
			i++
		case c == ')':
			toks = append(toks, token{kind: tRParen})
			i++
		case c == ',':
			toks = append(toks, token{kind: tComma})
			i++
		case c == '\'' || c == '"':
			j := i + 1
			for j < n && s[j] != c {
				j++
			}
			if j >= n {
				return nil, &ParseError{CodeMalformedExpr, "unterminated string literal"}
			}
			toks = append(toks, token{kind: tString, val: s[i+1 : j]})
			i = j + 1
		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentPart(s[j]) {
				j++
			}
			toks = append(toks, token{kind: tIdent, text: s[i:j]})
			i = j
		case c == '-' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < n && isNumPart(s[j]) {
				j++
			}
			numText := s[i:j]
			var v any
			if err := json.Unmarshal([]byte(numText), &v); err != nil {
				return nil, &ParseError{CodeMalformedExpr, "invalid number literal " + numText}
			}
			if _, ok := v.(float64); !ok {
				return nil, &ParseError{CodeMalformedExpr, "invalid number literal " + numText}
			}
			toks = append(toks, token{kind: tNumber, val: v})
			i = j
		default:
			return nil, &ParseError{CodeMalformedExpr, fmt.Sprintf("unexpected character %q", string(c))}
		}
	}
	toks = append(toks, token{kind: tEOF})
	return toks, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func isNumPart(c byte) bool {
	return (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-'
}
