package events

import "testing"

// TestClassFor_CoversEveryRelayTelemetrySchema pins the EVT-010 cost/retention
// class ClassFor assigns each of the five registered schemas the relay's
// telemetry channel carries (relay/1 REL-095: automation.run, content.played,
// entity.state_changed, device.heartbeat, box.vitals). An ingester
// (internal/app/eventingest) reads ClassFor BEFORE the payload validator, so a
// schema absent from classBySchema is dropped as EVT-013 for lacking a class and
// never reaches its (existing, passing) payload validator — a Durable-class
// content.played/entity.state_changed record would be silently lost while the
// ack falsely reports it delivered (REL-093). This test fails if any of the five
// loses its class.
func TestClassFor_CoversEveryRelayTelemetrySchema(t *testing.T) {
	// The five schemas relay/1 carries over its telemetry channel (REL-095), all
	// operational-telemetry-tier (matching the events/1 wire-shape examples and
	// the events1 conformance driver's fixture class).
	want := map[string]struct{ cost, retention string }{
		SchemaAutomationRun:      {"telemetry", "telemetry-standard"},
		SchemaContentPlayed:      {"telemetry", "telemetry-standard"},
		SchemaEntityStateChanged: {"telemetry", "telemetry-standard"},
		SchemaDeviceHeartbeat:    {"telemetry", "telemetry-standard"},
		SchemaBoxVitals:          {"telemetry", "telemetry-standard"},
	}
	for schema, w := range want {
		cost, retention, ok := ClassFor(schema)
		if !ok {
			t.Fatalf("ClassFor(%q) must return a class for a relay telemetry schema (REL-095); got ok=false — an ingester would drop it as EVT-013", schema)
		}
		if cost != w.cost || retention != w.retention {
			t.Fatalf("ClassFor(%q) = cost %q retention %q; want cost %q retention %q", schema, cost, retention, w.cost, w.retention)
		}
	}
}

// TestClassFor_UnclassedSchemaReportsNotOK locks the negative branch: an
// unregistered bare schema and a pack-namespaced schema (EVT-021/022) carry no
// class, so ClassFor reports ok=false and an ingester drops+logs the record
// rather than minting an envelope with an empty, un-validatable class (EVT-013).
func TestClassFor_UnclassedSchemaReportsNotOK(t *testing.T) {
	for _, schema := range []string{"not.a.registered.schema", "acme/thing.happened"} {
		if _, _, ok := ClassFor(schema); ok {
			t.Fatalf("ClassFor(%q) must report ok=false for an unclassed schema", schema)
		}
	}
}
