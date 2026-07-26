package ulid

import (
	"strings"
	"sync"
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

// TestMonotonicStrictlyAscendingWithinMillisecond confirms the generator
// Monotonic() returns is STRICTLY ascending even for a burst of ids minted
// inside one wall-clock millisecond — the guarantee plain New() deliberately
// does NOT make (its 80-bit tail is independent crypto/rand per call, so two
// same-millisecond ids have no defined relative order). The events/1 ingest
// assigns each event its recording-order id from this generator (EVT-011); if
// two automation.run events minted in the same millisecond could invert, the
// events/1 log would store and deliver them out of recording order, and a
// lagging SSE subscriber could permanently, silently drop one (EVT-011,
// REL-094/097, EVT-135/143). A tight loop is guaranteed to land many calls in
// the same millisecond on any real machine — the sameMS guard fails the test if
// it somehow did not, so the within-ms path is never left un-exercised.
func TestMonotonicStrictlyAscendingWithinMillisecond(t *testing.T) {
	next := Monotonic()
	prev := next()
	sameMS := 0
	for i := 0; i < 10000; i++ {
		cur := next()
		if cur <= prev {
			t.Fatalf("call %d: Monotonic id %q not strictly greater than previous %q (within-millisecond ordering violated)", i, cur, prev)
		}
		if cur[:10] == prev[:10] {
			sameMS++
		}
		prev = cur
	}
	if sameMS == 0 {
		t.Fatalf("test ineffective: no two successive Monotonic ids shared a millisecond prefix, so the within-ms increment path was never exercised")
	}
}

// TestMonotonicEmitsValidCanonicalULIDs confirms every id Monotonic() mints —
// including the same-millisecond increments — is a syntactically valid canonical
// ULID (Valid), so the events/1 envelope id it becomes satisfies EVT-011's
// "syntactically valid ULID" shape and events.Validate accepts it.
func TestMonotonicEmitsValidCanonicalULIDs(t *testing.T) {
	next := Monotonic()
	for i := 0; i < 2000; i++ {
		got := next()
		if !Valid(got) {
			t.Fatalf("call %d: Monotonic() = %q, not a valid canonical ULID", i, got)
		}
	}
}

// TestMonotonicIndependentFactoriesDoNotShareState confirms two generators from
// separate Monotonic() calls keep independent state — one factory's increments
// never perturb another's — so the production single-factory guarantee is not an
// accidental global.
func TestMonotonicIndependentFactoriesDoNotShareState(t *testing.T) {
	a := Monotonic()
	b := Monotonic()
	// Interleave the two factories; each must be internally strictly ascending.
	var lastA, lastB string
	for i := 0; i < 1000; i++ {
		ca := a()
		if lastA != "" && ca <= lastA {
			t.Fatalf("factory A call %d: %q not strictly greater than previous %q", i, ca, lastA)
		}
		lastA = ca
		cb := b()
		if lastB != "" && cb <= lastB {
			t.Fatalf("factory B call %d: %q not strictly greater than previous %q", i, cb, lastB)
		}
		lastB = cb
	}
}

// TestMonotonicConcurrentSafeUniqueAndOrdered confirms one Monotonic() factory
// shared across goroutines is race-free (run under -race), mints globally unique
// ids, and — because every call returns strictly above every prior call — leaves
// each goroutine's own observed subsequence strictly ascending. This backs the
// "safe for concurrent use" contract the factory documents.
func TestMonotonicConcurrentSafeUniqueAndOrdered(t *testing.T) {
	const goroutines, perG = 8, 500
	next := Monotonic()

	var wg sync.WaitGroup
	seqs := make([][]string, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			out := make([]string, perG)
			for i := 0; i < perG; i++ {
				out[i] = next()
			}
			seqs[g] = out
		}(g)
	}
	wg.Wait()

	seen := make(map[string]bool, goroutines*perG)
	for g, out := range seqs {
		for i, id := range out {
			if i > 0 && id <= out[i-1] {
				t.Fatalf("goroutine %d call %d: %q not strictly greater than previous %q", g, i, id, out[i-1])
			}
			if seen[id] {
				t.Fatalf("goroutine %d call %d: duplicate id %q across concurrent callers", g, i, id)
			}
			seen[id] = true
		}
	}
}

// TestValidAcceptsCanonical confirms Valid accepts a freshly minted ULID and
// the contract's own wire example — both canonical 26-character Crockford
// forms with a leading 0..7 character.
func TestValidAcceptsCanonical(t *testing.T) {
	for _, s := range []string{
		New(),
		"01J8Z3K4N5P6Q7R8S9T0V1W2ZF", // player-1 PLY-097 wire example
		"01J8Z3K4N5P6Q7R8S9T0V1W2Z1", // api-1 API-032 corpus id
		"00000000000000000000000000",
		"7ZZZZZZZZZZZZZZZZZZZZZZZZZ", // the spec's largest valid ULID
	} {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false, want true (a canonical ULID)", s)
		}
	}
}

// TestValidRejectsMalformed confirms Valid rejects everything that is not a
// canonical ULID: wrong length, non-Crockford characters (including the
// excluded I/L/O/U and lowercase), and a leading character above 7 (which
// would overflow the 48-bit timestamp past the 128-bit value width).
func TestValidRejectsMalformed(t *testing.T) {
	for _, s := range []string{
		"",                            // empty
		"01J8Z3K4N5P6Q7R8S9T0V1W2Z",   // 25 chars, too short
		"01J8Z3K4N5P6Q7R8S9T0V1W2Z11", // 27 chars, too long
		"01J8Z3K4N5P6Q7R8S9T0V1W2ZI",  // I is excluded from Crockford
		"01J8Z3K4N5P6Q7R8S9T0V1W2ZL",  // L is excluded from Crockford
		"01j8z3k4n5p6q7r8s9t0v1w2zf",  // lowercase is non-canonical
		"8ZZZZZZZZZZZZZZZZZZZZZZZZZ",  // leading 8 overflows the timestamp
		"ZZZZZZZZZZZZZZZZZZZZZZZZZZ",  // leading Z overflows the timestamp
		"01J8Z3K4N5P6Q7R8S9T0V1W2Z-",  // '-' is not a Crockford symbol
		"not a cursor!!",              // clearly not a ULID
	} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true, want false (not a canonical ULID)", s)
		}
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
