package packs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// hostconfig.go is the integrity guard every host-provisioned pack-configuration
// document is read through — currently two of them, and deliberately ONE guard
// rather than two:
//
//   - the required-pack roster (rosterfile.go, marketplace/1 MKT-093a);
//   - the registry source list (sourcesfile.go, marketplace/1 MKT-060/061/062).
//
// Both sit "behind the same namespace-to-configuration seam MKT-009b sanctions
// for its trust anchors" (MKT-093a's own words, MKT-062's for sources), and both
// grant something to whoever can write them: a roster entry pins a pack
// un-uninstallable, a source entry names a registry this box will fetch code
// from and — through `reserved_namespaces` — may authorize it for the `waiveo`
// namespace itself. So the same posture applies to both, and a second copy of
// this walk would be a second place for it to drift.
//
// What the guard checks, and why each one is here rather than left to the read:
//
//   - a REGULAR file. A directory or device would fail the read anyway; a fifo
//     would not — it would block the boot forever on a read that never returns,
//     which is a hang rather than a refusal and so is worse than either.
//   - a SIZE cap, so a file at a configured path cannot make the boot allocate
//     without bound.
//   - the file is not group- or world-WRITABLE.
//   - no ANCESTOR directory is group- or world-writable (sticky honoured). A
//     file-mode check alone is defeated by a writable parent — rename your own
//     0600 document over the path and the mode check passes on a file you wrote
//     — and a parent-only check is defeated one level up, by moving the whole
//     directory aside. Symlinks are resolved first, because os.Stat follows them
//     to the target while filepath.Dir does not, so a document symlinked out of
//     a tight directory into a loose one would otherwise pass both.
//   - no DUPLICATE JSON member name at any depth. encoding/json silently keeps
//     the LAST of two same-named members, so appending `,"sources":[]` before the
//     closing brace produces a document that reads top-to-bottom — in review, in
//     a config-management diff — as declaring what it declares, and resolves to
//     nothing. No parse error, no unknown field, no trailing content.
//
// Ownership is deliberately NOT checked, and the reason is not that the ancestor
// walk closes it — it does not. A directory owned by the process's own uid at
// 0700 passes everything here and is writable by that uid by definition. The
// reason is narrower: the process's euid is not a sound authority on who SHOULD
// own deployment configuration, and a uid comparison would refuse legitimate
// non-root service deployments. The defence against a compromised feeder writing
// its own configuration is to keep that configuration outside the paths the
// service can write — which the shipped systemd unit does, with ProtectSystem=
// strict and the config paths outside ReadWritePaths — not a check inside the
// process being compromised.

// readHostConfig applies the guard above to path and returns the document bytes.
//
// `what` names the document in every message ("required-pack roster", "registry
// sources document"), because the caller's own wrapper adds what an unreadable
// one MEANS for that document — which is the half that differs between them, and
// the half an operator needs. The caller handles os.ErrNotExist itself: absent is
// a legitimate, differently-handled state for both documents and is not a failure
// of integrity.
func readHostConfig(path, what string, maxBytes int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("the %s %s cannot be read (%w)", what, path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("the %s %s is not a regular file (mode %v) — refusing to read it", what, path, info.Mode())
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("the %s %s is %d bytes, past the %d-byte limit — refusing to read it", what, path, info.Size(), maxBytes)
	}
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		return nil, fmt.Errorf("the %s %s is group- or world-writable (mode %04o) — anyone who can write it can change what this deployment trusts, so it is refused rather than read", what, path, mode)
	}
	if err := checkConfigAncestors(path, what); err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the %s %s cannot be read (%w)", what, path, err)
	}
	// Before any decode: the decoder cannot see a duplicate member, and
	// DisallowUnknownFields only rejects names the struct does not define.
	if err := refuseDuplicateNames(raw); err != nil {
		return nil, fmt.Errorf("the %s %s %w", what, path, err)
	}
	return raw, nil
}

// decodeHostConfig decodes raw into out, refusing the two shapes JSON's own
// laxity would otherwise wave through: a member name the document does not
// define (a typo, which would decode to a valid document declaring nothing), and
// a second document or garbage after the first (Decode stops at the end of the
// first value, so a real declaration sitting after a decoy would never be read).
//
// The duplicate-member check is NOT here: it runs in readHostConfig, over the
// raw bytes, because it must run before anything is decoded.
func decodeHostConfig(raw []byte, path, what string, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("the %s %s is not a valid document (%w)", what, path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("the %s %s carries content after its document", what, path)
	}
	return nil
}

// checkConfigAncestors refuses a document any of whose ancestor directories is
// group- or world-writable, for the reason in this file's doc.
//
// The sticky bit is honoured: a sticky directory (1777, /tmp's mode) forbids
// non-owners from renaming or unlinking files they do not own, which is exactly
// the attack this check exists to stop, so refusing there would be a false
// refusal.
func checkConfigAncestors(path, what string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("the %s %s cannot be resolved (%w)", what, path, err)
	}
	dir := filepath.Dir(resolved)
	for {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("the directory %s, holding the %s, cannot be examined (%w)", dir, what, err)
		}
		if perm := info.Mode().Perm(); perm&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("%s, an ancestor of the %s, is group- or world-writable (mode %04o) and not sticky — anyone who can write it can substitute the document or the directory holding it, so it is refused rather than read", dir, what, perm)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil // reached the root
		}
		dir = parent
	}
}

// refuseDuplicateNames walks raw as a token stream and reports the first object
// that declares one name twice, at any depth.
//
// It exists because Go's decoder silently keeps the last of two same-named
// members, so a duplicate is invisible to Decode, to DisallowUnknownFields, and
// to the trailing-content check. For documents whose whole job is to declare what
// a deployment trusts and restricts, last-wins is the wrong direction: the later
// value is the one an appender controls.
//
// Depth matters as much as the top level: a duplicated `pack_id` inside a roster
// entry, or a duplicated `id` inside a source entry, would let the visible value
// differ from the one that binds.
func refuseDuplicateNames(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// One frame per open container. For an object, names records what it has
	// already declared and expectName says whether the next token is a member
	// name or the value of the one just read — without that distinction a string
	// VALUE would be mistaken for a name and a legitimate document refused.
	type frame struct {
		object     bool
		names      map[string]bool
		expectName bool
	}
	var stack []frame

	// valueRead advances the enclosing object past the value it was expecting,
	// so the token after it is read as the next member name.
	valueRead := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].expectName = true
		}
	}

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			// A malformed document is the decoder's error to report, not this
			// one's: returning nil here lets Decode produce the precise message.
			return nil
		}

		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, frame{object: true, names: map[string]bool{}, expectName: true})
			case '[':
				stack = append(stack, frame{})
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				// The container that just closed WAS a value of its parent.
				valueRead()
			}
			continue
		}

		n := len(stack)
		if n > 0 && stack[n-1].object && stack[n-1].expectName {
			name, ok := tok.(string)
			if !ok {
				return nil // not a name where one was due; leave it to Decode
			}
			if stack[n-1].names[name] {
				return fmt.Errorf("declares the member %q more than once, so its later value silently replaces the earlier one", name)
			}
			stack[n-1].names[name] = true
			stack[n-1].expectName = false
			continue
		}
		// A scalar value — of an object member, or an array element.
		valueRead()
	}
}
