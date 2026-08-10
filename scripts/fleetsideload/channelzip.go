package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// channelRoots are the entries of a Roku channel package.
//
// The first three mirror player-v3/bsconfig.json's own `files` list. The
// fourth, `testdata`, does NOT appear there and is included deliberately:
// bsconfig tells the COMPILER what to validate, while this tells the ZIP what
// the firmware needs at runtime, and the player's console-only crypto
// self-check (`launch/dev?selfcheck=1`, source/SelfCheck.brs) reads its golden
// vectors from `pkg:/testdata`. A channel packaged without it installs and
// runs perfectly right up until somebody tries to verify on-device crypto on
// new firmware, then fails in a way that looks like a crypto bug. The runbook
// (docs/runbooks/first-photon.md §3) zips it by hand for exactly this reason.
//
// Kept as a literal rather than derived from bsconfig for the same reason: a
// wrong answer here is silent on install and fatal later.
var channelRoots = []string{"manifest", "source", "components", "testdata"}

// zipEpoch is the modification time stamped on every entry. A real mtime makes
// two builds of identical source produce different bytes, which turns "is the
// wall running the build I think it is?" into a question a digest cannot
// answer. A fixed timestamp makes the zip a pure function of the source.
var zipEpoch = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

// buildChannelZip packages srcDir (a player-v3 channel source tree) into the
// zip the dev installer accepts, returning the bytes and the packaged file
// count.
//
// Entries are emitted in sorted path order and Deflate-compressed, so the same
// tree always yields the same bytes — see zipEpoch.
func buildChannelZip(srcDir string) ([]byte, int, error) {
	if _, err := os.Stat(filepath.Join(srcDir, "manifest")); err != nil {
		return nil, 0, fmt.Errorf("%s is not a Roku channel source dir (no manifest): %w", srcDir, err)
	}

	paths, err := collectChannelFiles(srcDir)
	if err != nil {
		return nil, 0, err
	}

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, rel := range paths {
		data, err := os.ReadFile(filepath.Join(srcDir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, 0, fmt.Errorf("read %s: %w", rel, err)
		}
		header := &zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: zipEpoch}
		entry, err := w.CreateHeader(header)
		if err != nil {
			return nil, 0, fmt.Errorf("zip %s: %w", rel, err)
		}
		if _, err := entry.Write(data); err != nil {
			return nil, 0, fmt.Errorf("zip %s: %w", rel, err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, 0, fmt.Errorf("finish channel zip: %w", err)
	}
	return buf.Bytes(), len(paths), nil
}

// collectChannelFiles lists every packaged file under srcDir as a sorted
// slash-separated relative path.
//
// Dotfiles are skipped at any depth. Editor swap files, .DS_Store, and a
// stray .git dir are not channel content, and the installer's compile step
// reports a confusing error rather than ignoring them.
func collectChannelFiles(srcDir string) ([]string, error) {
	var paths []string
	for _, root := range channelRoots {
		full := filepath.Join(srcDir, root)
		info, err := os.Stat(full)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("inspect %s: %w", root, err)
		}
		if !info.IsDir() {
			paths = append(paths, root)
			continue
		}
		err = filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, relErr := filepath.Rel(srcDir, p)
			if relErr != nil {
				return relErr
			}
			slashed := filepath.ToSlash(rel)
			if hasHiddenSegment(slashed) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			paths = append(paths, slashed)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", root, err)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%s contains no channel files", srcDir)
	}
	sort.Strings(paths)
	return paths, nil
}

// hasHiddenSegment reports whether any path segment starts with a dot.
func hasHiddenSegment(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// loadChannelZip reads a prebuilt channel zip and verifies it at least LOOKS
// like one — a readable archive containing a `manifest` at its root.
//
// The check is cheap and it catches the mistake this tool would otherwise make
// expensive: handing the installer a zip whose single top-level entry is a
// FOLDER (what every desktop "Compress" action produces). The firmware accepts
// the upload, fails to find the manifest, and reports a compile error per
// device — seven times, over seven multi-megabyte uploads.
func loadChannelZip(zipPath string) ([]byte, int, error) {
	data, err := os.ReadFile(zipPath)
	if err != nil {
		return nil, 0, fmt.Errorf("read channel zip %s: %w", zipPath, err)
	}
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, 0, fmt.Errorf("%s is not a readable zip: %w", zipPath, err)
	}
	manifest := false
	for _, f := range r.File {
		if path.Clean(f.Name) == "manifest" {
			manifest = true
			break
		}
	}
	if !manifest {
		return nil, 0, fmt.Errorf("%s has no `manifest` at its root — a Roku channel zip must be zipped from INSIDE the channel dir, not around it", zipPath)
	}
	return data, len(r.File), nil
}
