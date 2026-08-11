package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// main_test.go covers the parts of this command that are NOT a browser.
//
// The package had no test at all, which is worse here than it looks: every
// guard this file carries is a refusal that happens BEFORE Chromium is
// launched — a missing flag, a spec with a misspelt member, a spec the wire
// refuses, an absent -api — and each of them is the difference between a clear
// message and either a browser started for nothing or a nil-safe path taken on
// a value nobody checked. None of them needs a browser to drive, so none of
// them had an excuse for being undriven.
//
// What is deliberately NOT covered: anything past bf.runner(), which constructs
// a real Browser and fails on a machine with no Chromium. Those paths belong to
// internal/derive, where a Renderer is an interface and the whole loop is
// exercised without one.

// tokenFile writes a bearer token where -token-file can read it, so no test
// touches the developer's real dev-key file.
func tokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("test-token\n"), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
	return path
}

// TestRenderRefusesAnIncompleteInvocation: -spec, -out, -w and -h are all
// required, and the refusal must name them rather than failing later with a
// zero-sized page or an empty output path.
func TestRenderRefusesAnIncompleteInvocation(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(spec, []byte(`{"kind":"qr","data":"x"}`), 0o600); err != nil {
		t.Fatalf("write the spec: %v", err)
	}
	cases := [][]string{
		{"-out", "/dev/null", "-w", "100", "-h", "100"},
		{"-spec", spec, "-w", "100", "-h", "100"},
		{"-spec", spec, "-out", "/dev/null", "-h", "100"},
		{"-spec", spec, "-out", "/dev/null", "-w", "100"},
		{"-spec", spec, "-out", "/dev/null", "-w", "0", "-h", "100"},
	}
	for _, args := range cases {
		err := cmdRender(context.Background(), args)
		if err == nil {
			t.Errorf("cmdRender%v returned no error", args)
			continue
		}
		if !strings.Contains(err.Error(), "-spec") {
			t.Errorf("cmdRender%v said %q, want a message naming the required flags", args, err)
		}
	}
}

// TestRenderRefusesAMisspeltSpecMember is the reason the decoder disallows
// unknown fields. A spec carrying `shaddow` would otherwise render happily with
// no shadow, and the author would be left comparing the picture to the file
// wondering which of them was wrong.
func TestRenderRefusesAMisspeltSpecMember(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(spec, []byte(`{"kind":"rect","fill":{"kind":"solid","from":"#FFFFFF"},"shaddow":{"dy":4}}`), 0o600); err != nil {
		t.Fatalf("write the spec: %v", err)
	}
	err := cmdRender(context.Background(), []string{"-spec", spec, "-out", filepath.Join(dir, "o.png"), "-w", "100", "-h", "100"})
	if err == nil {
		t.Fatal("a spec with a misspelt member was accepted")
	}
	if !strings.Contains(err.Error(), "decode -spec") {
		t.Errorf("error = %q, want it to name the decode of -spec", err)
	}
}

// TestRenderRefusesAnInvalidSpecBeforeLaunchingABrowser: the spec passes the
// SAME wire gate the server applies, and it passes it before bf.runner() starts
// Chromium. A tool that launched a browser and then discovered the spec was
// nonsense would burn a browser launch per typo — and would report the failure
// as a render failure rather than as the authoring error it is.
func TestRenderRefusesAnInvalidSpecBeforeLaunchingABrowser(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.json")
	// A `qr` spec carrying a text-only member: refused by wire.ValidateDeriveSpec
	// because the page builder writes font-size into the text rule only, so it is
	// a control an operator sets and nothing reads.
	if err := os.WriteFile(spec, []byte(`{"kind":"qr","data":"https://waiveo.local/x","font_px":64}`), 0o600); err != nil {
		t.Fatalf("write the spec: %v", err)
	}
	err := cmdRender(context.Background(), []string{"-spec", spec, "-out", filepath.Join(dir, "o.png"), "-w", "100", "-h", "100"})
	if err == nil {
		t.Fatal("an invalid spec was accepted")
	}
	if !strings.Contains(err.Error(), "font_px") {
		t.Errorf("error = %q, want the wire's own reason", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "o.png")); statErr == nil {
		t.Error("an output file was written for a spec that never rendered")
	}
}

// TestASubcommandThatTalksToAFeederRequiresAnApi. Without it the tool would
// build a client against an empty base URL and fail somewhere inside net/http
// with a message about a missing protocol scheme.
func TestASubcommandThatTalksToAFeederRequiresAnApi(t *testing.T) {
	for name, run := range map[string]func(context.Context, []string) error{
		"pending": cmdPending,
		"sync":    cmdSync,
	} {
		err := run(context.Background(), []string{"-token-file", tokenFile(t)})
		if err == nil {
			t.Errorf("%s with no -api returned no error", name)
			continue
		}
		if !strings.Contains(err.Error(), "-api is required") {
			t.Errorf("%s with no -api said %q, want it to name the flag", name, err)
		}
	}
}

// TestPendingReadsTheQueueAndPresentsTheBearerAsAHeader drives the one
// subcommand that reaches the network without needing a browser, against a
// stand-in feeder.
//
// The credential assertion is the point of doing it over a real server: the
// token is read from a FILE and must arrive as an Authorization header. It is
// never a flag, because an argument is visible in `ps` and lands in shell
// history, and this one is a bearer for the whole authoring surface.
func TestPendingReadsTheQueueAndPresentsTheBearerAsAHeader(t *testing.T) {
	var reqs atomic.Int64
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		gotAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/api/v1/derive/pending" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"derive_jobs": []map[string]any{{
			"source": "playlist", "resource_id": "01J8LIST0000000000000000BB", "resource_name": "Foyer loop",
			"item_index": 1, "layer_index": 1, "state": "pending",
			"spec_digest": "abc", "w": 360, "h": 360,
			"spec": map[string]any{"kind": "qr", "data": "https://waiveo.local/pair/INLINE-1"},
		}}})
	}))
	t.Cleanup(srv.Close)

	if err := cmdPending(context.Background(), []string{"-api", srv.URL, "-token-file", tokenFile(t)}); err != nil {
		t.Fatalf("cmdPending: %v", err)
	}
	if reqs.Load() != 1 {
		t.Errorf("the feeder saw %d request(s), want 1", reqs.Load())
	}
	if got, _ := gotAuth.Load().(string); got != "Bearer test-token" {
		t.Errorf("Authorization = %q, want the token file's bearer with the trailing newline trimmed", got)
	}
}

// TestPendingReportsAFeederThatRefuses: a queue read that failed must be an
// error the process exits non-zero on, never "nothing outstanding". A run that
// could not ask must not look like a run with nothing to do — that is the whole
// reason this tool is scriptable.
func TestPendingReportsAFeederThatRefuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"code":"FORBIDDEN"}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	if err := cmdPending(context.Background(), []string{"-api", srv.URL, "-token-file", tokenFile(t)}); err == nil {
		t.Fatal("a 403 from the feeder was reported as success")
	}
}

// TestAnUnreadableTokenFileIsNamed: the credential is the commonest thing to get
// wrong, and "no such file" against the path the operator passed is the message
// that fixes it.
func TestAnUnreadableTokenFileIsNamed(t *testing.T) {
	err := cmdPending(context.Background(), []string{"-api", "https://box:7420", "-token-file", filepath.Join(t.TempDir(), "absent")})
	if err == nil {
		t.Fatal("an absent -token-file was accepted")
	}
	if !strings.Contains(err.Error(), "read -token-file") {
		t.Errorf("error = %q, want it to name -token-file", err)
	}
}
