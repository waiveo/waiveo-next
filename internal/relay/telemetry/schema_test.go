package telemetry

import "testing"

// TestClassOfDurableSchemas confirms REL-093's three durable-class schemas
// (content.played, automation.run, entity.state_changed — per events/1's own
// registered-schema catalog) each classify Durable.
func TestClassOfDurableSchemas(t *testing.T) {
	for _, schema := range []string{
		SchemaContentPlayed,
		SchemaAutomationRun,
		SchemaEntityStateChanged,
	} {
		class, ok := ClassOf(schema)
		if !ok {
			t.Fatalf("ClassOf(%q) ok = false, want true", schema)
		}
		if class != Durable {
			t.Fatalf("ClassOf(%q) class = %v, want Durable", schema, class)
		}
	}
}

// TestClassOfLatestOnlySchemas confirms REL-094's two latest-only schemas
// (device.heartbeat, box.vitals) each classify LatestOnly.
func TestClassOfLatestOnlySchemas(t *testing.T) {
	for _, schema := range []string{
		SchemaDeviceHeartbeat,
		SchemaBoxVitals,
	} {
		class, ok := ClassOf(schema)
		if !ok {
			t.Fatalf("ClassOf(%q) ok = false, want true", schema)
		}
		if class != LatestOnly {
			t.Fatalf("ClassOf(%q) class = %v, want LatestOnly", schema, class)
		}
	}
}

// TestClassOfUnknownSchema confirms a schema outside the five events/1
// registered schemas this channel carries (REL-095) — e.g. a pack-contributed
// schema, or audit.event/variable.changed which events/1 registers but
// relay/1 REL-093/094 does not name — reports ok=false.
func TestClassOfUnknownSchema(t *testing.T) {
	for _, schema := range []string{
		"audit.event",
		"variable.changed",
		"someplugin/widget.pressed",
		"",
	} {
		if class, ok := ClassOf(schema); ok {
			t.Fatalf("ClassOf(%q) = (%v, true), want ok=false", schema, class)
		}
	}
}

// TestClassIsZeroValueDurable documents Class's zero value: Durable is iota 0
// so a zero-value Class (e.g. a not-yet-set struct field) reads as the safer
// "never silently coalesced" default rather than defaulting to LatestOnly.
func TestClassIsZeroValueDurable(t *testing.T) {
	var c Class
	if c != Durable {
		t.Fatalf("zero-value Class = %v, want Durable", c)
	}
}
