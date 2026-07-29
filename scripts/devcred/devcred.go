// Package devcred is the ONE place this repo's dev HTTP probes get a credential
// for the running feeder, and the one place the provisioning path is written
// down. Every script under scripts/ that speaks to an authenticated route builds
// its client here; none of them reads the key file, spells the path, or decides
// what to do when the key is missing on its own.
//
// # Why a credential is needed at all
//
// Every /api/v1 route except the three `security: []` credential-exchange
// operations is authenticated (security-model/1 SEC-005 — an unresolvable
// principal is refused, never default-permitted), and /events/v1 is
// authenticated too (EVT-110/111). A probe presenting nothing is refused 401.
// Only /healthz, the SPA shell at "/", and the content origin at /content/ are
// reachable unauthenticated, which is why scripts/devsmoke and
// scripts/webuiprobe deliberately do NOT use this package.
//
// # The provisioning path
//
//	make dev-key
//
// That runs scripts/devkey, which opens the app's auth store directly (the same
// SQLite file the feeder opens, .dev/feeder-auth/auth.db), ensures a dedicated
// `dev-script` principal exists holding ONE role binding — `admin` at the root
// scope node — mints an api-key credential for it (auth.Store.MintAPIKey, so the
// key is an ordinary revocable session row, SEC-020), and writes the raw token
// to DefaultPath through internal/shared/secretfile: 0600, inside a 0700
// directory, installed by rename.
//
// `make dev-up` runs the same step before it starts the feeder, so `make dev` is
// self-provisioning and a developer only runs `make dev-key` by hand when
// driving the probes outside make.
//
// # Why `admin` at the root scope node, and not more or less
//
// It is the LEAST authority that lets all five probes do their jobs, and each
// bound is set by a surface rather than chosen for comfort:
//
//   - `viewer` cannot POST /api/v1/content or PATCH a daypart — auth.RequiredRole
//     puts every mutating method at `operator`, and auth.CanWrite puts the
//     placement check there too.
//
//   - `operator` cannot install a pack: the pack lifecycle routes (install,
//     update, uninstall) require `admin` AT THE WORKSPACE ORG NODE, because a
//     pack's capabilities are granted workspace-wide and there is no per-node
//     answer to "may this caller write here" for one (internal/app/api/packs.go,
//     packLifecycleWritable).
//
//   - `owner` is deliberately NOT taken. Owner is what SEC-011 reserves for the
//     irreversible and the break-glass — workspace destruction, the last owner
//     binding, `--new-owner`. Provisioning also does not CLAIM an unclaimed box:
//     it adds no owner binding, so a box that has never been claimed still has
//     its first-boot window (SEC-120) open and exercisable.
//
//     READ THIS BEFORE TREATING `admin` AS SAFELY BELOW `owner`. It very nearly
//     was not: the credential-reset flow gated issuance on the ISSUER holding
//     admin and checked nothing about the TARGET, so an admin could mint a reset
//     for the owner, redeem it over the unauthenticated redemption route, and log
//     in as owner — three calls, no other credential. SEC-012a now refuses a
//     target who outranks the issuer, which is what makes this bullet true rather
//     than merely intended. A dev box that has run the web e2e suite also already
//     holds a real `e2e-owner`, so "the box is unclaimed" is a property of a
//     fresh checkout, not a standing guarantee — do not build anything on it.
//
// The scope node is the root sentinel (auth.RootScopeNode) rather than a real
// org node because the make-dev seed authors no org node at all — the seeded
// tree is site -> screen — so packLifecycleWritable falls back to the root
// sentinel, exactly where a first-boot owner's binding sits. A binding at the
// seeded site would authorize the daypart write and still be refused the pack
// install. One binding at the root is therefore the only single binding that
// satisfies all five probes, and on a single-workspace dev box it is the same
// set of resources an org-node binding would reach.
//
// # Why an absent key is a refusal, not an attempt
//
// Load refuses, naming ProvisionCommand, rather than letting the probe run
// unauthenticated and discover a 401 later. A probe exists to assert that a
// product loop works, so its failure output is read as "the loop broke" — and
// that is exactly how the pre-credential failure read: contentloop retried an
// upload for ten seconds and reported "no successful upload after ~10s", with
// the 401 buried in the tail. The probe cannot recover from a missing
// credential, so continuing buys nothing and costs a worse diagnosis.
//
// # Handling of the secret itself
//
// The key is never a flag value, never an environment-variable value, and never
// an argv word — only the PATH is configurable (PathEnv). It is never rendered
// into a log line or an error: every refusal here names the path and the mode,
// never the bytes. The bearer header is attached by a RoundTripper, so it does
// not sit in any request the caller constructs or prints.
//
// The RoundTripper attaches the credential ONLY to loopback hosts. A header
// added by a transport is re-added on every hop a redirect takes, so a probe
// that followed a redirect off the box would carry the key with it; scoping the
// attachment to 127.0.0.0/8, ::1 and ::ffff:127.0.0.0/8 means it cannot.
package devcred

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/secretfile"
)

const (
	// DefaultDir is the app's auth state directory — the same one the feeder
	// keeps its principal/credential/session database and its first-boot setup
	// code in (auth.DefaultAuthDir). The dev key belongs beside them because it
	// IS one of them: a live bearer credential for this deployment.
	//
	// It is also already git-ignored twice over (.dev/ and an explicit
	// .dev/feeder-auth/ rule), which is the property that matters most for a
	// file whose contents authenticate as an admin.
	DefaultDir = ".dev/feeder-auth"

	// KeyFile is the dev key's filename inside DefaultDir. It is also a
	// standalone .gitignore rule, so the file stays untracked even if a
	// developer points PathEnv at the repo root.
	KeyFile = "dev-api-key"

	// PathEnv overrides the file location. It carries a PATH, never a key:
	// an environment variable holding the secret itself would be readable in
	// `ps e` output and inherited by every child process a probe spawns.
	PathEnv = "WAIVEO_DEV_API_KEY_FILE"

	// ProvisionCommand is what a refusal tells the developer to run. It is a
	// constant so the message cannot drift from the Makefile target.
	ProvisionCommand = "make dev-key"
)

// apiKeyPrefix is the api-key token form auth.MintToken produces (`wv_<aal>_<hex>`)
// and api/openapi.yaml's ApiKey scheme publishes. It is checked so an empty or
// obviously-wrong file is refused HERE, with a diagnosis, rather than becoming a
// 401 that reads like a server fault. It is deliberately a prefix check and not a
// reimplementation of auth.ParseToken: this package does not get to decide what a
// valid token is, only whether the file plausibly holds one.
const apiKeyPrefix = "wv_"

// Path returns the dev key's location: PathEnv when set, else DefaultDir/KeyFile.
func Path() string {
	if v := strings.TrimSpace(os.Getenv(PathEnv)); v != "" {
		return v
	}
	return filepath.Join(DefaultDir, KeyFile)
}

// Load reads the dev API key.
//
// It REFUSES a file any group or other bit is set on, rather than reading it or
// quietly tightening it. The tree already refuses to read a group- or
// world-WRITABLE pack trust-anchors document (packsig.FileAnchors) on the
// grounds that a trust root whose integrity the host is not enforcing should
// fail closed; this file is stronger material — a bearer credential, where
// READABILITY is the leak — so the check covers 0o077 rather than 0o022.
//
// Tightening it silently would be the wrong repair: a key file that has been
// 0644 has already been readable by every account on the box for as long as it
// existed, and chmod would hide that rather than end it. Refusing sends the
// developer back through ProvisionCommand, which mints a FRESH key and revokes
// the exposed one.
func Load() (string, error) {
	path := Path()
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("no dev API key at %s — run `%s` to provision one", path, ProvisionCommand)
	case err != nil:
		return "", fmt.Errorf("stat dev API key %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("dev API key %s is not a regular file", path)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return "", fmt.Errorf("dev API key %s is group- or world-accessible (mode %04o) — delete it and re-run `%s`, which mints a fresh key and revokes this one",
			path, mode, ProvisionCommand)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read dev API key %s: %w", path, err)
	}
	token := strings.TrimSpace(string(raw))
	// No branch below renders `token`. A malformed key is still a credential
	// someone may have meant to keep.
	if token == "" {
		return "", fmt.Errorf("dev API key %s is empty — run `%s`", path, ProvisionCommand)
	}
	if !strings.HasPrefix(token, apiKeyPrefix) || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("dev API key %s does not hold a %s… api-key token — run `%s`", path, apiKeyPrefix, ProvisionCommand)
	}
	return token, nil
}

// Write persists token as the dev key, through the tree's shared secret-file
// discipline: 0600, inside a 0700 directory, installed by rename. It returns the
// path it wrote, so a caller can report WHERE without reporting WHAT.
func Write(token string) (string, error) {
	path := Path()
	// secretfile.Write tightens the ENCLOSING DIRECTORY to 0700, which is right
	// for the directory this package owns and wrong for one a caller pointed
	// PathEnv at. `WAIVEO_DEV_API_KEY_FILE=$HOME/dev-api-key` would silently
	// chmod a home directory; the repo root likewise, and .gitignore explicitly
	// anticipates that placement. A tool does not get to re-permission a
	// directory it did not create, so an overridden path must already be in a
	// directory tight enough to hold a bearer credential — checked, and refused
	// with the mode named, rather than fixed behind the caller's back.
	if path != filepath.Join(DefaultDir, KeyFile) {
		dir := filepath.Dir(path)
		// An ABSENT directory is fine: secretfile.Write creates it at 0700 below,
		// and creating a directory is not re-permissioning someone else's. Only
		// an existing, too-permissive one is refused.
		switch info, err := os.Stat(dir); {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return "", fmt.Errorf("the directory for %s (%s): %w", PathEnv, dir, err)
		case info.Mode().Perm()&0o077 != 0:
			return "", fmt.Errorf("%s points into %s, which is mode %04o — a directory holding a bearer credential must be 0700, and this tool will not re-permission a directory it does not own; tighten it or unset %s",
				PathEnv, dir, info.Mode().Perm(), PathEnv)
		}
	}
	if err := secretfile.Write(path, []byte(token+"\n")); err != nil {
		return "", fmt.Errorf("persist dev API key: %w", err)
	}
	return path, nil
}

// InsecureClient returns the plain loopback dev client every probe in this repo
// needs and NO credential.
//
// The feeder serves an ed25519-leaf, self-signed dev certificate. Skipping
// verification is a probe against a loopback dev process, never a trust
// decision — and the ed25519 leaf is also why these probes are Go rather than
// curl, since some system curl builds (macOS LibreSSL) cannot complete the
// handshake at all.
//
// timeout of 0 means no client-level timeout, for a caller that bounds each
// request with its own context (a long-lived SSE read).
func InsecureClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: insecureTransport(),
	}
}

// Client returns InsecureClient with the dev API key attached as a bearer token
// on every request to a loopback host. It fails if no key is provisioned — see
// the package doc for why that is a refusal rather than an unauthenticated
// attempt.
func Client(timeout time.Duration) (*http.Client, error) {
	token, err := Load()
	if err != nil {
		return nil, err
	}
	c := InsecureClient(timeout)
	c.Transport = &bearerTransport{base: c.Transport, token: token}
	return c, nil
}

func insecureTransport() http.RoundTripper {
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback dev probe, never a trust decision
	}
}

// bearerTransport attaches `Authorization: Bearer <key>` to loopback requests.
//
// It clones the request before touching its headers because http.RoundTripper's
// contract forbids modifying the one it is given — a caller that retried a
// request would otherwise find a header it never set, and a caller that dumped
// its own request for diagnosis would find the credential in the dump.
type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !loopback(r.URL) {
		return t.base.RoundTrip(r)
	}
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// loopback reports whether u addresses this machine over the loopback interface —
// the only destination a dev key is ever presented to. A hostname that is not a
// literal loopback IP is not resolved here: resolution would make the decision
// depend on DNS, and a dev probe has no business sending an admin credential to
// whatever a name currently points at.
func loopback(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
