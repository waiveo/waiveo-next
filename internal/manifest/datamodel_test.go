package manifest

import (
	"path/filepath"
	"testing"
)

// man051File is the frozen oracle for a valid data-model declaration: one
// collection with title/summary field annotations and a bounded per-collection
// retention descriptor.
var man051File = filepath.Join("..", "..", "conformance", "corpora", "manifest-1", "MAN-051-valid-data-model.json")

// TestValidateDataModelValid drives the MAN-051 corpus case: a manifest
// declaring one collection with a title/summary-annotated field pair and a
// bounded retention descriptor validates with zero errors (MAN-050/051/052/054).
func TestValidateDataModelValid(t *testing.T) {
	m := loadManifest(t, man051File)
	if errs := Validate(m, testHost()); len(errs) != 0 {
		t.Fatalf("expected zero errors on MAN-051-valid-data-model, got %d: %+v", len(errs), errs)
	}
}

// TestValidateTwoTitleFieldsInvalid: a collection with two fields both
// declaring role:title violates MAN-052 (at most one title field per
// collection).
func TestValidateTwoTitleFieldsInvalid(t *testing.T) {
	m := loadManifest(t, man051File)
	m.DataModel.Collections[0].Fields[1].Role = "title"
	errs := Validate(m, testHost())
	if len(errs) == 0 {
		t.Fatalf("expected an error for two role:title fields in one collection, got none")
	}
}

// TestValidateEnvelopeFieldCollisionInvalid: a declared field named "revision"
// collides with the host-managed universal entity envelope, which every row
// carries in addition to declared fields (MAN-051).
func TestValidateEnvelopeFieldCollisionInvalid(t *testing.T) {
	m := loadManifest(t, man051File)
	m.DataModel.Collections[0].Fields = append(m.DataModel.Collections[0].Fields, Field{
		Name: "revision", Type: "integer",
	})
	errs := Validate(m, testHost())
	if len(errs) == 0 {
		t.Fatalf("expected an error for a field named %q colliding with the universal entity envelope, got none", "revision")
	}
}

// TestValidateRetentionUndeclaredCollectionInvalid: a retention key naming a
// collection the manifest never declared violates MAN-054.
func TestValidateRetentionUndeclaredCollectionInvalid(t *testing.T) {
	m := loadManifest(t, man051File)
	m.Retention["ghost-collection"] = []byte(`"unbounded"`)
	errs := Validate(m, testHost())
	if len(errs) == 0 {
		t.Fatalf("expected an error for a retention key naming an undeclared collection, got none")
	}
}

// TestValidateUnknownConnectionsProviderInvalid: a connections entry whose
// provider/authType pair the host does not recognize violates MAN-055.
func TestValidateUnknownConnectionsProviderInvalid(t *testing.T) {
	m := loadManifest(t, man051File)
	m.Connections = append(m.Connections, Connection{
		Provider: "bogus-service", AuthType: "api-key", Scopes: []string{"read"},
	})
	errs := Validate(m, testHost())
	if len(errs) == 0 {
		t.Fatalf("expected an error for an unregistered connections provider/authType, got none")
	}
}

// TestValidateDataModelVersionRegressionInvalid: a dataModel.version lower
// than the currently installed version fails validation (MAN-053).
func TestValidateDataModelVersionRegressionInvalid(t *testing.T) {
	m := loadManifest(t, man051File)
	m.DataModel.Version = 1
	host := testHost()
	host.InstalledDataModelVersion = 2
	errs := Validate(m, host)
	if !hasCode(errs, "DATAMODEL_VERSION_REGRESSION") {
		t.Fatalf("expected a DATAMODEL_VERSION_REGRESSION error, got %+v", errs)
	}
	if !hasField(errs, "dataModel.version") {
		t.Fatalf("expected the error to name dataModel.version, got %+v", errs)
	}
}
