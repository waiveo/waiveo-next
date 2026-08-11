package apiop

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

// DefaultTimeout bounds one request. A pack install or a workspace export is the
// slow end of this surface; a minute is generous for both and still short enough
// that a wedged box is reported rather than waited on forever.
const DefaultTimeout = 60 * time.Second

// maxResponseBytes caps what a single response may put in memory. The largest
// thing this surface returns is a workspace archive, and a caller that wants one
// of those wants it in a file, not in a CLI's response buffer.
const maxResponseBytes = 64 << 20

// maxBodyFileBytes caps a file-supplied request body for the same reason, from
// the other direction.
const maxBodyFileBytes = 256 << 20

// Config is everything a Client needs to reach a deployment. It holds no
// defaults for the two things that must never have one — the address and the
// credential — so a Client built from a zero Config fails rather than quietly
// addressing somebody's loopback.
type Config struct {
	// BaseURL is the deployment's origin, e.g. https://192.168.50.12:7420. The
	// document's own server prefix (/api/v1) is appended by the Client, so a
	// caller never spells the API version and cannot disagree with the document
	// about it.
	BaseURL string
	// Token is the api-key bearer. Empty is allowed and means "send no
	// credential": the three credential-exchange operations declare `security: []`
	// and a caller may legitimately have none yet.
	Token string
	// CAFile is a PEM bundle to verify the deployment's certificate against.
	CAFile string
	// InsecureTLS skips verification entirely. A dev feeder serves a self-signed
	// ed25519 leaf, so this is the loopback developer's escape hatch and nothing
	// else; it is mutually exclusive with CAFile, because a caller who supplied a
	// trust root and got no verification would be wrong about their own security.
	InsecureTLS bool
	Timeout     time.Duration
}

// Client executes operations from one Surface against one deployment.
type Client struct {
	surface *Surface
	cfg     Config
	base    *url.URL
	http    *http.Client
}

// NewClient validates the configuration and builds the transport.
func NewClient(s *Surface, cfg Config) (*Client, error) {
	if s == nil {
		return nil, errors.New("apiop: no surface")
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("no base URL: pass --api, set WAIVEO_API, or put base_url in the config file")
	}
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"))
	if err != nil {
		return nil, fmt.Errorf("base URL %q: %w", cfg.BaseURL, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("base URL %q needs a scheme and a host, e.g. https://192.168.50.12:7420", cfg.BaseURL)
	}
	if cfg.InsecureTLS && cfg.CAFile != "" {
		return nil, errors.New("--insecure-tls and --ca-file contradict each other: one says verify against this root, the other says do not verify")
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.InsecureTLS {
		tlsCfg.InsecureSkipVerify = true
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read --ca-file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--ca-file %s holds no PEM certificate", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg

	return &Client{
		surface: s,
		cfg:     cfg,
		base:    base,
		http: &http.Client{
			Timeout:   timeout,
			Transport: tr,
			// Redirects are REFUSED, not followed. The Authorization header is set on
			// the request, and a followed redirect re-sends it to wherever the
			// redirect points — which is how a bearer credential for one box ends up
			// at another host. This API declares no redirect anyway, so a 3xx here is
			// something the operator should see rather than something to chase.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// BaseURL is the origin this client addresses.
func (c *Client) BaseURL() string { return c.base.String() }

// Args are one invocation's arguments, keyed by the parameter names the document
// declares. Values may be strings (what a command line produces) or already-typed
// JSON values (what an MCP client produces); both go through the same coercion
// against the declared schema, so the two front ends cannot disagree about what
// a valid argument is.
type Args struct {
	Params map[string]any
	// Body is the request body as a JSON value, for an application/json operation.
	Body json.RawMessage
	// BodyPath names a file whose bytes are the request body, for an operation
	// whose body is not JSON.
	BodyPath string
}

// Result is one executed request.
type Result struct {
	OperationID string
	Method      string
	URL         string
	Status      int
	Header      http.Header
	Body        []byte
	ContentType string
	TraceID     string
	ETag        string
	// IdempotencyKey is the key this invocation sent, or "" when the operation
	// does not accept one. Reported because a caller retrying by hand needs to
	// send the SAME key, and a key it never saw is one it cannot reuse.
	IdempotencyKey string
	Duration       time.Duration
}

// OK reports a 2xx.
func (r *Result) OK() bool { return r.Status >= 200 && r.Status < 300 }

// JSONBody decodes the response body, or returns nil for an empty one.
func (r *Result) JSONBody() (any, error) {
	if len(bytes.TrimSpace(r.Body)) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(r.Body, &v); err != nil {
		return nil, fmt.Errorf("response body is not JSON: %w", err)
	}
	return v, nil
}

// Do executes op with args.
//
// It returns a Result for any completed exchange, INCLUDING a 4xx or 5xx: a
// refusal is an answer, and the Problem body carrying the error code is the most
// useful thing on the wire. Only a request that could not be made or a response
// that could not be read is an error.
func (c *Client) Do(ctx context.Context, op Operation, args Args) (*Result, error) {
	req, key, err := c.newRequest(ctx, op, args)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", op.Method, req.URL.Redacted(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response from %s: %w", req.URL.Redacted(), err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("response from %s exceeds the %d-byte ceiling this client buffers", req.URL.Redacted(), maxResponseBytes)
	}
	return &Result{
		OperationID:    op.ID,
		Method:         op.Method,
		URL:            req.URL.Redacted(),
		Status:         resp.StatusCode,
		Header:         resp.Header,
		Body:           body,
		ContentType:    resp.Header.Get("Content-Type"),
		TraceID:        resp.Header.Get("Trace-Id"),
		ETag:           resp.Header.Get("ETag"),
		IdempotencyKey: key,
		Duration:       time.Since(started),
	}, nil
}

// newRequest builds the HTTP request op+args describe, and reports the
// Idempotency-Key it attached.
func (c *Client) newRequest(ctx context.Context, op Operation, args Args) (*http.Request, string, error) {
	if err := checkArgNames(op, args); err != nil {
		return nil, "", err
	}

	path := op.Path
	query := url.Values{}
	header := http.Header{}

	for _, p := range op.Params {
		raw, supplied := args.Params[p.Name]
		if !supplied {
			if p.Required && !engineHeaders[p.Name] {
				return nil, "", fmt.Errorf("%s requires the %s parameter %q (%s)", op.ID, p.In, p.Name, firstLine(p.Description))
			}
			continue
		}
		wire, err := coerce(raw, p.Schema)
		if err != nil {
			return nil, "", fmt.Errorf("%s parameter %q: %w", p.In, p.Name, err)
		}
		switch p.In {
		case openapi3.ParameterInPath:
			placeholder := "{" + p.Name + "}"
			if !strings.Contains(path, placeholder) {
				return nil, "", fmt.Errorf("%s declares path parameter %q that its path template %s does not contain", op.ID, p.Name, op.Path)
			}
			path = strings.ReplaceAll(path, placeholder, url.PathEscape(wire))
		case openapi3.ParameterInQuery:
			query.Set(p.Name, wire)
		case openapi3.ParameterInHeader:
			header.Set(p.Name, wire)
		default:
			return nil, "", fmt.Errorf("%s parameter %q is declared `in: %s`, which this engine does not encode", op.ID, p.Name, p.In)
		}
	}
	// A template placeholder no declared parameter filled. Catching it here turns
	// a document defect into a diagnosis instead of a 404 against a URL with a
	// literal brace in it.
	if i := strings.Index(path, "{"); i >= 0 {
		return nil, "", fmt.Errorf("%s: path template %s has an unfilled placeholder at %q — no parameter declares it", op.ID, op.Path, path[i:])
	}

	body, contentType, err := c.requestBody(op, args)
	if err != nil {
		return nil, "", err
	}

	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + c.surface.BasePath() + path
	u.RawQuery = query.Encode()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, op.Method, u.String(), reader)
	if err != nil {
		return nil, "", err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", MediaJSON)

	// API-072: a mutating POST accepts Idempotency-Key so an MCP client's own
	// retry-on-timeout cannot double-apply. The engine mints one per invocation
	// rather than asking the caller for it, because the caller who forgets is
	// exactly the caller whose retry doubles the write. Driven off the DECLARED
	// parameter, so an operation that gains the header gains the key with no
	// change here.
	var key string
	if _, accepts := op.Param("Idempotency-Key"); accepts && req.Header.Get("Idempotency-Key") == "" && op.Method != http.MethodGet {
		key, err = newIdempotencyKey()
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Idempotency-Key", key)
	}
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	return req, key, nil
}

func (c *Client) requestBody(op Operation, args Args) ([]byte, string, error) {
	if op.Body == nil {
		if len(args.Body) > 0 || args.BodyPath != "" {
			return nil, "", fmt.Errorf("%s declares no request body", op.ID)
		}
		return nil, "", nil
	}
	if op.Body.JSON {
		if args.BodyPath != "" {
			return nil, "", fmt.Errorf("%s takes a JSON body — supply it as %s, not %s", op.ID, BodyArg, BodyPathArg)
		}
		if len(bytes.TrimSpace(args.Body)) == 0 {
			if op.Body.Required {
				return nil, "", fmt.Errorf("%s requires a request body (--body @file, --body -, or --body '<json>')", op.ID)
			}
			return nil, "", nil
		}
		if !json.Valid(args.Body) {
			return nil, "", fmt.Errorf("%s: the supplied body is not valid JSON", op.ID)
		}
		return args.Body, op.Body.MediaType, nil
	}

	if len(args.Body) > 0 {
		return nil, "", fmt.Errorf("%s takes a %s body — supply a file with %s", op.ID, op.Body.MediaType, BodyPathArg)
	}
	if args.BodyPath == "" {
		if op.Body.Required {
			return nil, "", fmt.Errorf("%s requires a %s body: supply a file path", op.ID, op.Body.MediaType)
		}
		return nil, "", nil
	}
	f, err := os.Open(args.BodyPath)
	if err != nil {
		return nil, "", fmt.Errorf("open request body: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxBodyFileBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read request body %s: %w", args.BodyPath, err)
	}
	if len(raw) > maxBodyFileBytes {
		return nil, "", fmt.Errorf("request body %s exceeds the %d-byte ceiling this client buffers", args.BodyPath, maxBodyFileBytes)
	}
	return raw, op.Body.MediaType, nil
}

// checkArgNames refuses an argument the operation does not declare.
//
// Dropping it silently is the failure mode that matters: `--param sceen_id=...`
// would list every screen instead of reading one, and report success.
func checkArgNames(op Operation, args Args) error {
	var unknown []string
	for name := range args.Params {
		p, declared := op.Param(name)
		if !declared {
			unknown = append(unknown, name)
			continue
		}
		if p.In == openapi3.ParameterInHeader && engineHeaders[name] {
			unknown = append(unknown, name+" (supplied by this client, not by the caller)")
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	declared := make([]string, 0, len(op.Params))
	for _, p := range op.Params {
		if p.In == openapi3.ParameterInHeader && engineHeaders[p.Name] {
			continue
		}
		declared = append(declared, p.Name)
	}
	if len(declared) == 0 {
		return fmt.Errorf("%s declares no parameters, but %s was supplied", op.ID, strings.Join(unknown, ", "))
	}
	return fmt.Errorf("%s does not declare %s (it declares: %s)", op.ID, strings.Join(unknown, ", "), strings.Join(declared, ", "))
}

// coerce turns a supplied argument into its wire form, checking it against the
// declared schema on the way.
//
// A command line hands every value over as a string; an MCP client hands over
// real JSON types. Both are converted to the DECLARED type first and validated
// second, so `limit=500` is refused here — against the document's own
// `maximum: 200` — rather than at the server, and `limit=abc` is refused as a
// type error rather than sent.
func coerce(raw any, ref *openapi3.SchemaRef) (string, error) {
	if ref == nil || ref.Value == nil {
		return fmt.Sprint(raw), nil
	}
	schema := ref.Value
	declared := ""
	if schema.Type != nil && len(*schema.Type) > 0 {
		declared = (*schema.Type)[0]
	}
	if declared == "array" || declared == "object" {
		return "", fmt.Errorf("declared as %s, which this engine does not encode into a URL", declared)
	}

	var typed any
	switch v := raw.(type) {
	case string:
		switch declared {
		case "integer":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return "", fmt.Errorf("%q is not an integer", v)
			}
			typed = float64(n)
		case "number":
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return "", fmt.Errorf("%q is not a number", v)
			}
			typed = n
		case "boolean":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return "", fmt.Errorf("%q is not a boolean", v)
			}
			typed = b
		default:
			typed = v
		}
	case json.Number:
		n, err := v.Float64()
		if err != nil {
			return "", err
		}
		typed = n
	case float64, bool:
		typed = v
	case nil:
		return "", errors.New("no value")
	default:
		return "", fmt.Errorf("unsupported argument type %T", raw)
	}

	if err := schema.VisitJSON(typed); err != nil {
		return "", fmt.Errorf("%v", schemaError(err))
	}
	return wireString(typed), nil
}

func wireString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// schemaError trims kin-openapi's multi-line rendering (which repeats the whole
// schema back) down to its reason.
func schemaError(err error) string {
	var se *openapi3.SchemaError
	if errors.As(err, &se) && se.Reason != "" {
		return se.Reason
	}
	return firstLine(err.Error())
}

// isJSONMedia accepts application/json and every `+json` structured suffix,
// ignoring parameters like `; charset=utf-8`.
func isJSONMedia(contentType string) bool {
	base := strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
	return base == MediaJSON || strings.HasSuffix(base, "+json")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// newIdempotencyKey mints a fresh replay key. 128 random bits, hex — inside the
// document's 1..255 length bound with room to spare, and drawn from crypto/rand
// because a key a second caller could guess is a key that collides with somebody
// else's write.
func newIdempotencyKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint an Idempotency-Key: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ValidateResponse checks a response body against the schema the document
// declares for that status.
//
// This is the half of "the document is the contract" that a request builder
// alone does not give you: it is what makes a check of a running box a check
// that the box answers what it PROMISED, not merely that it answered. A status
// the document declares no JSON schema for returns nil — several families are
// shape stubs whose response schema is a later minor, and inventing a verdict
// for them would be this gate lying in the other direction.
func (s *Surface) ValidateResponse(op Operation, r *Result) error {
	if r == nil {
		return errors.New("no result")
	}
	ref := op.ResponseSchema(r.Status)
	if ref == nil || ref.Value == nil {
		return nil
	}
	if ct := r.ContentType; ct != "" && !isJSONMedia(ct) {
		return fmt.Errorf("declared a JSON body for %d, got Content-Type %q", r.Status, ct)
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(r.Body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("body is not JSON: %w", err)
	}
	// kin-openapi validates plain Go values; json.Number is not one of them, so
	// the body is re-read without it. UseNumber above is only to reject a body
	// that is not JSON at all before the second pass.
	if err := json.Unmarshal(r.Body, &v); err != nil {
		return fmt.Errorf("body is not JSON: %w", err)
	}
	if err := ref.Value.VisitJSON(v, openapi3.MultiErrors()); err != nil {
		return errors.New(schemaError(err))
	}
	return nil
}
