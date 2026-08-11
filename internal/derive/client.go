package derive

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// client.go is the derive tool's side of the loop: read the work queue, upload
// the PNG, write the reference back.
//
// Every operation here is an ORDINARY api/1 call. There is deliberately no
// bespoke ingest endpoint for rendered output: the PNG goes through the same
// POST /content every other asset does, and the reference lands on the layer
// through the same PATCH /casts/{id} an operator's own edit uses — under the
// same If-Match precondition, so a derive run that races a human edit loses the
// write rather than clobbering it. A private write path for the renderer would
// be a second way for content to enter the system, with a second set of rules to
// keep in step with the first.

// PendingJob is one work order as GET /derive/pending returns it.
type PendingJob struct {
	CastID     string           `json:"cast_id"`
	CastName   string           `json:"cast_name"`
	SlideID    string           `json:"slide_id"`
	LayerIndex int              `json:"layer_index"`
	State      string           `json:"state"`
	SpecDigest string           `json:"spec_digest"`
	W          int              `json:"w"`
	H          int              `json:"h"`
	Spec       *wire.DeriveSpec `json:"spec"`
}

// Client talks to a waiveo-next feeder's api/1 surface.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// ClientOptions configures a Client.
type ClientOptions struct {
	// BaseURL is the feeder's origin, e.g. https://192.168.50.12:7420.
	BaseURL string
	// Token is the api-key bearer. It is sent to BaseURL and nowhere else.
	Token string
	// InsecureTLS skips certificate verification. Required against a dev feeder
	// (which serves an ed25519 leaf under a self-signed dev root) and off by
	// default everywhere, so reaching an unverified box is always a decision
	// somebody typed.
	InsecureTLS bool
	Timeout     time.Duration
}

// NewClient builds an api client.
func NewClient(opt ClientOptions) (*Client, error) {
	if opt.BaseURL == "" {
		return nil, fmt.Errorf("derive: no feeder base URL")
	}
	u, err := url.Parse(opt.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("derive: %q is not an absolute feeder URL", opt.BaseURL)
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 30 * time.Second
	}
	tr := http.DefaultTransport
	if opt.InsecureTLS {
		//nolint:gosec // Opt-in only: see ClientOptions.InsecureTLS.
		tr = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return &Client{
		base:  strings.TrimSuffix(opt.BaseURL, "/"),
		token: opt.Token,
		http:  &http.Client{Timeout: opt.Timeout, Transport: tr},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, hdr map[string]string) (*http.Response, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("derive: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("derive: %s %s: read body: %w", method, path, err)
	}
	return resp, raw, nil
}

// ListPending reads the outstanding rasterizations.
func (c *Client) ListPending(ctx context.Context) ([]PendingJob, error) {
	resp, raw, err := c.do(ctx, http.MethodGet, "/api/v1/derive/pending", nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("derive: GET /derive/pending: %d %s", resp.StatusCode, snippet(raw))
	}
	var out struct {
		Jobs []PendingJob `json:"derive_jobs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("derive: decode the pending listing: %w", err)
	}
	return out.Jobs, nil
}

// UploadContent stores PNG bytes and returns the server-computed asset_ref.
// The ref is never computed here even though it is a plain sha256 of the body:
// the server is authoritative for what it stored, and a locally-computed ref
// would agree right up until the day the two disagreed silently.
func (c *Client) UploadContent(ctx context.Context, png []byte) (string, error) {
	resp, raw, err := c.do(ctx, http.MethodPost, "/api/v1/content", png,
		map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("derive: POST /content: %d %s", resp.StatusCode, snippet(raw))
	}
	var out struct {
		AssetRef string `json:"asset_ref"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("derive: decode the upload response: %w", err)
	}
	if out.AssetRef == "" {
		return "", fmt.Errorf("derive: the upload response carried no asset_ref: %s", snippet(raw))
	}
	return out.AssetRef, nil
}

// FetchAsset downloads content-origin bytes by reference. It resolves the URL
// from the origin's own listing rather than composing one, so there is exactly
// one content-URL grammar in play and this tool cannot drift from the one the
// server publishes.
func (c *Client) FetchAsset(ctx context.Context, assetRef string) ([]byte, error) {
	resp, raw, err := c.do(ctx, http.MethodGet, "/api/v1/content", nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("derive: GET /content: %d %s", resp.StatusCode, snippet(raw))
	}
	var listing struct {
		Content []struct {
			AssetRef string `json:"asset_ref"`
			URL      string `json:"url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return nil, fmt.Errorf("derive: decode the content listing: %w", err)
	}
	for _, e := range listing.Content {
		if e.AssetRef != assetRef {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.URL, nil)
		if err != nil {
			return nil, err
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		r, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("derive: fetch %s: %w", assetRef, err)
		}
		defer r.Body.Close()
		if r.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("derive: fetch %s: %d", assetRef, r.StatusCode)
		}
		return io.ReadAll(io.LimitReader(r.Body, 64<<20))
	}
	return nil, fmt.Errorf("derive: %s is not in the content origin", assetRef)
}

// GetCast reads a cast and its ETag.
func (c *Client) GetCast(ctx context.Context, id string) (datamodel.Cast, string, error) {
	resp, raw, err := c.do(ctx, http.MethodGet, "/api/v1/casts/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return datamodel.Cast{}, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return datamodel.Cast{}, "", fmt.Errorf("derive: GET /casts/%s: %d %s", id, resp.StatusCode, snippet(raw))
	}
	var cast datamodel.Cast
	if err := json.Unmarshal(raw, &cast); err != nil {
		return datamodel.Cast{}, "", fmt.Errorf("derive: decode cast %s: %w", id, err)
	}
	return cast, resp.Header.Get("ETag"), nil
}

// PatchCastSlides replaces a cast's slide list under an If-Match precondition.
//
// The precondition is not optional and it is not decoration: a derive run reads
// a cast, spends seconds in a browser, and then writes back. If an operator
// edited the cast in between, an unconditional write would silently discard
// their edit and replace it with the pre-render snapshot. A 409 is the correct
// outcome — the run reports it and the next pass picks up the new spec.
func (c *Client) PatchCastSlides(ctx context.Context, id string, slides []datamodel.CastSlide, etag string) error {
	body, err := json.Marshal(map[string]any{"slides": slides})
	if err != nil {
		return err
	}
	hdr := map[string]string{"Content-Type": "application/json"}
	if etag != "" {
		hdr["If-Match"] = etag
	}
	resp, raw, err := c.do(ctx, http.MethodPatch, "/api/v1/casts/"+url.PathEscape(id), body, hdr)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("derive: PATCH /casts/%s: %d %s", id, resp.StatusCode, snippet(raw))
	}
	return nil
}

// snippet bounds an error body so a diagnostic stays readable.
func snippet(raw []byte) string {
	const limit = 400
	s := strings.TrimSpace(string(raw))
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}
