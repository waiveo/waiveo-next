package main

import (
	"go/build"
	"path/filepath"
	"strings"
	"testing"
)

// offappliance_test.go is the machine-checked statement of the one constraint
// that governs the rasterized fallback's whole design: THE APPLIANCE NEVER
// RASTERIZES.
//
// waiveo-next's two shipped binaries are deliberately Go-only, and the box this
// platform replaces already carries the legacy stack — a second headless
// Chromium on a Pi is not a performance question, it is a non-starter. So the
// renderer (internal/derive, cmd/waiveo-derive) is a separate off-appliance unit
// and the appliance's entire share of the loop is a read-only work queue.
//
// That is a design intention, and design intentions decay. The specific way this
// one would decay is easy to picture and impossible to see in review: somebody
// wants the feeder to "just render the pending ones itself", imports
// internal/derive from an api handler, and the feeder binary quietly grows a
// browser dependency and a subprocess launcher. Every test still passes. The
// binary still builds. The Pi falls over in the field.
//
// So the constraint is asserted from the actual IMPORT GRAPH rather than from a
// comment. It walks the transitive first-party imports of the two shipped
// binaries and fails if the renderer, or its browser transport, is reachable
// from either.

// forbiddenFromTheAppliance are the packages a shipped binary must never reach.
//
// internal/derive is the rasterizer. internal/derive/qr is listed too even
// though a pure-Go QR encoder would be harmless on the appliance: it is
// reachable only THROUGH the renderer today, so its appearance in this graph
// means somebody has started pulling the rasterizer apart to get one piece of it
// onto the box, which is the first half of exactly the change this test exists
// to stop. If a future need is genuinely "the appliance should generate QR
// matrices", that is a deliberate move of the package out from under
// internal/derive and a deliberate edit here — not a silent import.
var forbiddenFromTheAppliance = []string{
	"github.com/maaxton/waiveo-next/internal/derive",
	"github.com/maaxton/waiveo-next/internal/derive/qr",
}

const modulePrefix = "github.com/maaxton/waiveo-next/"

func TestTheAppliancesBinariesNeverReachTheRasterizer(t *testing.T) {
	for _, bin := range []string{
		modulePrefix + "cmd/waiveo-feeder",
		modulePrefix + "cmd/waiveo-relay",
	} {
		t.Run(filepath.Base(bin), func(t *testing.T) {
			reached := firstPartyClosure(t, bin)
			for _, banned := range forbiddenFromTheAppliance {
				if path, ok := reached[banned]; ok {
					t.Errorf("%s reaches %s via %s\n\n"+
						"The rasterizer is deliberately OFF-appliance: the shipped binaries are Go-only and "+
						"the box cannot host a second headless Chromium. Whatever this import was for belongs "+
						"in cmd/waiveo-derive, or behind the read-only GET /derive/pending queue the appliance "+
						"already serves.", bin, banned, strings.Join(path, " -> "))
				}
			}
			// A closure that found nothing would make this test vacuous, and a
			// vacuous version of this test is worse than none: it reads as proof.
			if len(reached) < 20 {
				t.Fatalf("the import closure of %s has only %d first-party packages — the walk is not working, so this check proves nothing", bin, len(reached))
			}
		})
	}
}

// TestTheRasterizerIsActuallyReachableFromItsOwnBinary is the mirror direction,
// and it is the half that makes the check above mean something.
//
// A test that only asserts absence passes trivially if the package is deleted,
// renamed, or never imported by anything at all — the same green either way. So
// the same walk is run over cmd/waiveo-derive, where the renderer MUST appear.
// Together the two say "this code exists, and it exists only there".
func TestTheRasterizerIsActuallyReachableFromItsOwnBinary(t *testing.T) {
	reached := firstPartyClosure(t, modulePrefix+"cmd/waiveo-derive")
	for _, want := range forbiddenFromTheAppliance {
		if _, ok := reached[want]; !ok {
			t.Errorf("cmd/waiveo-derive does not reach %s — either the tool is not wired to the renderer, or the package moved and the appliance check above is now guarding a name nothing uses", want)
		}
	}
}

// firstPartyClosure returns every first-party package transitively imported by
// root, mapped to one import path that reaches it (so a failure can name the
// chain rather than just the destination).
//
// It uses go/build rather than shelling out to `go list`, so the check runs
// under `go test` with no toolchain subprocess and no network.
func firstPartyClosure(t *testing.T, root string) map[string][]string {
	t.Helper()
	seen := map[string][]string{}
	var walk func(pkg string, path []string)
	walk = func(pkg string, path []string) {
		if _, done := seen[pkg]; done {
			return
		}
		here := append(append([]string{}, path...), pkg)
		if pkg != root {
			seen[pkg] = here
		}
		p, err := build.Import(pkg, "", 0)
		if err != nil {
			// A package that does not resolve is reported rather than skipped: a
			// silent skip is how this walk would stop seeing the very import it
			// exists to find.
			t.Fatalf("resolve %s: %v", pkg, err)
		}
		// Imports only — never TestImports. A test-only dependency on the
		// renderer would be fine; it is the SHIPPED binary that must not carry
		// one.
		for _, imp := range p.Imports {
			if strings.HasPrefix(imp, modulePrefix) {
				walk(imp, here)
			}
		}
	}
	walk(root, nil)
	return seen
}
