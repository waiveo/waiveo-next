// Command pairprobe is a dev-lab probe for the screen pairing path: handed a
// pairing code (the one the console's Screens page displays, or the one the
// relay logs), it stands in for a screen and proves the code works end to end.
//
// Two modes, because they answer two different questions:
//
//	pairprobe <code>          — the OBSERVABLE mode: decodes the code (the
//	  shared paircode codec), redeems it against the relay it dials, and
//	  prints what redemption actually returned — the screen_id (which, for a
//	  screen-bound grant, is the app's own screen identity row id, REL-121a)
//	  — then pulls a program with the minted channel token and prints the
//	  lease's own screen_id/program_revision/content. TLS verification is
//	  SKIPPED in this mode (it prints, it does not trust) — the mode below is
//	  the one that exercises the real trust path.
//
//	pairprobe -photon <code>  — the TRUST-PATH mode: runs the real virtual
//	  player (internal/virtualplayer.Photon): commitment-verified bootstrap
//	  (PLY-052 — the code's own fingerprint_commitment against the served
//	  cert), redemption, pinned re-dial, lease signature verification,
//	  content fetch, lease ack. Prints the fetched content's size and digest.
//
// Endpoint comes from the code itself (PLY-024: a pairing code alone must be
// enough to reach a relay). Dev tooling only, not a contract surface.
package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/paircode"
	"github.com/maaxton/waiveo-next/internal/virtualplayer"
)

func main() {
	photon := flag.Bool("photon", false, "run the real virtual player (commitment-verified trust path) instead of the observable probe")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: pairprobe [-photon] <pairing-code>")
		os.Exit(2)
	}
	code := flag.Arg(0)

	if *photon {
		runPhoton(code)
		return
	}
	runProbe(code)
}

// runPhoton drives the full virtual player. Everything it verifies (the
// commitment, the pinned re-dial, the lease signature, the content digest) is
// verified INSIDE Photon — a non-nil error is the probe's failure.
func runPhoton(code string) {
	bytes, err := virtualplayer.Photon(code)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pairprobe: photon: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("photon OK: fetched %d content byte(s), sha256 %x\n", len(bytes), sha256.Sum256(bytes))
}

// runProbe decodes, redeems, and pulls once, printing each step's real result.
func runProbe(code string) {
	host, port, selector, commitment, err := paircode.Decode(code)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pairprobe: decode: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("decoded: dial=%s grant_selector=%s fingerprint_commitment=%s\n",
		net.JoinHostPort(host, strconv.Itoa(port)), selector, hex.EncodeToString(commitment))

	base := "https://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/player/v1"
	// Observable mode: no verification (see package doc) — -photon is the
	// mode that performs PLY-052's commitment check for real.
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // #nosec G402 — dev probe, prints only
	}

	pairBody, _ := json.Marshal(map[string]any{
		"hardware_id":    "pairprobe",
		"grant_selector": selector,
		"capabilities":   map[string]any{"content_types": []string{"image", "video"}, "player_version": "pairprobe"},
	})
	resp, err := client.Post(base+"/pair", "application/json", bytes.NewReader(pairBody))
	if err != nil {
		fmt.Fprintf(os.Stderr, "pairprobe: POST /pair: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var pair struct {
		PairingStatus string `json:"pairing_status"`
		ChannelToken  string `json:"channel_token"`
		ScreenID      string `json:"screen_id"`
		IssuedAt      int64  `json:"issued_at"`
		ExpiresAt     int64  `json:"expires_at"`
		Code          string `json:"code"`
		Detail        string `json:"detail"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pair); err != nil {
		fmt.Fprintf(os.Stderr, "pairprobe: decode /pair response: %v\n", err)
		os.Exit(1)
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "pairprobe: /pair refused: %d %s %s\n", resp.StatusCode, pair.Code, pair.Detail)
		os.Exit(1)
	}
	fmt.Printf("redeemed: pairing_status=%s screen_id=%s channel_token=%s… expires_at=%s\n",
		pair.PairingStatus, pair.ScreenID, truncate(pair.ChannelToken, 12), time.UnixMilli(pair.ExpiresAt).Format(time.RFC3339))

	pullBody, _ := json.Marshal(map[string]any{
		"capabilities": map[string]any{"content_types": []string{"image", "video"}, "player_version": "pairprobe"},
	})
	req, _ := http.NewRequest(http.MethodGet, base+"/program", bytes.NewReader(pullBody))
	req.Header.Set("Authorization", "Bearer "+pair.ChannelToken)
	req.Header.Set("Content-Type", "application/json")
	presp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pairprobe: GET /program: %v\n", err)
		os.Exit(1)
	}
	defer presp.Body.Close()
	var lease struct {
		LeaseID         string `json:"lease_id"`
		ScreenID        string `json:"screen_id"`
		ProgramRevision string `json:"program_revision"`
		Display         string `json:"display"`
		Content         []struct {
			Type     string `json:"type"`
			AssetRef string `json:"asset_ref"`
			URL      string `json:"url"`
		} `json:"content"`
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(presp.Body).Decode(&lease); err != nil {
		fmt.Fprintf(os.Stderr, "pairprobe: decode /program response: %v\n", err)
		os.Exit(1)
	}
	if presp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "pairprobe: /program refused: %d %s %s\n", presp.StatusCode, lease.Code, lease.Detail)
		os.Exit(1)
	}
	fmt.Printf("lease: screen_id=%s program_revision=%s display=%s content_items=%d\n",
		lease.ScreenID, lease.ProgramRevision, lease.Display, len(lease.Content))
	for _, c := range lease.Content {
		fmt.Printf("  content: type=%s asset_ref=%s url=%s\n", c.Type, c.AssetRef, c.URL)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
