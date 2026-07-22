// Command webuiprobe is the `make dev` web-UI check: it confirms the feeder
// serves the embedded console SPA shell at "/". Like the other dev probes it is
// written in Go on purpose — the feeder serves an ed25519-leaf TLS cert some
// system curl builds (macOS LibreSSL) cannot handshake, so a curl probe would
// spuriously fail against a healthy server. Go's TLS stack handles ed25519.
//
// It GETs https://127.0.0.1:7420/ and asserts the response is the SPA shell: a
// 200 text/html body carrying the shell's <title> marker. The same assertion
// holds whether the embed tree is the committed placeholder or the real Vite
// build (both carry the "Waiveo" title), so the smoke exercises the real build
// when `make web-build` has populated it and the placeholder otherwise.
//
// Endpoint (dev tooling only, not a contract surface):
//   - feeder: https://127.0.0.1:7420/  (self-signed dev cert → skip verify)
//
// Retried for ~10s (cold Go start / no start ordering between the backgrounded
// binaries). Exits 0 on "WEB UI OK", non-zero otherwise.
package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	feederRoot = "https://127.0.0.1:7420/"
	// The SPA shell title marker — present in both the committed placeholder and
	// the real Vite build (both derive from web/index.html's <title>).
	titleMarker = "<title>Waiveo</title>"
)

func main() {
	// The feeder's dev cert is self-signed; skipping verification here is a probe
	// against a loopback dev process, never a trust decision.
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}

	if err := probe(client); err != nil {
		fmt.Fprintf(os.Stderr, "WEB UI FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("WEB UI OK (SPA served from the feeder at /)")
}

func probe(c *http.Client) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := get(c)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("feeder did not serve the SPA shell at %s after ~10s: %v", feederRoot, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func get(c *http.Client) error {
	resp, err := c.Get(feederRoot)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		return fmt.Errorf("content-type %q, want text/html", ct)
	}
	if !strings.Contains(string(body), titleMarker) {
		return fmt.Errorf("shell missing title marker %q", titleMarker)
	}
	return nil
}
