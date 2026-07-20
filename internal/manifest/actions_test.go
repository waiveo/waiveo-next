package manifest

import "testing"

// TestValidateActionsDuplicateName: two actions[] entries sharing a name
// violate MAN-100's pack-unique-name requirement with a typed
// ACTION_NAME_DUPLICATE error.
func TestValidateActionsDuplicateName(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Actions = append(m.Actions, m.Actions[0])
	errs := Validate(m, testHost())
	if !hasCode(errs, "ACTION_NAME_DUPLICATE") {
		t.Fatalf("expected an ACTION_NAME_DUPLICATE error, got %+v", errs)
	}
	if !hasField(errs, "actions[1].name") {
		t.Fatalf("expected the error to name actions[1].name, got %+v", errs)
	}
}

// TestValidateActionsCapabilityScopeUnknown: an actions[].capabilityScope
// naming no capability this manifest itself declares violates MAN-100 — the
// scope MUST resolve against THIS manifest's own capabilities, never the host
// capability registry directly. "notifications.send" is host-recognized
// (testHost) but not among this fixture's own declared capabilities, so a
// pass that checked the host registry instead would wrongly accept it.
func TestValidateActionsCapabilityScopeUnknown(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Actions[0].CapabilityScope = "notifications.send"
	errs := Validate(m, testHost())
	if !hasField(errs, "actions[0].capabilityScope") {
		t.Fatalf("expected a capabilityScope error for %q, got %+v", "notifications.send", errs)
	}
}

// TestValidateActionsAuditClassInvalid: an auditClass outside
// {read, write, privileged} violates MAN-100.
func TestValidateActionsAuditClassInvalid(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Actions[0].AuditClass = "super"
	errs := Validate(m, testHost())
	if !hasField(errs, "actions[0].auditClass") {
		t.Fatalf("expected an auditClass error for %q, got %+v", "super", errs)
	}
}

// TestValidateActionsIdempotencyClassMissing: an actions[] entry omitting
// idempotencyClass violates MAN-103 with a typed
// ACTION_IDEMPOTENCY_CLASS_INVALID error.
func TestValidateActionsIdempotencyClassMissing(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Actions[0].IdempotencyClass = ""
	errs := Validate(m, testHost())
	if !hasCode(errs, "ACTION_IDEMPOTENCY_CLASS_INVALID") {
		t.Fatalf("expected an ACTION_IDEMPOTENCY_CLASS_INVALID error, got %+v", errs)
	}
}

// TestValidateActionsIdempotencyClassInvalid: an idempotencyClass outside
// {safe-to-retry, not-idempotent} violates MAN-103.
func TestValidateActionsIdempotencyClassInvalid(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Actions[0].IdempotencyClass = "sometimes"
	errs := Validate(m, testHost())
	if !hasCode(errs, "ACTION_IDEMPOTENCY_CLASS_INVALID") {
		t.Fatalf("expected an ACTION_IDEMPOTENCY_CLASS_INVALID error, got %+v", errs)
	}
}

// TestValidateActionsAssetRefParamAllowed: a paramsSchema declaring a param
// typed asset-ref MUST NOT itself be a validation failure (MAN-102) — the
// host performs the upload-to-content-store step, and this contract does not
// deeply inspect paramsSchema.
func TestValidateActionsAssetRefParamAllowed(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Actions[0].ParamsSchema = []byte(`{"type":"object","properties":{"file":{"type":"asset-ref"}}}`)
	errs := Validate(m, testHost())
	if hasField(errs, "actions[0].paramsSchema") {
		t.Fatalf("expected an asset-ref param to validate clean, got %+v", errs)
	}
}
