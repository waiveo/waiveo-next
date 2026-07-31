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

// TestValidateReservedRowFieldCollisionInvalid: a declared field named
// external_id, created_at, or updated_at collides with the api/1 resource-row
// baseline every row carries as host-managed columns beside the MAN-051
// envelope. The pack-data write gate intercepts a body key by any of these names
// as a host/envelope field before consulting the declared fields, so such a
// field's client value would be silently dropped (external_id repurposed as the
// uniqueness key) — install MUST refuse it, exactly as it refuses an envelope
// collision (MAN-051).
func TestValidateReservedRowFieldCollisionInvalid(t *testing.T) {
	for _, name := range []string{"external_id", "created_at", "updated_at"} {
		m := loadManifest(t, man051File)
		m.DataModel.Collections[0].Fields = append(m.DataModel.Collections[0].Fields, Field{
			Name: name, Type: "string",
		})
		errs := Validate(m, testHost())
		if !hasCode(errs, "MANIFEST_SCHEMA_INVALID") {
			t.Fatalf("expected a MANIFEST_SCHEMA_INVALID error for a field named %q, got %+v", name, errs)
		}
		if !hasField(errs, "dataModel.collections[0].fields[2].name") {
			t.Fatalf("expected the error to name the offending field for %q, got %+v", name, errs)
		}
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

// TestValidateRetentionDescriptorShapes pins MAN-054's value rule: each
// retention entry is `"unbounded"` or a bounded descriptor naming exactly one of
// maxAge/maxRows, each a positive integer.
//
// Measured before written: of the six rejection rules MAN-054 validation
// implements, only the undeclared-collection key had a test. Deleting any of the
// five value-shape rules left the whole suite green — the rules were enforced in
// shipped code and pinned by nothing, so a refactor could have relaxed the
// manifest surface silently.
//
// Each case asserts the SPECIFIC message rather than "some error", because every
// one of these reports the same MANIFEST_SCHEMA_INVALID code: a test satisfied by
// any error passes when the wrong rule fires, which is how five rules end up
// looking covered by one.
//
// The valid rows are a control. Without them a mutant that rejects every
// descriptor — the easiest way to break this surface — passes every negative
// assertion here.
func TestValidateRetentionDescriptorShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		wantMsg string // "" means the descriptor must be accepted
	}{
		{"unbounded is the declared sentinel", `"unbounded"`, ""},
		{"a bounded maxAge in days", `{"maxAge": 365}`, ""},
		{"a bounded maxRows", `{"maxRows": 1000}`, ""},

		{"any other string is not a sentinel", `"forever"`,
			`retention value "forever" MUST be "unbounded" or a {maxAge}/{maxRows} descriptor (MAN-054)`},
		{"a number is neither sentinel nor descriptor", `42`,
			`retention value MUST be "unbounded" or a {maxAge}/{maxRows} descriptor (MAN-054)`},
		{"an array is neither sentinel nor descriptor", `[365]`,
			`retention value MUST be "unbounded" or a {maxAge}/{maxRows} descriptor (MAN-054)`},
		{"maxAge zero is not a bound", `{"maxAge": 0}`,
			"retention maxAge MUST be a positive integer (MAN-054)"},
		{"maxAge negative is not a bound", `{"maxAge": -30}`,
			"retention maxAge MUST be a positive integer (MAN-054)"},
		{"maxRows zero is not a bound", `{"maxRows": 0}`,
			"retention maxRows MUST be a positive integer (MAN-054)"},
		{"maxRows negative is not a bound", `{"maxRows": -5}`,
			"retention maxRows MUST be a positive integer (MAN-054)"},
		{"both bounds is ambiguous, not stricter", `{"maxAge": 30, "maxRows": 100}`,
			"retention descriptor MUST declare exactly one of maxAge or maxRows (MAN-054)"},
		{"an empty descriptor bounds nothing", `{}`,
			"retention descriptor MUST declare exactly one of maxAge or maxRows (MAN-054)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := loadManifest(t, man051File)
			// "notes" is the collection the MAN-051 fixture declares, so the
			// undeclared-key rule (the one rule already tested) cannot be what fires.
			m.Retention["notes"] = []byte(tc.value)

			var got []string
			for _, e := range Validate(m, testHost()) {
				if e.Field == "retention.notes" {
					got = append(got, e.Message)
				}
			}

			if tc.wantMsg == "" {
				if len(got) != 0 {
					t.Fatalf("retention %s is a conformant MAN-054 descriptor and was rejected: %v", tc.value, got)
				}
				return
			}
			if len(got) != 1 || got[0] != tc.wantMsg {
				t.Fatalf("retention %s\n got: %v\nwant: [%s]", tc.value, got, tc.wantMsg)
			}
		})
	}
}
