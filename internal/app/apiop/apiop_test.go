package apiop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── Derivation against the real document ────────────────────────────────────

func loadT(t *testing.T) *Surface {
	t.Helper()
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

// TestPathLevelParametersAreNotLost is the derivation bug that would be silent:
// every `{id}` in this document is declared on the PATH ITEM, not on the
// operation, so a collector reading only op.Parameters finds no path parameters
// at all — and every single-resource call would go to a URL with a literal brace
// in it.
func TestPathLevelParametersAreNotLost(t *testing.T) {
	s := loadT(t)
	op, ok := s.Lookup("getScopeNode")
	if !ok {
		t.Fatal("getScopeNode is not in the document")
	}
	p, ok := op.Param("scope_node_id")
	if !ok {
		t.Fatalf("scope_node_id was not derived; derived: %+v", op.Params)
	}
	if p.In != "path" || !p.Required {
		t.Errorf("scope_node_id derived as in=%q required=%v, want path/required", p.In, p.Required)
	}
	if p.Schema == nil || p.Schema.Value == nil || p.Schema.Value.Pattern == "" {
		t.Error("the ULID pattern did not survive the $ref resolution")
	}
}

// TestEveryDeclaredPathPlaceholderHasAParameter walks the WHOLE document: a
// template placeholder no parameter declares is an operation this engine can
// never call, and it would present as a 404 rather than as a defect.
func TestEveryDeclaredPathPlaceholderHasAParameter(t *testing.T) {
	s := loadT(t)
	for _, op := range s.Operations() {
		rest := op.Path
		for {
			i := strings.Index(rest, "{")
			if i < 0 {
				break
			}
			j := strings.Index(rest[i:], "}")
			if j < 0 {
				t.Errorf("%s: unterminated placeholder in %s", op.ID, op.Path)
				break
			}
			name := rest[i+1 : i+j]
			if p, ok := op.Param(name); !ok || p.In != "path" {
				t.Errorf("%s: path template names {%s} and no path parameter declares it", op.ID, name)
			}
			rest = rest[i+j:]
		}
	}
}

// TestNoToolSchemaCarriesAnUnresolvableRef: an MCP client cannot resolve
// `#/components/schemas/Ulid` against a document it has never seen, so a `$ref`
// surviving into a tool schema is a constraint no caller can read.
func TestNoToolSchemaCarriesAnUnresolvableRef(t *testing.T) {
	s := loadT(t)
	checked := 0
	for _, op := range s.Operations() {
		schema, err := s.InputSchema(op)
		if err != nil {
			t.Fatalf("%s: %v", op.ID, err)
		}
		raw, err := json.Marshal(schema)
		if err != nil {
			t.Fatalf("%s: %v", op.ID, err)
		}
		if strings.Contains(string(raw), `"$ref"`) {
			t.Errorf("%s: an unresolved $ref survived into the tool schema", op.ID)
		}
		if strings.Contains(string(raw), `"x-go-type"`) {
			t.Errorf("%s: a codegen extension leaked into the tool schema", op.ID)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no operations were checked: this would pass vacuously")
	}
}

// TestTheEngineManagedHeadersAreNotAskedOf TheCaller: Idempotency-Key and
// Trace-Id are supplied by this package, so advertising them as arguments would
// invite a caller to hand-roll the one thing the engine exists to get right.
// If-Match, by contrast, MUST be a caller's argument — an engine that filled it
// in would be an engine that silently clobbers.
func TestTheEngineManagedHeadersAreNotAskedOfTheCaller(t *testing.T) {
	s := loadT(t)
	create, _ := s.Lookup("createScopeNode")
	schema, err := s.InputSchema(create)
	if err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	for _, hidden := range []string{"Idempotency-Key", "Trace-Id"} {
		if _, present := props[hidden]; present {
			t.Errorf("createScopeNode's tool schema asks the caller for %s", hidden)
		}
	}

	update, _ := s.Lookup("updateScopeNode")
	schema, err = s.InputSchema(update)
	if err != nil {
		t.Fatal(err)
	}
	props = schema["properties"].(map[string]any)
	if _, present := props["If-Match"]; !present {
		t.Error("updateScopeNode's tool schema hides If-Match, which is the whole of optimistic concurrency")
	}
	required, _ := schema["required"].([]string)
	if !contains(required, "If-Match") {
		t.Errorf("If-Match is not required on updateScopeNode: %v", required)
	}
}

// ── The request engine, against a real listener ─────────────────────────────

type capture struct {
	srv *httptest.Server
	req *http.Request
	raw []byte
}

func newCapture(t *testing.T, status int, contentType, body string) *capture {
	t.Helper()
	c := &capture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		c.raw = buf[:n]
		c.req = r.Clone(r.Context())
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func clientFor(t *testing.T, s *Surface, base string) *Client {
	t.Helper()
	c, err := NewClient(s, Config{BaseURL: base, Token: "wv_std_test"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// TestEveryMutatingPostCarriesAMintedIdempotencyKey is API-072 checked at the
// place it actually matters — on the wire.
//
// mcp.Tools already reports that each of these ACCEPTS a key; that is a claim
// about the document. This drives every one of them through the engine and looks
// at the request it produced, which is a claim about the code. The two are
// different facts and the second is the one a retry depends on.
func TestEveryMutatingPostCarriesAMintedIdempotencyKey(t *testing.T) {
	s := loadT(t)
	c := newCapture(t, 200, "application/json", `{}`)
	client := clientFor(t, s, c.srv.URL)

	checked := 0
	seen := map[string]bool{}
	for _, op := range s.Operations() {
		if op.Method != http.MethodPost || !op.HasTag("mcp:act") {
			continue
		}
		args := Args{Params: map[string]any{}}
		for _, p := range op.Params {
			if !p.Required || engineHeaders[p.Name] {
				continue
			}
			args.Params[p.Name] = placeholderFor(p)
		}
		if op.Body != nil && op.Body.Required {
			if op.Body.JSON {
				args.Body = json.RawMessage(`{}`)
			} else {
				// A non-JSON body needs a file; the request is still built and sent,
				// which is all this test reads.
				args.BodyPath = writeTemp(t)
			}
		}
		if _, err := client.Do(context.Background(), op, args); err != nil {
			t.Fatalf("%s: %v", op.ID, err)
		}
		key := c.req.Header.Get("Idempotency-Key")
		if key == "" {
			t.Errorf("%s is a mutating POST and the engine sent no Idempotency-Key (API-072)", op.ID)
			continue
		}
		if seen[key] {
			t.Errorf("%s reused an Idempotency-Key another call already used — a key is per invocation", op.ID)
		}
		seen[key] = true
		checked++
	}
	if checked == 0 {
		t.Fatal("no mutating POSTs were exercised: this would pass vacuously")
	}
}

// TestAReadCarriesNoIdempotencyKey: API-072 is scoped to mutating POSTs, and a
// GET that sent one would be inventing a rule.
func TestAReadCarriesNoIdempotencyKey(t *testing.T) {
	s := loadT(t)
	c := newCapture(t, 200, "application/json", `{}`)
	client := clientFor(t, s, c.srv.URL)
	op, _ := s.Lookup("listScopeNodes")
	if _, err := client.Do(context.Background(), op, Args{}); err != nil {
		t.Fatal(err)
	}
	if got := c.req.Header.Get("Idempotency-Key"); got != "" {
		t.Errorf("a read sent Idempotency-Key %q", got)
	}
}

func TestTheCredentialTravelsAsABearerToken(t *testing.T) {
	s := loadT(t)
	c := newCapture(t, 200, "application/json", `{}`)
	client := clientFor(t, s, c.srv.URL)
	op, _ := s.Lookup("listScopeNodes")
	if _, err := client.Do(context.Background(), op, Args{}); err != nil {
		t.Fatal(err)
	}
	if got, want := c.req.Header.Get("Authorization"), "Bearer wv_std_test"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

// TestNoCredentialIsSentWhenNoneIsConfigured: the three credential-exchange
// operations declare `security: []`, so an empty token is a legitimate state and
// must not become an `Authorization: Bearer ` header.
func TestNoCredentialIsSentWhenNoneIsConfigured(t *testing.T) {
	s := loadT(t)
	c := newCapture(t, 200, "application/json", `{}`)
	client, err := NewClient(s, Config{BaseURL: c.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	op, _ := s.Lookup("listScopeNodes")
	if _, err := client.Do(context.Background(), op, Args{}); err != nil {
		t.Fatal(err)
	}
	if _, present := c.req.Header["Authorization"]; present {
		t.Error("an empty credential was sent as an Authorization header")
	}
}

// TestAnUndeclaredArgumentIsRefusedRatherThanDropped: a typo'd parameter name
// that travelled as a no-op is the worst shape of failure this engine can have —
// `--param sceen_id=…` would list every screen and report success.
func TestAnUndeclaredArgumentIsRefusedRatherThanDropped(t *testing.T) {
	s := loadT(t)
	c := newCapture(t, 200, "application/json", `{}`)
	client := clientFor(t, s, c.srv.URL)
	op, _ := s.Lookup("listScopeNodes")

	_, err := client.Do(context.Background(), op, Args{Params: map[string]any{"lmit": "7"}})
	if err == nil {
		t.Fatal("an undeclared parameter was accepted")
	}
	if !strings.Contains(err.Error(), "lmit") || !strings.Contains(err.Error(), "limit") {
		t.Errorf("the refusal names neither the mistake nor the alternatives: %v", err)
	}
	if c.req != nil {
		t.Error("the request was sent anyway")
	}

	// The engine-managed headers are refused as arguments too: a caller passing
	// their own Idempotency-Key would be hand-rolling the one thing this engine
	// exists to get right.
	if _, err := client.Do(context.Background(), op, Args{Params: map[string]any{"Idempotency-Key": "mine"}}); err == nil {
		t.Error("a caller-supplied Idempotency-Key was accepted")
	}
}

// TestAMissingRequiredParameterIsRefusedBeforeTheRequest: without this, the URL
// keeps its literal `{scope_node_id}` and the box answers 404 — a defect that
// reads like a missing resource.
func TestAMissingRequiredParameterIsRefusedBeforeTheRequest(t *testing.T) {
	s := loadT(t)
	c := newCapture(t, 200, "application/json", `{}`)
	client := clientFor(t, s, c.srv.URL)
	op, _ := s.Lookup("getScopeNode")
	_, err := client.Do(context.Background(), op, Args{})
	if err == nil {
		t.Fatal("a call missing its required path parameter was sent")
	}
	if !strings.Contains(err.Error(), "scope_node_id") {
		t.Errorf("the refusal does not name the missing parameter: %v", err)
	}
	if c.req != nil {
		t.Error("the request was sent anyway")
	}
}

// TestARefusalIsAnAnswer: a 4xx must come back as a Result carrying the Problem
// body, not as an error that discards it. The error code in that body is the
// most useful thing on the wire.
func TestARefusalIsAnAnswer(t *testing.T) {
	s := loadT(t)
	c := newCapture(t, 404, "application/problem+json", `{"code":"SCOPE_NODE_NOT_FOUND"}`)
	client := clientFor(t, s, c.srv.URL)
	op, _ := s.Lookup("getScopeNode")
	res, err := client.Do(context.Background(), op, Args{Params: map[string]any{"scope_node_id": "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"}})
	if err != nil {
		t.Fatalf("a 404 was raised as an error: %v", err)
	}
	if res.OK() || res.Status != 404 || !strings.Contains(string(res.Body), "SCOPE_NODE_NOT_FOUND") {
		t.Errorf("the refusal did not survive: %+v", res)
	}
}

// TestRedirectsAreNotFollowed: the Authorization header is set on the request,
// and a followed redirect re-sends it to wherever the redirect points.
func TestRedirectsAreNotFollowed(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the credential followed a redirect to another host")
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(elsewhere.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)

	s := loadT(t)
	client := clientFor(t, s, srv.URL)
	op, _ := s.Lookup("listScopeNodes")
	res, err := client.Do(context.Background(), op, Args{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want the 307 surfaced rather than followed", res.Status)
	}
}

// TestResponseValidationCatchesAWrongShape: the check exists so a running box
// can be held to the document, and a check that passed anything would be worse
// than none.
func TestResponseValidationCatchesAWrongShape(t *testing.T) {
	s := loadT(t)
	op, _ := s.Lookup("getSystemHealth")

	good := `{"status":"ok","checked_at_ms":1,"uptime_ms":1,"version":"dev","services":[],` +
		`"storage":{"path":"/","status":"ok","detail":"fine"},"relays":[],` +
		`"screens":{"total":0,"live":0,"fetching":0,"rejected":0,"stale":0,"never_seen":0,"paired":0,` +
		`"overridden":0,"live_window_ms":1,"content_transfer_window_ms":1,"fetching_max_unacked_pulls":1}}`
	if err := s.ValidateResponse(op, &Result{Status: 200, ContentType: "application/json", Body: []byte(good)}); err != nil {
		t.Fatalf("a conforming body was rejected: %v", err)
	}
	for name, body := range map[string]string{
		"missing a required member": strings.Replace(good, `"version":"dev",`, "", 1),
		"a value outside the enum":  strings.Replace(good, `"status":"ok"`, `"status":"fine"`, 1),
		"not JSON at all":           `<html>gateway timeout</html>`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := s.ValidateResponse(op, &Result{Status: 200, ContentType: "application/json", Body: []byte(body)}); err == nil {
				t.Fatal("accepted a body the document does not declare")
			}
		})
	}
}

// TestAnUndeclaredResponseSchemaIsNotAVerdict: several families are shape stubs
// whose response schema is a later minor. Inventing a pass or a fail for them
// would be this check lying in one direction or the other.
func TestAnUndeclaredResponseSchemaIsNotAVerdict(t *testing.T) {
	s := loadT(t)
	op, _ := s.Lookup("deleteScopeNode")
	if err := s.ValidateResponse(op, &Result{Status: 204}); err != nil {
		t.Errorf("a 204 with no declared schema was graded: %v", err)
	}
}

// ── Derivation shapes the real document does not contain ────────────────────
//
// These drive loadFrom with a purpose-built document. Without them the refusals
// below are written down and never executed, which is the shape this repo keeps
// producing: the mechanism built, the guard unproven.

const syntheticDoc = `
openapi: 3.1.0
info: {title: t, version: "1"}
servers: [{url: "/api/v1"}]
paths:
  /things/{thing_id}:
    parameters:
      - name: thing_id
        in: path
        required: true
        schema: {type: string}
    get:
      operationId: getThing
      responses: {"200": {description: ok}}
  /arrays:
    get:
      operationId: listArrays
      parameters:
        - name: ids
          in: query
          schema: {type: array, items: {type: string}}
      responses: {"200": {description: ok}}
  /collide:
    post:
      operationId: collide
      parameters:
        - name: body
          in: query
          schema: {type: string}
      requestBody:
        required: true
        content: {application/json: {schema: {type: object}}}
      responses: {"200": {description: ok}}
`

func TestAnArrayParameterIsRefusedRatherThanMisEncoded(t *testing.T) {
	s, err := loadFrom([]byte(syntheticDoc))
	if err != nil {
		t.Fatal(err)
	}
	c := newCapture(t, 200, "application/json", `{}`)
	client := clientFor(t, s, c.srv.URL)
	op, _ := s.Lookup("listArrays")
	_, err = client.Do(context.Background(), op, Args{Params: map[string]any{"ids": []any{"a", "b"}}})
	if err == nil {
		t.Fatal("an array-valued query parameter was encoded rather than refused")
	}
	if !strings.Contains(err.Error(), "array") {
		t.Errorf("the refusal does not name the reason: %v", err)
	}
}

// TestAParameterColliding WithTheBodyArgumentIsRefused: a parameter literally
// named `body` would shadow the request body, and the caller would never learn
// which of the two they filled in.
func TestAParameterCollidingWithTheBodyArgumentIsRefused(t *testing.T) {
	s, err := loadFrom([]byte(syntheticDoc))
	if err != nil {
		t.Fatal(err)
	}
	op, _ := s.Lookup("collide")
	if _, err := s.InputSchema(op); err == nil {
		t.Fatal("a parameter colliding with the body argument produced a schema anyway")
	}
}

func TestASyntheticPathParameterIsStillDerivedFromThePathItem(t *testing.T) {
	s, err := loadFrom([]byte(syntheticDoc))
	if err != nil {
		t.Fatal(err)
	}
	op, _ := s.Lookup("getThing")
	if p, ok := op.Param("thing_id"); !ok || p.In != "path" {
		t.Fatalf("thing_id not derived from the path item: %+v", op.Params)
	}
	if s.BasePath() != "/api/v1" {
		t.Errorf("base path = %q", s.BasePath())
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func placeholderFor(p Param) any {
	if p.Schema != nil && p.Schema.Value != nil {
		v := p.Schema.Value
		if v.Pattern == "^[0-9A-HJKMNP-TV-Z]{26}$" {
			return "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
		}
		if len(v.Enum) > 0 {
			return v.Enum[0]
		}
		if v.Type != nil && len(*v.Type) > 0 {
			switch (*v.Type)[0] {
			case "integer", "number":
				return "1"
			case "boolean":
				return "true"
			}
		}
	}
	return "x"
}

func writeTemp(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "body.bin")
	if err := os.WriteFile(path, []byte("PK\x03\x04not-really-a-zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
