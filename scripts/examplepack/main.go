// Command examplepack writes an in-repo example pack to a SIGNED zip artifact
// on disk — the `make example-pack` build target. The bytes it writes are what
// `make dev`'s pack smoke and the end-to-end test install over the API (all
// read the one source of truth, examples/packs, and sign with the same
// make-dev publisher key). It is a build helper, not a contract surface.
//
// Signing is not optional: the install pipeline verifies every artifact's
// signature envelope (internal/packsig), so an unsigned zip would be refused
// at POST /api/v1/packs. This tool provisions (once) the make-dev publisher
// keypair under the git-ignored key dir, ensures the feeder's trust-anchors
// document authorizes it for the pack's own publisher namespace, and signs the
// artifact as the identity the bundled manifest declares.
//
// A pack whose manifest declares `runtime` (MAN-065) carries a COMPILED entry.
// The convention that keeps this tool the only packaging path: the entry's Go
// source lives at examples/packs/<pack>/cmd/<base of runtime.entry>/, in the
// main module. This tool `go build`s it (CGO disabled, GOOS/GOARCH honoured
// from the environment) and injects the binary at the entry path. Per owner
// decision #190 an artifact is single-architecture — one signed zip per arch —
// so the output names the platform it was built for.
//
// Usage: go run ./scripts/examplepack -pack backups -out path/to/backups.pack.zip
// (run from the repo root, which is where `make example-pack` runs it).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

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

	entry, extra, platform, err := buildRuntimeEntry(*pack)
	if err != nil {
		fmt.Fprintf(os.Stderr, "examplepack: %v\n", err)
		os.Exit(1)
	}

	art, err := examplepacks.PackZipWithFiles(*pack, extra)
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
	if entry != "" {
		fmt.Printf("wrote %s (%d bytes): %s@%s signed by %s, %s at %s\n",
			*out, len(signed), id, version, keyID, platform, entry)
		return
	}
	fmt.Printf("wrote %s (%d bytes): %s@%s signed by %s\n", *out, len(signed), id, version, keyID)
}

// buildRuntimeEntry compiles a code-carrying pack's entry, returning the entry
// path, the extra artifact files to inject, and the platform the binary was
// built for. A purely declarative pack returns all zeros: nothing to build is
// the normal case, not an error.
func buildRuntimeEntry(pack string) (entry string, extra map[string][]byte, platform string, err error) {
	rawManifest, err := examplepacks.PackFile(pack, "manifest.json")
	if err != nil {
		return "", nil, "", fmt.Errorf("read %s manifest: %w", pack, err)
	}
	var doc struct {
		Runtime *struct {
			Entry string `json:"entry"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(rawManifest, &doc); err != nil {
		return "", nil, "", fmt.Errorf("decode %s manifest: %w", pack, err)
	}
	if doc.Runtime == nil {
		return "", nil, "", nil
	}
	entry = doc.Runtime.Entry

	// The source-location convention, spelled in one place: the entry package
	// sits under the pack's own tree, named after the binary it becomes.
	src := "./" + path.Join("examples/packs", pack, "cmd", path.Base(entry))
	if _, statErr := os.Stat(filepath.FromSlash(src)); statErr != nil {
		return "", nil, "", fmt.Errorf("%s declares runtime.entry %q but has no source at %s (run from the repo root; the entry's package lives at examples/packs/<pack>/cmd/<entry base>): %w",
			pack, entry, src, statErr)
	}

	tmp, err := os.MkdirTemp("", "examplepack-entry")
	if err != nil {
		return "", nil, "", err
	}
	defer os.RemoveAll(tmp)
	bin := filepath.Join(tmp, path.Base(entry))

	// -s -w strips the symbol and DWARF tables: a distributed binary is not a
	// debug target, and the artifact every box downloads should not carry one.
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", bin, src)
	// CGO off: a pack binary must not pick up a dynamic libc dependency by
	// accident — the appliance image is the wrong place to discover one.
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
		return "", nil, "", fmt.Errorf("build %s: %v\n%s", src, buildErr, out)
	}
	code, err := os.ReadFile(bin)
	if err != nil {
		return "", nil, "", err
	}

	// Asked of the toolchain rather than read from this process's env or its
	// own runtime: `go env` folds in every configuration source (env vars AND
	// `go env -w`), so the platform named in the output is the platform the
	// binary was actually built for.
	platform = buildPlatform()
	return entry, map[string][]byte{entry: code}, platform, nil
}

// buildPlatform reports the toolchain's effective GOOS/GOARCH, falling back to
// this process's own on any error — a reporting nicety must never fail a build.
// One `go env` invocation per variable: the names are queried individually so
// no call site carries a shape the error-code scanner reads as an emission.
func buildPlatform() string {
	goos := goEnvOr("GOOS", runtime.GOOS)
	goarch := goEnvOr("GOARCH", runtime.GOARCH)
	return goos + "/" + goarch
}

func goEnvOr(name, fallback string) string {
	out, err := exec.Command("go", "env", name).Output()
	v := strings.TrimSpace(string(out))
	if err != nil || v == "" {
		return fallback
	}
	return v
}
