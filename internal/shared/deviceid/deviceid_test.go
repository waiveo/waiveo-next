package deviceid

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

const (
	siteA = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	siteB = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z6"
)

// TestDerivedIdsAreCanonicalULIDs is data-model/1 DAT-005a on the derivation
// itself: every id it can produce — including from adversarial inputs a relay
// could put on the wire (empty parts, non-UTF-8 bytes, very long strings) — must
// satisfy ulid.Valid, because the registry's rows are paged by a keyset cursor
// that assumes it (api/1 API-034/API-045).
//
// This is the structural half of the claim in the package doc: derivation
// cannot emit a non-ULID, so nothing downstream has to check for one.
func TestDerivedIdsAreCanonicalULIDs(t *testing.T) {
	long := make([]byte, 4096)
	for i := range long {
		long[i] = byte(i)
	}
	cases := []struct{ site, driver, native, key string }{
		{siteA, "roku-ecp", "uuid:roku:ecp:X1", "main"},
		{"", "", "", ""},
		{siteA, "\x00\xff\xfe", "\xc3\x28", "\n\t"},
		{siteA, string(long), string(long), string(long)},
		{siteA, "driver with spaces", "native/with/slashes", "key:with:colons"},
	}
	for _, c := range cases {
		if got := Device(c.site, c.driver, c.native); !ulid.Valid(got) {
			t.Errorf("Device(%q,%q,%q) = %q, not a canonical ULID", c.site, c.driver, c.native, got)
		}
		if got := Entity(c.site, c.driver, c.native, c.key); !ulid.Valid(got) {
			t.Errorf("Entity(%q,%q,%q,%q) = %q, not a canonical ULID", c.site, c.driver, c.native, c.key, got)
		}
	}
}

// TestDeviceIdIsStableAndIndependentOfTheReportingRelay is REL-153: the id is a
// function of (site, driver, native_id) only. The reporting relay is not an
// input at all — there is no parameter for it — so two relays observing one
// device necessarily agree, and so does the same relay across restarts.
func TestDeviceIdIsStableAndIndependentOfTheReportingRelay(t *testing.T) {
	first := Device(siteA, "roku-ecp", "uuid:roku:ecp:X1")
	second := Device(siteA, "roku-ecp", "uuid:roku:ecp:X1")
	if first != second {
		t.Fatalf("two derivations of one identity differ: %q vs %q", first, second)
	}
}

// TestDistinctIdentitiesDeriveDistinctIds covers the axes that must separate:
// site, driver, native_id, entity key, and the device/entity domains
// themselves.
func TestDistinctIdentitiesDeriveDistinctIds(t *testing.T) {
	base := Device(siteA, "roku-ecp", "X1")
	for name, got := range map[string]string{
		"different site":      Device(siteB, "roku-ecp", "X1"),
		"different driver":    Device(siteA, "roku-ecp2", "X1"),
		"different native_id": Device(siteA, "roku-ecp", "X2"),
	} {
		if got == base {
			t.Errorf("%s derived the same device_id %q — identities must separate", name, got)
		}
	}

	e1 := Entity(siteA, "roku-ecp", "X1", "main")
	e2 := Entity(siteA, "roku-ecp", "X1", "aux")
	if e1 == e2 {
		t.Errorf("two entity keys of one device derived the same entity_id %q", e1)
	}
	// Domain separation: an entity derived under the EMPTY key shares every
	// other input with its own device, so only the domain tag separates them.
	if got := Entity(siteA, "roku-ecp", "X1", ""); got == base {
		t.Errorf("entity_id under an empty key equals its device_id (%q) — the two id domains are not separated", got)
	}
}

// TestFieldBoundariesAreUnambiguous is the injectivity guard the length prefix
// exists for. Without it, the parts are concatenated and ("roku","ecp1") hashes
// exactly as ("rokue","cp1") — so one relay could spell another device's
// identity tuple differently and derive its id, taking over its registry row and
// its commands.
//
// Disable the prefix (write only the part bytes in writePart) and this test
// fails: the pairs below are chosen so their naive concatenations are equal.
func TestFieldBoundariesAreUnambiguous(t *testing.T) {
	pairs := [][2][2]string{
		{{"roku", "ecp1"}, {"rokue", "cp1"}},
		{{"a", "bc"}, {"ab", "c"}},
		{{"", "ab"}, {"ab", ""}},
	}
	for _, p := range pairs {
		left := Device(siteA, p[0][0], p[0][1])
		right := Device(siteA, p[1][0], p[1][1])
		if left == right {
			t.Errorf("(%q,%q) and (%q,%q) derive the same device_id %q — field boundaries are ambiguous",
				p[0][0], p[0][1], p[1][0], p[1][1], left)
		}
	}
	// The same ambiguity across the site/driver boundary, and across the
	// device's own trailing field and an entity key.
	if Device(siteA+"x", "d", "n") == Device(siteA, "xd", "n") {
		t.Error("the site/driver boundary is ambiguous")
	}
	if Entity(siteA, "d", "na", "b") == Entity(siteA, "d", "n", "ab") {
		t.Error("the native_id/entity-key boundary is ambiguous")
	}
}

// TestDerivationIsPinned freezes the exact strings the current derivation
// produces for one identity. Both peers must agree byte-for-byte (REL-110b), so
// a change to the domain tags, the field order, or the prefix width is a
// protocol change between the app peer and every relay — not an implementation
// detail. If this test fails, that is what changed.
func TestDerivationIsPinned(t *testing.T) {
	const (
		wantDevice = "64WAW3WA9KGEF52VK8J0RDVR0F"
		wantEntity = "61S6E56TY8C5VV0KWSZP7GZDET"
	)
	if got := Device(siteA, "roku-ecp", "uuid:roku:ecp:X1"); got != wantDevice {
		t.Errorf("Device = %q, want %q (the derivation changed; every relay must change with it)", got, wantDevice)
	}
	if got := Entity(siteA, "roku-ecp", "uuid:roku:ecp:X1", "main"); got != wantEntity {
		t.Errorf("Entity = %q, want %q (the derivation changed; every relay must change with it)", got, wantEntity)
	}
}
