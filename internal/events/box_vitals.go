package events

import "encoding/json"

// box.vitals (EVT-070) publishes a relay's own physical/operational health
// snapshot: CPU temperature, active throttle/warning flags, undervoltage
// detection, and free operational-storage headroom. Per EVT-071
// throttled_flags MUST be present as an array even when no flag is active —
// never absent — since a subscriber checks for emptiness, never presence.

// validateBoxVitals enforces the EVT-070 box.vitals field definition:
// relay_id is a ULID; cpu_temp is a required number; throttled_flags is a
// required array of strings (present-even-empty, EVT-071); undervoltage is a
// required boolean; disk_headroom is a required integer; sd_health is an
// optional object. A missing or absent-when-required field is an EVT-013
// delivery-gate rejection.
func validateBoxVitals(raw json.RawMessage) error {
	m, err := payloadObject(raw, "box.vitals")
	if err != nil {
		return err
	}
	if err := requireULIDField(m, "relay_id"); err != nil {
		return err
	}
	if _, err := requireNumberField(m, "cpu_temp"); err != nil {
		return err
	}
	if _, err := requireStringArrayField(m, "throttled_flags"); err != nil {
		return err
	}
	if _, err := requireBoolField(m, "undervoltage"); err != nil {
		return err
	}
	if _, err := requireIntField(m, "disk_headroom"); err != nil {
		return err
	}
	if err := optionalObjectField(m, "sd_health"); err != nil {
		return err
	}
	return nil
}
