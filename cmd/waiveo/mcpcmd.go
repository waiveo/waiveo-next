package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/maaxton/waiveo-next/internal/app/apiop"
	"github.com/maaxton/waiveo-next/internal/app/mcp"
)

func cmdMCP(ctx context.Context, args []string, e env) (int, error) {
	if len(args) == 0 {
		return exitFailure, fmt.Errorf("usage: waiveo mcp tools [--json] | waiveo mcp serve [--api URL]")
	}
	switch args[0] {
	case "tools":
		return exitOK, cmdMCPTools(args[1:], e)
	case "serve":
		return exitOK, cmdMCPServe(ctx, args[1:], e)
	default:
		return exitFailure, fmt.Errorf("unknown mcp subcommand %q (tools, serve)", args[0])
	}
}

// cmdMCPTools lists the curated surface. It needs no running deployment and no
// credential, which is what makes the surface reviewable before any of that
// exists — and is why it was this CLI's first command.
func cmdMCPTools(args []string, e env) error {
	fs := flag.NewFlagSet("mcp tools", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "emit the surface as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	tools, err := mcp.Tools()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(e.out)
		enc.SetIndent("", "  ")
		return enc.Encode(tools)
	}

	// The mutating flag is shown per tool rather than by grouping, because an
	// operator scanning for "what can this change" reads down one column; grouping
	// would hide it behind a heading they might not scroll to.
	w := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TOOL\tKIND\tMETHOD\tPATH")
	for _, t := range tools {
		kind := "read"
		if t.Mutating {
			kind = "act"
			if t.RequiresIdempotencyKey {
				kind = "act (idempotency-key)"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Name, kind, t.Method, t.Path)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "\n%d curated operation(s).\n", len(tools))
	return nil
}

// cmdMCPServe serves the curated surface over MCP's stdio transport.
//
// It shares the engine with `waiveo call` rather than reimplementing it: an
// agent calling `createScopeNode` over MCP and an operator calling it from a
// shell build the same request, send the same Idempotency-Key discipline, and
// read the same result envelope. Two implementations would eventually be two
// answers to "what does this operation do".
//
// stdout carries protocol frames and nothing else; every diagnostic goes to
// stderr.
func cmdMCPServe(ctx context.Context, args []string, e env) error {
	fs := flag.NewFlagSet("mcp serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var conn connFlags
	conn.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := apiop.Load()
	if err != nil {
		return err
	}
	client, _, _, err := conn.client(s)
	if err != nil {
		return err
	}
	return mcp.Serve(ctx, mcp.ServeOptions{
		Surface:       s,
		Client:        client,
		In:            e.in,
		Out:           e.out,
		Log:           e.err,
		ServerVersion: buildVersion,
	})
}
