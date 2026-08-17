package macvendor

import "testing"

func TestVendorResolvesAKnownOUI(t *testing.T) {
	// The lab's fleet OUI, and the spelling the neighbour lane actually carries
	// (lowercase, colon-separated full MAC).
	if v, ok := Vendor("bc:24:11:3f:b9:4d"); !ok || v != "Proxmox" {
		t.Errorf("Vendor(bc:24:11:…) = %q,%v, want Proxmox,true", v, ok)
	}
	if v, ok := Vendor("6c:1f:f7:a6:e4:b6"); !ok || v != "Ugreen" {
		t.Errorf("Vendor(6c:1f:f7:…) = %q,%v, want Ugreen,true", v, ok)
	}
	if v, ok := Vendor("6c:1f:8a:00:00:01"); !ok || v != "Apple" {
		t.Errorf("Vendor(6c:1f:8a:…) = %q,%v, want Apple,true", v, ok)
	}
}

func TestVendorAcceptsEverySpelling(t *testing.T) {
	// One OUI, every separator and case — all must resolve to the same vendor,
	// because the neighbour lane's format is not the only one a caller may pass.
	for _, in := range []string{
		"50:3d:d1:aa:bb:cc",
		"50-3D-D1-AA-BB-CC",
		"503d.d1aa.bbcc",
		"503DD1AABBCC",
		"503dd1",
	} {
		if v, ok := Vendor(in); !ok || v != "TP-Link" {
			t.Errorf("Vendor(%q) = %q,%v, want TP-Link,true", in, v, ok)
		}
	}
}

// TestLocallyAdministeredMACHasNoVendor is the correctness guard the reference
// lab forced: two of its hosts carry privacy-randomized MACs (02:… and fa:…),
// whose first three octets are random and belong to no vendor. Looking them up
// would paste a real company's name onto a device that is not theirs.
func TestLocallyAdministeredMACHasNoVendor(t *testing.T) {
	for _, in := range []string{
		"02:db:78:11:22:33", // bit 0x02 set
		"fa:b1:ec:11:22:33", // fa = 1111_1010, bit 0x02 set
		"06:00:00:00:00:00",
		"3a:00:00:00:00:00", // 3a = 0011_1010, bit 0x02 set
	} {
		if v, ok := Vendor(in); ok {
			t.Errorf("Vendor(%q) = %q,true, want no vendor — a locally-administered MAC identifies no maker", in, v)
		}
	}
}

func TestMulticastMACHasNoVendor(t *testing.T) {
	// The multicast bit (0x01) also disqualifies an address as a NIC OUI. Even
	// if 01:00:5e were in the map, a multicast MAC is not a device.
	if v, ok := Vendor("01:00:5e:00:00:01"); ok {
		t.Errorf("Vendor(multicast) = %q,true, want no vendor", v)
	}
}

func TestUnknownOUIYieldsNoGuess(t *testing.T) {
	// A universal, well-formed MAC whose OUI is simply not curated must return
	// nothing rather than a nearest guess — the list stays honest.
	if v, ok := Vendor("08:11:22:33:44:55"); ok {
		t.Errorf("Vendor(uncurated) = %q,true, want no vendor", v)
	}
}

func TestVendorRejectsTooShort(t *testing.T) {
	for _, in := range []string{"", "bc", "bc:24", "bc2411"[:5]} {
		if v, ok := Vendor(in); ok {
			t.Errorf("Vendor(%q) = %q,true, want false — fewer than a full OUI", in, v)
		}
	}
}
