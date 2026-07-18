package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// numberCorpusCase mirrors the subset of
// conformance/corpora/rules-1/RUL-270-number-parsing-strict.json this
// package's ParseNumber oracle reads: the one strict number-parsing rule
// (RUL-270) accepts a JSON number or a plain decimal string as-is, and
// rejects every other shape as not-a-number (RUL-271 fail-closed).
type numberCorpusCase struct {
	Input struct {
		Values []struct {
			ID    string `json:"id"`
			Value any    `json:"value"`
		} `json:"values"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			ID               string   `json:"id"`
			IsNumber         bool     `json:"is_number"`
			Parsed           *float64 `json:"parsed"`
			SatisfiesAbove10 *bool    `json:"satisfies_above_10"`
		} `json:"results"`
	} `json:"expected"`
}

func loadNumberCorpusCase(t *testing.T) numberCorpusCase {
	t.Helper()
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "rules-1", "RUL-270-number-parsing-strict.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var c numberCorpusCase
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return c
}

// TestParseNumberCorpus replays every value in
// RUL-270-number-parsing-strict.json's input.values against ParseNumber and
// diffs against expected.results — the named oracle for this task.
func TestParseNumberCorpus(t *testing.T) {
	c := loadNumberCorpusCase(t)
	byID := map[string]struct {
		value any
	}{}
	for _, v := range c.Input.Values {
		byID[v.ID] = struct{ value any }{v.Value}
	}

	for _, want := range c.Expected.Results {
		want := want
		t.Run(want.ID, func(t *testing.T) {
			v, ok := byID[want.ID]
			if !ok {
				t.Fatalf("corpus case has no input.values entry with id %q", want.ID)
			}
			got, ok := ParseNumber(v.value)
			if ok != want.IsNumber {
				t.Fatalf("ParseNumber(%#v) ok = %v, want %v", v.value, ok, want.IsNumber)
			}
			if ok && want.Parsed != nil && got != *want.Parsed {
				t.Errorf("ParseNumber(%#v) = %v, want %v", v.value, got, *want.Parsed)
			}
			if want.SatisfiesAbove10 != nil {
				satisfies := ok && got > 10
				if satisfies != *want.SatisfiesAbove10 {
					t.Errorf("satisfies_above_10(%#v) = %v, want %v", v.value, satisfies, *want.SatisfiesAbove10)
				}
			}
		})
	}
}

// TestParseNumberOtherTypes covers Go-typed inputs outside the JSON-decoded
// shape the corpus fixture produces but that a Go caller could still pass
// through the any parameter: other numeric-literal Go types are used as-is
// (RUL-270 case 1, "a JSON number is used as-is" generalizes to any numeric
// Go type a JSON decode could plausibly produce); nil and composite types
// are not-a-number (RUL-270 case 3).
func TestParseNumberOtherTypes(t *testing.T) {
	tests := []struct {
		name       string
		value      any
		wantOK     bool
		wantParsed float64
	}{
		{"int", int(7), true, 7},
		{"int64", int64(7), true, 7},
		{"float32", float32(1.5), true, 1.5},
		{"nil", nil, false, 0},
		{"array", []any{1, 2}, false, 0},
		{"object", map[string]any{"a": 1}, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseNumber(tt.value)
			if ok != tt.wantOK {
				t.Fatalf("ParseNumber(%#v) ok = %v, want %v", tt.value, ok, tt.wantOK)
			}
			if ok && got != tt.wantParsed {
				t.Errorf("ParseNumber(%#v) = %v, want %v", tt.value, got, tt.wantParsed)
			}
		})
	}
}
