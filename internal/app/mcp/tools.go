// Package mcp derives the curated MCP tool surface from the OpenAPI document's
// own `mcp:read`/`mcp:act` tags, and serves it over the MCP stdio transport.
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
// The same reasoning is why the tools this package SERVES execute through
// internal/app/apiop rather than through anything written per operation: the
// request an MCP tool call makes is built from the document that declares it,
// so a tool cannot advertise one shape and send another, and `waiveo call`
// reaches the same operations through the same engine. A CLI and an MCP server
// that were two implementations would eventually be two different platforms.
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

	"github.com/maaxton/waiveo-next/internal/app/apiop"
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

	// Op is the callable declaration behind the tool. It is excluded from the JSON
	// rendering deliberately: `waiveo mcp tools --json` is a REPORT of the curated
	// surface, and folding a few thousand lines of inlined request schema into it
	// would bury the report. A caller that wants the schema asks for it by name.
	Op apiop.Operation `json:"-"`
}

// Tools returns every curated operation for the embedded document.
func Tools() ([]Tool, error) {
	s, err := apiop.Load()
	if err != nil {
		return nil, err
	}
	return ToolsFrom(s)
}

// ToolsFrom returns every curated operation on s, ordered by name so two runs of
// the same document produce the same surface — a tool list whose order drifted
// would show up as a diff in anything that records it.
func ToolsFrom(s *apiop.Surface) ([]Tool, error) {
	if s == nil {
		return nil, fmt.Errorf("mcp: no surface")
	}
	var tools []Tool
	for _, op := range s.Operations() {
		read, act := op.HasTag(TagRead), op.HasTag(TagAct)
		// Neither tag: API-070 says it "MUST NOT be exposed as an MCP tool".
		// Both: a violation validate-mcp-tags refuses, and this declines to guess
		// which one was meant rather than picking the safer-looking answer and
		// hiding the fault.
		if read == act {
			continue
		}
		_, acceptsKey := op.Param("Idempotency-Key")
		tools = append(tools, Tool{
			Name:                   op.ID,
			Description:            describe(op),
			Mutating:               act,
			Method:                 op.Method,
			Path:                   op.Path,
			RequiresIdempotencyKey: act && op.Method == "POST" && acceptsKey,
			Op:                     op,
		})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

// describe prefers the operation's description and falls back to its summary.
func describe(op apiop.Operation) string {
	if d := strings.TrimSpace(op.Description); d != "" {
		return d
	}
	return strings.TrimSpace(op.Summary)
}
