package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/maaxton/waiveo-next/internal/app/mcp"
)

// cmdCall invokes one curated operation.
//
// There is no per-operation code here and there is no switch: the operationId is
// looked up in the derived surface, its arguments are checked against the
// document's own parameter declarations, and the request is built by the engine.
// That is what makes `waiveo call` reach an operation added to the document this
// morning.
func cmdCall(ctx context.Context, args []string, e env) (int, error) {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var conn connFlags
	conn.bind(fs)
	params := paramList{}
	fs.Var(params, "param", "an argument, as name=value; repeat for more (see `waiveo describe <op>`)")
	body := fs.String("body", "", "request body: literal JSON, @file, or - for stdin")
	asJSON := fs.Bool("json", false, "emit the whole result — status, trace id, body — as one JSON object")
	name, err := parseWithOperand(fs, args)
	if err != nil {
		return exitFailure, err
	}
	if name == "" {
		return exitFailure, fmt.Errorf("usage: waiveo call <operationId> [--param k=v ...] [--body @file|-|<json>] [--json]")
	}

	s, _, byName, err := curatedSurface()
	if err != nil {
		return exitFailure, err
	}
	tool, ok := byName[name]
	if !ok {
		return exitFailure, unknownOperation(name, byName)
	}

	callArgs, err := readBodyArg(tool.Op, *body, e.in)
	if err != nil {
		return exitFailure, err
	}
	for k, v := range params {
		callArgs.Params[k] = v
	}

	client, cfg, _, err := conn.client(s)
	if err != nil {
		return exitFailure, err
	}
	res, err := client.Do(ctx, tool.Op, callArgs)
	if err != nil {
		return exitFailure, err
	}
	envelope := mcp.Envelope(s, tool.Op, res)

	if *asJSON {
		enc := json.NewEncoder(e.out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(envelope); err != nil {
			return exitFailure, err
		}
	} else {
		printHuman(e, envelope, res.ContentType)
	}
	if !res.OK() {
		// The refusal was PRINTED, not raised: a Problem body naming the error code
		// is the useful part. The exit status is what a script reads.
		if res.Status == 401 && cfg.Token == "" {
			fmt.Fprintf(e.err, "waiveo: no credential was sent — install an api key at %s (mode 0600) or pass --token-file\n", defaultTokenPath())
		}
		return exitFailure, nil
	}
	return exitOK, nil
}

func printHuman(e env, envelope mcp.ResultEnvelope, contentType string) {
	fmt.Fprintf(e.err, "%s %d  %s  %s\n", envelope.Method, envelope.Status, envelope.Operation, envelope.URL)
	if envelope.ETag != "" {
		fmt.Fprintf(e.err, "  ETag: %s  (pass it as --param If-Match=%s to update or delete this resource)\n", envelope.ETag, envelope.ETag)
	}
	if envelope.TraceID != "" {
		fmt.Fprintf(e.err, "  Trace-Id: %s\n", envelope.TraceID)
	}
	if envelope.IdempotencyKey != "" {
		fmt.Fprintf(e.err, "  Idempotency-Key: %s  (resend the SAME key to retry without double-applying)\n", envelope.IdempotencyKey)
	}
	if envelope.SchemaViolation != "" {
		// A warning, not a failure. The box answered; what it answered does not
		// match what the document promises, and that is a defect worth seeing on
		// every call rather than a reason to withhold the answer.
		fmt.Fprintf(e.err, "  WARNING: response does not match the declared schema for %d: %s\n", envelope.Status, envelope.SchemaViolation)
	}
	switch {
	case envelope.Body != nil:
		enc := json.NewEncoder(e.out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(envelope.Body)
	case envelope.BodyText != "":
		fmt.Fprintf(e.out, "%s\n", envelope.BodyText)
	default:
		fmt.Fprintf(e.err, "  (no body; %s)\n", orNone(contentType))
	}
}

func orNone(s string) string {
	if s == "" {
		return "no content-type"
	}
	return s
}
