package archive1_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/conformance/drivers/archive1"
	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
)

// expectedDriven is every archive-1 case this driver executes against the live
// internal/archive implementation.
var expectedDriven = []string{
	"ARC-014-invalid-decrypt-failed-wrong-passphrase",
	"ARC-016-invalid-truncated-tail-rejected",
	"ARC-023-invalid-signature-verification-failed",
	"ARC-060-valid-assets-by-reference",
}

// expectedPending is every case this driver deliberately does not drive, and each
// one is here because a specific thing does not exist yet — not because it is
// hard. Pinned as a set so a case leaving it is noticed as progress and a case
// joining it cannot happen quietly.
//
//   - ARC-041 / ARC-102 / ARC-103: restore-time refusals, and there is no restore
//     path (internal/archive.Open has no caller outside its own tests).
//   - ARC-031: the case declares an embedded asset whose asset_ref hex is 63
//     characters, so no bytes can hash to it and no round trip can carry it.
//   - ARC-091: Create implements full mode only; nothing can write an incremental
//     archive to read back.
var expectedPending = []string{
	"ARC-031-valid-manifest-full",
	"ARC-041-invalid-epoch-mismatch",
	"ARC-091-valid-manifest-incremental",
	"ARC-102-invalid-yanked-pack-blocked",
	"ARC-103-invalid-dev-channel-refused",
}

// TestArchive1DriverGreen replays the frozen archive-1 corpus against the live
// container implementation: every case is either driven and PASSing, or PENDING
// with a stated reason.
func TestArchive1DriverGreen(t *testing.T) {
	rep := archive1.Run()
	t.Logf("\n%s", rep.String())

	driven := rep.Driven()
	sort.Strings(driven)
	if !equalStrings(driven, expectedDriven) {
		t.Errorf("driven set = %v, want %v", driven, expectedDriven)
	}
	pending := rep.PendingIDs()
	sort.Strings(pending)
	if !equalStrings(pending, expectedPending) {
		t.Errorf("pending set = %v, want %v", pending, expectedPending)
	}
	if !rep.OK() {
		t.Errorf("report not OK:\n%s", rep.String())
	}
}

// TestEveryPendingCaseSaysWhy: a PENDING case with a vague reason is worse than a
// failing one, because it reads as considered. Each reason must name the thing
// that is missing.
func TestEveryPendingCaseSaysWhy(t *testing.T) {
	rep := archive1.Run()
	for _, c := range rep.Cases {
		if c.Status != "PENDING" {
			continue
		}
		if len(c.Reason) < 40 {
			t.Errorf("%s: pending reason is %d chars, too short to name what is missing: %q", c.CaseID, len(c.Reason), c.Reason)
		}
		// Every reason should point at a specific absence, not at effort.
		for _, banned := range []string{"not implemented", "TODO", "later", "hard"} {
			if strings.Contains(strings.ToLower(c.Reason), banned) {
				t.Errorf("%s: pending reason says %q, which describes effort rather than what is missing", c.CaseID, banned)
			}
		}
	}
}

// TestArchive1DriverHasTeeth is the guard against a driver that reports PASS
// whatever the implementation does. It corrupts one case's `expected` block in
// memory and confirms the SAME comparison logic — reached through RunCases, never
// re-implemented here — reports FAIL against the corrupted expectation.
func TestArchive1DriverHasTeeth(t *testing.T) {
	cases, err := archive1.LoadCorpus()
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}

	const victim = "ARC-014-invalid-decrypt-failed-wrong-passphrase"
	c, ok := cases[victim]
	if !ok {
		t.Fatalf("corpus has no %s", victim)
	}
	// Claim the wrong code. A driver with teeth must notice.
	corrupted := corpus.Case{
		CaseID: c.CaseID, Contract: c.Contract, ReqIDs: c.ReqIDs, Description: c.Description,
		Input:    c.Input,
		Expected: map[string]any{"error": map[string]any{"code": "ARCHIVE_TRUNCATED"}},
	}
	rep := archive1.RunCases(map[string]corpus.Case{victim: corrupted})
	if rep.OK() {
		t.Errorf("driver reported OK against a case claiming the wrong code:\n%s", rep.String())
	}
}

// TestAnUnknownCaseIsAFailureNotASkip: the corpus is the source of truth, so a
// case the driver has never heard of must be loud. A silent skip is how a frozen
// case ends up driven by nobody while every report looks green.
func TestAnUnknownCaseIsAFailureNotASkip(t *testing.T) {
	rep := archive1.RunCases(map[string]corpus.Case{
		"ARC-999-valid-a-case-nobody-taught-the-driver": {CaseID: "ARC-999-valid-a-case-nobody-taught-the-driver"},
	})
	if rep.OK() {
		t.Errorf("an unknown case did not fail the report:\n%s", rep.String())
	}
	if len(rep.PendingIDs()) != 0 {
		t.Errorf("an unknown case was recorded PENDING, which reads as deliberate: %v", rep.PendingIDs())
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
