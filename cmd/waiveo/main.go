// Command waiveo is the operator CLI and MCP server for a waiveo deployment.
//
// It exists so a person — or an agent — can drive and check a running box from a
// terminal, with no browser and without hand-writing a single curl.
//
//	waiveo ls                        what this deployment can be asked to do
//	waiveo describe <operationId>    one operation's arguments, in full
//	waiveo call <operationId> ...    invoke it
//	waiveo health                    is it reachable, answering, and answering the declared shape
//	waiveo mcp tools                 the curated MCP tool surface
//	waiveo mcp serve                 serve those tools over MCP's stdio transport
//	waiveo relay status              a relay's own state, read locally
//
// # One engine, two front ends
//
// Every operation above is built by internal/app/apiop from the SAME api/1
// document the server serves and the generated clients are produced from. There
// is no switch over operationIds anywhere in this binary: adding an operation to
// the document makes it listable, describable, callable and MCP-exposed with no
// change here. `waiveo call` and `waiveo mcp serve` are two front ends onto that
// one engine, which is what keeps a shell session and an agent session from
// being able to disagree about what the platform does.
//
// # What it will and will not reach
//
// The callable set is the CURATED set — the operations carrying `mcp:read` or
// `mcp:act` (API-070/071). That is deliberate rather than incidental: the
// uncurated remainder is credential exchange and second-factor enrolment, flows
// whose safety comes from a human being in front of them, and a CLI that could
// drive them is a CLI whose stolen token can rewrite an owner's credentials.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// buildVersion is this binary's channel-index/1 identity, matching
// cmd/waiveo-relay's convention: "dev" for an ordinary `go build`, overridden
// via -ldflags for a released build.
var buildVersion = "dev"

// Exit codes. `waiveo health` uses all three; every other command uses 0 and 1.
// They are distinct because a degraded box and an unreachable one send an
// operator to different places, and a script that cannot tell them apart treats
// both as an outage.
const (
	exitOK       = 0
	exitFailure  = 1
	exitDegraded = 2
)

type env struct {
	out io.Writer
	err io.Writer
	in  io.Reader
}

func main() {
	// SIGINT/SIGTERM cancels in-flight work: an interrupted `mcp serve` should
	// stop reading rather than be killed mid-frame.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e := env{out: os.Stdout, err: os.Stderr, in: os.Stdin}
	code, err := run(ctx, os.Args[1:], e)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waiveo:", err)
		if code == exitOK {
			code = exitFailure
		}
	}
	os.Exit(code)
}

// run is the whole CLI, taking its streams as arguments so the tests drive the
// real dispatch rather than a re-implementation of it.
func run(ctx context.Context, args []string, e env) (int, error) {
	if len(args) == 0 {
		usage(e.out)
		return exitOK, nil
	}
	switch args[0] {
	case "ls":
		return exitOK, cmdLs(args[1:], e)
	case "describe":
		return exitOK, cmdDescribe(args[1:], e)
	case "call":
		return cmdCall(ctx, args[1:], e)
	case "health":
		return cmdHealth(ctx, args[1:], e)
	case "mcp":
		return cmdMCP(ctx, args[1:], e)
	case "relay":
		return cmdRelay(ctx, args[1:], e)
	case "version", "--version", "-version":
		fmt.Fprintf(e.out, "waiveo %s\n", buildVersion)
		return exitOK, nil
	case "-h", "--help", "help":
		usage(e.out)
		return exitOK, nil
	default:
		return exitFailure, fmt.Errorf("unknown command %q (try: waiveo help)", args[0])
	}
}

func usage(out io.Writer) {
	fmt.Fprintf(out, `waiveo — operator CLI and MCP server for one deployment

Discover
  ls [family]                 resource families, or one family's operations
  describe <operationId>      an operation's parameters, body and responses

Drive
  call <operationId> [...]    invoke a curated operation
  health                      reachable + answering + answering the declared shape

Serve
  mcp tools [--json]          the curated MCP tool surface
  mcp serve                   serve those tools over MCP's stdio transport

Relay
  relay status                a relay's own operational state, read locally

Connection (flag beats environment beats config file)
  --api URL                   $%s              base_url
  --token-file PATH           $%s       token_file
  --ca-file PATH              $%s          ca_file
  --insecure-tls              $%s  insecure_tls
  --config PATH               $%s           (default %s)

The token is read from a FILE, never from a flag or an environment variable: an
argument is visible in `+"`ps`"+` and lands in shell history. The file must be mode
0600 or tighter.

Examples
  waiveo --api https://box:7420 ls
  waiveo call listScopeNodes --param limit=5 --json
  waiveo call createScopeNode --body '{"kind":"site","name":"Hangar"}'
  waiveo call updateScopeNode --param scope_node_id=01J... --param If-Match='"3"' --body @patch.json
`, envAPI, envTokenFil, envCAFile, envInsecure, envConfig, defaultConfigPathForHelp())
}
