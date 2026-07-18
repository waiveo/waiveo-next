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
