// Command contentloop is the `make dev` content-pipeline smoke: it exercises the
// live feeder's content origin end to end over real HTTP — upload an asset to
// POST /api/v1/content, then fetch it back from the url the server returns and
// verify content-addressing holds (the fetched bytes hash to the asset_ref the
// server computed). That is the loop a real screen walks: an operator uploads an
// asset, and a screen fetches those exact bytes direct from the content origin —
// the relay is never in this data path (REL-140).
//
// It is written in Go on purpose, like scripts/devsmoke: the feeder serves an
// ed25519-leaf TLS cert some system curl builds (macOS LibreSSL) cannot
// handshake, so a curl probe would spuriously fail against a healthy server.
// Go's TLS stack handles ed25519, matching the all-Go, all-ed25519 stack.
//
// The feeder's own /healthz is already confirmed up by scripts/devsmoke before
// this runs; a short retry on the upload absorbs any residual cold-start race.
// Exits 0 on "CONTENT LOOP OK", non-zero (with a diagnostic) otherwise.
package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// feederAPIBase is the loopback dev feeder (matching scripts/devsmoke and
// scripts/authoringdemo); the content upload + serve both hang off it.
const feederAPIBase = "https://127.0.0.1:7420"

// uploadResponse is the 201 body POST /api/v1/content returns: the server-computed
// content-addressed asset_ref and the direct-fetch url a screen resolves against
// the content origin (relay/1 REL-061).
type uploadResponse struct {
	AssetRef string `json:"asset_ref"`
	URL      string `json:"url"`
}

func main() {
	// The feeder's dev cert is self-signed; skipping verification here is a probe
	// against a loopback dev process, never a trust decision.
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}

	if err := run(client); err != nil {
		fmt.Fprintf(os.Stderr, "CONTENT LOOP FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(client *http.Client) error {
	// A distinctive, run-stable asset (no secrets). Its asset_ref is the sha256 of
	// these exact bytes — the server MUST compute the same, never trust a client ref.
	asset := []byte("waiveo-next make-dev content-loop asset — the quick brown fox jumps over the lazy dog")
	wantRef := "sha256:" + hexSHA256(asset)

	// 1. Upload the asset (retry ~10s to absorb any residual cold-start race).
	up, err := uploadWithRetry(client, asset)
	if err != nil {
		return err
	}
	if up.AssetRef != wantRef {
		return fmt.Errorf("upload asset_ref = %q, want server-computed %q", up.AssetRef, wantRef)
	}
	if up.URL == "" {
		return fmt.Errorf("upload returned an empty url")
	}

	// 2. Fetch the asset back from the url the server returned — the GET a screen
	// performs against the content origin — and verify content-addressing.
	fetched, err := get(client, up.URL)
	if err != nil {
		return fmt.Errorf("fetch resolved url %s: %w", up.URL, err)
	}
	if !bytes.Equal(fetched, asset) {
		return fmt.Errorf("fetched %d bytes != uploaded %d bytes from %s", len(fetched), len(asset), up.URL)
	}
	if got := "sha256:" + hexSHA256(fetched); got != wantRef {
		return fmt.Errorf("fetched-bytes content id = %q, want %q (content-addressing broken)", got, wantRef)
	}

	fmt.Printf("CONTENT LOOP OK (uploaded %d bytes, fetched them back from %s, sha256 verified)\n", len(asset), up.URL)
	return nil
}

// uploadWithRetry POSTs asset to /api/v1/content, retrying for ~10s, and decodes
// the 201 response.
func uploadWithRetry(client *http.Client, asset []byte) (uploadResponse, error) {
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for {
		up, err := upload(client, asset)
		if err == nil {
			return up, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return uploadResponse{}, fmt.Errorf("no successful upload after ~10s: %w", lastErr)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func upload(client *http.Client, asset []byte) (uploadResponse, error) {
	resp, err := client.Post(feederAPIBase+"/api/v1/content", "application/octet-stream", bytes.NewReader(asset))
	if err != nil {
		return uploadResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return uploadResponse{}, err
	}
	if resp.StatusCode != http.StatusCreated {
		return uploadResponse{}, fmt.Errorf("upload status %d: %s", resp.StatusCode, body)
	}
	var up uploadResponse
	if err := json.Unmarshal(body, &up); err != nil {
		return uploadResponse{}, fmt.Errorf("decode upload response: %w (body %s)", err, body)
	}
	return up, nil
}

func get(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
