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
	// The schemas relay/1 carries over its telemetry channel (REL-095), all
	// operational-telemetry-tier (matching the events/1 wire-shape examples and
	// the events1 conformance driver's fixture class).
	want := map[string]struct{ cost, retention string }{
		SchemaAutomationRun:      {"telemetry", "telemetry-standard"},
		SchemaContentPlayed:      {"telemetry", "telemetry-standard"},
		SchemaEntityStateChanged: {"telemetry", "telemetry-standard"},
		SchemaDeviceHeartbeat:    {"telemetry", "telemetry-standard"},
		SchemaBoxVitals:          {"telemetry", "telemetry-standard"},
		// A viewer's press on an interactive slide layer (EVT-055). It travels
		// this same channel because the relay is where the press is OBSERVED, so
		// an unclassed one would be buffered, pushed, and then dropped on arrival
		// — the automation never runs and nothing anywhere says why.
		SchemaScreenInteraction: {"telemetry", "telemetry-standard"},
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
// unregistered BARE schema carries no class, so ClassFor reports ok=false and an
// ingester drops+logs the record rather than minting an envelope with an empty,
// un-validatable class (EVT-013).
//
// A pack-namespaced schema used to be in this list and is deliberately no
// longer. It was unclassed because nothing could emit one; now that packs can,
// leaving it unclassed meant `contributes.automation.events` was a declaration
// whose events were dropped at their producer. See the positive case below.
func TestClassFor_UnclassedSchemaReportsNotOK(t *testing.T) {
	for _, schema := range []string{"not.a.registered.schema", "nonsense"} {
		if _, _, ok := ClassFor(schema); ok {
			t.Fatalf("ClassFor(%q) must report ok=false for an unclassed bare schema", schema)
		}
	}
}

// A pack-contributed schema is classed by its SHAPE (EVT-021): the `/` its pack
// id contributes is what a registered schema can never have, so the two
// namespaces are distinguishable by construction rather than by a table nobody
// could keep current — the set of installed packs is deployment state.
func TestClassFor_PackContributedSchemasAreClassed(t *testing.T) {
	for _, schema := range []string{"acme/thing.happened", "waiveo/backups.completed"} {
		cost, retention, ok := ClassFor(schema)
		if !ok {
			t.Fatalf("ClassFor(%q) reports ok=false; a pack event would be dropped at its producer", schema)
		}
		// The telemetry tier, not the audit tier. A pack's event volume is bounded
		// by nothing the platform controls, and the long-lived tier would let one
		// badly written extension fill a disk with records we promised to keep.
		if cost != "telemetry" || retention != "telemetry-standard" {
			t.Fatalf("ClassFor(%q) = %q/%q, want the telemetry tier", schema, cost, retention)
		}
	}
}

// ClassFor and Validate must agree about what "pack-namespaced" means. They
// share IsPackSchemaName rather than each carrying the rule, and this pins that:
// a schema Validate routes to ErrPackSchema must be one ClassFor can class, or a
// pack event would be classed and then rejected, or rejected and then classed.
func TestClassingAndValidationAgreeOnWhatIsPackNamespaced(t *testing.T) {
	// The implication runs ONE way: everything Validate routes to ErrPackSchema
	// must be classable. It is not an equivalence — a registered schema is
	// classed too, and is not pack-namespaced. Asserting the equivalence was the
	// first version of this test and it was simply wrong.
	for _, schema := range []string{"acme/thing.happened", "waiveo/backups.done", "x/y.z"} {
		if !IsPackSchemaName(schema) {
			t.Fatalf("%q should be pack-namespaced", schema)
		}
		if _, _, classed := ClassFor(schema); !classed {
			t.Fatalf("Validate routes %q to ErrPackSchema but ClassFor cannot class it — a pack event would be rejected before it could be stored", schema)
		}
	}
}
