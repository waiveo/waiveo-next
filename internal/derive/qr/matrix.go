package qr

import "math"

// matrix.go lays codewords out into the symbol: the function patterns that make
// a symbol findable, the zigzag data placement, the eight data masks and the ISO
// penalty score that picks between them, and the two protected fields (format
// information, and version information from version 7 up).

// newMatrix allocates an all-light symbol of the right size for a version.
func newMatrix(version int, level Level) *Matrix {
	size := version*4 + 17
	return &Matrix{
		Size:    size,
		Version: version,
		Level:   level,
		Dark:    make([]bool, size*size),
	}
}

func (m *Matrix) set(x, y int, dark bool) {
	m.Dark[y*m.Size+x] = dark
}

// placeFunctionPatterns draws every module whose value is fixed by the version
// rather than by the payload, and returns the reservation mask: reserved[i] true
// means module i is a function module, so data placement skips it and the data
// mask must not flip it.
//
// The format-information modules are reserved here but written later
// (writeFormatInfo), because their value depends on the mask, which is chosen
// only after the data is placed.
func (m *Matrix) placeFunctionPatterns() []bool {
	size := m.Size
	reserved := make([]bool, size*size)
	reserve := func(x, y int) {
		if x >= 0 && y >= 0 && x < size && y < size {
			reserved[y*size+x] = true
		}
	}

	// Three finder patterns with their one-module separators.
	for _, o := range [][2]int{{0, 0}, {size - 7, 0}, {0, size - 7}} {
		for dy := -1; dy <= 7; dy++ {
			for dx := -1; dx <= 7; dx++ {
				x, y := o[0]+dx, o[1]+dy
				if x < 0 || y < 0 || x >= size || y >= size {
					continue
				}
				inRing := dx >= 0 && dx <= 6 && dy >= 0 && dy <= 6 &&
					(dx == 0 || dx == 6 || dy == 0 || dy == 6)
				inCore := dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4
				m.set(x, y, inRing || inCore)
				reserve(x, y)
			}
		}
	}

	// Timing patterns: the alternating row and column at index 6.
	for i := 8; i < size-8; i++ {
		dark := i%2 == 0
		m.set(i, 6, dark)
		reserve(i, 6)
		m.set(6, i, dark)
		reserve(6, i)
	}

	// Alignment patterns at every coordinate pair except the three that would
	// collide with a finder pattern.
	coords := alignmentCoords[m.Version-1]
	for _, cy := range coords {
		for _, cx := range coords {
			if (cx == 6 && cy == 6) || (cx == 6 && cy == size-7) || (cx == size-7 && cy == 6) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					dark := dx == -2 || dx == 2 || dy == -2 || dy == 2 || (dx == 0 && dy == 0)
					m.set(cx+dx, cy+dy, dark)
					reserve(cx+dx, cy+dy)
				}
			}
		}
	}

	// The dark module, always at (8, 4*version+9).
	m.set(8, 4*m.Version+9, true)
	reserve(8, 4*m.Version+9)

	// Format-information reservations (value written after mask selection).
	for i := 0; i <= 8; i++ {
		reserve(i, 8)
		reserve(8, i)
	}
	for i := 0; i < 8; i++ {
		reserve(size-1-i, 8)
		reserve(8, size-1-i)
	}

	// Version information, versions 7 and up: two 6x3 blocks of an 18-bit
	// BCH(18,6) code, one beside each of the two far finder patterns.
	if m.Version >= 7 {
		bits := versionInfoBits(m.Version)
		for i := 0; i < 18; i++ {
			dark := bits&(1<<uint(i)) != 0
			x, y := i/3, size-11+i%3
			m.set(x, y, dark)
			reserve(x, y)
			m.set(y, x, dark)
			reserve(y, x)
		}
	}

	return reserved
}

// placeData walks the symbol in the ISO zigzag — two-module-wide columns from
// the right edge leftward, alternating upward and downward, skipping the timing
// column — writing one codeword bit per unreserved module.
//
// The version's REMAINDER BITS (the 0, 3, 4 or 7 spare modules some versions
// have past the last codeword) need no code of their own: they are specified as
// zero, an unwritten module is already light, and the mask is applied to every
// unreserved module whether data was placed there or not. An explicit
// remainder-bit table was written here first and then deleted, because mutating
// it changed nothing in a golden corpus covering all forty versions — it was
// inert by construction, which is worth stating so it does not get re-added.
func (m *Matrix) placeData(codewords []byte, reserved []bool) {
	size := m.Size
	total := 8 * len(codewords)
	bit := 0
	upward := true
	for right := size - 1; right >= 0; right -= 2 {
		if right == 6 {
			// Column 6 is the vertical timing pattern; the zigzag steps over it
			// entirely so the pair below it stays two data columns wide.
			right--
		}
		for i := 0; i < size; i++ {
			y := i
			if upward {
				y = size - 1 - i
			}
			for dx := 0; dx < 2; dx++ {
				x := right - dx
				if reserved[y*size+x] {
					continue
				}
				if bit >= total {
					continue
				}
				m.set(x, y, codewords[bit/8]&(1<<uint(7-bit%8)) != 0)
				bit++
			}
		}
		upward = !upward
	}
}

// maskCondition is the ISO data-mask predicate for mask number n at (x, y).
func maskCondition(n, x, y int) bool {
	switch n {
	case 0:
		return (y+x)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (y+x)%3 == 0
	case 4:
		return (y/2+x/3)%2 == 0
	case 5:
		return (y*x)%2+(y*x)%3 == 0
	case 6:
		return ((y*x)%2+(y*x)%3)%2 == 0
	default:
		return ((y+x)%2+(y*x)%3)%2 == 0
	}
}

// applyMask XORs the mask pattern into every NON-reserved module. Reserved
// modules are the function patterns and the format/version fields, which are
// never masked — masking them would destroy the very patterns a scanner uses to
// locate and read the symbol.
func applyMask(dark []bool, reserved []bool, size, mask int) {
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			i := y*size + x
			if reserved[i] {
				continue
			}
			if maskCondition(mask, x, y) {
				dark[i] = !dark[i]
			}
		}
	}
}

// formatInfoBits computes the 15-bit format information for a level and mask:
// a 5-bit value, a BCH(15,5) remainder, and the fixed 0x5412 XOR mask that keeps
// the all-zero format from being a valid codeword.
func formatInfoBits(level Level, mask int) int {
	data := formatBits[level]<<3 | mask
	rem := data << 10
	for i := 14; i >= 10; i-- {
		if rem&(1<<uint(i)) != 0 {
			rem ^= 0x537 << uint(i-10)
		}
	}
	return (data<<10 | rem) ^ 0x5412
}

// writeFormatInfo stamps the 15 format bits into their two copies. The copies
// are written from the SAME computed value in one function so the two can never
// disagree — a symbol whose two format copies differ is one a scanner may read
// with the wrong mask and decode as noise.
func writeFormatInfo(dark []bool, size int, level Level, mask int) {
	bits := formatInfoBits(level, mask)
	put := func(x, y int, on bool) { dark[y*size+x] = on }
	for i := 0; i < 15; i++ {
		on := bits&(1<<uint(i)) != 0
		// Copy 1: around the top-left finder, skipping the timing row/column.
		switch {
		case i < 6:
			put(8, i, on)
		case i == 6:
			put(8, 7, on)
		case i == 7:
			put(8, 8, on)
		case i == 8:
			put(7, 8, on)
		default:
			put(14-i, 8, on)
		}
		// Copy 2: split between the bottom-left and top-right finders.
		if i < 8 {
			put(size-1-i, 8, on)
		} else {
			put(8, size-15+i, on)
		}
	}
}

// versionInfoBits computes the 18-bit version information for versions 7+: the
// 6-bit version number plus a BCH(18,6) remainder.
func versionInfoBits(version int) int {
	rem := version << 12
	for i := 17; i >= 12; i-- {
		if rem&(1<<uint(i)) != 0 {
			rem ^= 0x1F25 << uint(i-12)
		}
	}
	return version<<12 | rem
}

// penalty is the ISO mask-evaluation score: lower is better. The four rules
// penalise the visual features that confuse scanners — long same-colour runs,
// solid 2x2 blocks, the finder-lookalike 1:1:3:1:1 sequence, and an unbalanced
// dark/light ratio.
func penalty(dark []bool, size int) int {
	at := func(x, y int) bool { return dark[y*size+x] }
	score := 0

	// Rule 1: runs of five or more identical modules in a row or column.
	for i := 0; i < size; i++ {
		runRow, runCol := 1, 1
		for j := 1; j < size; j++ {
			if at(j, i) == at(j-1, i) {
				runRow++
			} else {
				if runRow >= 5 {
					score += runRow - 2
				}
				runRow = 1
			}
			if at(i, j) == at(i, j-1) {
				runCol++
			} else {
				if runCol >= 5 {
					score += runCol - 2
				}
				runCol = 1
			}
		}
		if runRow >= 5 {
			score += runRow - 2
		}
		if runCol >= 5 {
			score += runCol - 2
		}
	}

	// Rule 2: every 2x2 block of one colour.
	for y := 0; y < size-1; y++ {
		for x := 0; x < size-1; x++ {
			v := at(x, y)
			if at(x+1, y) == v && at(x, y+1) == v && at(x+1, y+1) == v {
				score += 3
			}
		}
	}

	// Rule 3: the 1:1:3:1:1 finder-lookalike run with a four-module light area on
	// one side, in either orientation. Expressed as an 11-module sliding window
	// matched against the two literal bit patterns — 10111010000 and 00001011101 —
	// rather than as a "pattern plus a light run" search, because the window form
	// is what the field-proven reference this encoder is golden-tested against
	// uses, and the two formulations DISAGREE at the symbol edge: a "light run"
	// search that treats off-symbol as light finds extra occurrences in the first
	// and last four columns, shifts the score, and can pick a different mask. A
	// different mask is not a cosmetic difference — it is a different symbol.
	for i := 0; i < size; i++ {
		wRow, wCol := 0, 0
		for j := 0; j < size; j++ {
			bit := 0
			if at(j, i) {
				bit = 1
			}
			wRow = ((wRow << 1) & 0x7FF) | bit
			if j >= 10 && (wRow == 0x5D0 || wRow == 0x05D) {
				score += 40
			}
			bit = 0
			if at(i, j) {
				bit = 1
			}
			wCol = ((wCol << 1) & 0x7FF) | bit
			if j >= 10 && (wCol == 0x5D0 || wCol == 0x05D) {
				score += 40
			}
		}
	}

	// Rule 4: deviation of the dark-module proportion from 50%, in 5% steps.
	// The rounding is ceil-then-offset rather than truncate-then-divide for the
	// same reason rule 3 is a window match: it is the reference's arithmetic, and
	// the two round differently either side of every 5% boundary.
	darkCount := 0
	for _, d := range dark {
		if d {
			darkCount++
		}
	}
	steps := int(math.Ceil(float64(darkCount) * 100 / float64(size*size) / 5))
	if steps < 10 {
		steps = 10 - steps
	} else {
		steps -= 10
	}
	score += steps * 10

	return score
}
