// Package compile turns an authored rules/1 rule into a validated,
// edge/app-classified CompiledRuleEntry (or a typed CompileError). It is the
// rules/1 compiler front-end: parse (model) -> structural validation
// (validate.go) -> classification (classify.go). Evaluation, time/DST/misfire,
// and compile-time closure are later parts of the engine, not this package.
package compile

// CompileError is a typed rules/1 compile failure (Error taxonomy,
// contracts/rules-1.md). Code is a taxonomy code; Field is the JSON path of the
// offending member (RUL-006 addressing, e.g. "triggers[0].type",
// "actions[0].default[0].type"); Message is human-readable.
type CompileError struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

func (e *CompileError) Error() string {
	if e.Field != "" {
		return e.Code + " at " + e.Field + ": " + e.Message
	}
	return e.Code + ": " + e.Message
}

// Taxonomy codes this front-end emits (contracts/rules-1.md Error taxonomy).
const (
	codeUnknownVocabularyMember = "UNKNOWN_VOCABULARY_MEMBER"
	codeEntityRefAmbiguous      = "ENTITY_REF_AMBIGUOUS"
	codeModeMaxMissing          = "MODE_MAX_MISSING"
	codeModeMaxNotApplicable    = "MODE_MAX_NOT_APPLICABLE"
	codeMisfireInvalid          = "MISFIRE_INVALID"
)
