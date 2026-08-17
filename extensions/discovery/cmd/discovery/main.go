// Command discovery is the Discovery extension's runtime: the first-party
// extension that owns discovery POLICY while the enumeration engine stays in
// core, on the relay.
//
// # What this extension owns, and what it deliberately does not
//
// The relay is the only thing that can see a LAN, and extensions never run on
// the relay (owner's rule) — so the engine that enumerates hosts, listens, and
// probes is core. What is genuinely POLICY lives here:
//
//   - which subnets a scan may probe, on what schedule, and how hard
//     (the `settings` collection and its settings-form page);
//   - the operator's "scan the network now" action, which this process turns
//     into the platform's own scan (POST /api/v1/discovery/scan).
//
// That is the same division the backups pilot draws — the extension owns when
// and how much, core owns the mechanism — and it is what keeps "Discovery is an
// extension" true without moving network code out of the one process that can
// perform it.
//
// # The lifecycle (identical to the reference pilot)
//
//  1. The host writes a one-time tier-grant code on stdin.
//  2. Redeem it at POST {WAIVEO_API_BASE_URL}/api/v1/auth/tier-grant/redeem.
//  3. Long-poll GET /api/v1/pack-invocations/pending for work; answer each
//     invocation at POST /api/v1/pack-invocations/{id}/result.
//  4. EOF on stdin means the host is asking this process to stop.
//
// Redemption IS the readiness signal, and the CA the host hands over
// (WAIVEO_API_CA_FILE) is the only anchor trusted — never InsecureSkipVerify.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	base := os.Getenv("WAIVEO_API_BASE_URL")
	if base == "" {
		fmt.Fprintln(os.Stderr, "discovery: WAIVEO_API_BASE_URL is not set; this process must be started by the pack host")
		os.Exit(2)
	}

	stdin := bufio.NewReader(os.Stdin)
	code, err := stdin.ReadString('\n')
	if err != nil && code == "" {
		fmt.Fprintf(os.Stderr, "discovery: read grant code from stdin: %v\n", err)
		os.Exit(2)
	}
	code = trimEOL(code)

	client, err := newClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "discovery: %v\n", err)
		os.Exit(2)
	}

	sess, err := redeem(client, base, code)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discovery: redeem tier grant: %v\n", err)
		os.Exit(1)
	}

	// From here on, stdin has one meaning: EOF is the host asking us to stop.
	ctx, stop := context.WithCancel(context.Background())
	go func() {
		defer stop()
		_, _ = io.Copy(io.Discard, stdin)
	}()

	if err := serve(ctx, client, base, sess); err != nil {
		fmt.Fprintf(os.Stderr, "discovery: %v\n", err)
		os.Exit(1)
	}
}

// trimEOL strips the line ending without touching the code itself. \r matters:
// a grant piped through anything that writes CRLF would otherwise carry an
// invisible character into a credential (the LF-trim bug the first hardware
// bring-up hit, fixed here by construction).
func trimEOL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// newClient builds the ONE http.Client this pack ever uses. When the host
// supplies a trust anchor, verification runs against exactly that pool; when it
// does not (a deployment with a publicly-trusted certificate, or plain HTTP in
// a test harness), the default transport is already correct.
func newClient() (*http.Client, error) {
	caFile := os.Getenv("WAIVEO_API_CA_FILE")
	if caFile == "" {
		return &http.Client{Timeout: 90 * time.Second}, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read WAIVEO_API_CA_FILE %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("WAIVEO_API_CA_FILE %s holds no usable certificate", caFile)
	}
	return &http.Client{
		Timeout: 90 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}, nil
}

// session is what redemption returns; the fields this pack does not use are
// omitted rather than kept as documentation-by-struct.
type session struct {
	Token  string `json:"token"`
	PackID string `json:"pack_id"`
}

// redeem exchanges the one-time code for the pack's session.
//
// TRANSPORT errors are retried for a bounded window: the host starts packs as
// soon as its listener exists, and a slow boot can still lose the race between
// this dial and the first Accept. The window sits inside the supervisor's
// readiness budget and well inside the grant's own ttl, so retrying never
// outlives either. An HTTP refusal is NOT retried — the request arrived and
// was judged; the code is one-time, and re-presenting a judged code only
// spends redemption-attempt budget on an answer that will not change.
func redeem(client *http.Client, base, code string) (session, error) {
	body, _ := json.Marshal(map[string]string{"code": code})
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := client.Post(base+"/api/v1/auth/tier-grant/redeem", "application/json", bytes.NewReader(body))
		if err != nil {
			// A failed certificate VERIFICATION is transport-shaped and NOT
			// transient: this pack has decided not to trust the peer, and
			// re-dialing a host you refuse to trust is not a retry, it is a
			// loop. Fail immediately so a misconfigured anchor surfaces as one
			// crisp error instead of a silent 15-second stall.
			var certErr *tls.CertificateVerificationError
			if errors.As(err, &certErr) || time.Now().After(deadline) {
				return session{}, fmt.Errorf("redeem: %w", err)
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			return session{}, fmt.Errorf("redeem: %d %s", resp.StatusCode, raw)
		}
		var s session
		if err := json.Unmarshal(raw, &s); err != nil || s.Token == "" || s.PackID == "" {
			return session{}, fmt.Errorf("redeem: unusable session in %s", raw)
		}
		return s, nil
	}
}

// serve is the work loop: lease, perform, report, forever — until the host
// closes stdin (ctx) or the session dies (401).
func serve(ctx context.Context, client *http.Client, base string, sess session) error {
	backoff := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}

		inv, status, err := lease(ctx, client, base, sess.Token)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			backoff = nextBackoff(backoff)
			continue
		case status == http.StatusUnauthorized:
			// The session is gone and the one-time code with it. Exit non-zero:
			// restarting with a fresh grant is the HOST's decision (#191).
			return fmt.Errorf("session rejected (401); exiting for the supervisor")
		case status == http.StatusNoContent:
			backoff = 0
			continue
		case status != http.StatusOK:
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = 0

		result, errCode, errMsg := perform(ctx, client, base, sess, inv)
		if err := report(ctx, client, base, sess.Token, inv.InvocationID, result, errCode, errMsg); err != nil {
			// A lost report is the lease expiring on a recorded verdict (409) or
			// the host going away mid-write. Either way the queue owns the
			// outcome now; there is nothing to retry that could improve it.
			fmt.Fprintf(os.Stderr, "discovery: report %s: %v\n", inv.InvocationID, err)
		}
	}
}

// nextBackoff grows 500ms → 10s and caps there: a pack must survive a host
// restart without hammering it back up, and without going quiet for minutes.
func nextBackoff(cur time.Duration) time.Duration {
	if cur == 0 {
		return 500 * time.Millisecond
	}
	if next := cur * 2; next < 10*time.Second {
		return next
	}
	return 10 * time.Second
}

type invocation struct {
	InvocationID string          `json:"invocation_id"`
	Action       string          `json:"action"`
	Params       json.RawMessage `json:"params"`
}

// lease long-polls the pending queue. `wait=20` holds the request server-side,
// so an idle pack costs one open connection rather than a poll storm.
func lease(ctx context.Context, client *http.Client, base, token string) (invocation, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/pack-invocations/pending?wait=20", nil)
	if err != nil {
		return invocation{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return invocation{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return invocation{}, resp.StatusCode, nil
	}
	var inv invocation
	// The read cap sits WELL ABOVE the server's own 1 MiB invoke-body cap: the
	// lease response re-wraps an accepted invocation's params in a larger
	// envelope, so a cap equal to the server's would truncate the largest legal
	// invocations — a decode error here means the invocation is leased with no
	// verdict ever coming, and safe-to-retry re-queues it into the same choke
	// forever. Sizing the client cap by the SERVER's admission bound (plus
	// headroom) makes that loop unreachable rather than merely unlikely.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&inv); err != nil {
		return invocation{}, 0, err
	}
	if inv.InvocationID == "" {
		return invocation{}, 0, fmt.Errorf("lease: invocation with no id")
	}
	return inv, http.StatusOK, nil
}

// perform routes one invocation to its handler. The manifest declares exactly
// `scan-now`; anything else reaching this queue is a host-side bug, answered as
// a typed error rather than a crash, because the queue must always get its
// verdict back.
func perform(ctx context.Context, client *http.Client, base string, sess session, inv invocation) (json.RawMessage, string, string) {
	switch inv.Action {
	case "scan-now":
		return scanNow(ctx, client, base, sess, inv)
	default:
		return nil, "UNSUPPORTED_ACTION", fmt.Sprintf("this extension declares no handler for %q", inv.Action)
	}
}

// scanNow turns the operator's action into the platform's own scan.
//
// It deliberately does NOT probe anything itself: this process has no LAN
// visibility (it runs beside the app, not on the relay) and extensions never run
// on the relay, so the only correct move is to ask core, which asks each relay.
// The extension's contribution is the DECISION and the policy scope it carries.
//
// The response is an ACCEPTANCE per relay, not findings — a scan outlives this
// call, and its results arrive through the ordinary device reporting path. So
// the result recorded here is what was accepted, which is what a retry of a
// safe-to-retry action must be able to report identically.
func scanNow(ctx context.Context, client *http.Client, base string, sess session, inv invocation) (json.RawMessage, string, string) {
	// The scan is scoped by the policy this extension owns. An empty subnet
	// means "each relay's own default scope", which is the only scope a relay
	// can honour without being told about a network it cannot see.
	body := []byte(`{}`)
	if subnet := strings.TrimSpace(scanSubnet(ctx, client, base, sess)); subnet != "" {
		enc, err := json.Marshal(map[string]string{"subnet": subnet})
		if err == nil {
			body = enc
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/discovery/scan", bytes.NewReader(body))
	if err != nil {
		return nil, "SCAN_FAILED", err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+sess.Token)
	req.Header.Set("Content-Type", "application/json")
	// The invocation id is the idempotency key: a safe-to-retry action replayed
	// by the host must not start a second sweep of the segment.
	req.Header.Set("Idempotency-Key", inv.InvocationID)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "SCAN_FAILED", err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, "SCAN_FAILED", fmt.Sprintf("the platform refused the scan: %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.RawMessage(raw), "", ""
}

// scanSubnet reads the operator's configured scope from this extension's own
// settings collection. A read that fails is NOT an error the action reports: the
// scan still runs at each relay's default scope, which is the safer of the two
// outcomes — refusing to scan because a policy row could not be read would make
// a missing setting look like a broken engine.
func scanSubnet(ctx context.Context, client *http.Client, base string, sess session) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/api/v1/packs/"+sess.PackID+"/data/settings", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+sess.Token)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return ""
	}
	var doc struct {
		Subnets string `json:"subnets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return ""
	}
	return doc.Subnets
}

func tarGz(name string, data []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(data); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// report posts the invocation's verdict. Exactly one of result / error fields
// is sent: a result key on a failed invocation would be a field the host has to
// learn to ignore.
func report(ctx context.Context, client *http.Client, base, token, invocationID string, result json.RawMessage, errCode, errMsg string) error {
	body := map[string]any{}
	if errCode != "" {
		body["error_code"] = errCode
		body["error_message"] = errMsg
	} else {
		body["result"] = result
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/api/v1/pack-invocations/"+invocationID+"/result", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("result refused: %d %s", resp.StatusCode, out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
