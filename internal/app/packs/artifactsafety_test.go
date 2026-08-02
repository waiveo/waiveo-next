package packs_test

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// artifactsafety_test.go distinguishes entry-level defences that reader_test.go
// already exercises but cannot tell apart.
//
// ReadBundle applies its rules in pairs that report the SAME error code:
//
//   - a symlink entry is refused by the symlink check, and — if that check were
//     gone — by the not-a-regular-file check immediately after it. Both answer
//     PACK_ARTIFACT_UNSAFE_ENTRY.
//   - an oversized entry is refused by the header's DECLARED size, and — if that
//     check were gone — by the cap on bytes actually read. Both answer
//     PACK_ARTIFACT_FILE_TOO_LARGE.
//
// So a test asserting the code passes whichever half fired, and a mutation sweep
// finds all four unheld even though the behaviour is covered. These assert the
// MESSAGE, which is the only thing that names which rule ran.
//
// The pairing is not redundancy in either case. A symlink is one kind of
// non-regular entry and the explicit check exists to say so; more importantly
// the declared size is ATTACKER-CONTROLLED — it is read from the archive, not
// measured — so it is checked first because it is free, and the read is capped
// as well because a header that lies about a small size is what a decompression
// bomb looks like.

func zipWithModes(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetMode(e.mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("create %s: %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("write %s: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

type zipEntry struct {
	name string
	body string
	mode fs.FileMode
}

// TestSymlinkIsRefusedAsASymlinkNotMerelyAsNonRegular pins the symlink rule
// itself. Deleting it leaves the behaviour identical from the outside — the
// not-a-regular-file check answers with the same code — so only the message
// distinguishes them, and only this assertion notices if the dedicated check
// goes away.
func TestSymlinkIsRefusedAsASymlinkNotMerelyAsNonRegular(t *testing.T) {
	art := zipWithModes(t, zipEntry{"link.json", "/etc/passwd", fs.ModeSymlink | 0o777})
	_, err := packs.ReadBundle(art, packs.DefaultLimits)
	if err == nil {
		t.Fatal("a symlink entry was accepted")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("a symlink entry was refused as %q — the dedicated symlink rule no longer runs, and an operator "+
			"is told the entry is 'not a regular file' rather than what it actually is", err)
	}
}

// TestNonRegularEntryIsRefusedOnItsOwn is the other half: an entry that is not a
// symlink and not a regular file must still be refused, so the pair covers the
// whole mode space rather than only the case each test happened to use.
func TestNonRegularEntryIsRefusedOnItsOwn(t *testing.T) {
	art := zipWithModes(t, zipEntry{"fifo", "", fs.ModeNamedPipe | 0o644})
	_, err := packs.ReadBundle(art, packs.DefaultLimits)
	if err == nil {
		t.Fatal("a named-pipe entry was accepted")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a named-pipe entry was refused as %q, want the not-a-regular-file rule", err)
	}
}

// TestOversizedEntryIsRefusedOnItsDeclaredSize pins the cheap half of the size
// pair: the header's declared size is checked BEFORE any bytes are read, so an
// honestly-oversized entry costs nothing to refuse. Deleting it still refuses
// the artifact — via the read cap — but only after decompressing up to the
// limit, which is the work the check exists to avoid.
func TestOversizedEntryIsRefusedOnItsDeclaredSize(t *testing.T) {
	lim := packs.DefaultLimits
	lim.MaxFileBytes = 64

	art := zipWithModes(t, zipEntry{"big.json", strings.Repeat("A", 4096), 0o644})
	_, err := packs.ReadBundle(art, lim)
	if err == nil {
		t.Fatal("an entry over the per-file limit was accepted")
	}
	if strings.Contains(err.Error(), "when decompressed") {
		t.Errorf("the entry was refused by the READ cap (%q) rather than by its declared size — the cheap check "+
			"no longer runs, so every oversized entry is now decompressed before being refused", err)
	}
	if !strings.Contains(err.Error(), "per-file limit") {
		t.Errorf("refused with %q, want the per-file limit rule", err)
	}

	// The control: the same artifact under the real limits reads, so the refusal
	// above is the cap rather than a broken reader.
	if _, err := packs.ReadBundle(art, packs.DefaultLimits); err != nil {
		t.Errorf("a conformant artifact was refused under the default limits: %v", err)
	}
}

// The read cap in readCapped — "exceeds the limit when decompressed" — is NOT
// pinned here, and CANNOT BE while the declared-size check above it stands.
//
// An earlier version of this note said reaching it needed an archive whose
// declared size disagrees with its content. That was wrong, and two probes say
// why:
//
//   - Patch the central directory to declare 64 bytes for a 4096-byte entry and
//     the declared-size check passes it — but the read then fails as
//     PACK_ARTIFACT_INVALID ("zip: not a valid zip file"), because Go's zip
//     reader enforces the declared size itself and refuses to yield more.
//   - Disable the declared-size check and feed an honest 4096-byte entry under a
//     1024-byte limit, and the read cap fires exactly as written.
//
// So it is a BACKSTOP for the check above it rather than an independently
// reachable rule: no input reaches it while that check runs. That is the same
// category as isJSONObject's null pre-check in internal/events — correct code,
// worth keeping, and not something a test can hold.
//
// Keeping it is still right. It costs nothing, and it is the check that would
// still bound the read if the declared-size test were ever relaxed or reordered
// — which is precisely the change that would otherwise reintroduce the bomb.
