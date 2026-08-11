// Package vocab is the rules/1 closed-vocabulary class table: for every
// trigger/condition/action leaf member and every mode, it states the execution
// class (edge or app) contracts/rules-1.md fixes per member (RUL-002). It is a
// pure lookup — composition members (and/or/not) and choose actions are NOT in
// the table; their class is computed recursively by the compiler (RUL-100/180),
// which consults this table only for leaf discriminants.
//
// Each entry's class is the one stated in that member's own contract section:
// triggers RUL-020/030/040/050/060 (edge) and RUL-070/080/090 (app); conditions
// RUL-110/120/130/141/150 (edge) and RUL-160 template (app); actions
// device_command RUL-160, preset_batch RUL-170, delay RUL-190, log RUL-200
// (edge) and notify RUL-210, variable_write RUL-220, workflow_start RUL-230,
// play_cast RUL-234, show_alert/dismiss_alert RUL-235 (app); modes
// single/restart edge, queued/parallel app-only (RUL-240).
package vocab

// Class is a member's execution class.
type Class int

const (
	// Unknown means the type is not a member of the closed vocabulary
	// (RUL-001: it MUST fail compilation, never be treated as app-class).
	Unknown Class = iota
	Edge
	App
)

func (c Class) String() string {
	switch c {
	case Edge:
		return "edge"
	case App:
		return "app"
	default:
		return "unknown"
	}
}

// Kind selects which member vocabulary a type is looked up in.
type Kind int

const (
	TriggerKind Kind = iota
	ConditionKind
	ActionKind
)

var triggers = map[string]Class{
	"state": Edge, "numeric": Edge, "time": Edge, "time_pattern": Edge, "sun": Edge,
	"template": App, "event": App, "webhook": App,
}

var conditions = map[string]Class{
	"state": Edge, "numeric": Edge, "time": Edge, "sun": Edge, "variable": Edge,
	"template": App,
}

var actions = map[string]Class{
	"device_command": Edge, "preset_batch": Edge, "choose": Edge, "delay": Edge, "log": Edge,
	"notify": App, ActionVariableWrite: App, "workflow_start": App,
	// The signage actions (RUL-234/235). App-class unconditionally, and for a
	// structural reason rather than a policy one: each writes a screen row's
	// authored program override (data-model/1 DAT-004c), which is app-peer state
	// the edge engine neither holds nor may write — the same argument RUL-220
	// makes for variable_write.
	"play_cast": App, "show_alert": App, "dismiss_alert": App,
}

// Class returns the execution class of a leaf member type within kind, or
// Unknown if the type is outside the closed vocabulary. "choose" resolves to
// Edge here as its own member class; whether a given choose is edge overall
// still depends on its nested actions (RUL-180/182), computed by the caller.
func ClassOf(kind Kind, typ string) Class {
	var table map[string]Class
	switch kind {
	case TriggerKind:
		table = triggers
	case ConditionKind:
		table = conditions
	case ActionKind:
		table = actions
	default:
		return Unknown
	}
	if c, ok := table[typ]; ok {
		return c
	}
	return Unknown
}

// The signage action types (RUL-234/235) — the three actions that target
// SCREENS rather than entities, and therefore carry a ScreenRef (RUL-233) where
// a device-affecting action carries an EntityRef.
const (
	ActionPlayCast     = "play_cast"
	ActionShowAlert    = "show_alert"
	ActionDismissAlert = "dismiss_alert"
)

// ActionVariableWrite is the RUL-220 action type: the one action that targets
// neither an entity nor a screen but a data-model/1 variable row (DAT-130), and
// so carries a `variable` name in place of any Ref at all. It is named here,
// beside the signage constants and for the same reason, so the evaluator's
// dispatch arm and the `actions` class table above cannot drift apart on the
// spelling of a member one of them dispatches.
const ActionVariableWrite = "variable_write"

// IsSignageAction reports whether typ is one of the three RUL-233 ScreenRef-
// carrying action types. It is the ONE definition of that set: the compiler
// consults it to know which actions to apply the ScreenRef ambiguity check to
// (RUL-233 / SCREEN_REF_AMBIGUOUS), and the evaluator consults the same three
// constants to dispatch them. Two independently-maintained lists would let an
// action be dispatchable without ever being checked, which is the compile gate
// silently not covering a member of the vocabulary it gates.
func IsSignageAction(typ string) bool {
	switch typ {
	case ActionPlayCast, ActionShowAlert, ActionDismissAlert:
		return true
	}
	return false
}

// ModeClass returns a rule mode's class: "" (omitted, defaults to single),
// "single", "restart" are Edge; "queued", "parallel" are App; anything else is
// Unknown (RUL-240).
func ModeClass(mode string) Class {
	switch mode {
	case "", "single", "restart":
		return Edge
	case "queued", "parallel":
		return App
	default:
		return Unknown
	}
}
