package api

// The two properties of the declared-schema gate that cannot be driven from
// outside the package: that it fails CLOSED when it cannot resolve the schema a
// family names, and that no family names a schema the embedded document does not
// define (so the fail-closed path is unreachable in practice rather than merely
// handled).

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSchemaGateFailsClosedOnAnUnresolvableSchema: a family naming a component
// the document does not define must REFUSE the request, never fall through
// unchecked. An unchecked write is the exact condition this gate exists to
// remove, so it may not be reachable by getting the wiring wrong (SEC-005).
func TestSchemaGateFailsClosedOnAnUnresolvableSchema(t *testing.T) {
	rs := &resource{cfg: resourceConfig{createSchema: "NoSuchComponentSchema"}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/screens", nil)
	w := httptest.NewRecorder()

	if !rs.schemaRejected(w, req, rs.cfg.createSchema, []byte(`{"name":"x"}`)) {
		t.Fatal("a family naming an unresolvable schema was allowed through unchecked")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — an unresolvable declared schema is a wiring fault, not a client error", w.Code)
	}
}

// TestNoSchemaGateIsAConfiguredNoOp: every schema name a family declares
// resolves in the embedded document. Without this, a typo'd component name would
// turn every write on that family into a 500 the moment it shipped.
func TestNoSchemaGateIsAConfiguredNoOp(t *testing.T) {
	for _, cfg := range []resourceConfig{
		scopeNodesConfig(), screensConfig(), adoptedDevicesConfig(), automationsConfig(),
		webhookEndpointsConfig(), schedulesConfig(), daypartsConfig(), playlistsConfig(),
	} {
		for _, name := range []string{cfg.createSchema, cfg.updateSchema} {
			if name == "" {
				continue
			}
			if _, err := declaredSchema(name); err != nil {
				t.Errorf("family %q names schema %q: %v", cfg.path, name, err)
			}
		}
	}
}
