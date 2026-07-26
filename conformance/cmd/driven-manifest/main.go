// Command driven-manifest regenerates the Go-driver-owned entries of
// conformance/driven-manifest.json — the machine-checkable record of which
// corpus case IDs each of this repo's six executable conformance drivers
// (conformance/drivers/api1, deviceclass1, events1, manifest1, player1,
// relay1) actually drove against a LIVE implementation the last time this
// tool ran, and which it deliberately left PENDING.
//
// scripts/validate-coverage.mjs (the traceability coverage checker) reads
// this file to decide whether a traceability map's "covered"
// row is backed by a case some driver genuinely executes — never by a case
// nobody drives. This tool is the only writer of the Go-driver-owned entries;
// three further entries (data-model/1, rules/1, ui-schema/1) are
// hand-maintained because their drivers live inside _test.go compilation
// units this tool cannot import (see each entry's own "driver" field for
// what asserts it fresh) — this tool reads them back from the existing file
// and writes them through unchanged.
//
// Usage:
//
//	go run ./conformance/cmd/driven-manifest            # print the regenerated entries; exit 0
//	go run ./conformance/cmd/driven-manifest -write      # regenerate and persist to disk
//
// conformance/cmd/driven-manifest/main_test.go is the teeth: it calls
// generate() itself (never shells out to `go run`) and fails if the result
// disagrees with what is checked in, so a driver that starts driving a new
// case (or stops driving one) without anyone re-running -write is caught by
// `go test ./...` — no separate CI step is needed to keep this file fresh.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/maaxton/waiveo-next/conformance/drivers/api1"
	"github.com/maaxton/waiveo-next/conformance/drivers/deviceclass1"
	"github.com/maaxton/waiveo-next/conformance/drivers/events1"
	"github.com/maaxton/waiveo-next/conformance/drivers/manifest1"
	"github.com/maaxton/waiveo-next/conformance/drivers/player1"
	"github.com/maaxton/waiveo-next/conformance/drivers/relay1"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
)

// manifestPath is the repo-relative spelling used in every message this tool
// prints. The actual file location resolves at runtime via manifestFilePath,
// below — `go run ./conformance/cmd/driven-manifest` runs with cwd at the
// repo root, but `go test ./...` runs each package with cwd set to the
// package's OWN directory, so a bare relative path would silently miss the
// file under `go test` (the exact mistake the conformance/drivers/* packages
// avoid with their own runtime.Caller-based corpusDir() helpers).
const manifestPath = "conformance/driven-manifest.json"

// manifestFilePath resolves manifestPath to an absolute path anchored on this
// source file's own location, so it is correct regardless of the caller's
// working directory.
func manifestFilePath() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "driven-manifest.json")
}

// Entry is one contract's driven-manifest record. Driven and Pending are
// FULL corpus case IDs (never a driver's internal short-prefix literal) —
// straight off report.Report.Driven()/PendingIDs(), which already resolve
// through the real corpus.Case the driver looked up.
type Entry struct {
	// Driver documents what produced/asserts this entry — a Go driver
	// package for the six this tool owns, or the hand-maintained source +
	// asserting test for the other three.
	Driver string `json:"driver"`
	// Driven lists every case ID actually executed (PASS or FAIL — execution
	// is the bar, not passing; report.Report.Driven() already reflects that).
	Driven []string `json:"driven"`
	// Pending lists cases the driver deliberately did not execute yet, each
	// with a stated reason living in the driver's own source (§10 "no silent
	// caps") — this file only records the ID list.
	Pending []string `json:"pending"`
}

// Manifest is the full conformance/driven-manifest.json shape: contract slug
// ("api/1", "relay/1", ...) -> its Entry.
type Manifest map[string]Entry

// generatedContracts are the contract slugs this tool owns end to end: it
// computes their Driven/Pending sets fresh every run by actually invoking
// each driver's Run() in-process. Any other key present in the checked-in
// file is a hand-maintained entry this tool must read back unchanged.
var generatedContracts = []string{
	"api/1",
	"device-class-registry/1",
	"events/1",
	"manifest/1",
	"player/1",
	"relay/1",
}

// generate runs all six owned drivers in-process, pinning the same
// deterministic, CI-safe targets their own *_test.go TestXDriverGreen tests
// use (NewRealRelayClient/NewInProcessFeeder, NewVirtualPlayerTarget/
// NewInProcessRelay — never the env-gated remote-Roku player target), and
// returns their Go-driver-owned entries keyed by contract slug.
func generate() (map[string]Entry, error) {
	out := map[string]Entry{}

	out["api/1"] = fromReport(api1.Run(), "conformance/drivers/api1 (go run ./conformance/cmd/driven-manifest -write)")

	feeder, err := relay1.NewInProcessFeeder()
	if err != nil {
		return nil, fmt.Errorf("relay1.NewInProcessFeeder: %w", err)
	}
	defer feeder.Close()
	out["relay/1"] = fromReport(
		relay1.Run(relay1.NewRealRelayClient(), feeder),
		"conformance/drivers/relay1 (go run ./conformance/cmd/driven-manifest -write)",
	)

	inProcRelay, err := player1.NewInProcessRelay()
	if err != nil {
		return nil, fmt.Errorf("player1.NewInProcessRelay: %w", err)
	}
	defer inProcRelay.Close()
	out["player/1"] = fromReport(
		player1.Run(player1.NewVirtualPlayerTarget(), inProcRelay),
		"conformance/drivers/player1 (go run ./conformance/cmd/driven-manifest -write)",
	)

	out["events/1"] = fromReport(events1.Run(), "conformance/drivers/events1 (go run ./conformance/cmd/driven-manifest -write)")
	out["manifest/1"] = fromReport(manifest1.Run(), "conformance/drivers/manifest1 (go run ./conformance/cmd/driven-manifest -write)")
	out["device-class-registry/1"] = fromReport(deviceclass1.Run(), "conformance/drivers/deviceclass1 (go run ./conformance/cmd/driven-manifest -write)")

	return out, nil
}

// fromReport converts a driver's Report into a manifest Entry. It tolerates
// FAILed cases exactly as report.Report.Driven() does (execution is the bar,
// not passing — api1's API-111 is driven-but-FAILING by design) and dedupes,
// since a case a driver records more than once must still appear once here.
func fromReport(rep report.Report, driver string) Entry {
	return Entry{Driver: driver, Driven: dedupeSorted(rep.Driven()), Pending: dedupeSorted(rep.PendingIDs())}
}

func dedupeSorted(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// readManifest loads the checked-in manifest, if any. A missing file is not
// an error (first run): it is treated as an empty manifest with no
// hand-maintained entries yet.
func readManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Manifest{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

// merge overlays the freshly generated entries onto the existing manifest,
// leaving every hand-maintained entry (any key not in generatedContracts)
// exactly as read from disk.
func merge(existing Manifest, generated map[string]Entry) Manifest {
	out := Manifest{}
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range generated {
		out[k] = v
	}
	return out
}

func writeManifest(path string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func main() {
	write := flag.Bool("write", false, "persist the regenerated entries to "+manifestPath+" (merging with existing hand-maintained entries)")
	flag.Parse()

	generated, err := generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "driven-manifest:", err)
		os.Exit(1)
	}

	existing, err := readManifest(manifestFilePath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "driven-manifest:", err)
		os.Exit(1)
	}
	merged := merge(existing, generated)

	if !*write {
		data, _ := json.MarshalIndent(merged, "", "  ")
		fmt.Println(string(data))
		return
	}

	if err := writeManifest(manifestFilePath(), merged); err != nil {
		fmt.Fprintln(os.Stderr, "driven-manifest:", err)
		os.Exit(1)
	}
	fmt.Printf("driven-manifest: wrote %s (%d contract(s), %d generated)\n", manifestPath, len(merged), len(generated))
}
