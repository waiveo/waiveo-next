package main

import (
	"bytes"
	"testing"
)

// TestPrintVersionFormatsIdentity proves printVersion writes the exact
// one-line shape scripts/install-from-channel-index.sh's own header comment
// and this file's --version flag rely on: "waiveo-relay <version> (channel
// <channel>)\n". Rebuilding the binary with -ldflags to exercise the flag
// itself is overkill here (main_test.go already covers everything this
// binary does at runtime) -- printVersion is the one function that owns the
// wire shape, so it is what this test targets directly.
func TestPrintVersionFormatsIdentity(t *testing.T) {
	var buf bytes.Buffer
	printVersion(&buf, "1.4.2", "relay-v1")

	want := "waiveo-relay 1.4.2 (channel relay-v1)\n"
	if got := buf.String(); got != want {
		t.Errorf("printVersion(%q, %q) = %q, want %q", "1.4.2", "relay-v1", got, want)
	}
}

// TestPrintVersionDefaultIdentity proves the package-level defaults
// (buildVersion/buildChannel, unset by -ldflags) are both "dev" -- the
// identity an ordinary `go build`/`go run` (Wave-1 first-photon's dev/CI
// binary) reports, byte-identical to today's behavior.
func TestPrintVersionDefaultIdentity(t *testing.T) {
	if buildVersion != "dev" {
		t.Errorf("buildVersion default = %q, want %q", buildVersion, "dev")
	}
	if buildChannel != "dev" {
		t.Errorf("buildChannel default = %q, want %q", buildChannel, "dev")
	}

	var buf bytes.Buffer
	printVersion(&buf, buildVersion, buildChannel)
	want := "waiveo-relay dev (channel dev)\n"
	if got := buf.String(); got != want {
		t.Errorf("printVersion(defaults) = %q, want %q", got, want)
	}
}
