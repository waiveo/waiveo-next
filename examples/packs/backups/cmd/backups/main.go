// Command backups is the pilot extension's runtime entry — the FIRST pack
// process this platform runs, and therefore the REFERENCE every future pack
// copies. It is deliberately small and deliberately complete: every step below
// is a step no pack can skip, and nothing below is specific to backups.
//
// The lifecycle, in the order it happens:
//
//  1. Read ONE line from standard input: the one-time tier-grant code. SEC-037
//     keeps it off the environment (an env var is readable from the process
//     table for the life of the process; stdin is read once and gone).
//  2. Redeem it at POST {WAIVEO_API_BASE_URL}/api/v1/auth/tier-grant/redeem.
//     Redemption IS the readiness signal — the supervisor watches for the grant
//     to be consumed, so a pack that dawdles here is a pack that never starts.
//  3. Keep the returned bearer token. It is the pack's ONLY credential and it
//     dies with this process; there is nothing to persist and nowhere to
//     persist it.
//  4. Long-poll GET /api/v1/extension-invocations/pending for work; answer each
//     invocation at POST /api/v1/extension-invocations/{id}/result.
//
// TLS: the feeder usually serves HTTPS with a self-signed certificate, and the
// host hands this process the anchor to trust as WAIVEO_API_CA_FILE. The file
// is loaded into a CertPool and verification stays ON. Do not copy a pack that
// uses InsecureSkipVerify; this one exists so that none has to.
//
// Shutdown: the host closes this process's stdin. Stdin is therefore drained in
// the background after the grant line, and EOF means "stop": finish nothing,
// exit 0. An expired session (401) exits NON-ZERO instead — the pack cannot
// re-redeem a one-time code, so dying honestly is the only correct move (the
// supervisor's restart policy is tracker #191, deliberately not invented here).
//
// This pilot is a REFERENCE, not the backups extraction: run-backup archives
// the pack's own settings collection — a real authenticated read, a real
// archive, a real digest — without touching the platform's archive engine.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	base := os.Getenv("WAIVEO_API_BASE_URL")
	if base == "" {
		fmt.Fprintln(os.Stderr, "backups: WAIVEO_API_BASE_URL is not set; this process must be started by the pack host")
		os.Exit(2)
	}

	stdin := bufio.NewReader(os.Stdin)
	code, err := stdin.ReadString('\n')
	if err != nil && code == "" {
		fmt.Fprintf(os.Stderr, "backups: read grant code from stdin: %v\n", err)
		os.Exit(2)
	}
	code = trimEOL(code)

	client, err := newClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "backups: %v\n", err)
		os.Exit(2)
	}

	sess, err := redeem(client, base, code)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backups: redeem tier grant: %v\n", err)
		os.Exit(1)
	}

	// From here on, stdin has one meaning: EOF is the host asking us to stop.
	ctx, stop := context.WithCancel(context.Background())
	go func() {
		defer stop()
		_, _ = io.Copy(io.Discard, stdin)
	}()

	if err := serve(ctx, client, base, sess); err != nil {
		fmt.Fprintf(os.Stderr, "backups: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "backups: report %s: %v\n", inv.InvocationID, err)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/extension-invocations/pending?wait=20", nil)
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
// `run-backup`; anything else reaching this queue is a host-side bug, answered
// as a typed error rather than a crash, because the queue must always get its
// verdict back.
func perform(ctx context.Context, client *http.Client, base string, sess session, inv invocation) (json.RawMessage, string, string) {
	switch inv.Action {
	case "run-backup":
		return runBackup(ctx, client, base, sess, inv)
	default:
		return nil, "UNSUPPORTED_ACTION", fmt.Sprintf("this pack declares no handler for %q", inv.Action)
	}
}

// runBackup is the reference action: read the pack's OWN settings collection
// through the authenticated API, archive the response, and report the archive's
// name, size and digest. Real work end to end — an authenticated read, real
// bytes, a verifiable digest — with no coupling to the platform's archive
// engine, which stays in core (that extraction is a later, owner-scoped job).
//
// The archive is named after the invocation id, not the clock: two runs of a
// safe-to-retry action on the same invocation must not race each other into
// differently-named results.
func runBackup(ctx context.Context, client *http.Client, base string, sess session, inv invocation) (json.RawMessage, string, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/api/v1/extensions/"+sess.PackID+"/data/settings", nil)
	if err != nil {
		return nil, "BACKUP_FAILED", err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+sess.Token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "BACKUP_FAILED", fmt.Sprintf("read settings: %v", err)
	}
	defer resp.Body.Close()
	settings, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, "BACKUP_FAILED", fmt.Sprintf("read settings: %d %s", resp.StatusCode, settings)
	}

	archive, err := tarGz("settings.json", settings)
	if err != nil {
		return nil, "BACKUP_FAILED", fmt.Sprintf("assemble archive: %v", err)
	}
	sum := sha256.Sum256(archive)
	result, _ := json.Marshal(map[string]any{
		"archive": "backup-" + inv.InvocationID + ".tar.gz",
		"bytes":   len(archive),
		"sha256":  hex.EncodeToString(sum[:]),
	})
	return result, "", ""
}

// tarGz builds a one-file tar.gz in memory. Header times are zero so the same
// input yields the same bytes — a reproducible archive is one whose digest
// means something.
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
		base+"/api/v1/extension-invocations/"+invocationID+"/result", bytes.NewReader(raw))
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
