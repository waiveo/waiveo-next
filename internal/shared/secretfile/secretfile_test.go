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

// TestTightenSQLiteSidecarsCoversWhatTheDatabaseModeProtects reads the modes off
// disk rather than trusting the call, because this is exactly the property that
// regresses silently: nothing fails, nothing logs, and a file that should be
// 0600 is 0644 until someone happens to run `ls -la`.
//
// The `-wal` sidecar holds recently-written page images. A reader who cannot
// open the database can read what was just written to it, so a database at 0600
// beside a WAL at 0644 protects nothing it meant to.
//
// The suffixes are named LITERALLY here rather than driven off
// SQLiteSidecarSuffixes, and that is the point. This list was `{-wal, -shm}`
// for a round after internal/app/restoreswap's equivalent list gained
// `-journal`, and a test that iterated the list under test could never have
// said so: iterating proves internal consistency and is blind to a missing
// member. `-journal` holds the ORIGINAL page images of rows being overwritten —
// the same secret material as the WAL, pointed backwards — and this tree really
// does produce one, because a restored store arrives as rollback-mode `VACUUM
// INTO` output.
func TestTightenSQLiteSidecarsCoversWhatTheDatabaseModeProtects(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "x.db")
	companions := []string{"x.db-wal", "x.db-shm", "x.db-journal"}
	for _, name := range append([]string{"x.db"}, companions...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		// WriteFile honours the umask, so set the mode explicitly — otherwise
		// this test asserts the umask rather than the function.
		if err := os.Chmod(filepath.Join(dir, name), 0o644); err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
	}

	if err := TightenSQLiteSidecars(db, 0o600); err != nil {
		t.Fatalf("TightenSQLiteSidecars: %v", err)
	}
	for _, name := range companions {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s is mode %04o, want 0600 — it holds what the database mode protects", name, got)
		}
	}

	// An absent sidecar is not an error: WAL and SHM are removed on a clean
	// close and recreated on the next open, when this runs again.
	if err := TightenSQLiteSidecars(filepath.Join(dir, "gone.db"), 0o600); err != nil {
		t.Errorf("an absent sidecar was treated as an error: %v", err)
	}
}
