// Package deviceid derives the platform identifiers a physical device and its
// entities are known by, from the identity relay/1 REL-153 scopes them to.
//
// REL-153 fixes a device's identity as the tuple `(site, driver, native_id)` —
// "never the relay that happens to currently report it" — and REL-110b requires
// both peers to compute the same `device_id`/`entity_id` from it. Derivation,
// rather than minting, is what makes those two sentences implementable at once:
//
//   - Two relays serving one site that both observe the same physical device
//     derive the SAME device_id, so the app peer's read model holds one row for
//     one device rather than one per reporting relay, and re-homing a device
//     between relays resolves to the id it already had (REL-153).
//   - The id survives a process restart and a full-set re-report (REL-111)
//     unchanged, so an operator's page cursor (api/1 API-034) and an in-flight
//     command's target id stay meaningful across both.
//   - The app peer never has to accept an identifier off the wire. A relay is
//     untrusted input; an id it supplied could name another relay's adopted
//     entity and redirect that entity's commands. Here the id is a function of
//     the identity the relay is asserting, computed on the app peer's own side,
//     so there is no field for a relay to put an id in — the read model's
//     intake carries none (internal/app/devices).
//   - The relay can compute the same value for a device it discovered but that
//     nobody has adopted yet, which is what lets a command against a
//     discovered entity resolve at the relay without the app peer first having
//     to tell it an id it never assigned (REL-110b).
//
// The derived value is a canonical ULID (data-model/1 DAT-005a) by
// construction, not by check: any 128 bits encode to one (ulid.FromBytes).
// What it is not is a ULID whose leading 48 bits are a creation timestamp —
// derived ids sort stably but not chronologically. Everything the platform asks
// of a row id needs a stable total order (keyset pagination) rather than a
// chronological one, and the row this id names has no creation instant on this
// side anyway: it exists when a relay observes it.
package deviceid

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"strings"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// Domain-separation tags. Device and Entity hash disjoint domains, so an entity
// derived under an empty key can never collide with its own device's id even
// though the two would otherwise share every input field.
const (
	deviceDomain = "waiveo-next/device-id/1"
	entityDomain = "waiveo-next/entity-id/1"
)

// Device returns the device_id for REL-153's identity tuple.
//
// site is the site's own scope-node id — the app peer's authoritative
// site_binding.scope_node (relay/1 REL-036), which a relay adopts from
// hello-ack, so both peers derive from the same value without either asserting
// it to the other.
func Device(site, driver, nativeID string) string {
	return derive(deviceDomain, site, driver, nativeID)
}

// Entity returns the entity_id for one entity of the device REL-153's tuple
// names. entityKey is the device-native key the relay addresses that entity by
// (REL-110a) — unique within its own device, which is what makes the four-part
// tuple unique platform-wide.
func Entity(site, driver, nativeID, entityKey string) string {
	return derive(entityDomain, site, driver, nativeID, entityKey)
}

// derive hashes domain plus every part under a LENGTH-PREFIXED encoding and
// encodes the leading 128 bits as a canonical ULID.
//
// The length prefixes are what make the encoding injective, and injectivity is
// the whole security property: without them ("roku", "ecp-1") and ("rok",
// "uecp-1") would hash identically, so one device's identity tuple could be
// spelled as another's and the two would derive the same id — one relay's
// device silently taking over another's row, and its commands. With a 4-byte
// big-endian length before each part, no two distinct part sequences share a
// preimage, so distinct identities derive distinct ids (up to SHA-256
// collision). The prefix is 8 bytes rather than 4 so no part length any caller
// could hold in memory can wrap it — a wrapped length would silently restore
// exactly the ambiguity the prefix exists to remove.
func derive(domain string, parts ...string) string {
	h := sha256.New()
	writePart(h, domain)
	for _, p := range parts {
		writePart(h, p)
	}
	var data [16]byte
	copy(data[:], h.Sum(nil))
	return ulid.FromBytes(data)
}

// writePart writes one length-prefixed part into h.
func writePart(h hash.Hash, part string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(part)))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(part))
}

// IdentityKey is the injective in-memory key for one REL-153 device identity.
//
// It exists so a caller deduplicating or indexing by identity uses the SAME
// notion of "these two records name one device" that Device/Entity derive ids
// under. A separator-joined key does not: a NUL is valid UTF-8 and may appear
// inside a native_id read off unauthenticated multicast, so ("a", "b\x00c") and
// ("a\x00b", "c") share a joined key while deriving different ids — which shows
// up as a report refused for naming one device twice when it named two.
//
// It is NOT an id and never reaches the wire: it is unencoded, may contain NULs,
// and carries no domain separation. Use Device/Entity for anything durable.
func IdentityKey(driver, nativeID string) string {
	var b strings.Builder
	var n [8]byte
	for _, part := range []string{driver, nativeID} {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		b.Write(n[:])
		b.WriteString(part)
	}
	return b.String()
}
