// Package expr is the rules/1 closed Expression grammar (RUL-280–292): a
// parser turning a JSON literal or an {"expr": "<pipeline>"} object into an
// AST, plus (in later parts) a fail-closed evaluator. An Expression is either a
// JSON literal used as-is or a `source (| filter(args...))*` pipeline whose
// source is one of state/attr/now (RUL-281/292) and whose every filter stage
// draws from the closed 14-filter list (RUL-290). The grammar is one shared
// list between edge and app evaluation (RUL-283); only the entity-reference
// scope (RUL-282) restricts the edge side.
package expr

// Expr is a parsed rules/1 Expression (RUL-280): either a JSON literal used
// as-is (Literal set, Pipeline nil) or a source|filter pipeline (Pipeline set,
// Literal nil). A null literal is a non-nil Literal pointing to a nil value, so
// a non-nil Literal — not the pointee — marks the literal form.
type Expr struct {
	Literal  *any
	Pipeline *Pipeline
}

// Pipeline is a source followed by zero or more filter stages (RUL-280/292):
// `source (| filter(args...))*`. Source is always one of state/attr/now; each
// Filters entry is a non-source filter from the RUL-290 closed list.
type Pipeline struct {
	Source  Call
	Filters []Call
}

// Call is one pipeline stage — a source or a filter — carrying its name and its
// parsed literal arguments (RUL-281/290).
type Call struct {
	Name string
	Args []Arg
}

// Arg is one literal call argument: a string (a quoted string, or a source's
// literal entity-id / attribute-name per RUL-281), a float64 (a JSON number), a
// bool, or nil (a null literal).
type Arg struct {
	Value any
}
