package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPToolsListsTheSurface(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"mcp", "tools"}, &out); err != nil {
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
	var out bytes.Buffer
	if err := run([]string{"mcp", "tools", "--json"}, &out); err != nil {
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
	var out bytes.Buffer
	if err := run([]string{"delete-everything"}, &out); err == nil {
		t.Fatalf("an unknown command succeeded and printed:\n%s", out.String())
	}
	if out.Len() != 0 {
		t.Errorf("an unknown command produced output: %s", out.String())
	}
}

func TestNoArgumentsShowsUsage(t *testing.T) {
	var out bytes.Buffer
	if err := run(nil, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "mcp tools") {
		t.Errorf("usage does not mention the one command that exists:\n%s", out.String())
	}
}
