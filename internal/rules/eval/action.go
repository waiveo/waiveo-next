package eval

import (
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// CommandSink is where a `device_command` or `preset_batch` action's
// individual device commands are actually dispatched (RUL-160/170). A
// deployment's own implementation talks to the real device layer; RunActions
// itself never knows or cares how a command is carried out, only whether it
// succeeded.
type CommandSink interface {
	Dispatch(entityID, command string, params map[string]any) error
}

// PresetCommand is one device command in a platform-owned preset's command
// list (RUL-170) — the shape PresetStore.Commands returns, and the same shape
// RUL-390 freezes into a compiled generation's closed_over.preset_batches for
// an edge-classified rule.
type PresetCommand struct {
	EntityID string
	Command  string
	Params   map[string]any
}

// PresetStore resolves a preset_batch action's preset_id (RUL-170) to its
// device-command list. Commands reports ok=false when presetID does not
// resolve to a known preset row; RunActions treats that as a no-op rather
// than an error (compile-time validation, PRESET_NOT_FOUND, is what actually
// guards against an unresolvable preset_id reaching evaluation).
type PresetStore interface {
	Commands(presetID string) ([]PresetCommand, bool)
}

// CommandResult is one target's pass/fail outcome within a PresetBatchOutcome
// (RUL-172, Wire shapes).
type CommandResult struct {
	Target  string `json:"target"`
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// PresetBatchOutcome is the three-value outcome RUL-172 defines: `complete`
// (every command succeeded), `partial` (at least one succeeded and at least
// one failed), or `failed` (none succeeded), plus the per-command result
// list. A multi-entity `device_command` dispatch (RUL-012/161) is aggregated
// into this identical shape.
type PresetBatchOutcome struct {
	Outcome string          `json:"outcome"`
	Results []CommandResult `json:"results"`
}

// LogEntry is one `log` action's recorded output (RUL-200): this part
// supports only a literal string message — an Expression-shaped message is
// Part 2b's concern (see runLog).
type LogEntry struct {
	Level   string
	Message string
}

// ActionContext carries everything RunActions needs to execute one action
// sequence: the registry/snapshot/clock/vars a nested `choose` condition
// evaluates against (reusing EvalCondition, Task 6 unchanged), the sinks a
// device command or preset batch dispatches through, and two optional
// recording slots (Outcomes, Logs) the caller supplies to observe what ran.
// Resolve is how a `selector`/`device_class`-form EntityRef (RUL-012) expands
// to a concrete entity-ID fan-out; an `entity_id`-form ref never needs it.
// Per RUL-390, freezing that expansion at compile time for an edge-classified
// rule is a later-part concern — Resolve is simply whatever live expansion
// the caller (the engine, Task 8/9) wires in for now.
type ActionContext struct {
	Reg      registry.Registry
	Snap     state.Snapshot
	Clk      clock.Clock
	Vars     map[string]any
	Sink     CommandSink
	Presets  PresetStore
	Resolve  func(ref model.EntityRef) []string
	Outcomes *[]PresetBatchOutcome
	Logs     *[]LogEntry
}

// DelayPending is RunActions' signal that a `delay` action (RUL-190) was
// reached: the delay itself performs no real sleep — it reports the
// monotonic instant (Clk.Mono() + duration) at which the sequence's
// remaining actions should resume, so the engine (Task 8/9) can drive the
// wait via its own Tick loop on a real or fake clock. RemainingActions is
// already flattened across any enclosing `choose` (see runChoose): resuming
// is exactly `RunActions(ctx, dp.RemainingActions)`.
type DelayPending struct {
	RemainingActions []model.Member
	ResumeAtMono     int64
}

func (d *DelayPending) Error() string {
	return fmt.Sprintf("eval: delay pending until mono=%d (%d action(s) remaining)", d.ResumeAtMono, len(d.RemainingActions))
}

// RunActions executes actions in order (RUL-160/170/180/190/200). It returns
// nil once every action has run to completion, or a *DelayPending the moment
// a `delay` action is reached with a positive duration — never a real error
// for a failed device command or preset-batch command, since neither halts
// the rest of the sequence (RUL-171/172): those failures are recorded into
// ctx.Outcomes instead. Resuming after a DelayPending is the caller's own
// subsequent call to RunActions(ctx, dp.RemainingActions).
func RunActions(ctx ActionContext, actions []model.Member) error {
	for i, m := range actions {
		switch m.Type {
		case "device_command":
			runDeviceCommand(ctx, m)
		case "preset_batch":
			runPresetBatch(ctx, m)
		case "choose":
			if err := runChoose(ctx, m); err != nil {
				return mergeDelayTail(err, actions[i+1:])
			}
		case "delay":
			if dp := runDelay(ctx, m); dp != nil {
				return mergeDelayTail(dp, actions[i+1:])
			}
		case "log":
			runLog(ctx, m)
		default:
			// An app-coupled or later-part action type (notify,
			// variable_write, workflow_start, pack_action) is out of this
			// evaluation core's scope; no-op rather than error.
		}
	}
	return nil
}

// mergeDelayTail flattens a nested DelayPending's remaining actions with
// whatever comes after it in the enclosing sequence (e.g. after the `choose`
// action a delay surfaced inside), so a single RunActions(ctx,
// dp.RemainingActions) call resumes the whole rest of the rule, not just the
// branch it was found in. A non-DelayPending error is returned unchanged.
func mergeDelayTail(err error, tail []model.Member) error {
	dp, ok := err.(*DelayPending)
	if !ok {
		return err
	}
	if len(tail) == 0 {
		return dp
	}
	merged := make([]model.Member, 0, len(dp.RemainingActions)+len(tail))
	merged = append(merged, dp.RemainingActions...)
	merged = append(merged, tail...)
	return &DelayPending{RemainingActions: merged, ResumeAtMono: dp.ResumeAtMono}
}

// runChoose implements RUL-180/181/182: the first branch whose condition
// passes (array order) runs and no other branch is considered; if none
// passes, `default` runs when present; if none passes and there is no
// `default`, the action performs no work.
func runChoose(ctx ActionContext, m model.Member) error {
	for _, b := range m.Branches {
		if EvalCondition(ctx.Reg, ctx.Snap, ctx.Clk, ctx.Vars, b.Condition) {
			return RunActions(ctx, b.Actions)
		}
	}
	if len(m.Default) > 0 {
		return RunActions(ctx, m.Default)
	}
	return nil
}

// runDelay implements RUL-190: duration_seconds <= 0 (including the field's
// absence, which decodes as the zero value) behaves as an immediate no-op —
// no DelayPending is produced and RunActions falls straight through to the
// next action. A positive duration reports the monotonic resume instant;
// remaining time is tracked purely as that instant minus whatever Mono()
// reads when the engine checks, never as elapsed-so-far state owned here.
func runDelay(ctx ActionContext, m model.Member) *DelayPending {
	var spec struct {
		DurationSeconds int `json:"duration_seconds"`
	}
	_ = json.Unmarshal(m.Raw, &spec)
	if spec.DurationSeconds <= 0 {
		return nil
	}
	return &DelayPending{ResumeAtMono: ctx.Clk.Mono() + int64(spec.DurationSeconds)*1000}
}

// runLog implements RUL-200 for a literal message (an Expression-shaped
// message is Part 2b's concern): message MUST decode as a JSON string or the
// action is skipped (fail-closed — no fabricated log entry), never an error.
// level defaults to "info" when absent.
func runLog(ctx ActionContext, m model.Member) {
	var spec struct {
		Message json.RawMessage `json:"message"`
		Level   string          `json:"level"`
	}
	if err := json.Unmarshal(m.Raw, &spec); err != nil {
		return
	}
	var msg string
	if err := json.Unmarshal(spec.Message, &msg); err != nil {
		// Not a literal string (e.g. an Expression object) — out of scope
		// this part.
		return
	}
	level := spec.Level
	if level == "" {
		level = "info"
	}
	if ctx.Logs != nil {
		*ctx.Logs = append(*ctx.Logs, LogEntry{Level: level, Message: msg})
	}
}

// runDeviceCommand implements RUL-160/161/012: a device_command's EntityRef
// resolves to one or more concrete entity IDs (resolveTargets); a single
// target is dispatched atomically (its pass/fail is not aggregated into a
// PresetBatchOutcome — RUL-161 draws that line explicitly), while more than
// one target aggregates into the identical partial-failure shape a
// preset_batch action uses (RUL-172).
func runDeviceCommand(ctx ActionContext, m model.Member) {
	if m.EntityRef == nil {
		return
	}
	var spec struct {
		Command string         `json:"command"`
		Params  map[string]any `json:"params"`
	}
	if err := json.Unmarshal(m.Raw, &spec); err != nil || spec.Command == "" {
		return
	}

	targets := resolveTargets(ctx, *m.EntityRef)
	if len(targets) == 0 {
		return
	}
	if len(targets) == 1 {
		dispatchOne(ctx, targets[0], spec.Command, spec.Params)
		return
	}
	recordOutcome(ctx, dispatchAll(ctx, targets, spec.Command, spec.Params))
}

// runPresetBatch implements RUL-170/171/172: every command in the referenced
// preset's list is attempted independently — a failing command never
// prevents the remaining commands in the same batch, and the batch's own
// failure never halts the rest of the rule's action sequence (RunActions'
// caller simply continues its loop regardless of what recordOutcome stores).
func runPresetBatch(ctx ActionContext, m model.Member) {
	var spec struct {
		PresetID string `json:"preset_id"`
	}
	if err := json.Unmarshal(m.Raw, &spec); err != nil || spec.PresetID == "" || ctx.Presets == nil {
		return
	}
	cmds, ok := ctx.Presets.Commands(spec.PresetID)
	if !ok {
		return
	}
	results := make([]CommandResult, 0, len(cmds))
	for _, c := range cmds {
		results = append(results, dispatchResult(ctx, c.EntityID, c.Command, c.Params))
	}
	recordOutcome(ctx, results)
}

// resolveTargets expands a device-affecting action's EntityRef to concrete
// entity IDs: the entity_id form needs no resolver, while selector/
// device_class forms defer to ctx.Resolve (nil resolves to no targets,
// fail-closed rather than dispatching to an unbounded set).
func resolveTargets(ctx ActionContext, ref model.EntityRef) []string {
	if ref.EntityID != "" {
		return []string{ref.EntityID}
	}
	if ctx.Resolve != nil {
		return ctx.Resolve(ref)
	}
	return nil
}

// dispatchOne performs a single-entity, atomic dispatch (RUL-161) — its
// pass/fail is not recorded into ctx.Outcomes.
func dispatchOne(ctx ActionContext, entityID, command string, params map[string]any) {
	if ctx.Sink == nil {
		return
	}
	_ = ctx.Sink.Dispatch(entityID, command, params)
}

// dispatchAll attempts command against every target independently (RUL-012),
// a failing target never skipping the rest, and returns the per-target
// result list a multi-entity PresetBatchOutcome carries.
func dispatchAll(ctx ActionContext, targets []string, command string, params map[string]any) []CommandResult {
	results := make([]CommandResult, 0, len(targets))
	for _, t := range targets {
		results = append(results, dispatchResult(ctx, t, command, params))
	}
	return results
}

// dispatchResult dispatches one command and reports its CommandResult.
func dispatchResult(ctx ActionContext, entityID, command string, params map[string]any) CommandResult {
	if ctx.Sink == nil {
		return CommandResult{Target: entityID, Command: command, OK: false, Error: "no command sink configured"}
	}
	if err := ctx.Sink.Dispatch(entityID, command, params); err != nil {
		return CommandResult{Target: entityID, Command: command, OK: false, Error: err.Error()}
	}
	return CommandResult{Target: entityID, Command: command, OK: true}
}

// recordOutcome appends a PresetBatchOutcome computed from results into
// ctx.Outcomes, when the caller supplied one to observe (RUL-172).
func recordOutcome(ctx ActionContext, results []CommandResult) {
	if ctx.Outcomes == nil {
		return
	}
	*ctx.Outcomes = append(*ctx.Outcomes, PresetBatchOutcome{Outcome: outcomeFor(results), Results: results})
}

// outcomeFor computes RUL-172's three-value outcome from a per-target result
// list: complete (no failures), failed (no successes), partial (a mix).
func outcomeFor(results []CommandResult) string {
	ok, fail := 0, 0
	for _, r := range results {
		if r.OK {
			ok++
		} else {
			fail++
		}
	}
	switch {
	case fail == 0:
		return "complete"
	case ok == 0:
		return "failed"
	default:
		return "partial"
	}
}
