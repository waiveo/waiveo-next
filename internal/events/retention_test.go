package events

import "testing"

// TestRetentionPolicy_AuditOutlivesEveryOtherRegisteredSchema is EVT-082 stated
// as an assertion: an audit.event's retention_class MUST be "a value the
// platform's retention configuration treats as long-lived relative to the other
// registered schemas".
//
// The comparison walks classBySchema — the SAME index that decides what class
// each schema's envelope actually carries — rather than a list written out here,
// so a schema added to the catalog later is compared automatically, and a schema
// silently moved onto the audit tier would show up as a schema whose window is
// no longer strictly below it.
func TestRetentionPolicy_AuditOutlivesEveryOtherRegisteredSchema(t *testing.T) {
	p := DefaultRetentionPolicy()
	audit := p.For(AuditRetentionClass)
	if audit.Window <= 0 {
		t.Fatalf("the audit retention class %q must configure a window; got %v", AuditRetentionClass, audit.Window)
	}
	if !p.Known(AuditRetentionClass) {
		t.Fatalf("the audit retention class %q must be explicitly configured, not fall through to the unknown-class answer", AuditRetentionClass)
	}

	compared := 0
	for schema, c := range classBySchema {
		if schema == SchemaAuditEvent {
			if c.retention != AuditRetentionClass {
				t.Fatalf("audit.event must carry the audit retention class %q (EVT-082); got %q", AuditRetentionClass, c.retention)
			}
			continue
		}
		compared++
		other := p.For(c.retention)
		if other.Window <= 0 {
			t.Fatalf("%s: retention class %q must configure a window", schema, c.retention)
		}
		if !(other.Window < audit.Window) {
			t.Fatalf("EVT-082: audit.event's retention window (%v, class %q) must be strictly longer than %s's (%v, class %q)",
				audit.Window, AuditRetentionClass, schema, other.Window, c.retention)
		}
	}
	if compared == 0 {
		t.Fatal("EVT-082 is a RELATIVE requirement; with no other classed registered schema to compare against, this test proves nothing")
	}
}

// TestRetentionPolicy_AuditRowsAreNeverEvictedToAdmitTelemetry: the per-class
// row cap is what stops a telemetry flood from deleting audit history. A single
// global cap would evict oldest-first across the whole log, which is exactly the
// inversion EVT-082 forbids — so the audit tier carries no row cap at all, and
// the telemetry tier's cap is finite.
func TestRetentionPolicy_AuditRowsAreNeverEvictedToAdmitTelemetry(t *testing.T) {
	p := DefaultRetentionPolicy()
	if got := p.For(AuditRetentionClass).MaxRows; got != 0 {
		t.Fatalf("the audit tier must be row-uncapped so no audit record is ever evicted to admit another (SEC-150); got MaxRows=%d", got)
	}
	telemetry := p.For(classBySchema[SchemaBoxVitals].retention)
	if telemetry.MaxRows <= 0 {
		t.Fatalf("the telemetry tier needs a finite row cap as the box's disk backstop; got MaxRows=%d", telemetry.MaxRows)
	}
}

// TestRetentionPolicy_UnknownClassIsKeptLongAndCapped: a class this build does
// not recognize (a pack-declared schema, EVT-021/022, or a record written by a
// build that knew a class this one does not) is retained for the longest
// configured window — deleting records whose retention cannot be reasoned about
// is unrecoverable — but is still row-capped, so one unrecognized producer
// cannot fill the disk.
func TestRetentionPolicy_UnknownClassIsKeptLongAndCapped(t *testing.T) {
	p := DefaultRetentionPolicy()
	unknown := p.For("acme/thing.retention-tier")
	if p.Known("acme/thing.retention-tier") {
		t.Fatal("a pack-namespaced class must not be an explicitly configured class")
	}
	longest := p.For(AuditRetentionClass).Window
	for _, c := range p.Classes() {
		if w := p.For(c).Window; w > longest {
			longest = w
		}
	}
	if unknown.Window != longest {
		t.Fatalf("an unrecognized class must be kept for the longest configured window (%v); got %v", longest, unknown.Window)
	}
	if unknown.MaxRows <= 0 {
		t.Fatalf("an unrecognized class must still be row-capped; got MaxRows=%d", unknown.MaxRows)
	}
}

// TestRetentionPolicy_ClassesCoversEveryClassedRegisteredSchema: the sweep a
// persisted log runs is over RetentionPolicy.Classes(), so any class a
// registered schema actually stamps but the policy does not configure would be
// swept only by the unknown-class fallback — retained far longer than intended
// and never reported as a policy gap.
func TestRetentionPolicy_ClassesCoversEveryClassedRegisteredSchema(t *testing.T) {
	p := DefaultRetentionPolicy()
	configured := make(map[string]bool)
	for _, c := range p.Classes() {
		configured[c] = true
	}
	for schema, c := range classBySchema {
		if !configured[c.retention] {
			t.Fatalf("%s stamps retention_class %q, which the policy does not configure", schema, c.retention)
		}
	}
}
