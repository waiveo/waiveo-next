package devices

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// naming_test.go pins candidateName's fallback: a host revealed only as a bare
// MAC should read as its maker when the OUI is known, and honestly as the bare
// address when it is not — and a device that named itself must never be
// overridden by either.

func TestCandidateNameUsesVendorForABareMAC(t *testing.T) {
	// A net host with no self-reported name and a recognized OUI: the driver
	// token "net" gives way to the vendor, and the MAC stays for identity.
	c := candidate("net", "bc:24:11:3f:b9:4d")
	if got := candidateName(c); got != "Proxmox bc:24:11:3f:b9:4d" {
		t.Errorf("candidateName = %q, want %q", got, "Proxmox bc:24:11:3f:b9:4d")
	}
}

func TestCandidateNameFallsBackForUnknownAndRandomMACs(t *testing.T) {
	// An uncurated OUI and a privacy-randomized MAC both keep the honest bare
	// form — no guess, and no real vendor pasted onto a random address.
	for _, tc := range []struct{ nativeID, want string }{
		{"08:11:22:33:44:55", "net 08:11:22:33:44:55"}, // universal but uncurated
		{"02:db:78:11:22:33", "net 02:db:78:11:22:33"}, // locally administered (random)
		{"fa:b1:ec:11:22:33", "net fa:b1:ec:11:22:33"}, // locally administered (random)
	} {
		c := candidate("net", tc.nativeID)
		if got := candidateName(c); got != tc.want {
			t.Errorf("candidateName(%s) = %q, want %q", tc.nativeID, got, tc.want)
		}
	}
}

func TestCandidateNameNeverOverridesASelfReportedName(t *testing.T) {
	// Even with a recognized OUI, a device that told the network its own name
	// keeps it — the vendor only ever fills the fallback the driver token used to.
	c := candidate("net", "bc:24:11:3f:b9:4d")
	c.Name = "Media Server"
	if got := candidateName(c); got != "Media Server" {
		t.Errorf("candidateName = %q, want the self-reported %q", got, "Media Server")
	}
}

func TestCandidateNameLeavesNonMACDriversAlone(t *testing.T) {
	// A driver whose native id is not a MAC (a Roku ECP UUID) is untouched by the
	// vendor path — the lookup simply does not resolve, and the tuple is spelled
	// out as before.
	c := wire.DeviceCandidate{Driver: "roku-ecp", NativeID: "uuid:roku:ecp:X1"}
	if got := candidateName(c); got != "roku-ecp uuid:roku:ecp:X1" {
		t.Errorf("candidateName = %q, want %q", got, "roku-ecp uuid:roku:ecp:X1")
	}
}
