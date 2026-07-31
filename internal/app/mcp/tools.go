// Package mcp derives the curated MCP tool surface from the OpenAPI document's
// own `mcp:read`/`mcp:act` tags.
//
// API-071 makes those tags "the sole input to MCP tool generation; no separate
// allowlist or denylist exists" — so this package holds no list of operations,
// and there is nothing here to keep in step with the document. Removing both tags
// from an operation, or removing the operation, retires its tool the next time
// this runs, which is what that requirement asks for.
//
// It DERIVES rather than generates a checked-in artifact, and that is the
// stronger reading of "generated, not hand-maintained": a generated file can be
// stale between the change and the regeneration, and a reviewer cannot tell by
// looking. Reading the embedded document at call time cannot drift from it at
// all.
//
// # What this does not decide
//
// Which operations are curated is the document's business, checked by
// scripts/validate-mcp-tags.mjs (API-070's exclusivity, API-071's sole-channel
// rule, API-072's Idempotency-Key requirement on every `mcp:act` POST). This
// package trusts that gate rather than re-implementing its rules: two checks of
// the same requirement are two things that can disagree about it.
package mcp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "github.com/maaxton/waiveo-next/api"
)

// Curation tags, exactly as API-070 spells them.
const (
	TagRead = "mcp:read"
	TagAct  = "mcp:act"
)

// Tool is one curated operation, in the terms a tool caller needs.
type Tool struct {
	// Name is the operationId. It is already unique across the document (OpenAPI
	// requires it) and is what the generated clients name the same call, so a tool
	// and a client method are recognisably the same operation.
	Name string
	// Description is what the tool tells a model it does. Falls back to the
	// summary, then to nothing — a tool with no description is a tool a model will
	// guess about, so the emptiness is worth seeing rather than papering over.
	Description string
	// Mutating is API-070's distinction: `mcp:act` mutates state, `mcp:read` has
	// "no side effect a retry could double-apply". A caller decides retry and
	// confirmation policy from this, so it is a field rather than a tag string to
	// re-parse.
	Mutating bool
	Method   string
	Path     string
	// RequiresIdempotencyKey mirrors API-072: a mutating POST accepts one, so an
	// MCP client's own retry-on-timeout cannot double-apply the call. Reported so a
	// tool caller can supply one rather than having to know the rule.
	RequiresIdempotencyKey bool
}

// Tools returns every curated operation, ordered by name so two runs of the same
// document produce the same surface — a tool list whose order drifted would show
// up as a diff in anything that records it.
func Tools() ([]Tool, error) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.OpenAPIYAML)
	if err != nil {
		return nil, fmt.Errorf("mcp: parse the embedded api surface: %w", err)
	}

	var tools []Tool
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			read, act := hasTag(op, TagRead), hasTag(op, TagAct)
			// Neither tag: API-070 says it "MUST NOT be exposed as an MCP tool".
			// Both: a violation validate-mcp-tags refuses, and this declines to guess
			// which one was meant rather than picking the safer-looking answer and
			// hiding the fault.
			if read == act {
				continue
			}
			tools = append(tools, Tool{
				Name:                   op.OperationID,
				Description:            describe(op),
				Mutating:               act,
				Method:                 method,
				Path:                   path,
				RequiresIdempotencyKey: act && method == "POST" && acceptsIdempotencyKey(item, op),
			})
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

func hasTag(op *openapi3.Operation, tag string) bool {
	for _, t := range op.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// describe prefers the operation's description and falls back to its summary.
func describe(op *openapi3.Operation) string {
	if d := strings.TrimSpace(op.Description); d != "" {
		return d
	}
	return strings.TrimSpace(op.Summary)
}

// acceptsIdempotencyKey reports whether the operation accepts the header, counting
// a parameter declared once for the whole PATH — one declared at the path item
// applies to every operation on it, and an operation that inherits the header does
// accept it. Missing that inheritance is how a checker concludes a conformant
// document is in violation.
//
// No path item in this document declares the header that way TODAY, so that half
// is defensive rather than exercised — measured, not assumed, and stated because a
// mutation that removes it currently fails no test. It is kept because
// scripts/validate-mcp-tags.mjs counts the same inheritance for the same
// requirement: dropping it here would make the two disagree the first time anyone
// hoists a parameter to a path item, and they would disagree silently.
func acceptsIdempotencyKey(item *openapi3.PathItem, op *openapi3.Operation) bool {
	for _, set := range []openapi3.Parameters{item.Parameters, op.Parameters} {
		for _, ref := range set {
			if ref.Value != nil && ref.Value.In == "header" && ref.Value.Name == "Idempotency-Key" {
				return true
			}
		}
	}
	return false
}
