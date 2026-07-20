package manifest

import (
	"encoding/json"
	"strconv"
	"strings"
)

// validAutomationActionExecutions is the MAN-091 execution enum.
var validAutomationActionExecutions = map[string]bool{
	"relay-command": true, "app-service": true,
}

// automationTriggerMatch is the shape this contract recognizes for a
// contributes.automation.triggers[].matches value (MAN-092): a deviceClass
// reference, resolved against the host's flat device-class registry, or an
// event reference naming one of this same pack's own contributes.automation.
// events entries. The finer-grained capability/state-or-event pattern grammar
// a device class itself exposes is device-class-registry/1's own concern —
// HostRegistries carries only a flat recognized-name set here (leaf
// discipline: this package cites that registry, it does not reimplement it).
type automationTriggerMatch struct {
	DeviceClass string `json:"deviceClass,omitempty"`
	Event       string `json:"event,omitempty"`
}

// validateAutomation enforces MAN-090/091/092. contributes.automation is
// optional; when absent, nothing here applies.
//
//   - MAN-090: every contributes.automation.events[].name is unique within the
//     pack and namespaced as <pack-id>.<local-name>.
//   - MAN-091: every contributes.automation.actions[] entry also appears in
//     actions (MAN-100) under the same name, and declares a recognized
//     execution class (relay-command|app-service) — the automation-facing view
//     is a subset of the pack's declared actions, never a second registry.
//   - MAN-092: every contributes.automation.triggers[].matches resolves
//     against the host device-class registry or one of this same pack's own
//     automation.events entries; an unresolvable trigger fails with a typed
//     TRIGGER_UNRESOLVABLE error.
func validateAutomation(m PackManifest, host HostRegistries) []Error {
	if m.Contributes.Automation == nil {
		return nil
	}
	a := m.Contributes.Automation
	var errs []Error

	prefix := m.ID + "."
	eventNames := map[string]bool{}
	seenEvents := map[string]bool{}
	for i, e := range a.Events {
		at := "contributes.automation.events[" + strconv.Itoa(i) + "]"

		if !strings.HasPrefix(e.Name, prefix) || len(e.Name) == len(prefix) {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".name",
				`event name "` + e.Name + `" MUST be namespaced as "` + prefix + `<local-name>" (MAN-090)`})
		}
		if seenEvents[e.Name] {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".name",
				`event name "` + e.Name + `" is declared more than once (MAN-090)`})
		}
		seenEvents[e.Name] = true
		eventNames[e.Name] = true
	}

	declaredActions := map[string]bool{}
	for _, act := range m.Actions {
		declaredActions[act.Name] = true
	}
	for i, aa := range a.Actions {
		at := "contributes.automation.actions[" + strconv.Itoa(i) + "]"

		if !declaredActions[aa.Name] {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".name",
				`automation action "` + aa.Name + `" MUST also appear in actions under the same name (MAN-091)`})
		}
		if !validAutomationActionExecutions[aa.Execution] {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".execution",
				`automation action execution "` + aa.Execution + `" MUST be one of relay-command, app-service (MAN-091)`})
		}
	}

	for i, tr := range a.Triggers {
		at := "contributes.automation.triggers[" + strconv.Itoa(i) + "]"

		var match automationTriggerMatch
		_ = json.Unmarshal(tr.Matches, &match)

		// unresolved names the offending value (and which alternative,
		// deviceClass or event, was actually supplied) so a pack author sees
		// exactly what failed to resolve, matching every sibling
		// registry-recognition error in this package.
		var unresolved string
		switch {
		case match.Event != "":
			if !eventNames[match.Event] {
				unresolved = `event "` + match.Event + `" is not one of this pack's own automation.events entries`
			}
		case match.DeviceClass != "":
			if !host.DeviceClasses[match.DeviceClass] {
				unresolved = `device class "` + match.DeviceClass + `" is not in the host device-class registry`
			}
		default:
			unresolved = "matches declares neither a deviceClass nor an event"
		}
		if unresolved != "" {
			errs = append(errs, Error{"TRIGGER_UNRESOLVABLE", at + ".matches",
				unresolved + " (MAN-092)"})
		}
	}

	return errs
}
