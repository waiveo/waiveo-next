package store_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// Pack data retention (MAN-054). The field was declarable and read by nothing,
// which is worse than absent: a pack author declaring `{maxRows: 500}` has told
// an operator their extension is bounded, on an appliance whose disk has filled
// before.

// retentionStore installs a pack whose `retention` block is `spec`.
func retentionStore(t *testing.T, spec string) (*store.Store, *testClock) {
	t.Helper()
	clock := &testClock{ms: 1_752_537_600_000}
	s, err := store.Open(":memory:", clock.now)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	seedPlacementNode(t, s, testScopeNode)

	in := packSpec("acme/menu-board", "1.0.0", 1)
	in.Manifest = json.RawMessage(`{"id":"acme/menu-board","version":"1.0.0","retention":` + spec + `}`)
	if _, _, err := s.InstallPack(context.Background(), in); err != nil {
		t.Fatalf("install: %v", err)
	}
	return s, clock
}

func addRow(t *testing.T, s *store.Store, name string) store.PackRow {
	t.Helper()
	row, err := s.CreatePackRow(context.Background(), "acme/menu-board", "menu_items",
		rowIn(testScopeNode, "", `{"name":"`+name+`"}`))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return row
}

func rowCount(t *testing.T, s *store.Store) int {
	t.Helper()
	rows, err := s.ListPackRows(context.Background(), "acme/menu-board", "menu_items")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return len(rows)
}

// A maxRows bound keeps the newest N and releases the rest, swept as each new
// row lands.
func TestAMaxRowsBoundKeepsTheNewest(t *testing.T) {
	s, clock := retentionStore(t, `{"menu_items":{"maxRows":3}}`)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		addRow(t, s, n)
		clock.advance(10)
	}
	// WHICH rows survived, not merely how many. Counting alone passes when the
	// sweep keeps the OLDEST three — a mutation proved exactly that about the
	// first version of this test, and "3 rows retained" is what a bound that
	// deletes the wrong end looks like.
	if got := bodies(t, s); len(got) != 3 || !got["c"] || !got["d"] || !got["e"] {
		t.Fatalf("retained %v, want exactly the newest three (c d e), with a and b released", keysOfSet(got))
	}
}

// bodies returns the retained rows' `name` values as a set.
func bodies(t *testing.T, s *store.Store) map[string]bool {
	t.Helper()
	rows, err := s.ListPackRows(context.Background(), "acme/menu-board", "menu_items")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		var body struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatalf("decode row body: %v", err)
		}
		out[body.Name] = true
	}
	return out
}

func keysOfSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// A maxAge bound releases rows older than its horizon.
func TestAMaxAgeBoundReleasesOldRows(t *testing.T) {
	s, clock := retentionStore(t, `{"menu_items":{"maxAge":7}}`)
	addRow(t, s, "old")
	clock.advance(8 * 24 * 60 * 60 * 1000) // eight days later
	addRow(t, s, "new")

	if got := bodies(t, s); len(got) != 1 || !got["new"] {
		t.Fatalf("retained %v, want only the row inside the 7-day horizon", keysOfSet(got))
	}
}

// A collection with no entry is unbounded — MAN-054's stated default, and the
// posture every pack shipped so far relies on.
func TestACollectionWithNoRetentionEntryIsUnbounded(t *testing.T) {
	s, clock := retentionStore(t, `{}`)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		addRow(t, s, n)
		clock.advance(10)
	}
	if got := rowCount(t, s); got != 5 {
		t.Fatalf("%d rows retained, want all 5 — no entry means unbounded", got)
	}
}

// `unbounded` is a bare string, not a descriptor. It must read as no bound
// rather than as a bound that failed to parse into something aggressive.
func TestTheUnboundedLiteralIsNotABound(t *testing.T) {
	s, clock := retentionStore(t, `{"menu_items":"unbounded"}`)
	for _, n := range []string{"a", "b", "c"} {
		addRow(t, s, n)
		clock.advance(10)
	}
	if got := rowCount(t, s); got != 3 {
		t.Fatalf("%d rows retained, want all 3", got)
	}
}

// A maxRows of zero is NOT "keep nothing". A declaration that empties a
// collection on every write is far more likely a typo than an intent, and the
// cost of guessing wrong is the pack's entire dataset.
func TestAZeroMaxRowsDoesNotEmptyTheCollection(t *testing.T) {
	s, clock := retentionStore(t, `{"menu_items":{"maxRows":0}}`)
	for _, n := range []string{"a", "b"} {
		addRow(t, s, n)
		clock.advance(10)
	}
	if got := rowCount(t, s); got != 2 {
		t.Fatalf("%d rows retained; a zero bound must not wipe a collection", got)
	}
}

// A bound on ANOTHER collection does not sweep this one. The rule is per
// collection, and a bound that leaked across them would delete data no one
// declared a bound for.
func TestABoundOnAnotherCollectionDoesNotSweepThisOne(t *testing.T) {
	s, clock := retentionStore(t, `{"articles":{"maxRows":1}}`)
	for _, n := range []string{"a", "b", "c"} {
		addRow(t, s, n)
		clock.advance(10)
	}
	if got := rowCount(t, s); got != 3 {
		t.Fatalf("%d rows retained in menu_items; the bound was declared on articles", got)
	}
}
