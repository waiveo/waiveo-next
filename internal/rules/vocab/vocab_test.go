package vocab

import "testing"

func TestTriggerClasses(t *testing.T) {
	edge := []string{"state", "numeric", "time", "time_pattern", "sun"}
	app := []string{"template", "event", "webhook"}
	for _, ty := range edge {
		if got := ClassOf(TriggerKind, ty); got != Edge {
			t.Errorf("trigger %q class = %v, want Edge", ty, got)
		}
	}
	for _, ty := range app {
		if got := ClassOf(TriggerKind, ty); got != App {
			t.Errorf("trigger %q class = %v, want App", ty, got)
		}
	}
	if got := ClassOf(TriggerKind, "geofence"); got != Unknown {
		t.Errorf("unknown trigger class = %v, want Unknown", got)
	}
}

func TestConditionClasses(t *testing.T) {
	edge := []string{"state", "numeric", "time", "sun", "variable"}
	for _, ty := range edge {
		if got := ClassOf(ConditionKind, ty); got != Edge {
			t.Errorf("condition %q class = %v, want Edge", ty, got)
		}
	}
	if got := ClassOf(ConditionKind, "template"); got != App {
		t.Errorf("template condition class = %v, want App", got)
	}
}

func TestActionAndModeClasses(t *testing.T) {
	if ClassOf(ActionKind, "device_command") != Edge || ClassOf(ActionKind, "preset_batch") != Edge {
		t.Error("device_command/preset_batch must be Edge")
	}
	if ClassOf(ActionKind, "delay") != Edge || ClassOf(ActionKind, "log") != Edge {
		t.Error("delay/log must be Edge")
	}
	if ClassOf(ActionKind, "notify") != App || ClassOf(ActionKind, "workflow_start") != App || ClassOf(ActionKind, "variable_write") != App {
		t.Error("notify/workflow_start/variable_write must be App")
	}
	// RUL-234/235: the signage actions write app-peer authored state (a screen
	// row's program override, data-model/1 DAT-004c), so a rule carrying one is
	// app-class and the edge engine never loads it. Edge here would ship the
	// rule to a relay that cannot perform it — the "accepts work it never
	// performs" shape, at the classification level.
	for _, ty := range []string{"play_cast", "show_alert", "dismiss_alert"} {
		if got := ClassOf(ActionKind, ty); got != App {
			t.Errorf("signage action %q class = %v, want App", ty, got)
		}
	}
	if ModeClass("") != Edge || ModeClass("single") != Edge || ModeClass("restart") != Edge {
		t.Error("'', single, restart modes must be Edge")
	}
	if ModeClass("queued") != App || ModeClass("parallel") != App {
		t.Error("queued/parallel modes must be App")
	}
	if ModeClass("turbo") != Unknown {
		t.Error("unknown mode must be Unknown")
	}
}
