package expr

import (
	"encoding/json"
	"testing"
)

func mustParse(t *testing.T, raw string) Expr {
	t.Helper()
	e, perr := Parse(json.RawMessage(raw))
	if perr != nil {
		t.Fatalf("Parse(%s) unexpected error: %v", raw, perr)
	}
	return e
}

func TestParseLiteralString(t *testing.T) {
	e := mustParse(t, `"hello"`)
	if e.Pipeline != nil {
		t.Fatalf("expected literal, got pipeline")
	}
	if e.Literal == nil {
		t.Fatalf("expected non-nil Literal")
	}
	if got := *e.Literal; got != "hello" {
		t.Fatalf("literal = %v (%T), want \"hello\"", got, got)
	}
}

func TestParseLiteralNumber(t *testing.T) {
	e := mustParse(t, `42`)
	if e.Literal == nil {
		t.Fatalf("expected non-nil Literal")
	}
	if got := *e.Literal; got != float64(42) {
		t.Fatalf("literal = %v (%T), want float64(42)", got, got)
	}
}

func TestParseLiteralBool(t *testing.T) {
	e := mustParse(t, `true`)
	if e.Literal == nil {
		t.Fatalf("expected non-nil Literal")
	}
	if got := *e.Literal; got != true {
		t.Fatalf("literal = %v (%T), want true", got, got)
	}
}

func TestParseLiteralNull(t *testing.T) {
	// A null literal is a non-nil Literal pointing to a nil value (RUL-280):
	// Literal != nil, not the pointee, distinguishes the literal form.
	e := mustParse(t, `null`)
	if e.Pipeline != nil {
		t.Fatalf("expected literal, got pipeline")
	}
	if e.Literal == nil {
		t.Fatalf("expected non-nil Literal for a null literal")
	}
	if got := *e.Literal; got != nil {
		t.Fatalf("literal = %v (%T), want nil", got, got)
	}
}

func TestParseArrayRejected(t *testing.T) {
	// RUL-280: a literal is string/number/boolean/null only — never an array.
	_, perr := Parse(json.RawMessage(`[1,2]`))
	if perr == nil {
		t.Fatalf("expected error parsing an array literal")
	}
}

func TestParsePipelineStateUpper(t *testing.T) {
	const id = "01J8Z3K4N5P6Q7R8S9T0V1W2Z2"
	e := mustParse(t, `{"expr":"state('`+id+`') | upper"}`)
	if e.Literal != nil {
		t.Fatalf("expected pipeline, got literal")
	}
	if e.Pipeline == nil {
		t.Fatalf("expected non-nil Pipeline")
	}
	if e.Pipeline.Source.Name != "state" {
		t.Fatalf("source = %q, want state", e.Pipeline.Source.Name)
	}
	if len(e.Pipeline.Source.Args) != 1 || e.Pipeline.Source.Args[0].Value != id {
		t.Fatalf("source args = %#v, want [%q]", e.Pipeline.Source.Args, id)
	}
	if len(e.Pipeline.Filters) != 1 {
		t.Fatalf("filters = %d, want 1", len(e.Pipeline.Filters))
	}
	if e.Pipeline.Filters[0].Name != "upper" {
		t.Fatalf("filter = %q, want upper", e.Pipeline.Filters[0].Name)
	}
	if len(e.Pipeline.Filters[0].Args) != 0 {
		t.Fatalf("upper should carry no args, got %#v", e.Pipeline.Filters[0].Args)
	}
}

func TestParsePipelineAttrDefault(t *testing.T) {
	const id = "01J8Z3K4N5P6Q7R8S9T0V1W2Z3"
	e := mustParse(t, `{"expr":"attr('`+id+`','battery') | default(true)"}`)
	if e.Pipeline == nil {
		t.Fatalf("expected non-nil Pipeline")
	}
	src := e.Pipeline.Source
	if src.Name != "attr" {
		t.Fatalf("source = %q, want attr", src.Name)
	}
	if len(src.Args) != 2 || src.Args[0].Value != id || src.Args[1].Value != "battery" {
		t.Fatalf("attr args = %#v, want [%q battery]", src.Args, id)
	}
	if len(e.Pipeline.Filters) != 1 || e.Pipeline.Filters[0].Name != "default" {
		t.Fatalf("filters = %#v, want [default]", e.Pipeline.Filters)
	}
	fargs := e.Pipeline.Filters[0].Args
	if len(fargs) != 1 || fargs[0].Value != true {
		t.Fatalf("default args = %#v, want [true]", fargs)
	}
}

func TestParseNowSource(t *testing.T) {
	e := mustParse(t, `{"expr":"now()"}`)
	if e.Pipeline == nil {
		t.Fatalf("expected non-nil Pipeline")
	}
	if e.Pipeline.Source.Name != "now" {
		t.Fatalf("source = %q, want now", e.Pipeline.Source.Name)
	}
	if len(e.Pipeline.Source.Args) != 0 {
		t.Fatalf("now() args = %#v, want none", e.Pipeline.Source.Args)
	}
	if len(e.Pipeline.Filters) != 0 {
		t.Fatalf("filters = %d, want 0", len(e.Pipeline.Filters))
	}
}

func TestParseNumericAndDoubleQuotedArgs(t *testing.T) {
	// double-quoted string args + numeric args parse (round precision, convert units).
	e := mustParse(t, `{"expr":"attr(\"01J8Z3K4N5P6Q7R8S9T0V1W2Z2\",\"temp\") | round(2) | convert('s','ms')"}`)
	if e.Pipeline == nil {
		t.Fatalf("expected pipeline")
	}
	if e.Pipeline.Filters[0].Name != "round" || e.Pipeline.Filters[0].Args[0].Value != float64(2) {
		t.Fatalf("round arg = %#v, want float64(2)", e.Pipeline.Filters[0].Args)
	}
	conv := e.Pipeline.Filters[1]
	if conv.Name != "convert" || len(conv.Args) != 2 || conv.Args[0].Value != "s" || conv.Args[1].Value != "ms" {
		t.Fatalf("convert = %#v, want convert('s','ms')", conv)
	}
}

func TestParseUnknownFilter(t *testing.T) {
	// RUL-290: a filter name not in the closed list surfaces UNKNOWN_FILTER.
	_, perr := Parse(json.RawMessage(`{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z2') | bogus()"}`))
	if perr == nil {
		t.Fatalf("expected error for unknown filter")
	}
	if perr.Code != CodeUnknownFilter {
		t.Fatalf("code = %q, want %q", perr.Code, CodeUnknownFilter)
	}
}

func TestParseFilterAsSourceRejected(t *testing.T) {
	// RUL-281/292: only state/attr/now may be a pipeline source; a filter
	// (here upper) in the source position is rejected.
	_, perr := Parse(json.RawMessage(`{"expr":"upper('x')"}`))
	if perr == nil {
		t.Fatalf("expected error for filter-as-source")
	}
	if perr.Code != CodeInvalidSource {
		t.Fatalf("code = %q, want %q", perr.Code, CodeInvalidSource)
	}
}

func TestParseUnknownNameAsSourceRejected(t *testing.T) {
	// A name that is neither a source nor a filter, in source position, is
	// still an invalid source (RUL-281/292) — not UNKNOWN_FILTER.
	_, perr := Parse(json.RawMessage(`{"expr":"nope('x')"}`))
	if perr == nil {
		t.Fatalf("expected error")
	}
	if perr.Code != CodeInvalidSource {
		t.Fatalf("code = %q, want %q", perr.Code, CodeInvalidSource)
	}
}

func TestParseSourceOnlyNameAsFilterRejected(t *testing.T) {
	// RUL-292: state/attr/now may appear ONLY as a source, never as a later
	// filter stage.
	_, perr := Parse(json.RawMessage(`{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z2') | state('01J8Z3K4N5P6Q7R8S9T0V1W2Z2')"}`))
	if perr == nil {
		t.Fatalf("expected error for source-name in filter position")
	}
	if perr.Code != CodeInvalidSource {
		t.Fatalf("code = %q, want %q", perr.Code, CodeInvalidSource)
	}
}

func TestParseAllFourteenFiltersKnown(t *testing.T) {
	// RUL-290 closed list: every one of the 11 non-source filters parses as a
	// known filter (the 3 source-only names state/attr/now are covered above).
	filters := []string{
		"default(0)", "upper", "lower", "trim", "round(1)", "abs",
		"int", "float", "elapsed", "duration('s')", "convert('s','ms')",
	}
	for _, f := range filters {
		raw := `{"expr":"now() | ` + f + `"}`
		if _, perr := Parse(json.RawMessage(raw)); perr != nil {
			t.Fatalf("filter %q should be known, got %v", f, perr)
		}
	}
}

func TestParseObjectMustBeExprKey(t *testing.T) {
	// RUL-280: the only valid Expression object shape is {"expr": "<pipeline>"}.
	for _, raw := range []string{`{}`, `{"pipeline":"now()"}`, `{"expr":42}`, `{"expr":"now()","x":1}`} {
		if _, perr := Parse(json.RawMessage(raw)); perr == nil {
			t.Fatalf("expected error for malformed object %s", raw)
		}
	}
}

func TestParseMalformedPipeline(t *testing.T) {
	for _, raw := range []string{
		`{"expr":"state('x'"}`,        // unbalanced paren
		`{"expr":"state('x') |"}`,     // trailing pipe
		`{"expr":""}`,                 // empty pipeline
		`{"expr":"state('x') upper"}`, // missing pipe
	} {
		if _, perr := Parse(json.RawMessage(raw)); perr == nil {
			t.Fatalf("expected error for malformed pipeline %s", raw)
		}
	}
}
