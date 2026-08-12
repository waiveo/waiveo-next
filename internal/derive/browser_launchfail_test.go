package derive

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// A launch that dies has to say WHY, and this is the guard on that.
//
// It is not a hypothetical failure mode. internal/derive's real-render tests
// failed in CI twelve consecutive times — every run from the day the rasterizer
// landed — behind one sentence:
//
//	derive: chromium exited before announcing a DevTools endpoint
//
// That sentence is equally true of a missing shared library, a sandbox that
// cannot initialize, an unrecognised flag, and a process someone killed. The
// browser prints which one it is on stderr, and the launch path was reading
// that stream, scanning it for the DevTools banner, and discarding it. Twelve
// red runs produced no evidence, and the actual cause (Ubuntu 24.04 restricting
// the user namespaces Chromium's sandbox needs) had to be found by reasoning
// about the runner rather than by reading the failure.
//
// No real Chromium is involved here, deliberately: this must fail on a
// developer laptop with a perfectly good browser, because what it guards is the
// error message, not the renderer.
func TestLaunchFailureReportsChromiumStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub browser is a POSIX shell script")
	}

	// The real thing Ubuntu 24.04 prints when the sandbox cannot start — so a
	// reader of this test learns the signature it was written for.
	const marker = "Failed to move to new namespace: PID namespaces supported, Network namespace supported"

	stub := filepath.Join(t.TempDir(), "fake-chromium")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho '"+marker+"' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write the stub browser: %v", err)
	}

	b, err := NewBrowser(BrowserOptions{ExecPath: stub, LaunchTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("NewBrowser: %v", err)
	}
	page, err := BuildPage(Job{Key: "stub", W: 64, H: 64, Spec: &wire.DeriveSpec{
		Kind: wire.DeriveKindQR, Data: "x", ECLevel: "M", Color: "#000000",
		Fill: &wire.DeriveFill{Kind: wire.DeriveFillSolid, From: "#FFFFFF"},
	}})
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = b.Render(ctx, page)
	if err == nil {
		t.Fatal("Render reported success against a browser that exits immediately")
	}
	if !strings.Contains(err.Error(), marker) {
		t.Errorf("the launch error does not carry the browser's own explanation, which is the whole point of it\nwant substring: %s\ngot:            %v", marker, err)
	}
}
