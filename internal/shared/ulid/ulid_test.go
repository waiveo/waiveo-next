package ulid

import (
	"strings"
	"testing"
	"time"
)

// TestNewLength confirms New() always returns the canonical ULID length:
// 26 characters, per the contract's own wire example
// (01J8Z3K4N5P6Q7R8S9T0V1W2ZF, contracts/player-1.md PLY-097).
func TestNewLength(t *testing.T) {
	got := New()
	if len(got) != 26 {
		t.Fatalf("len(New()) = %d, want 26 (got %q)", len(got), got)
	}
}

// TestNewCharsetIsCrockford confirms every character New() emits is one of
// the 32 Crockford base32 symbols (uppercase; I, L, O, U excluded).
func TestNewCharsetIsCrockford(t *testing.T) {
	got := New()
	for i, c := range got {
		if !strings.ContainsRune(crockfordAlphabet, c) {
			t.Fatalf("New() char %d = %q, not in Crockford alphabet %q (got %q)", i, c, crockfordAlphabet, got)
		}
	}
}

// TestNewIsUnique confirms two successive calls to New() never collide —
// PLY-097's "unique per issuance" requirement.
func TestNewIsUnique(t *testing.T) {
	a := New()
	b := New()
	if a == b {
		t.Fatalf("two successive New() calls returned the same id %q, want distinct", a)
	}
}

// TestNewTimestampComponentMonotonic confirms the leading 10-character
// timestamp component of New()'s output is monotonic non-decreasing across
// a sequence of calls made in wall-clock order. Crockford's alphabet lists
// its 32 symbols in ascending order, so — for same-length prefixes — a Go
// string comparison of the prefix agrees with a numeric comparison of the
// 48-bit timestamp it encodes; no separate decoder is needed for this
// assertion.
func TestNewTimestampComponentMonotonic(t *testing.T) {
	const timestampChars = 10

	prev := New()[:timestampChars]
	for i := 0; i < 200; i++ {
		cur := New()[:timestampChars]
		if cur < prev {
			t.Fatalf("call %d: timestamp component %q < previous %q, want non-decreasing", i, cur, prev)
		}
		prev = cur
	}
}

// TestNewTimestampWithinSaneWindow decodes New()'s 48-bit timestamp
// component (via decodeMillisForTest, this file's standalone reverse of
// encode) and confirms it falls within a generous window around
// time.Now() — catching a gross encoding error (e.g. wrong bit width, byte
// order, or unit) that the other tests' shape/ordering checks wouldn't
// necessarily surface.
func TestNewTimestampWithinSaneWindow(t *testing.T) {
	before := time.Now().UnixMilli()
	got := New()
	after := time.Now().UnixMilli()

	ms := decodeMillisForTest(t, got)

	const slackMS = 1000 // generous, to absorb any clock-resolution wobble
	if ms < before-slackMS || ms > after+slackMS {
		t.Fatalf("decoded timestamp %d ms outside [%d, %d] window (id %q)", ms, before-slackMS, after+slackMS, got)
	}
}

// decodeMillisForTest is a standalone (non-exported-from-package) reverse
// of encode, used only to verify New()'s timestamp component actually
// decodes to a sane value — it is deliberately independent of encode's own
// bit-shift arithmetic so it can catch a bug in encode rather than merely
// mirroring it.
func decodeMillisForTest(t *testing.T, u string) int64 {
	t.Helper()
	if len(u) != 26 {
		t.Fatalf("decodeMillisForTest: len(%q) = %d, want 26", u, len(u))
	}

	idx := make(map[byte]uint64, len(crockfordAlphabet))
	for i := 0; i < len(crockfordAlphabet); i++ {
		idx[crockfordAlphabet[i]] = uint64(i)
	}

	var bitBuf uint64
	bitCount := 0
	var out [16]byte
	outIdx := 0
	for i := 0; i < len(u); i++ {
		v, ok := idx[u[i]]
		if !ok {
			t.Fatalf("decodeMillisForTest: char %q at %d not in Crockford alphabet", u[i], i)
		}

		bits := 5
		if i == 0 {
			// The first character carries only 3 real bits (encode
			// treats the 128-bit payload as preceded by 2 zero pad
			// bits); mask off those 2 leading pad bits here.
			bits = 3
			v &= 0x07
		}

		bitBuf = (bitBuf << uint(bits)) | v
		bitCount += bits
		for bitCount >= 8 {
			bitCount -= 8
			out[outIdx] = byte(bitBuf >> uint(bitCount))
			outIdx++
		}
	}
	if outIdx != 16 {
		t.Fatalf("decodeMillisForTest: decoded %d bytes, want 16", outIdx)
	}

	ms := uint64(out[0])<<40 | uint64(out[1])<<32 | uint64(out[2])<<24 |
		uint64(out[3])<<16 | uint64(out[4])<<8 | uint64(out[5])
	return int64(ms)
}
