package manifest

import "testing"

// TestValidatePlayableFixedRequiresDurationSeconds: durationSemantics: fixed
// with no durationSeconds violates MAN-081.
func TestValidatePlayableFixedRequiresDurationSeconds(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Contributes.Playable.DurationSeconds = nil
	errs := Validate(m, testHost())
	if !hasField(errs, "contributes.playable.durationSeconds") {
		t.Fatalf("expected a durationSeconds error for fixed with none declared, got %+v", errs)
	}
}

// TestValidatePlayableFixedRejectsNonPositiveDuration: durationSeconds MUST be
// positive when durationSemantics is fixed (MAN-081).
func TestValidatePlayableFixedRejectsNonPositiveDuration(t *testing.T) {
	m := loadManifest(t, man020File)
	zero := 0.0
	m.Contributes.Playable.DurationSeconds = &zero
	errs := Validate(m, testHost())
	if !hasField(errs, "contributes.playable.durationSeconds") {
		t.Fatalf("expected a durationSeconds error for a non-positive value, got %+v", errs)
	}
}

// TestValidatePlayableSourceDrivenForbidsDuration: durationSemantics:
// source-driven MUST NOT declare durationSeconds (MAN-081).
func TestValidatePlayableSourceDrivenForbidsDuration(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Contributes.Playable.DurationSemantics = "source-driven"
	// durationSeconds is still set to 15 from the fixture's fixed case.
	errs := Validate(m, testHost())
	if !hasField(errs, "contributes.playable.durationSeconds") {
		t.Fatalf("expected a durationSeconds error for source-driven with one declared, got %+v", errs)
	}
}

// TestValidatePlayableOperatorSetForbidsDuration: durationSemantics:
// operator-set MUST NOT declare durationSeconds (MAN-081).
func TestValidatePlayableOperatorSetForbidsDuration(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Contributes.Playable.DurationSemantics = "operator-set"
	errs := Validate(m, testHost())
	if !hasField(errs, "contributes.playable.durationSeconds") {
		t.Fatalf("expected a durationSeconds error for operator-set with one declared, got %+v", errs)
	}
}

// TestValidatePlayableSourceDrivenClean: source-driven with no durationSeconds
// validates clean (MAN-081).
func TestValidatePlayableSourceDrivenClean(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Contributes.Playable.DurationSemantics = "source-driven"
	m.Contributes.Playable.DurationSeconds = nil
	errs := Validate(m, testHost())
	if hasField(errs, "contributes.playable.durationSeconds") || hasField(errs, "contributes.playable.durationSemantics") {
		t.Fatalf("expected source-driven with no durationSeconds to validate clean, got %+v", errs)
	}
}

// TestValidatePlayableUnknownContentType: a contentType outside the enum fails
// MAN-080.
func TestValidatePlayableUnknownContentType(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Contributes.Playable.ContentType = "audio"
	errs := Validate(m, testHost())
	if !hasField(errs, "contributes.playable.contentType") {
		t.Fatalf("expected a contentType error for %q, got %+v", m.Contributes.Playable.ContentType, errs)
	}
}

// TestValidatePlayableUnknownDurationSemantics: a durationSemantics outside the
// enum fails MAN-080.
func TestValidatePlayableUnknownDurationSemantics(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Contributes.Playable.DurationSemantics = "eternal"
	errs := Validate(m, testHost())
	if !hasField(errs, "contributes.playable.durationSemantics") {
		t.Fatalf("expected a durationSemantics error for %q, got %+v", m.Contributes.Playable.DurationSemantics, errs)
	}
}
