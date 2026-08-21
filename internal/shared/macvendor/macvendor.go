// Package macvendor turns a MAC address into the name of the organization that
// owns its OUI — the first three octets, an IEEE-assigned block (MA-L). It is a
// DISPLAY aid for discovery: a host the network reveals only as a bare MAC
// ("net bc:24:11:3f:b9:4d") is far more legible named for its maker
// ("Proxmox bc:24:11:3f:b9:4d"), which is the difference between "35 unknown
// things" and "seeing everything on the network" (discovery spec §2/§7, the
// vendor half of build-order step 2's "MAC/vendor (OUI)").
//
// # Why a curated map rather than the full IEEE registry
//
// The full registry is ~35k assignments — a megabyte of generated table for a
// display nicety. This is instead a curated set of the vendors a home/AV/IoT/
// networking deployment actually meets, seeded from authoritative IEEE data
// (api.macvendors.com, which serves the registry). Every entry is the real
// assignee for that block; an OUI absent from the map yields no vendor rather
// than a guess, so the list is always honest and simply grows as new hardware
// appears. Swapping in the full registry later is a change to this one map and
// nothing else.
//
// # Why a randomized MAC gets no vendor
//
// A MAC whose first octet has the locally-administered bit set (0x02) was NOT
// assigned by IEEE to a vendor — it is administered locally, and in practice is
// a PRIVACY-RANDOMIZED address a phone or laptop rotates so it cannot be
// tracked across networks. Its first three octets are random and belong to no
// one, so looking them up would attach a real vendor's name to a device that is
// not theirs. Such an address is reported as having no vendor, which is the
// truth: nothing about a randomized MAC identifies its maker.
package macvendor

import "strings"

// byOUI maps a normalized 6-hex-lowercase OUI to a short display name. Every
// value is the IEEE assignee for that block, shortened to the name an operator
// would recognize (IEEE's own strings carry legal suffixes — "Roku, Inc.",
// "GIGA-BYTE TECHNOLOGY CO.,LTD." — that are noise in a device list). Grouped by
// vendor; a vendor owns many blocks and this carries the common ones.
var byOUI = map[string]string{
	// Seen in the reference lab, verified against IEEE — the fleet these names
	// first made legible (a wall of "net bc:24:11:…" is 15 Proxmox VMs).
	"bc2411": "Proxmox",
	"bcb923": "Alta Networks",
	"3c0b59": "Tuya",
	"503dd1": "TP-Link",
	"d85ed3": "Gigabyte",
	"3cefa5": "Cloud Network Tech",
	"6c1ff7": "Ugreen",

	// Apple.
	"000393": "Apple",
	"3c0754": "Apple",
	"6c1f8a": "Apple",
	// Samsung.
	"0000f0": "Samsung",
	// Google / Nest.
	"001a11": "Google",
	"3c5ab4": "Google",
	"18b430": "Nest",
	// Amazon.
	"44650d": "Amazon",
	"fc65de": "Amazon",
	// Roku.
	"b0a737": "Roku",
	"dc3a5e": "Roku",
	// Espressif (the ESP32/ESP8266 in countless IoT devices) and Silicon Labs.
	"240ac4": "Espressif",
	"8caab5": "Espressif",
	"ec1bbd": "Silicon Labs",
	// Raspberry Pi.
	"b827eb": "Raspberry Pi",
	"dca632": "Raspberry Pi",
	"e45f01": "Raspberry Pi",
	// Home AV / smart home.
	"000e58": "Sonos",
	"001788": "Philips",
	"645299": "Chamberlain",
	// Networking gear.
	"00156d": "Ubiquiti",
	"fcecda": "Ubiquiti",
	"50c7bf": "TP-Link",
	"00095b": "Netgear",
	"001e6b": "Cisco",
	// Consoles, TVs, PCs.
	"001ae9": "Nintendo",
	"0013a9": "Sony",
	"001c62": "LG",
	"001b21": "Intel",
	"00155d": "Microsoft",
}

// Vendor returns the organization that owns mac's OUI and true, or "" and false
// when mac is not a usable universal address or its OUI is not in the curated
// map. It accepts a MAC in any common spelling (colons, dashes, dots, or none;
// any case).
//
// A locally-administered address (first-octet bit 0x02) returns no vendor by
// design — see the package doc: its OUI is random and identifies no maker.
func Vendor(mac string) (string, bool) {
	hex := normalize(mac)
	if len(hex) < 6 {
		return "", false
	}
	// The first octet is hex[0:2]. Its low two bits are the multicast (0x01) and
	// locally-administered (0x02) flags; a real vendor NIC is unicast and
	// universally administered, so anything with either bit set is not an
	// IEEE-assigned OUI and must not be looked up.
	first, ok := hexByte(hex[0], hex[1])
	if !ok || first&0x03 != 0 {
		return "", false
	}
	name, ok := byOUI[hex[:6]]
	return name, ok
}

// IsMAC reports whether s spells a 48-bit hardware address, in any of the
// spellings Vendor accepts.
//
// It exists because a device's `native_id` is DRIVER-SPECIFIC: for a host the
// network revealed by its neighbour entry it is the MAC, and for a device a
// protocol lane named it is that protocol's own id (an ECP serial, a UUID). A
// caller that wants to publish "the hardware address, when one is known" has to
// be able to tell those apart, and it cannot use Vendor for the job — Vendor
// answers false for a perfectly good MAC whose OUI is simply not curated, and
// for a randomized address, both of which ARE addresses.
//
// Twelve hex digits exactly: a longer run is not a longer MAC, it is something
// else that happens to start like one.
func IsMAC(s string) bool {
	hex := normalize(s)
	if len(hex) != 12 {
		return false
	}
	// normalize stops at the first character it does not recognize, so a
	// trailing non-hex tail would silently truncate to a valid-looking 12. Check
	// the input had nothing after the address it spelled.
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r == ':', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// Canonical returns s as the one spelling this platform publishes — lowercase
// hex, colon-separated — or "" when s is not a MAC. One spelling matters
// because the value is shown to an operator and searched by them: two rows
// spelling the same address differently read as two devices.
func Canonical(s string) string {
	if !IsMAC(s) {
		return ""
	}
	hex := normalize(s)
	var b strings.Builder
	b.Grow(17)
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hex[i : i+2])
	}
	return b.String()
}

// normalize strips the separators a MAC may carry (`:` `-` `.`) and lowercases
// the rest, returning only the hex run. A non-hex character ends the address —
// the caller has already length-checked what it needs.
func normalize(mac string) string {
	var b strings.Builder
	b.Grow(len(mac))
	for _, r := range strings.ToLower(mac) {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			b.WriteRune(r)
		case r == ':' || r == '-' || r == '.':
			// separator: drop it
		default:
			return b.String()
		}
	}
	return b.String()
}

// hexByte decodes two lowercase-hex digits to a byte.
func hexByte(hi, lo byte) (byte, bool) {
	h, ok1 := hexNibble(hi)
	l, ok2 := hexNibble(lo)
	if !ok1 || !ok2 {
		return 0, false
	}
	return h<<4 | l, true
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	default:
		return 0, false
	}
}
