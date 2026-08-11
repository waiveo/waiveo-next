package eval

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/model"
)

// variablewrite_test.go exercises RUL-220's `variable_write` action as
// RunActions dispatches it.
//
// The defect being closed: this action type used to fall through RunActions'
// `default:` arm and no-op. So the FIRST assertion any case here has to make is
// that the sink was called at all — a test that only checked the recorded
// outcome would have passed against the no-op implementation for every case
// where the expected outcome was "nothing recorded".

// recordingVarSink captures every WriteVariable call and answers with a
// caller-chosen outcome.
type recordingVarSink struct {
	calls  []varCall
	answer func(name string, value any) VariableOutcome
}

type varCall struct {
	Name  string
	Value any
}

func (s *recordingVarSink) WriteVariable(name string, value any) VariableOutcome {
	s.calls = append(s.calls, varCall{Name: name, Value: value})
	if s.answer != nil {
		return s.answer(name, value)
	}
	return VariableOutcome{Action: "variable_write", Variable: name, Value: value, OK: true}
}

// varActions builds action members from raw JSON THROUGH model.ParseRule — the
// same decoder the store's compile gate and the engine use.
//
// It is not `json.Unmarshal` into a model.Member, and that is load-bearing
// rather than fussy: Member.Raw is populated only by the package's own decoder,
// and a Member built by a bare Unmarshal arrives with Raw empty. Every case here
// would then exercise the "unparseable action" branch and pass for the wrong
// reason — which is exactly what the first draft of this file did.
func varActions(t *testing.T, raws ...string) []model.Member {
	t.Helper()
	body := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z1","actions":[`
	for i, raw := range raws {
		if i > 0 {
			body += ","
		}
		body += raw
	}
	body += `]}`
	rule, err := model.ParseRule([]byte(body))
	if err != nil {
		t.Fatalf("ParseRule(%s): %v", body, err)
	}
	if len(rule.Actions) != len(raws) {
		t.Fatalf("ParseRule yielded %d action(s), want %d", len(rule.Actions), len(raws))
	}
	for i, m := range rule.Actions {
		if len(m.Raw) == 0 {
			t.Fatalf("action[%d] decoded with an EMPTY Raw; every case below would exercise the unparseable branch", i)
		}
	}
	return rule.Actions
}

// varAction is varActions for the single-action cases.
func varAction(t *testing.T, raw string) model.Member {
	t.Helper()
	return varActions(t, raw)[0]
}

// runVarActions runs one action through the real RunActions with a recording
// sink, returning the sink and the recorded outcomes.
func runVarActions(t *testing.T, raw string) (*recordingVarSink, []VariableOutcome) {
	t.Helper()
	sink := &recordingVarSink{}
	var outs []VariableOutcome
	ctx := ActionContext{Variables: sink, VariableOutcomes: &outs}
	if err := RunActions(ctx, []model.Member{varAction(t, raw)}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	return sink, outs
}

// --- the headline: the action actually reaches a sink -----------------------

func TestVariableWrite_ReachesTheSink_RUL220(t *testing.T) {
	sink, outs := runVarActions(t, `{"type":"variable_write","variable":"guest_mode","value":true}`)

	if len(sink.calls) != 1 {
		t.Fatalf("RUL-220: the action MUST reach the sink; got %d call(s) — this is the no-op regression", len(sink.calls))
	}
	if sink.calls[0].Name != "guest_mode" {
		t.Errorf("name = %q, want guest_mode", sink.calls[0].Name)
	}
	if sink.calls[0].Value != true {
		t.Errorf("value = %v (%T), want true", sink.calls[0].Value, sink.calls[0].Value)
	}
	if len(outs) != 1 || !outs[0].OK || outs[0].Variable != "guest_mode" {
		t.Fatalf("the outcome must be recorded; got %+v", outs)
	}
}

// Every scalar the data model admits survives the trip to the sink at its own
// Go type — a number must not arrive as a string.
func TestVariableWrite_CarriesEachScalarTypeThrough(t *testing.T) {
	cases := []struct {
		raw  string
		want any
	}{
		{`{"type":"variable_write","variable":"a","value":"open"}`, "open"},
		{`{"type":"variable_write","variable":"a","value":42}`, float64(42)},
		{`{"type":"variable_write","variable":"a","value":true}`, true},
		{`{"type":"variable_write","variable":"a","value":false}`, false},
	}
	for _, c := range cases {
		sink, _ := runVarActions(t, c.raw)
		if len(sink.calls) != 1 {
			t.Fatalf("%s: want 1 sink call, got %d", c.raw, len(sink.calls))
		}
		if sink.calls[0].Value != c.want {
			t.Errorf("%s: value = %v (%T), want %v (%T)", c.raw, sink.calls[0].Value, sink.calls[0].Value, c.want, c.want)
		}
	}
}

// --- the nil-sink posture ----------------------------------------------------

// The EDGE posture. A nil sink is normal, not a misconfiguration: the action is
// app-class (RUL-220) so the relay's engine never holds one. It must not panic
// and must not record a fabricated outcome.
func TestVariableWrite_NilSinkIsASilentSkip_NotAPanic(t *testing.T) {
	var outs []VariableOutcome
	ctx := ActionContext{VariableOutcomes: &outs} // Variables deliberately nil
	if err := RunActions(ctx, []model.Member{
		varAction(t, `{"type":"variable_write","variable":"guest_mode","value":true}`),
	}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(outs) != 0 {
		t.Fatalf("an edge context has no sink and must record nothing; got %+v", outs)
	}
}

// A recorder-less context must not panic either — VariableOutcomes is optional
// exactly as SignageOutcomes is.
func TestVariableWrite_NilRecorderStillWrites(t *testing.T) {
	sink := &recordingVarSink{}
	ctx := ActionContext{Variables: sink} // VariableOutcomes deliberately nil
	if err := RunActions(ctx, []model.Member{
		varAction(t, `{"type":"variable_write","variable":"guest_mode","value":true}`),
	}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("the write must still happen without a recorder; got %d call(s)", len(sink.calls))
	}
}

// --- malformed actions are REPORTED, never silently skipped -----------------

// Each of these is a rule that reached the executor in a shape RUL-220 refuses.
// The requirement is the same one runSignage makes: report it, because a run
// that answered `ran` with an empty effect report and an unchanged variable is
// indistinguishable from one that worked.
func TestVariableWrite_MalformedActionsAreReportedNotSkipped(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"no variable member", `{"type":"variable_write","value":true}`},
		{"variable at the wrong type", `{"type":"variable_write","variable":7,"value":true}`},
		{"no value member", `{"type":"variable_write","variable":"guest_mode"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink, outs := runVarActions(t, c.raw)
			if len(sink.calls) != 0 {
				t.Fatalf("a malformed action must not reach the sink; got %+v", sink.calls)
			}
			if len(outs) != 1 {
				t.Fatalf("a malformed action must be REPORTED, not skipped; got %d outcome(s)", len(outs))
			}
			if outs[0].OK {
				t.Errorf("outcome must be a failure; got %+v", outs[0])
			}
			if outs[0].Error == "" {
				t.Errorf("a failed outcome must carry a reason; got %+v", outs[0])
			}
			if outs[0].Action != "variable_write" {
				t.Errorf("outcome must name the action; got %q", outs[0].Action)
			}
		})
	}
}

// An action whose value Expression fails closed (RUL-284) writes nothing AND is
// recorded — RUL-284 requires the failure be recorded for operator visibility
// rather than silently discarded.
func TestVariableWrite_ValueExpressionFailingClosedIsRecorded_RUL284(t *testing.T) {
	sink := &recordingVarSink{}
	var outs []VariableOutcome
	ctx := ActionContext{
		Variables:        sink,
		VariableOutcomes: &outs,
		EvalExpr:         func(json.RawMessage) (any, bool) { return nil, false },
	}
	if err := RunActions(ctx, []model.Member{
		varAction(t, `{"type":"variable_write","variable":"guest_mode","value":{"expr":"state(01J8Z3K4N5P6Q7R8S9T0V1W2Z2)"}}`),
	}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("a failed-closed Expression must write nothing; got %+v", sink.calls)
	}
	if len(outs) != 1 || outs[0].OK || outs[0].Error == "" {
		t.Fatalf("RUL-284: the failure must be recorded with a reason; got %+v", outs)
	}
}

// The other half of the Expression path: an evaluator that SUCCEEDS supplies
// the written value, so the value the sink sees is the evaluated one, not the
// raw `{"expr": ...}` object.
func TestVariableWrite_ValueExpressionIsLiveEvaluated_RUL220(t *testing.T) {
	sink := &recordingVarSink{}
	var outs []VariableOutcome
	ctx := ActionContext{
		Variables:        sink,
		VariableOutcomes: &outs,
		EvalExpr:         func(json.RawMessage) (any, bool) { return "evaluated", true },
	}
	if err := RunActions(ctx, []model.Member{
		varAction(t, `{"type":"variable_write","variable":"guest_mode","value":{"expr":"now()"}}`),
	}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 1 || sink.calls[0].Value != "evaluated" {
		t.Fatalf("the EVALUATED value must be written, not the expr object; got %+v", sink.calls)
	}
}

// --- the sink's own refusal reaches the report ------------------------------

// A sink that refuses (a duplicate name, an unauthorized placement) has its
// outcome recorded verbatim. Without this the store's refusal would be a
// variable that did not change and nothing anywhere saying why.
func TestVariableWrite_SinkRefusalIsRecordedVerbatim(t *testing.T) {
	sink := &recordingVarSink{answer: func(name string, _ any) VariableOutcome {
		return VariableOutcome{Action: "variable_write", Variable: name, OK: false, Error: "refused by the store"}
	}}
	var outs []VariableOutcome
	ctx := ActionContext{Variables: sink, VariableOutcomes: &outs}
	if err := RunActions(ctx, []model.Member{
		varAction(t, `{"type":"variable_write","variable":"guest_mode","value":true}`),
	}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(outs) != 1 || outs[0].OK || outs[0].Error != "refused by the store" {
		t.Fatalf("the sink's refusal must reach the report verbatim; got %+v", outs)
	}
}

// --- sequencing: a variable_write does not halt the rest of the sequence ----

// A refused write must not stop the actions after it, exactly as a failed
// device command does not (RUL-171).
func TestVariableWrite_ARefusedWriteDoesNotHaltTheSequence(t *testing.T) {
	sink := &recordingVarSink{answer: func(name string, _ any) VariableOutcome {
		return VariableOutcome{Action: "variable_write", Variable: name, OK: false, Error: "refused"}
	}}
	var outs []VariableOutcome
	var logs []LogEntry
	ctx := ActionContext{Variables: sink, VariableOutcomes: &outs, Logs: &logs}

	actions := varActions(t,
		`{"type":"variable_write","variable":"a","value":1}`,
		`{"type":"log","message":"after"}`)
	if err := RunActions(ctx, actions); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 1 || logs[0].Message != "after" {
		t.Fatalf("the action after a refused write must still run (RUL-171); got %+v", logs)
	}
}

// Two writes in one sequence are two calls in order, not one — the report is
// per action.
func TestVariableWrite_EachActionIsItsOwnCallAndOutcome(t *testing.T) {
	sink := &recordingVarSink{}
	var outs []VariableOutcome
	ctx := ActionContext{Variables: sink, VariableOutcomes: &outs}

	actions := varActions(t,
		`{"type":"variable_write","variable":"first","value":1}`,
		`{"type":"variable_write","variable":"second","value":2}`)
	if err := RunActions(ctx, actions); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 2 || sink.calls[0].Name != "first" || sink.calls[1].Name != "second" {
		t.Fatalf("both writes must dispatch, in order; got %+v", sink.calls)
	}
	if len(outs) != 2 {
		t.Fatalf("one outcome per action; got %d", len(outs))
	}
}
