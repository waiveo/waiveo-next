// Package packrun turns installed packs into running processes at boot.
//
// It is the missing link the whole extension programme waited on. Everything
// either side of it existed: the store holds a code-carrying pack's entry
// (MAN-065, `file_kind='code'`), and `internal/packhost` starts, proves and
// hot-swaps a pack process. Nothing joined them — `packhost` was imported by no
// binary at all — so an extension could be installed, served, invoked and
// updated, and never RUN.
//
// # Why the code has to reach the disk first
//
// A pack's entry lives in SQLite as bytes. A process cannot be exec'd out of a
// database row, so the bytes are written to a per-pack, per-version directory
// and executed from there. Per-VERSION and not just per-pack because a hot swap
// (packhost.Swap) runs the replacement BEFORE retiring the incumbent: they are
// briefly both live, and one directory would have the new bytes overwriting the
// file the running process was started from.
//
// # The owner's Go decision, and the one thing it does not yet say
//
// #189 (2026-08-13) settled that extensions are compiled Go binaries, shipped
// per-architecture. That makes materialisation an executable-bit problem rather
// than a text-file problem, which is what this package implements.
//
// It does NOT yet say how a multi-architecture artifact declares which binary is
// which: MAN-065's `runtime.entry` is a single path, so a pack today carries one
// entry and this host runs it whatever it was built for. An arm64 box handed an
// amd64 entry gets an exec failure from the kernel, reported as a start failure
// for that pack. That is a real gap and it is left visible rather than guessed
// at — inventing a naming convention here would be a contract decision taken in
// an implementation.
package packrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/manifest"
	"github.com/maaxton/waiveo-next/internal/packhost"
)

// Store is the slice of the app store this package reads. An interface so the
// package is drivable without a real database, and so its dependency is legible.
type Store interface {
	ListPacks(ctx context.Context) ([]store.Pack, error)
	GetPackFile(ctx context.Context, packID, kind, name string) ([]byte, bool, error)
}

// Starter is the supervisor seam — `*packhost.Supervisor` satisfies it.
type Starter interface {
	Start(ctx context.Context, spec packhost.Spec) (packhost.Running, error)
}

// Runtime is the manifest's code-carrying tier (MAN-065), read here rather than
// through internal/manifest because this package needs exactly two fields and
// manifest.PackManifest carries the whole document.
type runtimeBlock struct {
	Entry string   `json:"entry"`
	Exec  []string `json:"exec"`
}

// Host materialises and starts the packs an installation declares.
type Host struct {
	store Store
	sup   Starter
	// dir is the root every pack's binaries are written under. One directory
	// per (pack, version) beneath it.
	dir string
	// scopeNode is the node a pack principal's role binding is issued at
	// (SEC-037). Deployment-wide for now: a pack is installed workspace-wide,
	// so there is no per-pack placement to read one from.
	scopeNode string
	// apiBaseURL is where a started pack reaches api/1 to redeem its grant and
	// then lease its work. Handed over as an environment variable — see
	// packhost.Spec.APIBaseURL for why that is right for an address and wrong
	// for the code.
	apiBaseURL string
	// apiCACertPEM is the certificate a pack must trust to reach apiBaseURL,
	// written out once per boot and named to each pack by path. Empty when the
	// deployment's certificate is publicly trusted.
	apiCACertPEM []byte
}

func New(st Store, sup Starter, dir, scopeNode, apiBaseURL string, apiCACertPEM []byte) *Host {
	return &Host{
		store: st, sup: sup, dir: dir, scopeNode: scopeNode,
		apiBaseURL: apiBaseURL, apiCACertPEM: apiCACertPEM,
	}
}

// writeCA materialises the API's trust anchor once per boot and returns its
// path, or "" when the deployment supplied none.
//
// 0400 and inside the host-owned bin directory: it is not a secret — a
// certificate is public by construction — but it IS the thing that decides
// whether a pack talks to this host or to something impersonating it, so a
// pack-writable anchor would be a pack choosing its own trust.
func (h *Host) writeCA() (string, error) {
	if len(h.apiCACertPEM) == 0 {
		return "", nil
	}
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		return "", fmt.Errorf("packrun: create %s: %w", h.dir, err)
	}
	path := filepath.Join(h.dir, "api-ca.pem")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("packrun: replace %s: %w", path, err)
	}
	if err := os.WriteFile(path, h.apiCACertPEM, 0o400); err != nil {
		return "", fmt.Errorf("packrun: write %s: %w", path, err)
	}
	return path, nil
}

// entryPathIsSafe reports whether a manifest-declared entry path may be joined
// onto a directory this host owns.
//
// The entry is UNTRUSTED: it is a string in a document a publisher wrote, and
// the install pipeline validates that it names a file present in the artifact,
// not that it is a sane relative path. Joining `../../../usr/bin/something`, or
// an absolute path, would have this package write an executable outside its own
// directory — as whatever user the feeder runs as, at boot, on every restart.
//
// Rejected: anything absolute, anything with a `..` element, anything empty, and
// anything with a volume name (Windows `C:`), which `filepath.IsAbs` does not
// catch on a Unix host. Checked on the SPLIT elements rather than by substring,
// so a legitimate file called `..hidden` is not refused for containing two dots.
func entryPathIsSafe(entry string) bool {
	if entry == "" || filepath.IsAbs(entry) || filepath.VolumeName(entry) != "" {
		return false
	}
	// Reject a Windows-style separator too: a pack authored on one would carry
	// `bin\pack`, which is ONE element to filepath on Unix and would create a
	// file with a backslash in its name rather than the nesting it meant.
	if strings.ContainsRune(entry, '\\') {
		return false
	}
	for _, el := range strings.Split(filepath.ToSlash(entry), "/") {
		if el == ".." {
			return false
		}
	}
	return true
}

// packDir is where one pack VERSION's files are materialised. The pack id
// contains a `/` (publisher/name), which would nest a directory per publisher —
// harmless, but it also means a pack id and a directory layout are the same
// namespace, so the separator is replaced rather than joined through.
func (h *Host) packDir(packID, version string) string {
	return filepath.Join(h.dir, strings.ReplaceAll(packID, "/", "__"), version)
}

// Materialize writes a pack's entry to disk and returns the absolute path of the
// written file.
//
// The file is written 0o500 (r-x, owner only) and its directories 0o700: this is
// an executable the host is about to run, and a group- or world-writable one is
// a way to make the host run something else. It is REWRITTEN on every boot
// rather than reused if present — the bytes in the database are the truth, and a
// file left behind by a previous version of this code is not.
func (h *Host) Materialize(ctx context.Context, p store.Pack, rt runtimeBlock) (string, error) {
	if !entryPathIsSafe(rt.Entry) {
		return "", fmt.Errorf("packrun: %s: runtime.entry %q is not a safe relative path", p.ID, rt.Entry)
	}
	code, found, err := h.store.GetPackFile(ctx, p.ID, store.PackFileCode, rt.Entry)
	if err != nil {
		return "", fmt.Errorf("packrun: %s: read code entry: %w", p.ID, err)
	}
	if !found {
		// The install pipeline refuses a runtime block whose entry names no file
		// (PACK_CODE_ENTRY_MISSING), so this is a pack whose rows were written by
		// an older build or edited underneath us. Refused rather than started
		// empty.
		return "", fmt.Errorf("packrun: %s: no stored code entry %q", p.ID, rt.Entry)
	}

	dir := h.packDir(p.ID, p.Version)
	full := filepath.Join(dir, filepath.FromSlash(rt.Entry))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return "", fmt.Errorf("packrun: %s: create %s: %w", p.ID, filepath.Dir(full), err)
	}
	// Removed first: os.WriteFile does not change the mode of an existing file,
	// so a rewrite over a 0o500 file from a previous boot fails with EACCES.
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("packrun: %s: replace %s: %w", p.ID, full, err)
	}
	if err := os.WriteFile(full, code, 0o500); err != nil {
		return "", fmt.Errorf("packrun: %s: write %s: %w", p.ID, full, err)
	}
	return full, nil
}

// argvFor resolves the manifest's `runtime.exec` against the materialised path.
//
// MAN-065's contract shape is an argv in which the placeholder `$entry` appears
// exactly once, substituted here with the host's local path to the stored file.
// The substitution is WHOLE-TOKEN: a token that IS `$entry` becomes the
// materialised path, and a token merely containing it (`--path=$entry`) is left
// alone — manifest validation refuses that shape, and substring rewriting here
// would make this host accept what the validator tells publishers is invalid.
//
// Two lenient shapes survive for stored manifests the validator never saw:
// an EMPTY exec runs the entry itself (the natural default for a compiled
// binary), and an exec with NO placeholder keeps the older behaviour —
// `exec[0]` resolved RELATIVE TO THE PACK'S OWN DIRECTORY when it is not
// absolute, so a manifest saying `["./bin/pack"]` runs the binary this host
// just wrote rather than whatever `./bin/pack` means in the feeder's working
// directory. An absolute exec[0] is passed through: a pack declaring
// `/usr/bin/env` has said something explicit, and this host is not a sandbox
// (the owner settled that).
//
// More than one `$entry` is refused rather than substituted N times: the
// contract says exactly once, and a duplicated placeholder is a manifest this
// host cannot claim to understand.
func (h *Host) argvFor(p store.Pack, rt runtimeBlock, entryPath string) ([]string, error) {
	if len(rt.Exec) == 0 {
		return []string{entryPath}, nil
	}
	argv := append([]string(nil), rt.Exec...)
	placeholders := 0
	for i, tok := range argv {
		if tok == manifest.RuntimeEntryPlaceholder {
			placeholders++
			argv[i] = entryPath
		}
	}
	if placeholders > 1 {
		return nil, fmt.Errorf("packrun: %s: runtime.exec declares $entry %d times, want exactly once (MAN-065)", p.ID, placeholders)
	}
	if placeholders == 0 && !filepath.IsAbs(argv[0]) {
		if !entryPathIsSafe(argv[0]) {
			return nil, fmt.Errorf("packrun: %s: runtime.exec[0] %q is not a safe relative path", p.ID, argv[0])
		}
		argv[0] = filepath.Join(h.packDir(p.ID, p.Version), filepath.FromSlash(argv[0]))
	}
	return argv, nil
}

// Result is one pack's outcome, so a caller can log per pack rather than
// abandoning the boot on the first failure.
type Result struct {
	PackID string
	// Started is the live process; zero when Err is set.
	Started packhost.Running
	Err     error
}

// StartAll starts every installed, enabled, code-carrying pack.
//
// A pack that fails to materialise or start is REPORTED AND SKIPPED, never fatal:
// one publisher's broken binary must not stop a box from booting, and the packs
// that do work are the ones an operator is relying on. The caller logs each
// failure; the console's pack-health surface is where a running pack's own
// condition shows up afterwards.
//
// Packs with no `runtime` block are skipped silently — a purely declarative pack
// is a supported shape (pages, data, locales), not a misconfiguration. So is a
// disabled pack (MKT-097): disabled means "do not run this", and starting one
// because it happens to carry code would make the toggle a lie.
func (h *Host) StartAll(ctx context.Context) ([]Result, error) {
	packs, err := h.store.ListPacks(ctx)
	if err != nil {
		return nil, fmt.Errorf("packrun: list packs: %w", err)
	}
	// Written once, before any pack starts: every pack is handed the same
	// anchor, and writing it per pack would be the same bytes N times with N
	// chances to disagree.
	caFile, err := h.writeCA()
	if err != nil {
		return nil, err
	}

	var out []Result
	for _, p := range packs {
		if !p.Enabled {
			continue
		}
		var doc struct {
			Runtime *runtimeBlock `json:"runtime"`
		}
		if err := json.Unmarshal(p.Manifest, &doc); err != nil || doc.Runtime == nil {
			continue
		}
		res := Result{PackID: p.ID}
		entryPath, err := h.Materialize(ctx, p, *doc.Runtime)
		if err != nil {
			res.Err = err
			out = append(out, res)
			continue
		}
		argv, err := h.argvFor(p, *doc.Runtime, entryPath)
		if err != nil {
			res.Err = err
			out = append(out, res)
			continue
		}
		running, err := h.sup.Start(ctx, packhost.Spec{
			ID: p.ID, Version: p.Version, Argv: argv,
			ScopeNode: h.scopeNode, Role: auth.RoleOperator,
			APIBaseURL: h.apiBaseURL, APICAFile: caFile,
		})
		res.Started, res.Err = running, err
		out = append(out, res)
	}
	return out, nil
}
