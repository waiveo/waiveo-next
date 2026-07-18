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
// (edge) and notify RUL-210, variable_write RUL-220, workflow_start RUL-230
// (app); modes single/restart edge, queued/parallel app-only (RUL-240).
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
	"notify": App, "variable_write": App, "workflow_start": App,
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
