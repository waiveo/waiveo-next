package api

import (
	"slices"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	apispec "github.com/maaxton/waiveo-next/api"
)

// bodyschema_drift_test.go guards the gap that let a declared constraint go
// unenforced without anyone noticing.
//
// kin-openapi models most of JSON Schema and parks what it does not model in
// Schema.Extensions — parsed, carried, and applied by nothing. `propertyNames`
// sat there for the whole life of the body-schema gate: the label-key charset was
// written in the document, generated into both clients, and enforced nowhere, so
// a key carrying selector metacharacters stored fine and then could not be named
// by any selector.
//
// Nothing failed when that happened, and nothing would have failed for the next
// one. These tests are what makes the next one loud.

// enforcedUnmodelledKeywords are the JSON Schema keywords this package enforces
// ITSELF, out of Extensions, because kin-openapi does not.
//
// A keyword goes in here only when there is code applying it. The list is the
// claim; the test below is what checks the claim against the document.
var enforcedUnmodelledKeywords = []string{"propertyNames"}

// annotationKeywords constrain nothing, by JSON Schema's own classification: they
// are documentation carried alongside the schema. `examples` is the one this
// document uses, on several `scope_node` and `entity_id` members.
//
// They are listed rather than pattern-matched because "annotation" is a property
// of the specification, not of the spelling — and the point of this test is to
// notice a keyword nobody thought about, which a loose rule would swallow.
var annotationKeywords = []string{"examples", "$comment", "externalDocs", "$anchor", "$id"}

// loadDoc parses the embedded document the running server validates against —
// not a copy, so a test cannot pass against a shape the server never sees.
func loadDoc(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(apispec.OpenAPIYAML)
	if err != nil {
		t.Fatalf("parse the embedded api surface: %v", err)
	}
	return doc
}

// requestSchemaNames is every component schema a resource family validates a
// request body against, plus the schemas those reach. It is deliberately the
// REQUEST side only: a keyword on a response schema is not silently unenforced,
// it is simply not this gate's business (`const` on Problem.type is one, and
// nothing validates a response body against the document at runtime).
func requestSchemaNames(t *testing.T, doc *openapi3.T) map[string]*openapi3.Schema {
	t.Helper()
	out := map[string]*openapi3.Schema{}

	var walk func(name string, s *openapi3.Schema)
	walk = func(name string, s *openapi3.Schema) {
		if s == nil || out[name] != nil {
			return
		}
		out[name] = s
		for prop, sub := range s.Properties {
			walk(name+"."+prop, sub.Value)
		}
		if ap := s.AdditionalProperties.Schema; ap != nil {
			walk(name+".*", ap.Value)
		}
		if s.Items != nil {
			walk(name+"[]", s.Items.Value)
		}
		for _, group := range [][]*openapi3.SchemaRef{s.AllOf, s.AnyOf, s.OneOf} {
			for i, sub := range group {
				walk(name+".of", sub.Value)
				_ = i
			}
		}
	}

	for _, cfg := range mountedResourceConfigs() {
		for _, schemaName := range []string{cfg.createSchema, cfg.updateSchema, cfg.createMembers, cfg.updateMembers} {
			if schemaName == "" {
				continue
			}
			ref := doc.Components.Schemas[schemaName]
			if ref == nil {
				t.Fatalf("resource config names schema %q, which the document does not declare", schemaName)
			}
			walk(schemaName, ref.Value)
		}
	}
	return out
}

// TestNoRequestSchemaUsesAnUnenforcedKeyword: a keyword kin-openapi does not
// model, on a schema a request body is validated against, is a constraint the
// document states and the server ignores. Adding one must fail here — the choice
// is to enforce it (and list it above) or to stop declaring it, never to leave it
// decorative.
func TestNoRequestSchemaUsesAnUnenforcedKeyword(t *testing.T) {
	for name, schema := range requestSchemaNames(t, loadDoc(t)) {
		for keyword := range schema.Extensions {
			// `x-` members are OpenAPI's own extension namespace and are not JSON
			// Schema constraints at all; this document uses x-go-type.
			if len(keyword) > 2 && keyword[:2] == "x-" {
				continue
			}
			if slices.Contains(annotationKeywords, keyword) {
				continue
			}
			if slices.Contains(enforcedUnmodelledKeywords, keyword) {
				continue
			}
			t.Errorf("%s declares %q, which kin-openapi does not model and this package does not enforce — "+
				"the constraint is in the document, in both generated clients, and applied to nothing. "+
				"Enforce it and add it to enforcedUnmodelledKeywords, or remove it from the document.", name, keyword)
		}
	}
}

// TestEveryEnforcedKeywordIsStillUsed is the inventory's other direction. A
// keyword listed as enforced that no request schema declares means the
// enforcement code is now dead, and a list that outlives what it describes is the
// same lie as a baseline entry that outlives its gap.
func TestEveryEnforcedKeywordIsStillUsed(t *testing.T) {
	used := map[string]bool{}
	for _, schema := range requestSchemaNames(t, loadDoc(t)) {
		for keyword := range schema.Extensions {
			used[keyword] = true
		}
	}
	for _, keyword := range enforcedUnmodelledKeywords {
		if !used[keyword] {
			t.Errorf("enforcedUnmodelledKeywords lists %q, which no request schema declares any more — "+
				"delete the entry and the code that applies it", keyword)
		}
	}
}

// TestEachFamilyIsPairedWithItsOwnSchemas pins what fail-closed does NOT cover.
//
// The existing guard asserts every configured schema name RESOLVES. A name that
// resolves but is the wrong one passes it: `createSchema: "ScreenUpdate"` names a
// real schema, and would accept a create with no name and no scope_node, because
// an Update schema makes every member optional. Resolvable is not correct.
func TestEachFamilyIsPairedWithItsOwnSchemas(t *testing.T) {
	for _, cfg := range mountedResourceConfigs() {
		for _, pair := range []struct{ role, name string }{
			{"Create", cfg.createSchema},
			{"Create", cfg.createMembers},
			{"Update", cfg.updateSchema},
			{"Update", cfg.updateMembers},
		} {
			if pair.name == "" {
				continue
			}
			if !hasSuffix(pair.name, pair.role) {
				t.Errorf("a family validates its %s body against %q, whose name does not end in %q — "+
					"a schema that resolves is not the same as the right schema, and an Update schema on a create path "+
					"accepts a body with every required member missing", pair.role, pair.name, pair.role)
			}
		}
	}
}

// mountedResourceConfigs is every family New() mounts. Enumerated by calling the
// same constructors mountAll does, so a family added there and forgotten here
// shows up as a compile error at the call site rather than as a silently
// unchecked schema.
func mountedResourceConfigs() []resourceConfig {
	return []resourceConfig{
		scopeNodesConfig(),
		schedulesConfig(),
		daypartsConfig(),
		playlistsConfig(),
		automationsConfig(),
		screensConfig(),
		adoptedDevicesConfig(),
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
