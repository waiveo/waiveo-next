package packs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpfetcher.go is the network half of the Fetcher seam (marketplace.go): the
// transport a registry source served over the internet is read through, beside
// the FileFetcher a `file://` source is read through.
//
// It adds NO trust, and that is worth stating plainly because "fetched over TLS"
// reads like it does. Every check that decides whether an artifact is acceptable
// runs on the bytes AFTER they arrive — the entry's own digest and size
// (CHI-021/023), the artifact's signature envelope against the host's own trust
// anchors (MKT-009b), the manifest gate, the anti-rollback floors. TLS here
// establishes that the bytes came from the host the OPERATOR configured, not
// that their contents may be trusted; a compromised registry serving a
// well-signed index of artifacts it may not vouch for is refused by the same
// checks whether it reached the box over https or off a USB stick.
//
// What this file is actually for is the other direction: keeping an index
// author's chosen `download_url` from becoming a request this appliance would
// not otherwise make.
//
// # This runs on an appliance inside someone's LAN
//
// `download_url` is a value whoever serves the index chooses, and the index
// signature chain that would authenticate it does not exist in this repository
// yet (marketplace.go's package doc; conformance/unimplemented-error-codes.json
// records the nine channel-index/1 codes). So an unconfined network fetcher
// would hand any party who can serve or tamper with an index a request-forgery
// primitive originating INSIDE the network the box sits in — a box that can
// reach the router's admin page, the other appliances, and the cloud metadata
// endpoint of whatever it is virtualized on. The confinements below are what
// stop that, and they are all keyed on values the HOST configured:
//
//   - https only. Not for confidentiality — a pack artifact is public — but
//     because a plaintext index is rewritable by anyone on the path, and an
//     index is precisely the document that decides WHICH signed artifact this
//     box installs. Yanks and channel pointers are the parts an on-path attacker
//     most wants to edit, and both are transport-authenticated only.
//   - one ORIGIN, the one the configured index URL names. An index may point at
//     any path on its own registry and at nothing else.
//   - no REDIRECTS. A redirect is a second URL the registry did not have to
//     publish and the operator never saw; following one would reintroduce the
//     cross-origin hop the origin check exists to prevent, one Location header
//     later. A registry serves its artifacts at the URLs it publishes.
//   - no CREDENTIALS in the URL, which an index author could otherwise use to
//     make the box present them to a host it reached.
//   - a BYTE CAP and a TIMEOUT on every object, both applied before and during
//     the read, so a registry cannot exhaust the box's memory or hold a boot-time
//     goroutine open indefinitely.

// registryFetchTimeout bounds ONE index or artifact fetch, end to end.
//
// Generous by API standards on purpose: the object at the far end may be an
// 8 MiB artifact and the box may be on a domestic uplink, so a tight timeout
// would turn a slow-but-working registry into an unresolvable one. It is a
// bound on hanging, not a latency target.
const registryFetchTimeout = 60 * time.Second

// refuseRedirect is the redirect policy every fetch runs under, without
// exception — see effectiveClient for why "without exception" is the whole
// point.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("refusing a redirect to %s: a registry source serves its objects at the urls it publishes", req.URL.Redacted())
}

// registryHTTPClient is the shared client every HTTPFetcher without an injected
// one uses: one connection pool for the process rather than a fresh transport
// per fetch.
//
// No Client.Timeout: the per-fetch deadline rides the request CONTEXT instead,
// so one shared client can serve fetchers with different budgets and so a caller
// cancelling its context (a shutdown mid-install) is honoured immediately.
var registryHTTPClient = &http.Client{CheckRedirect: refuseRedirect}

// effectiveClient returns the client this fetch runs on, with the redirect
// refusal imposed on it whatever the caller supplied.
//
// The override is not defensive tidiness; it closes a real hole. The
// no-cross-origin rule is enforced on the URL BEFORE the request, so a followed
// redirect steps around it entirely: the checked URL is on the configured
// origin, and the Location header the registry answers with is not. An injected
// client carries its own redirect policy — net/http's default FOLLOWS up to ten
// — so leaving the policy to the client would mean the confinement held for the
// default configuration and silently lapsed for any other. It is a property of
// this fetcher, so it is set here.
//
// The copy is shallow on purpose: Transport is a pointer, so the connection pool
// and TLS configuration are shared, and only the policy differs.
func (f HTTPFetcher) effectiveClient() *http.Client {
	if f.Client == nil {
		return registryHTTPClient
	}
	c := *f.Client
	c.CheckRedirect = refuseRedirect
	return &c
}

// HTTPFetcher serves one network registry source's index and artifacts over
// https, confined to the origin of the index URL the HOST configured.
//
// Base is that index URL — the whole URL, not just the origin, because a
// relative `download_url` in the index is resolved against it, which is how a
// registry publishes artifact locations without hardcoding its own hostname.
type HTTPFetcher struct {
	// Base is the source's absolute https index URL (channel-index/1 CHI-080).
	// Its origin is the only one this fetcher will talk to.
	Base string
	// MaxBytes caps a single fetched object. Zero means DefaultLimits' artifact
	// cap, which is also the cap the install pipeline enforces — so a fetcher
	// left at its default cannot admit bytes the pipeline would refuse anyway.
	MaxBytes int64
	// Timeout bounds one fetch. Zero means registryFetchTimeout.
	Timeout time.Duration
	// Client is the HTTP client, nil meaning the shared redirect-refusing one.
	// It exists so a test can inject a client trusting a test server's cert;
	// production takes the default.
	Client *http.Client
}

func (f HTTPFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	target, err := f.resolve(rawURL)
	if err != nil {
		return nil, err
	}
	limit := f.MaxBytes
	if limit <= 0 {
		limit = DefaultLimits.MaxArtifactBytes
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = registryFetchTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("packs: fetch %s: %w", rawURL, err)
	}
	resp, err := f.effectiveClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("packs: fetch %s: %w", rawURL, err)
	}
	defer func() {
		// Drain a bounded amount before closing so the connection can be reused;
		// an unbounded drain would hand the cap we just enforced straight back.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("packs: fetch %s: registry answered %s", rawURL, resp.Status)
	}
	// The declared length first, so an oversize object is refused before a byte
	// of it is read. It is only a hint — a lying or absent Content-Length is
	// caught by the read below — but when it is honest it saves the transfer.
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("packs: fetch %s: registry declares %d bytes, past the %d-byte cap", rawURL, resp.ContentLength, limit)
	}
	// limit+1, so "exactly at the cap" and "past it" are distinguishable: a read
	// that stopped AT the cap cannot tell a complete object from a truncated one.
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("packs: fetch %s: %w", rawURL, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("packs: fetch %s: object exceeds the %d-byte cap", rawURL, limit)
	}
	return body, nil
}

// resolve turns an index URL or a `download_url` into the one absolute https URL
// this fetcher is permitted to request, or refuses it.
func (f HTTPFetcher) resolve(rawURL string) (*url.URL, error) {
	base, err := url.Parse(f.Base)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return nil, fmt.Errorf("packs: fetch %s: the registry source's base url %q is not an absolute https url", rawURL, f.Base)
	}
	ref, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("packs: fetch %s: not a url (%w)", rawURL, err)
	}
	// A relative reference resolves against the index's own URL; an absolute one
	// keeps its own scheme and host and faces the two checks below.
	target := base.ResolveReference(ref)
	if target.Scheme != "https" {
		return nil, fmt.Errorf("packs: fetch %s: only the https:// scheme is served by this registry source (got %q)", rawURL, target.Scheme)
	}
	if !strings.EqualFold(target.Host, base.Host) {
		return nil, fmt.Errorf("packs: fetch %s: resolves to host %q, outside the %q origin this registry source is configured for", rawURL, target.Host, base.Host)
	}
	if target.User != nil {
		return nil, fmt.Errorf("packs: fetch %s: carries credentials in the url", rawURL)
	}
	return target, nil
}
