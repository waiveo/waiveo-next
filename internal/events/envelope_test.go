package events

import "testing"

// TestIsRegisteredSchema pins the EVT-020 catalog membership: the seven bare
// platform-registered names are members; a pack-namespaced name (EVT-021) and an
// unregistered bare name are not.
func TestIsRegisteredSchema(t *testing.T) {
	if !IsRegisteredSchema("automation.run") {
		t.Fatal("automation.run is a registered platform schema (EVT-020)")
	}
	if IsRegisteredSchema("acme/widget.pinged") {
		t.Fatal("a pack-namespaced name is never a registered platform schema (EVT-021)")
	}
	if IsRegisteredSchema("unknown.schema") {
		t.Fatal("an unregistered bare name is not registered (EVT-013)")
	}
	for _, s := range []string{
		"entity.state_changed", "automation.run", "content.played",
		"device.heartbeat", "box.vitals", "audit.event", "variable.changed",
	} {
		if !IsRegisteredSchema(s) {
			t.Fatalf("%s must be in the registered catalog (EVT-020)", s)
		}
	}
	if len(RegisteredSchemas) != 7 {
		t.Fatalf("the registered catalog has exactly seven schemas (EVT-020); got %d", len(RegisteredSchemas))
	}
}

// TestIsPackSchemaName pins the EVT-021 discriminator: a '/'-bearing name is a
// pack-contributed name; a bare dotted name is not.
func TestIsPackSchemaName(t *testing.T) {
	if !IsPackSchemaName("acme/widget.pinged") {
		t.Fatal("a slash-bearing name is pack-namespaced (EVT-021)")
	}
	if IsPackSchemaName("automation.run") {
		t.Fatal("a bare dotted name is not pack-namespaced (EVT-021)")
	}
}

// TestValidOrigin pins the closed EVT-010 origin enum.
func TestValidOrigin(t *testing.T) {
	for _, o := range []string{"internal", "relay", "ingest"} {
		if !ValidOrigin(o) {
			t.Fatalf("%s is a valid origin (EVT-010)", o)
		}
	}
	for _, o := range []string{"spork", "", "external"} {
		if ValidOrigin(o) {
			t.Fatalf("%q is not a valid origin (EVT-010)", o)
		}
	}
}

// TestRegisteredCatalogAndValidatorsAgree guards against drift between the
// exported EVT-020 catalog and the EVT-013 dispatch registry: every registered
// schema must have a payload validator and vice versa (later tasks fill the
// stubs — this keeps their key sets aligned).
func TestRegisteredCatalogAndValidatorsAgree(t *testing.T) {
	if len(payloadValidators) != len(RegisteredSchemas) {
		t.Fatalf("payloadValidators (%d) and RegisteredSchemas (%d) must have the same size (EVT-013/020)",
			len(payloadValidators), len(RegisteredSchemas))
	}
	for name := range RegisteredSchemas {
		if _, ok := payloadValidators[name]; !ok {
			t.Fatalf("registered schema %q has no payload validator (EVT-013 dispatch)", name)
		}
	}
}
