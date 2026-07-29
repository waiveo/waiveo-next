package store

import "fmt"

// packsfloor.go is the store half of marketplace/1 MKT-093a/MKT-093b: the
// required-pack floor, enforced inside the install and uninstall transactions.
//
// The roster lives on the Store rather than arriving per call. That is the whole
// point of MKT-093b's "enforced where a caller cannot step around it": an
// InstallGuard-style precondition is only as good as every caller remembering to
// pass it, and there is exactly one way to write a packs row (InstallPack) and
// exactly one way to remove one (UninstallPack). Hanging the check off those two
// transactions means a future caller inherits it by existing, not by remembering.
//
// The store deliberately does NOT know a version grammar. MKT-050a fixes the
// order (component-wise numeric over MAJOR.MINOR.PATCH), and a store-side string
// comparison would be exactly the lexicographic order that requirement forbids —
// so the comparison, like ChannelMarkAdvance.Higher's, is the caller's, supplied
// through the RequiredPacks seam.

// RequiredPacks resolves a deployment's required-pack roster (marketplace/1
// MKT-093a): which packs hold tier-granted "Required" status here, and the floor
// version each may not be uninstalled or regressed below.
//
// It is a host-provisioned resolution seam, the same shape MKT-009b gives its
// trust anchors: required status is never learned from an artifact, an index
// entry, or the party presenting one. A NIL roster — the default — makes no pack
// required, which is deliberately the opposite of the anchors' fail-closed
// default (MKT-093a): an absent anchor set withholds a permission, so it must
// refuse; an absent roster withholds a restriction, so failing closed there would
// refuse to remove packs no deployment ever called essential.
type RequiredPacks interface {
	// RequiredFloor returns packID's floor version and whether packID is on the
	// roster at all.
	RequiredFloor(packID string) (floor string, required bool)
	// MeetsFloor reports whether version is at or above floor under the caller's
	// own version order (MKT-050a). It is the caller's job to fail closed on a
	// version or floor it cannot place in that order.
	MeetsFloor(version, floor string) bool
}

// RequiredPackFloorError is the typed refusal MKT-093b names REQUIRED_PACK_FLOOR:
// an uninstall of a required pack, or an install/update/revert that would leave
// one below its floor. Version is empty for the uninstall case — removal is
// removal to NO version, which is below every floor.
type RequiredPackFloorError struct {
	PackID  string
	Floor   string
	Version string
}

func (e *RequiredPackFloorError) Error() string {
	if e.Version == "" {
		return fmt.Sprintf("store: %s is a required pack (floor %s) and cannot be uninstalled (marketplace/1 MKT-093b)", e.PackID, e.Floor)
	}
	return fmt.Sprintf("store: %s is a required pack whose floor is %s; version %s is below it (marketplace/1 MKT-093b)",
		e.PackID, e.Floor, e.Version)
}

// SetRequiredPacks installs the deployment's required-pack roster (MKT-093a).
// Intended for wiring at construction, before the store serves anything; it
// takes the write lock so a later call cannot race a reader.
func (s *Store) SetRequiredPacks(r RequiredPacks) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.required = r
}

// RequiredFloor reports the floor version this deployment requires for packID,
// and whether packID is required at all (MKT-093a).
//
// This is the READ accessor a caller uses to narrow its own candidates — for
// instance to skip a revert target it already knows is below the floor. It is
// NOT where the rule is enforced: this read happens outside the write lock, so
// it can go stale between the read and the write it informs. The enforcing check
// runs inside the install/uninstall transaction (MKT-093b).
func (s *Store) RequiredFloor(packID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.required == nil {
		return "", false
	}
	return s.required.RequiredFloor(packID)
}

// checkRequiredFloor is the in-transaction floor assertion (MKT-093b). It is
// called with the store write lock already held (from inside writeTx), so it
// reads s.required directly rather than through the RLock accessor — taking the
// same mutex again would deadlock, and the value cannot change under a lock the
// caller holds.
//
// version == "" means "the pack is being removed": below every floor.
func (s *Store) checkRequiredFloor(packID, version string) error {
	if s.required == nil {
		return nil
	}
	floor, required := s.required.RequiredFloor(packID)
	if !required {
		return nil
	}
	if version != "" && s.required.MeetsFloor(version, floor) {
		return nil
	}
	return &RequiredPackFloorError{PackID: packID, Floor: floor, Version: version}
}
