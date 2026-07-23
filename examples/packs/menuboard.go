// Package examplepacks embeds the in-repo declarative example packs and builds
// their install artifacts, so `make dev`'s pack smoke, the end-to-end test, and
// the `make example-pack` build target all install the SAME bytes from ONE
// source of truth (the on-disk pack tree under this directory) with no relative-
// path coupling to a caller's working directory.
//
// The only example pack today is menu-board (examples/packs/menu-board/): a
// tasteful, entirely declarative pack — a manifest, two ui-schema/1 page
// documents, and a locale catalog. It is DATA: nothing here (or anywhere in the
// pack) is ever executed. MenuBoardZip assembles those files into an in-memory
// zip artifact byte-for-byte identical to what a publisher would upload to
// POST /api/v1/packs.
package examplepacks

import (
	"archive/zip"
	"bytes"
	"embed"
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
var menuBoardFS embed.FS

// MenuBoardZip assembles the menu-board example pack into an in-memory zip
// artifact — its entries named relative to the pack root (manifest.json,
// ui/menu-items.json, ui/settings.json, messages/en.json), exactly the layout the
// install pipeline expects. Entries are written in sorted order so the artifact
// is deterministic (a reproducible build, and a stable Idempotency-Key content
// hash across runs). Nothing is executed: the files are copied as bytes.
func MenuBoardZip() ([]byte, error) {
	names, err := menuBoardEntryNames()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range names {
		data, err := menuBoardFS.ReadFile(path.Join(menuBoardDir, name))
		if err != nil {
			return nil, err
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
func MenuBoardFile(name string) ([]byte, error) {
	return menuBoardFS.ReadFile(path.Join(menuBoardDir, name))
}

// MenuBoardEntryNames returns the pack's bundle entry names (relative to the pack
// root), sorted — the exact set MenuBoardZip writes.
func MenuBoardEntryNames() ([]string, error) { return menuBoardEntryNames() }

func menuBoardEntryNames() ([]string, error) {
	var names []string
	err := fs.WalkDir(menuBoardFS, menuBoardDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		names = append(names, strings.TrimPrefix(p, menuBoardDir+"/"))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}
