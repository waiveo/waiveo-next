package packs

import (
	"errors"
	"fmt"
	"os"
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
// The mechanics of that posture — regular-file, size cap, file mode, the walk up
// EVERY ancestor directory, and the duplicate-JSON-member refusal — live in
// hostconfig.go, shared with the registry sources document, which is provisioned
// by the same party and grants in the same way. Read that file's doc for what
// each check defeats and for the one thing (ownership) deliberately not checked.
//
// One limit particular to the roster: ancestors are checked only when a roster
// file is PRESENT. A deployment that never authored a roster is not made
// unbootable by the mode of a directory it does not use — but note the
// consequence: a writable config directory lets an attacker PLANT a roster that
// the next boot honours. That is an argument for the directory's mode, not for
// refusing to boot a deployment that declared nothing.

// RosterFormat is the required-pack roster document's format discriminant. It is
// the check that keeps some OTHER well-formed JSON document — the trust-anchors
// file being the obvious neighbour — from resolving to a valid, empty roster.
const RosterFormat = "required-packs/1"

// maxRosterBytes caps the roster document this host will read. A roster is a
// list of pack ids and versions; a few dozen entries is a few kilobytes. The cap
// is here so a file at the roster path cannot make the boot allocate without
// bound, and it is generous enough that no real roster can reach it.
const maxRosterBytes = 1 << 20

// rosterWhat names the document in the shared guard's messages.
const rosterWhat = "required-pack roster"

// rosterUnresolvable stamps every guard/decode failure with what it MEANS for
// this document — the half the shared guard cannot know. Applied to all of them,
// so no route out of LoadRoster reports a broken roster as anything other than
// UNRESOLVABLE: an operator reading "not a regular file" alone could reasonably
// conclude the file was simply ignored, which is the one thing MKT-093a says it
// must not be.
func rosterUnresolvable(err error) error {
	return fmt.Errorf("packs: %w — it is UNRESOLVABLE, not empty (marketplace/1 MKT-093a)", err)
}

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
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		// The declared-nothing case, and the ONLY permissive early return in
		// this function. NewRoster(nil) rather than a bare Roster{}: an empty
		// roster has to be RESOLVED, and going through the one constructor is
		// what makes that true by construction instead of by remembering.
		return NewRoster(nil)
	}
	// The shared host-config guard: regular file, size cap, file mode, every
	// ancestor directory's mode, and the duplicate-JSON-member refusal — the last
	// of which matters most here, because appending `,"required":[]` before the
	// closing brace produces a document that reads top to bottom as declaring
	// strict floors and resolves to a roster that requires nothing.
	raw, err := readHostConfig(path, rosterWhat, maxRosterBytes)
	if err != nil {
		return Roster{}, rosterUnresolvable(err)
	}
	// A typo'd field name would otherwise decode to a document with the right
	// format and no entries — a well-formed roster that requires nothing, which
	// is precisely the degradation MKT-093a's amendment forbids. Trailing content
	// is refused for the neighbouring reason: a file whose real roster sits after
	// a decoy would resolve to the decoy.
	var doc rosterFile
	if err := decodeHostConfig(raw, path, rosterWhat, &doc); err != nil {
		return Roster{}, rosterUnresolvable(err)
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
