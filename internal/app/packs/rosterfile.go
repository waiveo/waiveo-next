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

// rosterfile.go is the on-disk half of marketplace/1 MKT-093a: how a deployment
// DECLARES its required-pack roster, and how this host resolves that declaration
// into the Roster the store enforces with.
//
// Until this file existed, Roster was reachable only from a test. A floor that
// nothing a deployment can author ever reaches is not a floor.
//
// ---- Why a file, in the anchors' shape -------------------------------------
//
// MKT-093a puts the roster "behind the same namespace-to-configuration seam
// MKT-009b sanctions for its trust anchors", and the anchors are already a
// host-provisioned JSON document at a pinned absolute path (packsig.FileAnchors).
// Reusing that shape means an operator learns one provisioning idiom, and the
// eventual root-signed replacement can slot in behind the same seam.
//
// What the operator has to author is a list of {pack_id, floor_version} pairs —
// which is why entries are an ARRAY of objects rather than a JSON object keyed
// by pack id. A keyed object cannot express a duplicate: Go's decoder silently
// keeps the last of two entries for the same key, so a file declaring
// `waiveo/system` at both 1.0.0 and 3.0.0 would resolve to one of them with no
// diagnostic, and an operator who believed the other would be running a
// deployment whose floor is not the one they wrote. The array makes the
// duplicate visible, and it is refused.
//
// ---- Absent vs UNRESOLVABLE, which is the whole point ----------------------
//
// MKT-093a: "An absent or empty roster makes no pack required", but "an
// UNRESOLVABLE roster is a different case and MUST NOT be treated as an empty
// one" — a roster the host cannot read, parse, or validate means the deployment
// DID declare restrictions and the host failed to learn them.
//
// Every refusal below therefore returns the ZERO Roster, which is unresolved and
// refuses every pack mutation (Roster's own doc). This is structural, not a
// convention a future edit can drift from: there is exactly one expression in
// this package that sets resolved=true, it is inside NewRoster, and the only way
// out of LoadRoster with a permissive value is to reach one of the two calls to
// it below — the absent-file line and the final line. A caller who ignores the
// error gets a host that refuses pack mutations, never one that has silently
// stopped requiring anything.
//
// Note which errors are NOT parse errors and still land here: a `format`
// mismatch and an unknown field. Both exist because JSON's own laxity is the
// degradation risk. Point this loader at the trust-anchors document and it
// parses perfectly into a rosterFile with zero entries — a well-formed,
// zero-required roster from a file that is not a roster at all. The format
// discriminant catches that one; DisallowUnknownFields catches the same failure
// spelled as a typo, where `"requird": [...]` decodes to a document with the
// right format and no entries.
//
// ---- Integrity: yes, the anchors' mode check, and more --------------------
//
// The anchors refuse a group- or world-writable file because anyone who can
// write a trust root can name themselves trusted. A roster is a RESTRICTION
// rather than a permission, so it is worth asking whether writing one grants
// anything. It does, in both directions:
//
//   - ADDING an entry makes a pack un-uninstallable and un-downgradable. An
//     attacker who has landed a malicious pack can make the platform refuse to
//     remove it by naming it in the roster. Write access to the roster is write
//     access to "make my pack permanent".
//   - REMOVING an entry, or lowering a floor, lifts the protection — permitting
//     the downgrade to a known-vulnerable version, or the removal of a pack the
//     deployment says it cannot run without, that the floor exists to prevent.
//
// So there is no direction in which roster write access is harmless, and the
// anchors' posture applies unchanged. One asymmetry makes it MORE important
// here, not less: because absent/empty means nothing is required, an attacker
// does not even need to craft valid content — truncating the file to an empty
// but well-formed roster lifts every floor and leaves a document that parses.
// The equivalent attack on the anchors merely denies installs, because an empty
// anchor set fails closed.
//
// The path is resolved through symlinks and EVERY ancestor directory is checked.
// A file-mode check alone is defeated by a writable parent — an attacker who can
// write the directory renames their own 0600 document over the path and the mode
// check passes on a file they authored — and a parent-only check is defeated one
// level up, by moving the whole directory aside. A symlinked path defeats both,
// since the mode check follows the link to its target while a lexical parent
// does not.
//
// Limits, stated rather than left to be discovered:
//
//   - OWNERSHIP is not checked, and the reason is NOT that the directory check
//     already closes it — that argument is false. A directory owned by the
//     process's own uid at 0700 passes every check here and is writable by that
//     uid by definition, so a compromised feeder can author a roster pinning its
//     own pack un-uninstallable. The real reason is narrower: the process's euid
//     is not a sound authority on who SHOULD own deployment configuration, and a
//     uid comparison would refuse legitimate non-root service deployments. The
//     defence against a compromised process writing its own config is to keep
//     that config outside the paths the service can write — which the shipped
//     unit does — not a check inside the process being compromised.
//   - Ancestors are checked only when a roster file is PRESENT. A deployment
//     that never authored a roster is not made unbootable by the mode of a
//     directory it does not use — but note the consequence: a writable config
//     directory lets an attacker PLANT a roster that the next boot honours.
//     That is an argument for the directory's mode, not for refusing to boot a
//     deployment that declared nothing.

// RosterFormat is the required-pack roster document's format discriminant. It is
// the check that keeps some OTHER well-formed JSON document — the trust-anchors
// file being the obvious neighbour — from resolving to a valid, empty roster.
const RosterFormat = "required-packs/1"

// maxRosterBytes caps the roster document this host will read. A roster is a
// list of pack ids and versions; a few dozen entries is a few kilobytes. The cap
// is here so a file at the roster path cannot make the boot allocate without
// bound, and it is generous enough that no real roster can reach it.
const maxRosterBytes = 1 << 20

// rosterFile is the on-disk required-pack roster document (MKT-093a). It carries
// deployment configuration only: no key material, no artifact bytes.
type rosterFile struct {
	Format   string        `json:"format"`
	Required []rosterEntry `json:"required"`
}

// rosterEntry is one {pack_id, floor_version} declaration.
type rosterEntry struct {
	PackID       string `json:"pack_id"`
	FloorVersion string `json:"floor_version"`
}

// LoadRoster resolves the deployment's required-pack roster from the document at
// path (MKT-093a).
//
// It returns, in the three cases the requirement distinguishes:
//
//   - ABSENT file → a RESOLVED empty roster and a nil error. No pack is required;
//     this is the contract's default and the state every unprovisioned host,
//     including every dev and CI run, is in.
//   - a readable, well-formed, valid document → a RESOLVED roster carrying its
//     entries.
//   - anything else → a non-nil error AND the zero Roster, which is UNRESOLVED
//     and refuses every install and every uninstall of every pack. It is never
//     an empty roster. See this file's doc for why that is structural.
//
// The caller decides what an error means for the process. The feeder treats it
// as fatal at boot: refusing to start is MKT-093a's first option, and it is the
// honest one for a host that reads the roster once, because the alternative is a
// box that runs indefinitely with its pack surface dead and nothing saying why.
//
// path should already be absolute. The caller pins it once at config load, for
// the reason the trust-anchor path is pinned: a relative path means the file the
// host consults follows the process's working directory.
// RosterAbsent reports whether no roster file existed at path — as distinct from
// one that existed and declared nothing.
//
// The two are the same to the enforcement (neither requires any pack) and very
// different to an operator. A typo'd env var, an edited-out unit line, a wrong
// working directory, or a roster on a filesystem that mounts after the unit all
// land on ABSENT, which is permissive and — because the roster is read once —
// permanent for the process's life. If the boot report cannot tell an operator
// which of the two happened, it cannot confirm the deployment loaded the roster
// it meant to, which is the only reason the report exists.
func RosterAbsent(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

func LoadRoster(path string) (Roster, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		// The declared-nothing case, and the ONLY permissive early return in
		// this function. NewRoster(nil) rather than a bare Roster{}: an empty
		// roster has to be RESOLVED, and going through the one constructor is
		// what makes that true by construction instead of by remembering.
		return NewRoster(nil)
	}
	if err != nil {
		return Roster{}, fmt.Errorf("packs: the required-pack roster %s cannot be read (%w) — it is UNRESOLVABLE, not empty (marketplace/1 MKT-093a)", path, err)
	}
	// Not a regular file. A directory or a device would fail the read anyway, but
	// a FIFO would not: it would block the boot forever on a read that never
	// returns, which is a hang rather than a refusal and so is worse than either.
	if !info.Mode().IsRegular() {
		return Roster{}, fmt.Errorf("packs: the required-pack roster %s is not a regular file (mode %v) — refusing to read it (marketplace/1 MKT-093a)", path, info.Mode())
	}
	if info.Size() > maxRosterBytes {
		return Roster{}, fmt.Errorf("packs: the required-pack roster %s is %d bytes, past the %d-byte limit — refusing to read it (marketplace/1 MKT-093a)", path, info.Size(), maxRosterBytes)
	}
	// Anyone who can write the roster can pin their own pack in place, or lift
	// every floor by emptying it. Refused rather than read, the same posture the
	// trust anchors take, for the reasons in this file's doc.
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		return Roster{}, fmt.Errorf("packs: the required-pack roster %s is group- or world-writable (mode %04o) — anyone who can write it can lift every floor or pin a pack in place, so it is refused rather than read (marketplace/1 MKT-093a)", path, mode)
	}
	if err := checkRosterDir(path); err != nil {
		return Roster{}, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Roster{}, fmt.Errorf("packs: the required-pack roster %s cannot be read (%w) — it is UNRESOLVABLE, not empty (marketplace/1 MKT-093a)", path, err)
	}

	// A DUPLICATED key is checked before anything is decoded, because the decoder
	// cannot see it: encoding/json has no duplicate-name detection, and
	// DisallowUnknownFields only rejects names the struct does not define —
	// `required` is a name it DOES define, so a second one is simply last-wins.
	//
	// That is the worst possible failure for this file. Appending
	// `,"required":[]` before the closing brace produces a document that reads
	// top to bottom — in review, in a config-management diff — as declaring
	// strict floors, and resolves to a roster that requires nothing. No parse
	// error, no format mismatch, no unknown field, no trailing content: it walks
	// through every other guard here and lands on the PERMISSIVE side, which is
	// precisely the degradation this loader exists to make impossible.
	if err := refuseDuplicateNames(raw); err != nil {
		return Roster{}, fmt.Errorf("packs: the required-pack roster %s %w — it is UNRESOLVABLE, not empty (marketplace/1 MKT-093a)", path, err)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	// A typo'd field name would otherwise decode to a document with the right
	// format and no entries — a well-formed roster that requires nothing, which
	// is precisely the degradation MKT-093a's amendment forbids.
	dec.DisallowUnknownFields()
	var doc rosterFile
	if err := dec.Decode(&doc); err != nil {
		return Roster{}, fmt.Errorf("packs: the required-pack roster %s is not a valid %s document (%w) — it is UNRESOLVABLE, not empty (marketplace/1 MKT-093a)", path, RosterFormat, err)
	}
	// Trailing content: a second JSON document, or garbage, after the first.
	// Decode stops at the end of the first value and would ignore the rest, so a
	// file whose real roster sits after a decoy would resolve to the decoy.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Roster{}, fmt.Errorf("packs: the required-pack roster %s carries content after its document — it is UNRESOLVABLE, not empty (marketplace/1 MKT-093a)", path)
	}
	if doc.Format != RosterFormat {
		return Roster{}, fmt.Errorf("packs: the required-pack roster %s declares format %q, want %q — a document that is not a roster is UNRESOLVABLE, never an empty roster (marketplace/1 MKT-093a)", path, doc.Format, RosterFormat)
	}

	entries := make(map[string]string, len(doc.Required))
	for i, e := range doc.Required {
		if _, dup := entries[e.PackID]; dup {
			return Roster{}, fmt.Errorf("packs: the required-pack roster %s declares %q twice (entry %d) — the host cannot know which floor the deployment meant, so the roster is UNRESOLVABLE (marketplace/1 MKT-093a)", path, e.PackID, i)
		}
		entries[e.PackID] = e.FloorVersion
	}
	// The one remaining route to a resolved roster. NewRoster applies MAN-001's
	// id grammar and MAN-002's version grammar and returns the zero Roster on any
	// violation, so a malformed entry lands unresolved exactly like a malformed
	// document.
	r, err := NewRoster(entries)
	if err != nil {
		return Roster{}, fmt.Errorf("packs: the required-pack roster %s is invalid: %w", path, err)
	}
	return r, nil
}

// checkRosterDir refuses a roster whose containing directory is group- or
// world-writable, because an attacker who can write that directory can rename
// their own well-moded document over the roster path and pass the file's own
// mode check on a file they authored.
//
// The sticky bit is honoured: a sticky directory (1777, /tmp's mode) forbids
// non-owners from renaming or unlinking files they do not own, which is exactly
// the attack this check exists to stop, so refusing there would be a false
// refusal.
func checkRosterDir(path string) error {
	// Resolve symlinks FIRST. os.Stat follows them, so the file's own mode check
	// applies to the target — while filepath.Dir(path) is the lexical parent of
	// the LINK, which need not be the directory actually holding the file. A
	// roster symlinked out of a tight directory into a writable one would pass
	// both checks while living somewhere anyone can replace it, which is not an
	// exotic setup: packaging that points /etc/waiveo/required-packs.json at a
	// path under the service's own writable tree produces exactly it.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("packs: the required-pack roster %s cannot be resolved (%w) — it is UNRESOLVABLE, not empty (marketplace/1 MKT-093a)", path, err)
	}

	// EVERY ancestor, not just the immediate parent. The argument for checking
	// the parent — someone who can write it renames their own document over the
	// path — applies unchanged one level up: with write access to a grandparent,
	// an attacker moves the whole directory aside and puts their own in its
	// place, and every mode below is theirs to choose. Checking one level is
	// checking the level an attacker simply steps over.
	dir := filepath.Dir(resolved)
	for {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("packs: the directory %s, holding the required-pack roster, cannot be examined (%w) — the roster is UNRESOLVABLE, not empty (marketplace/1 MKT-093a)", dir, err)
		}
		if perm := info.Mode().Perm(); perm&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("packs: %s, an ancestor of the required-pack roster, is group- or world-writable (mode %04o) and not sticky — anyone who can write it can substitute the roster or the directory holding it with one that lifts every floor, so the roster is refused rather than read (marketplace/1 MKT-093a)", dir, perm)
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
// to the trailing-content check. For a document whose whole job is to declare
// restrictions, last-wins is the wrong direction: the later value is the one an
// appender controls, and an empty later value lifts every floor.
//
// Depth matters as much as the top level: a duplicated `pack_id` inside an entry
// would let the visible id differ from the one that binds.
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
