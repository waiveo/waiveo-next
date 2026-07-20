package events

import (
	"encoding/json"
	"errors"
	"testing"
)

// validBoxVitalsPayload is the EVT-070 corpus payload: a relay reports its
// vitals with no active throttle flags — throttled_flags is present as an
// empty array, never absent.
func validBoxVitalsPayload() json.RawMessage {
	return json.RawMessage(`{
		"relay_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZF",
		"cpu_temp": 46.5,
		"throttled_flags": [],
		"undervoltage": false,
		"disk_headroom": 2147483648
	}`)
}

// TestBoxVitals_CorpusPayloadValidates: the EVT-070 corpus payload validates
// against its own field definition.
func TestBoxVitals_CorpusPayloadValidates(t *testing.T) {
	if err := validateBoxVitals(validBoxVitalsPayload()); err != nil {
		t.Fatalf("the EVT-070 corpus box.vitals payload must validate; got %v", err)
	}
}

// TestBoxVitals_ThrottledFlagsAbsentRejected: throttled_flags MUST be present
// as an array, never absent, even when no flag is active (EVT-071).
func TestBoxVitals_ThrottledFlagsAbsentRejected(t *testing.T) {
	p := json.RawMessage(`{
		"relay_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZF",
		"cpu_temp": 46.5,
		"undervoltage": false,
		"disk_headroom": 2147483648
	}`)
	err := validateBoxVitals(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "throttled_flags" {
		t.Fatalf("an absent throttled_flags must be rejected on throttled_flags (EVT-071); got %v", err)
	}
}

// TestBoxVitals_ThrottledFlagsPopulatedValid: throttled_flags also accepts a
// populated array of flag strings.
func TestBoxVitals_ThrottledFlagsPopulatedValid(t *testing.T) {
	p := json.RawMessage(`{
		"relay_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZF",
		"cpu_temp": 68.2,
		"throttled_flags": ["undervoltage-now", "throttled-now"],
		"undervoltage": true,
		"disk_headroom": 512000000
	}`)
	if err := validateBoxVitals(p); err != nil {
		t.Fatalf("a populated throttled_flags array must validate; got %v", err)
	}
}

// TestBoxVitals_BadRelayIDULIDRejected: relay_id is a ULID field (EVT-070).
func TestBoxVitals_BadRelayIDULIDRejected(t *testing.T) {
	p := json.RawMessage(`{
		"relay_id": "not-a-ulid",
		"cpu_temp": 46.5,
		"throttled_flags": [],
		"undervoltage": false,
		"disk_headroom": 2147483648
	}`)
	err := validateBoxVitals(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "relay_id" {
		t.Fatalf("a non-ULID relay_id must be rejected on relay_id (EVT-070); got %v", err)
	}
}

// TestBoxVitals_NullUndervoltageRejected: a JSON null for the required
// boolean undervoltage must be an EVT-013 rejection — encoding/json unmarshals
// null as a no-op, so a null must never silently pass as a zero-value false.
func TestBoxVitals_NullUndervoltageRejected(t *testing.T) {
	p := json.RawMessage(`{
		"relay_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZF",
		"cpu_temp": 46.5,
		"throttled_flags": [],
		"undervoltage": null,
		"disk_headroom": 2147483648
	}`)
	err := validateBoxVitals(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "undervoltage" {
		t.Fatalf("a null undervoltage must be an EVT-013 rejection on undervoltage, not a silent false; got %v", err)
	}
}

// TestBoxVitals_SDHealthOptionalObjectWhenPresent: sd_health is optional
// (EVT-070) — absent is fine (the corpus payload already omits it); when
// present it must be an object.
func TestBoxVitals_SDHealthOptionalObjectWhenPresent(t *testing.T) {
	valid := json.RawMessage(`{
		"relay_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZF",
		"cpu_temp": 46.5,
		"throttled_flags": [],
		"undervoltage": false,
		"disk_headroom": 2147483648,
		"sd_health": {"wear_level": "low"}
	}`)
	if err := validateBoxVitals(valid); err != nil {
		t.Fatalf("an sd_health object must validate (EVT-070); got %v", err)
	}

	invalid := json.RawMessage(`{
		"relay_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZF",
		"cpu_temp": 46.5,
		"throttled_flags": [],
		"undervoltage": false,
		"disk_headroom": 2147483648,
		"sd_health": "low"
	}`)
	err := validateBoxVitals(invalid)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "sd_health" {
		t.Fatalf("a non-object sd_health must be rejected on sd_health; got %v", err)
	}
}

// TestBoxVitals_DeliversThroughValidate: a well-formed box.vitals envelope
// passes the full EVT-013 gate (delivered).
func TestBoxVitals_DeliversThroughValidate(t *testing.T) {
	env := validEnvelope()
	env.Schema = SchemaBoxVitals
	env.Payload = validBoxVitalsPayload()
	if err := Validate(env); err != nil {
		t.Fatalf("a well-formed box.vitals envelope must deliver (EVT-013); got %v", err)
	}
}
