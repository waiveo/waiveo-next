package macvendor_test

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/macvendor"
)

// IsMAC exists because `native_id` is DRIVER-SPECIFIC — a MAC for a host the
// neighbour lane found, a protocol id for one a protocol lane named — and a
// caller publishing "the hardware address, when one is known" has to tell them
// apart. Vendor cannot do that job: it answers false for a perfectly good MAC
// whose OUI is uncurated, and for a randomized one, both of which ARE addresses.

func TestIsMACAcceptsTheSpellingsAMACArrivesIn(t *testing.T) {
	for _, s := range []string{
		"bc:24:11:3f:b9:4d",
		"BC:24:11:3F:B9:4D",
		"bc-24-11-3f-b9-4d",
		"bc24.113f.b94d",
		"bc24113fb94d",
	} {
		if !macvendor.IsMAC(s) {
			t.Errorf("IsMAC(%q) = false, want true", s)
		}
	}
}

func TestIsMACRefusesWhatIsNotAnAddress(t *testing.T) {
	cases := map[string]string{
		"an ECP serial": "X01500ABCDEF",
		"a UUID":        "uuid:roku:ecp:X015",
		"empty":         "",
		"too short":     "bc:24:11:3f:b9",
		"too long — not a longer MAC, something else": "bc:24:11:3f:b9:4d:5e",
		// normalize() stops at the first unrecognized character, so without the
		// second pass this would truncate to a valid-looking twelve.
		"twelve hex digits with a tail": "bc24113fb94dZZZ",
	}
	for what, s := range cases {
		if macvendor.IsMAC(s) {
			t.Errorf("%s: IsMAC(%q) = true, want false", what, s)
		}
	}
}

func TestCanonicalGivesOneSpelling(t *testing.T) {
	// One spelling matters because an operator reads and searches this value:
	// two rows spelling one address differently read as two devices.
	for _, s := range []string{"BC-24-11-3F-B9-4D", "bc24113fb94d", "bc24.113f.b94d"} {
		if got := macvendor.Canonical(s); got != "bc:24:11:3f:b9:4d" {
			t.Errorf("Canonical(%q) = %q", s, got)
		}
	}
	if got := macvendor.Canonical("X01500ABCDEF"); got != "" {
		t.Errorf("Canonical of a non-MAC = %q, want empty", got)
	}
}

func TestARandomizedAddressIsStillAnAddress(t *testing.T) {
	// The precise reason IsMAC is not "Vendor returned something": a
	// locally-administered MAC has no vendor by design, and is a MAC.
	const randomized = "ce:41:9a:7b:22:10"
	if !macvendor.IsMAC(randomized) {
		t.Error("a randomized address must still be recognized as a MAC")
	}
	if v, ok := macvendor.Vendor(randomized); ok {
		t.Errorf("a randomized address must resolve to no vendor, got %q", v)
	}
}
