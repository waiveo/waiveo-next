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

// Roster is a host-provisioned required-pack roster: pack id → floor version
// (MKT-093a). It satisfies store.RequiredPacks, so the store enforces it inside
// the install and uninstall transactions while the version ORDER stays here,
// where MKT-050a's component-wise numeric comparison already lives.
//
// A nil or empty Roster makes no pack required. That is the contract's own
// default (MKT-093a) and the opposite of the trust anchors' fail-closed one: an
// empty anchor set withholds a permission and must refuse everything, while an
// empty roster withholds a restriction, and refusing everything there would mean
// declining to uninstall packs no deployment ever called essential.
type Roster map[string]string

// NewRoster validates a declared roster at wiring time: every key a fully
// publisher-qualified pack id (MAN-001/MKT-008) and every floor a MAN-002
// version (MKT-093a). A malformed entry is refused HERE, loudly, at
// construction — rather than silently at every later install, which is what
// MeetsFloor's own fail-closed behaviour would otherwise degrade to.
func NewRoster(entries map[string]string) (Roster, error) {
	out := make(Roster, len(entries))
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
			return nil, fmt.Errorf("packs: required-pack roster entry %q is not a manifest/1 MAN-001 pack id (<publisher>/<name>, each segment ^[a-z][a-z0-9-]{1,38}$) — a key no installed pack can equal would protect nothing", id)
		}
		floor := entries[id]
		if !ValidVersion(floor) {
			return nil, fmt.Errorf("packs: required-pack roster entry %q declares floor %q, which is not a three-component MAJOR.MINOR.PATCH version (manifest/1 MAN-002, marketplace/1 MKT-093a)", id, floor)
		}
		out[id] = floor
	}
	return out, nil
}

// RequiredFloor reports packID's declared floor and whether packID is required
// on this deployment at all (MKT-093a).
func (r Roster) RequiredFloor(packID string) (string, bool) {
	floor, ok := r[packID]
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
func (r Roster) MeetsFloor(version, floor string) bool { return meetsFloor(version, floor) }

// meetsFloor is Roster.MeetsFloor as a plain function, so the revert path's own
// candidate narrowing compares under the identical rule the store enforces with
// rather than a second, drifting copy of it.
func meetsFloor(version, floor string) bool {
	if !ValidVersion(version) || !ValidVersion(floor) {
		return false
	}
	return !VersionLower(version, floor)
}
