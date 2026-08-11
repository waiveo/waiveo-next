// Package apiop makes the management API CALLABLE from the document that
// declares it.
//
// `api/openapi.yaml` already is the single source every generated client, the
// served-surface drift check and the MCP curation read from. This package adds
// the last missing consumer: something that can take an operationId and a bag of
// arguments and produce the actual HTTP request, with the path template, the
// query and header parameters, the request body's media type and its
// required-ness all read OUT OF THE DOCUMENT at call time.
//
// # Why derived, and not a switch
//
// The obvious shape — a switch over operationIds, or a wrapper per method of the
// generated client — is a second, hand-maintained copy of the surface. It is
// stale the moment an operation is added, and nothing tells you: the new
// operation simply is not callable, and the omission looks exactly like the
// author having decided not to expose it. internal/app/mcp makes the same
// argument for the curated tool LIST; this package makes it for the CALL. Adding
// an operation to the document makes it callable here with no Go change at all,
// which is the property that keeps the CLI, the MCP server and the document from
// ever describing three different platforms.
//
// # What it deliberately does not do
//
// It does not decide WHICH operations a caller may reach. Curation is
// internal/app/mcp's job (API-070/071), and a package that re-decided it would be
// a second answer to a question the contract says has one.
package apiop

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "github.com/maaxton/waiveo-next/api"
)

// MediaJSON is the media type nearly every operation's body is written in; the
// handful that are not (a pack zip, a cast bundle, an uploaded asset) declare
// `application/octet-stream` and take opaque bytes.
const MediaJSON = "application/json"

// Param is one declared parameter, flattened across the two places OpenAPI lets
// it be declared: on the path item (applying to every operation on that path)
// and on the operation itself. A caller supplying arguments does not care which
// of the two it came from, and code that reads only one of them silently drops
// every path-level `{id}`.
type Param struct {
	Name        string
	In          string // "path", "query" or "header"
	Required    bool
	Description string
	// Schema is the resolved declaration — the type, bounds and enum a supplied
	// value is checked against. It may be nil for a parameter declared without
	// one, which the document does not currently do.
	Schema *openapi3.SchemaRef
}

// Body describes an operation's request body.
type Body struct {
	Required bool
	// MediaType is the ONE media type this package will send. An operation
	// declaring several is reduced to application/json when it offers it, because
	// that is the one a structured caller can actually construct; otherwise the
	// first in sorted order, so the choice is deterministic across runs.
	MediaType string
	// JSON reports whether MediaType is application/json — i.e. whether a caller
	// may hand this operation a structured value rather than opaque bytes.
	JSON bool
	// Schema is the resolved body schema, or nil when the document reserves it
	// (several families are SHAPE stubs whose schema is a later minor).
	Schema *openapi3.SchemaRef
}

// Operation is one callable operation.
type Operation struct {
	ID          string
	Method      string // upper-case, e.g. "GET"
	Path        string // the template, e.g. "/scope-nodes/{scope_node_id}"
	Summary     string
	Description string
	// Tags is every tag the document puts on the operation, curation tags
	// included. Family() picks the resource family out of it.
	Tags   []string
	Params []Param
	Body   *Body

	// op is retained so a response schema can be looked up without re-walking the
	// document. It is unexported because it is the raw declaration, and a caller
	// reaching into it would be reading the document through a keyhole this
	// package is meant to replace.
	op *openapi3.Operation
}

// Family is the resource family the operation belongs to: its first tag that is
// not a curation tag. Curation is a cross-cutting mark, not a place in the tree,
// so it never names a family.
func (o Operation) Family() string {
	for _, t := range o.Tags {
		if !strings.HasPrefix(t, "mcp:") {
			return t
		}
	}
	return "(untagged)"
}

// HasTag reports whether the operation carries tag.
func (o Operation) HasTag(tag string) bool {
	for _, t := range o.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// Param returns the named parameter.
func (o Operation) Param(name string) (Param, bool) {
	for _, p := range o.Params {
		if p.Name == name {
			return p, true
		}
	}
	return Param{}, false
}

// ResponseSchema is the JSON schema declared for status, or nil when the
// document declares none — either because the operation is a shape stub whose
// response schema is reserved, or because the response has no body at all (a
// 204). A nil schema means "nothing to check here", never "anything goes":
// distinguishing those two is why this returns the schema rather than a verdict.
func (o Operation) ResponseSchema(status int) *openapi3.SchemaRef {
	if o.op == nil || o.op.Responses == nil {
		return nil
	}
	ref := o.op.Responses.Status(status)
	if ref == nil || ref.Value == nil {
		return nil
	}
	// application/json OR any `+json` structured-suffix type. Every error response
	// in this document is `application/problem+json`, and a lookup that only knew
	// the exact type would check success bodies and never check a single Problem —
	// which is the half of the surface a client is most likely to mis-handle.
	if mt := ref.Value.Content.Get(MediaJSON); mt != nil {
		return mt.Schema
	}
	names := make([]string, 0, len(ref.Value.Content))
	for name := range ref.Value.Content {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.HasSuffix(name, "+json") {
			return ref.Value.Content[name].Schema
		}
	}
	return nil
}

// ResponseStatuses is every status the operation declares, ascending. A status
// with no JSON schema (a 204, a shape stub) is still declared and still
// listed — the two questions "what can this answer" and "what shape is that
// answer" have different answers, and collapsing them would report an operation
// whose only declared response is a 204 as declaring none.
func (o Operation) ResponseStatuses() []int {
	if o.op == nil || o.op.Responses == nil {
		return nil
	}
	var out []int
	for code := range o.op.Responses.Map() {
		n, err := strconv.Atoi(code)
		if err != nil {
			// "default" and range forms like "4XX". Neither names one status, and
			// this reports statuses.
			continue
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// Surface is the whole declared API, loaded once.
type Surface struct {
	doc      *openapi3.T
	ops      []Operation
	byID     map[string]int
	basePath string
	families map[string]string
}

// Load parses the embedded document.
//
// Every consumer holds the result rather than calling this per request: parsing
// a 7000-line document is cheap once and silly sixty-four times.
func Load() (*Surface, error) { return loadFrom(apispec.OpenAPIYAML) }

// loadFrom is Load over supplied bytes. It exists so this package's own tests can
// drive derivation from a small, deliberately-shaped document — including shapes
// the real one does not contain today (an array-valued query parameter, a
// parameter colliding with the body argument), which is the only way to prove
// the refusals for those are real rather than merely written down.
func loadFrom(data []byte) (*Surface, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(data)
	if err != nil {
		return nil, fmt.Errorf("apiop: parse the embedded api surface: %w", err)
	}

	s := &Surface{doc: doc, byID: map[string]int{}, families: map[string]string{}}
	for _, t := range doc.Tags {
		s.families[t.Name] = strings.TrimSpace(t.Description)
	}
	// The declared server URL is the prefix every path is relative to ("/api/v1").
	// Reading it is what keeps a CLI from spelling the version prefix itself and
	// then disagreeing with the document the day it moves.
	if len(doc.Servers) > 0 {
		s.basePath = strings.TrimSuffix(doc.Servers[0].URL, "/")
	}

	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if strings.TrimSpace(op.OperationID) == "" {
				return nil, fmt.Errorf("apiop: %s %s declares no operationId", method, path)
			}
			s.ops = append(s.ops, Operation{
				ID:          op.OperationID,
				Method:      strings.ToUpper(method),
				Path:        path,
				Summary:     strings.TrimSpace(op.Summary),
				Description: strings.TrimSpace(op.Description),
				Tags:        append([]string(nil), op.Tags...),
				Params:      collectParams(item, op),
				Body:        collectBody(op),
				op:          op,
			})
		}
	}
	// Sorted by id so two runs over one document produce one order — a surface
	// whose order drifted would show as a diff in anything that records it.
	sort.Slice(s.ops, func(i, j int) bool { return s.ops[i].ID < s.ops[j].ID })
	for i, op := range s.ops {
		if prev, dup := s.byID[op.ID]; dup {
			return nil, fmt.Errorf("apiop: operationId %q declared twice (%s %s and %s %s)",
				op.ID, s.ops[prev].Method, s.ops[prev].Path, op.Method, op.Path)
		}
		s.byID[op.ID] = i
	}
	return s, nil
}

// Operations returns every declared operation, ordered by id.
func (s *Surface) Operations() []Operation { return s.ops }

// Lookup finds an operation by its operationId.
func (s *Surface) Lookup(id string) (Operation, bool) {
	i, ok := s.byID[id]
	if !ok {
		return Operation{}, false
	}
	return s.ops[i], true
}

// BasePath is the prefix declared for the document's first server ("/api/v1").
func (s *Surface) BasePath() string { return s.basePath }

// FamilyDescription is what the document says a resource family is for, or "".
func (s *Surface) FamilyDescription(name string) string { return s.families[name] }

// collectParams flattens the path item's parameters and the operation's own into
// one list, operation-level winning on a name collision — OpenAPI's own override
// rule. Path-level parameters are where every `{id}` in this document is
// declared, so a reader of only op.Parameters would find no path parameters at
// all.
func collectParams(item *openapi3.PathItem, op *openapi3.Operation) []Param {
	byKey := map[string]Param{}
	var order []string
	add := func(refs openapi3.Parameters) {
		for _, ref := range refs {
			if ref == nil || ref.Value == nil {
				continue
			}
			v := ref.Value
			key := v.In + " " + v.Name
			if _, seen := byKey[key]; !seen {
				order = append(order, key)
			}
			byKey[key] = Param{
				Name:        v.Name,
				In:          v.In,
				Required:    v.Required || v.In == openapi3.ParameterInPath,
				Description: strings.TrimSpace(v.Description),
				Schema:      v.Schema,
			}
		}
	}
	add(item.Parameters)
	add(op.Parameters)

	out := make([]Param, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	// Deterministic: path parameters first (they are positional in spirit), then
	// query, then header, alphabetical within each.
	rank := map[string]int{openapi3.ParameterInPath: 0, openapi3.ParameterInQuery: 1, openapi3.ParameterInHeader: 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].In] != rank[out[j].In] {
			return rank[out[i].In] < rank[out[j].In]
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func collectBody(op *openapi3.Operation) *Body {
	if op.RequestBody == nil || op.RequestBody.Value == nil {
		return nil
	}
	rb := op.RequestBody.Value
	if len(rb.Content) == 0 {
		return nil
	}
	media := MediaJSON
	if _, ok := rb.Content[MediaJSON]; !ok {
		names := make([]string, 0, len(rb.Content))
		for k := range rb.Content {
			names = append(names, k)
		}
		sort.Strings(names)
		media = names[0]
	}
	return &Body{
		Required:  rb.Required,
		MediaType: media,
		JSON:      media == MediaJSON,
		Schema:    rb.Content[media].Schema,
	}
}
