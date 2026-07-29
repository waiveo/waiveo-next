package packs

import (
	"fmt"
	"sort"

	"github.com/maaxton/waiveo-next/internal/manifest"
)

// required.go is the host side of marketplace/1 MKT-093a: the required-pack
// roster a deployment resolves "is this pack required, and what may it not go
// below" through.
//
// The roster is CONFIGURATION, not discovery. Nothing an index entry, a
// manifest, or the party presenting an artifact says can put a pack on it —
// the same posture MKT-009b takes for its trust anchors, for the same reason:
// a status that grants a pack protection from removal is a status a hostile
// publisher would otherwise be able to claim for itself.
//
// The floor is deployment-DECLARED and never derived. A floor tracking, say,
// the highest version ever applied would sit above MKT-093's own revert
// target — which is by construction an earlier successfully-applied version —
// and so would forbid the revert it exists alongside. That is why Roster is a
// plain declared map and nothing here consults the store.

// UnresolvedFloor is the floor an UNRESOLVED roster reports for every pack id.
// It is deliberately NOT a MAN-002 version, so MeetsFloor can never place it in
// MKT-050a's order and therefore never satisfies it — an unresolved roster
// refuses every install and every removal, of every pack.
//
// It is exported because it surfaces in the operator-visible refusal
// (store.RequiredPackFloorError renders the floor), and a reader who sees
// `floor unresolved-roster` in a log should be able to find this line.
const UnresolvedFloor = "unresolved-roster"

// Roster is a host-provisioned required-pack roster: pack id → floor version
// (MKT-093a). It satisfies store.RequiredPacks, so the store enforces it inside
// the install and uninstall transactions while the version ORDER stays here,
// where MKT-050a's component-wise numeric comparison already lives.
//
// A RESOLVED but empty Roster makes no pack required. That is the contract's own
// default (MKT-093a) and the opposite of the trust anchors' fail-closed one: an
// empty anchor set withholds a permission and must refuse everything, while an
// empty roster withholds a restriction, and refusing everything there would mean
// declining to uninstall packs no deployment ever called essential.
//
// THE ZERO VALUE IS NOT THAT EMPTY ROSTER. MKT-093a draws a distinction the type
// itself has to carry: an absent or empty roster makes no pack required, but a
// roster the host cannot read, parse, or validate is UNRESOLVABLE and MUST NOT be
// treated as an empty one — the deployment did declare restrictions and the host
// failed to learn them, so proceeding would run with every floor silently lifted.
//
// So `resolved` is a field rather than an assumption, and it is spelled in the
// direction that makes the DANGEROUS state unreachable by accident: the zero
// value has resolved=false and refuses everything. A `var r Roster`, a
// `Roster{}`, and the value LoadRoster returns beside an error are all the same
// fail-closed thing. The only way to obtain a permissive roster is to have
// successfully constructed one through NewRoster, which is the only place
// resolved is set true. A caller who drops an error therefore gets a host that
// refuses pack mutations, never one that has quietly stopped protecting
// anything — the exact degradation the requirement's amendment closed.
type Roster struct {
	// floors is pack id → declared floor version, populated only on a resolved
	// roster. Unexported so no caller can hand the store a bare map whose nil
	// value would read as "nothing required" (the pre-amendment shape).
	floors map[string]string
	// resolved records that this value came from a roster the host actually read
	// and validated. See the type doc: false is the fail-closed state, and it is
	// the zero value on purpose.
	resolved bool
}

// NewRoster validates a declared roster at wiring time: every key a fully
// publisher-qualified pack id (MAN-001/MKT-008) and every floor a MAN-002
// version (MKT-093a). A malformed entry is refused HERE, loudly, at
// construction — rather than silently at every later install, which is what
// MeetsFloor's own fail-closed behaviour would otherwise degrade to.
//
// A nil or empty entries map yields a RESOLVED empty roster: the caller has
// declared, successfully, that nothing is required. That is different in kind
// from the zero Roster this function returns alongside an error, which is
// UNRESOLVED and refuses everything.
func NewRoster(entries map[string]string) (Roster, error) {
	out := make(map[string]string, len(entries))
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic first-offender reporting
	for _, id := range ids {
		// MAN-001's own grammar, not packsig.Namespace's deliberately coarse
		// split: a roster key that no installed pack id could ever equal protects
		// nothing, and would do it silently. `Waiveo/System`, `waiveo/sys/tem` and
		// a trailing space all pass a coarse check and match no pack, because
		// every installed id is pinned to MAN-001 by manifest.Validate.
		if !manifest.IsPublisherNameID(id) {
			return Roster{}, fmt.Errorf("packs: required-pack roster entry %q is not a manifest/1 MAN-001 pack id (<publisher>/<name>, each segment ^[a-z][a-z0-9-]{1,38}$) — a key no installed pack can equal would protect nothing", id)
		}
		floor := entries[id]
		if !ValidVersion(floor) {
			return Roster{}, fmt.Errorf("packs: required-pack roster entry %q declares floor %q, which is not a three-component MAJOR.MINOR.PATCH version (manifest/1 MAN-002, marketplace/1 MKT-093a)", id, floor)
		}
		out[id] = floor
	}
	return Roster{floors: out, resolved: true}, nil
}

// Resolved reports whether this roster came from a source the host successfully
// read and validated. A false answer is the UNRESOLVABLE state (MKT-093a): every
// pack reads as required at a floor nothing satisfies.
func (r Roster) Resolved() bool { return r.resolved }

// Declared renders the roster's entries as sorted `<pack_id>@<floor>` strings,
// for the one-line boot report an operator reads to confirm the deployment
// loaded the roster it meant to. An unresolved roster declares nothing — its
// refusals do not come from entries — so this is empty for it, and a caller that
// prints it must report Resolved separately.
func (r Roster) Declared() []string {
	if !r.resolved {
		return nil
	}
	out := make([]string, 0, len(r.floors))
	for id, floor := range r.floors {
		out = append(out, id+"@"+floor)
	}
	sort.Strings(out)
	return out
}

// RequiredFloor reports packID's declared floor and whether packID is required
// on this deployment at all (MKT-093a).
//
// An UNRESOLVED roster reports EVERY pack as required, at UnresolvedFloor —
// which no version satisfies — so the store refuses every install and every
// uninstall while it is wired. That is MKT-093a's "refuse every pack removal, if
// it is already running" branch, available as a value rather than as a second
// mode threaded through the store. It refuses installs too, which the
// requirement does not spell out: a host that does not know the floors cannot
// establish that an install leaves a required pack at or above one.
func (r Roster) RequiredFloor(packID string) (string, bool) {
	if !r.resolved {
		return UnresolvedFloor, true
	}
	floor, ok := r.floors[packID]
	return floor, ok
}

// MeetsFloor reports whether version is at or above floor under MKT-050a's
// component-wise numeric order.
//
// A version or floor outside MAN-002's grammar has NO position in that order
// (MKT-050a), so it never satisfies a floor: an unplaceable candidate is refused
// rather than compared under some fallback, and a roster entry whose floor
// somehow bypassed NewRoster refuses every install of that pack instead of
// silently protecting nothing.
//
// An unresolved roster answers false outright, before the comparison, rather
// than relying on UnresolvedFloor failing to parse. Two independent reasons to
// refuse, because one of them is a string constant somebody could later "fix"
// into a valid-looking version.
func (r Roster) MeetsFloor(version, floor string) bool {
	if !r.resolved {
		return false
	}
	return meetsFloor(version, floor)
}

// meetsFloor is Roster.MeetsFloor as a plain function, so the revert path's own
// candidate narrowing compares under the identical rule the store enforces with
// rather than a second, drifting copy of it.
func meetsFloor(version, floor string) bool {
	if !ValidVersion(version) || !ValidVersion(floor) {
		return false
	}
	return !VersionLower(version, floor)
}
