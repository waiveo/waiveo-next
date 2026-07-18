// Package corpus loads the frozen conformance corpora
// (conformance/corpora/<contract>/*.json) as generic, abstract behavioral
// cases so the executable drivers (conformance/drivers/player1,
// conformance/drivers/relay1) can read each case's own declared `expected`
// block and diff the LIVE implementation's behavior against it — the §10
// differential oracle reads its expectations from the same frozen corpus the
// contract review signed off on, never from values hard-coded in the driver.
package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Case is one corpus case in its abstract form: identity/traceability
// metadata plus the opaque `input` and `expected` blocks a driver reads the
// specific fields it can observe out of. `input`/`expected` are intentionally
// map[string]any — each case is a behavioral spec, not a fixed schema, so a
// driver pulls exactly the fields it drives and leaves the rest.
type Case struct {
	CaseID      string         `json:"case_id"`
	Contract    string         `json:"contract"`
	ReqIDs      []string       `json:"req_ids"`
	Description string         `json:"description"`
	Input       map[string]any `json:"input"`
	Expected    map[string]any `json:"expected"`
}

// Expect returns the expected value at dotted key path (e.g.
// "state_ack.body.applied_generation"), and whether it was present — the
// driver's read side of the differential oracle.
func (c Case) Expect(path string) (any, bool) {
	return dig(c.Expected, path)
}

// ExpectBool returns a boolean expected field, defaulting to false when
// absent or not a bool.
func (c Case) ExpectBool(path string) bool {
	v, ok := c.Expect(path)
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// ExpectString returns a string expected field, defaulting to "" when absent
// or not a string.
func (c Case) ExpectString(path string) string {
	v, ok := c.Expect(path)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func dig(m map[string]any, path string) (any, bool) {
	cur := any(m)
	start := 0
	for i := 0; i <= len(path); i++ {
		if i < len(path) && path[i] != '.' {
			continue
		}
		key := path[start:i]
		start = i + 1
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[key]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			// A numeric path segment indexes into a JSON array (e.g.
			// "responses.1.code").
			idx, err := strconv.Atoi(key)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// Load reads and decodes a single corpus case file.
func Load(path string) (Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Case{}, fmt.Errorf("corpus: load %s: %w", path, err)
	}
	var c Case
	if err := json.Unmarshal(data, &c); err != nil {
		return Case{}, fmt.Errorf("corpus: decode %s: %w", path, err)
	}
	if c.CaseID == "" {
		return Case{}, fmt.Errorf("corpus: %s: case has no case_id", path)
	}
	return c, nil
}

// LoadDir loads every *.json case under dir into a map keyed by the full
// case_id. A driver looks up the exact case it drives via ByID (by stable
// short prefix).
func LoadDir(dir string) (map[string]Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("corpus: read dir %s: %w", dir, err)
	}
	out := map[string]Case{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		c, err := Load(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out[c.CaseID] = c
	}
	return out, nil
}

// ByID looks up a case whose case_id starts with the given short id (e.g.
// "PLY-057") from a map keyed by full case_id, returning it and whether a
// unique match was found. The corpus files name cases
// "<SHORT>-<description>", so a driver refers to a case by its stable short
// prefix without hard-coding the descriptive tail.
func ByID(cases map[string]Case, short string) (Case, bool) {
	var found Case
	n := 0
	for id, c := range cases {
		if id == short || hasPrefixToken(id, short) {
			found = c
			n++
		}
	}
	return found, n == 1
}

// hasPrefixToken reports whether full begins with short followed by a '-'
// boundary (so "PLY-05" does not match "PLY-057-...").
func hasPrefixToken(full, short string) bool {
	if len(full) <= len(short) {
		return full == short
	}
	return full[:len(short)] == short && full[len(short)] == '-'
}
