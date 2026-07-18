package eval

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/model"
)

// upperEvaluator is a stand-in ExprEvaluator that mimics the shape of the real
// expr evaluator the engine wires (RUL-200/393) without importing the expr
// package (which would cycle back into eval): it decodes an {"expr":"<s>"}
// object to the uppercased literal string <s>, passes a plain literal through
// as-is, and fails closed on anything else. It records every raw input it was
// asked to evaluate so a test can prove routing happened at dispatch.
type upperEvaluator struct {
	seen []string
}

func (u *upperEvaluator) eval(raw json.RawMessage) (any, bool) {
	u.seen = append(u.seen, string(raw))
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		if rawExpr, ok := obj["expr"]; ok && len(obj) == 1 {
			var s string
			if err := json.Unmarshal(rawExpr, &s); err == nil {
				return strings.ToUpper(s), true
			}
			return nil, false
		}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}

// TestRunLogMessageRoutedThroughEvalExpr proves a `log` action's message is an
// Expression evaluated at dispatch through ctx.EvalExpr (RUL-200), not decoded
// as a bare literal string: an {"expr":...} message is evaluated and its result
// logged.
func TestRunLogMessageRoutedThroughEvalExpr(t *testing.T) {
	u := &upperEvaluator{}
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Logs: &logs, EvalExpr: u.eval}

	a := parseAction(t, `{"type":"log","message":{"expr":"hello"}}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 1 || logs[0].Message != "HELLO" || logs[0].Level != "info" {
		t.Fatalf("log entries = %+v, want one info entry message=HELLO", logs)
	}
	if len(u.seen) != 1 || u.seen[0] != `{"expr":"hello"}` {
		t.Fatalf("EvalExpr was not called with the raw message: seen=%v", u.seen)
	}
}

// TestRunLogLiteralStringMessageBackwardCompatible proves a plain literal string
// message still logs as-is: with an evaluator wired it is a literal Expression
// (RUL-280) evaluated to itself; the message survives unchanged.
func TestRunLogLiteralStringMessageBackwardCompatible(t *testing.T) {
	u := &upperEvaluator{}
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Logs: &logs, EvalExpr: u.eval}

	a := parseAction(t, `{"type":"log","message":"plain text"}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 1 || logs[0].Message != "plain text" {
		t.Fatalf("log entries = %+v, want one entry message='plain text'", logs)
	}
}

// TestRunLogLiteralMessageWithNoEvaluatorWired proves the nil-EvalExpr fallback
// keeps a literal string message logging as-is (the degenerate no-Expression
// caller, e.g. a Part-1/2 unit context): a literal is used verbatim.
func TestRunLogLiteralMessageWithNoEvaluatorWired(t *testing.T) {
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Logs: &logs} // EvalExpr nil

	a := parseAction(t, `{"type":"log","message":"still works"}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 1 || logs[0].Message != "still works" {
		t.Fatalf("log entries = %+v, want one entry message='still works'", logs)
	}
}

// TestRunLogMessageFailClosedRecordsNoEntry proves a message Expression that
// fails closed (RUL-284) fabricates no log entry — the action is skipped, never
// an error.
func TestRunLogMessageFailClosedRecordsNoEntry(t *testing.T) {
	failEval := func(json.RawMessage) (any, bool) { return nil, false }
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Logs: &logs, EvalExpr: failEval}

	a := parseAction(t, `{"type":"log","message":{"expr":"state('x')"}}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("fail-closed message fabricated a log entry: %+v", logs)
	}
}

// TestRunLogMessageNonStringResultRecordsNoEntry proves a message Expression
// evaluating to a non-string value is treated as a type mismatch and fails
// closed (RUL-200 requires a string), logging nothing.
func TestRunLogMessageNonStringResultRecordsNoEntry(t *testing.T) {
	numEval := func(json.RawMessage) (any, bool) { return float64(42), true }
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Logs: &logs, EvalExpr: numEval}

	a := parseAction(t, `{"type":"log","message":{"expr":"state('x') | int"}}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("a non-string message result was logged: %+v", logs)
	}
}

// TestRunLogMessageEvaluatedLivePerDispatch proves the message Expression is
// evaluated at each dispatch, never once and frozen (RUL-393's live posture
// applies equally to the message): the same action logged twice against a
// changing evaluator produces two different messages.
func TestRunLogMessageEvaluatedLivePerDispatch(t *testing.T) {
	current := "first"
	liveEval := func(json.RawMessage) (any, bool) { return current, true }
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Logs: &logs, EvalExpr: liveEval}

	a := parseAction(t, `{"type":"log","message":{"expr":"state('x')"}}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	current = "second"
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 2 || logs[0].Message != "first" || logs[1].Message != "second" {
		t.Fatalf("live per-dispatch message evaluation failed: %+v", logs)
	}
}

// TestRunDeviceCommandParamsLiveEvaluated proves each `params` value is an
// Expression live-evaluated at dispatch (RUL-393): an {"expr":...} param is
// evaluated and its result dispatched, and the evaluator is invoked with the
// raw param JSON.
func TestRunDeviceCommandParamsLiveEvaluated(t *testing.T) {
	u := &upperEvaluator{}
	sink := &fakeSink{}
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink, EvalExpr: u.eval}

	a := parseAction(t, `{"type":"device_command","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","command":"power_on","params":{"app":{"expr":"netflix"},"n":7}}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(sink.calls))
	}
	p := sink.calls[0].Params
	if p["app"] != "NETFLIX" {
		t.Errorf("params[app] = %v, want NETFLIX (live-evaluated Expression)", p["app"])
	}
	if p["n"] != float64(7) {
		t.Errorf("params[n] = %v, want 7 (literal passed through)", p["n"])
	}
}

// TestRunDeviceCommandParamsFailClosedSkipsDispatch proves a `params` Expression
// that fails closed (RUL-284/393) prevents the whole command dispatch — a device
// command is never sent on a fabricated or partially-evaluated parameter set.
func TestRunDeviceCommandParamsFailClosedSkipsDispatch(t *testing.T) {
	failParam := func(raw json.RawMessage) (any, bool) {
		if strings.Contains(string(raw), "expr") {
			return nil, false
		}
		var v any
		_ = json.Unmarshal(raw, &v)
		return v, true
	}
	sink := &fakeSink{}
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink, EvalExpr: failParam}

	a := parseAction(t, `{"type":"device_command","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","command":"power_on","params":{"level":{"expr":"attr('x','missing')"}}}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("a fail-closed params Expression still dispatched: %+v", sink.calls)
	}
}

// TestRunDeviceCommandLiteralParamsNoEvaluatorBackwardCompatible proves the
// nil-EvalExpr fallback keeps literal params dispatching unchanged.
func TestRunDeviceCommandLiteralParamsNoEvaluatorBackwardCompatible(t *testing.T) {
	sink := &fakeSink{}
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink} // EvalExpr nil

	a := parseAction(t, `{"type":"device_command","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","command":"power_on","params":{"x":1}}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 1 || sink.calls[0].Params["x"] != float64(1) {
		t.Fatalf("literal params dispatch regressed: %+v", sink.calls)
	}
}
