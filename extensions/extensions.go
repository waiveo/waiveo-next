// Package extensions embeds the FIRST-PARTY extensions this platform ships and
// assembles them into installable artifacts.
//
// # Why this is not examples/packs
//
// `examples/packs` holds reference material: the menu-board declarative sample
// and the backups pilot, which exist to prove mechanisms. What lives here ships
// — `waiveo/discovery` is a product surface an operator installs and depends on,
// and filing it under "examples" would say the opposite.
//
// The publisher namespace is the load-bearing part. `marketplace/1` MKT-001/021
// reserves the `first-party` trust channel to the sole publisher namespace
// `waiveo`, so everything assembled here is, by construction, the class of
// extension the owner means by "core extensions ... signed by us": signed on the
// first-party channel, and therefore eligible for capabilities granted by trust
// channel rather than prompted at install (manifest/1's consent tier).
//
// The assembly itself is deliberately identical to the example packs' — the
// layout IS the install contract, and two assemblers would be two places for it
// to drift.
package extensions

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

//go:embed discovery
var extensionFS embed.FS

// ZipWithFiles assembles a first-party extension by directory name into an
// in-memory zip artifact: entries named relative to the extension root, written
// in sorted order so the artifact is reproducible (a stable content hash across
// runs). Nothing is executed — files are copied as bytes.
//
// `extra` carries entries the on-disk tree does not — the compiled `runtime.entry` binary, which exists only after a
// build step and must never be committed. An extra whose name collides with an
// embedded file is an error rather than a silent preference: the two would be
// different bytes claiming one name in an artifact whose layout is the install
// contract.
func ZipWithFiles(dir string, extra map[string][]byte) ([]byte, error) {
	return zipExtension(dir, extra)
}

// File returns one embedded file's bytes, named relative to the extension root.
func File(dir, name string) ([]byte, error) {
	return extensionFS.ReadFile(path.Join(dir, name))
}

func zipExtension(dir string, extra map[string][]byte) ([]byte, error) {
	names, err := entryNames(dir)
	if err != nil {
		return nil, err
	}
	all := make([]string, 0, len(names)+len(extra))
	all = append(all, names...)
	embedded := map[string]bool{}
	for _, n := range names {
		embedded[n] = true
	}
	for n := range extra {
		if embedded[n] {
			return nil, fmt.Errorf("extensions: %s: extra entry %q collides with an embedded file", dir, n)
		}
		all = append(all, n)
	}
	sort.Strings(all)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range all {
		body, ok := extra[name]
		if !ok {
			body, err = extensionFS.ReadFile(path.Join(dir, name))
			if err != nil {
				return nil, fmt.Errorf("extensions: %s: read %s: %w", dir, name, err)
			}
		}
		w, err := zw.Create(name)
		if err != nil {
			return nil, fmt.Errorf("extensions: %s: create %s: %w", dir, name, err)
		}
		if _, err := w.Write(body); err != nil {
			return nil, fmt.Errorf("extensions: %s: write %s: %w", dir, name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("extensions: %s: close zip: %w", dir, err)
	}
	return buf.Bytes(), nil
}

// entryNames lists every embedded file under dir, named relative to it.
// Directories carry no bytes and are not entries — a zip's directory structure
// is implied by its entry names.
func entryNames(dir string) ([]string, error) {
	var out []string
	err := fs.WalkDir(extensionFS, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
		if rel == "" {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("extensions: walk %s: %w", dir, err)
	}
	sort.Strings(out)
	return out, nil
}
