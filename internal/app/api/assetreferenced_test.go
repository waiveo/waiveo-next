package api

import (
	"errors"
	"testing"
)

// The failure posture, tested rather than inferred. A boolean has no "unknown",
// so when the reference set cannot be read the value has to lean — and the two
// mistakes are not equally bad: calling in-use content unreferenced invites an
// operator to delete a live asset, while calling an orphan referenced merely
// withholds one cleanup.

func TestAnUnreadableReferenceSetReportsEVERYTHINGReferenced(t *testing.T) {
	if !assetReferenced(errors.New("the store said no"), nil, "abc") {
		t.Fatal("a listing that could not read the reference set reported an asset as " +
			"UNREFERENCED — an operator reading that would treat content in use as disposable")
	}
}

func TestAReadableSetIsHonoured(t *testing.T) {
	digests := map[string]bool{"used": true}
	if !assetReferenced(nil, digests, "used") {
		t.Error("a referenced asset reported unreferenced")
	}
	if assetReferenced(nil, digests, "orphan") {
		t.Error("an orphan reported referenced — the member says nothing and the sweep " +
			"looks broken when it reclaims it")
	}
}
