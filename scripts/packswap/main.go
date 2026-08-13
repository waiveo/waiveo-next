// Command packswap starts a pack under the real supervisor, hot-swaps it for a
// second version, and reports whether the old process actually went away.
//
// It is the check a pack author runs before shipping an update, and it exercises
// the whole start path rather than a simulation of it: it opens the deployment's
// own auth store, mints a real tier-grant (SEC-037), hands the code to the child
// over stdin, and treats the grant's REDEMPTION as the readiness signal. So the
// pack under test has to genuinely start, read its stdin, reach the running
// feeder and authenticate — exactly what the host will require of it.
//
// The failure it exists to catch is the one legacy hit repeatedly: an update
// that appears to apply while the previous code is still running. A surviving
// pid makes that visible in one line instead of as mysterious stale behaviour
// days later.
//
// Developer tooling, not a contract surface.
//
// Usage:
//
//	go run ./scripts/packswap -store .dev/app.db \
//	  -id waiveo/backups -scope <scope-node-ulid> \
//	  -from "node v1/index.js" -to "node v2/index.js"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/packhost"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

func main() {
	storePath := flag.String("store", "", "the deployment's auth store, so grants are minted the way the host mints them (required)")
	id := flag.String("id", "", "the pack's manifest id (required)")
	scope := flag.String("scope", "", "the scope node the pack's principal is bound at (required)")
	from := flag.String("from", "", "argv of the version to start first (required)")
	to := flag.String("to", "", "argv of the version to swap in (required)")
	fromVersion := flag.String("from-version", "1.0.0", "label for the first version")
	toVersion := flag.String("to-version", "2.0.0", "label for the replacement")
	ready := flag.Duration("ready", 20*time.Second, "how long a pack has to redeem its identity")
	flag.Parse()

	missing := map[string]string{"-store": *storePath, "-id": *id, "-scope": *scope, "-from": *from, "-to": *to}
	for name, v := range missing {
		if v == "" {
			fmt.Fprintf(os.Stderr, "packswap: %s is required\n", name)
			os.Exit(2)
		}
	}

	st, err := auth.Open(*storePath, func() int64 { return time.Now().UnixMilli() }, ulid.New)
	if err != nil {
		fmt.Fprintf(os.Stderr, "packswap: open auth store %s: %v\n", *storePath, err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	sup := packhost.New(st, packhost.Options{ReadyTimeout: *ready})
	defer sup.StopAll()
	ctx := context.Background()

	spec := func(version, argv string) packhost.Spec {
		return packhost.Spec{
			ID: *id, Version: version, Argv: strings.Fields(argv),
			ScopeNode: *scope, Role: auth.RoleOperator,
		}
	}

	first, err := sup.Start(ctx, spec(*fromVersion, *from))
	if err != nil {
		fmt.Fprintf(os.Stderr, "packswap: start: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("started  %s %s  pid=%d\n", first.ID, first.Version, first.PID)

	second, err := sup.Swap(ctx, spec(*toVersion, *to))
	if err != nil {
		// A failed swap is a SUCCESSFUL outcome for the incumbent: it is still
		// serving. Said explicitly, because "update failed" and "update failed
		// and your extension is down" are different sentences.
		fmt.Fprintf(os.Stderr, "packswap: swap failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "packswap: %s %s (pid %d) is still running\n", first.ID, first.Version, first.PID)
		os.Exit(1)
	}
	fmt.Printf("swapped  %s %s  pid=%d\n", second.ID, second.Version, second.PID)

	// The assertion the tool exists for. Reported as a failure rather than a
	// note: a surviving old process is the leak, not a curiosity.
	if stillRunning(first.PID) {
		fmt.Fprintf(os.Stderr, "packswap: LEAK — the old process (pid %d) is still alive after the swap\n", first.PID)
		os.Exit(1)
	}
	// Exactly one entry, at the new version. A swap that registered a SECOND
	// pack under the same id would leave both listed here, which is the other
	// half of the leak — the process one is visible above, this one is not.
	running := sup.Running()
	if len(running) != 1 || running[0].Version != *toVersion {
		fmt.Fprintf(os.Stderr, "packswap: supervisor lists %+v; want exactly one pack at %s\n", running, *toVersion)
		os.Exit(1)
	}
	fmt.Printf("old pid %d is gone; the host never restarted\n", first.PID)

	// Stopped explicitly rather than left to the deferred StopAll, so the tool
	// exercises the same teardown an operator disabling an extension takes, and
	// so a stop that hangs is this tool's failure rather than a process left
	// behind after it exits.
	if err := sup.Stop(*id); err != nil {
		fmt.Fprintf(os.Stderr, "packswap: stop: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("stopped  %s; pid %d gone too\n", *id, second.PID)
}

// stillRunning reports whether pid is still present. Signal 0 delivers nothing
// and reports only existence.
//
// It answers "yes" for a zombie as well as a live process, which is the right
// direction to err in: teardown reaps synchronously, so anything still present
// once Swap has returned is a real leak rather than a race with cleanup.
func stillRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
