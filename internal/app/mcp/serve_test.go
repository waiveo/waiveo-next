package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/apiop"
)

// session drives the stdio transport the way a real client does: write lines in,
// read lines out.
type session struct {
	t       *testing.T
	surface *apiop.Surface
	client  *apiop.Client
}

func newSession(t *testing.T, handler http.HandlerFunc) *session {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s, err := apiop.Load()
	if err != nil {
		t.Fatal(err)
	}
	c, err := apiop.NewClient(s, apiop.Config{BaseURL: srv.URL, Token: "wv_std_test"})
	if err != nil {
		t.Fatal(err)
	}
	return &session{t: t, surface: s, client: c}
}

// exchange feeds every request line through one Serve run and returns the
// responses, decoded.
func (s *session) exchange(requests ...any) []map[string]any {
	s.t.Helper()
	var in bytes.Buffer
	for _, r := range requests {
		raw, err := json.Marshal(r)
		if err != nil {
			s.t.Fatal(err)
		}
		in.Write(raw)
		in.WriteByte('\n')
	}
	var out, logs bytes.Buffer
	if err := Serve(context.Background(), ServeOptions{
		Surface: s.surface, Client: s.client,
		In: &in, Out: &out, Log: &logs, ServerVersion: "test",
	}); err != nil {
		s.t.Fatalf("Serve: %v", err)
	}
	// Nothing but protocol frames may reach Out: a banner there desyncs a client.
	var decoded []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			s.t.Fatalf("a non-JSON line reached stdout: %q", line)
		}
		decoded = append(decoded, v)
	}
	if logs.Len() == 0 {
		s.t.Error("nothing was written to the log stream; the startup line is missing")
	}
	return decoded
}

func okJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Trace-Id", "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5")
		_, _ = w.Write([]byte(body))
	}
}

func TestInitializeNegotiatesTheProtocol(t *testing.T) {
	s := newSession(t, okJSON(`{}`))
	resp := s.exchange(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2024-11-05", "clientInfo": map[string]any{"name": "test"}},
	})
	if len(resp) != 1 {
		t.Fatalf("want 1 response, got %d: %v", len(resp), resp)
	}
	result := resp[0]["result"].(map[string]any)
	if got := result["protocolVersion"]; got != "2024-11-05" {
		// Answering with our own newest would tell an older client it is speaking a
		// revision it does not implement.
		t.Errorf("protocolVersion = %v, want the client's own 2024-11-05", got)
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("the server did not advertise the tools capability")
	}
}

func TestAnUnknownProtocolFallsBackToOurs(t *testing.T) {
	s := newSession(t, okJSON(`{}`))
	resp := s.exchange(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "1999-01-01"},
	})
	if got := resp[0]["result"].(map[string]any)["protocolVersion"]; got != ProtocolVersion {
		t.Errorf("protocolVersion = %v, want %q", got, ProtocolVersion)
	}
}

// TestANotificationIsNeverAnswered: `notifications/initialized` carries no id,
// and a response to it is a protocol violation some clients treat as fatal.
func TestANotificationIsNeverAnswered(t *testing.T) {
	s := newSession(t, okJSON(`{}`))
	resp := s.exchange(
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"},
	)
	if len(resp) != 1 {
		t.Fatalf("want exactly the ping's answer, got %d frames: %v", len(resp), resp)
	}
	if resp[0]["id"].(float64) != 2 {
		t.Errorf("the answered frame was not the ping: %v", resp[0])
	}
}

// TestToolsListIsTheCuratedSurfaceWithUsableSchemas: the list a model reads must
// be the curated set, and each entry must carry a schema the model can fill in
// without seeing api/openapi.yaml.
func TestToolsListIsTheCuratedSurfaceWithUsableSchemas(t *testing.T) {
	s := newSession(t, okJSON(`{}`))
	resp := s.exchange(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	tools := resp[0]["result"].(map[string]any)["tools"].([]any)

	curated, err := Tools()
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != len(curated) {
		t.Fatalf("tools/list returned %d tools, the curated surface has %d", len(tools), len(curated))
	}

	byName := map[string]map[string]any{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		byName[tool["name"].(string)] = tool
		if tool["description"].(string) == "" {
			t.Errorf("%v has no description", tool["name"])
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Errorf("%v has no object input schema", tool["name"])
		}
	}
	// An uncurated operation must not be reachable over MCP (API-070).
	if _, present := byName["login"]; present {
		t.Error("login is exposed as an MCP tool and carries neither curation tag")
	}

	// The read/act distinction must reach the client as metadata, not only as prose.
	read := byName["listScopeNodes"]["annotations"].(map[string]any)
	if read["readOnlyHint"] != true {
		t.Errorf("listScopeNodes is not marked read-only: %v", read)
	}
	act := byName["createScopeNode"]["annotations"].(map[string]any)
	if act["readOnlyHint"] != false {
		t.Errorf("createScopeNode is marked read-only: %v", act)
	}
	del := byName["deleteScopeNode"]["annotations"].(map[string]any)
	if del["destructiveHint"] != true {
		t.Errorf("deleteScopeNode is not marked destructive: %v", del)
	}
	// The path parameter must be in the schema, or a model cannot address a node.
	props := byName["getScopeNode"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["scope_node_id"]; !ok {
		t.Errorf("getScopeNode's schema does not ask for scope_node_id: %v", props)
	}
}

// TestToolsCallReachesTheDeploymentThroughTheSameEngine is the claim the whole
// design rests on: an MCP tool call and `waiveo call` produce the same request.
func TestToolsCallReachesTheDeploymentThroughTheSameEngine(t *testing.T) {
	var got *http.Request
	var body []byte
	s := newSession(t, func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		body = buf.Bytes()
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"3"`)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"}`))
	})
	resp := s.exchange(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "createScopeNode",
			"arguments": map[string]any{"body": map[string]any{"kind": "site", "name": "Hangar"}},
		},
	})
	result := resp[0]["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("the call reported an error: %v", result)
	}
	if got == nil {
		t.Fatal("the deployment was never reached")
	}
	if got.URL.Path != "/api/v1/scope-nodes" {
		t.Errorf("path = %q", got.URL.Path)
	}
	if got.Header.Get("Idempotency-Key") == "" {
		t.Error("an MCP act call carried no Idempotency-Key (API-072)")
	}
	if !strings.Contains(string(body), `"kind":"site"`) {
		t.Errorf("body = %q", body)
	}

	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	var env ResultEnvelope
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("the tool result is not the shared envelope: %v\n%s", err, text)
	}
	if env.Status != 201 || !env.OK || env.ETag != `"3"` || env.IdempotencyKey == "" {
		t.Errorf("envelope lost something: %+v", env)
	}
}

// TestToolsCallWithTypedArgumentsCoercesThem: an MCP client sends real JSON
// numbers where a command line sends strings, and both must reach the wire the
// same way.
func TestToolsCallWithTypedArgumentsCoercesThem(t *testing.T) {
	var got *http.Request
	s := newSession(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"cursor":null}`))
	})
	s.exchange(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "listScopeNodes", "arguments": map[string]any{"limit": 7}},
	})
	if got == nil {
		t.Fatal("the deployment was never reached")
	}
	if q := got.URL.Query().Get("limit"); q != "7" {
		t.Errorf("limit reached the wire as %q, want \"7\"", q)
	}
}

// TestABadToolCallComesBackAsAToolErrorNotATransportError: an argument mistake is
// the model's to correct, and a JSON-RPC error may never reach it in a form it
// can act on.
func TestABadToolCallComesBackAsAToolErrorNotATransportError(t *testing.T) {
	reached := false
	s := newSession(t, func(w http.ResponseWriter, r *http.Request) { reached = true })
	resp := s.exchange(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "listScopeNodes", "arguments": map[string]any{"limit": 9000}},
	})
	result, ok := resp[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("an argument mistake came back as a transport error: %v", resp[0])
	}
	if result["isError"] != true {
		t.Errorf("the failure was not flagged: %v", result)
	}
	if reached {
		t.Error("the out-of-range argument was sent to the deployment anyway")
	}
}

func TestAnUnknownToolIsRefused(t *testing.T) {
	s := newSession(t, okJSON(`{}`))
	resp := s.exchange(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "dropDatabase", "arguments": map[string]any{}},
	})
	if _, ok := resp[0]["error"]; !ok {
		t.Errorf("an unknown tool was accepted: %v", resp[0])
	}
}

// TestAnUncuratedOperationCannotBeCalledOverMCP is API-070 at the invocation
// point rather than at the listing point. A tool absent from tools/list but
// callable by name would be a curation rule that only applies to what a client
// is shown.
func TestAnUncuratedOperationCannotBeCalledOverMCP(t *testing.T) {
	reached := false
	s := newSession(t, func(w http.ResponseWriter, r *http.Request) { reached = true })
	resp := s.exchange(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "login", "arguments": map[string]any{}},
	})
	if _, ok := resp[0]["error"]; !ok {
		t.Errorf("login was callable over MCP: %v", resp[0])
	}
	if reached {
		t.Error("an uncurated operation reached the deployment")
	}
}

func TestAnUnsupportedMethodIsRefused(t *testing.T) {
	s := newSession(t, okJSON(`{}`))
	resp := s.exchange(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/list"})
	rpcErr, ok := resp[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("an unsupported method was answered: %v", resp[0])
	}
	if int(rpcErr["code"].(float64)) != codeMethodNotFound {
		t.Errorf("code = %v, want %d", rpcErr["code"], codeMethodNotFound)
	}
}

// TestASchemaViolationIsReportedNotSwallowed: the response check is what holds a
// running box to the document, and an agent that never saw the violation would
// go on parsing a body that does not match what it was promised.
func TestASchemaViolationIsReportedNotSwallowed(t *testing.T) {
	s := newSession(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// getSystemHealth declares SystemHealth; this is not it.
		_, _ = w.Write([]byte(`{"status":"marvellous"}`))
	})
	resp := s.exchange(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "getSystemHealth", "arguments": map[string]any{}},
	})
	text := resp[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	var env ResultEnvelope
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	if env.SchemaViolation == "" {
		t.Errorf("a body that does not match the declared schema was reported clean: %s", text)
	}
}
