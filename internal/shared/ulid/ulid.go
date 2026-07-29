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
	"sync"
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

// Monotonic returns a stateful ULID generator whose successive calls are
// STRICTLY ascending — including for ids minted within the same millisecond,
// which plain New does NOT order (New's 80-bit tail is independent crypto/rand
// per call, so two same-millisecond ids have no defined relative order). It
// implements the ULID spec's monotonic factory: a fresh 80-bit random tail per
// new millisecond, and within one millisecond the tail is incremented by one so
// each id strictly exceeds the last. A backwards clock step reuses the last
// (higher) timestamp rather than regressing. The returned closure is safe for
// concurrent use.
//
// This exists for the events/1 envelope-id path (EVT-011): the ingest assigns
// each event its recording-order id from this generator, and the event log
// stores and a resuming/live SSE subscriber streams strictly in id order
// (After/Has are id comparisons). A within-millisecond inversion — two rule
// firings inside one telemetry flush tick, easily the same wall-clock ms — would
// otherwise deliver them out of recording order and, in a narrow interleaving,
// silently and permanently drop one from a lagging subscriber's stream
// (REL-094/097, EVT-135/143). A monotonic id closes that: same-ms ids sort in
// mint (recording) order.
func Monotonic() func() string { return monotonic("") }

// MonotonicFrom is Monotonic seeded with a FLOOR: every id it mints strictly
// exceeds floor, in addition to strictly exceeding every id it minted before.
// An empty or non-canonical floor is ignored, making it exactly Monotonic.
//
// It exists because a generator's monotonicity is per-process, and the events/1
// log the ids are minted into is not. EVT-011 requires ids to be "assigned in
// the platform's own recording order, so that comparing two IDs
// lexicographically also orders them by recording time" — a property the log
// itself depends on for delivery ordering and gap accounting, and one a
// restarted process breaks the moment its clock reads at or below the last id
// already stored. That is not hypothetical on a box: security-model/1
// SEC-066-068 exists precisely because an appliance can boot with a clock
// behind where it left off, and a fresh Monotonic() would then mint ids sorting
// UNDER events already in the log — appended behind a connected subscriber's
// cursor, and so never delivered to it at all.
//
// Seeding from the stored head closes that: the floor's own timestamp and
// 80-bit tail become the generator's starting point, so a low clock reuses the
// stored (higher) timestamp and increments the tail rather than regressing.
func MonotonicFrom(floor string) func() string { return monotonic(floor) }

// monotonic is the shared factory behind Monotonic and MonotonicFrom.
func monotonic(floor string) func() string {
	var (
		mu       sync.Mutex
		lastMS   uint64
		lastRand [10]byte // the 80-bit random tail of the last id minted
		seeded   bool
	)
	if data, ok := decode(floor); ok {
		// Adopt the floor's exact 128 bits as "the last id minted". Every
		// branch below then produces something strictly above it: a higher
		// wall-clock millisecond outranks it on the timestamp, and an equal or
		// lower one reuses this timestamp and increments this tail.
		lastMS = uint64(data[0])<<40 | uint64(data[1])<<32 | uint64(data[2])<<24 |
			uint64(data[3])<<16 | uint64(data[4])<<8 | uint64(data[5])
		copy(lastRand[:], data[6:])
		seeded = true
	}
	return func() string {
		mu.Lock()
		defer mu.Unlock()

		ms := uint64(time.Now().UnixMilli())
		if !seeded || ms > lastMS {
			// A new (or the first) millisecond: fresh randomness.
			lastMS = ms
			if _, err := rand.Read(lastRand[:]); err != nil {
				// Same fatal-environment convention as New: crypto/rand failing
				// is an unrecoverable entropy-source problem, and this is a
				// value-returning helper with nowhere to propagate an error.
				panic("ulid: Monotonic: " + err.Error())
			}
			seeded = true
		} else {
			// Same millisecond (or the clock stepped backwards): keep the prior
			// timestamp and increment the 80-bit tail so the id strictly exceeds
			// the previous one, preserving recording order (EVT-011). Reusing the
			// higher lastMS on a backwards step means an id never regresses.
			if incr80(&lastRand) {
				// The 80-bit space wrapped within one millisecond (2^80 calls —
				// not physically reachable). Carry into the timestamp so the id
				// stays strictly ascending rather than error out of a value
				// helper; the tail is now all-zero, still below the next call's.
				lastMS++
			}
		}

		var data [16]byte
		data[0] = byte(lastMS >> 40)
		data[1] = byte(lastMS >> 32)
		data[2] = byte(lastMS >> 24)
		data[3] = byte(lastMS >> 16)
		data[4] = byte(lastMS >> 8)
		data[5] = byte(lastMS)
		copy(data[6:], lastRand[:])
		return encode(data)
	}
}

// incr80 increments the 80-bit big-endian value in b by one, in place,
// returning true only on overflow (b was all 0xFF and wrapped to all 0x00).
func incr80(b *[10]byte) bool {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return false
		}
	}
	return true
}

// FromBytes encodes an arbitrary 128-bit value as a canonical ULID string.
//
// Every possible input satisfies Valid: encode gives the first output symbol
// only the top 3 bits of data[0] (26*5 = 130 bits carry 128 behind two zero pad
// bits), so the leading character is always in 0..7, and every symbol it emits
// comes from crockfordAlphabet by construction. That is what lets a DERIVED id
// — one whose 128 bits are a hash of an identity rather than a timestamp plus
// randomness — still satisfy a contract that requires a syntactically valid
// canonical ULID (data-model/1 DAT-005a).
//
// What such an id does NOT carry is New's creation-time ordering: its leading
// 48 bits are hash output, not a millisecond clock, so comparing two derived
// ids orders them stably but arbitrarily. Callers that need creation order MUST
// use New or Monotonic; callers that need a STABLE TOTAL ORDER (a keyset
// cursor, api/1 API-034) get exactly that from either.
func FromBytes(data [16]byte) string { return encode(data) }

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

// decode is encode's inverse: it reads a canonical 26-character ULID back into
// the 128 bits it encodes, reporting ok=false for anything Valid rejects. Only
// the leading symbol is special — 26*5 = 130 bits carry a 128-bit value behind
// two zero pad bits, so the first character contributes just 3 significant bits
// and every other one a full 5 (Valid already bounds that first symbol to 0..7,
// which is what guarantees the two pad bits really are zero).
func decode(s string) ([16]byte, bool) {
	var out [16]byte
	if !Valid(s) {
		return out, false
	}
	bitBuf := uint32(strings.IndexByte(crockfordAlphabet, s[0])) & 0x07
	bitCount := 3
	outIdx := 0
	for i := 1; i < len(s); i++ {
		bitBuf = (bitBuf << 5) | uint32(strings.IndexByte(crockfordAlphabet, s[i]))
		bitCount += 5
		for bitCount >= 8 {
			bitCount -= 8
			out[outIdx] = byte(bitBuf >> uint(bitCount))
			outIdx++
		}
	}
	return out, true
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
