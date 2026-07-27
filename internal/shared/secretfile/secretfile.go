// Package secretfile is the one implementation of this tree's on-disk secret
// discipline: a secret lands 0600, inside a 0700 directory, and is installed by
// rename so a crash mid-write leaves either the old bytes or the new ones and
// never a truncated file.
//
// It exists as a shared package rather than as a habit each holder of key
// material re-implements. The auth bootstrap's first-boot setup code
// (internal/app/auth), the workspace signing key and the per-workspace data key
// (internal/app/workspacekey) all persist secrets, and a mode this package gets
// right in one place cannot drift to 0644 in another.
//
// Directory permissions are ENFORCED, not merely requested. os.MkdirAll applies
// its mode only to directories it creates: pointed at a path that already
// exists, it returns nil and leaves a 0755 directory 0755. Every function here
// that takes a directory therefore chmods it after the MkdirAll, which is the
// difference between "we asked for 0700" and "it is 0700".
package secretfile

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// DirMode is the mode a directory holding secrets is kept at: owner-only,
	// including the execute bit that makes the directory traversable at all. A
	// group- or world-executable secrets directory lets anyone who guesses a
	// filename inside it stat and open that file, whatever the file's own mode.
	DirMode os.FileMode = 0o700

	// FileMode is the mode a secret file is kept at: owner read/write, nothing
	// for anyone else.
	FileMode os.FileMode = 0o600
)

// EnsureDir creates dir with DirMode if it is absent and tightens it to DirMode
// if it already exists.
func EnsureDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("secretfile: empty directory")
	}
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("secretfile: create dir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, DirMode); err != nil {
		return fmt.Errorf("secretfile: tighten dir %s: %w", dir, err)
	}
	return nil
}

// Write persists data at path as a secret: FileMode, under a DirMode parent,
// installed atomically.
//
// The temporary file is created 0600 from the outset rather than chmod'ed after
// its bytes are written, so the secret is never momentarily readable by anyone
// else — a window a create-then-chmod ordering leaves open for exactly as long
// as the write takes.
func Write(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := EnsureDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".secret-*")
	if err != nil {
		return fmt.Errorf("secretfile: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup of a temp file the rename below never claimed.
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(FileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secretfile: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secretfile: write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secretfile: sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("secretfile: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("secretfile: install %s: %w", path, err)
	}
	return nil
}

// Harden tightens an ALREADY-WRITTEN file to FileMode.
//
// It is for the case Write cannot serve: a file whose bytes some other component
// produces at a mode of its own choosing — SQLite's `VACUUM INTO`, which creates
// its destination 0644 and offers no way to ask for anything else. The bytes are
// that component's to write; the mode is still ours to fix, and fixing it
// immediately is what keeps the window measured in microseconds rather than for
// as long as the file exists.
func Harden(path string) error {
	if err := os.Chmod(path, FileMode); err != nil {
		return fmt.Errorf("secretfile: tighten %s: %w", path, err)
	}
	return nil
}
