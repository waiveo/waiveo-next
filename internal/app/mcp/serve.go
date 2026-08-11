package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/maaxton/waiveo-next/internal/app/apiop"
)

// ProtocolVersion is the MCP revision this server speaks by default. A client
// asking for a revision in supportedProtocols is answered in THAT revision — the
// handshake is a negotiation, and a server that always answered with its own
// newest would tell a 2024-11-05 client it is talking 2025-06-18.
const ProtocolVersion = "2025-06-18"

var supportedProtocols = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// JSON-RPC 2.0 error codes, as the MCP transport uses them.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// maxLineBytes bounds one JSON-RPC message. The transport is newline-delimited,
// so an unbounded reader is an unbounded allocation controlled by whatever is on
// the other end of stdin.
const maxLineBytes = 32 << 20

// ServeOptions wires one stdio MCP server.
type ServeOptions struct {
	// Surface and Client are the SAME pair `waiveo call` uses. A server that built
	// its own would be a second implementation of every operation.
	Surface *apiop.Surface
	Client  *apiop.Client
	// In and Out are the transport. Out carries protocol frames and NOTHING else:
	// a stray Printf on stdout corrupts the stream, which is why every diagnostic
	// in this package goes to Log.
	In  io.Reader
	Out io.Writer
	Log io.Writer
	// ServerVersion is reported at initialize.
	ServerVersion string
}

// Serve runs the MCP stdio transport until the input closes or ctx is cancelled.
//
// The transport is newline-delimited JSON-RPC: one message per line, responses
// written in the order requests arrive. Requests are handled sequentially rather
// than concurrently — a tool call here is an HTTP request against one box, and
// serialising them means a model cannot accidentally fire twenty mutations at a
// single appliance at once.
func Serve(ctx context.Context, opts ServeOptions) error {
	if opts.Surface == nil || opts.Client == nil {
		return errors.New("mcp: serve needs a surface and a client")
	}
	if opts.In == nil || opts.Out == nil {
		return errors.New("mcp: serve needs an input and an output")
	}
	logw := opts.Log
	if logw == nil {
		logw = io.Discard
	}
	tools, err := ToolsFrom(opts.Surface)
	if err != nil {
		return err
	}
	byName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}

	s := &server{
		opts:   opts,
		tools:  tools,
		byName: byName,
		out:    bufio.NewWriter(opts.Out),
	}
	// Announced on the LOG stream, never on Out: the first byte a client reads
	// from a stdio server must be a protocol frame, and a banner on stdout is the
	// classic way to make a working server look broken.
	fmt.Fprintf(logw, "waiveo mcp: serving %d curated tool(s) for %s over stdio (MCP %s)\n",
		len(tools), opts.Client.BaseURL(), ProtocolVersion)

	scanner := bufio.NewScanner(opts.In)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := s.handleLine(ctx, []byte(line)); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("mcp: read stdin: %w", err)
	}
	return nil
}

type server struct {
	opts ServeOptions

	tools  []Tool
	byName map[string]Tool

	// mu guards out. Requests are dispatched sequentially, so it exists for the
	// one thing that is not sequential: nothing here may interleave two frames on
	// the transport, and a half-written frame is an unrecoverable desync.
	mu  sync.Mutex
	out *bufio.Writer
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *server) handleLine(ctx context.Context, line []byte) error {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return s.write(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeParseError, "not JSON-RPC: " + err.Error()}})
	}
	// A notification carries no id and MUST NOT be answered. `notifications/
	// initialized` is the common one; answering it is a protocol violation that
	// some clients treat as a fatal desync.
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"
	if req.JSONRPC != "2.0" {
		if isNotification {
			return nil
		}
		return s.write(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{codeInvalidRequest, `"jsonrpc" must be "2.0"`}})
	}

	result, rerr := s.dispatch(ctx, req)
	if isNotification {
		return nil
	}
	if rerr != nil {
		return s.write(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rerr})
	}
	return s.write(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *server) dispatch(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params), nil
	case "notifications/initialized", "notifications/cancelled":
		return map[string]any{}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return s.listTools()
	case "tools/call":
		return s.callTool(ctx, req.Params)
	default:
		return nil, &rpcError{codeMethodNotFound, "unsupported method " + req.Method}
	}
}

func (s *server) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	agreed := ProtocolVersion
	if supportedProtocols[p.ProtocolVersion] {
		agreed = p.ProtocolVersion
	}

	reads, acts := 0, 0
	for _, t := range s.tools {
		if t.Mutating {
			acts++
		} else {
			reads++
		}
	}
	return map[string]any{
		"protocolVersion": agreed,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "waiveo", "version": s.opts.ServerVersion},
		"instructions": fmt.Sprintf(
			"Tools for one waiveo deployment at %s. %d read tools and %d act tools, derived from that deployment's own api/1 document — "+
				"a read has no side effect a retry could double-apply; an act mutates state and carries a fresh Idempotency-Key per call. "+
				"Updates and deletes require If-Match carrying the resource's current ETag, which a read returns. "+
				"A tool result is a JSON envelope: status, the response body, and the server's Trace-Id for correlating with its logs.",
			s.opts.Client.BaseURL(), reads, acts),
	}
}

func (s *server) listTools() (any, *rpcError) {
	out := make([]any, 0, len(s.tools))
	for _, t := range s.tools {
		schema, err := s.opts.Surface.InputSchema(t.Op)
		if err != nil {
			return nil, &rpcError{codeInternalError, err.Error()}
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"title":       t.Op.Summary,
			"description": toolDescription(t),
			"inputSchema": schema,
			// The read/act distinction, in the vocabulary a client already reads.
			// readOnlyHint is the one that matters: it is what lets a client decide
			// to confirm before calling, and it is API-070's own distinction rather
			// than a guess from the HTTP method.
			"annotations": map[string]any{
				"title":           t.Op.Summary,
				"readOnlyHint":    !t.Mutating,
				"destructiveHint": t.Mutating && t.Method == "DELETE",
				"idempotentHint":  !t.Mutating || t.RequiresIdempotencyKey || t.Method == "PUT" || t.Method == "DELETE",
				"openWorldHint":   false,
			},
		})
	}
	return map[string]any{"tools": out}, nil
}

// toolDescription prefixes the document's own prose with the two facts a caller
// needs before reading any of it: whether this changes anything, and what it
// addresses.
func toolDescription(t Tool) string {
	kind := "READ"
	if t.Mutating {
		kind = "ACT (mutates state)"
	}
	return fmt.Sprintf("[%s] %s %s\n\n%s", kind, t.Method, t.Path, t.Description)
}

func (s *server) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{codeInvalidParams, "params: " + err.Error()}
	}
	tool, ok := s.byName[p.Name]
	if !ok {
		return nil, &rpcError{codeInvalidParams, fmt.Sprintf("no tool named %q (%d tools; call tools/list)", p.Name, len(s.tools))}
	}

	args, err := ArgsFrom(tool.Op, p.Arguments)
	if err != nil {
		// An argument mistake is the MODEL's error to correct, not a transport
		// fault, so it comes back as a tool result it can read and retry from
		// rather than as a JSON-RPC error that may never reach it.
		return toolFailure(err.Error()), nil
	}

	res, err := s.opts.Client.Do(ctx, tool.Op, args)
	if err != nil {
		return toolFailure(err.Error()), nil
	}
	envelope := Envelope(s.opts.Surface, tool.Op, res)
	text, mErr := json.MarshalIndent(envelope, "", "  ")
	if mErr != nil {
		return nil, &rpcError{codeInternalError, mErr.Error()}
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(text)}},
		"isError": !res.OK(),
	}, nil
}

func toolFailure(msg string) any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": msg}},
		"isError": true,
	}
}

// ArgsFrom splits an MCP tool call's flat argument object into the parameter bag
// and the request body the engine takes.
//
// It is exported because `waiveo call` needs the SAME split for the arguments it
// assembles from flags: the two front ends differ in how a caller TYPES an
// argument, and must not differ in what the argument then means.
func ArgsFrom(op apiop.Operation, in map[string]any) (apiop.Args, error) {
	args := apiop.Args{Params: map[string]any{}}
	for k, v := range in {
		switch {
		case k == apiop.BodyArg && op.Body != nil && op.Body.JSON:
			raw, err := json.Marshal(v)
			if err != nil {
				return args, fmt.Errorf("%s: %w", apiop.BodyArg, err)
			}
			args.Body = raw
		case k == apiop.BodyPathArg && op.Body != nil && !op.Body.JSON:
			path, ok := v.(string)
			if !ok {
				return args, fmt.Errorf("%s must be a string path", apiop.BodyPathArg)
			}
			args.BodyPath = path
		default:
			args.Params[k] = v
		}
	}
	return args, nil
}

// ResultEnvelope is what a tool call and `waiveo call --json` both return. One
// shape, so an agent driving the box over MCP and an operator driving it from a
// shell are reading the same answer.
type ResultEnvelope struct {
	Operation string `json:"operation"`
	Method    string `json:"method"`
	URL       string `json:"url"`
	Status    int    `json:"status"`
	OK        bool   `json:"ok"`
	TraceID   string `json:"trace_id,omitempty"`
	// ETag is the value an update or delete of this resource must send back as
	// If-Match. Surfaced because a caller that has to go and find it makes one
	// extra call to learn what the call it just made already knew.
	ETag           string `json:"etag,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	DurationMs     int64  `json:"duration_ms"`
	Body           any    `json:"body,omitempty"`
	// BodyText carries a non-JSON response verbatim (an archive download's bytes
	// are summarised rather than embedded).
	BodyText string `json:"body_text,omitempty"`
	// SchemaViolation is set when the response did not match the schema the
	// document declares for its status. It is REPORTED rather than raised: the
	// answer the box gave is still the most useful thing here, and a caller that
	// only ever saw "invalid" would have lost it.
	SchemaViolation string `json:"schema_violation,omitempty"`
}

// Envelope renders one executed request.
func Envelope(s *apiop.Surface, op apiop.Operation, r *apiop.Result) ResultEnvelope {
	env := ResultEnvelope{
		Operation:      r.OperationID,
		Method:         r.Method,
		URL:            r.URL,
		Status:         r.Status,
		OK:             r.OK(),
		TraceID:        r.TraceID,
		ETag:           r.ETag,
		IdempotencyKey: r.IdempotencyKey,
		DurationMs:     r.Duration.Milliseconds(),
	}
	if body, err := r.JSONBody(); err == nil && body != nil {
		env.Body = body
	} else if n := len(r.Body); n > 0 {
		env.BodyText = fmt.Sprintf("<%d bytes of %s>", n, r.ContentType)
	}
	if err := s.ValidateResponse(op, r); err != nil {
		env.SchemaViolation = err.Error()
	}
	return env
}

func (s *server) write(resp rpcResponse) error {
	raw, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("mcp: encode response: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.out.Write(raw); err != nil {
		return err
	}
	if err := s.out.WriteByte('\n'); err != nil {
		return err
	}
	return s.out.Flush()
}
