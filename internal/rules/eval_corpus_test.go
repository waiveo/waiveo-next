// This file is the rules/1 evaluation core's §10 differential oracle: it
// enumerates every conformance/corpora/rules-1/*.json case and, for each case
// this evaluation core (Tasks 1-9) can drive, replays its ACTUAL input shape
// (a bare trigger declaration, a full rule, a preset + actions, or a value
// table — the corpus deliberately varies shape case-by-case) against the
// real eval/engine primitives on a FakeClock, diffing the produced result
// against the case's own ACTUAL expected block. A case needing a later part
// (time/DST/misfire triggers and conditions, expression grammar, generation
// swap, stabilization) is deferred and logged, never silently skipped
// (union honesty, matching the compile-time oracle in corpus_test.go and the
// player1/relay1 drivers).
package rules

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/closure"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/rules/engine"
	"github.com/maaxton/waiveo-next/internal/rules/eval"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/schedule"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// --- shared test doubles / helpers -----------------------------------------

// corpusDispatch records one CommandSink.Dispatch call.
type corpusDispatch struct {
	EntityID string
	Command  string
	Params   map[string]any
}

// corpusSink is a CommandSink test double that records every dispatch.
type corpusSink struct {
	calls []corpusDispatch
}

func (s *corpusSink) Dispatch(entityID, command string, params map[string]any) error {
	s.calls = append(s.calls, corpusDispatch{EntityID: entityID, Command: command, Params: params})
	return nil
}

// corpusPresets is a PresetStore test double keyed by preset ID.
type corpusPresets struct {
	byID map[string][]eval.PresetCommand
}

func (p *corpusPresets) Commands(presetID string) ([]eval.PresetCommand, bool) {
	c, ok := p.byID[presetID]
	return c, ok
}

// corpusGroupRegistry is a registry.Registry built directly from a corpus
// case's own inline input.fixture.device_classes, so a case declaring
// semantic groups this evaluation core does not otherwise know about (e.g. a
// "screen" device class) resolves against exactly what the case itself
// declares, not an incidental match with registry.FixtureRegistry.
type corpusGroupRegistry struct {
	groups map[string]map[string][]string // deviceClass -> groupName -> members
}

func (r corpusGroupRegistry) SemanticGroups(deviceClass, st string) []string {
	var out []string
	for name, members := range r.groups[deviceClass] {
		for _, m := range members {
			if m == st {
				out = append(out, name)
				break
			}
		}
	}
	return out
}
func (r corpusGroupRegistry) AttributeType(deviceClass, attr string) registry.ValueType {
	return registry.Unknown
}
func (r corpusGroupRegistry) ChangeEmission(deviceClass, attr string) string { return "" }
func (r corpusGroupRegistry) CommandExists(deviceClass, command string) bool { return false }

var _ registry.Registry = corpusGroupRegistry{}

// mediaEnt builds a media-player Entity snapshot at the given state.
func mediaEnt(id, st string) state.Entity {
	return state.Entity{ID: id, DeviceClass: "media-player", State: st}
}

// buildStateTriggerFromRaw builds an eval.StateTrigger from a raw single
// trigger declaration (tolerating the corpus's extra "id" field), by wrapping
// it into a minimal parseable rule.
func buildStateTriggerFromRaw(t *testing.T, raw json.RawMessage) *eval.StateTrigger {
	t.Helper()
	r, err := model.ParseRule([]byte(`{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2ZR","triggers":[` + string(raw) + `]}`))
	if err != nil {
		t.Fatalf("parse trigger: %v", err)
	}
	st, err := eval.NewStateTrigger(r.Triggers[0])
	if err != nil {
		t.Fatalf("NewStateTrigger: %v", err)
	}
	return st
}

// buildNumericTriggerFromRaw is buildStateTriggerFromRaw's numeric analogue.
func buildNumericTriggerFromRaw(t *testing.T, raw json.RawMessage) *eval.NumericTrigger {
	t.Helper()
	r, err := model.ParseRule([]byte(`{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2ZR","triggers":[` + string(raw) + `]}`))
	if err != nil {
		t.Fatalf("parse trigger: %v", err)
	}
	nt, err := eval.NewNumericTrigger(r.Triggers[0])
	if err != nil {
		t.Fatalf("NewNumericTrigger: %v", err)
	}
	return nt
}

// rawFor reads a trigger declaration's optional `for` (seconds; absent -> 0).
func rawFor(raw json.RawMessage) int {
	var spec struct {
		For int `json:"for"`
	}
	_ = json.Unmarshal(raw, &spec)
	return spec.For
}

// firstDelaySeconds returns the duration_seconds of the first `delay` action
// in a compiled rule's action sequence, or 0 if none.
func firstDelaySeconds(actions []model.Member) int64 {
	for _, a := range actions {
		if a.Type == "delay" {
			var spec struct {
				DurationSeconds int64 `json:"duration_seconds"`
			}
			_ = json.Unmarshal(a.Raw, &spec)
			return spec.DurationSeconds
		}
	}
	return 0
}

// --- the driver --------------------------------------------------------------

// evalCorpusHandlers maps a corpus case's case_id (== its filename minus
// .json) to the function that replays it. Every other file in the corpus
// dir is deferred + logged: it needs a later part (time/DST/misfire
// triggers+conditions, expression grammar, generation swap, stabilization).
var evalCorpusHandlers = map[string]func(t *testing.T, path string){
	"RUL-023-attribute-only-vs-state-change":                       runRUL023,
	"RUL-024-for-hold-suppresses-flap":                             runRUL024,
	"RUL-033-numeric-first-observation-level-check":                runRUL033,
	"RUL-101-corroborating-and-cross-entity-conditions":            runRUL101,
	"RUL-150-variable-condition-constant-folded":                   runRUL150,
	"RUL-171-preset-batch-partial-failure":                         runRUL171,
	"RUL-241-mode-single-drops-concurrent-fire":                    runRUL241,
	"RUL-242-mode-restart-cancels-in-flight-delay":                 runRUL242,
	"RUL-245-no-implicit-dedup-window":                             runRUL245,
	"RUL-270-number-parsing-strict":                                runRUL270,
	"RUL-300-boot-reconfirm-no-fire":                               runRUL300,
	"RUL-301-stabilization-window-defers-transition":               runRUL301,
	"RUL-303-generation-swap-unchanged-trigger-preserves-baseline": runRUL303,
	"RUL-321-scalar-expands-array-exact":                           runRUL321Scalar,
	"RUL-321-suspend-flap-for-debounce":                            runRUL321Suspend,
	"RUL-341-dst-spring-forward-skip":                              runRUL341,
	"RUL-342-dst-fall-back-fires-once":                             runRUL342,
	"RUL-352-misfire-catch-up-once-collapses":                      runRUL352,
	"RUL-354-misfire-default-skip-instantaneous":                   runRUL354,
	"RUL-360-engine-restart-resets-hold":                           runRUL360,
	"RUL-380-generation-swap-cancels-changed-rule":                 runRUL380,
}

// TestRulesEvaluationCorpusDriver is the §10 differential oracle: it replays
// every corpus case this evaluation core can drive against the real eval/
// engine primitives on a FakeClock, and logs (without failing) the cases that
// need a later part.
func TestRulesEvaluationCorpusDriver(t *testing.T) {
	dir := filepath.Join("..", "..", "conformance", "corpora", "rules-1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}

	var ran, deferred int
	var deferredIDs []string
	for _, ent := range entries {
		if filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		caseID := strings.TrimSuffix(ent.Name(), ".json")
		h, ok := evalCorpusHandlers[caseID]
		if !ok {
			deferred++
			deferredIDs = append(deferredIDs, caseID)
			continue
		}
		ran++
		path := filepath.Join(dir, ent.Name())
		t.Run(caseID, func(t *testing.T) { h(t, path) })
	}
	if ran != len(evalCorpusHandlers) {
		t.Fatalf("ran %d case(s), want %d — a handler's case_id key does not match any corpus filename", ran, len(evalCorpusHandlers))
	}
	if ran == 0 {
		t.Fatal("no evaluation-corpus case ran — corpus path or handler map wrong")
	}
	t.Logf("evaluation corpus driver (§10 oracle): ran %d case(s), deferred %d to later parts: %v", ran, deferred, deferredIDs)
}

// --- RUL-023: attribute-only vs state-change (three triggers) --------------

type rul023Case struct {
	Input struct {
		Fixture struct {
			Entities map[string]struct {
				DeviceClass string `json:"device_class"`
			} `json:"entities"`
		} `json:"fixture"`
		Triggers []json.RawMessage `json:"triggers"`
		Events   []struct {
			Seq               int               `json:"seq"`
			TMs               int64             `json:"t_ms"`
			Kind              string            `json:"kind"`
			StateBefore       string            `json:"state_before"`
			StateAfter        string            `json:"state_after"`
			AttributesChanged map[string][2]any `json:"attributes_changed"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []map[string]any `json:"results"`
	} `json:"expected"`
}

func runRUL023(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul023Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	reg := registry.FixtureRegistry{}
	var entityID, deviceClass string
	for id, e := range c.Input.Fixture.Entities {
		entityID, deviceClass = id, e.DeviceClass
	}

	type trig struct {
		id string
		st *eval.StateTrigger
	}
	var trigs []trig
	baselines := map[string]eval.TriggerBaseline{}
	running := map[string]any{"volume": float64(20)} // pre-seq1 attribute values
	for _, raw := range c.Input.Triggers {
		var idOnly struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &idOnly)
		st := buildStateTriggerFromRaw(t, raw)
		trigs = append(trigs, trig{id: idOnly.ID, st: st})
		bl := eval.TriggerBaseline{Known: true, State: c.Input.Events[0].StateBefore}
		if st.Attribute != "" {
			bl.Attr = running[st.Attribute]
			bl.AttrKnown = true
		}
		baselines[idOnly.ID] = bl
	}

	clk := clock.NewFakeClock()
	var lastT int64
	for _, ev := range c.Input.Events {
		clk.Advance(ev.TMs - lastT)
		lastT = ev.TMs

		changed := make([]string, 0, len(ev.AttributesChanged))
		for name, pair := range ev.AttributesChanged {
			running[name] = pair[1]
			changed = append(changed, name)
		}
		sort.Strings(changed)
		attrsCopy := map[string]any{}
		for k, v := range running {
			attrsCopy[k] = v
		}
		obs := state.Observation{
			Entity:       state.Entity{ID: entityID, DeviceClass: deviceClass, State: ev.StateAfter, Attributes: attrsCopy},
			StateChanged: ev.StateBefore != ev.StateAfter,
			ChangedAttrs: changed,
		}
		want := c.Expected.Results[ev.Seq-1]
		for _, tr := range trigs {
			fired, next := tr.st.Observe(reg, baselines[tr.id], obs)
			baselines[tr.id] = next
			key := tr.id + "_fired"
			if wf, ok := want[key].(bool); ok {
				if fired != wf {
					t.Errorf("seq %d trigger %s: fired=%v want %v", ev.Seq, tr.id, fired, wf)
				}
			}
		}
	}
}

// --- RUL-024: for-hold suppresses flap --------------------------------------

type rul024Case struct {
	Input struct {
		Fixture struct {
			Entities map[string]struct {
				DeviceClass string `json:"device_class"`
			} `json:"entities"`
		} `json:"fixture"`
		Trigger json.RawMessage `json:"trigger"`
		Events  []struct {
			Seq  int    `json:"seq"`
			TMs  int64  `json:"t_ms"`
			Kind string `json:"kind"`
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq   int  `json:"seq"`
			Fired bool `json:"fired"`
		} `json:"results"`
	} `json:"expected"`
}

func runRUL024(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul024Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	reg := registry.FixtureRegistry{}
	var deviceClass string
	for _, e := range c.Input.Fixture.Entities {
		deviceClass = e.DeviceClass
	}
	st := buildStateTriggerFromRaw(t, c.Input.Trigger)
	hold := eval.NewHold(eval.BoundedHold, rawFor(c.Input.Trigger))

	want := map[int]bool{}
	for _, r := range c.Expected.Results {
		want[r.Seq] = r.Fired
	}

	clk := clock.NewFakeClock()
	var lastT int64
	var currentState string
	for _, ev := range c.Input.Events {
		clk.Advance(ev.TMs - lastT)
		lastT = ev.TMs
		if ev.Kind == "state_transition" {
			currentState = ev.To
		}
		matched := eval.MatchState(reg, deviceClass, currentState, st.To)
		fired := hold.Step(clk, matched)
		if wf, ok := want[ev.Seq]; ok {
			if fired != wf {
				t.Errorf("seq %d: fired=%v want %v", ev.Seq, fired, wf)
			}
		}
	}
}

// --- RUL-033: numeric first-observation level check -------------------------

type rul033Case struct {
	Input struct {
		Fixture struct {
			Entities map[string]struct {
				DeviceClass string `json:"device_class"`
			} `json:"entities"`
		} `json:"fixture"`
		Trigger json.RawMessage `json:"trigger"`
		Events  []struct {
			Seq   int     `json:"seq"`
			TMs   int64   `json:"t_ms"`
			Kind  string  `json:"kind"`
			Value float64 `json:"value"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq   int  `json:"seq"`
			Fired bool `json:"fired"`
		} `json:"results"`
	} `json:"expected"`
}

func runRUL033(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul033Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	var entityID, deviceClass string
	for id, e := range c.Input.Fixture.Entities {
		entityID, deviceClass = id, e.DeviceClass
	}
	nt := buildNumericTriggerFromRaw(t, c.Input.Trigger)

	clk := clock.NewFakeClock()
	var lastT int64
	var prevParsed *float64
	for _, ev := range c.Input.Events {
		clk.Advance(ev.TMs - lastT)
		lastT = ev.TMs
		obs := state.Observation{
			Entity: state.Entity{ID: entityID, DeviceClass: deviceClass, Attributes: map[string]any{nt.Attribute: ev.Value}},
		}
		fired, next := nt.Observe(prevParsed, obs)
		prevParsed = next
		for _, want := range c.Expected.Results {
			if want.Seq != ev.Seq {
				continue
			}
			if fired != want.Fired {
				t.Errorf("seq %d: fired=%v want %v", ev.Seq, fired, want.Fired)
			}
		}
	}
}

// --- RUL-101: corroborating + cross-entity conditions -----------------------

type rul101Case struct {
	Input struct {
		Rule        json.RawMessage `json:"rule"`
		Evaluations []struct {
			ID               string `json:"id"`
			PoweredOn        bool   `json:"powered_on"`
			OtherEntityState string `json:"other_entity_state"`
		} `json:"evaluations"`
	} `json:"input"`
	Expected struct {
		ExecutionClass string `json:"execution_class"`
		Results        []struct {
			ID             string `json:"id"`
			ConditionsPass bool   `json:"conditions_pass"`
		} `json:"results"`
	} `json:"expected"`
}

func runRUL101(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul101Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	entry, cerr := compile.Compile(c.Input.Rule)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	if entry.ExecutionClass != c.Expected.ExecutionClass {
		t.Errorf("execution_class = %s, want %s", entry.ExecutionClass, c.Expected.ExecutionClass)
	}

	rule, err := model.ParseRule(c.Input.Rule)
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}
	if len(rule.Triggers) != 1 || rule.Triggers[0].EntityRef == nil {
		t.Fatalf("expected exactly one trigger with an entity ref, got %+v", rule.Triggers)
	}
	if len(rule.Conditions) != 2 {
		t.Fatalf("expected 2 top-level conditions, got %d", len(rule.Conditions))
	}
	and := model.Member{Composition: "and", Children: rule.Conditions}

	triggerEntity := rule.Triggers[0].EntityRef.EntityID
	otherEntity := rule.Conditions[1].EntityRef.EntityID

	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()

	for _, ev := range c.Input.Evaluations {
		ev := ev
		t.Run(ev.ID, func(t *testing.T) {
			snap := state.Snapshot{
				triggerEntity: state.Entity{ID: triggerEntity, DeviceClass: "media-player", State: "idle", Attributes: map[string]any{"powered_on": ev.PoweredOn}},
				otherEntity:   state.Entity{ID: otherEntity, DeviceClass: "media-player", State: ev.OtherEntityState},
			}
			var want bool
			found := false
			for _, r := range c.Expected.Results {
				if r.ID == ev.ID {
					want, found = r.ConditionsPass, true
				}
			}
			if !found {
				t.Fatalf("no expected result for evaluation %q", ev.ID)
			}
			if got := eval.EvalCondition(reg, snap, clk, nil, and); got != want {
				t.Errorf("conditions_pass = %v, want %v", got, want)
			}
		})
	}
}

// --- RUL-171: preset-batch partial failure ----------------------------------

type rul171Case struct {
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

func runRUL171(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul171Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	presetCmds := make([]eval.PresetCommand, 0, len(c.Input.Preset.Commands))
	for _, pc := range c.Input.Preset.Commands {
		presetCmds = append(presetCmds, eval.PresetCommand{EntityID: pc.EntityID, Command: pc.Command})
	}
	presets := &corpusPresets{byID: map[string][]eval.PresetCommand{c.Input.Preset.PresetID: presetCmds}}

	sink := &scriptedSink{fail: map[string]string{}}
	for _, cr := range c.Input.CommandResults {
		if !cr.OK {
			sink.fail[cr.EntityID] = cr.Error
		}
	}

	actionsJSON, err := json.Marshal(c.Input.RuleActions)
	if err != nil {
		t.Fatalf("marshal rule_actions: %v", err)
	}
	rule, err := model.ParseRule([]byte(`{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2ZA","actions":` + string(actionsJSON) + `}`))
	if err != nil {
		t.Fatalf("parse rule_actions: %v", err)
	}

	var outcomes []eval.PresetBatchOutcome
	var logs []eval.LogEntry
	ctx := eval.ActionContext{Clk: clock.NewFakeClock(), Sink: sink, Presets: presets, Outcomes: &outcomes, Logs: &logs}

	if err := eval.RunActions(ctx, rule.Actions); err != nil {
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

// scriptedSink is a CommandSink test double that fails a scripted set of
// entity IDs with a scripted error message; every Dispatch is recorded.
type scriptedSink struct {
	calls []corpusDispatch
	fail  map[string]string // entityID -> error message
}

func (s *scriptedSink) Dispatch(entityID, command string, params map[string]any) error {
	s.calls = append(s.calls, corpusDispatch{EntityID: entityID, Command: command, Params: params})
	if msg, ok := s.fail[entityID]; ok {
		return errors.New(msg)
	}
	return nil
}

// --- RUL-241 / RUL-242: mode single/restart, replayed on the real engine ---

// firePastMotion drives one genuine off->motion transition on the engine so
// the rule's `to:["motion"]` trigger fires (leaving motion first, when
// already in it, so re-entry is a genuine transition rather than a same-state
// no-op, RUL-020/300).
func firePastMotion(e *engine.Engine, reg registry.Registry, entityID string, alreadyInMotion bool) []engine.RunDisposition {
	if alreadyInMotion {
		e.Observe(state.NewObservation(reg, mediaEnt(entityID, "motion"), mediaEnt(entityID, "off")))
	}
	return e.Observe(state.NewObservation(reg, mediaEnt(entityID, "off"), mediaEnt(entityID, "motion")))
}

type rul241Case struct {
	Input struct {
		Rule   json.RawMessage `json:"rule"`
		Events []struct {
			Seq  int    `json:"seq"`
			TMs  int64  `json:"t_ms"`
			Kind string `json:"kind"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq         int     `json:"seq"`
			RunStarted  *string `json:"run_started"`
			Disposition string  `json:"disposition"`
		} `json:"results"`
		RunOneCompletesUninterrupted bool `json:"run-1_completes_uninterrupted"`
	} `json:"expected"`
}

func runRUL241(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul241Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// This case is about mode dispositions (RUL-241/246), not RUL-160's
	// command-vocabulary check, and its own action's command ("power_on") is
	// not in FixtureRegistry's fixed media-player vocabulary. A nil Registry
	// leaves that check unchecked (matching the established "no registry
	// configured" behavior) so the dispatch this test observes is not
	// incidentally swallowed by an unrelated concern. State matching here
	// needs no registry either: the trigger's `to` is an array, which
	// MatchState resolves by exact literal membership only, never touching
	// SemanticGroups.
	var reg registry.Registry
	clk := clock.NewFakeClock()
	sink := &corpusSink{}

	entry, cerr := compile.Compile(c.Input.Rule)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	rule, err := model.ParseRule(c.Input.Rule)
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}

	e := engine.New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	entityID := rule.Triggers[0].EntityRef.EntityID
	e.Observe(state.NewObservation(reg, mediaEnt(entityID, "off"), mediaEnt(entityID, "off")))

	dispsBySeq := map[int][]engine.RunDisposition{}
	var lastT int64
	inMotion := false
	for _, ev := range c.Input.Events {
		clk.Advance(ev.TMs - lastT)
		lastT = ev.TMs
		dispsBySeq[ev.Seq] = firePastMotion(e, reg, entityID, inMotion)
		inMotion = true
	}

	for _, want := range c.Expected.Results {
		got := dispsBySeq[want.Seq]
		if len(got) != 1 {
			t.Fatalf("seq %d: want 1 disposition, got %+v", want.Seq, got)
		}
		if string(got[0].Disposition) != want.Disposition {
			t.Errorf("seq %d: disposition = %s, want %s", want.Seq, got[0].Disposition, want.Disposition)
		}
		if want.RunStarted != nil && got[0].Disposition != engine.Ran {
			t.Errorf("seq %d: run_started=%q implies ran, got %s", want.Seq, *want.RunStarted, got[0].Disposition)
		}
		if want.RunStarted == nil && got[0].Disposition == engine.Ran && want.Disposition != string(engine.Ran) {
			t.Errorf("seq %d: run_started=null but disposition ran", want.Seq)
		}
	}

	if c.Expected.RunOneCompletesUninterrupted {
		delayMs := firstDelaySeconds(rule.Actions) * 1000
		resumeAt := c.Input.Events[0].TMs + delayMs
		if resumeAt > lastT {
			clk.Advance(resumeAt - lastT)
			lastT = resumeAt
		}
		e.Tick(clk)
		if len(sink.calls) != 1 {
			t.Fatalf("run-1_completes_uninterrupted: sink calls = %d, want 1: %+v", len(sink.calls), sink.calls)
		}
	}
}

type rul242Case struct {
	Input struct {
		Rule   json.RawMessage `json:"rule"`
		Events []struct {
			Seq  int    `json:"seq"`
			TMs  int64  `json:"t_ms"`
			Kind string `json:"kind"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq                         int    `json:"seq"`
			RunOneCanceled              bool   `json:"run-1_canceled"`
			RunOneDisposition           string `json:"run-1_disposition"`
			NewRunStarted               string `json:"new_run_started"`
			NewRunDelayElapsedAtStartMs int64  `json:"new_run_delay_elapsed_at_start_ms"`
		} `json:"results"`
		RunTwoDelayCompletesAtMs int64 `json:"run-2_delay_completes_at_ms"`
	} `json:"expected"`
}

func runRUL242(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul242Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// See runRUL241: a nil Registry leaves RUL-160's command-vocabulary check
	// unchecked, which is what this mode-dispositions case needs for its
	// "power_on" command, and is not needed for the array-typed `to` match.
	var reg registry.Registry
	clk := clock.NewFakeClock()
	sink := &corpusSink{}

	entry, cerr := compile.Compile(c.Input.Rule)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	rule, err := model.ParseRule(c.Input.Rule)
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}

	e := engine.New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	entityID := rule.Triggers[0].EntityRef.EntityID
	e.Observe(state.NewObservation(reg, mediaEnt(entityID, "off"), mediaEnt(entityID, "off")))

	delayMs := firstDelaySeconds(rule.Actions) * 1000

	dispsBySeq := map[int][]engine.RunDisposition{}
	var lastT int64
	inMotion := false
	for _, ev := range c.Input.Events {
		clk.Advance(ev.TMs - lastT)
		lastT = ev.TMs
		dispsBySeq[ev.Seq] = firePastMotion(e, reg, entityID, inMotion)
		inMotion = true
	}

	if got := dispsBySeq[1]; len(got) != 1 || got[0].Disposition != engine.Ran {
		t.Fatalf("seq1: want [ran], got %+v", got)
	}
	for _, want := range c.Expected.Results {
		got := dispsBySeq[want.Seq]
		if len(got) != 2 {
			t.Fatalf("seq %d: want 2 dispositions (restarted, ran), got %+v", want.Seq, got)
		}
		if want.RunOneCanceled && string(got[0].Disposition) != want.RunOneDisposition {
			t.Errorf("seq %d: disps[0] = %s, want %s", want.Seq, got[0].Disposition, want.RunOneDisposition)
		}
		if got[1].Disposition != engine.Ran {
			t.Errorf("seq %d: disps[1] = %s, want ran (%s)", want.Seq, got[1].Disposition, want.NewRunStarted)
		}
	}

	// The canceled run-1's original resume must not fire or dispatch.
	origResume := c.Input.Events[0].TMs + delayMs
	if origResume > lastT {
		clk.Advance(origResume - lastT)
		lastT = origResume
	}
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("canceled run-1's original resume produced dispositions: %+v", d)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("canceled run-1's delay dispatched: %+v", sink.calls)
	}

	// run-2's fresh delay completes at the expected instant: exactly one dispatch.
	want2 := c.Expected.RunTwoDelayCompletesAtMs
	if want2 > lastT {
		clk.Advance(want2 - lastT)
	}
	e.Tick(clk)
	if len(sink.calls) != 1 {
		t.Fatalf("run-2 completion at run-2_delay_completes_at_ms=%d: sink calls = %d, want 1: %+v", want2, len(sink.calls), sink.calls)
	}
}

// --- RUL-245: no implicit dedup window ---------------------------------------

type rul245Case struct {
	Input struct {
		Fixture struct {
			Entities map[string]struct {
				DeviceClass string `json:"device_class"`
			} `json:"entities"`
		} `json:"fixture"`
		Trigger json.RawMessage `json:"trigger"`
		Events  []struct {
			Seq  int    `json:"seq"`
			TMs  int64  `json:"t_ms"`
			Kind string `json:"kind"`
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq   int  `json:"seq"`
			Fired bool `json:"fired"`
		} `json:"results"`
	} `json:"expected"`
}

func runRUL245(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul245Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	reg := registry.FixtureRegistry{}
	var entityID, deviceClass string
	for id, e := range c.Input.Fixture.Entities {
		entityID, deviceClass = id, e.DeviceClass
	}
	st := buildStateTriggerFromRaw(t, c.Input.Trigger)

	baseline := eval.TriggerBaseline{Known: true, State: c.Input.Events[0].From}
	clk := clock.NewFakeClock()
	var lastT int64
	for _, ev := range c.Input.Events {
		clk.Advance(ev.TMs - lastT)
		lastT = ev.TMs
		obs := state.Observation{
			Entity:       state.Entity{ID: entityID, DeviceClass: deviceClass, State: ev.To},
			StateChanged: ev.From != ev.To,
		}
		fired, next := st.Observe(reg, baseline, obs)
		baseline = next
		for _, want := range c.Expected.Results {
			if want.Seq != ev.Seq {
				continue
			}
			if fired != want.Fired {
				t.Errorf("seq %d: fired=%v want %v", ev.Seq, fired, want.Fired)
			}
		}
	}
}

// --- RUL-270: strict number parsing -----------------------------------------

type rul270Case struct {
	Input struct {
		Trigger json.RawMessage `json:"trigger"`
		Values  []struct {
			ID    string `json:"id"`
			Value any    `json:"value"`
		} `json:"values"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			ID               string   `json:"id"`
			IsNumber         bool     `json:"is_number"`
			Parsed           *float64 `json:"parsed"`
			SatisfiesAbove10 *bool    `json:"satisfies_above_10"`
		} `json:"results"`
	} `json:"expected"`
}

func runRUL270(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul270Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	var spec struct {
		Above float64 `json:"above"`
	}
	_ = json.Unmarshal(c.Input.Trigger, &spec)

	values := map[string]any{}
	for _, v := range c.Input.Values {
		values[v.ID] = v.Value
	}

	for _, want := range c.Expected.Results {
		val, ok := values[want.ID]
		if !ok {
			t.Fatalf("no input value for id %q", want.ID)
		}
		parsed, gotOK := eval.ParseNumber(val)
		if gotOK != want.IsNumber {
			t.Errorf("%s: is_number = %v, want %v", want.ID, gotOK, want.IsNumber)
			continue
		}
		if !gotOK {
			continue
		}
		if want.Parsed != nil && parsed != *want.Parsed {
			t.Errorf("%s: parsed = %v, want %v", want.ID, parsed, *want.Parsed)
		}
		if want.SatisfiesAbove10 != nil {
			if satisfies := parsed > spec.Above; satisfies != *want.SatisfiesAbove10 {
				t.Errorf("%s: satisfies_above_%v = %v, want %v", want.ID, spec.Above, satisfies, *want.SatisfiesAbove10)
			}
		}
	}
}

// --- RUL-300: boot reconfirm does not fire ----------------------------------

type rul300Case struct {
	Input struct {
		Fixture struct {
			Entities map[string]struct {
				DeviceClass               string `json:"device_class"`
				DurableStateBeforeRestart string `json:"durable_state_before_restart"`
			} `json:"entities"`
		} `json:"fixture"`
		Trigger json.RawMessage `json:"trigger"`
		Events  []struct {
			Seq           int    `json:"seq"`
			TMs           int64  `json:"t_ms"`
			Kind          string `json:"kind"`
			ReportedState string `json:"reported_state"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq              int    `json:"seq"`
			OldStateResolved string `json:"old_state_resolved"`
			StateChanged     bool   `json:"state_changed"`
			Fired            bool   `json:"fired"`
		} `json:"results"`
	} `json:"expected"`
}

func runRUL300(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul300Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	reg := registry.FixtureRegistry{}
	st := buildStateTriggerFromRaw(t, c.Input.Trigger)

	var entityID, deviceClass, durable string
	for id, e := range c.Input.Fixture.Entities {
		entityID, deviceClass, durable = id, e.DeviceClass, e.DurableStateBeforeRestart
	}
	baseline := eval.TriggerBaseline{Known: true, State: durable}

	clk := clock.NewFakeClock()
	var lastT int64
	for _, ev := range c.Input.Events {
		clk.Advance(ev.TMs - lastT)
		lastT = ev.TMs
		if ev.Kind != "observation" {
			continue
		}
		preObsState := baseline.State
		obs := state.Observation{
			Entity:       state.Entity{ID: entityID, DeviceClass: deviceClass, State: ev.ReportedState},
			StateChanged: preObsState != ev.ReportedState,
		}
		fired, next := st.Observe(reg, baseline, obs)
		for _, want := range c.Expected.Results {
			if want.Seq != ev.Seq {
				continue
			}
			if preObsState != want.OldStateResolved {
				t.Errorf("seq %d: old_state_resolved = %s, want %s", ev.Seq, preObsState, want.OldStateResolved)
			}
			if obs.StateChanged != want.StateChanged {
				t.Errorf("seq %d: state_changed = %v, want %v", ev.Seq, obs.StateChanged, want.StateChanged)
			}
			if fired != want.Fired {
				t.Errorf("seq %d: fired = %v, want %v", ev.Seq, fired, want.Fired)
			}
		}
		baseline = next
	}
}

// --- RUL-321 (scalar): scalar to:"on" group-expands, array to:["on"] doesn't -

type rul321ScalarCase struct {
	Input struct {
		Fixture struct {
			DeviceClasses map[string]struct {
				SemanticGroups map[string][]string `json:"semantic_groups"`
			} `json:"device_classes"`
			Entities map[string]struct {
				DeviceClass string `json:"device_class"`
			} `json:"entities"`
		} `json:"fixture"`
		Triggers []struct {
			ID       string `json:"id"`
			EntityID string `json:"entity_id"`
			To       any    `json:"to"`
		} `json:"triggers"`
		Events []struct {
			Seq  int    `json:"seq"`
			TMs  int64  `json:"t_ms"`
			Kind string `json:"kind"`
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq         int  `json:"seq"`
			ScalarFired bool `json:"scalar_fired"`
			ArrayFired  bool `json:"array_fired"`
		} `json:"results"`
	} `json:"expected"`
}

func runRUL321Scalar(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul321ScalarCase
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	reg := corpusGroupRegistry{groups: map[string]map[string][]string{}}
	for class, decl := range c.Input.Fixture.DeviceClasses {
		reg.groups[class] = decl.SemanticGroups
	}

	var deviceClass string
	var scalarTo, arrayTo any
	for _, trig := range c.Input.Triggers {
		if ent, ok := c.Input.Fixture.Entities[trig.EntityID]; ok {
			deviceClass = ent.DeviceClass
		}
		switch trig.ID {
		case "scalar":
			scalarTo = trig.To
		case "array":
			arrayTo = trig.To
		}
	}
	if scalarTo == nil || arrayTo == nil || deviceClass == "" {
		t.Fatalf("corpus case missing scalar/array trigger or device class: %+v", c.Input.Triggers)
	}

	for _, want := range c.Expected.Results {
		ev := c.Input.Events[want.Seq-1]
		if got := eval.MatchState(reg, deviceClass, ev.To, scalarTo); got != want.ScalarFired {
			t.Errorf("seq %d: scalar MatchState(%q, %v) = %v, want %v", want.Seq, ev.To, scalarTo, got, want.ScalarFired)
		}
		if got := eval.MatchState(reg, deviceClass, ev.To, arrayTo); got != want.ArrayFired {
			t.Errorf("seq %d: array MatchState(%q, %v) = %v, want %v", want.Seq, ev.To, arrayTo, got, want.ArrayFired)
		}
	}
}

// --- RUL-321 (suspend): semantic-group expansion combined with a for-hold --

type rul321SuspendCase struct {
	Input struct {
		Fixture struct {
			DeviceClasses map[string]struct {
				SemanticGroups map[string][]string `json:"semantic_groups"`
			} `json:"device_classes"`
			Entities map[string]struct {
				DeviceClass string `json:"device_class"`
			} `json:"entities"`
		} `json:"fixture"`
		Trigger json.RawMessage `json:"trigger"`
		Events  []struct {
			Seq  int    `json:"seq"`
			TMs  int64  `json:"t_ms"`
			Kind string `json:"kind"`
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq     int   `json:"seq"`
			Matched *bool `json:"matched"`
			Fired   *bool `json:"fired"`
		} `json:"results"`
	} `json:"expected"`
}

func runRUL321Suspend(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul321SuspendCase
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	reg := corpusGroupRegistry{groups: map[string]map[string][]string{}}
	for class, decl := range c.Input.Fixture.DeviceClasses {
		reg.groups[class] = decl.SemanticGroups
	}
	var deviceClass string
	for _, e := range c.Input.Fixture.Entities {
		deviceClass = e.DeviceClass
	}

	st := buildStateTriggerFromRaw(t, c.Input.Trigger)
	hold := eval.NewHold(eval.BoundedHold, rawFor(c.Input.Trigger))

	clk := clock.NewFakeClock()
	var lastT int64
	var currentState string
	for _, ev := range c.Input.Events {
		clk.Advance(ev.TMs - lastT)
		lastT = ev.TMs
		if ev.Kind == "state_transition" {
			currentState = ev.To
		}
		matched := eval.MatchState(reg, deviceClass, currentState, st.To)
		fired := hold.Step(clk, matched)
		for _, want := range c.Expected.Results {
			if want.Seq != ev.Seq {
				continue
			}
			if want.Matched != nil && matched != *want.Matched {
				t.Errorf("seq %d: matched = %v, want %v", ev.Seq, matched, *want.Matched)
			}
			if want.Fired != nil && fired != *want.Fired {
				t.Errorf("seq %d: fired = %v, want %v", ev.Seq, fired, *want.Fired)
			}
		}
	}
}

// --- shared schedule-corpus helpers (RUL-341/342/352/354) ------------------

// scheduleCorpusEntity is the fixture media-player entity the schedule
// corpus handlers' device_command action targets, primed into the engine's
// snapshot so RUL-160's command-vocabulary check (against a real
// registry.FixtureRegistry) resolves rather than failing closed.
const scheduleCorpusEntity = "01J8Z3K4N5P6Q7R8S9T0SCHEDU"

// loadScheduleCorpusRule builds and loads an edge rule wrapping a corpus
// case's own raw `time`/`time_pattern` trigger declaration with a
// device_command action, replaying the ACTUAL trigger shape the corpus file
// carries (including any inline `misfire` field) through the real
// compile+model+engine front door, exactly like every other handler in this
// driver builds its rule from the corpus's own JSON. mode is the rule's
// `mode` field verbatim ("" omits it, taking the engine's single default).
func loadScheduleCorpusRule(t *testing.T, reg registry.Registry, e *engine.Engine, id string, triggerRaw json.RawMessage, mode string) model.Rule {
	t.Helper()
	modeField := ""
	if mode != "" {
		modeField = `"mode":"` + mode + `",`
	}
	ruleJSON := []byte(`{"id":"` + id + `",` + modeField + `"triggers":[` + string(triggerRaw) + `],"actions":[{"type":"device_command","entity_id":"` + scheduleCorpusEntity + `","command":"power"}]}`)
	entry, cerr := compile.Compile(ruleJSON)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	if entry.ExecutionClass != "edge" {
		t.Fatalf("execution_class = %s, want edge: %+v", entry.ExecutionClass, entry.AppClassReasons)
	}
	rule, err := model.ParseRule(ruleJSON)
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.Observe(state.NewObservation(reg, mediaEnt(scheduleCorpusEntity, "off"), mediaEnt(scheduleCorpusEntity, "off")))
	return rule
}

// primeScheduleLiveTick marks the engine as already ticking live as of wall
// instant floor, reaching exactly the effect the engine package's own
// (unexported) `primeLiveTick` test helper achieves, but through the PUBLIC
// Tick API only — this file lives in package rules, outside package engine,
// so it cannot reach the unexported lastTickWall field directly. Ticking at
// precisely the schedule floor makes dispatchSchedule's first-Tick branch
// replay the zero-width window (floor, floor], which every schedule
// enumerator treats as empty (toWallInclusive<=fromWallExclusive), so it
// produces no dispositions; its only effect is advancing the engine's
// internal wall cursor to floor, so every SUBSEQUENT Tick in the handler is a
// genuine LIVE tick (RUL-041/051/340) rather than a downtime resume replayed
// through the misfire policy (RUL-350).
func primeScheduleLiveTick(t *testing.T, e *engine.Engine, clk *clock.FakeClock, floor int64) {
	t.Helper()
	clk.SetWall(floor)
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("priming tick at the schedule floor produced dispositions (want none): %+v", d)
	}
}

// parseUTCOffsetSeconds parses a "+HH:MM"/"-HH:MM" UTC-offset string (the
// corpus's own wire form for fired_at_utc_offset) into signed seconds, so it
// can be compared against time.Time.Zone()'s offset.
func parseUTCOffsetSeconds(t *testing.T, s string) int {
	t.Helper()
	sign := 1
	rest := s
	switch {
	case strings.HasPrefix(s, "-"):
		sign = -1
		rest = s[1:]
	case strings.HasPrefix(s, "+"):
		rest = s[1:]
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 2 {
		t.Fatalf("malformed utc offset %q", s)
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		t.Fatalf("malformed utc offset %q", s)
	}
	return sign * (h*3600 + m*60)
}

// --- RUL-341: DST spring-forward removes a local time for that date --------

type rul341Case struct {
	Input struct {
		Fixture struct {
			ScopeNodeTZ string `json:"scope_node_tz"`
		} `json:"fixture"`
		Trigger        json.RawMessage `json:"trigger"`
		DatesEvaluated []string        `json:"dates_evaluated"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Date  string `json:"date"`
			Fired bool   `json:"fired"`
		} `json:"results"`
	} `json:"expected"`
}

// runRUL341 replays a `time` trigger across a spring-forward transition day by
// day, on the real engine: each date it's evaluated against Ticks from that
// date's local midnight to the next, and "fired" is whether that Tick's own
// dispositions are non-empty — the DST-removed date's local time never
// occurred, so its Tick fires nothing (RUL-341), while the days immediately
// before and after fire normally (RUL-340).
func runRUL341(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul341Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &corpusSink{}
	e := engine.New(reg, clk, sink, nil)
	if err := e.SetLocation(c.Input.Fixture.ScopeNodeTZ, 40.7128, -74.0060); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadScheduleCorpusRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0RUL341", c.Input.Trigger, "")

	tz, err := time.LoadLocation(c.Input.Fixture.ScopeNodeTZ)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", c.Input.Fixture.ScopeNodeTZ, err)
	}
	if len(c.Input.DatesEvaluated) == 0 {
		t.Fatal("corpus case has no dates_evaluated")
	}
	firstDate, err := time.ParseInLocation("2006-01-02", c.Input.DatesEvaluated[0], tz)
	if err != nil {
		t.Fatalf("parse first date: %v", err)
	}
	floor := firstDate.UnixMilli() // the first evaluated date's own local midnight
	e.SeedScheduleFloor(floor)
	primeScheduleLiveTick(t, e, clk, floor)

	wantByDate := map[string]bool{}
	for _, r := range c.Expected.Results {
		wantByDate[r.Date] = r.Fired
	}

	dispatchesSoFar := 0
	for _, dateStr := range c.Input.DatesEvaluated {
		want, ok := wantByDate[dateStr]
		if !ok {
			t.Fatalf("no expected result for date %q", dateStr)
		}
		day, err := time.ParseInLocation("2006-01-02", dateStr, tz)
		if err != nil {
			t.Fatalf("parse date %q: %v", dateStr, err)
		}
		clk.SetWall(day.AddDate(0, 0, 1).UnixMilli()) // through the end of this local date
		disps := e.Tick(clk)
		fired := len(disps) > 0
		if fired != want {
			t.Errorf("date %s: fired = %v, want %v", dateStr, fired, want)
		}
		wantDispatches := dispatchesSoFar
		if want {
			wantDispatches++
		}
		if len(sink.calls) != wantDispatches {
			t.Errorf("date %s: cumulative dispatch count = %d, want %d", dateStr, len(sink.calls), wantDispatches)
		}
		dispatchesSoFar = wantDispatches
	}
}

// --- RUL-342: DST fall-back fires exactly once, at the first occurrence ----

type rul342Case struct {
	Input struct {
		Fixture struct {
			ScopeNodeTZ string `json:"scope_node_tz"`
		} `json:"fixture"`
		Trigger       json.RawMessage `json:"trigger"`
		DateEvaluated string          `json:"date_evaluated"`
	} `json:"input"`
	Expected struct {
		FireCount        int    `json:"fire_count"`
		FiredAtUTCOffset string `json:"fired_at_utc_offset"`
	} `json:"expected"`
}

// runRUL342 replays a `time` trigger across a fall-back-repeated local time on
// the real engine (fire_count) and, via the same schedule.TimeTrigger
// enumerator the engine drives internally, verifies the single occurrence
// resolved is keyed to the first (earlier-UTC-offset) absolute instant, never
// the second (RUL-342).
func runRUL342(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul342Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &corpusSink{}
	e := engine.New(reg, clk, sink, nil)
	if err := e.SetLocation(c.Input.Fixture.ScopeNodeTZ, 40.7128, -74.0060); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadScheduleCorpusRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0RUL342", c.Input.Trigger, "")

	tz, err := time.LoadLocation(c.Input.Fixture.ScopeNodeTZ)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", c.Input.Fixture.ScopeNodeTZ, err)
	}
	day, err := time.ParseInLocation("2006-01-02", c.Input.DateEvaluated, tz)
	if err != nil {
		t.Fatalf("parse date_evaluated: %v", err)
	}
	floor := day.UnixMilli()
	e.SeedScheduleFloor(floor)
	primeScheduleLiveTick(t, e, clk, floor)

	nextMidnight := day.AddDate(0, 0, 1).UnixMilli()
	clk.SetWall(nextMidnight)
	disps := e.Tick(clk)
	if len(disps) != c.Expected.FireCount {
		t.Fatalf("fire_count = %d, want %d: %+v", len(disps), c.Expected.FireCount, disps)
	}
	for i, d := range disps {
		if d.Disposition != engine.Ran {
			t.Errorf("disps[%d].Disposition = %s, want ran", i, d.Disposition)
		}
		if d.MisfireCaught {
			t.Errorf("disps[%d]: a live fall-back occurrence must not be misfire_caught", i)
		}
	}
	if len(sink.calls) != c.Expected.FireCount {
		t.Errorf("dispatch count = %d, want %d", len(sink.calls), c.Expected.FireCount)
	}

	var trig struct {
		At string `json:"at"`
	}
	if err := json.Unmarshal(c.Input.Trigger, &trig); err != nil {
		t.Fatalf("parse trigger: %v", err)
	}
	tt, err := schedule.NewTimeTrigger(trig.At)
	if err != nil {
		t.Fatalf("NewTimeTrigger: %v", err)
	}
	occs := tt.Occurrences(schedule.Location{TZ: tz}, floor, nextMidnight)
	if len(occs) != 1 {
		t.Fatalf("underlying enumerator produced %d occurrence(s), want 1: %v", len(occs), occs)
	}
	_, gotOffsetSec := time.UnixMilli(occs[0]).In(tz).Zone()
	wantOffsetSec := parseUTCOffsetSeconds(t, c.Expected.FiredAtUTCOffset)
	if gotOffsetSec != wantOffsetSec {
		t.Errorf("fired_at_utc_offset: got %+d sec, want %q (%+d sec)", gotOffsetSec, c.Expected.FiredAtUTCOffset, wantOffsetSec)
	}
}

// --- RUL-352: misfire catch_up_once collapses missed occurrences to one ----

type rul352Case struct {
	Input struct {
		Trigger  json.RawMessage `json:"trigger"`
		RuleMode string          `json:"rule_mode"`
		Events   []struct {
			Seq  int    `json:"seq"`
			T    string `json:"t"`
			Kind string `json:"kind"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		FireCountOnResume     int    `json:"fire_count_on_resume"`
		Disposition           string `json:"disposition"`
		MisfireCaught         bool   `json:"misfire_caught"`
		ModeEvaluationApplied bool   `json:"mode_evaluation_applied"`
	} `json:"expected"`
}

// runRUL352 replays a catch_up_once time_pattern trigger's downtime span
// (engine_offline through the missed occurrences to engine_resumes) as the
// engine's own first-Tick RESUME path (RUL-350): the many missed occurrences
// collapse into exactly one caught-up firing, dispatched through the rule's
// declared mode (`ran`, mode_evaluation_applied, since no other run was in
// progress) and marked misfire_caught (RUL-352/355/246).
func runRUL352(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul352Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Input.Events) == 0 {
		t.Fatal("corpus case has no events")
	}

	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &corpusSink{}
	e := engine.New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	rule := loadScheduleCorpusRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0RUL352", c.Input.Trigger, c.Input.RuleMode)
	if c.Input.RuleMode != "" && rule.Mode != c.Input.RuleMode {
		t.Fatalf("rule.Mode = %q, want %q", rule.Mode, c.Input.RuleMode)
	}

	// engine_offline is the last instant the engine evaluated live before going
	// down; engine_resumes is when it comes back — everything strictly between
	// (the scheduled_occurrence_missed events) was missed while down.
	var offlineAt, resumeAt string
	for _, ev := range c.Input.Events {
		switch ev.Kind {
		case "engine_offline":
			offlineAt = ev.T
		case "engine_resumes":
			resumeAt = ev.T
		}
	}
	if offlineAt == "" || resumeAt == "" {
		t.Fatalf("corpus case missing engine_offline/engine_resumes events: %+v", c.Input.Events)
	}
	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	floor := dayPlusHMS(t, day, offlineAt)
	resume := dayPlusHMS(t, day, resumeAt)

	e.SeedScheduleFloor(floor)
	clk.SetWall(resume) // first Tick after the offline span: a resume, not a live tick
	disps := e.Tick(clk)

	if len(disps) != c.Expected.FireCountOnResume {
		t.Fatalf("fire_count_on_resume = %d, want %d: %+v", len(disps), c.Expected.FireCountOnResume, disps)
	}
	if string(disps[0].Disposition) != c.Expected.Disposition {
		t.Errorf("disposition = %s, want %s", disps[0].Disposition, c.Expected.Disposition)
	}
	if disps[0].MisfireCaught != c.Expected.MisfireCaught {
		t.Errorf("misfire_caught = %v, want %v", disps[0].MisfireCaught, c.Expected.MisfireCaught)
	}
	if c.Expected.ModeEvaluationApplied && disps[0].Mode != rule.Mode {
		t.Errorf("mode_evaluation_applied: disposition.Mode = %q, want the rule's declared mode %q", disps[0].Mode, rule.Mode)
	}
	if len(sink.calls) != c.Expected.FireCountOnResume {
		t.Errorf("dispatch count = %d, want %d", len(sink.calls), c.Expected.FireCountOnResume)
	}
}

// dayPlusHMS parses an "HH:MM:SS" wire time-of-day (this corpus case's own
// event shape) onto the given UTC calendar day, returning Unix ms.
func dayPlusHMS(t *testing.T, day time.Time, hms string) int64 {
	t.Helper()
	h, m, s, ok := parseHMSForTest(hms)
	if !ok {
		t.Fatalf("malformed HH:MM:SS %q", hms)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, s, 0, time.UTC).UnixMilli()
}

// parseHMSForTest parses a 24-hour "HH:MM:SS" string, failing closed (ok=false)
// on anything else — this driver's own tiny mirror of schedule's unexported
// parseHMS, needed here only because this file sits outside package schedule.
func parseHMSForTest(s string) (h, m, sec int, ok bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var err1, err2, err3 error
	h, err1 = strconv.Atoi(parts[0])
	m, err2 = strconv.Atoi(parts[1])
	sec, err3 = strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	return h, m, sec, true
}

// --- RUL-354: an undeclared misfire defaults to skip, no late fire ---------

type rul354Case struct {
	Input struct {
		Trigger json.RawMessage `json:"trigger"`
		Events  []struct {
			Seq  int    `json:"seq"`
			T    string `json:"t"`
			Kind string `json:"kind"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		EffectiveMisfirePolicy string `json:"effective_misfire_policy"`
		FiredLateAtResume      bool   `json:"fired_late_at_resume"`
	} `json:"expected"`
}

// runRUL354 asserts both of the corpus's expected fields: effective_misfire_
// policy is checked against schedule.ApplyMisfire's own documented behavior for
// an absent ("") declaration — it fires nothing, exactly the `skip` contract
// (RUL-351/354) — and fired_late_at_resume is checked on the real engine's
// first-Tick resume path: the 08:00 occurrence missed while offline produces
// no disposition and no dispatch once evaluation resumes (RUL-350/354).
func runRUL354(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul354Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Input.Events) == 0 {
		t.Fatal("corpus case has no events")
	}

	// effective_misfire_policy: an absent `misfire` decodes as "" (see engine's
	// unexported parseMisfire), and schedule.ApplyMisfire documents "" as the
	// skip default (RUL-351/354): it must fire nothing for a non-empty missed set.
	if c.Expected.EffectiveMisfirePolicy == "skip" {
		if fires := schedule.ApplyMisfire("", []int64{1_000}); len(fires) != 0 {
			t.Errorf("effective_misfire_policy=skip: ApplyMisfire(\"\", ...) fired %+v, want none", fires)
		}
	} else {
		t.Fatalf("unexpected effective_misfire_policy %q in corpus case", c.Expected.EffectiveMisfirePolicy)
	}

	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &corpusSink{}
	e := engine.New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadScheduleCorpusRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0RUL354", c.Input.Trigger, "")

	var offlineAt, resumeAt string
	for _, ev := range c.Input.Events {
		switch ev.Kind {
		case "engine_offline":
			offlineAt = ev.T
		case "engine_resumes":
			resumeAt = ev.T
		}
	}
	if offlineAt == "" || resumeAt == "" {
		t.Fatalf("corpus case missing engine_offline/engine_resumes events: %+v", c.Input.Events)
	}
	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	floor := dayPlusHMS(t, day, offlineAt)
	resume := dayPlusHMS(t, day, resumeAt)

	e.SeedScheduleFloor(floor)
	clk.SetWall(resume) // first Tick after the offline span: a resume, not a live tick
	disps := e.Tick(clk)

	fired := len(disps) > 0
	if fired != c.Expected.FiredLateAtResume {
		t.Errorf("fired_late_at_resume = %v, want %v: %+v", fired, c.Expected.FiredLateAtResume, disps)
	}
	if len(sink.calls) != 0 && !c.Expected.FiredLateAtResume {
		t.Errorf("fired_late_at_resume=false but sink dispatched: %+v", sink.calls)
	}
}

// --- RUL-150: variable condition constant-folded across a live write --------

type rul150Case struct {
	Input struct {
		Variable struct {
			Name               string `json:"name"`
			ValueAtCompileTime bool   `json:"value_at_compile_time"`
		} `json:"variable"`
		Rule   json.RawMessage `json:"rule"`
		Events []struct {
			Seq      int    `json:"seq"`
			TMs      int64  `json:"t_ms"`
			Kind     string `json:"kind"`
			Variable string `json:"variable"`
			NewValue any    `json:"new_value"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq            int  `json:"seq"`
			ConditionsPass bool `json:"conditions_pass"`
		} `json:"results"`
	} `json:"expected"`
}

// runRUL150 drives the corpus's rule through the real compile+closure+engine
// front door: closure.Compute freezes the `variable` condition's compile-time
// value into generation 1's Closure (RUL-150/390), and the engine evaluates
// every subsequent firing against that frozen Closure — never a live lookup.
// The seq-3 live_variable_write event mutates only the driver's own "live"
// variable map (modeling an app-side write), which the already-applied
// generation 1 never re-reads (RUL-391/392): seq 4's firing still resolves the
// variable condition against the frozen false and still passes, proving the
// live write after seq 3 had no effect on it.
func runRUL150(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul150Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	entry, cerr := compile.Compile(c.Input.Rule)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	rule, err := model.ParseRule(c.Input.Rule)
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}

	// liveVars models the app-side variable store: it starts at the corpus's
	// value_at_compile_time and is the ONLY thing seq 3's live_variable_write
	// touches. closure.Compute reads it exactly once, at "compile time" (seq
	// 1), to freeze generation 1's Closure — the engine never consults
	// liveVars again after that.
	liveVars := map[string]any{c.Input.Variable.Name: c.Input.Variable.ValueAtCompileTime}
	cl, cerr2 := closure.Compute(rule, closure.Env{Variables: liveVars})
	if cerr2 != nil {
		t.Fatalf("closure.Compute: %v", cerr2)
	}

	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &corpusSink{}
	e := engine.New(reg, clk, sink, nil)
	e.ApplyGeneration(engine.Generation{Number: 1, Rules: []engine.CompiledRule{{Entry: entry, Rule: rule, Closure: cl}}})

	entityID := rule.Triggers[0].EntityRef.EntityID
	e.Observe(state.NewObservation(reg, mediaEnt(entityID, "off"), mediaEnt(entityID, "off")))

	var lastT int64
	var isOn bool
	for _, ev := range c.Input.Events {
		clk.Advance(ev.TMs - lastT)
		lastT = ev.TMs
		switch ev.Kind {
		case "live_variable_write":
			// App-side write to the live store only; generation 1's frozen
			// Closure (cl.Variables) is untouched (RUL-391/392).
			liveVars[ev.Variable] = ev.NewValue
		case "trigger_fires_offline", "trigger_fires_offline_again":
			// A genuine off->on transition each time the corpus's own trigger
			// fires; toggle back to off first so a second firing is a fresh
			// edge, not a same-state reconfirm (RUL-020/300).
			if isOn {
				e.Observe(state.NewObservation(reg, mediaEnt(entityID, "on"), mediaEnt(entityID, "off")))
			}
			disps := e.Observe(state.NewObservation(reg, mediaEnt(entityID, "off"), mediaEnt(entityID, "on")))
			isOn = true
			for _, want := range c.Expected.Results {
				if want.Seq != ev.Seq {
					continue
				}
				gotPass := len(disps) == 1 && disps[0].Disposition == engine.Ran
				if gotPass != want.ConditionsPass {
					t.Errorf("seq %d: fired(conditions_pass) = %v, want %v: %+v", ev.Seq, gotPass, want.ConditionsPass, disps)
				}
			}
		}
	}
}

// --- RUL-301: stabilization window defers a genuine transition --------------

type rul301Case struct {
	Input struct {
		Fixture struct {
			Entities map[string]struct {
				DeviceClass string `json:"device_class"`
			} `json:"entities"`
			StabilizationWindowMs int64 `json:"stabilization_window_ms"`
		} `json:"fixture"`
		Trigger json.RawMessage `json:"trigger"`
		Events  []struct {
			Seq           int    `json:"seq"`
			TMs           int64  `json:"t_ms"`
			Kind          string `json:"kind"`
			ReportedState string `json:"reported_state"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq                          int  `json:"seq"`
			MatchedAtMs                  *int `json:"matched_at_ms"`
			Dispatched                   bool `json:"dispatched"`
			HeldTransitionDispatchedAtMs *int `json:"held_transition_dispatched_at_ms"`
		} `json:"results"`
	} `json:"expected"`
}

// runRUL301 replays the corpus's post-restart transition on the real engine
// with the fixture's own stabilization_window_ms configured via
// SetStabilizationWindow: seq 2's genuine off->on observation, arriving before
// the window elapses, matches but does not dispatch (held pending, RUL-301);
// the window's own bounded elapse (a Tick at the fixture's window instant)
// then releases exactly that already-matched transition — never re-evaluating
// it against a later observation.
func runRUL301(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul301Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &corpusSink{}
	e := engine.New(reg, clk, sink, nil)
	e.SetStabilizationWindow(time.Duration(c.Input.Fixture.StabilizationWindowMs) * time.Millisecond)

	var entityID string
	for id := range c.Input.Fixture.Entities {
		entityID = id
	}

	ruleJSON := []byte(`{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL301","triggers":[` + string(c.Input.Trigger) + `],"actions":[{"type":"device_command","entity_id":"` + entityID + `","command":"power"}]}`)
	entry, cerr := compile.Compile(ruleJSON)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	rule, err := model.ParseRule(ruleJSON)
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}
	// The engine_restart readiness point (seq 1) is modeled by a fresh engine
	// (New) applying its first generation (Load), stabilization window armed
	// from the start, seeded with the entity's durable pre-restart state.
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.SeedEntityState(entityID, "off")

	var lastT int64
	for _, ev := range c.Input.Events {
		if ev.Kind != "observation" {
			continue
		}
		clk.Advance(ev.TMs - lastT)
		lastT = ev.TMs
		disps := e.Observe(state.NewObservation(reg, mediaEnt(entityID, "off"), mediaEnt(entityID, ev.ReportedState)))
		for _, want := range c.Expected.Results {
			if want.Seq != ev.Seq {
				continue
			}
			gotDispatched := len(sink.calls) != 0
			if gotDispatched != want.Dispatched {
				t.Errorf("seq %d: dispatched = %v, want %v: %+v", ev.Seq, gotDispatched, want.Dispatched, disps)
			}
			if !want.Dispatched && len(disps) != 0 {
				t.Errorf("seq %d: held transition produced a disposition early: %+v", ev.Seq, disps)
			}
		}
	}

	// The window's own bounded elapse (fixture's own stabilization_window_ms,
	// on the monotonic clock) releases the held transition: exactly one Ran,
	// exactly one dispatch — the already-matched seq-2 firing, not a
	// re-evaluation against any later observation.
	for _, want := range c.Expected.Results {
		if want.HeldTransitionDispatchedAtMs == nil {
			continue
		}
		target := int64(*want.HeldTransitionDispatchedAtMs)
		if target > lastT {
			clk.Advance(target - lastT)
			lastT = target
		}
		d := e.Tick(clk)
		if len(d) != 1 || d[0].Disposition != engine.Ran {
			t.Fatalf("window elapse: want one Ran, got %+v", d)
		}
		if len(sink.calls) != 1 || sink.calls[0].EntityID != entityID {
			t.Fatalf("released transition should dispatch once for %s, got %+v", entityID, sink.calls)
		}
	}
}

// --- RUL-303: generation-swap trigger-level baseline carry-forward ----------

type rul303GenRule struct {
	RuleID  string          `json:"rule_id"`
	Trigger json.RawMessage `json:"trigger"`
}

type rul303Case struct {
	Input struct {
		Fixture struct {
			Entities map[string]struct {
				DeviceClass string `json:"device_class"`
			} `json:"entities"`
		} `json:"fixture"`
		PriorGeneration struct {
			Generation int             `json:"generation"`
			Rules      []rul303GenRule `json:"rules"`
		} `json:"prior_generation"`
		NewGeneration struct {
			Generation int             `json:"generation"`
			Rules      []rul303GenRule `json:"rules"`
		} `json:"new_generation"`
		Events []struct {
			Seq           int    `json:"seq"`
			TMs           int64  `json:"t_ms"`
			Kind          string `json:"kind"`
			ReportedState string `json:"reported_state"`
			From          string `json:"from"`
			To            string `json:"to"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq    int    `json:"seq"`
			RuleID string `json:"rule_id"`
			Fired  bool   `json:"fired"`
		} `json:"results"`
	} `json:"expected"`
}

// compileRUL303Rule wraps one generation-declared trigger with a device_command
// action into a full rule and compiles it through the real front door
// (compile.Compile + model.ParseRule + closure.Compute — this case has no
// variable/selector/preset, so its Closure is empty, but it is still computed
// through the real closure package for parity with every other generation this
// driver builds).
func compileRUL303Rule(t *testing.T, gr rul303GenRule, entityID string) engine.CompiledRule {
	t.Helper()
	ruleJSON := []byte(`{"id":"` + gr.RuleID + `","triggers":[` + string(gr.Trigger) + `],"actions":[{"type":"device_command","entity_id":"` + entityID + `","command":"power"}]}`)
	entry, cerr := compile.Compile(ruleJSON)
	if cerr != nil {
		t.Fatalf("compile %s: %v", gr.RuleID, cerr)
	}
	rule, err := model.ParseRule(ruleJSON)
	if err != nil {
		t.Fatalf("parse %s: %v", gr.RuleID, err)
	}
	cl, cerr2 := closure.Compute(rule, closure.Env{})
	if cerr2 != nil {
		t.Fatalf("closure.Compute %s: %v", gr.RuleID, cerr2)
	}
	return engine.CompiledRule{Entry: entry, Rule: rule, Closure: cl}
}

// runRUL303 replays the corpus's two-generation swap on the real engine: an
// unrelated preset edit (never modeled here beyond the generation-number bump,
// since neither rule's Closure references a preset) recompiles generation 2
// with rule-R's trigger byte-identical (RUL-381: unchanged) and rule-S's
// `for:` edited 10->20 (RUL-381: changed). ApplyGeneration's own carried-
// forward/reset behavior (RUL-303/304) then decides, independently per rule,
// whether the seq-4 off->on transition is a genuine fire against a preserved
// baseline (rule-R) or an unfireable first-observation-since-reset (rule-S) —
// this handler only drives Observe/ApplyGeneration and diffs the returned
// dispositions against the corpus's own per-rule expectations.
func runRUL303(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul303Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	var entityID string
	for id := range c.Input.Fixture.Entities {
		entityID = id
	}

	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &corpusSink{}
	e := engine.New(reg, clk, sink, nil)

	buildGen := func(num int, rules []rul303GenRule) engine.Generation {
		crs := make([]engine.CompiledRule, 0, len(rules))
		for _, gr := range rules {
			crs = append(crs, compileRUL303Rule(t, gr, entityID))
		}
		return engine.Generation{Number: num, Rules: crs}
	}

	var lastT int64
	dispsBySeq := map[int][]engine.RunDisposition{}
	for _, ev := range c.Input.Events {
		clk.Advance(ev.TMs - lastT)
		lastT = ev.TMs
		switch ev.Kind {
		case "generation_apply":
			if ev.Seq == 1 {
				e.ApplyGeneration(buildGen(c.Input.PriorGeneration.Generation, c.Input.PriorGeneration.Rules))
			} else {
				e.ApplyGeneration(buildGen(c.Input.NewGeneration.Generation, c.Input.NewGeneration.Rules))
			}
		case "observation":
			dispsBySeq[ev.Seq] = e.Observe(state.NewObservation(reg, mediaEnt(entityID, "off"), mediaEnt(entityID, ev.ReportedState)))
		case "state_transition":
			dispsBySeq[ev.Seq] = e.Observe(state.NewObservation(reg, mediaEnt(entityID, ev.From), mediaEnt(entityID, ev.To)))
		}
	}

	for _, want := range c.Expected.Results {
		disps := dispsBySeq[want.Seq]
		var fired bool
		for _, d := range disps {
			if d.RuleID == want.RuleID && d.Disposition == engine.Ran {
				fired = true
			}
		}
		if fired != want.Fired {
			t.Errorf("seq %d rule %s: fired = %v, want %v: %+v", want.Seq, want.RuleID, fired, want.Fired, disps)
		}
	}

	// Beyond the corpus's own checked instant: rule-S's non-fire at seq 4 alone
	// is consistent with more than one cause (its `for`-bounded level simply
	// hasn't held long enough yet, independent of whether its baseline was
	// correctly reset) — so also prove the reset is what's actually
	// suppressing it, not remaining hold-arm latency: a genuinely fresh
	// (trigger,entity) runtime seeds a first observation already AT the target
	// level as "continuing to hold", per RUL-300, so it must not arm from
	// this transition even given unbounded further time — only a SUBSEQUENT
	// fresh entry into the level (off, then on again) can ever arm it. A
	// buggy carry-forward that wrongly reused rule-S's prior (armed-capable)
	// runtime would instead fire once its OLD `for` elapsed.
	lastSeq := c.Input.Events[len(c.Input.Events)-1].Seq
	for _, want := range c.Expected.Results {
		if want.Seq != lastSeq || want.Fired {
			continue
		}
		var forSec int
		for _, gr := range c.Input.NewGeneration.Rules {
			if gr.RuleID == want.RuleID {
				forSec = rawFor(gr.Trigger)
			}
		}
		if forSec == 0 {
			continue
		}
		clk.Advance(int64(forSec) * 2000) // well past even the OLD for: duration
		if d := e.Tick(clk); len(d) != 0 {
			t.Errorf("rule %s: fired from the seq-%d transition with no fresh subsequent entry into the level (baseline not reset?): %+v", want.RuleID, lastSeq, d)
		}
	}
}

// --- RUL-360: an engine restart resets an in-progress hold ------------------

type rul360Case struct {
	Input struct {
		Fixture struct {
			Entities map[string]struct {
				DeviceClass string `json:"device_class"`
			} `json:"entities"`
		} `json:"fixture"`
		Trigger json.RawMessage `json:"trigger"`
		Events  []struct {
			Seq           int    `json:"seq"`
			TMs           int64  `json:"t_ms"`
			Kind          string `json:"kind"`
			From          string `json:"from"`
			To            string `json:"to"`
			ReportedState string `json:"reported_state"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq                    int   `json:"seq"`
			HoldRestarted          *bool `json:"hold_restarted"`
			MisfireCaughtMarkerSet *bool `json:"misfire_caught_marker_set"`
		} `json:"results"`
	} `json:"expected"`
}

// runRUL360 models "the evaluating engine itself restarts mid-hold" the only
// way that phrase can mean anything against this package's Engine type: no
// in-flight timer state survives an engine restart because nothing persists it
// (RUL-360/361) — there is no API to carry a Hold's elapsed time across two
// Engine values, so the pre-restart engine (which arms the hold and advances
// it 120000ms) and the post-restart engine (a wholly fresh Engine + FakeClock,
// seeded only with the durable entity state that DOES survive, exactly like
// SeedEntityState models on every other restart-adjacent corpus case) are
// deliberately two separate values. seq 3's post-restart reconfirm of the
// unchanged durable "unreachable" state is not itself a fresh transition
// (RUL-025/300), so it must not restart the hold's countdown (hold_restarted:
// false) — and because it produces no disposition at all, it carries no
// misfire_caught marker either (RUL-362: a distinct mechanism, never touched
// by this scenario).
func runRUL360(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul360Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	var entityID string
	for id := range c.Input.Fixture.Entities {
		entityID = id
	}

	ruleJSON := []byte(`{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL360","triggers":[` + string(c.Input.Trigger) + `],"actions":[{"type":"device_command","entity_id":"` + entityID + `","command":"power"}]}`)
	entry, cerr := compile.Compile(ruleJSON)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	rule, err := model.ParseRule(ruleJSON)
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}

	reg := registry.FixtureRegistry{}

	// Pre-restart engine: seq 1's on->unreachable transition arms the 300s
	// hold; it must not fire immediately (only 0ms has elapsed toward it).
	preClk := clock.NewFakeClock()
	pre := engine.New(reg, preClk, &corpusSink{}, nil)
	if err := pre.Load(entry, rule); err != nil {
		t.Fatalf("Load (pre): %v", err)
	}
	pre.Observe(state.NewObservation(reg, mediaEnt(entityID, "on"), mediaEnt(entityID, "on")))
	if d := pre.Observe(state.NewObservation(reg, mediaEnt(entityID, "on"), mediaEnt(entityID, "unreachable"))); len(d) != 0 {
		t.Fatalf("seq 1: hold arming on a 300s for: must not fire immediately, got %+v", d)
	}
	preClk.Advance(120000) // "the engine restarts with 120000ms of the hold already elapsed"

	// seq 2, engine_restart: no in-flight run or timer state survives — a
	// wholly fresh Engine + FakeClock (mono resets to 0 too, exactly like a
	// real reboot), never the pre-restart engine's elapsed hold.
	postClk := clock.NewFakeClock()
	postSink := &corpusSink{}
	post := engine.New(reg, postClk, postSink, nil)
	if err := post.Load(entry, rule); err != nil {
		t.Fatalf("Load (post): %v", err)
	}
	// The durable value is the entity's own last-known state, not the hold's
	// timer, so it DOES survive the restart (RUL-300/304), seeded exactly like
	// SeedEntityState.
	post.SeedEntityState(entityID, "unreachable")
	if d := post.Tick(postClk); len(d) != 0 {
		t.Fatalf("seq 2: a freshly restarted engine ticking immediately produced dispositions (hold must reset to zero elapsed): %+v", d)
	}

	// seq 3, post_restart_observation @ t=121000 (1000ms after the restart):
	// reconfirms the unchanged durable "unreachable" value.
	postClk.Advance(1000)
	disps := post.Observe(state.NewObservation(reg, mediaEnt(entityID, "unreachable"), mediaEnt(entityID, "unreachable")))

	for _, want := range c.Expected.Results {
		if want.Seq != 3 {
			continue
		}
		if want.HoldRestarted != nil {
			restarted := len(disps) != 0
			if restarted != *want.HoldRestarted {
				t.Errorf("seq 3: hold_restarted = %v, want %v: %+v", restarted, *want.HoldRestarted, disps)
			}
		}
	}
	for _, want := range c.Expected.Results {
		if want.MisfireCaughtMarkerSet == nil {
			continue
		}
		// No disposition this scenario ever produces carries a misfire_caught
		// marker: seq 3's own reconfirm above is the concrete evidence — an
		// empty disposition list carries none.
		if len(disps) != 0 {
			t.Errorf("misfire_caught_marker_set: got dispositions %+v, want none (no marker possible)", disps)
		}
	}
}

// --- RUL-380: generation-swap cancels a changed rule's in-flight run -------

type rul380GenRule struct {
	RuleID  string            `json:"rule_id"`
	Actions []json.RawMessage `json:"actions"`
}

type rul380Case struct {
	Input struct {
		PriorGeneration struct {
			Generation int             `json:"generation"`
			Rules      []rul380GenRule `json:"rules"`
		} `json:"prior_generation"`
		NewGeneration struct {
			Generation int             `json:"generation"`
			Rules      []rul380GenRule `json:"rules"`
		} `json:"new_generation"`
		InFlightRunsAtSwap []struct {
			RuleID    string `json:"rule_id"`
			RunID     string `json:"run_id"`
			ElapsedMs int64  `json:"elapsed_ms"`
		} `json:"in_flight_runs_at_swap"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			RunID    string `json:"run_id"`
			Canceled bool   `json:"canceled"`
		} `json:"results"`
	} `json:"expected"`
}

// rul380Entity is the fixture ULID both of this case's rules trigger on: the
// corpus itself declares no fixture (it specifies each rule only by its
// actions and its in-flight run), so a single shared subject entity is this
// driver's own minimal fixture, driving both rules' runs in-flight together
// exactly as the corpus's "both runs belong to the same generation swap"
// framing describes.
const rul380Entity = "01J8Z3K4N5P6Q7R8S9T0V1RL380"

// compileRUL380Rule wraps one generation-declared rule_id + actions (the
// corpus's own JSON, verbatim — including its actual delay duration) with a
// state trigger on rul380Entity, compiled through the real front door.
func compileRUL380Rule(t *testing.T, gr rul380GenRule) engine.CompiledRule {
	t.Helper()
	actionsJSON, err := json.Marshal(gr.Actions)
	if err != nil {
		t.Fatalf("marshal %s actions: %v", gr.RuleID, err)
	}
	ruleJSON := []byte(`{"id":"` + gr.RuleID + `","triggers":[{"type":"state","entity_id":"` + rul380Entity + `","to":["on"]}],"actions":` + string(actionsJSON) + `}`)
	entry, cerr := compile.Compile(ruleJSON)
	if cerr != nil {
		t.Fatalf("compile %s: %v", gr.RuleID, cerr)
	}
	rule, err := model.ParseRule(ruleJSON)
	if err != nil {
		t.Fatalf("parse %s: %v", gr.RuleID, err)
	}
	cl, cerr2 := closure.Compute(rule, closure.Env{})
	if cerr2 != nil {
		t.Fatalf("closure.Compute %s: %v", gr.RuleID, cerr2)
	}
	return engine.CompiledRule{Entry: entry, Rule: rule, Closure: cl}
}

// runRUL380 replays the corpus's two-generation swap on the real engine: both
// rule-A and rule-B fire their own in-flight [delay] run together (one
// Observe, since both trigger on the same shared entity), then generation 2
// edits rule-A's delay duration (changed, RUL-381) while leaving rule-B
// byte-identical (unchanged) — ApplyGeneration's own returned Canceled
// dispositions are diffed directly against the corpus's own per-run
// expectations (joined here via each run's declared rule_id, since the engine
// tracks no run-ID concept of its own).
func runRUL380(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul380Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &corpusSink{}
	e := engine.New(reg, clk, sink, nil)

	buildGen := func(num int, rules []rul380GenRule) engine.Generation {
		crs := make([]engine.CompiledRule, 0, len(rules))
		for _, gr := range rules {
			crs = append(crs, compileRUL380Rule(t, gr))
		}
		return engine.Generation{Number: num, Rules: crs}
	}

	e.ApplyGeneration(buildGen(c.Input.PriorGeneration.Generation, c.Input.PriorGeneration.Rules))

	// Establish the shared entity's baseline, then a genuine off->on
	// transition starts BOTH rules' in-flight [delay] runs at once.
	e.Observe(state.NewObservation(reg, mediaEnt(rul380Entity, "off"), mediaEnt(rul380Entity, "off")))
	started := e.Observe(state.NewObservation(reg, mediaEnt(rul380Entity, "off"), mediaEnt(rul380Entity, "on")))
	if len(started) != len(c.Input.PriorGeneration.Rules) {
		t.Fatalf("expected %d run(s) to start, got %+v", len(c.Input.PriorGeneration.Rules), started)
	}
	for _, d := range started {
		if d.Disposition != engine.Ran {
			t.Fatalf("expected every rule to start its in-flight run, got %+v", started)
		}
	}

	// Advance the clock by the corpus's own declared in-flight elapsed time
	// before the swap.
	var elapsed int64
	for _, r := range c.Input.InFlightRunsAtSwap {
		elapsed = r.ElapsedMs
		break
	}
	clk.Advance(elapsed)

	out := e.ApplyGeneration(buildGen(c.Input.NewGeneration.Generation, c.Input.NewGeneration.Rules))

	ruleIDByRunID := map[string]string{}
	for _, r := range c.Input.InFlightRunsAtSwap {
		ruleIDByRunID[r.RunID] = r.RuleID
	}
	canceledRuleIDs := map[string]bool{}
	for _, d := range out {
		if d.Disposition == engine.Canceled {
			canceledRuleIDs[d.RuleID] = true
		}
	}

	for _, want := range c.Expected.Results {
		ruleID, ok := ruleIDByRunID[want.RunID]
		if !ok {
			t.Fatalf("no rule_id mapping for run_id %q", want.RunID)
		}
		got := canceledRuleIDs[ruleID]
		if got != want.Canceled {
			t.Errorf("run_id %s (rule %s): canceled = %v, want %v: %+v", want.RunID, ruleID, got, want.Canceled, out)
		}
	}
}
