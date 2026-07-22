package events

import "encoding/json"

// device.heartbeat (EVT-060) publishes a device's periodic liveness/status
// report: its canonical power_state and app_state, and the content currently
// playing (or null when nothing is). EVT-061 additionally requires
// power_state be a member of the reporting device's OWN device class's
// states (device-class-registry/1 REG-020) and app_state a member of its
// app_type attribute enum (REG-064) — that class-membership cross-check is a
// producer-side guarantee this validator cannot make: internal/events is a
// leaf and never imports internal/deviceclass, which remains the single
// vocabulary source a producer consults before emitting a heartbeat. This
// file validates the payload's structural shape only (EVT-060); EVT-061's
// cross-check is a documented seam, not implemented here.

// The cost/retention classing for a device.heartbeat event, co-located with the
// schema (EVT-060) so class.go indexes it without duplicating the strings. The
// corpus pins neither; both are operational-telemetry-tier, matching the events1
// conformance driver's fixture class (device.heartbeat is a latest-only telemetry
// schema on the relay channel, REL-094/095).
const (
	deviceHeartbeatCostClass      = "telemetry"
	deviceHeartbeatRetentionClass = "telemetry-standard"
)

// validateDeviceHeartbeat enforces the EVT-060 device.heartbeat field
// definition: device_id is a ULID; power_state/app_state are required
// strings; now_playing_content_id is a required field that is either a
// string or null (nothing currently playing). A missing required field is an
// EVT-013 delivery-gate rejection.
func validateDeviceHeartbeat(raw json.RawMessage) error {
	m, err := payloadObject(raw, "device.heartbeat")
	if err != nil {
		return err
	}
	if err := requireULIDField(m, "device_id"); err != nil {
		return err
	}
	if err := requireStringField(m, "power_state"); err != nil {
		return err
	}
	if err := requireStringField(m, "app_state"); err != nil {
		return err
	}
	if err := requireNullableStringField(m, "now_playing_content_id"); err != nil {
		return err
	}
	return nil
}
