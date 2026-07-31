// Command waiveo is the operator CLI.
//
// It starts with the one thing that needs no running deployment and no
// credential: printing the curated MCP tool surface. That surface is DERIVED from
// the OpenAPI document's own `mcp:read`/`mcp:act` tags (API-071 makes those tags
// "the sole input to MCP tool generation"), so this command reports what the
// document currently says rather than a list anybody maintains.
//
// It is deliberately the first subcommand: the credential flows this CLI
// eventually performs all talk to a running app, and a command that can be run
// and checked with nothing installed is what makes the surface reviewable before
// any of that exists.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/maaxton/waiveo-next/internal/app/mcp"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "waiveo:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		usage(out)
		return nil
	}
	switch args[0] {
	case "mcp":
		return runMCP(args[1:], out)
	case "-h", "--help", "help":
		usage(out)
		return nil
	default:
		return fmt.Errorf("unknown command %q (try: waiveo help)", args[0])
	}
}

func usage(out io.Writer) {
	fmt.Fprint(out, `waiveo — operator CLI

Commands:
  mcp tools    List the curated MCP tool surface
`)
}

func runMCP(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "tools" {
		return fmt.Errorf("usage: waiveo mcp tools [--json]")
	}
	fs := flag.NewFlagSet("mcp tools", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "emit the surface as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	tools, err := mcp.Tools()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(tools)
	}

	// The mutating flag is shown per tool rather than by grouping, because an
	// operator scanning for "what can this change" reads down one column; grouping
	// would hide it behind a heading they might not scroll to.
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
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
	fmt.Fprintf(out, "\n%d curated operation(s).\n", len(tools))
	return nil
}
