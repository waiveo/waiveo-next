// Package examplepacks embeds the in-repo declarative example packs and builds
// their install artifacts, so `make dev`'s pack smoke, the end-to-end test, and
// the `make example-pack` build target all install the SAME bytes from ONE
// source of truth (the on-disk pack tree under this directory) with no relative-
// path coupling to a caller's working directory.
//
// The only example pack today is menu-board (examples/packs/menu-board/): a
// tasteful, entirely declarative pack — a manifest, two ui-schema/1 page
// documents, and a locale catalog. It is DATA: nothing here (or anywhere in the
// pack) is ever executed. MenuBoardZip assembles those files into the UNSIGNED
// source artifact; the install pipeline requires a signature envelope
// (internal/packsig, marketplace/1 MKT-009a/b), so every consumer signs it
// before upload — `make example-pack`/the pack smoke with the make-dev
// publisher key, the end-to-end test with its fixture signer.
package examplepacks

import (
	"archive/zip"
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// menuBoardDir is the embedded pack tree's root, relative to this source file.
// Only the pack's own files live under it (the developer README sits a level up,
// outside the embed), so the assembled zip carries exactly the pack: the
// manifest, the page documents, and the locale catalog.
const menuBoardDir = "menu-board"

//go:embed menu-board
//go:embed backups
var menuBoardFS embed.FS

// PackZip assembles ANY in-repo example pack by directory name, on the same
// terms MenuBoardZip assembles menu-board: entries named relative to the pack
// root, written in sorted order so the artifact is reproducible.
//
// Generalised rather than copied when a second pack arrived — two hardcoded
// builders would be two places for the entry-naming and the sort order to drift
// apart, and the layout IS the install contract.
func PackZip(dir string) ([]byte, error) { return zipPack(dir, nil) }

// PackZipWithFiles assembles a pack with EXTRA entries the on-disk tree does not
// carry — the compiled `runtime.entry` binary, which exists only after a build
// step and must never be committed. An extra whose name collides with an
// embedded file is an error rather than a silent preference: the two would be
// different bytes claiming one name in an artifact whose layout is the install
// contract.
func PackZipWithFiles(dir string, extra map[string][]byte) ([]byte, error) {
	return zipPack(dir, extra)
}

// MenuBoardZip assembles the menu-board example pack into an in-memory zip
// artifact — its entries named relative to the pack root (manifest.json,
// ui/menu-items.json, ui/settings.json, messages/en.json), exactly the layout the
// install pipeline expects. Entries are written in sorted order so the artifact
// is deterministic (a reproducible build, and a stable Idempotency-Key content
// hash across runs). Nothing is executed: the files are copied as bytes.
func MenuBoardZip() ([]byte, error) { return zipPack(menuBoardDir, nil) }

// zipPack is the shared assembler. Extra entries (a compiled runtime binary)
// are merged into the same sorted order, so an artifact with an injected entry
// is exactly as reproducible as one without.
func zipPack(dir string, extra map[string][]byte) ([]byte, error) {
	names, err := entryNames(dir)
	if err != nil {
		return nil, err
	}
	embedded := make(map[string]bool, len(names))
	for _, n := range names {
		embedded[n] = true
	}
	for name := range extra {
		if embedded[name] {
			return nil, fmt.Errorf("examplepacks: extra entry %q collides with an embedded file", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range names {
		data, ok := extra[name]
		if !ok {
			var err error
			data, err = menuBoardFS.ReadFile(path.Join(dir, name))
			if err != nil {
				return nil, err
			}
		}
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MenuBoardFile returns the raw bytes of one embedded pack file, named relative to
// the pack root (e.g. "manifest.json", "ui/menu-items.json"). It lets a test
// assert the API serves a page document or catalog byte-for-byte as it ships on
// disk.
// PackFile returns one file from any in-repo example pack, named relative to
// the pack root.
func PackFile(dir, name string) ([]byte, error) {
	return menuBoardFS.ReadFile(path.Join(dir, name))
}

func MenuBoardFile(name string) ([]byte, error) {
	return menuBoardFS.ReadFile(path.Join(menuBoardDir, name))
}

// MenuBoardEntryNames returns the pack's bundle entry names (relative to the pack
// root), sorted — the exact set MenuBoardZip writes.
func MenuBoardEntryNames() ([]string, error) { return entryNames(menuBoardDir) }

// entryNames walks one pack's embedded tree. The `cmd/` subtree is SOURCE — the
// runtime entry's Go package, which the build step compiles and injects as a
// binary (PackZipWithFiles) — so it is skipped here: an artifact carries what
// the host runs and serves, never what a toolchain consumes.
func entryNames(dir string) ([]string, error) {
	var names []string
	err := fs.WalkDir(menuBoardFS, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, dir+"/")
		if d.IsDir() {
			if rel == "cmd" {
				return fs.SkipDir
			}
			return nil
		}
		names = append(names, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}
