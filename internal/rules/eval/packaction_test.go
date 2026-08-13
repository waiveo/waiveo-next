package eval

import (
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/model"
)

// packaction_test.go exercises RUL-231's `pack_action` as RunActions dispatches
// it.
//
// The defect being closed is the same one variablewrite_test.go closed for
// RUL-220, and it was even quieter here: `pack_action` was NAMED in RunActions'
// default arm's comment as a type deliberately not performed. So the first
// assertion every case makes is that the sink was reached — a test that only
// checked the recorded outcome would pass against the no-op for every case
// whose expectation is "nothing happened".

type recordingPackSink struct {
	calls  []packCall
	answer func(name string, params map[string]any) PackActionOutcome
}

type packCall struct {
	Name   string
	Params map[string]any
}

func (s *recordingPackSink) InvokePackAction(name string, params map[string]any) PackActionOutcome {
	s.calls = append(s.calls, packCall{Name: name, Params: params})
	if s.answer != nil {
		return s.answer(name, params)
	}
	return PackActionOutcome{Action: "pack_action", Name: name, OK: true}
}

// packAction builds one action member THROUGH model.ParseRule, for the reason
// varActions states: Member.Raw is populated only by the package's own decoder,
// and a member built by a bare Unmarshal would send every case down the
// "unparseable action" branch and pass for the wrong reason.
func packAction(t *testing.T, raw string) model.Member {
	t.Helper()
	body := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z1","actions":[` + raw + `]}`
	rule, err := model.ParseRule([]byte(body))
	if err != nil {
		t.Fatalf("ParseRule(%s): %v", body, err)
	}
	if len(rule.Actions) != 1 || len(rule.Actions[0].Raw) == 0 {
		t.Fatalf("ParseRule yielded %d action(s) / empty Raw; the case would test the wrong branch", len(rule.Actions))
	}
	return rule.Actions[0]
}

func runPackActions(t *testing.T, raw string) (*recordingPackSink, []PackActionOutcome) {
	t.Helper()
	sink := &recordingPackSink{}
	var outs []PackActionOutcome
	ctx := ActionContext{PackActions: sink, PackActionOutcomes: &outs}
	if err := RunActions(ctx, []model.Member{packAction(t, raw)}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	return sink, outs
}

// --- the headline ------------------------------------------------------------

func TestPackAction_ReachesTheSink_RUL231(t *testing.T) {
	sink, outs := runPackActions(t,
		`{"type":"pack_action","action":"waiveo/backups.run-backup","params":{"target":"nightly"}}`)

	if len(sink.calls) != 1 {
		t.Fatalf("RUL-231: the action MUST reach the sink; got %d call(s) — this is the no-op regression", len(sink.calls))
	}
	if sink.calls[0].Name != "waiveo/backups.run-backup" {
		t.Errorf("name = %q, want waiveo/backups.run-backup", sink.calls[0].Name)
	}
	if got := sink.calls[0].Params["target"]; got != "nightly" {
		t.Errorf("params[target] = %v, want nightly", got)
	}
	if len(outs) != 1 || !outs[0].OK || outs[0].Name != "waiveo/backups.run-backup" || outs[0].Action != "pack_action" {
		t.Fatalf("the outcome must be recorded; got %+v", outs)
	}
}

// An action with no params reaches the sink with an EMPTY map rather than a nil
// one, so a sink may index it without a nil check.
func TestPackAction_OmittedParamsArriveAsAnEmptyMap(t *testing.T) {
	sink, outs := runPackActions(t, `{"type":"pack_action","action":"waiveo/system.reboot"}`)

	if len(sink.calls) != 1 {
		t.Fatalf("the action MUST reach the sink; got %d call(s)", len(sink.calls))
	}
	if sink.calls[0].Params == nil {
		t.Fatalf("params arrived nil; an action declaring none must still hand the sink a usable map")
	}
	if len(sink.calls[0].Params) != 0 {
		t.Errorf("params = %v, want empty", sink.calls[0].Params)
	}
	if len(outs) != 1 || !outs[0].OK {
		t.Fatalf("outcome = %+v, want one OK", outs)
	}
}

// --- refusals: named, never silent -------------------------------------------

// Each of these is a rule the AUTHOR got wrong. The requirement is not merely
// that the invocation does not happen — it is that the sink is never called AND
// the run says why, since a `pack_action` that vanished would leave the operator
// with a run that reported success and a pack that was never asked.
func TestPackAction_MalformedDeclarationsAreRefusedNotDropped(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // a substring the reported error must contain
	}{
		{"no action member", `{"type":"pack_action"}`, "must declare `action`"},
		{"action at the wrong type", `{"type":"pack_action","action":42}`, "non-empty string"},
		{"empty action", `{"type":"pack_action","action":""}`, "non-empty string"},
		{"params at the wrong type", `{"type":"pack_action","action":"waiveo/x.y","params":[1,2]}`, "`params` must be an object"},
		{"params as a string", `{"type":"pack_action","action":"waiveo/x.y","params":"nightly"}`, "`params` must be an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink, outs := runPackActions(t, tc.raw)

			if len(sink.calls) != 0 {
				t.Fatalf("the sink MUST NOT be called for a malformed action; got %+v", sink.calls)
			}
			if len(outs) != 1 {
				t.Fatalf("the refusal MUST be reported; got %d outcome(s)", len(outs))
			}
			if outs[0].OK {
				t.Errorf("outcome reported OK for a refused action: %+v", outs[0])
			}
			if !strings.Contains(outs[0].Error, tc.want) {
				t.Errorf("error = %q, want it to contain %q", outs[0].Error, tc.want)
			}
		})
	}
}

// A `params` object that is present but empty is not a malformed one.
func TestPackAction_EmptyParamsObjectIsAccepted(t *testing.T) {
	sink, outs := runPackActions(t, `{"type":"pack_action","action":"waiveo/x.y","params":{}}`)
	if len(sink.calls) != 1 {
		t.Fatalf("an empty params object is valid; the sink must still be called (got %d)", len(sink.calls))
	}
	if len(outs) != 1 || !outs[0].OK {
		t.Fatalf("outcome = %+v, want one OK", outs)
	}
}

// --- the edge posture ---------------------------------------------------------

// No sink wired is the RELAY's normal posture, not a misconfiguration: a rule
// containing a pack_action is app-classified, so the edge engine is never handed
// one. It must not panic, and it must not fabricate an outcome either — an
// engine with no sink performed no pack action and has nothing to report.
func TestPackAction_NoSinkIsSilentNotAPanic(t *testing.T) {
	var outs []PackActionOutcome
	ctx := ActionContext{PackActionOutcomes: &outs}
	if err := RunActions(ctx, []model.Member{
		packAction(t, `{"type":"pack_action","action":"waiveo/backups.run-backup"}`)}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(outs) != 0 {
		t.Fatalf("an unwired sink must report nothing; got %+v", outs)
	}
}

// A run that collects no outcomes at all (a caller that passed no slice) must
// still call the sink: the recording is a report, not the mechanism.
func TestPackAction_RunsEvenWhenNoOutcomeSliceIsCollected(t *testing.T) {
	sink := &recordingPackSink{}
	if err := RunActions(ActionContext{PackActions: sink}, []model.Member{
		packAction(t, `{"type":"pack_action","action":"waiveo/backups.run-backup"}`)}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("the sink MUST be called regardless of outcome collection; got %d call(s)", len(sink.calls))
	}
}

// --- sequencing ---------------------------------------------------------------

// A refused pack action must not halt the actions that follow it (the
// independence rule RUL-236 fixes for signage and RUL-171 for commands), and the
// outcomes must arrive in ACTION ORDER so the report can be read against the
// rule.
func TestPackAction_ARefusalDoesNotHaltTheRestOfTheSequence(t *testing.T) {
	sink := &recordingPackSink{}
	var outs []PackActionOutcome
	body := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z1","actions":[` +
		`{"type":"pack_action","action":""},` +
		`{"type":"pack_action","action":"waiveo/a.first"},` +
		`{"type":"pack_action","action":"waiveo/b.second"}]}`
	rule, err := model.ParseRule([]byte(body))
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	if err := RunActions(ActionContext{PackActions: sink, PackActionOutcomes: &outs}, rule.Actions); err != nil {
		t.Fatalf("RunActions: %v", err)
	}

	if len(sink.calls) != 2 {
		t.Fatalf("the two valid actions must both run; got %d call(s): %+v", len(sink.calls), sink.calls)
	}
	if sink.calls[0].Name != "waiveo/a.first" || sink.calls[1].Name != "waiveo/b.second" {
		t.Errorf("the sink saw %+v, want a.first then b.second in order", sink.calls)
	}
	if len(outs) != 3 {
		t.Fatalf("all three outcomes must be reported; got %d: %+v", len(outs), outs)
	}
	if outs[0].OK || !outs[1].OK || !outs[2].OK {
		t.Errorf("outcomes must be refused,ok,ok in action order; got %+v", outs)
	}
	if outs[1].Name != "waiveo/a.first" || outs[2].Name != "waiveo/b.second" {
		t.Errorf("outcomes are out of action order: %+v", outs)
	}
}
