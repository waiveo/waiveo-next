package secretfile

import (
	"os"
	"path/filepath"
	"testing"
)

// modeOf reports path's permission bits.
func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// TestWritePersistsSecretOwnerOnly: a secret lands 0600 inside a 0700 directory,
// with its bytes intact.
func TestWritePersistsSecretOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	path := filepath.Join(dir, "secret")
	want := []byte("key material")

	if err := Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := modeOf(t, path); got != 0o600 {
		t.Errorf("secret file mode = %04o, want 0600", got)
	}
	if got := modeOf(t, dir); got != 0o700 {
		t.Errorf("secret directory mode = %04o, want 0700", got)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("read back %q, want %q", got, want)
	}
}

// TestWriteLeavesNoTempFileBehind: the install is a rename, and the directory
// holds exactly the secret afterward — a stray temp file would be a second copy
// of the same secret nobody is tracking.
func TestWriteReplacesAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")

	if err := Write(path, []byte("first")); err != nil {
		t.Fatalf("Write first: %v", err)
	}
	if err := Write(path, []byte("second")); err != nil {
		t.Fatalf("Write second: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "secret" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want exactly [secret]", names)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("read back %q, want the second write's bytes", got)
	}
}

// TestEnsureDirTightensExistingDirectory is the case os.MkdirAll alone does NOT
// handle: MkdirAll applies its mode only to directories it creates, so a
// pre-existing 0755 secrets directory stays 0755 and every file inside it is
// reachable by anyone who can guess a name.
func TestEnsureDirTightensExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "preexisting")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("pre-create dir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("pre-chmod dir: %v", err)
	}
	if got := modeOf(t, dir); got != 0o755 {
		t.Fatalf("precondition: directory mode = %04o, want the loose 0755 this case is about", got)
	}

	if err := EnsureDir(dir); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if got := modeOf(t, dir); got != 0o700 {
		t.Errorf("directory mode = %04o after EnsureDir, want 0700", got)
	}
}

// TestHardenTightensAnExistingFile covers the mode a component outside this
// package chose for bytes it wrote itself (SQLite's `VACUUM INTO` creates its
// destination 0644).
func TestHardenTightensAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot")
	if err := os.WriteFile(path, []byte("cleartext"), 0o644); err != nil {
		t.Fatalf("pre-write file: %v", err)
	}
	if got := modeOf(t, path); got != 0o644 {
		t.Fatalf("precondition: file mode = %04o, want the loose 0644 this case is about", got)
	}

	if err := Harden(path); err != nil {
		t.Fatalf("Harden: %v", err)
	}
	if got := modeOf(t, path); got != 0o600 {
		t.Errorf("file mode = %04o after Harden, want 0600", got)
	}
}

// TestWriteRejectsAnEmptyPath: a secret with nowhere to go is a caller bug, and
// the failure belongs at the call rather than as a file named "." nobody meant.
func TestWriteRejectsAnEmptyDirectory(t *testing.T) {
	if err := EnsureDir(""); err == nil {
		t.Fatal("EnsureDir(\"\") returned nil, want an error")
	}
}
