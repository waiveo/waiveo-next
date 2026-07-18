package eval

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// --- test doubles ------------------------------------------------------------

// dispatchCall records one CommandSink.Dispatch invocation for assertion.
type dispatchCall struct {
	EntityID string
	Command  string
	Params   map[string]any
}

// fakeSink is a CommandSink test double: every Dispatch is recorded, and fails
// with the scripted error message for any entityID present in fail.
type fakeSink struct {
	calls []dispatchCall
	fail  map[string]string // entityID -> error message
}

func (f *fakeSink) Dispatch(entityID, command string, params map[string]any) error {
	f.calls = append(f.calls, dispatchCall{EntityID: entityID, Command: command, Params: params})
	if msg, ok := f.fail[entityID]; ok {
		return errors.New(msg)
	}
	return nil
}

// fakePresets is a PresetStore test double.
type fakePresets struct {
	byID map[string][]PresetCommand
}

func (f *fakePresets) Commands(presetID string) ([]PresetCommand, bool) {
	c, ok := f.byID[presetID]
	return c, ok
}

func parseActions(t *testing.T, actionsJSON string) []model.Member {
	t.Helper()
	ruleJSON := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2ZA","actions":` + actionsJSON + `}`
	r, err := model.ParseRule([]byte(ruleJSON))
	if err != nil {
		t.Fatalf("ParseRule(%s): %v", ruleJSON, err)
	}
	return r.Actions
}

func parseAction(t *testing.T, actionJSON string) model.Member {
	t.Helper()
	as := parseActions(t, `[`+actionJSON+`]`)
	if len(as) != 1 {
		t.Fatalf("expected 1 action, got %d", len(as))
	}
	return as[0]
}

// --- RUL-171 corpus: preset-batch partial failure ---------------------------

type rul171Corpus struct {
	Input struct {
		Preset struct {
			PresetID string `json:"preset_id"`
			Commands []struct {
				EntityID string `json:"entity_id"`
				Command  string `json:"command"`
			} `json:"commands"`
		} `json:"preset"`
		CommandResults []struct {
			EntityID string `json:"entity_id"`
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
		} `json:"command_results"`
		RuleActions []json.RawMessage `json:"rule_actions"`
	} `json:"input"`
	Expected struct {
		PresetBatchOutcome struct {
			Outcome string `json:"outcome"`
			Results []struct {
				Target  string `json:"target"`
				Command string `json:"command"`
				OK      bool   `json:"ok"`
				Error   string `json:"error"`
			} `json:"results"`
		} `json:"preset_batch_outcome"`
		BothCommandsAttempted bool `json:"both_commands_attempted"`
		LogActionRan          bool `json:"log_action_ran"`
	} `json:"expected"`
}

func TestRunActionsRUL171PresetBatchPartialFailure(t *testing.T) {
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "rules-1", "RUL-171-preset-batch-partial-failure.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var c rul171Corpus
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	presetCmds := make([]PresetCommand, 0, len(c.Input.Preset.Commands))
	for _, pc := range c.Input.Preset.Commands {
		presetCmds = append(presetCmds, PresetCommand{EntityID: pc.EntityID, Command: pc.Command})
	}
	presets := &fakePresets{byID: map[string][]PresetCommand{c.Input.Preset.PresetID: presetCmds}}

	sink := &fakeSink{fail: map[string]string{}}
	for _, cr := range c.Input.CommandResults {
		if !cr.OK {
			sink.fail[cr.EntityID] = cr.Error
		}
	}

	actionsJSON, err := json.Marshal(c.Input.RuleActions)
	if err != nil {
		t.Fatalf("marshal rule_actions: %v", err)
	}
	actions := parseActions(t, string(actionsJSON))

	var outcomes []PresetBatchOutcome
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink, Presets: presets, Outcomes: &outcomes, Logs: &logs}

	if err := RunActions(ctx, actions); err != nil {
		t.Fatalf("RunActions: %v", err)
	}

	if c.Expected.BothCommandsAttempted && len(sink.calls) != 2 {
		t.Fatalf("both_commands_attempted: sink got %d calls, want 2: %+v", len(sink.calls), sink.calls)
	}

	if len(outcomes) != 1 {
		t.Fatalf("expected 1 preset-batch outcome, got %d", len(outcomes))
	}
	got := outcomes[0]
	want := c.Expected.PresetBatchOutcome
	if got.Outcome != want.Outcome {
		t.Errorf("outcome = %q, want %q", got.Outcome, want.Outcome)
	}
	if len(got.Results) != len(want.Results) {
		t.Fatalf("results len = %d, want %d", len(got.Results), len(want.Results))
	}
	for i, wr := range want.Results {
		gr := got.Results[i]
		if gr.Target != wr.Target || gr.Command != wr.Command || gr.OK != wr.OK || gr.Error != wr.Error {
			t.Errorf("results[%d] = %+v, want %+v", i, gr, wr)
		}
	}

	if c.Expected.LogActionRan && len(logs) != 1 {
		t.Fatalf("log_action_ran: got %d log entries, want 1", len(logs))
	}
}

// --- RUL-160/161: device_command, single-entity atomic pass/fail -----------

func TestRunActionsDeviceCommandSingleEntityAtomicPass(t *testing.T) {
	sink := &fakeSink{}
	var outcomes []PresetBatchOutcome
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink, Outcomes: &outcomes}

	a := parseAction(t, `{"type":"device_command","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","command":"power_on","params":{"x":1}}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(sink.calls))
	}
	call := sink.calls[0]
	if call.EntityID != "01J8Z3K4N5P6Q7R8S9T0V1W2Z2" || call.Command != "power_on" {
		t.Errorf("dispatch call = %+v, want entity/command match", call)
	}
	if params, ok := call.Params["x"]; !ok || params != float64(1) {
		t.Errorf("dispatch params = %+v, want x=1", call.Params)
	}
	// RUL-161: single-entity dispatch is atomic pass/fail, not aggregated into
	// a PresetBatchOutcome (that shape is reserved for multi-entity fan-out
	// and preset_batch).
	if len(outcomes) != 0 {
		t.Errorf("outcomes = %+v, want none for a single-entity dispatch", outcomes)
	}
}

func TestRunActionsDeviceCommandSingleEntityAtomicFailDoesNotHaltSequence(t *testing.T) {
	sink := &fakeSink{fail: map[string]string{"01J8Z3K4N5P6Q7R8S9T0V1W2Z2": "unreachable"}}
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink, Logs: &logs}

	a := parseAction(t, `{"type":"device_command","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","command":"power_on"}`)
	logAction := parseAction(t, `{"type":"log","message":"after failed command"}`)
	if err := RunActions(ctx, []model.Member{a, logAction}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(sink.calls))
	}
	if len(logs) != 1 {
		t.Fatalf("a failing device_command halted the sequence: got %d log entries, want 1", len(logs))
	}
}

// --- RUL-012/161/172: device_command multi-entity fan-out ------------------

func TestRunActionsDeviceCommandMultiEntityFanOutOutcomes(t *testing.T) {
	targets := []string{"e1", "e2", "e3"}
	resolve := func(ref model.EntityRef) []string {
		if ref.Selector == "label==all" {
			return targets
		}
		return nil
	}

	cases := []struct {
		name        string
		fail        map[string]string
		wantOutcome string
	}{
		{"complete", nil, "complete"},
		{"partial", map[string]string{"e2": "unreachable"}, "partial"},
		{"failed", map[string]string{"e1": "x", "e2": "y", "e3": "z"}, "failed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{fail: tc.fail}
			var outcomes []PresetBatchOutcome
			ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink, Resolve: resolve, Outcomes: &outcomes}

			a := parseAction(t, `{"type":"device_command","selector":"label==all","command":"power_on"}`)
			if err := RunActions(ctx, []model.Member{a}); err != nil {
				t.Fatalf("RunActions: %v", err)
			}
			if len(sink.calls) != 3 {
				t.Fatalf("RUL-012: sink calls = %d, want 3 (every target attempted)", len(sink.calls))
			}
			if len(outcomes) != 1 {
				t.Fatalf("outcomes = %d, want 1", len(outcomes))
			}
			if outcomes[0].Outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", outcomes[0].Outcome, tc.wantOutcome)
			}
			if len(outcomes[0].Results) != 3 {
				t.Fatalf("results len = %d, want 3", len(outcomes[0].Results))
			}
			for _, r := range outcomes[0].Results {
				wantOK := tc.fail[r.Target] == ""
				if r.OK != wantOK {
					t.Errorf("result[%s].OK = %v, want %v", r.Target, r.OK, wantOK)
				}
				if !wantOK && r.Error != tc.fail[r.Target] {
					t.Errorf("result[%s].Error = %q, want %q", r.Target, r.Error, tc.fail[r.Target])
				}
			}
		})
	}
}

// --- RUL-160: device_command/preset_batch command-vocabulary enforcement ---

func TestRunActionsDeviceCommandRejectsCommandNotInDeviceClassVocabulary(t *testing.T) {
	const entityID = "01J8Z3K4N5P6Q7R8S9T0V1W2Z2"
	sink := &fakeSink{}
	snap := state.Snapshot{entityID: state.Entity{ID: entityID, DeviceClass: "media-player"}}
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink, Reg: registry.FixtureRegistry{}, Snap: snap}

	// "reboot" is not in media-player's command vocabulary (fixture.go,
	// registry_test.go's own CommandExists("media-player","reboot")==false
	// assertion) — RUL-160 requires this be rejected, never dispatched as if
	// valid.
	a := parseAction(t, `{"type":"device_command","entity_id":"`+entityID+`","command":"reboot"}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("RUL-160: sink.calls = %+v, want none (command not in media-player's vocabulary)", sink.calls)
	}
}

func TestRunActionsDeviceCommandAllowsCommandInDeviceClassVocabulary(t *testing.T) {
	const entityID = "01J8Z3K4N5P6Q7R8S9T0V1W2Z2"
	sink := &fakeSink{}
	snap := state.Snapshot{entityID: state.Entity{ID: entityID, DeviceClass: "media-player"}}
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink, Reg: registry.FixtureRegistry{}, Snap: snap}

	// "power" IS in media-player's fixture vocabulary — a positive control
	// proving the RUL-160 check does not block a valid command.
	a := parseAction(t, `{"type":"device_command","entity_id":"`+entityID+`","command":"power"}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 1 || sink.calls[0].Command != "power" {
		t.Fatalf("sink.calls = %+v, want one dispatch of power", sink.calls)
	}
}

func TestRunActionsDeviceCommandUncheckedWhenNoRegistryConfigured(t *testing.T) {
	// ctx.Reg == nil means no registry has been wired in at all (the same
	// "not configured" posture ctx.Sink/ctx.Presets already carry elsewhere
	// in this package) — the RUL-160 check is skipped, not treated as a
	// violation, so a caller that hasn't wired a registry yet keeps its prior
	// (unchecked) dispatch behavior.
	const entityID = "01J8Z3K4N5P6Q7R8S9T0V1W2Z2"
	sink := &fakeSink{}
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink}

	a := parseAction(t, `{"type":"device_command","entity_id":"`+entityID+`","command":"reboot"}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("sink.calls = %+v, want one dispatch (no registry configured, nothing to enforce)", sink.calls)
	}
}

func TestRunActionsDeviceCommandMultiEntityRejectsInvalidCommandPerTarget(t *testing.T) {
	targets := []string{"e1", "e2"}
	resolve := func(ref model.EntityRef) []string {
		if ref.Selector == "label==all" {
			return targets
		}
		return nil
	}
	snap := state.Snapshot{
		"e1": state.Entity{ID: "e1", DeviceClass: "media-player"}, // has "power"
		"e2": state.Entity{ID: "e2", DeviceClass: "not-a-class"},  // no vocabulary at all
	}
	sink := &fakeSink{}
	var outcomes []PresetBatchOutcome
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink, Resolve: resolve, Reg: registry.FixtureRegistry{}, Snap: snap, Outcomes: &outcomes}

	a := parseAction(t, `{"type":"device_command","selector":"label==all","command":"power"}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}

	// Only e1's dispatch (a valid command for its device class) should ever
	// reach the sink — e2's is rejected before dispatch (RUL-160).
	if len(sink.calls) != 1 || sink.calls[0].EntityID != "e1" {
		t.Fatalf("sink.calls = %+v, want exactly one call for e1", sink.calls)
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %+v, want 1", outcomes)
	}
	if outcomes[0].Outcome != "partial" {
		t.Errorf("outcome = %q, want partial (one valid target, one vocabulary rejection)", outcomes[0].Outcome)
	}
	for _, r := range outcomes[0].Results {
		if r.Target == "e1" && !r.OK {
			t.Errorf("e1 result = %+v, want OK (valid command)", r)
		}
		if r.Target == "e2" && r.OK {
			t.Errorf("e2 result = %+v, want rejected (command not in device class's vocabulary)", r)
		}
	}
}

func TestRunActionsPresetBatchRejectsInvalidCommandInPresetList(t *testing.T) {
	const presetID = "01J8Z3K4N5P6Q7R8S9T0V1W2Z5"
	snap := state.Snapshot{
		"e1": state.Entity{ID: "e1", DeviceClass: "media-player"}, // "launch" is valid
		"e2": state.Entity{ID: "e2", DeviceClass: "media-player"}, // "reboot" is not
	}
	presets := &fakePresets{byID: map[string][]PresetCommand{
		presetID: {
			{EntityID: "e1", Command: "launch"},
			{EntityID: "e2", Command: "reboot"},
		},
	}}
	sink := &fakeSink{}
	var outcomes []PresetBatchOutcome
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink, Presets: presets, Reg: registry.FixtureRegistry{}, Snap: snap, Outcomes: &outcomes}

	a := parseAction(t, `{"type":"preset_batch","preset_id":"`+presetID+`"}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}

	if len(sink.calls) != 1 || sink.calls[0].EntityID != "e1" {
		t.Fatalf("sink.calls = %+v, want exactly one call for e1 (RUL-160 rejects e2's reboot before dispatch)", sink.calls)
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %+v, want 1", outcomes)
	}
	if outcomes[0].Outcome != "partial" {
		t.Errorf("outcome = %q, want partial", outcomes[0].Outcome)
	}
	for _, r := range outcomes[0].Results {
		if r.Target == "e2" && r.OK {
			t.Errorf("e2 result = %+v, want rejected (reboot not in media-player's vocabulary)", r)
		}
	}
}

// --- RUL-180/181/182: choose ------------------------------------------------

func TestRunActionsChooseFirstMatchingBranchWinsAndStops(t *testing.T) {
	sink := &fakeSink{}
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink, Logs: &logs}
	vars := map[string]any{"pick": "first"}
	ctx.Vars = vars

	a := parseAction(t, `{
		"type":"choose",
		"branches":[
			{"condition":{"type":"variable","variable":"pick","equals":"first"},"actions":[{"type":"log","message":"branch-1"}]},
			{"condition":{"type":"variable","variable":"pick","equals":"first"},"actions":[{"type":"log","message":"branch-2"}]}
		],
		"default":[{"type":"log","message":"default"}]
	}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 1 || logs[0].Message != "branch-1" {
		t.Fatalf("logs = %+v, want exactly [branch-1] (first matching branch only, RUL-181)", logs)
	}
}

func TestRunActionsChooseDefaultWhenNoBranchMatches(t *testing.T) {
	sink := &fakeSink{}
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink, Logs: &logs, Vars: map[string]any{"pick": "neither"}}

	a := parseAction(t, `{
		"type":"choose",
		"branches":[{"condition":{"type":"variable","variable":"pick","equals":"first"},"actions":[{"type":"log","message":"branch-1"}]}],
		"default":[{"type":"log","message":"default"}]
	}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 1 || logs[0].Message != "default" {
		t.Fatalf("logs = %+v, want exactly [default]", logs)
	}
}

func TestRunActionsChooseNoOpWhenNoBranchAndNoDefault(t *testing.T) {
	sink := &fakeSink{}
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Sink: sink, Logs: &logs, Vars: map[string]any{"pick": "neither"}}

	a := parseAction(t, `{
		"type":"choose",
		"branches":[{"condition":{"type":"variable","variable":"pick","equals":"first"},"actions":[{"type":"log","message":"branch-1"}]}]
	}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("logs = %+v, want none (no branch matched, no default)", logs)
	}
}

// --- RUL-190: delay ----------------------------------------------------------

func TestRunActionsDelayYieldsScheduledResumption(t *testing.T) {
	clk := clock.NewFakeClock()
	clk.Advance(1_000)
	var logs []LogEntry
	ctx := ActionContext{Clk: clk, Logs: &logs}

	delayAction := parseAction(t, `{"type":"delay","duration_seconds":20}`)
	afterAction := parseAction(t, `{"type":"log","message":"after delay"}`)

	err := RunActions(ctx, []model.Member{delayAction, afterAction})
	if err == nil {
		t.Fatalf("RunActions with a delay: got nil error, want a *DelayPending")
	}
	var dp *DelayPending
	if !errors.As(err, &dp) {
		t.Fatalf("RunActions error = %v (%T), want *DelayPending", err, err)
	}
	if want := clk.Mono() + 20_000; dp.ResumeAtMono != want {
		t.Errorf("ResumeAtMono = %d, want %d (mono=%d + 20s)", dp.ResumeAtMono, want, clk.Mono())
	}
	if len(dp.RemainingActions) != 1 || dp.RemainingActions[0].Type != "log" {
		t.Fatalf("RemainingActions = %+v, want exactly [log]", dp.RemainingActions)
	}
	// The delay itself must not run anything past it yet (no real sleep).
	if len(logs) != 0 {
		t.Fatalf("logs = %+v, want none until resumption", logs)
	}

	// The engine resumes by re-invoking RunActions with the remaining slice.
	if err := RunActions(ctx, dp.RemainingActions); err != nil {
		t.Fatalf("resumed RunActions: %v", err)
	}
	if len(logs) != 1 || logs[0].Message != "after delay" {
		t.Fatalf("logs after resumption = %+v, want [after delay]", logs)
	}
}

func TestRunActionsDelayZeroIsImmediateNoOp(t *testing.T) {
	clk := clock.NewFakeClock()
	var logs []LogEntry
	ctx := ActionContext{Clk: clk, Logs: &logs}

	delayAction := parseAction(t, `{"type":"delay","duration_seconds":0}`)
	afterAction := parseAction(t, `{"type":"log","message":"immediate"}`)

	if err := RunActions(ctx, []model.Member{delayAction, afterAction}); err != nil {
		t.Fatalf("RunActions: %v (delay:0 must behave as absent, RUL-190)", err)
	}
	if len(logs) != 1 || logs[0].Message != "immediate" {
		t.Fatalf("logs = %+v, want [immediate] run in the same pass", logs)
	}
}

func TestRunActionsDelayInsideChooseMergesOuterRemaining(t *testing.T) {
	clk := clock.NewFakeClock()
	var logs []LogEntry
	ctx := ActionContext{Clk: clk, Logs: &logs, Vars: map[string]any{"pick": "yes"}}

	choose := parseAction(t, `{
		"type":"choose",
		"branches":[{"condition":{"type":"variable","variable":"pick","equals":"yes"},"actions":[
			{"type":"delay","duration_seconds":5},
			{"type":"log","message":"branch-tail"}
		]}]
	}`)
	outer := parseAction(t, `{"type":"log","message":"outer-tail"}`)

	err := RunActions(ctx, []model.Member{choose, outer})
	var dp *DelayPending
	if !errors.As(err, &dp) {
		t.Fatalf("RunActions error = %v (%T), want *DelayPending", err, err)
	}
	if len(dp.RemainingActions) != 2 {
		t.Fatalf("RemainingActions = %+v, want [branch-tail, outer-tail]", dp.RemainingActions)
	}

	if err := RunActions(ctx, dp.RemainingActions); err != nil {
		t.Fatalf("resumed RunActions: %v", err)
	}
	if len(logs) != 2 || logs[0].Message != "branch-tail" || logs[1].Message != "outer-tail" {
		t.Fatalf("logs after resumption = %+v, want [branch-tail, outer-tail] in order", logs)
	}
}

// --- RUL-200: log (literal message this part) -------------------------------

func TestRunActionsLogLiteralMessageDefaultLevel(t *testing.T) {
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Logs: &logs}

	a := parseAction(t, `{"type":"log","message":"hello"}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 1 || logs[0].Message != "hello" || logs[0].Level != "info" {
		t.Fatalf("logs = %+v, want [{info hello}]", logs)
	}
}

func TestRunActionsLogExplicitLevel(t *testing.T) {
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Logs: &logs}

	a := parseAction(t, `{"type":"log","message":"careful","level":"warning"}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 1 || logs[0].Level != "warning" {
		t.Fatalf("logs = %+v, want level=warning", logs)
	}
}

func TestRunActionsLogNonLiteralMessageSkipped(t *testing.T) {
	var logs []LogEntry
	ctx := ActionContext{Clk: clock.NewFakeClock(), Logs: &logs}

	// An Expression-shaped message ({"expr": ...}) is Part 2b's concern; this
	// part supports only a literal string message, and fails closed (no log
	// entry fabricated) rather than coercing the object into a string.
	a := parseAction(t, `{"type":"log","message":{"expr":"state('x')"}}`)
	if err := RunActions(ctx, []model.Member{a}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("logs = %+v, want none for a non-literal message this part", logs)
	}
}
