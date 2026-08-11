package qr

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// goldenCase is one row of testdata/golden.json: a payload, an
// error-correction level, and the EXACT symbol an independent implementation
// produces for it.
type goldenCase struct {
	Data    string   `json:"data"`
	EC      string   `json:"ec"`
	Version int      `json:"version"`
	Size    int      `json:"size"`
	Rows    []string `json:"rows"`
}

// TestEncodeMatchesAnIndependentImplementation is the only test that can
// actually prove this encoder right.
//
// Every other property one can assert about a QR symbol — that it is square,
// that the finder patterns are where they belong, that the size matches the
// version — is satisfied by a symbol whose error-correction codewords are
// garbage, and a symbol with garbage parity looks completely normal and does not
// scan. Structural assertions written from the same understanding that wrote the
// encoder cannot catch a misread of the spec, because they share the misreading.
//
// So the expectation comes from OUTSIDE: testdata/golden.json holds the exact
// module matrices the `qrcode` npm package (the implementation the legacy
// slidecast extension generated its QR images with) produces for these payloads,
// dumped in byte mode so the mode selection cannot differ. A byte-for-byte match
// means the version selection, the padding, the Reed-Solomon parity, the block
// interleave, the function patterns, the mask CHOICE and the format/version
// information all agree with a implementation whose output has been scanned in
// production for years.
//
// The cases are chosen to reach the parts that differ structurally: version 1
// (single block, no alignment pattern, no version information), version 3 and 5
// (alignment pattern, still one or two blocks), and version 16 (multi-block
// interleave, version-information field, many alignment patterns).
func TestEncodeMatchesAnIndependentImplementation(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decode golden vectors: %v", err)
	}
	if len(cases) < 5 {
		t.Fatalf("golden.json holds %d cases, want at least 5 — the vector file is the whole proof", len(cases))
	}

	for _, c := range cases {
		level, err := ParseLevel(c.EC)
		if err != nil {
			t.Fatalf("golden case names level %q: %v", c.EC, err)
		}
		m, err := Encode([]byte(c.Data), level)
		if err != nil {
			t.Fatalf("Encode(%q, %s): %v", truncate(c.Data), c.EC, err)
		}
		if m.Version != c.Version {
			t.Errorf("Encode(%q, %s): version %d, want %d", truncate(c.Data), c.EC, m.Version, c.Version)
			continue
		}
		if m.Size != c.Size {
			t.Errorf("Encode(%q, %s): size %d, want %d", truncate(c.Data), c.EC, m.Size, c.Size)
			continue
		}
		for y, want := range c.Rows {
			var got strings.Builder
			for x := 0; x < m.Size; x++ {
				if m.At(x, y) {
					got.WriteByte('#')
				} else {
					got.WriteByte('.')
				}
			}
			if got.String() != want {
				t.Errorf("Encode(%q, %s): row %d differs\n got %s\nwant %s",
					truncate(c.Data), c.EC, y, got.String(), want)
				break
			}
		}
	}
}

// TestEncodeIsDeterministic pins the property the content-addressed derive
// pipeline depends on: the same payload encodes to the same bytes every time.
// If it did not, every re-derive would mint a new asset digest, every screen
// would refetch content that had not changed, and the content origin would grow
// without bound.
func TestEncodeIsDeterministic(t *testing.T) {
	first, err := Encode([]byte("https://waiveo.local/pair/ABCD-1234"), LevelM)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := Encode([]byte("https://waiveo.local/pair/ABCD-1234"), LevelM)
		if err != nil {
			t.Fatalf("Encode (repeat %d): %v", i, err)
		}
		for j := range first.Dark {
			if first.Dark[j] != again.Dark[j] {
				t.Fatalf("repeat %d differs at module %d — the encoder is not deterministic", i, j)
			}
		}
	}
}

// TestEncodeRejectsAnOversizePayload proves the capacity check reports rather
// than truncates. Silently dropping the tail of a URL yields a symbol that
// scans perfectly and goes to the wrong place.
func TestEncodeRejectsAnOversizePayload(t *testing.T) {
	// Version 40-H holds well under 1,300 bytes; 4,000 fits nothing.
	if _, err := Encode(make([]byte, 4000), LevelH); err == nil {
		t.Fatal("Encode accepted a payload no version can hold — it must report, never truncate")
	}
}

// TestParseLevelIsAClosedVocabulary: an unrecognised level name is refused
// rather than defaulted. A spec that asked for H and silently got L renders a
// symbol that is valid and less robust than the author asked for, and nothing
// downstream can tell.
func TestParseLevelIsAClosedVocabulary(t *testing.T) {
	for _, name := range []string{"L", "M", "Q", "H"} {
		lv, err := ParseLevel(name)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", name, err)
		}
		if lv.String() != name {
			t.Errorf("ParseLevel(%q).String() = %q — the two directions disagree", name, lv.String())
		}
	}
	for _, name := range []string{"", "l", "X", "MEDIUM"} {
		if _, err := ParseLevel(name); err == nil {
			t.Errorf("ParseLevel(%q) accepted an unknown level", name)
		}
	}
}

// TestAtIsBoundsSafe: a renderer walks a padded box around the symbol, so
// out-of-range reads must answer "light" rather than panic.
func TestAtIsBoundsSafe(t *testing.T) {
	m, err := Encode([]byte("x"), LevelL)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, p := range [][2]int{{-1, 0}, {0, -1}, {m.Size, 0}, {0, m.Size}} {
		if m.At(p[0], p[1]) {
			t.Errorf("At(%d,%d) reported dark outside the symbol", p[0], p[1])
		}
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
