package api

// bodyschema.go enforces, at request time, the request-body schema
// `api/openapi.yaml` DECLARES for a resource family — `required`,
// `additionalProperties: false`, `minLength`, `pattern`, `enum`, `minItems`,
// `minProperties`, and every other JSON Schema keyword the document uses.
//
// Before this, the declared schemas were enforced nowhere on the generic
// resource surface. The document said `scope_node` was a required 26-character
// ULID; the handlers read whatever arrived, and a create carrying
// `"scope_node": ""` was stored — placed at a node that does not exist, and
// thereafter readable only by a principal whose binding is the root sentinel.
// Only two of the eight families ran ANY pre-write body check at all
// (`resourceConfig.validate`, opt-in and hand-written), and each checked one
// field of its own. The single field the handlers did enforce from a declared
// schema — a client-supplied `id` (rejectClientSuppliedID, API-105) — was
// written by hand precisely because the schema behind it was not enforced.
//
// The check runs against the EMBEDDED document (apispec.OpenAPIYAML), not a
// transcription of it, so the surface a client is validated against and the
// surface the generated clients are built from cannot disagree.
//
// # What this does NOT cover — stated, not silent
//
// The gap this closes is narrower than the gap that exists. Two exclusions, both
// deliberate:
//
//  1. Playlists, schedules, and dayparts declare NO request body at all in
//     `api/openapi.yaml` ("Shape stub — the row schema is a later minor"). There
//     is nothing to enforce for them. Their bodies remain unchecked, including
//     the `scope_node` placement — a create carrying `"scope_node": ""` on those
//     three kinds is still stored. Closing that has to start with declaring
//     their schemas; this file enforces what is declared and invents nothing.
//
//  2. Scope nodes are deliberately NOT gated here (scopenodes.go says so at the
//     config). Their bodies are already validated field-by-field by
//     datamodel.BuildFullScopeTree inside the store write, which reports EVERY
//     failing member at once under data-model/1's own published per-field codes
//     in API-013's multi-field `errors[]` shape — the shape api/1's own
//     API-013 corpus case pins. A fail-fast gate ahead of it would replace a
//     richer, published, corpus-pinned answer with a poorer one. (Their declared
//     ScopeNodeCreate is also incomplete: it declares no `account_state`, which
//     data-model/1 DAT-010 makes mandatory on an `org`-kind node, so enforcing
//     it as written would make the org node uncreatable.)
//
// Everything downstream stays exactly where it was: this is a pre-write gate in
// front of the store, never a replacement for the store's own validation
// (datamodel rules, referential integrity, the rules/1 compile gate), which
// enforces things a request-body schema cannot express.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	apispec "github.com/maaxton/waiveo-next/api"
)

// declaredSchemas parses the embedded document once per process and yields its
// component schemas by name. Parsing is deferred to first use rather than done
// in an init, and memoized, because the same document is otherwise re-parsed by
// every server constructed in a test binary.
//
// The parsed schemas are read-only from here on: VisitJSON does not mutate the
// schema it validates against (kin-openapi memoizes compiled patterns in its own
// package-level sync.Map), so one parsed copy serves every concurrent request.
var declaredSchemas = sync.OnceValues(func() (map[string]*openapi3.SchemaRef, error) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.OpenAPIYAML)
	if err != nil {
		return nil, fmt.Errorf("parse the declared api surface: %w", err)
	}
	if doc.Components == nil || len(doc.Components.Schemas) == 0 {
		return nil, errors.New("the declared api surface defines no component schemas")
	}
	return doc.Components.Schemas, nil
})

// declaredSchema resolves one component schema by name. A name the document does
// not define is an ERROR rather than a nil schema: a family that names a schema
// and gets nothing back must refuse the write, never proceed unchecked. That is
// the one failure mode this whole file exists to prevent, so it cannot be
// allowed in through the back door of a typo'd component name (SEC-005).
func declaredSchema(name string) (*openapi3.Schema, error) {
	schemas, err := declaredSchemas()
	if err != nil {
		return nil, err
	}
	ref, ok := schemas[name]
	if !ok || ref.Value == nil {
		return nil, fmt.Errorf("api/openapi.yaml declares no component schema %q", name)
	}
	return ref.Value, nil
}

// schemaRejected checks raw against the component schema named by name and, when
// it does not conform, writes the 422 / VALIDATION_FAILED Problem and reports
// true so the caller aborts before any store write (API-013a: a body failure is
// 422). An empty name means the family declares no request body — nothing is
// checked and nothing is written.
//
// A resolution failure (the document unparseable, or the named schema absent)
// answers 500 INTERNAL and returns true: unchecked is not a fallback.
//
// The Problem names the offending member in `detail` and carries no `errors[]`
// array. Two reasons, both deliberate. API-013 mandates `errors[]` for a
// response carrying MORE THAN ONE independent field-level failure, and this
// reports the first violation only. And every `errors[]` entry's own `code` must
// come from the registry of the contract that owns the failing field's rule
// (API-013) — there is no published registry entry for "this member violates the
// declared request schema", and minting one here would put an unpublished code
// in front of a client, which is the exact defect a `code` registry exists to
// prevent (API-011).
func (rs *resource) schemaRejected(w http.ResponseWriter, r *http.Request, name string, raw []byte) bool {
	if name == "" {
		return false
	}
	schema, err := declaredSchema(name)
	if err != nil {
		rs.internal(w, r, err)
		return true
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		rs.problem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Unprocessable Content",
			"The request body is not valid JSON.")
		return true
	}
	if err := schema.VisitJSON(body); err != nil {
		rs.problem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Unprocessable Content",
			schemaViolationDetail(err))
		return true
	}
	return false
}

// schemaViolationDetail renders one schema violation as a human-readable
// `detail` (API-016: `detail` is a per-occurrence explanation whose wording no
// requirement pins; API-045 sets the precedent of naming the offending term
// there).
//
// It is built from the JSON pointer and the validator's own REASON — never from
// the error's default rendering, which appends the offending VALUE. A rejected
// body can carry anything a client sent, including a credential; echoing it into
// a Problem would copy it into every proxy log, every retained idempotency
// replay, and every stored Problem document between here and the caller.
func schemaViolationDetail(err error) string {
	var se *openapi3.SchemaError
	if !errors.As(err, &se) {
		return "The request body does not conform to this operation's declared schema."
	}
	reason := se.Reason
	if reason == "" {
		reason = "does not conform to this operation's declared schema"
	}
	if ptr := strings.Join(se.JSONPointer(), "."); ptr != "" {
		return ptr + ": " + reason + "."
	}
	return strings.ToUpper(reason[:1]) + reason[1:] + "."
}
