package manifest

import "testing"

// TestValidateAutomationEventNotNamespaced: a contributes.automation.events
// name that is not <pack-id>.<local-name> namespaced violates MAN-090.
func TestValidateAutomationEventNotNamespaced(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Contributes.Automation.Events[0].Name = "forecast_updated"
	errs := Validate(m, testHost())
	if !hasField(errs, "contributes.automation.events[0].name") {
		t.Fatalf("expected a name error for an un-namespaced event, got %+v", errs)
	}
}

// TestValidateAutomationEventNamespacedClean: an event name namespaced as
// <pack-id>.<local-name> validates clean (MAN-090).
func TestValidateAutomationEventNamespacedClean(t *testing.T) {
	m := loadManifest(t, man020File)
	errs := Validate(m, testHost())
	if hasField(errs, "contributes.automation.events[0].name") {
		t.Fatalf("expected the fixture's already-namespaced event to validate clean, got %+v", errs)
	}
}

// TestValidateAutomationEventDuplicateName: two events sharing a name within
// the same pack violate MAN-090's pack-unique requirement.
func TestValidateAutomationEventDuplicateName(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Contributes.Automation.Events = append(m.Contributes.Automation.Events, m.Contributes.Automation.Events[0])
	errs := Validate(m, testHost())
	if !hasField(errs, "contributes.automation.events[1].name") {
		t.Fatalf("expected a duplicate-name error on contributes.automation.events[1].name, got %+v", errs)
	}
}

// TestValidateAutomationActionNotInActions: a contributes.automation.actions
// entry with no same-named actions[] entry violates MAN-091 — the
// automation-facing view MUST NOT diverge into a second registry.
func TestValidateAutomationActionNotInActions(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Actions = nil
	errs := Validate(m, testHost())
	if !hasField(errs, "contributes.automation.actions[0].name") {
		t.Fatalf("expected a name error for an automation action absent from actions, got %+v", errs)
	}
}

// TestValidateAutomationActionExecutionInvalid: an execution value outside
// {relay-command, app-service} violates MAN-091.
func TestValidateAutomationActionExecutionInvalid(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Contributes.Automation.Actions[0].Execution = "cloud-function"
	errs := Validate(m, testHost())
	if !hasField(errs, "contributes.automation.actions[0].execution") {
		t.Fatalf("expected an execution error for %q, got %+v", "cloud-function", errs)
	}
}

// TestValidateAutomationTriggerUnresolvable: a trigger whose matches names
// neither a host-registered device class nor one of this pack's own events
// violates MAN-092 with a typed TRIGGER_UNRESOLVABLE error.
func TestValidateAutomationTriggerUnresolvable(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Contributes.Automation.Triggers = []AutomationTrigger{
		{
			Name:         "unknown-trigger",
			Msg:          "msg:trigger.unknown",
			Matches:      []byte(`{"deviceClass":"printer"}`),
			ParamsSchema: []byte(`{"type":"object"}`),
		},
	}
	errs := Validate(m, testHost())
	if !hasCode(errs, "TRIGGER_UNRESOLVABLE") {
		t.Fatalf("expected a TRIGGER_UNRESOLVABLE error, got %+v", errs)
	}
	if !hasField(errs, "contributes.automation.triggers[0].matches") {
		t.Fatalf("expected the error to name contributes.automation.triggers[0].matches, got %+v", errs)
	}
}

// TestValidateAutomationTriggerResolvesDeviceClass: a trigger whose matches
// names a host-registered device class resolves clean (MAN-092).
func TestValidateAutomationTriggerResolvesDeviceClass(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Contributes.Automation.Triggers = []AutomationTrigger{
		{
			Name:         "player-online",
			Msg:          "msg:trigger.playerOnline",
			Matches:      []byte(`{"deviceClass":"media-player"}`),
			ParamsSchema: []byte(`{"type":"object"}`),
		},
	}
	errs := Validate(m, testHost())
	if hasCode(errs, "TRIGGER_UNRESOLVABLE") {
		t.Fatalf("expected a device-class-resolved trigger to validate clean, got %+v", errs)
	}
}

// TestValidateAutomationTriggerResolvesOwnEvent: a trigger whose matches names
// one of this same pack's contributes.automation.events entries resolves
// clean (MAN-092).
func TestValidateAutomationTriggerResolvesOwnEvent(t *testing.T) {
	m := loadManifest(t, man020File)
	eventName := m.Contributes.Automation.Events[0].Name
	m.Contributes.Automation.Triggers = []AutomationTrigger{
		{
			Name:         "forecast-changed",
			Msg:          "msg:trigger.forecastChanged",
			Matches:      []byte(`{"event":"` + eventName + `"}`),
			ParamsSchema: []byte(`{"type":"object"}`),
		},
	}
	errs := Validate(m, testHost())
	if hasCode(errs, "TRIGGER_UNRESOLVABLE") {
		t.Fatalf("expected an own-event-resolved trigger to validate clean, got %+v", errs)
	}
}
