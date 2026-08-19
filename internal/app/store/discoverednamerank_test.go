package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// discoverednamerank_test.go drives REL-110c's rank through the DURABLE layer,
// which is the only layer whose lifetime the defect is about.
//
// mergeDiscovered's own rules are pinned as a unit next door
// (discoveredmerge_internal_test.go). What cannot be proven there is the thing
// the box actually showed: that the rank is on DISK, so it still refuses a worse
// name after the relay process that reported the better one has died and come
// back with an empty memory. A rank held only in the merge would satisfy every
// unit case and none of these.

const (
	nrDisplayName = "onn. 4K Streaming Box"
	nrCastName    = "onn.-4K-Streaming-Bo"
	nrRelay       = "relay-ce2123df9a253d1c927af97766a18307"
)

// nrRow is the onn box as it is mirrored on box .12 — the real identity tuple,
// so the derived id and the merge path are the ones in production.
func nrRow(name, rank string) store.DiscoveredDevice {
	return store.DiscoveredDevice{
		DeviceID:    ddDeviceA,
		RelayID:     nrRelay,
		ScopeNode:   ddNodeID,
		Driver:      "net",
		NativeID:    "48:5c:2c:31:6e:6e",
		DeviceClass: "media-player",
		Name:        name,
		NameRank:    rank,
		Address:     "192.168.50.63",
		LastSeen:    2_000,
	}
}

// THE WHOLE POINT, on disk and across a restart of both processes.
//
// A relay restart re-mints the relay's candidate map from nothing, so the first
// post-restart report is whichever lane swept first — routinely the Machine-
// ranked `_googlecast` record, because the Friendly `_androidtvremote2` one is
// not in every avahi dump. Before REL-110c the durable row took that report on
// presence alone and kept it, which is how the console came to read
// "onn.-4K-Streaming-Bo" for a device that announces "onn. 4K Streaming Box".
//
// Both halves are asserted, because the refusal alone would also be satisfied by
// a mirror that simply never changed a name.
func TestTheDurableNameSurvivesARelayRestartThatReportsAWorseSource(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")

	s := openFileStoreAt(t, path, func() int64 { return ddAppNowMs })
	if _, err := s.ReplaceDiscoveredDevices(ctx, nrRelay, []store.DiscoveredDevice{nrRow(nrDisplayName, "friendly")}); err != nil {
		t.Fatalf("first report: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// THE RELAY RESTARTED. Its whole in-memory ranking is gone; it re-reports
	// this device from whichever record its first sweep found — and the feeder
	// restarted too, so the mirror is the only thing that remembers anything.
	reopened := openFileStoreAt(t, path, func() int64 { return ddAppNowMs })
	stored, err := reopened.ReplaceDiscoveredDevices(ctx, nrRelay, []store.DiscoveredDevice{nrRow(nrCastName, "machine")})
	if err != nil {
		t.Fatalf("post-restart report: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored %d rows, want 1", len(stored))
	}
	if stored[0].Name != nrDisplayName {
		t.Fatalf("name = %q, want %q — the relay's memory died at the restart and the DURABLE rank is the only thing left that can tell a display name from a Cast truncation",
			stored[0].Name, nrDisplayName)
	}
	if stored[0].NameRank != "friendly" {
		t.Fatalf("name_rank = %q, want friendly — the held name and its rank must move as a pair, or the next worse report walks in", stored[0].NameRank)
	}

	// It is on the FILE, not merely in the value the write returned: reopen and
	// ask again, with no report in between.
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	third := openFileStoreAt(t, path, func() int64 { return ddAppNowMs })
	defer func() { _ = third.Close() }()
	rows, err := third.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != nrDisplayName || rows[0].NameRank != "friendly" {
		t.Fatalf("after a reopen the mirror holds %+v, want name %q at rank friendly", rows, nrDisplayName)
	}

	// AND THE RENAME STILL LANDS. The refusal above is only safe because the
	// record that authored the held name keeps sweeping and can restate itself;
	// a mirror that could not be renamed would have traded a flapping name for a
	// frozen one, which is the same defect facing the other way.
	renamed, err := third.ReplaceDiscoveredDevices(ctx, nrRelay, []store.DiscoveredDevice{nrRow("Living Room", "friendly")})
	if err != nil {
		t.Fatalf("rename report: %v", err)
	}
	if renamed[0].Name != "Living Room" {
		t.Fatalf("name = %q, want Living Room — a device restating itself at the same rank is a rename and must land", renamed[0].Name)
	}
}

// THE UPGRADE, driven through the REAL ALTER on a file that genuinely predates
// the column — preOpenPortsStore's fixture DDL, which is the historical shape
// with a name and no rank, seeded with rows written by that build.
//
// Reasoning about the upgrade is not the same as running it, and the difference
// is the whole content of #197: what a column means for the rows that existed
// before it is a decision that has to be visible, and the only way to see it is
// to read those rows back through the build that added it.
//
// Never-wipe is asserted first — the migration must not have touched a stored
// fact — and then the two halves of the decision: an unrecorded rank refuses
// NOTHING, and the first ranked report ENDS the unrecorded state so the ladder
// can protect the row from then on.
func TestAnUpgradedStoreKeepsItsNamesAndThenStartsRankingThem(t *testing.T) {
	ctx := context.Background()
	// A store whose discovered_devices is the pre-REL-110c shape, carrying rows
	// that build wrote. Opening it runs the migration.
	dsn := preOpenPortsStore(t)
	if cols := columnsOf(t, dsn, "discovered_devices"); hasColumn(cols, "name_rank") {
		t.Fatalf("the fixture already has name_rank (%v) — this test would prove nothing about an upgrade", cols)
	}

	upgraded := openFileStoreAt(t, dsn, func() int64 { return ddAppNowMs })
	defer func() { _ = upgraded.Close() }()
	if cols := columnsOf(t, dsn, "discovered_devices"); !hasColumn(cols, "name_rank") {
		t.Fatalf("the open did not add name_rank: %v", cols)
	}

	rows, err := upgraded.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("the upgrade lost rows: %+v", rows)
	}
	var old store.DiscoveredDevice
	for _, r := range rows {
		if r.DeviceID == "dev-mirrored-one" {
			old = r
		}
	}
	if old.Name != "Lobby Roku" || old.Address != "192.168.50.31" {
		t.Fatalf("the upgrade changed a stored fact: %+v — never-wipe", old)
	}
	if old.NameRank != "" {
		t.Fatalf("name_rank = %q, want the empty string — a row written before the column existed must read as UNRECORDED, not as a rank some relay is claimed to have stated (#197)",
			old.NameRank)
	}

	// One report, replacing that relay's set with the one device this case
	// follows (REL-111's wholesale replace; the sibling row is the other relay-
	// less half of the fixture and is not what is under test).
	report := func(name, rank string) store.DiscoveredDevice {
		t.Helper()
		row := old
		row.Name, row.NameRank = name, rank
		row.LastSeen = 0
		stored, err := upgraded.ReplaceDiscoveredDevices(ctx, old.RelayID, []store.DiscoveredDevice{row})
		if err != nil {
			t.Fatalf("report %q/%q: %v", name, rank, err)
		}
		if len(stored) != 1 {
			t.Fatalf("stored %d rows, want 1", len(stored))
		}
		return stored[0]
	}

	// Unrecorded refuses NOTHING: even the weakest ranked report lands.
	landed := report(nrCastName, "machine")
	if landed.Name != nrCastName || landed.NameRank != "machine" {
		t.Fatalf("name/rank = %q/%q, want %q/machine — an unrecorded rank is not evidence of quality, and the landing must record the reported rank",
			landed.Name, landed.NameRank, nrCastName)
	}

	// ...and from that moment the row is protected, which is what makes the one
	// report's worth of wrong above self-healing rather than permanent.
	if healed := report(nrDisplayName, "friendly"); healed.Name != nrDisplayName {
		t.Fatalf("name = %q, want %q", healed.Name, nrDisplayName)
	}
	if pinned := report(nrCastName, "machine"); pinned.Name != nrDisplayName {
		t.Fatalf("name = %q, want %q — once the row carries a real rank the truncation must be refused for good", pinned.Name, nrDisplayName)
	}
}
