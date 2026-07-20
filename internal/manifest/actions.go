package manifest

import "strconv"

// validAuditClasses is the MAN-100 auditClass enum.
var validAuditClasses = map[string]bool{"read": true, "write": true, "privileged": true}

// validIdempotencyClasses is the MAN-103 idempotencyClass enum.
var validIdempotencyClasses = map[string]bool{"safe-to-retry": true, "not-idempotent": true}

// validateActions enforces MAN-100/102/103: every declared action has a
// pack-unique name, a capabilityScope naming one of THIS manifest's own
// declared capabilities (never merely a host-registered one), a recognized
// auditClass, and a recognized idempotencyClass. paramsSchema is an opaque
// JSON Schema object this contract does not deeply inspect, so a parameter
// typed asset-ref (MAN-102) is never itself a validation failure here — the
// host performs the upload-to-content-store step at invocation time.
func validateActions(m PackManifest) []Error {
	var errs []Error

	declaredCaps := map[string]bool{}
	for _, c := range m.Capabilities {
		declaredCaps[c.Capability] = true
	}

	seenNames := map[string]bool{}
	for i, act := range m.Actions {
		at := "actions[" + strconv.Itoa(i) + "]"

		if act.Name == "" {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".name",
				"actions[].name is required (MAN-100)"})
		} else if seenNames[act.Name] {
			errs = append(errs, Error{"ACTION_NAME_DUPLICATE", at + ".name",
				`action name "` + act.Name + `" is declared more than once (MAN-100)`})
		}
		seenNames[act.Name] = true

		if !declaredCaps[act.CapabilityScope] {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".capabilityScope",
				`action capabilityScope "` + act.CapabilityScope + `" does not name a capability this manifest declares (MAN-100)`})
		}

		if !validAuditClasses[act.AuditClass] {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".auditClass",
				`action auditClass "` + act.AuditClass + `" MUST be one of read, write, privileged (MAN-100)`})
		}

		if !validIdempotencyClasses[act.IdempotencyClass] {
			errs = append(errs, Error{"ACTION_IDEMPOTENCY_CLASS_INVALID", at + ".idempotencyClass",
				`action idempotencyClass "` + act.IdempotencyClass + `" MUST be one of safe-to-retry, not-idempotent (MAN-103)`})
		}
	}

	return errs
}
