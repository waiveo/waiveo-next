package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// ignoreddevices_test.go drives the ignore decision as what it must be: a
// durable, reversible, app-side annotation that outlives a restart. The mirror's
// own test proves persistence with a reopen; this file does the same, because an
// ignore that did not survive the reopen would silently un-ignore every device
// every time the box rebooted.

const (
	igDeviceA = "01J8Z8IGN0RED0EV1CEAAAAAA1"
	igDeviceB = "01J8Z8IGN0RED0EV1CEBBBBBB2"
)

func TestIgnoreDeviceIsCreatedThenIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openFileStore(t, filepath.Join(t.TempDir(), "app.db"))
	defer func() { _ = s.Close() }()

	created, err := s.IgnoreDevice(ctx, igDeviceA, 1_000)
	if err != nil {
		t.Fatalf("IgnoreDevice: %v", err)
	}
	if !created {
		t.Fatalf("first IgnoreDevice: created = false, want true")
	}
	// A second ignore of the same device is a benign no-op — a double-click or a
	// retried request, not a conflict.
	created, err = s.IgnoreDevice(ctx, igDeviceA, 2_000)
	if err != nil {
		t.Fatalf("second IgnoreDevice: %v", err)
	}
	if created {
		t.Fatalf("second IgnoreDevice: created = true, want false")
	}

	ids, err := s.ListIgnoredDeviceIDs(ctx)
	if err != nil {
		t.Fatalf("ListIgnoredDeviceIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != igDeviceA {
		t.Fatalf("ListIgnoredDeviceIDs = %v, want [%s]", ids, igDeviceA)
	}
}

// TestIgnoreDecisionSurvivesAReopen is the requirement the durable table exists
// for: the decision must be re-appliable after a restart, before any relay has
// reconnected.
func TestIgnoreDecisionSurvivesAReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")

	first := openFileStore(t, path)
	if _, err := first.IgnoreDevice(ctx, igDeviceA, 1_000); err != nil {
		t.Fatalf("IgnoreDevice: %v", err)
	}
	if _, err := first.IgnoreDevice(ctx, igDeviceB, 1_000); err != nil {
		t.Fatalf("IgnoreDevice: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := openFileStore(t, path)
	defer func() { _ = second.Close() }()
	ids, err := second.ListIgnoredDeviceIDs(ctx)
	if err != nil {
		t.Fatalf("ListIgnoredDeviceIDs after reopen: %v", err)
	}
	if len(ids) != 2 || ids[0] != igDeviceA || ids[1] != igDeviceB {
		t.Fatalf("ListIgnoredDeviceIDs after reopen = %v, want [%s %s]", ids, igDeviceA, igDeviceB)
	}
}

func TestUnignoreDeviceIsReversibleAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openFileStore(t, filepath.Join(t.TempDir(), "app.db"))
	defer func() { _ = s.Close() }()

	if _, err := s.IgnoreDevice(ctx, igDeviceA, 1_000); err != nil {
		t.Fatalf("IgnoreDevice: %v", err)
	}

	removed, err := s.UnignoreDevice(ctx, igDeviceA)
	if err != nil {
		t.Fatalf("UnignoreDevice: %v", err)
	}
	if !removed {
		t.Fatalf("UnignoreDevice: removed = false, want true")
	}
	ids, err := s.ListIgnoredDeviceIDs(ctx)
	if err != nil {
		t.Fatalf("ListIgnoredDeviceIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ListIgnoredDeviceIDs after unignore = %v, want []", ids)
	}
	// Un-ignoring a device that is not ignored is a no-op, not an error.
	removed, err = s.UnignoreDevice(ctx, igDeviceA)
	if err != nil {
		t.Fatalf("second UnignoreDevice: %v", err)
	}
	if removed {
		t.Fatalf("second UnignoreDevice: removed = true, want false")
	}
}

func TestIgnoreDeviceRejectsEmptyID(t *testing.T) {
	ctx := context.Background()
	s := openFileStore(t, filepath.Join(t.TempDir(), "app.db"))
	defer func() { _ = s.Close() }()

	if _, err := s.IgnoreDevice(ctx, "", 1_000); err == nil {
		t.Fatalf("IgnoreDevice(\"\"): err = nil, want an error")
	}
	if _, err := s.UnignoreDevice(ctx, ""); err == nil {
		t.Fatalf("UnignoreDevice(\"\"): err = nil, want an error")
	}
}

// TestOpenPortsSurviveAReopen closes the hole the deadcode gate caught by
// accident: the column, the SELECT and the decode all landed while the INSERT
// did not, so ports would have read back as `[]` forever and looked exactly like
// a device nothing had scanned. Asserted across a REOPEN, which is the only way
// to tell a value that was stored from one that was merely held in memory.
func TestOpenPortsSurviveAReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")

	first := openFileStore(t, path)
	if _, err := first.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{{
		DeviceID: igDeviceA, RelayID: "relay-a", ScopeNode: ddNodeID,
		Driver: "net", NativeID: "c4:8b:66:68:21:25", DeviceClass: "unclassified",
		FirstSeen: 1000, LastSeen: 2000, OpenPorts: []int{80, 8060},
	}}); err != nil {
		t.Fatalf("ReplaceDiscoveredDevices: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := openFileStore(t, path)
	defer func() { _ = second.Close() }()
	rows, err := second.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if got := rows[0].OpenPorts; len(got) != 2 || got[0] != 80 || got[1] != 8060 {
		t.Fatalf("open ports after reopen = %v, want [80 8060] — a column written by nothing reads back empty and looks unscanned", got)
	}
}
