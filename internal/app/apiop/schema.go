package apiop

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// BodyArg is the argument name a structured (application/json) request body is
// supplied under, in both the CLI and the MCP tool schema.
const BodyArg = "body"

// BodyPathArg is the argument name a NON-json body is supplied under: a path to
// the bytes. A pack zip or a cast bundle cannot travel as a JSON value, and
// base64 through a model's context is not a serious way to move a megabyte, so
// the caller names a local file and the engine streams it.
const BodyPathArg = "body_path"

// engineHeaders are the two header parameters a caller never supplies, because
// this engine supplies them:
//
//   - Idempotency-Key: API-072 requires a mutating POST to accept one so a
//     client's retry-on-timeout cannot double-apply. A key the CALLER has to
//     remember to pass is a key that is missing exactly when a retry happens, so
//     the engine mints one per invocation and reports it.
//   - Trace-Id: the server mints one when the client sends none, and echoes it on
//     every response including every error. Asking a caller to invent one buys
//     nothing over reading back the one the server used, which every Result
//     carries.
//
// Any OTHER header parameter — If-Match above all — is a caller's argument and
// appears in the schema. If-Match is the whole of optimistic concurrency
// (API-022/023): an engine that filled it in would be an engine that silently
// clobbers.
var engineHeaders = map[string]bool{
	"Idempotency-Key": true,
	"Trace-Id":        true,
}

// maxSchemaDepth bounds how far a request-body schema is inlined into a tool's
// input schema. The document's deepest bodies (a cast's slides, each a stack of
// layers) run several levels, and an unbounded inline of a self-referential
// schema does not terminate. Past the bound the sub-schema is left unconstrained
// and SAYS SO, rather than being dropped: a caller that cannot see a constraint
// still learns the field exists.
const maxSchemaDepth = 10

// InputSchema is the JSON Schema for one operation's arguments: every declared
// parameter the caller supplies, plus the request body.
//
// It is the SAME schema the CLI validates against and the MCP server advertises,
// which is the point — a tool description and a command-line check that were
// derived separately would eventually disagree about what a valid call is.
//
// Every `$ref` is inlined. An MCP client has no way to resolve
// `#/components/schemas/Ulid` against a document it has never seen, so a tool
// schema that referenced one would advertise a constraint no caller could read.
func (s *Surface) InputSchema(op Operation) (map[string]any, error) {
	props := map[string]any{}
	var required []string

	for _, p := range op.Params {
		if p.In == openapi3.ParameterInHeader && engineHeaders[p.Name] {
			continue
		}
		sub := s.inline(p.Schema, map[string]bool{}, 0)
		if p.Description != "" {
			sub = withDescription(sub, p.Description)
		}
		sub = withDescription(sub, fmt.Sprintf("(%s parameter)", p.In))
		props[p.Name] = sub
		if p.Required {
			required = append(required, p.Name)
		}
	}

	if op.Body != nil {
		name := BodyArg
		if !op.Body.JSON {
			name = BodyPathArg
		}
		if _, clash := props[name]; clash {
			// A parameter literally named `body` would silently shadow the request
			// body and the caller would never learn which one they filled in. No
			// operation does this today; the day one does, this says so instead of
			// producing a quietly wrong tool.
			return nil, fmt.Errorf("apiop: %s declares a parameter named %q, which collides with the request-body argument", op.ID, name)
		}
		if op.Body.JSON {
			sub := s.inline(op.Body.Schema, map[string]bool{}, 0)
			props[name] = withDescription(sub, "The request body.")
		} else {
			props[name] = map[string]any{
				"type":        "string",
				"description": fmt.Sprintf("Path to a local file holding the request body (%s).", op.Body.MediaType),
			}
		}
		if op.Body.Required {
			required = append(required, name)
		}
	}

	sort.Strings(required)
	schema := map[string]any{
		"type":       "object",
		"properties": props,
		// Closed: an argument this operation does not declare is a mistake worth
		// hearing about, not a value to drop on the floor.
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema, nil
}

// withDescription appends text to a schema's description, creating it when
// absent. Appending rather than overwriting is what lets the parameter's own
// prose and this package's "(query parameter)" note both survive.
func withDescription(schema map[string]any, text string) map[string]any {
	text = strings.TrimSpace(text)
	if text == "" {
		return schema
	}
	existing, _ := schema["description"].(string)
	switch {
	case existing == "":
		schema["description"] = text
	case strings.Contains(existing, text):
	default:
		schema["description"] = existing + " " + text
	}
	return schema
}

// inline turns a resolved SchemaRef into plain JSON Schema.
//
// Scalar keywords are carried over WHOLESALE, by marshalling the resolved schema
// and reading the result back: enumerating them by hand is how a keyword gets
// dropped the first time the document uses one nobody thought of. Only the
// schema-VALUED keywords are handled explicitly below, because those are the
// ones that carry a `$ref` needing resolution and a cycle needing breaking.
func (s *Surface) inline(ref *openapi3.SchemaRef, seen map[string]bool, depth int) map[string]any {
	if ref == nil || ref.Value == nil {
		return map[string]any{}
	}
	if depth >= maxSchemaDepth {
		return map[string]any{"description": "Nested beyond the depth this tool schema inlines; see api/openapi.yaml for the full shape."}
	}
	if ref.Ref != "" {
		if seen[ref.Ref] {
			return map[string]any{"description": fmt.Sprintf("Recursive reference to %s; unconstrained here.", shortRef(ref.Ref))}
		}
		// A copy per branch, not one shared map: two SIBLING properties may both
		// legitimately reference the same schema, and a shared set would call the
		// second one recursive.
		next := make(map[string]bool, len(seen)+1)
		for k := range seen {
			next[k] = true
		}
		next[ref.Ref] = true
		seen = next
	}

	raw, err := json.Marshal(ref.Value)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	// OpenAPI-only vocabulary an MCP client's JSON Schema validator has no meaning
	// for, plus every `x-` extension — `x-go-type: "*string"` is a codegen
	// instruction, and a model reading it learns something about this repo's Go
	// bindings rather than about the value it is being asked for.
	for _, k := range []string{"discriminator", "xml", "externalDocs"} {
		delete(out, k)
	}
	for k := range out {
		if strings.HasPrefix(k, "x-") {
			delete(out, k)
		}
	}

	v := ref.Value
	if len(v.Properties) > 0 {
		p := make(map[string]any, len(v.Properties))
		for name, sub := range v.Properties {
			p[name] = s.inline(sub, seen, depth+1)
		}
		out["properties"] = p
	}
	if v.Items != nil {
		out["items"] = s.inline(v.Items, seen, depth+1)
	}
	for key, set := range map[string]openapi3.SchemaRefs{"allOf": v.AllOf, "anyOf": v.AnyOf, "oneOf": v.OneOf} {
		if len(set) == 0 {
			continue
		}
		list := make([]any, 0, len(set))
		for _, sub := range set {
			list = append(list, s.inline(sub, seen, depth+1))
		}
		out[key] = list
	}
	if v.Not != nil {
		out["not"] = s.inline(v.Not, seen, depth+1)
	}
	if v.AdditionalProperties.Schema != nil {
		out["additionalProperties"] = s.inline(v.AdditionalProperties.Schema, seen, depth+1)
	} else if v.AdditionalProperties.Has != nil {
		out["additionalProperties"] = *v.AdditionalProperties.Has
	}
	return out
}

func shortRef(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}
