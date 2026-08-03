package packs_test

import (
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// caplimits_test.go pins whether each artifact cap is INCLUSIVE — whether a
// value sitting exactly on the limit is admitted or refused.
//
// Every one of them is written with `>`, so exactly-at-the-limit passes, and
// every refusal message says "over the N-byte limit", which agrees. Nothing
// tested it: a boundary-mutation sweep flipped all five to `>=` and the package
// stayed green, so an implementation that refused a pack sitting exactly on a
// documented limit would have shipped looking correct.
//
// The limits are set to the value a fixture actually produces rather than the
// fixture being built to an exact size — a zip's own framing makes the second
// approach fragile, and the property under test is the comparison, not the
// arithmetic.

func capFixture(t *testing.T) (artifact []byte, entries map[string]string, totalUncompressed int64) {
	t.Helper()
	entries = map[string]string{
		"manifest.json": strings.Repeat("m", 40),
		"a.json":        strings.Repeat("a", 30),
		"b.json":        strings.Repeat("b", 20),
	}
	for _, body := range entries {
		totalUncompressed += int64(len(body))
	}
	return filesZip(t, entries), entries, totalUncompressed
}

// TestArtifactByteCapIsInclusive: an artifact whose compressed size is EXACTLY
// the cap is admitted; one byte less of allowance refuses it.
func TestArtifactByteCapIsInclusive(t *testing.T) {
	artifact, _, _ := capFixture(t)
	size := int64(len(artifact))

	atCap := packs.DefaultLimits
	atCap.MaxArtifactBytes = size
	if _, err := packs.ReadBundle(artifact, atCap); err != nil {
		t.Errorf("an artifact of exactly %d bytes was refused under a %d-byte cap: %v — the limit is written `>` "+
			"and its message says OVER the limit, so the boundary belongs to the artifact", size, size, err)
	}

	below := packs.DefaultLimits
	below.MaxArtifactBytes = size - 1
	if _, err := packs.ReadBundle(artifact, below); err == nil {
		t.Errorf("an artifact of %d bytes was admitted under a %d-byte cap", size, size-1)
	}
}

// TestEntryCountCapIsInclusive: an artifact with exactly MaxFiles entries is
// admitted. Off by one here refuses a pack that names precisely as many files as
// the documented maximum.
func TestEntryCountCapIsInclusive(t *testing.T) {
	artifact, entries, _ := capFixture(t)
	n := len(entries)

	atCap := packs.DefaultLimits
	atCap.MaxFiles = n
	if _, err := packs.ReadBundle(artifact, atCap); err != nil {
		t.Errorf("an artifact with exactly %d entries was refused under a %d-entry cap: %v", n, n, err)
	}

	below := packs.DefaultLimits
	below.MaxFiles = n - 1
	if _, err := packs.ReadBundle(artifact, below); err == nil {
		t.Errorf("an artifact with %d entries was admitted under a %d-entry cap", n, n-1)
	}
}

// TestPerEntryByteCapIsInclusive: an entry whose uncompressed size is exactly
// MaxFileBytes is admitted. This is the cap a pack author reads as "1 MiB per
// file", so a file of exactly that size has to work.
func TestPerEntryByteCapIsInclusive(t *testing.T) {
	artifact, entries, _ := capFixture(t)
	largest := 0
	for _, body := range entries {
		if len(body) > largest {
			largest = len(body)
		}
	}

	atCap := packs.DefaultLimits
	atCap.MaxFileBytes = int64(largest)
	if _, err := packs.ReadBundle(artifact, atCap); err != nil {
		t.Errorf("an entry of exactly %d bytes was refused under a %d-byte per-entry cap: %v", largest, largest, err)
	}

	below := packs.DefaultLimits
	below.MaxFileBytes = int64(largest) - 1
	if _, err := packs.ReadBundle(artifact, below); err == nil {
		t.Errorf("an entry of %d bytes was admitted under a %d-byte per-entry cap", largest, largest-1)
	}
}

// TestTotalUncompressedCapIsInclusive: the sum of every entry's uncompressed
// size may reach the cap exactly. It is checked as a RUNNING total during
// extraction, so the boundary belongs to the last entry that fits rather than to
// the artifact as a whole.
func TestTotalUncompressedCapIsInclusive(t *testing.T) {
	artifact, _, total := capFixture(t)

	atCap := packs.DefaultLimits
	atCap.MaxTotalUncompressed = total
	if _, err := packs.ReadBundle(artifact, atCap); err != nil {
		t.Errorf("an artifact uncompressing to exactly %d bytes was refused under a %d-byte total cap: %v",
			total, total, err)
	}

	below := packs.DefaultLimits
	below.MaxTotalUncompressed = total - 1
	if _, err := packs.ReadBundle(artifact, below); err == nil {
		t.Errorf("an artifact uncompressing to %d bytes was admitted under a %d-byte total cap", total, total-1)
	}
}
