package packs_test

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/manifest"
)

// A fixture scope-node ULID (fixture-ULID convention; no secrets).
const fixtureScopeNode = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"

// menuItemsCollection is a non-lifecycle collection with a mix of declared field
// types (string/number/boolean) — the shape the example menu-board pack uses.
func menuItemsCollection() manifest.Collection {
	return manifest.Collection{
		Name: "menu_items",
		Fields: []manifest.Field{
			{Name: "name", Type: "string", Role: "title", Searchable: true},
			{Name: "price", Type: "number"},
			{Name: "section", Type: "string"},
			{Name: "available", Type: "boolean"},
		},
	}
}

// draftPublishCollection carries the MAN-052 lifecycle:draft-publish annotation on
// a field, so the whole collection participates in the draft/publish flow.
func draftPublishCollection() manifest.Collection {
	return manifest.Collection{
		Name: "articles",
		Fields: []manifest.Field{
			{Name: "title", Type: "string", Role: "title", Lifecycle: "draft-publish"},
		},
	}
}

// rowErrByField returns the RowError at field, and whether it is present.
func rowErrByField(errs []packs.RowError, field string) (packs.RowError, bool) {
	for _, e := range errs {
		if e.Field == field {
			return e, true
		}
	}
	return packs.RowError{}, false
}

func mustNoRowErrors(t *testing.T, errs []packs.RowError) {
	t.Helper()
	if len(errs) != 0 {
		t.Fatalf("unexpected validation errors: %+v", errs)
	}
}

// TestValidateRowWriteValidRow: a well-formed row (required scope_node + declared
// fields of the right types) passes, and the returned body carries exactly the
// declared fields with defaults applied to the envelope.
func TestValidateRowWriteValidRow(t *testing.T) {
	body := `{"scope_node":"` + fixtureScopeNode + `","name":"Burger","price":9.5,"available":true,"section":"Mains"}`
	vr, errs := packs.ValidateRowWrite(menuItemsCollection(), []byte(body))
	mustNoRowErrors(t, errs)
	if vr.ScopeNode != fixtureScopeNode {
		t.Fatalf("scope_node = %q, want %q", vr.ScopeNode, fixtureScopeNode)
	}
	if vr.LifecycleState != "published" {
		t.Fatalf("lifecycle_state default = %q, want published", vr.LifecycleState)
	}
	if vr.Labels == nil || len(vr.Labels) != 0 {
		t.Fatalf("labels default = %v, want []", vr.Labels)
	}
	var got map[string]any
	if err := json.Unmarshal(vr.Body, &got); err != nil {
		t.Fatalf("decode body: %v (%s)", err, vr.Body)
	}
	if got["name"] != "Burger" || got["section"] != "Mains" {
		t.Fatalf("declared body = %+v", got)
	}
	// The envelope must NOT leak into the declared-fields body.
	if _, ok := got["scope_node"]; ok {
		t.Fatalf("scope_node leaked into declared body: %+v", got)
	}
}

// TestValidateRowWriteScopeNodeRequired: a row missing scope_node is rejected.
func TestValidateRowWriteScopeNodeRequired(t *testing.T) {
	_, errs := packs.ValidateRowWrite(menuItemsCollection(), []byte(`{"name":"Burger"}`))
	e, ok := rowErrByField(errs, "scope_node")
	if !ok || e.Code != "REQUIRED" {
		t.Fatalf("scope_node error = %+v, want REQUIRED", errs)
	}
}

// TestValidateRowWriteScopeNodeNotULID: a scope_node that is not a ULID is rejected.
func TestValidateRowWriteScopeNodeNotULID(t *testing.T) {
	_, errs := packs.ValidateRowWrite(menuItemsCollection(), []byte(`{"scope_node":"not-a-ulid","name":"Burger"}`))
	e, ok := rowErrByField(errs, "scope_node")
	if !ok || e.Code != "INVALID_ULID" {
		t.Fatalf("scope_node error = %+v, want INVALID_ULID", errs)
	}
}

// TestValidateRowWriteFieldTypeMismatch: a declared field whose value's JSON type
// does not match the declared type is rejected on that field.
func TestValidateRowWriteFieldTypeMismatch(t *testing.T) {
	body := `{"scope_node":"` + fixtureScopeNode + `","name":"Burger","price":"free","available":"yes"}`
	_, errs := packs.ValidateRowWrite(menuItemsCollection(), []byte(body))
	if e, ok := rowErrByField(errs, "price"); !ok || e.Code != "INVALID_TYPE" {
		t.Fatalf("price error = %+v, want INVALID_TYPE", errs)
	}
	if e, ok := rowErrByField(errs, "available"); !ok || e.Code != "INVALID_TYPE" {
		t.Fatalf("available error = %+v, want INVALID_TYPE", errs)
	}
}

// TestValidateRowWriteUnknownField: a body key that is not an envelope field,
// external_id, host-owned, or a declared field is refused UNKNOWN_FIELD.
func TestValidateRowWriteUnknownField(t *testing.T) {
	body := `{"scope_node":"` + fixtureScopeNode + `","name":"Burger","surprise":1}`
	_, errs := packs.ValidateRowWrite(menuItemsCollection(), []byte(body))
	if e, ok := rowErrByField(errs, "surprise"); !ok || e.Code != "UNKNOWN_FIELD" {
		t.Fatalf("surprise error = %+v, want UNKNOWN_FIELD", errs)
	}
}

// TestValidateRowWriteHostOwnedIgnored: host-owned envelope fields echoed in the
// body (a full-row PATCH roundtrip) are ignored, not errors, and never land in the
// declared-fields body.
func TestValidateRowWriteHostOwnedIgnored(t *testing.T) {
	body := `{"scope_node":"` + fixtureScopeNode + `","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2X3","revision":7,"created_at":1,"updated_at":2,"name":"Burger"}`
	vr, errs := packs.ValidateRowWrite(menuItemsCollection(), []byte(body))
	mustNoRowErrors(t, errs)
	var got map[string]any
	_ = json.Unmarshal(vr.Body, &got)
	for _, k := range []string{"entity_id", "revision", "created_at", "updated_at"} {
		if _, ok := got[k]; ok {
			t.Fatalf("host-owned %q leaked into declared body: %+v", k, got)
		}
	}
}

// TestValidateRowWriteLifecycleRejectedWhenNotDeclared: MAN-052 — a collection with
// no draft-publish annotation rejects any non-published lifecycle_state.
func TestValidateRowWriteLifecycleRejectedWhenNotDeclared(t *testing.T) {
	body := `{"scope_node":"` + fixtureScopeNode + `","name":"Burger","lifecycle_state":"draft"}`
	_, errs := packs.ValidateRowWrite(menuItemsCollection(), []byte(body))
	if e, ok := rowErrByField(errs, "lifecycle_state"); !ok || e.Code != "LIFECYCLE_NOT_ALLOWED" {
		t.Fatalf("lifecycle error = %+v, want LIFECYCLE_NOT_ALLOWED", errs)
	}
}

// TestValidateRowWritePublishedAllowedWhenNotDeclared: MAN-052 — the non-participating
// collection accepts an explicit "published" (the only permitted state).
func TestValidateRowWritePublishedAllowedWhenNotDeclared(t *testing.T) {
	body := `{"scope_node":"` + fixtureScopeNode + `","name":"Burger","lifecycle_state":"published"}`
	vr, errs := packs.ValidateRowWrite(menuItemsCollection(), []byte(body))
	mustNoRowErrors(t, errs)
	if vr.LifecycleState != "published" {
		t.Fatalf("lifecycle_state = %q, want published", vr.LifecycleState)
	}
}

// TestValidateRowWriteDraftAllowedWhenDeclared: MAN-052 — a participating collection
// accepts draft/published/archived and rejects anything else.
func TestValidateRowWriteDraftAllowedWhenDeclared(t *testing.T) {
	for _, state := range []string{"draft", "published", "archived"} {
		body := `{"scope_node":"` + fixtureScopeNode + `","title":"X","lifecycle_state":"` + state + `"}`
		vr, errs := packs.ValidateRowWrite(draftPublishCollection(), []byte(body))
		mustNoRowErrors(t, errs)
		if vr.LifecycleState != state {
			t.Fatalf("lifecycle_state = %q, want %q", vr.LifecycleState, state)
		}
	}
	body := `{"scope_node":"` + fixtureScopeNode + `","title":"X","lifecycle_state":"bogus"}`
	_, errs := packs.ValidateRowWrite(draftPublishCollection(), []byte(body))
	if e, ok := rowErrByField(errs, "lifecycle_state"); !ok || e.Code != "INVALID_LIFECYCLE_STATE" {
		t.Fatalf("bogus lifecycle error = %+v, want INVALID_LIFECYCLE_STATE", errs)
	}
}

// TestValidateRowWriteParamsWithoutTemplate: params present without template_ref is
// rejected (MAN-051: params is present only when template_ref is).
func TestValidateRowWriteParamsWithoutTemplate(t *testing.T) {
	body := `{"scope_node":"` + fixtureScopeNode + `","name":"Burger","params":{"k":"v"}}`
	_, errs := packs.ValidateRowWrite(menuItemsCollection(), []byte(body))
	if e, ok := rowErrByField(errs, "params"); !ok || e.Code != "PARAMS_WITHOUT_TEMPLATE" {
		t.Fatalf("params error = %+v, want PARAMS_WITHOUT_TEMPLATE", errs)
	}
}

// TestValidateRowWriteLabelsMustBeStrings: labels that are not an array of strings
// are rejected.
func TestValidateRowWriteLabelsMustBeStrings(t *testing.T) {
	body := `{"scope_node":"` + fixtureScopeNode + `","name":"Burger","labels":[1,2,3]}`
	_, errs := packs.ValidateRowWrite(menuItemsCollection(), []byte(body))
	if e, ok := rowErrByField(errs, "labels"); !ok || e.Code != "INVALID_TYPE" {
		t.Fatalf("labels error = %+v, want INVALID_TYPE", errs)
	}
}

// TestValidateRowWriteIntegerType: a declared integer field accepts a whole number
// and rejects a fractional one.
func TestValidateRowWriteIntegerType(t *testing.T) {
	c := manifest.Collection{Name: "counts", Fields: []manifest.Field{{Name: "qty", Type: "integer"}}}
	if _, errs := packs.ValidateRowWrite(c, []byte(`{"scope_node":"`+fixtureScopeNode+`","qty":5}`)); len(errs) != 0 {
		t.Fatalf("integer 5 rejected: %+v", errs)
	}
	_, errs := packs.ValidateRowWrite(c, []byte(`{"scope_node":"`+fixtureScopeNode+`","qty":5.5}`))
	if e, ok := rowErrByField(errs, "qty"); !ok || e.Code != "INVALID_TYPE" {
		t.Fatalf("integer 5.5 error = %+v, want INVALID_TYPE", errs)
	}
}

// TestValidateRowWriteMalformedBody: a non-object body is a single VALIDATION_FAILED.
func TestValidateRowWriteMalformedBody(t *testing.T) {
	_, errs := packs.ValidateRowWrite(menuItemsCollection(), []byte(`not json`))
	if len(errs) != 1 || errs[0].Code != "VALIDATION_FAILED" {
		t.Fatalf("malformed body errors = %+v, want one VALIDATION_FAILED", errs)
	}
}

// TestCollectionByName: the manifest lookup finds a declared collection and misses
// an undeclared one.
func TestCollectionByName(t *testing.T) {
	m := &manifest.PackManifest{DataModel: manifest.DataModel{Collections: []manifest.Collection{menuItemsCollection()}}}
	if _, ok := packs.CollectionByName(m, "menu_items"); !ok {
		t.Fatal("menu_items not found")
	}
	if _, ok := packs.CollectionByName(m, "nope"); ok {
		t.Fatal("nope unexpectedly found")
	}
}
