package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newEnv builds an env whose three streams are buffers the test can read.
func newEnv(stdin string) (env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return env{out: &out, err: &errb, in: strings.NewReader(stdin)}, &out, &errb
}

func TestMCPToolsListsTheSurface(t *testing.T) {
	e, out, _ := newEnv("")
	if _, err := run(context.Background(), []string{"mcp", "tools"}, e); err != nil {
		t.Fatalf("run: %v", err)
	}
	text := out.String()
	for _, want := range []string{"createScopeNode", "act (idempotency-key)", "curated operation(s)."} {
		if !strings.Contains(text, want) {
			t.Errorf("output does not mention %q:\n%s", want, text)
		}
	}
}

// TestMCPToolsJSONIsMachineReadable: the whole point of a derived surface is that
// something else can consume it. A table nobody can parse would make this a
// human-only report.
func TestMCPToolsJSONIsMachineReadable(t *testing.T) {
	e, out, _ := newEnv("")
	if _, err := run(context.Background(), []string{"mcp", "tools", "--json"}, e); err != nil {
		t.Fatalf("run: %v", err)
	}
	var tools []struct {
		Name                   string `json:"Name"`
		Mutating               bool   `json:"Mutating"`
		Method                 string `json:"Method"`
		RequiresIdempotencyKey bool   `json:"RequiresIdempotencyKey"`
	}
	if err := json.Unmarshal(out.Bytes(), &tools); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	if len(tools) == 0 {
		t.Fatal("no tools in the JSON surface")
	}
	for _, tool := range tools {
		if tool.Name == "" || tool.Method == "" {
			t.Errorf("incomplete tool entry: %+v", tool)
		}
	}
}

// TestAnUnknownCommandFailsRatherThanDoingSomething: a CLI that quietly does the
// default when it does not understand its argument is one an operator can invoke
// wrongly without noticing.
func TestAnUnknownCommandFailsRatherThanDoingSomething(t *testing.T) {
	e, out, _ := newEnv("")
	code, err := run(context.Background(), []string{"delete-everything"}, e)
	if err == nil {
		t.Fatalf("an unknown command succeeded and printed:\n%s", out.String())
	}
	if code == exitOK {
		t.Error("an unknown command exited 0")
	}
	if out.Len() != 0 {
		t.Errorf("an unknown command produced output: %s", out.String())
	}
}

func TestNoArgumentsShowsUsage(t *testing.T) {
	e, out, _ := newEnv("")
	if _, err := run(context.Background(), nil, e); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"mcp tools", "mcp serve", "call <operationId>", "health"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage does not mention %q:\n%s", want, out.String())
		}
	}
}

// TestLsBuildsTheTreeFromTheDocument: `ls` must show families the document
// declares, not families written here.
func TestLsBuildsTheTreeFromTheDocument(t *testing.T) {
	e, out, _ := newEnv("")
	if _, err := run(context.Background(), []string{"ls"}, e); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"scope-nodes", "automations", "diagnostics", "callable operation(s)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ls does not mention %q:\n%s", want, out.String())
		}
	}
	// A curation tag is not a resource family and must never head a branch.
	if strings.Contains(out.String(), "\nmcp:read") {
		t.Errorf("a curation tag was rendered as a resource family:\n%s", out.String())
	}
}

func TestLsOnOneFamilyExpandsIt(t *testing.T) {
	e, out, _ := newEnv("")
	if _, err := run(context.Background(), []string{"ls", "scope-nodes"}, e); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"listScopeNodes", "createScopeNode", "GET /scope-nodes"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ls scope-nodes does not mention %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "listAutomations") {
		t.Errorf("ls scope-nodes leaked another family:\n%s", out.String())
	}
}

// TestDescribeTellsACallerHowToCallIt is the discovery loop's closing half: an
// operator who has found an operation must learn its arguments here rather than
// in the spec.
func TestDescribeTellsACallerHowToCallIt(t *testing.T) {
	e, out, _ := newEnv("")
	if _, err := run(context.Background(), []string{"describe", "updateScopeNode"}, e); err != nil {
		t.Fatalf("run: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"--param scope_node_id=", // the path parameter
		"--param If-Match=",      // the concurrency precondition a caller MUST supply
		"REQUIRED",
		"--body <json>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("describe does not mention %q:\n%s", want, text)
		}
	}
	// A PATCH declares no Idempotency-Key — API-072 scopes it to POST — so the
	// note must NOT appear here, and must appear on the POST.
	if strings.Contains(text, "Idempotency-Key is minted") {
		t.Errorf("describe promised an Idempotency-Key on a PATCH, which declares none:\n%s", text)
	}

	e, out, _ = newEnv("")
	if _, err := run(context.Background(), []string{"describe", "createScopeNode"}, e); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "Idempotency-Key is minted per invocation") {
		t.Errorf("describe does not tell a caller the POST's replay key is handled for them:\n%s", out.String())
	}
}

func TestDescribeRefusesAnUncuratedOperation(t *testing.T) {
	// `login` is a real operationId the document declares and deliberately does
	// NOT curate (API-070). Naming a real-but-uncurated operation is what makes
	// this test about curation rather than about typos.
	e, _, _ := newEnv("")
	if _, err := run(context.Background(), []string{"describe", "login"}, e); err == nil {
		t.Fatal("describe accepted an uncurated operation")
	}
}

// fakeBox is a stand-in deployment that records what the CLI actually sent.
type fakeBox struct {
	server  *httptest.Server
	lastReq *http.Request
	lastRaw []byte
}

func newFakeBox(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *fakeBox {
	t.Helper()
	box := &fakeBox{}
	box.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		box.lastRaw = buf.Bytes()
		box.lastReq = r.Clone(r.Context())
		handler(w, r)
	}))
	t.Cleanup(box.server.Close)
	return box
}

// TestCallSendsWhatTheDocumentDeclares drives the real dispatch against a real
// HTTP listener: the path parameter is substituted, the query parameter travels,
// the credential is attached, and an act operation carries a minted
// Idempotency-Key (API-072) nobody had to remember to pass.
func TestCallSendsWhatTheDocumentDeclares(t *testing.T) {
	box := newFakeBox(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"7"`)
		w.Header().Set("Trace-Id", "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	tokenFile := writeToken(t, "wv_std_"+strings.Repeat("a", 64))

	e, out, _ := newEnv("")
	code, err := run(context.Background(), []string{
		"call", "--api", box.server.URL, "--token-file", tokenFile, "--json",
		"--body", `{"kind":"site","name":"Hangar"}`,
		"createScopeNode",
	}, e)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != exitOK {
		t.Fatalf("exit %d, want 0; output:\n%s", code, out.String())
	}
	if got, want := box.lastReq.URL.Path, "/api/v1/scope-nodes"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if got := box.lastReq.Header.Get("Idempotency-Key"); got == "" {
		t.Error("no Idempotency-Key on a mutating POST (API-072)")
	}
	if got, want := box.lastReq.Header.Get("Authorization"), "Bearer wv_std_"+strings.Repeat("a", 64); got != want {
		t.Errorf("Authorization = %q, want the token from the file", got)
	}
	if got, want := string(box.lastRaw), `{"kind":"site","name":"Hangar"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	var env struct {
		Status  int    `json:"status"`
		OK      bool   `json:"ok"`
		ETag    string `json:"etag"`
		TraceID string `json:"trace_id"`
		Key     string `json:"idempotency_key"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, out.String())
	}
	if env.Status != 201 || !env.OK || env.ETag != `"7"` || env.TraceID == "" || env.Key == "" {
		t.Errorf("envelope lost something: %+v", env)
	}
}

func TestCallSubstitutesPathAndQueryParameters(t *testing.T) {
	box := newFakeBox(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"cursor":null}`))
	})
	// Flags AFTER the operationId, which is the word order anyone actually types.
	// Go's flag package stops at the first non-flag word, so without the operand
	// re-parse this `--param` would be dropped and a list of everything would come
	// back looking like the filtered answer.
	e, _, _ := newEnv("")
	if _, err := run(context.Background(), []string{
		"call", "--api", box.server.URL, "listScopeNodes", "--param", "limit=7",
	}, e); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := box.lastReq.URL.Path, "/api/v1/scope-nodes"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if got, want := box.lastReq.URL.Query().Get("limit"), "7"; got != want {
		t.Errorf("limit = %q, want %q", got, want)
	}

	e, _, _ = newEnv("")
	if _, err := run(context.Background(), []string{
		"call", "--api", box.server.URL, "--param", "scope_node_id=01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", "getScopeNode",
	}, e); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := box.lastReq.URL.Path, "/api/v1/scope-nodes/01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// TestCallRefusesABadArgumentBeforeSendingIt: a typo'd parameter name that
// travelled as a no-op would list every scope node and report success.
func TestCallRefusesABadArgumentBeforeSendingIt(t *testing.T) {
	reached := false
	box := newFakeBox(t, func(w http.ResponseWriter, r *http.Request) { reached = true })
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"misspelled parameter", []string{"--param", "lmit=7", "listScopeNodes"}},
		{"out of the declared range", []string{"--param", "limit=5000", "listScopeNodes"}},
		{"not the declared type", []string{"--param", "limit=lots", "listScopeNodes"}},
		{"missing a required path parameter", []string{"getScopeNode"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Reset per case: a flag shared across subtests makes the FIRST failure
			// fail every one after it, which is a report that points at the wrong
			// case.
			reached = false
			e, _, _ := newEnv("")
			args := append([]string{"call", "--api", box.server.URL}, tc.args...)
			if _, err := run(context.Background(), args, e); err == nil {
				t.Fatal("accepted an argument the document does not admit")
			}
			if reached {
				t.Fatal("the request was sent anyway")
			}
		})
	}
}

func TestCallReportsARefusalWithoutHidingIt(t *testing.T) {
	box := newFakeBox(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"SCOPE_NODE_NOT_FOUND","title":"no such node"}`))
	})
	e, out, _ := newEnv("")
	code, err := run(context.Background(), []string{
		"call", "--api", box.server.URL, "--json",
		"--param", "scope_node_id=01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", "getScopeNode",
	}, e)
	if err != nil {
		t.Fatalf("a 404 was raised as an error rather than reported: %v", err)
	}
	if code != exitFailure {
		t.Errorf("exit %d on a 404, want %d", code, exitFailure)
	}
	if !strings.Contains(out.String(), "SCOPE_NODE_NOT_FOUND") {
		t.Errorf("the error code the server sent did not reach the operator:\n%s", out.String())
	}
}

func writeToken(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestATokenFileAnyoneCanReadIsRefused: readability is how a bearer credential
// leaks, and silently tightening the mode would hide an exposure that has
// already happened.
func TestATokenFileAnyoneCanReadIsRefused(t *testing.T) {
	path := writeToken(t, "wv_std_secret")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readTokenFile(path); err == nil {
		t.Fatal("a world-readable token file was accepted")
	} else if strings.Contains(err.Error(), "wv_std_secret") {
		t.Fatalf("the refusal rendered the credential: %v", err)
	}
}

// TestConfigPrecedenceIsFlagThenEnvThenFile pins the order the whole CLI's
// addressing rests on. Getting it backwards would send a command meant for a lab
// box to whatever the config file last named.
func TestConfigPrecedenceIsFlagThenEnvThenFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"base_url":"https://from-file:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envConfig, cfgPath)

	t.Run("file only", func(t *testing.T) {
		t.Setenv(envAPI, "")
		c := &connFlags{}
		got, _, err := c.resolve()
		if err != nil {
			t.Fatal(err)
		}
		if got.BaseURL != "https://from-file:1" {
			t.Errorf("base URL = %q, want the config file's", got.BaseURL)
		}
	})
	t.Run("env beats file", func(t *testing.T) {
		t.Setenv(envAPI, "https://from-env:2")
		c := &connFlags{}
		got, _, err := c.resolve()
		if err != nil {
			t.Fatal(err)
		}
		if got.BaseURL != "https://from-env:2" {
			t.Errorf("base URL = %q, want the environment's", got.BaseURL)
		}
	})
	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv(envAPI, "https://from-env:2")
		c := &connFlags{api: "https://from-flag:3"}
		got, _, err := c.resolve()
		if err != nil {
			t.Fatal(err)
		}
		if got.BaseURL != "https://from-flag:3" {
			t.Errorf("base URL = %q, want the flag's", got.BaseURL)
		}
	})
}

// TestNoAddressIsARefusalNotADefault: a CLI that silently addressed localhost
// would, run ON the appliance, drive production because someone forgot a flag.
func TestNoAddressIsARefusalNotADefault(t *testing.T) {
	t.Setenv(envConfig, filepath.Join(t.TempDir(), "absent.json"))
	t.Setenv(envAPI, "")
	c := &connFlags{}
	if _, _, err := c.resolve(); err == nil {
		t.Fatal("resolved a base URL from nowhere")
	}
}

// TestASecondPositionalIsStillRefused: parsing flags on both sides of the
// operand must not turn a mistyped command into a silently-accepted one.
func TestASecondPositionalIsStillRefused(t *testing.T) {
	e, _, _ := newEnv("")
	if _, err := run(context.Background(), []string{"describe", "listScopeNodes", "getScopeNode"}, e); err == nil {
		t.Fatal("two operands were accepted")
	}
}

func TestAMistypedConfigMemberIsRefused(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"base_ur1":"https://typo:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envConfig, cfgPath)
	t.Setenv(envAPI, "")
	c := &connFlags{}
	if _, _, err := c.resolve(); err == nil {
		t.Fatal("a mistyped config member was ignored rather than refused")
	}
}
