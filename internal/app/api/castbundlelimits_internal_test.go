package api

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/castbundle"
)

// TestTheCastBundleLimitsMatchTheUploadCeiling pins the derivation castbundle's
// size block states: an asset inside a bundle may be exactly as large as an
// asset this platform accepts through `POST /content`, because that door is how
// every asset in a bundle got onto its source box in the first place.
//
// It lives in an INTERNAL test file because maxContentUploadBytes is unexported
// and this is the only place both numbers are visible at once. Without it, the
// two ceilings are joined by a comment — which is precisely the arrangement that
// produced the finding this closes: a reader advertising 512 MiB, a route
// capping at 64 MiB, and a paragraph asserting they agreed.
func TestTheCastBundleLimitsMatchTheUploadCeiling(t *testing.T) {
	if castbundle.MaxAssetBytes != maxContentUploadBytes {
		t.Fatalf("castbundle.MaxAssetBytes = %d but maxContentUploadBytes = %d.\n"+
			"A bundle entry must be able to hold any asset this box would accept as an upload, or a cast built here "+
			"from a legitimate image exports to a bundle nothing can read.",
			int64(castbundle.MaxAssetBytes), int64(maxContentUploadBytes))
	}
	// And the whole-bundle limit must exceed the single-asset one, or the
	// original defect returns wearing a different constant: a bundle can never
	// be smaller than the largest thing it carries.
	if castbundle.MaxBundleBytes <= castbundle.MaxAssetBytes {
		t.Fatalf("castbundle.MaxBundleBytes = %d is not larger than one asset (%d); a bundle carrying a single full-size image would exceed its own limit",
			int64(castbundle.MaxBundleBytes), int64(castbundle.MaxAssetBytes))
	}
}
