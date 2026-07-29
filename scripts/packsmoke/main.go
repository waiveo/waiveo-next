// Command packsmoke is the `make dev` declarative-pack check: it installs the
// in-repo example pack (examples/packs/menu-board) through the REAL POST
// /api/v1/packs on the running feeder — the same manifest engine that gates a
// third party's upload — and asserts the install summary (the pack identity, its
// two pages, and its menu_items collection). It proves a declarative extension
// installs end to end on the live stack; NOTHING in the pack executes — a pack is
// data (a manifest, page documents, a locale catalog).
//
// AUTHENTICATION: every /api/v1 route is authenticated (security-model/1
// SEC-005), and the pack lifecycle routes ask for more than the method floor —
// `admin` at the workspace org node, since a pack's capabilities are granted
// workspace-wide. The probe presents the local dev API key through
// scripts/devcred, whose package doc carries the provisioning path (`make
// dev-key`) and the argument for the authority that key holds. This probe is
// the reason that authority is `admin` rather than `operator`.
//
// Like the other dev probes it is written in Go on purpose: the feeder serves an
// ed25519/ECDSA self-signed leaf a Go client handshakes cleanly (a curl upload
// could spuriously fail against a healthy server). The artifact it uploads is
// byte-identical to what `make example-pack` writes (one source of truth,
// examples/packs; one make-dev publisher key, so even the signature envelope
// matches — ed25519 signing and the zip rewrite are both deterministic).
//
// Endpoint (the real api/1 surface): POST https://127.0.0.1:7420/api/v1/packs.
// A fresh `make dev` seeds an empty store, so the first install is a 201; a
// re-run against a pre-installed pack is a 200 (updated in place). Both are OK —
// the install succeeded either way. Retried for ~10s (no start ordering between
// the backgrounded binaries). Exits 0 on "PACK OK", non-zero otherwise.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	examplepacks "github.com/maaxton/waiveo-next/examples/packs"
	"github.com/maaxton/waiveo-next/internal/packsig"
	"github.com/maaxton/waiveo-next/scripts/devcred"
)

const packsEndpoint = "https://127.0.0.1:7420/api/v1/packs"

// The make-dev publisher key dir and the feeder's trust-anchors document —
// the same paths `make example-pack` provisions and the feeder's default
// config reads (the feeder re-reads the anchors file per verification, so
// provisioning here takes effect on an already-running feeder).
const (
	devKeyDir      = ".dev/pack-publisher"
	devAnchorsPath = ".dev/pack-trust/anchors.json"
)

// installSummary is the subset of the install response (packs.Result) this probe
// asserts on and renders into the PACK OK line.
type installSummary struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	Pages       []string `json:"pages"`
	Collections []string `json:"collections"`
}

func main() {
	art, err := examplepacks.MenuBoardZip()
	if err != nil {
		fmt.Fprintf(os.Stderr, "PACK FAIL: build example pack zip: %v\n", err)
		os.Exit(1)
	}

	// Sign the artifact with the make-dev publisher key (provisioned into the
	// feeder's trust anchors on first use) — the install pipeline verifies every
	// artifact's signature envelope, so an unsigned upload is refused by design.
	id, version, err := packsig.ArtifactIdentity(art)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PACK FAIL: read pack identity: %v\n", err)
		os.Exit(1)
	}
	ns, err := packsig.Namespace(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PACK FAIL: %v\n", err)
		os.Exit(1)
	}
	keyID, priv, err := packsig.DevProvision(devKeyDir, devAnchorsPath, ns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PACK FAIL: provision publisher key: %v\n", err)
		os.Exit(1)
	}
	if art, err = packsig.Sign(art, id, version, keyID, priv); err != nil {
		fmt.Fprintf(os.Stderr, "PACK FAIL: sign artifact: %v\n", err)
		os.Exit(1)
	}

	client, err := devcred.Client(5 * time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PACK FAIL: %v\n", err)
		os.Exit(1)
	}

	summary, err := install(client, art)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PACK FAIL: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("PACK OK (%s v%s: %d pages, %s collection)\n",
		summary.ID, summary.Version, len(summary.Pages), strings.Join(summary.Collections, ", "))
}

// install POSTs the artifact to the real packs endpoint, retrying for ~10s while
// the feeder finishes coming up. A 201 (fresh install) or 200 (reinstall in
// place) is success; anything else — including a 422 manifest refusal — is a
// failure carrying the server's body for diagnosis.
func install(c *http.Client, artifact []byte) (installSummary, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		summary, retryable, err := attempt(c, artifact)
		if err == nil {
			return summary, nil
		}
		if !retryable || time.Now().After(deadline) {
			return installSummary{}, err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// attempt makes one install request. retryable is true only for a transport error
// (the feeder is not yet listening); a reachable server that answers non-2xx is a
// definitive failure (retrying would not help).
func attempt(c *http.Client, artifact []byte) (summary installSummary, retryable bool, err error) {
	req, err := http.NewRequest(http.MethodPost, packsEndpoint, bytes.NewReader(artifact))
	if err != nil {
		return installSummary{}, false, err
	}
	req.Header.Set("Content-Type", "application/zip")

	resp, err := c.Do(req)
	if err != nil {
		return installSummary{}, true, err // feeder not up yet — retry
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return installSummary{}, false, fmt.Errorf("install returned %d (want 201|200): %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &summary); err != nil {
		return installSummary{}, false, fmt.Errorf("decode install summary: %v (%s)", err, body)
	}
	if summary.ID != "waiveo/menu-board" || len(summary.Pages) != 2 || len(summary.Collections) != 1 {
		return installSummary{}, false, fmt.Errorf("unexpected install summary: %+v", summary)
	}
	return summary, false, nil
}
