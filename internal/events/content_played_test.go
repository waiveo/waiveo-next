package events

import (
	"encoding/json"
	"errors"
	"testing"
)

// validContentPlayedPayload is the EVT-050 corpus payload: a scheduled
// playback completes with a known t_end, carrying the compiled program
// revision in force at t_start.
func validContentPlayedPayload() json.RawMessage {
	return json.RawMessage(`{
		"asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
		"program_revision": "rev-00042",
		"t_start": 1752537000000,
		"t_end": 1752537030000,
		"cause": "scheduled",
		"completion": "completed"
	}`)
}

// TestContentPlayed_CorpusPayloadValidates: the EVT-050 corpus payload
// validates against its own field definition.
func TestContentPlayed_CorpusPayloadValidates(t *testing.T) {
	if err := validateContentPlayed(validContentPlayedPayload()); err != nil {
		t.Fatalf("the EVT-050 corpus content.played payload must validate; got %v", err)
	}
}

// TestContentPlayed_CauseEnumBites: cause is the closed EVT-050 enum
// {scheduled,preempted,fallback,manual,loop} — a value outside it is rejected.
func TestContentPlayed_CauseEnumBites(t *testing.T) {
	p := json.RawMessage(`{
		"asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
		"program_revision": "rev-00042",
		"t_start": 1752537000000,
		"t_end": 1752537030000,
		"cause": "teleported",
		"completion": "completed"
	}`)
	err := validateContentPlayed(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "cause" {
		t.Fatalf("cause outside the EVT-050 enum must be rejected on cause; got %v", err)
	}
}

// TestContentPlayed_CompletionEnumBites: completion is the closed EVT-050 enum
// {completed,interrupted,skipped} — a value outside it is rejected.
func TestContentPlayed_CompletionEnumBites(t *testing.T) {
	p := json.RawMessage(`{
		"asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
		"program_revision": "rev-00042",
		"t_start": 1752537000000,
		"t_end": 1752537030000,
		"cause": "scheduled",
		"completion": "vaporized"
	}`)
	err := validateContentPlayed(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "completion" {
		t.Fatalf("completion outside the EVT-050 enum must be rejected on completion; got %v", err)
	}
}

// TestContentPlayed_BadScreenIDULIDRejected: screen_id is a ULID field
// (EVT-050).
func TestContentPlayed_BadScreenIDULIDRejected(t *testing.T) {
	p := json.RawMessage(`{
		"asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"screen_id": "not-a-ulid",
		"program_revision": "rev-00042",
		"t_start": 1752537000000,
		"t_end": 1752537030000,
		"cause": "scheduled",
		"completion": "completed"
	}`)
	err := validateContentPlayed(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "screen_id" {
		t.Fatalf("a non-ULID screen_id must be rejected on screen_id (EVT-050); got %v", err)
	}
}

// TestContentPlayed_NullTStartRejected: a JSON null for the required int64
// t_start must be an EVT-013 rejection — encoding/json unmarshals null as a
// no-op, so a null must never silently pass as a zero-value 0 (EVT-050
// requires an integer here).
func TestContentPlayed_NullTStartRejected(t *testing.T) {
	p := json.RawMessage(`{
		"asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
		"program_revision": "rev-00042",
		"t_start": null,
		"t_end": 1752537030000,
		"cause": "scheduled",
		"completion": "completed"
	}`)
	err := validateContentPlayed(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "t_start" {
		t.Fatalf("a null t_start must be an EVT-013 rejection on t_start, not a silent 0; got %v", err)
	}
}

// TestContentPlayed_PowerEvidenceOptionalObjectWhenPresent: power_evidence is
// optional (EVT-050) — absent is fine (the corpus payload already omits it,
// TestContentPlayed_CorpusPayloadValidates); when present it must be an
// object.
func TestContentPlayed_PowerEvidenceOptionalObjectWhenPresent(t *testing.T) {
	valid := json.RawMessage(`{
		"asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
		"program_revision": "rev-00042",
		"t_start": 1752537000000,
		"t_end": 1752537030000,
		"cause": "scheduled",
		"completion": "completed",
		"power_evidence": {"power_state": "on", "source": "device.heartbeat"}
	}`)
	if err := validateContentPlayed(valid); err != nil {
		t.Fatalf("a power_evidence object must validate (EVT-050); got %v", err)
	}

	invalid := json.RawMessage(`{
		"asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
		"program_revision": "rev-00042",
		"t_start": 1752537000000,
		"t_end": 1752537030000,
		"cause": "scheduled",
		"completion": "completed",
		"power_evidence": "on"
	}`)
	err := validateContentPlayed(invalid)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "power_evidence" {
		t.Fatalf("a non-object power_evidence must be rejected on power_evidence; got %v", err)
	}
}

// TestContentPlayed_DeliversThroughValidate: a well-formed content.played
// envelope passes the full EVT-013 gate (delivered).
func TestContentPlayed_DeliversThroughValidate(t *testing.T) {
	env := validEnvelope()
	env.Schema = SchemaContentPlayed
	env.Payload = validContentPlayedPayload()
	if err := Validate(env); err != nil {
		t.Fatalf("a well-formed content.played envelope must deliver (EVT-013); got %v", err)
	}
}
