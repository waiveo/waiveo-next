// Command examplepack writes the in-repo menu-board example pack to a zip file on
// disk — the `make example-pack` build target. The bytes it writes are identical
// to what `make dev`'s pack smoke and the end-to-end test install over the API
// (all three read the one source of truth, examples/packs). It is a build helper,
// not a contract surface.
//
// Usage: go run ./scripts/examplepack -out path/to/menu-board.pack.zip
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	examplepacks "github.com/maaxton/waiveo-next/examples/packs"
)

func main() {
	out := flag.String("out", "", "path to write the example-pack zip to (required)")
	flag.Parse()

	if *out == "" {
		fmt.Fprintln(os.Stderr, "examplepack: -out is required")
		os.Exit(2)
	}

	art, err := examplepacks.MenuBoardZip()
	if err != nil {
		fmt.Fprintf(os.Stderr, "examplepack: build zip: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "examplepack: mkdir: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, art, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "examplepack: write: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes): waiveo/menu-board\n", *out, len(art))
}
