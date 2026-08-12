// Command ctxprobe drives the HOST side of a ctx/1 handshake against a real
// pack process and reports what was negotiated.
//
// This is the tool for answering "does this pack speak the protocol?" without a
// running feeder. It launches the pack over ctx/1's `local` binding in its
// stdio form (CTX-011 — `CTX_TRANSPORT=stdio`, framing identical to the socket
// form), reads its `control.hello`, negotiates (CTX-021–023), and writes the
// `control.hello-ack`. A pack that never sends hello, sends something else
// first, or declares a range this host cannot satisfy fails here with the
// contract's own error code rather than with a hang nobody can attribute.
//
// It is developer tooling, not a contract surface: the host that supervises
// packs in production is a different thing, and this deliberately does not
// supervise, restart, or dispatch verbs. It completes one handshake and exits.
//
// Usage:
//
//	go run ./scripts/ctxprobe -pack "node ./my-pack/index.js"
//	go run ./scripts/ctxprobe -pack ./pack-binary -versions 1.0,1.1
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/ctxproto"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

func main() {
	pack := flag.String("pack", "", "the pack process to launch (a shell-less argv, split on spaces) (required)")
	versions := flag.String("versions", "1.0", "comma-separated ctx/1 major.minor versions this probe implements")
	flags := flag.String("features", "", "comma-separated feature flags this probe supports")
	timeout := flag.Duration("timeout", 5*time.Second, "handshake budget (CTX-013's local-binding window)")
	flag.Parse()

	if *pack == "" {
		fmt.Fprintln(os.Stderr, "ctxprobe: -pack is required")
		os.Exit(2)
	}
	implemented, err := parseVersions(*versions)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ctxprobe: %v\n", err)
		os.Exit(2)
	}

	if err := probe(*pack, implemented, splitList(*flags), *timeout); err != nil {
		// A typed protocol refusal prints its CODE first: that string is what a
		// pack author greps for in the contract, and burying it inside prose is
		// how a precise failure becomes a vague one.
		var fe *ctxproto.FrameError
		if errors.As(err, &fe) {
			fmt.Fprintf(os.Stderr, "ctxprobe: %s: %s\n", fe.Code, fe.Message)
		} else {
			fmt.Fprintf(os.Stderr, "ctxprobe: %v\n", err)
		}
		os.Exit(1)
	}
}

func probe(packCmd string, implemented []ctxproto.HostVersion, supported []string, budget time.Duration) error {
	argv := strings.Fields(packCmd)
	cmd := exec.Command(argv[0], argv[1:]...)
	// CTX-011's stdio form of the `local` binding. The pack's stderr is passed
	// through untouched so its own diagnostics reach the operator running this —
	// a pack that fails to start usually says why there, and swallowing it would
	// leave only "no hello arrived".
	cmd.Env = append(os.Environ(), "CTX_TRANSPORT=stdio")
	cmd.Stderr = os.Stderr

	toPack, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open pack stdin: %w", err)
	}
	fromPack, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open pack stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start pack %q: %w", packCmd, err)
	}
	// Killed on every exit path. A probe that left a pack process running after
	// a failed handshake would leak one process per invocation, and the usual
	// caller of this is a loop over a directory of packs.
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	type result struct {
		hello ctxproto.Hello
		err   error
	}
	done := make(chan result, 1)
	go func() {
		h, err := ctxproto.ReadHello(fromPack)
		done <- result{h, err}
	}()

	var hello ctxproto.Hello
	select {
	case r := <-done:
		if r.err != nil {
			return r.err
		}
		hello = r.hello
	case <-time.After(budget):
		// CTX-013: a handshake that does not complete inside the window is a
		// failed pack start, not something to wait out.
		return fmt.Errorf("pack sent no control.hello within %s (CTX-013)", budget)
	}

	negotiated, granted, err := ctxproto.Negotiate(hello.CtxRange, implemented, hello.FeatureFlags, setOf(supported))
	if err != nil {
		// CTX-022: the pack is told WHY in its own taxonomy before the
		// connection goes away, so a pack author sees INCOMPATIBLE_RANGE rather
		// than an unexplained close.
		var fe *ctxproto.FrameError
		if errors.As(err, &fe) {
			_ = ctxproto.WriteMessage(toPack, ctxproto.Message{
				Type: ctxproto.TypeError, ID: ulid.New(), TraceID: ulid.New(),
				Code: fe.Code, Text: fe.Message,
			})
		}
		return err
	}

	ack := ctxproto.HelloAck{NegotiatedVersion: negotiated.String(), FeatureFlags: granted}
	if err := ctxproto.WriteMessage(toPack, ctxproto.Message{
		Type: ctxproto.TypeResponse, ID: ulid.New(), TraceID: ulid.New(),
		Verb: ctxproto.VerbHelloAck, Body: ack.Body(),
	}); err != nil {
		return fmt.Errorf("write hello-ack: %w", err)
	}

	fmt.Printf("handshake ok: %s@%s speaks ctx/%s (range %q); granted flags: %s\n",
		hello.ManifestID, hello.ManifestVersion, negotiated, hello.CtxRange, describe(granted))
	return nil
}

// parseVersions reads the `major.minor` list this probe claims to implement.
func parseVersions(s string) ([]ctxproto.HostVersion, error) {
	parts := splitList(s)
	if len(parts) == 0 {
		return nil, fmt.Errorf("-versions is empty; a host that implements nothing can negotiate nothing")
	}
	out := make([]ctxproto.HostVersion, 0, len(parts))
	for _, p := range parts {
		major, minor, ok := strings.Cut(p, ".")
		if !ok {
			return nil, fmt.Errorf("version %q is not major.minor", p)
		}
		maj, err := strconv.Atoi(major)
		if err != nil {
			return nil, fmt.Errorf("version %q: major is not a number", p)
		}
		min, err := strconv.Atoi(minor)
		if err != nil {
			return nil, fmt.Errorf("version %q: minor is not a number", p)
		}
		out = append(out, ctxproto.HostVersion{Major: maj, Minor: min})
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func setOf(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, s := range list {
		m[s] = true
	}
	return m
}

// describe renders a granted-flag list for the success line. "(none)" rather
// than an empty string, so the line never reads as though the field was dropped.
func describe(flags []string) string {
	if len(flags) == 0 {
		return "(none)"
	}
	return strings.Join(flags, ",")
}
