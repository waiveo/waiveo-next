// Command devsmoke is the `make dev` health check: it confirms the feeder and
// relay booted and answer /healthz. It is written in Go on purpose — the feeder
// serves its relay/1 surface over an ed25519-leaf TLS cert, and some system
// curl builds (e.g. macOS LibreSSL) cannot complete an ed25519 TLS handshake,
// so a curl-based probe would spuriously fail against a perfectly healthy
// server. Go's TLS stack handles ed25519, matching the all-Go, all-ed25519
// stack this checks.
//
// Endpoints (dev tooling only, not contract surfaces):
//   - feeder: https://127.0.0.1:7420/healthz  (self-signed dev cert → skip verify)
//   - relay:  http://127.0.0.1:7421/healthz
//
// Each endpoint is retried for ~10s (cold Go start / no start ordering between
// the backgrounded binaries). Exits 0 on "SMOKE OK", non-zero otherwise.
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

type target struct {
	name string
	url  string
}

func main() {
	// The feeder's dev cert is self-signed; skipping verification here is a
	// health probe against a loopback dev process, never a trust decision.
	insecure := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	plain := &http.Client{Timeout: 3 * time.Second}

	targets := []struct {
		target
		client *http.Client
	}{
		{target{"feeder", "https://127.0.0.1:7420/healthz"}, insecure},
		{target{"relay", "http://127.0.0.1:7421/healthz"}, plain},
	}

	for _, t := range targets {
		if err := probe(t.client, t.name, t.url); err != nil {
			fmt.Fprintf(os.Stderr, "SMOKE FAIL: %v\n", err)
			os.Exit(1)
		}
	}
	fmt.Println("SMOKE OK")
}

func probe(c *http.Client, name, url string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		body, err := get(c, url)
		if err == nil {
			if !strings.Contains(body, `"status":"ok"`) {
				return fmt.Errorf("%s wrong payload: %s", name, body)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no listener at %s after ~10s (%v)", url, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func get(c *http.Client, url string) (string, error) {
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return string(b), nil
}
