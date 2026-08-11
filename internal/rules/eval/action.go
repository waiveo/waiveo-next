package eval

import (
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/schedule"
	"github.com/maaxton/waiveo-next/internal/rules/state"
	"github.com/maaxton/waiveo-next/internal/rules/vocab"
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

// ScreenRef is a signage action's target (RUL-233): exactly one of a single
// screen identity row (data-model/1 DAT-004a) or a label selector resolved
// against SCREEN rows. It is deliberately a distinct type from model.EntityRef
// rather than a reuse of it: the two name different row families, and a
// selector that means "these entities" in a device_command means "these
// screens" here. Sharing one type would make that difference invisible at every
// call site, and the compiler's own arity checks for the two are different
// (SCREEN_REF_AMBIGUOUS vs ENTITY_REF_AMBIGUOUS).
//
// The compile gate guarantees exactly one field is set (compile.validateScreenRef),
// so a SignageSink receiving both, or neither, is looking at a rule that never
// compiled.
type ScreenRef struct {
	ScreenID string
	Selector string
}

// ScreenResult is one targeted screen's pass/fail within a SignageOutcome
// (RUL-236) — the same per-target shape a device dispatch reports through
// CommandResult, for the same reason: an operator needs to know WHICH screen
// did not take the change, not merely that one did not.
type ScreenResult struct {
	ScreenID string `json:"screen_id"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// SignageOutcome is one signage action's result (RUL-236): the action type, the
// three-value outcome RUL-172 defines (reused verbatim rather than restated —
// `complete`, `partial`, `failed`), and the per-screen result list.
//
// An action whose ScreenRef matched NO screen is `complete` with an empty
// Screens list, not `failed`: RUL-233 makes an empty selector match a legal
// no-op, and reporting it as a failure would make "no lobby screens are
// currently placed here" indistinguishable from "the write was refused".
type SignageOutcome struct {
	Action  string         `json:"action"`
	Outcome string         `json:"outcome"`
	Screens []ScreenResult `json:"screens"`

	// Error is the ACTION-level reason, present only when the action failed as a
	// whole rather than at any particular screen: its own required members are
	// not declared (RUL-234/235), or its ScreenRef could not be resolved at all.
	//
	// It exists because those failures have no screen to hang off, and the two
	// answers available without it are both wrong. Returning silently reports a
	// run as `ran` with an empty effect report and an unchanged screen, which is
	// the "accepts work it never performs" shape this codebase keeps removing.
	// Fabricating a ScreenResult with an empty ScreenID instead puts a value that
	// is not a ULID into a field the schema declares as one, so a generated typed
	// client is handed an invalid id on the error path — the response is only
	// schema-conformant while nothing goes wrong.
	Error string `json:"error,omitempty"`
}

// FailedSignageOutcome reports a signage action that failed AS AN ACTION: no
// screen was attempted, because the action itself could not be carried out.
//
// Screens is an empty list rather than nil so the required `screens` member is
// served as `[]` and not `null`, and it deliberately carries no synthetic entry:
// there is no screen this failure belongs to, and inventing one would either
// name a screen that was never touched or carry an empty id (see
// SignageOutcome.Error).
func FailedSignageOutcome(action, reason string) SignageOutcome {
	return SignageOutcome{
		Action:  action,
		Outcome: "failed",
		Screens: []ScreenResult{},
		Error:   reason,
	}
}

// SignageSink performs the three RUL-234/235 signage actions against the app
// peer's authored rows: each one writes (or clears) the targeted screens' own
// program override (data-model/1 DAT-004c), which is what makes a fired rule
// change what a screen actually shows.
//
// It lives as an injected seam for the same reason CommandSink does — this
// package never knows how a change is carried out, only what was asked and what
// came back. Unlike CommandSink, though, no EDGE deployment ever wires one: the
// signage actions are app-class unconditionally (vocab), so the relay's engine
// is never handed a rule containing one, and its ActionContext leaves this nil.
// A nil sink is therefore the normal edge posture, not a misconfiguration, and
// the action is skipped exactly as an out-of-scope app action is.
//
// TTLSeconds on ShowAlert is 0 when the author declared none (no expiry); the
// sink converts it to an absolute DAT-004c `expires_at` against its own clock,
// because this package has no authority over what "now" means on the app peer.
type SignageSink interface {
	PlayCast(ref ScreenRef, castID string) SignageOutcome
	ShowAlert(ref ScreenRef, castID, message string, ttlSeconds int) SignageOutcome
	DismissAlert(ref ScreenRef) SignageOutcome
}

// LogEntry is one `log` action's recorded output (RUL-200): a level plus the
// message an Expression evaluated to (runLog / ctx.EvalExpr).
type LogEntry struct {
	Level   string
	Message string
}

// ExprEvaluator live-evaluates one Expression-valued action field — a `log`
// action's `message` (RUL-200) or a `params` value (RUL-393) — against the
// firing context at the instant the action runs, returning ok=false when the
// Expression fails closed (RUL-284). The engine wires the real closed-grammar
// evaluator (package expr), baking in the firing trigger's subject entity as
// the edge scope (RUL-282); eval itself never imports expr, so the evaluator is
// injected rather than called directly. Per RUL-393 the evaluation is LIVE at
// dispatch — the engine never freezes a params/message Expression into the
// compiled generation (contrast RUL-390).
type ExprEvaluator func(raw json.RawMessage) (value any, ok bool)

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
	Loc      schedule.Location
	Vars     map[string]any
	Sink     CommandSink
	Presets  PresetStore
	Resolve  func(ref model.EntityRef) []string
	Outcomes *[]PresetBatchOutcome
	Logs     *[]LogEntry

	// Rejected, when non-nil, receives every SINGLE-TARGET dispatch this context
	// refused before it ever reached Sink — an unresolvable entity, or a command
	// outside that entity's device-class vocabulary (RUL-160).
	//
	// It exists because RUL-161 makes a single-target dispatch atomic and
	// unaggregated, so those refusals have no result channel at all: on the edge
	// engine that is correct and deliberate (nothing is listening, and a rule
	// firing on a device that has gone away must not become an error). On a
	// MANUAL run it is not: the caller is a human who pressed a button and is
	// owed the reason their command did not go out. A run that reported
	// "disposition: ran" with an empty command list, because the one command it
	// carried was silently absorbed here, is the same defect this whole track
	// exists to close, one level down.
	//
	// Leaving it nil restores the exact prior behaviour, which is what every
	// edge context does.
	Rejected *[]CommandResult

	// Signage performs the app-class signage actions (RUL-234/235); nil on every
	// edge context, where such a rule never arrives. SignageOutcomes is the
	// caller's optional recorder for what each one did (RUL-236), exactly as
	// Outcomes is for a device dispatch.
	Signage         SignageSink
	SignageOutcomes *[]SignageOutcome

	// EvalExpr live-evaluates a `log` message (RUL-200) and each `params` value
	// (RUL-393) at dispatch. The engine supplies the closed-grammar evaluator with
	// the firing trigger's subject entity as the edge scope (RUL-282). When nil —
	// a caller with no Expression support (Part-1/2 unit contexts) — an
	// Expression-valued field is treated as a plain JSON literal used as-is, so a
	// literal message/param still dispatches unchanged.
	EvalExpr ExprEvaluator
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
		case vocab.ActionPlayCast, vocab.ActionShowAlert, vocab.ActionDismissAlert:
			runSignage(ctx, m)
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
		if EvalConditionAt(ctx.Reg, ctx.Snap, ctx.Clk, ctx.Loc, ctx.Vars, b.Condition) {
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

// runLog implements RUL-200: `message` is an Expression evaluated to a string at
// dispatch (ctx.EvalExpr). A plain literal string message is a literal
// Expression (RUL-280) that evaluates to itself; an {"expr":...} message is
// evaluated live. The action is skipped (fail-closed — no fabricated log entry,
// RUL-284), never an error, when the message Expression fails closed OR
// evaluates to a non-string value (RUL-200 requires a string). level defaults to
// "info" when absent.
func runLog(ctx ActionContext, m model.Member) {
	var spec struct {
		Message json.RawMessage `json:"message"`
		Level   string          `json:"level"`
	}
	if err := json.Unmarshal(m.Raw, &spec); err != nil || len(spec.Message) == 0 {
		return
	}
	v, ok := evalParam(ctx, spec.Message)
	if !ok {
		return // message Expression failed closed (RUL-284) — no fabricated entry
	}
	msg, ok := v.(string)
	if !ok {
		return // evaluated to a non-string; RUL-200 requires a string (fail-closed)
	}
	level := spec.Level
	if level == "" {
		level = "info"
	}
	if ctx.Logs != nil {
		*ctx.Logs = append(*ctx.Logs, LogEntry{Level: level, Message: msg})
	}
}

// evalParam live-evaluates one Expression-valued action field (RUL-200/393): a
// `log` message or a `params` value, each a literal OR an {"expr":...} pipeline,
// resolved at dispatch through ctx.EvalExpr (never a frozen closure, RUL-393).
// When no evaluator is wired (ctx.EvalExpr == nil) the raw JSON is used as a
// plain literal — the degenerate no-Expression path that keeps literal
// messages/params dispatching unchanged. ok=false means the Expression failed
// closed (RUL-284) and the caller must skip the field.
func evalParam(ctx ActionContext, raw json.RawMessage) (any, bool) {
	if ctx.EvalExpr != nil {
		return ctx.EvalExpr(raw)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}

// evalParams live-evaluates every `params` value (RUL-393): each is a literal or
// an Expression evaluated at dispatch, subject to the edge entity-scope
// (RUL-282). A single value failing closed (RUL-284) fails the whole map
// (ok=false) — a device command is never dispatched on a partially-evaluated or
// fabricated parameter set. An absent/empty params map yields a nil map, ok.
func evalParams(ctx ActionContext, raw map[string]json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		pv, ok := evalParam(ctx, v)
		if !ok {
			return nil, false
		}
		out[k] = pv
	}
	return out, true
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
		Command string                     `json:"command"`
		Params  map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(m.Raw, &spec); err != nil || spec.Command == "" {
		return
	}
	// RUL-393: each params value is a literal or an Expression, live-evaluated at
	// dispatch (never frozen). A fail-closed params Expression (RUL-284) skips the
	// whole dispatch rather than sending a command on a fabricated parameter set.
	params, ok := evalParams(ctx, spec.Params)
	if !ok {
		return
	}

	targets := resolveTargets(ctx, *m.EntityRef)
	if len(targets) == 0 {
		return
	}
	if len(targets) == 1 {
		dispatchOne(ctx, targets[0], spec.Command, params)
		return
	}
	recordOutcome(ctx, dispatchAll(ctx, targets, spec.Command, params))
}

// runSignage implements the three RUL-234/235 signage actions: decode the
// action's ScreenRef and its own members, hand them to ctx.Signage, and record
// the outcome (RUL-236).
//
// Like a failing preset-batch command, a failing signage write never halts the
// rest of the rule's action sequence — the sink reports per-screen results and
// RunActions simply continues. A nil sink (every edge context) or an
// undecodable action body is a skip, never an error: an app-class action
// reaching an edge evaluator is out of that evaluator's scope by construction,
// which is the same posture the default branch takes for notify/variable_write.
func runSignage(ctx ActionContext, m model.Member) {
	if ctx.Signage == nil {
		return
	}
	var spec struct {
		ScreenID   string `json:"screen_id"`
		Selector   string `json:"selector"`
		CastID     string `json:"cast_id"`
		Message    string `json:"message"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if err := json.Unmarshal(m.Raw, &spec); err != nil {
		return
	}
	ref := ScreenRef{ScreenID: spec.ScreenID, Selector: spec.Selector}

	var out SignageOutcome
	switch m.Type {
	case vocab.ActionPlayCast:
		if spec.CastID == "" {
			// RUL-234 requires cast_id. A play_cast with none names no content,
			// so there is nothing to put on a screen — but it is REPORTED as a
			// failed action rather than skipped. compile.Validate refuses this
			// shape at authoring (SIGNAGE_CONTENT_AMBIGUOUS), so reaching here at
			// all means a rule got in some other way (a row written before that
			// gate existed, a hand-edited store); answering an operator's Run with
			// `ran`, an empty effect report and an unchanged screen is the one
			// answer that tells them nothing at all.
			out = FailedSignageOutcome(m.Type,
				"a play_cast action MUST declare cast_id (rules/1 RUL-234); this action declares none")
			break
		}
		out = ctx.Signage.PlayCast(ref, spec.CastID)
	case vocab.ActionShowAlert:
		if (spec.CastID == "") == (spec.Message == "") {
			// RUL-235: exactly one of cast_id/message. Neither is nothing to show;
			// both is two different things to show with no rule ranking them.
			// Reported, not skipped, for the reason play_cast gives above.
			reason := "a show_alert action MUST declare exactly one of cast_id or message (rules/1 RUL-235); this action declares neither"
			if spec.CastID != "" {
				reason = "a show_alert action MUST declare exactly one of cast_id or message (rules/1 RUL-235); this action declares both"
			}
			out = FailedSignageOutcome(m.Type, reason)
			break
		}
		out = ctx.Signage.ShowAlert(ref, spec.CastID, spec.Message, spec.TTLSeconds)
	case vocab.ActionDismissAlert:
		out = ctx.Signage.DismissAlert(ref)
	default:
		return
	}
	if ctx.SignageOutcomes != nil {
		*ctx.SignageOutcomes = append(*ctx.SignageOutcomes, out)
	}
}

// SignageOutcomeFor computes RUL-236's three-value outcome from a per-screen
// result list, reusing RUL-172's own definition (outcomeFor) rather than a
// second copy of the same three-way count — the contract says "the same
// three-value outcome", and two implementations of one rule is how they stop
// being the same. An EMPTY list is `complete`: a selector matching no screen is
// a legal no-op (RUL-233), not a failure.
//
// It is exported because the sink — which lives on the app peer, outside this
// package — is what assembles the per-screen results, and it must classify them
// by the same rule an aggregated device dispatch is classified by.
func SignageOutcomeFor(action string, results []ScreenResult) SignageOutcome {
	cmd := make([]CommandResult, 0, len(results))
	for _, r := range results {
		cmd = append(cmd, CommandResult{Target: r.ScreenID, OK: r.OK, Error: r.Error})
	}
	return SignageOutcome{Action: action, Outcome: outcomeFor(cmd), Screens: results}
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
// pass/fail is not recorded into ctx.Outcomes. A command not drawn from the
// entity's device class's command vocabulary (RUL-160) is rejected the same
// way a nil Sink is: no dispatch happens, and the rejection is absorbed rather
// than raised (single-entity dispatch has no OUTCOME channel to report into).
//
// "Absorbed" is not the same as invisible: a caller that supplied ctx.Rejected
// is told what was refused and why, in the same CommandResult shape a
// multi-target dispatch reports a failure in. See Rejected's own doc for why a
// manual run needs that and the edge engine does not.
func dispatchOne(ctx ActionContext, entityID, command string, params map[string]any) {
	if ctx.Sink == nil {
		recordRejected(ctx, entityID, command, "no command sink configured")
		return
	}
	if !commandAllowed(ctx, entityID, command) {
		recordRejected(ctx, entityID, command, "command not declared in entity's device class command vocabulary")
		return
	}
	_ = ctx.Sink.Dispatch(entityID, command, params)
}

// recordRejected notes one pre-dispatch refusal into ctx.Rejected, when the
// caller supplied a recorder. Reusing CommandResult rather than a second shape
// is deliberate: to a reader of a run report, "this target was refused before
// dispatch" and "this target failed at dispatch" are the same fact with
// different reasons, and two shapes would make a consumer merge them by hand.
func recordRejected(ctx ActionContext, entityID, command, reason string) {
	if ctx.Rejected == nil {
		return
	}
	*ctx.Rejected = append(*ctx.Rejected, CommandResult{Target: entityID, Command: command, OK: false, Error: reason})
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

// dispatchResult dispatches one command and reports its CommandResult. A
// command not drawn from the entity's device class's command vocabulary
// (RUL-160, device-class-registry/1 REG-050/052) is rejected before ever
// reaching ctx.Sink — reported as a failed CommandResult, same as any other
// dispatch failure (RUL-171/172).
func dispatchResult(ctx ActionContext, entityID, command string, params map[string]any) CommandResult {
	if ctx.Sink == nil {
		return CommandResult{Target: entityID, Command: command, OK: false, Error: "no command sink configured"}
	}
	if !commandAllowed(ctx, entityID, command) {
		return CommandResult{Target: entityID, Command: command, OK: false, Error: "command not declared in entity's device class command vocabulary"}
	}
	if err := ctx.Sink.Dispatch(entityID, command, params); err != nil {
		return CommandResult{Target: entityID, Command: command, OK: false, Error: err.Error()}
	}
	return CommandResult{Target: entityID, Command: command, OK: true}
}

// commandAllowed implements RUL-160's command-vocabulary restriction: command
// MUST be drawn from entityID's resolved device class's command vocabulary,
// declared in the device-class registry (registry.Registry.CommandExists).
// ctx.Reg == nil means no registry has been wired into this ActionContext at
// all — the same "not configured, nothing to enforce" posture ctx.Sink and
// ctx.Presets already carry elsewhere in this file — so the check is skipped
// rather than treated as a violation. Once a registry IS configured, an
// entityID that does not resolve in ctx.Snap is an unresolvable reference and
// fails closed (never permitted), per this contract's fail-closed posture for
// unresolvable references.
func commandAllowed(ctx ActionContext, entityID, command string) bool {
	if ctx.Reg == nil {
		return true
	}
	e, ok := ctx.Snap.Get(entityID)
	if !ok {
		return false
	}
	return ctx.Reg.CommandExists(e.DeviceClass, command)
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
