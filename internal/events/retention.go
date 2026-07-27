package events

// retention.go is the platform's retention-class CONFIGURATION: how long an
// event of each retention_class (EVT-010) is actually kept, and how many rows of
// it the box will hold. events/1 fixes the field and its meaning ("a short
// classification string governing how long the event is retained") but
// explicitly leaves the vocabulary and the durations outside itself — EVT-082
// says so in as many words: "even though this contract does not itself enumerate
// retention-class values or their durations". class.go pins WHICH class each
// registered schema carries; this file pins what each class BUYS.
//
// EVT-082 is the one requirement that constrains the numbers rather than just
// naming the field: an audit.event's retention_class MUST be "a value the
// platform's retention configuration treats as long-lived relative to the other
// registered schemas — an audit trail outliving the operational telemetry
// recorded alongside it is the property this field exists to express". So the
// audit window here is not merely longer by taste: it is the requirement, and
// DefaultRetentionPolicy is checked against it (retention_test.go asserts the
// audit window strictly exceeds every other registered schema's).
//
// That requirement has teeth beyond storage accounting. security-model/1
// SEC-150 makes audit.event the platform's SOLE audit mechanism — there is
// deliberately no second audit schema — so every login, every session and
// API-key issuance and revocation, and every mutating api/1 request files its
// only permanent record here. A retention policy that let those expire on the
// telemetry tier would be the platform quietly discarding its own audit trail.
//
// Two knobs per class, because they answer different questions:
//
//   - Window is the contract-facing one: the age past which an event is no
//     longer retained, and therefore past which a resume from it is a
//     retention_expired gap (EVT-141) rather than a clean resume.
//   - MaxRows is the box-facing one: a disk backstop, enforced per class so a
//     flood of telemetry can NEVER evict an audit record to make room for
//     itself. A single global row cap would do exactly that — evicting
//     oldest-first across the whole log means the oldest audit record is the
//     first thing a busy telemetry hour deletes, which is precisely the
//     inversion EVT-082 exists to forbid.
//
// Eviction under either knob is marked, never silent: the log advances its
// evicted-through watermark, so a subscriber resuming from an evicted point
// gets a gap (EVT-143).

import (
	"sort"
	"time"
)

// The retention windows and per-class row caps this platform runs. They are
// named constants rather than inline literals because they are exactly the
// "platform's retention configuration" EVT-082 refers to — the thing a
// deployment may tune, and the thing the EVT-082 relationship is asserted over.
const (
	// telemetryStandardWindow is how long operational telemetry is kept: long
	// enough that a screen or relay offline over a long weekend still resumes
	// cleanly rather than through a gap, short enough that a busy box is not
	// storing months of heartbeats.
	telemetryStandardWindow = 7 * 24 * time.Hour
	// telemetryStandardMaxRows is the disk backstop for the telemetry tier. It
	// is the same horizon the in-memory log was bounded to before this log
	// became persistent, kept so the operational tier's footprint did not
	// silently change size when it gained a window.
	telemetryStandardMaxRows = 4096

	// auditLongWindow is the audit tier: long-lived relative to the operational
	// telemetry recorded alongside it (EVT-082). Just over a year, so a full
	// annual review period is still readable.
	auditLongWindow = 400 * 24 * time.Hour
	// auditLongMaxRows is 0 — UNCAPPED. An audit record is never evicted to
	// make room for another audit record: SEC-150 makes this the only place a
	// security-relevant flow is recorded, so dropping the oldest to admit the
	// newest would silently rewrite the beginning of the trail. The window is
	// the only thing that retires an audit record.
	auditLongMaxRows = 0
)

// ClassRetention is what one retention_class buys. A zero Window means "never
// expires by age"; a zero MaxRows means "no row cap".
type ClassRetention struct {
	// Window is the age past which an event of this class is no longer
	// retained. Measured against the envelope's own ts (EVT-010: "when the
	// event was recorded"), which is the platform's own recording clock — the
	// same clock the expiry check is evaluated against, injected rather than
	// read from the wall.
	Window time.Duration
	// MaxRows caps how many events of this class are retained at once,
	// evicting oldest-first. It is a storage backstop, not a contract window.
	MaxRows int
}

// RetentionPolicy maps each retention_class the platform assigns (class.go) to
// its ClassRetention, plus the answer for a class it does not recognize.
type RetentionPolicy struct {
	classes map[string]ClassRetention
	unknown ClassRetention
}

// DefaultRetentionPolicy is the shipping retention configuration: the telemetry
// tier every relay-carried registered schema uses, and the long-lived audit tier
// EVT-082 requires audit.event to sit on.
//
// The class keys come from the schema files' own class constants (the same
// single source class.go indexes), so the policy and the classification cannot
// drift into naming different strings.
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		classes: map[string]ClassRetention{
			// automation.run, content.played, entity.state_changed,
			// device.heartbeat and box.vitals all carry this one class.
			automationRunRetentionClass: {Window: telemetryStandardWindow, MaxRows: telemetryStandardMaxRows},
			auditEventRetentionClass:    {Window: auditLongWindow, MaxRows: auditLongMaxRows},
		},
		// An unrecognized class — a pack-declared schema whose class this build
		// does not pin (EVT-021/022), or a record written by a build that knew a
		// class this one does not — is kept for the LONGEST window the policy
		// configures, and capped at the telemetry tier's row count.
		//
		// The asymmetry is deliberate and is the conservative reading in both
		// directions. Expiring an unknown class on the SHORT window would delete
		// records whose retention this build cannot reason about, and deletion is
		// unrecoverable; leaving it uncapped would let one unrecognized producer
		// fill the box's disk, which is recoverable but not acceptable. So: the
		// longest window, behind a row cap.
		unknown: ClassRetention{Window: auditLongWindow, MaxRows: telemetryStandardMaxRows},
	}
}

// For returns the retention configured for class, falling back to the
// unrecognized-class answer (see DefaultRetentionPolicy) for a class the policy
// does not name.
func (p RetentionPolicy) For(class string) ClassRetention {
	if c, ok := p.classes[class]; ok {
		return c
	}
	return p.unknown
}

// Known reports whether class is one the policy explicitly configures, as
// opposed to falling through to the unrecognized-class answer.
func (p RetentionPolicy) Known(class string) bool {
	_, ok := p.classes[class]
	return ok
}

// Classes returns every explicitly configured retention class, sorted — the set
// a persisted log iterates when it enforces windows and row caps, so the sweep
// is over the policy's own vocabulary rather than over whatever class strings
// happen to be sitting in storage.
func (p RetentionPolicy) Classes() []string {
	out := make([]string, 0, len(p.classes))
	for c := range p.classes {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// AuditRetentionClass is the class EVT-082 constrains: the one audit.event
// carries, which the platform's retention configuration must treat as
// long-lived relative to every other registered schema's. Exported so a
// deployment (and the test that asserts the EVT-082 relationship) names the
// same string the audit envelope constructor stamps, never a second copy.
const AuditRetentionClass = auditEventRetentionClass
