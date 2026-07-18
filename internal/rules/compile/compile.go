package compile

import "github.com/maaxton/waiveo-next/internal/rules/model"

// Compile validates then classifies an authored rule. On a parse or structural
// violation it returns the typed CompileError (a caller reads this as
// compiles:false); otherwise the classified CompiledRuleEntry (compiles:true).
// It NEVER silently accepts an unknown member as app-class (RUL-001): an
// unparseable or unknown-member rule fails here, it does not fall through to
// classification.
func Compile(raw []byte) (CompiledRuleEntry, *CompileError) {
	r, err := model.ParseRule(raw)
	if err != nil {
		return CompiledRuleEntry{}, &CompileError{Code: codeUnknownVocabularyMember, Message: err.Error()}
	}
	if ce := Validate(r); ce != nil {
		return CompiledRuleEntry{}, ce
	}
	return Classify(r), nil
}
