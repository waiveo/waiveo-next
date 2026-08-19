package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// devicefirstseen_internal_test.go covers the one case an external test cannot
// construct: a store FILE written by the build that predates the first-seen
// ledger. Such a file has the mirror rows and no ledger at all, and the fix must
// take the history it finds there rather than restamping the whole site as new
// at the moment of the upgrade.
//
// It is internal because staging that file means reaching past the public API to
// delete the ledger the current build always writes — there is deliberately no
// exported way to produce a mirror row with no ledger row behind it.

const fsInternalAppNow = int64(1_800_000_000_000)

// stagePreLedgerStore writes a file in the shape the pre-ledger build left
// behind: one mirror row carrying columnValue in `first_seen`, and no ledger row
// at all.
func stagePreLedgerStore(t *testing.T, columnValue int64) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")

	old, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := old.ReplaceDiscoveredDevices(ctx, "relay-a", []DiscoveredDevice{{
		DeviceID: preLedgerDeviceID, RelayID: "relay-a", ScopeNode: "01J8Z8D1SC0VEREDN0DE000001",
		Driver: "roku-ecp", NativeID: "uuid:roku:ecp:X1", DeviceClass: "media-player",
		FirstSeen: 900_000, LastSeen: 900_000,
	}}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if _, err := old.db.ExecContext(ctx, `DELETE FROM device_first_seen`); err != nil {
		t.Fatalf("stage the pre-ledger file: %v", err)
	}
	if _, err := old.db.ExecContext(ctx, `UPDATE discovered_devices SET first_seen = ?`, columnValue); err != nil {
		t.Fatalf("stage the older build's value: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

const preLedgerDeviceID = "01J8Z8D1SC0VEREDDEV1CEAAA1"

// ledgerValue reads the staged device's ledger row, reporting whether there is
// one at all.
func ledgerValue(t *testing.T, s *Store) (int64, bool) {
	t.Helper()
	var v int64
	switch err := s.db.QueryRowContext(context.Background(),
		`SELECT first_seen FROM device_first_seen WHERE device_id = ?`, preLedgerDeviceID).Scan(&v); {
	case err == nil:
		return v, true
	case errors.Is(err, sql.ErrNoRows):
		return 0, false
	default:
		t.Fatalf("read ledger: %v", err)
		return 0, false
	}
}

// reportOnce sends one ordinary post-upgrade report for the staged device — the
// relay has just restarted, so it claims the device was first seen a moment ago.
func reportOnce(t *testing.T, s *Store, relayLastSeen int64) DiscoveredDevice {
	t.Helper()
	ctx := context.Background()
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []DiscoveredDevice{{
		DeviceID: preLedgerDeviceID, RelayID: "relay-a", ScopeNode: "01J8Z8D1SC0VEREDN0DE000001",
		Driver: "roku-ecp", NativeID: "uuid:roku:ecp:X1", DeviceClass: "media-player",
		FirstSeen: relayLastSeen - 100, LastSeen: relayLastSeen,
	}}); err != nil {
		t.Fatalf("post-upgrade report: %v", err)
	}
	rows, err := readDiscoveredDevices(ctx, s.db, preLedgerDeviceID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read back: %v (rows=%d)", err, len(rows))
	}
	return rows[0]
}

// TestTheLedgerAdoptsAnOlderBuildsColumn is never-wipe applied to this change's
// own upgrade. The pre-ledger column held the wrong value (the last relay boot),
// but wrong is not worthless: it is a real instant at which this side held a
// report of the device, and it is earlier than anything the first post-upgrade
// report can say. Dropping it would commit defect #196 one final time, in the
// commit that fixes it.
func TestTheLedgerAdoptsAnOlderBuildsColumn(t *testing.T) {
	// The value box .12 actually carries: a plausible instant, months before the
	// app clock these cases run on.
	const olderBuildsValue = 1_787_098_315_675
	path := stagePreLedgerStore(t, olderBuildsValue)

	upgraded, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	seeded, ok := ledgerValue(t, upgraded)
	if !ok {
		t.Fatalf("the ledger has no row for a device the mirror holds")
	}
	if seeded != olderBuildsValue {
		t.Errorf("seeded first_seen = %d, want the older build's %d — the upgrade must adopt the history it finds, not restamp it",
			seeded, olderBuildsValue)
	}

	// And the very next report — which claims the device was first seen a moment
	// ago, because the relay has just restarted — does not undo the rescue.
	if got := reportOnce(t, upgraded, 500).FirstSeen; got != olderBuildsValue {
		t.Errorf("first_seen after the first post-upgrade report = %d, want %d preserved", got, olderBuildsValue)
	}
}

// TestABrokenClockAtUpgradeDoesNotDestroyThePreLedgerHistory is never-wipe
// aimed at the seam between the two refusals this change makes.
//
// A box being upgraded whose own clock has not yet been set reads below
// minPlausibleInstantMs, so the backfill imports nothing and the plant writes
// nothing — correctly, both times. What must NOT follow is the projection
// writing its zero over the mirror column, because on that box the column is the
// pre-ledger history and it is the only copy. Blanking it would mean the rescue
// could never run once the clock came right: refusing to answer must not mean
// deleting the answer somebody else could still give.
func TestABrokenClockAtUpgradeDoesNotDestroyThePreLedgerHistory(t *testing.T) {
	const olderBuildsValue = 1_787_098_315_675
	path := stagePreLedgerStore(t, olderBuildsValue)

	// The upgrade boot happens before NTP lands: the app clock reads 1970.
	brokenNow := int64(1_000_000_000)
	broken, err := Open(path, func() int64 { return brokenNow })
	if err != nil {
		t.Fatalf("reopen on a broken clock: %v", err)
	}
	if _, ok := ledgerValue(t, broken); ok {
		t.Errorf("the ledger seeded itself from a clock that reads 1970")
	}
	if got := reportOnce(t, broken, 500).FirstSeen; got != olderBuildsValue {
		t.Fatalf("first_seen after a report on a broken clock = %d, want the pre-ledger %d left alone — "+
			"the column is the only copy of that history until the backfill can run", got, olderBuildsValue)
	}
	if err := broken.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The clock comes right and the box reboots. The rescue runs now.
	fixed, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("reopen on a working clock: %v", err)
	}
	t.Cleanup(func() { _ = fixed.Close() })
	seeded, ok := ledgerValue(t, fixed)
	if !ok || seeded != olderBuildsValue {
		t.Errorf("seeded first_seen = %d (present=%v), want the older build's %d — "+
			"the history had to survive the boot that could not read it", seeded, ok, olderBuildsValue)
	}
	if got := reportOnce(t, fixed, 600).FirstSeen; got != olderBuildsValue {
		t.Errorf("served first_seen = %d, want %d", got, olderBuildsValue)
	}
}

// TestAPreLedgerLastSeenIsReplacedNotCarriedForward pins the guard rather than
// the mechanism.
//
// The freeze rule holds last_seen still when the relay's stamp is unchanged
// since the previous report. On a row this build has never written there IS no
// previous report to compare against — the comparator column defaults to 0 at
// the upgrade that adds it — and the stored last_seen it would be holding still
// is the OLDER build's, read off the relay's clock. Carrying that forward would
// serve a relay-clock instant from a field the whole design says is this side's,
// and it needs a relay reporting a zero stamp to do it, which is precisely the
// input a relay is not trusted to get right.
//
// So a missing comparator means "no evidence of a replay", not "identical".
func TestAPreLedgerLastSeenIsReplacedNotCarriedForward(t *testing.T) {
	ctx := context.Background()
	path := stagePreLedgerStore(t, 1_787_098_315_675)

	// What the pre-ledger build left in last_seen: the reporting relay's own
	// wall-clock reading, a hundred days off this side's.
	const olderBuildsLastSeen = 1_791_360_000_000
	staged, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := staged.db.ExecContext(ctx,
		`UPDATE discovered_devices SET last_seen = ?, relay_last_seen = 0`, olderBuildsLastSeen); err != nil {
		t.Fatalf("stage the older build's last_seen: %v", err)
	}
	t.Cleanup(func() { _ = staged.Close() })

	// The first post-upgrade report, from a relay that reports no stamp at all.
	if got := reportOnce(t, staged, 0).LastSeen; got != fsInternalAppNow {
		t.Errorf("last_seen after the first post-upgrade report = %d, want this side's %d — "+
			"with no comparator there is no replay to detect, and %d is the relay's clock, not ours",
			got, fsInternalAppNow, olderBuildsLastSeen)
	}
}

// TestAnImplausiblePreLedgerValueIsNotAdopted bounds the ONE path by which a
// relay's clock can still reach this table.
//
// The pre-ledger column was written from the reporting relay's wall clock, and
// relay/1's `clock_state` is hardcoded `untrusted` in every live relay, so a
// relay whose clock had never been set wrote a near-epoch value into it. Adopting
// that would show "20833d ago" in the console forever — on exactly the device
// population the backfill exists to rescue, and unreachable afterwards because
// the ledger is plant-once.
//
// It is REFUSED rather than clamped. A clamp would invent a durable fact ("first
// seen precisely thirty days ago") that no evidence supports; refusing leaves the
// device with no answer until the next report plants a real one, which is the
// same repairable-absence the app's own broken clock produces.
func TestAnImplausiblePreLedgerValueIsNotAdopted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value int64
	}{
		// A relay that booted at the epoch, which is REL-136's cold boot.
		{"a relay whose clock was never set", 1_000_000},
		// A relay running ahead of this side: an age that renders as negative.
		{"a relay running ahead of the app", fsInternalAppNow + 90*24*60*60*1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := stagePreLedgerStore(t, tc.value)
			upgraded, err := Open(path, func() int64 { return fsInternalAppNow })
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			t.Cleanup(func() { _ = upgraded.Close() })

			if seeded, ok := ledgerValue(t, upgraded); ok {
				t.Errorf("the upgrade adopted %d as a first_seen (%d days from the app's now) — "+
					"a value this side could not have observed is not history", seeded, (fsInternalAppNow-seeded)/(24*60*60*1000))
			}

			// And it is repaired by the ordinary next report rather than left
			// permanently blank.
			if got := reportOnce(t, upgraded, 500).FirstSeen; got != fsInternalAppNow {
				t.Errorf("first_seen after the first post-upgrade report = %d, want %d — "+
					"refusing a bad value must leave the device repairable", got, fsInternalAppNow)
			}
		})
	}
}
