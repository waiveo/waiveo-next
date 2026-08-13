// Command examplepack writes the in-repo menu-board example pack to a SIGNED
// zip artifact on disk — the `make example-pack` build target. The bytes it
// writes are what `make dev`'s pack smoke and the end-to-end test install over
// the API (all read the one source of truth, examples/packs, and sign with the
// same make-dev publisher key). It is a build helper, not a contract surface.
//
// Signing is not optional: the install pipeline verifies every artifact's
// signature envelope (internal/packsig), so an unsigned zip would be refused
// at POST /api/v1/packs. This tool provisions (once) the make-dev publisher
// keypair under the git-ignored key dir, ensures the feeder's trust-anchors
// document authorizes it for the pack's own publisher namespace, and signs the
// artifact as the identity the bundled manifest declares.
//
// Usage: go run ./scripts/examplepack -out path/to/menu-board.pack.zip
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	examplepacks "github.com/maaxton/waiveo-next/examples/packs"
	"github.com/maaxton/waiveo-next/internal/packsig"
)

func main() {
	out := flag.String("out", "", "path to write the signed example-pack zip to (required)")
	pack := flag.String("pack", "menu-board", "which in-repo example pack to build (examples/packs/<name>)")
	keyDir := flag.String("key-dir", ".dev/pack-publisher",
		"directory the make-dev publisher signing keypair persists in (created on first run)")
	anchors := flag.String("anchors", ".dev/pack-trust/anchors.json",
		"trust-anchors document to provision the publisher key into (what the feeder verifies against)")
	flag.Parse()

	if *out == "" {
		fmt.Fprintln(os.Stderr, "examplepack: -out is required")
		os.Exit(2)
	}

	art, err := examplepacks.PackZip(*pack)
	if err != nil {
		fmt.Fprintf(os.Stderr, "examplepack: build zip: %v\n", err)
		os.Exit(1)
	}

	id, version, err := packsig.ArtifactIdentity(art)
	if err != nil {
		fmt.Fprintf(os.Stderr, "examplepack: read pack identity: %v\n", err)
		os.Exit(1)
	}
	ns, err := packsig.Namespace(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "examplepack: %v\n", err)
		os.Exit(1)
	}
	keyID, priv, err := packsig.DevProvision(*keyDir, *anchors, ns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "examplepack: provision publisher key: %v\n", err)
		os.Exit(1)
	}
	signed, err := packsig.Sign(art, id, version, keyID, priv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "examplepack: sign artifact: %v\n", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "examplepack: mkdir: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, signed, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "examplepack: write: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes): %s@%s signed by %s\n", *out, len(signed), id, version, keyID)
}
