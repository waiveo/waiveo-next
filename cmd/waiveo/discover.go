package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/maaxton/waiveo-next/internal/app/apiop"
	"github.com/maaxton/waiveo-next/internal/app/mcp"
)

// curatedSurface loads the document and returns the curated operations, indexed
// by operationId. Both discovery commands and `call` go through here, so all
// three agree on exactly one answer to "what is reachable".
func curatedSurface() (*apiop.Surface, []mcp.Tool, map[string]mcp.Tool, error) {
	s, err := apiop.Load()
	if err != nil {
		return nil, nil, nil, err
	}
	tools, err := mcp.ToolsFrom(s)
	if err != nil {
		return nil, nil, nil, err
	}
	byName := make(map[string]mcp.Tool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	return s, tools, byName, nil
}

// cmdLs renders the callable surface as a two-level tree: resource family, then
// operation.
//
// The tree is DERIVED from each operation's own non-curation tag and from the
// document's tag descriptions, not from a layout written here. A hand-drawn menu
// is a thing that goes stale silently — the operator reads a family that no
// longer exists and never sees the one that was added.
func cmdLs(args []string, e env) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "emit the tree as JSON")
	all := fs.Bool("all", false, "expand every family")
	want, err := parseWithOperand(fs, args)
	if err != nil {
		return err
	}

	s, tools, _, err := curatedSurface()
	if err != nil {
		return err
	}

	families := map[string][]mcp.Tool{}
	for _, t := range tools {
		fam := t.Op.Family()
		families[fam] = append(families[fam], t)
	}
	names := make([]string, 0, len(families))
	for name := range families {
		names = append(names, name)
	}
	sort.Strings(names)

	if want != "" {
		if _, ok := families[want]; !ok {
			return fmt.Errorf("no callable family %q (families: %s)", want, strings.Join(names, ", "))
		}
		names = []string{want}
		*all = true
	}

	if *asJSON {
		type opJSON struct {
			ID      string `json:"operation_id"`
			Kind    string `json:"kind"`
			Method  string `json:"method"`
			Path    string `json:"path"`
			Summary string `json:"summary"`
		}
		type famJSON struct {
			Family      string   `json:"family"`
			Description string   `json:"description"`
			Operations  []opJSON `json:"operations"`
		}
		out := make([]famJSON, 0, len(names))
		for _, name := range names {
			f := famJSON{Family: name, Description: s.FamilyDescription(name)}
			for _, t := range families[name] {
				f.Operations = append(f.Operations, opJSON{t.Name, kindOf(t), t.Method, t.Path, t.Op.Summary})
			}
			out = append(out, f)
		}
		enc := json.NewEncoder(e.out)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	for _, name := range names {
		ops := families[name]
		reads, acts := 0, 0
		for _, t := range ops {
			if t.Mutating {
				acts++
			} else {
				reads++
			}
		}
		fmt.Fprintf(e.out, "\n%s  (%d read, %d act)\n", name, reads, acts)
		if d := firstSentence(s.FamilyDescription(name)); d != "" {
			fmt.Fprintf(e.out, "  %s\n", d)
		}
		if !*all {
			continue
		}
		w := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
		for _, t := range ops {
			fmt.Fprintf(w, "    %s\t%s\t%s %s\t%s\n", t.Name, kindOf(t), t.Method, t.Path, firstSentence(t.Op.Summary))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	fmt.Fprintf(e.out, "\n%d callable operation(s) in %d families.", len(tools), len(families))
	if !*all {
		fmt.Fprint(e.out, " `waiveo ls <family>` or `waiveo ls --all` for the operations.")
	}
	fmt.Fprintln(e.out)
	return nil
}

// cmdDescribe prints everything a caller needs to construct one call: which
// arguments exist, which are required, what each accepts, and what a success
// looks like.
//
// It closes the loop `ls` opens. Discovery that stops at "this operation exists"
// still sends the operator to the spec for the arguments, which is precisely the
// thing this CLI is for not having to do.
func cmdDescribe(args []string, e env) error {
	fs := flag.NewFlagSet("describe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "emit the derived JSON Schema for the operation's arguments")
	name, err := parseWithOperand(fs, args)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("usage: waiveo describe <operationId> [--json]")
	}
	s, _, byName, err := curatedSurface()
	if err != nil {
		return err
	}
	tool, ok := byName[name]
	if !ok {
		return unknownOperation(name, byName)
	}

	if *asJSON {
		schema, err := s.InputSchema(tool.Op)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(e.out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"operation_id": tool.Name,
			"kind":         kindOf(tool),
			"method":       tool.Method,
			"path":         tool.Path,
			"family":       tool.Op.Family(),
			"summary":      tool.Op.Summary,
			"description":  tool.Description,
			"input_schema": schema,
		})
	}

	fmt.Fprintf(e.out, "%s  [%s]  %s %s\n", tool.Name, kindOf(tool), tool.Method, tool.Path)
	fmt.Fprintf(e.out, "family: %s\n", tool.Op.Family())
	if tool.Op.Summary != "" {
		fmt.Fprintf(e.out, "\n%s\n", tool.Op.Summary)
	}
	if tool.Description != "" && tool.Description != tool.Op.Summary {
		fmt.Fprintf(e.out, "\n%s\n", wrap(tool.Description, 78))
	}

	fmt.Fprintln(e.out, "\nParameters")
	printed := 0
	w := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	for _, p := range tool.Op.Params {
		if hiddenParam(p) {
			continue
		}
		req := "optional"
		if p.Required {
			req = "REQUIRED"
		}
		fmt.Fprintf(w, "  --param %s=\t%s\t%s\t%s\n", p.Name, p.In, req, describeSchema(p.Schema))
		printed++
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if printed == 0 {
		fmt.Fprintln(e.out, "  (none)")
	}
	// Named explicitly, because they are the two arguments a caller would
	// otherwise go looking for in the parameter list and not find.
	if _, ok := tool.Op.Param("Idempotency-Key"); ok {
		fmt.Fprintln(e.out, "  Idempotency-Key is minted per invocation by this CLI (API-072).")
	}
	fmt.Fprintln(e.out, "  Trace-Id is minted by the server and reported back in the result.")

	fmt.Fprintln(e.out, "\nRequest body")
	switch {
	case tool.Op.Body == nil:
		fmt.Fprintln(e.out, "  (none)")
	case !tool.Op.Body.JSON:
		fmt.Fprintf(e.out, "  --body @file   %s%s\n", tool.Op.Body.MediaType, requiredNote(tool.Op.Body.Required))
	default:
		fmt.Fprintf(e.out, "  --body <json>  %s%s\n", tool.Op.Body.MediaType, requiredNote(tool.Op.Body.Required))
		if tool.Op.Body.Schema != nil && tool.Op.Body.Schema.Value != nil {
			printBodyFields(e.out, tool.Op.Body.Schema.Value)
		}
	}

	fmt.Fprintln(e.out, "\nResponses")
	for _, code := range tool.Op.ResponseStatuses() {
		note := ""
		if tool.Op.ResponseSchema(code) != nil {
			note = "  (schema declared — `waiveo call` checks the body against it)"
		}
		fmt.Fprintf(e.out, "  %d%s\n", code, note)
	}
	fmt.Fprintf(e.out, "\nFull argument schema: waiveo describe %s --json\n", tool.Name)
	return nil
}

// printBodyFields lists a JSON body's top-level members. Only the top level:
// deeper nesting is what `--json` is for, and a wall of text is a thing an
// operator scrolls past rather than reads.
func printBodyFields(out io.Writer, schema *openapi3.Schema) {
	if len(schema.Properties) == 0 {
		return
	}
	required := map[string]bool{}
	for _, r := range schema.Required {
		required[r] = true
	}
	names := make([]string, 0, len(schema.Properties))
	for n := range schema.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, n := range names {
		req := ""
		if required[n] {
			req = "REQUIRED"
		}
		fmt.Fprintf(w, "    %s\t%s\t%s\n", n, req, describeSchema(schema.Properties[n]))
	}
	_ = w.Flush()
}

func hiddenParam(p apiop.Param) bool {
	return p.In == openapi3.ParameterInHeader && (p.Name == "Idempotency-Key" || p.Name == "Trace-Id")
}

func requiredNote(required bool) string {
	if required {
		return "  REQUIRED"
	}
	return "  optional"
}

// describeSchema renders a parameter or field's declaration in one line.
func describeSchema(ref *openapi3.SchemaRef) string {
	if ref == nil || ref.Value == nil {
		return ""
	}
	v := ref.Value
	var parts []string
	if v.Type != nil && len(*v.Type) > 0 {
		parts = append(parts, strings.Join(*v.Type, "|"))
	}
	if len(v.Enum) > 0 {
		vals := make([]string, 0, len(v.Enum))
		for _, item := range v.Enum {
			vals = append(vals, fmt.Sprint(item))
		}
		parts = append(parts, "one of "+strings.Join(vals, "/"))
	}
	if v.Min != nil || v.Max != nil {
		lo, hi := "", ""
		if v.Min != nil {
			lo = fmt.Sprint(*v.Min)
		}
		if v.Max != nil {
			hi = fmt.Sprint(*v.Max)
		}
		parts = append(parts, lo+".."+hi)
	}
	if v.Pattern != "" {
		parts = append(parts, "match "+v.Pattern)
	}
	if v.Default != nil {
		parts = append(parts, fmt.Sprintf("default %v", v.Default))
	}
	if d := firstSentence(v.Description); d != "" {
		parts = append(parts, d)
	}
	return strings.Join(parts, "; ")
}

func kindOf(t mcp.Tool) string {
	if t.Mutating {
		return "act"
	}
	return "read"
}

func unknownOperation(name string, byName map[string]mcp.Tool) error {
	var near []string
	lower := strings.ToLower(name)
	for candidate := range byName {
		if strings.Contains(strings.ToLower(candidate), lower) {
			near = append(near, candidate)
		}
	}
	sort.Strings(near)
	if len(near) > 0 {
		if len(near) > 8 {
			near = near[:8]
		}
		return fmt.Errorf("no callable operation %q — did you mean: %s", name, strings.Join(near, ", "))
	}
	return fmt.Errorf("no callable operation %q (%d are callable; run `waiveo ls --all`)", name, len(byName))
}

func firstSentence(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}

func wrap(s string, width int) string {
	var b strings.Builder
	line := 0
	for _, word := range strings.Fields(s) {
		if line > 0 && line+1+len(word) > width {
			b.WriteByte('\n')
			line = 0
		} else if line > 0 {
			b.WriteByte(' ')
			line++
		}
		b.WriteString(word)
		line += len(word)
	}
	return b.String()
}
