package player1

import (
	"errors"

	"github.com/maaxton/waiveo-next/internal/virtualplayer"
)

// VirtualPlayerTarget is the first-photon PlayerTarget: the in-process
// virtual player (internal/virtualplayer.Photon), the software photon this
// wave ships. It is the concrete target the driver validates now; a later
// wave plugs a BrightScript PlayerTarget into the exact same driver.
//
// The adapter is deliberately thin: it maps virtualplayer.Photon's
// (bytes, error) result onto the driver's PairResult vocabulary, classifying
// an ErrCommitmentMismatch as the specific OOB-commitment rejection the
// PLY-057 assertion distinguishes. It observes nothing of the player's
// internals — exactly what a wire-only BrightScript adapter would also do.
type VirtualPlayerTarget struct{}

// NewVirtualPlayerTarget returns the first-photon virtual-player target.
func NewVirtualPlayerTarget() VirtualPlayerTarget { return VirtualPlayerTarget{} }

// Name implements PlayerTarget.
func (VirtualPlayerTarget) Name() string { return "virtualplayer" }

// Pair implements PlayerTarget by running the full virtualplayer.Photon
// thread for pairingCode.
func (VirtualPlayerTarget) Pair(pairingCode string) PairResult {
	bytes, err := virtualplayer.Photon(pairingCode)
	if err != nil {
		return PairResult{
			Rejected:           true,
			CommitmentMismatch: errors.Is(err, virtualplayer.ErrCommitmentMismatch),
			Err:                err.Error(),
		}
	}
	return PairResult{Completed: len(bytes) > 0}
}
