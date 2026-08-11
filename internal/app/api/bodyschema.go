package api

// bodyschema.go enforces, at request time, the request-body schema
// `api/openapi.yaml` DECLARES for a resource family — `required`,
// `additionalProperties: false`, `minLength`, `pattern`, `enum`, `minItems`,
// `minProperties`, and `propertyNames`.
//
// Not "every keyword the document uses", which this comment used to claim and
// which was wrong twice over. `propertyNames` was declared and enforced by
// nothing until propertyNamesViolation below; and `format` is not validated at
// all, because kin-openapi only applies it when explicitly enabled — so
// `WebhookEndpointCreate.url: format: uri` accepts `not a url at all` here. That
// one is not exploitable, since the handler independently checks scheme, host,
// userinfo and query before storing, but a comment claiming coverage it does not
// have is worse than no comment: it stops the next reader looking.
//
// What is and is not enforced is now checked rather than described:
// bodyschema_drift_test.go fails on any keyword this document uses that
// kin-openapi does not model and this file does not apply.
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
// The gap this closes is narrower than the gap that exists. Two exclusions are
// deliberate and described below — but they are not the whole residue. Of the
// document's declared request schemas, the ones reached here are the seven
// resource families; the action-style POSTs (bulk-enable, workspace export and
// delete, the webhook signing secret, entity command, automation run, marketplace
// ref, TOTP confirm, login, claim, credential reset) each carry hand-written
// guards on their key fields, so what they miss in practice is
// `additionalProperties: false` — POST /automations/bulk-enable accepts an
// undeclared member. That is tracked, not fixed here.
//
//  1. Playlists, schedules, and dayparts declare NO request body at all in
//     `api/openapi.yaml` ("Shape stub — the row schema is a later minor"). There
//     is nothing to enforce for them. Their bodies remain unchecked HERE, and
//     closing that has to start with declaring their schemas; this file enforces
//     what is declared and invents nothing.
//
//     The `scope_node` placement is no longer among the gaps: an unplaced
//     scheduling-core row is refused by the datamodel validator
//     (checkRowPlacement, ROW_SCOPE_NODE_MISSING), on create and on a PATCH that
//     would clear an existing placement. What is still unchecked here is the rest
//     of those bodies — an undeclared member, a mistyped field — which is a
//     schema question rather than a placement one.
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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	apispec "github.com/maaxton/waiveo-next/api"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
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
	return rs.srv.schemaRejected(w, r, name, raw)
}

// schemaRejected on the server is the same whole-schema gate for an ACTION-style
// body that is not a resource family's — today, the push-now surface's
// ScreenNowRequest.
//
// One implementation, two entry points, for the reason undeclaredMemberRejected
// gives below: a second copy is how two checks drift into disagreeing about what
// the same document says. It matters more here than for the member-name half,
// because this one reads VALUES: a hand-written restatement of a schema's
// `enum`/`minimum`/`maxLength` is a restatement that can be incomplete, and the
// push-now body's was — the document declared `ttl_seconds: minimum: 1` and the
// handler checked only `< 0`, so a `ttl_seconds: 0` the schema forbids was
// accepted and stored. Whatever the document declares for this body — including
// `additionalProperties: false` at EVERY nesting depth, which a top-level
// DisallowUnknownFields decode does not reach into a member the struct types as
// a map or a raw message — is now enforced from the document itself.
func (srv *server) schemaRejected(w http.ResponseWriter, r *http.Request, name string, raw []byte) bool {
	if name == "" {
		return false
	}
	problem := func(detail string) {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
			"VALIDATION_FAILED", "Unprocessable Content", detail, nil)
	}
	schema, err := declaredSchema(name)
	if err != nil {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusInternalServerError,
			"INTERNAL", "Internal Server Error", "An unexpected server error occurred.", nil)
		return true
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		problem("The request body is not valid JSON.")
		return true
	}
	if err := schema.VisitJSON(body); err != nil {
		problem(schemaViolationDetail(err))
		return true
	}
	if detail := propertyNamesViolation(schema, body); detail != "" {
		problem(detail)
		return true
	}
	return false
}

// propertyNamesViolation enforces `propertyNames`, which VisitJSON does not.
//
// kin-openapi does not model the keyword, so it parks it in Schema.Extensions —
// parsed, carried, and applied by nothing. The distinction matters: the
// constraint is not lost, it is unenforced, which is why this can read it from
// the same parsed document rather than re-parsing the YAML or restating the rule.
//
// It is the only place the label-key charset is enforced on a write. API-042
// constrains keys and values alike so "every label is expressible in a selector
// term without escaping", and the VALUE half was enforced (its `pattern` sits on
// additionalProperties, which VisitJSON does apply) while the KEY half was not.
// A key carrying `,` `=` `!` or a space stored fine and then could not be named
// by any selector — the row is there and no scoped query finds it. The selector
// parser's own key check runs when READING a selector, never on the way in, so
// there was no second line of defence.
//
// Returns "" when the body conforms.
func propertyNamesViolation(schema *openapi3.Schema, body any) string {
	if schema == nil {
		return ""
	}
	obj, isObject := body.(map[string]any)

	if pattern, ok := propertyNamePattern(schema); ok && isObject {
		re, err := regexp.Compile(pattern)
		if err != nil {
			// A document whose own pattern will not compile is a document problem, and
			// reporting it as a client's malformed body would send an operator hunting
			// through their request for a fault that is not there.
			return fmt.Sprintf("this operation's declared propertyNames pattern is not a valid regular expression (%v).", err)
		}
		// Sorted, so a body with several bad keys always names the same one — an
		// unordered map would make the same request answer differently between runs.
		var bad []string
		for k := range obj {
			if !re.MatchString(k) {
				bad = append(bad, k)
			}
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			return fmt.Sprintf("%s: not a permitted property name.", clipKey(bad[0]))
		}
	}

	// Recurse the shapes a request body actually takes here. Deliberately not a
	// general JSON Schema walker: this reaches the members, array elements and
	// composition branches this document uses, and a keyword it does not reach is
	// caught by TestNoRequestSchemaUsesAnUnenforcedKeyword rather than passing
	// quietly.
	if isObject {
		for name, sub := range schema.Properties {
			if v, present := obj[name]; present {
				if d := propertyNamesViolation(sub.Value, v); d != "" {
					return d
				}
			}
		}
		if ap := schema.AdditionalProperties.Schema; ap != nil {
			for name, v := range obj {
				if _, declared := schema.Properties[name]; declared {
					continue
				}
				if d := propertyNamesViolation(ap.Value, v); d != "" {
					return d
				}
			}
		}
	}
	if arr, isArray := body.([]any); isArray && schema.Items != nil {
		for _, v := range arr {
			if d := propertyNamesViolation(schema.Items.Value, v); d != "" {
				return d
			}
		}
	}
	for _, group := range [][]*openapi3.SchemaRef{schema.AllOf, schema.AnyOf, schema.OneOf} {
		for _, sub := range group {
			if d := propertyNamesViolation(sub.Value, body); d != "" {
				return d
			}
		}
	}
	return ""
}

// propertyNamePattern reads a schema's `propertyNames.pattern` out of the
// Extensions map kin-openapi parks unmodelled keywords in.
func propertyNamePattern(schema *openapi3.Schema) (string, bool) {
	raw, ok := schema.Extensions["propertyNames"]
	if !ok {
		return "", false
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return "", false
	}
	pattern, ok := obj["pattern"].(string)
	return pattern, ok
}

// maxEchoedKey bounds how much of a client-supplied KEY a Problem echoes.
//
// The detail never carries a value, but a key is a name and names are reflected —
// by this function's callers and by kin-openapi's own reason string. A 100 KB
// property name produced a 100 KB detail, and because a 422 is under 500 it is
// RETAINED and replayed verbatim for the idempotency window: a client controls
// both the amplification and how long it is stored. Clipping costs nothing an
// operator needs, since a name too long to read is a name too long to fix by
// reading.
const maxEchoedKey = 96

func clipKey(key string) string {
	if len(key) <= maxEchoedKey {
		return key
	}
	return key[:maxEchoedKey] + fmt.Sprintf("… (%d bytes)", len(key))
}

// declaredMemberNames returns the member names the component schema `name`
// declares, memoized per process.
//
// It reads the SAME embedded document schemaRejected validates against, so the
// set a body is bounded by and the set the generated clients are built from
// cannot disagree.
var declaredMemberNames = func() func(string) (map[string]bool, error) {
	var mu sync.Mutex
	cache := map[string]map[string]bool{}
	return func(name string) (map[string]bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if got, ok := cache[name]; ok {
			return got, nil
		}
		schema, err := declaredSchema(name)
		if err != nil {
			return nil, err
		}
		set := make(map[string]bool, len(schema.Properties))
		for member := range schema.Properties {
			set[member] = true
		}
		if len(set) == 0 {
			// A schema with no properties would bound a body to nothing, refusing every
			// write. That is a document problem, not a client problem, and silently
			// accepting everything instead would make this check invisible.
			return nil, fmt.Errorf("component schema %q declares no properties, so it cannot bound a request body", name)
		}
		cache[name] = set
		return set, nil
	}
}()

// undeclaredMemberRejected enforces ONE keyword of a declared schema —
// `additionalProperties: false` — and nothing else.
//
// It exists because the scope-node family cannot use the full schema gate above.
// Their bodies are validated field by field inside the store write, which reports
// EVERY failing member at once under data-model/1's published per-field codes in
// API-013's multi-field `errors[]` shape; a fail-fast whole-schema check ahead of
// that would replace a richer, corpus-pinned answer with a poorer one. But the
// document also declares `additionalProperties: false`, and no data-model rule can
// express that: those validators see a DECODED row, where a member the struct does
// not define has already vanished. So an undeclared member was accepted, stored,
// and served back on every read.
//
// The narrowness is structural rather than promised: this compares member NAMES
// against the declared set and never inspects a value, so it is incapable of
// pre-empting a per-field code no matter how the body is malformed.
func (rs *resource) undeclaredMemberRejected(w http.ResponseWriter, r *http.Request, name string, raw []byte) bool {
	return rs.srv.undeclaredMemberRejected(w, r, name, raw)
}

// undeclaredMemberRejected on the server is the same check for the ACTION-style
// POSTs — run, bulk-enable, entity command, workspace export and delete, the
// webhook signing secret, pack install. They are not resource families, so they
// reach none of the resource pipeline, and each carries hand-written guards on the
// fields it needs. What none of them had was a bound on the members they do NOT
// declare, so an undeclared member rode through: bulk-enable answered 202 and
// accepted the job.
//
// One implementation, two entry points, because the alternative — a second copy
// for the handlers that are not families — is how the two drift into disagreeing
// about what the same document says.
func (srv *server) undeclaredMemberRejected(w http.ResponseWriter, r *http.Request, name string, raw []byte) bool {
	if name == "" {
		return false
	}
	declared, err := declaredMemberNames(name)
	if err != nil {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.", nil)
		return true
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		// Not this check's business: a non-object or unparseable body is caught by the
		// decode the write path already performs, with its own message.
		return false
	}
	// Sorted, so a body carrying several undeclared members always names the same
	// one — an unordered map would report whichever the runtime happened to yield
	// and make the same request answer differently between runs.
	var unknown []string
	for member := range body {
		if !declared[member] {
			unknown = append(unknown, member)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
			"VALIDATION_FAILED", "Unprocessable Content",
			fmt.Sprintf("%s: not a member this operation declares.", clipKey(unknown[0])), nil)
		return true
	}

	// propertyNames is the same CLASS of constraint — a rule about a name rather
	// than a value — so a family running the member half runs this too. It is what
	// keeps a scope node's label keys inside API-042's charset: nothing downstream
	// looks at labels at all, and the selector parser's own key check runs when
	// READING a selector, never on the way in.
	//
	// Label VALUES stay unchecked for these families, and that is the value half
	// they deliberately opt out of rather than an omission here.
	schema, err := declaredSchema(name)
	if err != nil {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.", nil)
		return true
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return false
	}
	if detail := propertyNamesViolation(schema, decoded); detail != "" {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
			"VALIDATION_FAILED", "Unprocessable Content", detail, nil)
		return true
	}
	// minProperties belongs here for the same reason: it constrains how many
	// members a body has, not what any of them contains, so a family running the
	// member half can enforce it without touching the value half it opted out of.
	//
	// It was NOT enforced for these families, and the consequence was API-013b's
	// exact failure mode. `PATCH {}` answered 200 and returned the row unchanged
	// on schedules, dayparts and playlists, while answering 422 on the five
	// families that run the whole-schema gate. One rule, two behaviours, decided
	// by which validation path a family happens to use — and a client that
	// learned the behaviour on one resource got a success back describing a
	// resource it had not written on another.
	//
	// No data-model rule can express this either: an empty patch has no members
	// to fail, so the validators the member-half families defer to see a body
	// that is entirely in order and correctly change nothing.
	if schema.MinProps > 0 {
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err == nil && uint64(len(members)) < schema.MinProps {
			apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
				"VALIDATION_FAILED", "Unprocessable Content",
				fmt.Sprintf("this operation requires at least %d member(s); the body carries %d.", schema.MinProps, len(members)),
				nil)
			return true
		}
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
	// Both halves are clipped, because both embed client-supplied NAMES: the
	// pointer is built from nested keys, and kin-openapi's reason quotes the
	// offending property for an additionalProperties violation. A 100 KB property
	// name produced a 100 KB detail — retained and replayed for the idempotency
	// window, since a 422 is under 500 — so a client controlled both the
	// amplification and how long it was stored.
	if ptr := strings.Join(se.JSONPointer(), "."); ptr != "" {
		return clipKey(ptr) + ": " + clipKey(reason) + "."
	}
	reason = clipKey(reason)
	return strings.ToUpper(reason[:1]) + reason[1:] + "."
}

// withDeclaredMembers wraps a handler whose body this package does not otherwise
// see, bounding it to the members the named schema declares.
//
// It exists for the auth surface. Those handlers live in internal/app/auth, so
// they reach neither the resource pipeline nor srv.undeclaredMemberRejected — and
// the alternative, a second copy of this check over there, is how two checks
// drift into disagreeing about what the same document says. Wrapping at the mount
// keeps one implementation and puts the schema name next to the route it belongs
// to, where a reader comparing the two can see both at once.
//
// The body is read here and handed back to the wrapped handler intact: it decodes
// its own body, and a wrapper that consumed it would leave every one of those
// handlers reading an empty request.
func (srv *server) withDeclaredMembers(schema string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := readBody(w, r)
		if !ok {
			return
		}
		if srv.undeclaredMemberRejected(w, r, schema, raw) {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		next(w, r)
	}
}
