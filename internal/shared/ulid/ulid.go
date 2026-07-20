// Package ulid mints canonical ULIDs (https://github.com/ulid/spec): 128
// bits — a 48-bit big-endian millisecond Unix timestamp followed by 80 bits
// of crypto/rand randomness — Crockford-base32 encoded as a 26-character
// string. It exists for PLY-097: unlike this codebase's other opaque ids
// (screen_id, channel_token, grant_id, relay_id — each only required to be
// "opaque" by its own contract), a Lease's lease_id is contractually
// required to be a real ULID, so `lease-<hex>` (this codebase's usual
// newOpaqueToken shape) doesn't satisfy it. No external dependency: the
// codebase avoids adding one for a ~30-line encoder, and there's no ULID
// library already in go.mod.
package ulid

import (
	"crypto/rand"
	"strings"
	"time"
)

// crockfordAlphabet is the ULID spec's encoding alphabet: the 32 symbols
// 0-9 and A-Z minus I, L, O, U (dropped to avoid visual confusion with
// 1/1/0/V), in ascending order — which is what makes a lexicographic
// string comparison of two same-length ULIDs agree with the numeric
// comparison of the values they encode.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns a fresh canonical ULID: 26 characters, Crockford-base32,
// encoding a 48-bit big-endian millisecond timestamp (time.Now().UnixMilli)
// followed by 80 bits of crypto/rand randomness. Two calls in the same
// millisecond differ (with overwhelming probability) purely from the
// random tail; the timestamp prefix is monotonic non-decreasing across
// calls made in wall-clock order.
func New() string {
	var data [16]byte

	ms := uint64(time.Now().UnixMilli())
	data[0] = byte(ms >> 40)
	data[1] = byte(ms >> 32)
	data[2] = byte(ms >> 24)
	data[3] = byte(ms >> 16)
	data[4] = byte(ms >> 8)
	data[5] = byte(ms)

	if _, err := rand.Read(data[6:]); err != nil {
		// crypto/rand.Reader failing is a fatal environment problem
		// (entropy source unavailable); there is no meaningful error to
		// propagate through this value-returning helper — the same
		// convention playerserver.newOpaqueToken already uses.
		panic("ulid: New: " + err.Error())
	}

	return encode(data)
}

// Valid reports whether s is a syntactically valid canonical ULID — the
// inverse check to New/encode. A canonical ULID is exactly 26 characters, each
// one of the 32 uppercase Crockford base32 symbols crockfordAlphabet lists
// (I, L, O, U excluded; lowercase and any other byte rejected), and its leading
// character is in 0..7. The leading-character bound matters because 26*5 = 130
// bits hold a 128-bit value behind two zero pad bits, so the first symbol
// carries only 3 significant bits — a value above 7 there would overflow the
// 48-bit timestamp past the representable width (the spec's largest ULID is
// 7ZZZZZZZZZZZZZZZZZZZZZZZZZ). It backs api/1's "syntactically valid ULID"
// requirements (API-045) and keyset-cursor sanity (API-035): a cursor that
// satisfies the opaque grammar but does not decode to a valid ULID position is
// not a valid cursor.
func Valid(s string) bool {
	if len(s) != 26 {
		return false
	}
	if s[0] < '0' || s[0] > '7' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(crockfordAlphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

// encode Crockford-base32-encodes data's 128 bits into the canonical
// 26-character ULID text form. 128 isn't a multiple of 5 (26*5 = 130), so
// the encoding is defined as if 2 zero bits preceded the 128 real bits —
// achieved here by seeding bitCount at 2 with bitBuf at 0, so the very
// first output character only carries 3 real bits and every other
// character carries a full 5.
func encode(data [16]byte) string {
	var out [26]byte

	var bitBuf uint32
	bitCount := 2
	outIdx := 0
	for _, b := range data {
		bitBuf = (bitBuf << 8) | uint32(b)
		bitCount += 8
		for bitCount >= 5 {
			bitCount -= 5
			out[outIdx] = crockfordAlphabet[(bitBuf>>uint(bitCount))&0x1F]
			outIdx++
		}
	}

	return string(out[:])
}
