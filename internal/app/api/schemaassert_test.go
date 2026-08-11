package api_test

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	apispec "github.com/maaxton/waiveo-next/api"
)

// schemaassert_test.go gives a test one assertion: this response body conforms
// to the component schema `api/openapi.yaml` declares for it — VALUES included.
//
// It exists because responseschema_test.go deliberately stops short of values
// ("a hand-rolled partial JSON Schema evaluator that silently skipped the
// keywords it did not implement would be exactly the vacuous check the guards
// exist to prevent") and drives happy paths only. Both choices are right for a
// blanket sweep across every declared operation, and both leave the same hole:
// an ERROR path can serve a member that is present, correctly typed, and still
// not what the document says it is. `AutomationRunScreen.screen_id` was served
// as "" against a required `$ref: Ulid` — every presence check passes and every
// generated typed client is handed an invalid id.
//
// This is not a second sweep and does not try to be: it is a targeted assertion
// a test that DRIVES a specific error path can make, using kin-openapi's own
// evaluator (the same one the request-body gate runs), so nothing is hand-rolled
// and no keyword is silently skipped.

// declaredResponseSchemas parses the embedded document once per test binary.
// The EMBEDDED one, not a copy: a test must not be able to pass against a shape
// the running server never serves.
var declaredResponseSchemas = sync.OnceValues(func() (openapi3.Schemas, error) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.OpenAPIYAML)
	if err != nil {
		return nil, err
	}
	return doc.Components.Schemas, nil
})

// assertMatchesDeclaredSchema fails t unless raw conforms to the named
// component schema.
func assertMatchesDeclaredSchema(t *testing.T, name string, raw []byte) {
	t.Helper()
	schemas, err := declaredResponseSchemas()
	if err != nil {
		t.Fatalf("parse the embedded api surface: %v", err)
	}
	ref, ok := schemas[name]
	if !ok || ref.Value == nil {
		t.Fatalf("api/openapi.yaml declares no component schema %q", name)
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("response is not valid JSON: %v (%s)", err, raw)
	}
	if err := ref.Value.VisitJSON(body); err != nil {
		t.Fatalf("response does not conform to the declared %s schema: %v\nbody: %s", name, err, raw)
	}
}
