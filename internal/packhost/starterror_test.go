package packhost

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// The multi-arch decision (per-arch artifacts, 2026-08-16) makes ONE failure
// routine on purpose: the wrong zip lands on a box — an amd64 entry on an arm64
// appliance, which is the ordinary shape of a Raspberry Pi install. What the
// operator saw was the kernel's own phrasing, "exec format error", which names
// neither the machine nor the remedy.

// wrongArchEntry writes an ELF header claiming a machine this host is not:
// little-endian 64-bit, e_machine = 0xF3 (RISC-V). Well-formed enough to reach
// the kernel's loader and be refused for the reason under test, rather than for
// being empty.
func wrongArchEntry(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "wrongarch")
	hdr := []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0xF3, 0x00, 1, 0, 0, 0}
	if err := os.WriteFile(p, append(hdr, make([]byte, 128)...), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestAnUnexecutableEntryIsNamedAsOneRatherThanAsExecFormatError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ENOEXEC semantics are POSIX")
	}
	entry := wrongArchEntry(t)
	err := startError(Spec{ID: "waiveo/x", Argv: []string{entry}}, execIt(t, entry))

	if !errors.Is(err, ErrEntryNotExecutable) {
		t.Fatalf("not classified as unexecutable: %v", err)
	}
	msg := err.Error()
	// The machine, because that is the fact the operator compares their artifact
	// against.
	if !strings.Contains(msg, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("the message does not name this machine (%s/%s): %s", runtime.GOOS, runtime.GOARCH, msg)
	}
	// The remedy.
	if !strings.Contains(msg, "install the artifact matching this machine") {
		t.Errorf("the message does not name the remedy: %s", msg)
	}
	// The pack and the path, so an operator knows WHICH entry to replace.
	if !strings.Contains(msg, "waiveo/x") || !strings.Contains(msg, entry) {
		t.Errorf("the message does not identify the entry: %s", msg)
	}
}

func TestItDoesNotCLAIMAnArchitectureMismatch(t *testing.T) {
	// ENOEXEC does not mean "wrong architecture". The kernel raises the same
	// error for a file that is not an executable at all — a text file with the
	// exec bit, a truncated download, a zip entry that was never a binary.
	// Asserting an architecture mismatch would send an operator hunting a
	// cross-compile when the artifact is simply corrupt.
	if runtime.GOOS == "windows" {
		t.Skip("ENOEXEC semantics are POSIX")
	}
	junk := filepath.Join(t.TempDir(), "junk")
	if err := os.WriteFile(junk, []byte("not a binary at all"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := startError(Spec{ID: "waiveo/x", Argv: []string{junk}}, execIt(t, junk))

	if !errors.Is(err, ErrEntryNotExecutable) {
		t.Fatalf("a non-binary must still classify as unexecutable: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "is not an executable at all") {
		t.Errorf("the message does not offer the other cause: %s", msg)
	}
	// The word must appear only as a POSSIBILITY, never as a finding.
	if strings.Contains(msg, "is built for a different architecture") {
		t.Errorf("the message asserts an architecture mismatch it cannot know: %s", msg)
	}
}

func TestAnOrdinaryStartFailureIsLeftAlone(t *testing.T) {
	// Only ENOEXEC gets the treatment. A missing file, a permission refusal or a
	// context cancellation are all different problems, and wrapping them in an
	// architecture sentence would be the mirror of the defect being fixed.
	missing := filepath.Join(t.TempDir(), "nope")
	err := startError(Spec{ID: "waiveo/x", Argv: []string{missing}}, execIt(t, missing))
	if errors.Is(err, ErrEntryNotExecutable) {
		t.Fatalf("a missing entry was classified as unexecutable: %v", err)
	}
	if !strings.Contains(err.Error(), "packhost: start waiveo/x") {
		t.Errorf("the ordinary wrapping was lost: %v", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the cause was not preserved for callers: %v", err)
	}
}

func TestTheKernelsCauseIsPreserved(t *testing.T) {
	// The sentence is for the operator; the wrapped error is for the caller. A
	// message that replaced the cause would make the condition undetectable by
	// anything but string matching.
	if runtime.GOOS == "windows" {
		t.Skip("ENOEXEC semantics are POSIX")
	}
	entry := wrongArchEntry(t)
	err := startError(Spec{ID: "waiveo/x", Argv: []string{entry}}, execIt(t, entry))
	if !errors.Is(err, syscall.ENOEXEC) {
		t.Errorf("ENOEXEC did not survive the wrapping: %v", err)
	}
}

// execIt runs the real thing: this package's whole subject is what the kernel
// says, and a hand-made syscall.ENOEXEC would test the fixture rather than the
// behaviour.
func execIt(t *testing.T, path string) error {
	t.Helper()
	err := exec.Command(path).Start()
	if err == nil {
		t.Fatalf("%s unexpectedly started", path)
	}
	return err
}
