package events

import (
	"encoding/json"
	"errors"
	"testing"
)

// validDeviceHeartbeatPayload is the EVT-060 corpus payload: a heartbeat
// reporting canonical power_state/app_state with nothing currently playing.
func validDeviceHeartbeatPayload() json.RawMessage {
	return json.RawMessage(`{
		"device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YA",
		"power_state": "on",
		"app_state": "app",
		"now_playing_content_id": null
	}`)
}

// TestDeviceHeartbeat_CorpusPayloadValidates: the EVT-060 corpus payload
// validates against its own field definition, including a null
// now_playing_content_id (nothing currently playing).
func TestDeviceHeartbeat_CorpusPayloadValidates(t *testing.T) {
	if err := validateDeviceHeartbeat(validDeviceHeartbeatPayload()); err != nil {
		t.Fatalf("the EVT-060 corpus device.heartbeat payload must validate; got %v", err)
	}
}

// TestDeviceHeartbeat_NowPlayingStringValid: now_playing_content_id also
// accepts a non-null string (something IS currently playing).
func TestDeviceHeartbeat_NowPlayingStringValid(t *testing.T) {
	p := json.RawMessage(`{
		"device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YA",
		"power_state": "on",
		"app_state": "app",
		"now_playing_content_id": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85"
	}`)
	if err := validateDeviceHeartbeat(p); err != nil {
		t.Fatalf("a string now_playing_content_id must validate; got %v", err)
	}
}

// TestDeviceHeartbeat_NowPlayingMissingRejected: now_playing_content_id MUST
// be present (null when nothing is playing) — outright absence is an EVT-013
// rejection, distinct from an explicit null.
func TestDeviceHeartbeat_NowPlayingMissingRejected(t *testing.T) {
	p := json.RawMessage(`{
		"device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YA",
		"power_state": "on",
		"app_state": "app"
	}`)
	err := validateDeviceHeartbeat(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "now_playing_content_id" {
		t.Fatalf("a missing now_playing_content_id must be rejected on now_playing_content_id; got %v", err)
	}
}

// TestDeviceHeartbeat_BadDeviceIDULIDRejected: device_id is a ULID field
// (EVT-060).
func TestDeviceHeartbeat_BadDeviceIDULIDRejected(t *testing.T) {
	p := json.RawMessage(`{
		"device_id": "not-a-ulid",
		"power_state": "on",
		"app_state": "app",
		"now_playing_content_id": null
	}`)
	err := validateDeviceHeartbeat(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "device_id" {
		t.Fatalf("a non-ULID device_id must be rejected on device_id (EVT-060); got %v", err)
	}
}

// TestDeviceHeartbeat_MissingPowerStateRejected: power_state is a required
// string (EVT-060).
func TestDeviceHeartbeat_MissingPowerStateRejected(t *testing.T) {
	p := json.RawMessage(`{
		"device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YA",
		"app_state": "app",
		"now_playing_content_id": null
	}`)
	err := validateDeviceHeartbeat(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "power_state" {
		t.Fatalf("a missing power_state must be rejected on power_state; got %v", err)
	}
}

// TestDeviceHeartbeat_DeliversThroughValidate: a well-formed device.heartbeat
// envelope passes the full EVT-013 gate (delivered).
func TestDeviceHeartbeat_DeliversThroughValidate(t *testing.T) {
	env := validEnvelope()
	env.Schema = SchemaDeviceHeartbeat
	env.Payload = validDeviceHeartbeatPayload()
	if err := Validate(env); err != nil {
		t.Fatalf("a well-formed device.heartbeat envelope must deliver (EVT-013); got %v", err)
	}
}
