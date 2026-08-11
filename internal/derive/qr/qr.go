// Package qr encodes a byte string into a QR symbol matrix (ISO/IEC 18004).
//
// It exists because a QR code is one of the five things the legacy Studio could
// put on a slide that native SceneGraph layers cannot draw — there is no Roku
// node that turns a URL into a scannable symbol — and it is the one of the five
// whose correctness is not a matter of taste. A gradient that is off by a shade
// is a cosmetic defect; a QR symbol with one wrong error-correction codeword is
// a sign on a wall that nobody's phone can read, and nothing about looking at it
// says so.
//
// The encoder is deliberately PURE GO and deliberately DETERMINISTIC: the same
// (data, level) always yields the identical matrix, because the derive pipeline
// is content-addressed and a symbol that re-encoded differently on each run
// would defeat the deduplication that makes re-deriving a cast cheap. Nothing
// here reads a clock, a random source, or the environment.
//
// Scope: byte mode only, all 40 versions, all four error-correction levels. Byte
// mode alone is the right scope — a QR on a sign carries a URL or a pairing
// code, both of which are byte-mode payloads, and the numeric/alphanumeric modes
// exist to save space in a symbol that is already far below capacity here.
package qr

import (
	"errors"
	"fmt"
)

// Level is a QR error-correction level. The zero value is LevelL, but callers
// should name the level they want: the four levels trade payload capacity for
// how much of a printed or screen-rendered symbol can be obscured and still
// scan. A sign on a wall is read at an angle in bad light, so the derive
// pipeline defaults to LevelM rather than to the zero value.
type Level int

// The four error-correction levels, in the ISO ordering the tables in tables.go
// are indexed by. Do NOT reorder: the index IS the second subscript of ecBlocks.
const (
	LevelL Level = iota // ~7% recovery
	LevelM              // ~15% recovery
	LevelQ              // ~25% recovery
	LevelH              // ~30% recovery
)

// formatBits is the 2-bit format-information encoding of each level. It is NOT
// the same as the Level index — the ISO format-info ordering is L=01, M=00,
// Q=11, H=10 — which is exactly the kind of two-orderings-for-one-concept trap
// that produces a symbol that looks right and scans as garbage, so the mapping
// is stated once here and never inlined.
var formatBits = [4]int{0b01, 0b00, 0b11, 0b10}

// ParseLevel maps the single-letter level name an authored derive spec carries
// ("L"/"M"/"Q"/"H") onto a Level. An unknown name is an error rather than a
// silent fallback: a spec that asked for H and quietly got L would produce a
// symbol that is valid, scannable in the lab, and less robust than the author
// asked for — a downgrade nothing would ever report.
func ParseLevel(s string) (Level, error) {
	switch s {
	case "L":
		return LevelL, nil
	case "M":
		return LevelM, nil
	case "Q":
		return LevelQ, nil
	case "H":
		return LevelH, nil
	}
	return 0, fmt.Errorf("qr: unknown error-correction level %q (want L, M, Q or H)", s)
}

// String renders the level back as its single-letter name, so a spec digest and
// an error message can name the level an operator typed.
func (l Level) String() string {
	switch l {
	case LevelL:
		return "L"
	case LevelM:
		return "M"
	case LevelQ:
		return "Q"
	case LevelH:
		return "H"
	}
	return "?"
}

// ErrTooLong reports a payload that does not fit any version at the requested
// level. It is a distinct error because it is the one failure a caller can
// actually act on — shorten the URL, or drop to a weaker level.
var ErrTooLong = errors.New("qr: payload does not fit any QR version at this error-correction level")

// Matrix is a square QR symbol: Size×Size modules, Dark[y*Size+x] true where the
// module is dark. It carries no quiet zone — the renderer adds one, because how
// wide a quiet zone is depends on the pixel geometry it is being drawn into, not
// on the encoding.
type Matrix struct {
	Size    int
	Version int
	Level   Level
	Dark    []bool
}

// At reports whether the module at (x, y) is dark. Out-of-range coordinates read
// as light so a renderer can walk a padded box without bounds-checking every
// access.
func (m *Matrix) At(x, y int) bool {
	if x < 0 || y < 0 || x >= m.Size || y >= m.Size {
		return false
	}
	return m.Dark[y*m.Size+x]
}

// Encode encodes data as a byte-mode QR symbol at the given error-correction
// level, choosing the smallest version that fits.
//
// The result is fully determined by (data, level): version selection, padding,
// Reed-Solomon parity, and mask selection are all deterministic functions of the
// input, with mask chosen by the ISO penalty score and ties broken by the lowest
// mask number.
func Encode(data []byte, level Level) (*Matrix, error) {
	if level < LevelL || level > LevelH {
		return nil, fmt.Errorf("qr: error-correction level %d out of range", int(level))
	}
	version, err := chooseVersion(len(data), level)
	if err != nil {
		return nil, err
	}

	codewords := buildCodewords(data, version, level)
	final := interleave(codewords, version, level)

	m := newMatrix(version, level)
	reserved := m.placeFunctionPatterns()
	m.placeData(final, reserved)

	bestScore := -1
	var bestDark []bool
	for mask := 0; mask < 8; mask++ {
		trial := make([]bool, len(m.Dark))
		copy(trial, m.Dark)
		applyMask(trial, reserved, m.Size, mask)
		writeFormatInfo(trial, m.Size, level, mask)
		score := penalty(trial, m.Size)
		// Strictly-less keeps the LOWEST mask number on a tie, which is what
		// makes the chosen mask a deterministic function of the payload.
		if bestScore < 0 || score < bestScore {
			bestScore, bestDark = score, trial
		}
	}
	m.Dark = bestDark
	return m, nil
}

// chooseVersion returns the smallest version whose data capacity at this level
// holds a byte-mode segment of n bytes.
func chooseVersion(n int, level Level) (int, error) {
	for v := 1; v <= 40; v++ {
		need := 4 + charCountBits(v) + 8*n
		if need <= 8*dataCodewords(v, level) {
			return v, nil
		}
	}
	return 0, fmt.Errorf("%w: %d bytes at level %s", ErrTooLong, n, level)
}

// charCountBits is the byte-mode character-count field width for a version: 8
// bits for versions 1-9, 16 bits from version 10 up.
func charCountBits(version int) int {
	if version <= 9 {
		return 8
	}
	return 16
}

// dataCodewords is the number of DATA codewords (total minus error correction)
// a version holds at a level.
func dataCodewords(version int, level Level) int {
	return versionTotalCodewords[version-1] - ecBlocks[version-1][level][1]
}

// buildCodewords produces the data codeword stream: mode indicator, character
// count, payload, terminator, byte alignment, and the alternating 0xEC/0x11 pad.
func buildCodewords(data []byte, version int, level Level) []byte {
	capacity := dataCodewords(version, level)
	var bits bitWriter
	bits.write(0b0100, 4) // byte mode
	bits.write(len(data), charCountBits(version))
	for _, b := range data {
		bits.write(int(b), 8)
	}
	// Terminator: up to four zero bits, truncated by the remaining capacity.
	remaining := 8*capacity - bits.length()
	if remaining > 4 {
		remaining = 4
	}
	if remaining > 0 {
		bits.write(0, remaining)
	}
	bits.padToByte()

	out := bits.bytes()
	for i := 0; len(out) < capacity; i++ {
		if i%2 == 0 {
			out = append(out, 0xEC)
		} else {
			out = append(out, 0x11)
		}
	}
	return out
}

// interleave splits the data codewords into the version/level's block
// structure, computes each block's Reed-Solomon parity, and interleaves data
// then parity into the final codeword sequence.
//
// The block split is the ISO rule rather than a table: the blocks divide the
// data codewords as evenly as possible, and the remainder blocks (the LONG
// ones) go last.
func interleave(data []byte, version int, level Level) []byte {
	numBlocks := ecBlocks[version-1][level][0]
	totalEC := ecBlocks[version-1][level][1]
	ecPerBlock := totalEC / numBlocks

	totalData := len(data)
	shortLen := totalData / numBlocks
	longCount := totalData % numBlocks

	blocks := make([][]byte, numBlocks)
	ecs := make([][]byte, numBlocks)
	off := 0
	for i := 0; i < numBlocks; i++ {
		n := shortLen
		if i >= numBlocks-longCount {
			n = shortLen + 1
		}
		blocks[i] = data[off : off+n]
		off += n
		ecs[i] = reedSolomon(blocks[i], ecPerBlock)
	}

	out := make([]byte, 0, totalData+totalEC)
	for i := 0; i < shortLen+1; i++ {
		for _, b := range blocks {
			if i < len(b) {
				out = append(out, b[i])
			}
		}
	}
	for i := 0; i < ecPerBlock; i++ {
		for _, e := range ecs {
			out = append(out, e[i])
		}
	}
	return out
}

// bitWriter accumulates a big-endian bit stream.
type bitWriter struct {
	buf  []byte
	bits int
}

func (w *bitWriter) write(value, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.bits%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		if value&(1<<uint(i)) != 0 {
			w.buf[w.bits/8] |= 1 << uint(7-w.bits%8)
		}
		w.bits++
	}
}

func (w *bitWriter) length() int { return w.bits }

func (w *bitWriter) padToByte() {
	for w.bits%8 != 0 {
		w.write(0, 1)
	}
}

func (w *bitWriter) bytes() []byte { return w.buf }
