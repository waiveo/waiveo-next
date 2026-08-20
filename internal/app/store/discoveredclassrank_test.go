package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// discoveredclassrank_test.go drives REL-110d's rank through the DURABLE layer,
// which is the only layer whose lifetime the defect is about.
//
// keepClassFact's own rules are pinned as a unit next door
// (discoveredmerge_internal_test.go). What cannot be proven there is the thing
// that makes #204 an operator-visible defect rather than a flicker: the class is
// written to DISK, so a sweep artifact outlives the relay process that produced
// it and every later sweep that knew better. A rank held only in relay memory —
// which is all the relay had before this change, and only for the duration of
// ONE sweep — would satisfy every unit case and none of these.

const (
	// 192.168.39.241, the device the DURABLE rank exists for: an ecobee
	// thermostat advertising `_ecobee._tcp` -> smart-home at PRODUCT authority
	// beside `_airplay._tcp` and `_spotify-connect._tcp` -> media-player at
	// FEATURE. It is one missing `_ecobee` record from being called a media
	// player, and the rank on the wire is the only thing that can tell the two
	// sweeps apart once the relay's own memory is gone.
	crEcobeeNativeID = "44:61:32:1b:9e:c4"
	crRelay          = "relay-ce2123df9a253d1c927af97766a18307"
)

func crRow(class, rank string) store.DiscoveredDevice {
	return store.DiscoveredDevice{
		DeviceID:    ddDeviceA,
		RelayID:     crRelay,
		ScopeNode:   ddNodeID,
		Driver:      "net",
		NativeID:    crEcobeeNativeID,
		DeviceClass: class,
		ClassRank:   rank,
		Address:     "192.168.39.241",
		LastSeen:    2_000,
	}
}

// THE WHOLE POINT, on disk and across a restart of both processes.
//
// A relay restart re-mints the relay's candidate map from nothing, so the first
// post-restart report is whatever its first sweep's browse happened to hold. If
// that browse is missing the ecobee's own `_ecobee` record — and browses have
// been measured missing records nondeterministically — the relay honestly
// reports media-player, off the `_airplay` and `_spotify-connect` the thermostat
// also advertises. Nothing in relay memory survives to say that verdict came
// from a weaker record than the one before it, so the durable rank is the only
// thing left that can refuse it, and without it the thermostat silently loses
// its command vocabulary (REG-052) because one UDP record was absent from one
// 30-second window.
//
// The recovery is asserted too, because the refusal alone would also be
// satisfied by a mirror that simply never changed a class.
func TestTheDurableRankRefusesAPostRestartSweepThatLostTheProductRecord(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")

	s := openFileStoreAt(t, path, func() int64 { return ddAppNowMs })
	if _, err := s.ReplaceDiscoveredDevices(ctx, crRelay, []store.DiscoveredDevice{crRow("smart-home", "product")}); err != nil {
		t.Fatalf("first report: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// THE RELAY RESTARTED. Its whole in-memory ranking is gone; it re-reports
	// this device from whichever records its first sweep found — and the feeder
	// restarted too, so the mirror is the only thing that remembers anything.
	reopened := openFileStoreAt(t, path, func() int64 { return ddAppNowMs })
	stored, err := reopened.ReplaceDiscoveredDevices(ctx, crRelay, []store.DiscoveredDevice{crRow("media-player", "feature")})
	if err != nil {
		t.Fatalf("post-restart report: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored %d rows, want 1", len(stored))
	}
	if stored[0].DeviceClass != "smart-home" {
		t.Fatalf("device_class = %q, want smart-home — the relay's memory died at the restart and the DURABLE rank is the only thing left that can tell a thermostat's own service from the media features bolted onto it",
			stored[0].DeviceClass)
	}
	if stored[0].ClassRank != "product" {
		t.Fatalf("class_rank = %q, want product — the held class and its rank must move as a pair, or the next worse report walks in", stored[0].ClassRank)
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
	if len(rows) != 1 || rows[0].DeviceClass != "smart-home" || rows[0].ClassRank != "product" {
		t.Fatalf("after a reopen the mirror holds %+v, want smart-home at rank product", rows)
	}

	// AND A GENUINE RECLASSIFICATION STILL LANDS ON DISK, at the same authority.
	// The refusal above is only safe because every rank here is one a sweeping
	// lane restates: the device really is something else — a pack corrected the
	// declared class, or the thermostat was replaced by a speaker on the same
	// MAC — and it says so at the same authority on every sweep from now on. A
	// mirror that refused THAT would have traded a flapping class for a frozen
	// one, which is the same defect facing the other way and strictly worse,
	// because a freeze has no expiry and no operator escape.
	corrected, err := third.ReplaceDiscoveredDevices(ctx, crRelay, []store.DiscoveredDevice{crRow("media-player", "product")})
	if err != nil {
		t.Fatalf("reclassification report: %v", err)
	}
	if corrected[0].DeviceClass != "media-player" || corrected[0].ClassRank != "product" {
		t.Fatalf("class/rank = %q/%q, want media-player/product — an equal-authority restatement is the relay's settled verdict and must land durably",
			corrected[0].DeviceClass, corrected[0].ClassRank)
	}

	// AND IT LANDS IN THE LESS-CONCRETE DIRECTION TOO, which is the arm that
	// actually distinguishes this rule from a concreteness tiebreak. The pack
	// corrects itself back, or a thermostat replaces the speaker on that MAC:
	// same authority, LESS concrete class. A tiebreak on concreteness would
	// refuse this one — silently, permanently, and with no operator action that
	// clears it — while accepting the one above, which is what makes "it can
	// still be reclassified" a half-truth rather than a property.
	back, err := third.ReplaceDiscoveredDevices(ctx, crRelay, []store.DiscoveredDevice{crRow("smart-home", "product")})
	if err != nil {
		t.Fatalf("re-correction report: %v", err)
	}
	if back[0].DeviceClass != "smart-home" {
		t.Fatalf("device_class = %q, want smart-home — an equal-authority correction to a LESS concrete class must land, or the mirror has traded a flapping class for a frozen one",
			back[0].DeviceClass)
	}

	// ...and it is on the file, not just in the returned value: a freeze that
	// only shows up after a reopen is still a freeze.
	if err := third.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fourth := openFileStoreAt(t, path, func() int64 { return ddAppNowMs })
	defer func() { _ = fourth.Close() }()
	after, err := fourth.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	if len(after) != 1 || after[0].DeviceClass != "smart-home" {
		t.Fatalf("after a reopen the mirror holds %+v, want smart-home — the correction must be on the FILE", after)
	}
}

// THE UPGRADE, driven through the REAL ALTER on a file that genuinely predates
// the column — preOpenPortsStore's fixture DDL, seeded with rows that build
// wrote.
//
// Reasoning about the upgrade is not the same as running it, and the difference
// is the whole content of #197: what a column means for the rows that existed
// before it is a decision that has to be visible, and the only way to see it is
// to read those rows back through the build that added it.
//
// Never-wipe is asserted first. Then the decision: an unrecorded rank refuses
// NOTHING — a rank this store never recorded is not evidence of quality, and
// guarding on it would pin every pre-upgrade class against every lane on the
// LAN at disk lifetime — and the row becomes guarded the moment a real rank
// lands on it. That transition is the whole upgrade story, so both halves are
// driven through the real ALTER rather than reasoned about.
func TestAnUpgradedRowRefusesNothingUntilARealRankLandsOnIt(t *testing.T) {
	ctx := context.Background()
	dsn := preOpenPortsStore(t)
	if cols := columnsOf(t, dsn, "discovered_devices"); hasColumn(cols, "class_rank") {
		t.Fatalf("the fixture already has class_rank (%v) — this test would prove nothing about an upgrade", cols)
	}

	upgraded := openFileStoreAt(t, dsn, func() int64 { return ddAppNowMs })
	defer func() { _ = upgraded.Close() }()
	if cols := columnsOf(t, dsn, "discovered_devices"); !hasColumn(cols, "class_rank") {
		t.Fatalf("the open did not add class_rank: %v", cols)
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
	if old.DeviceClass != "media_player" || old.Name != "Lobby Roku" {
		t.Fatalf("the upgrade changed a stored fact: %+v — never-wipe", old)
	}
	if old.ClassRank != "" {
		t.Fatalf("class_rank = %q, want the empty string — a row written before the column existed must read as UNRECORDED, not as a rank some relay is claimed to have stated (#197)",
			old.ClassRank)
	}

	report := func(class, rank string) store.DiscoveredDevice {
		t.Helper()
		row := old
		row.DeviceClass, row.ClassRank = class, rank
		row.LastSeen = 0
		stored, err := upgraded.ReplaceDiscoveredDevices(ctx, old.RelayID, []store.DiscoveredDevice{row})
		if err != nil {
			t.Fatalf("report %q/%q: %v", class, rank, err)
		}
		if len(stored) != 1 {
			t.Fatalf("stored %d rows, want 1", len(stored))
		}
		return stored[0]
	}

	// An unrecorded rank refuses nothing an AUTHORITY-ranked report says: the
	// pre-upgrade row is not evidence of quality, so the ladder must not pin it.
	if landed := report("smart-home", "product"); landed.DeviceClass != "smart-home" || landed.ClassRank != "product" {
		t.Fatalf("class/rank = %q/%q, want smart-home/product — an unrecorded rank is not evidence of quality and must refuse nothing a ranked report states",
			landed.DeviceClass, landed.ClassRank)
	}
	// (An equally UNRANKED report against this unrecorded row is the
	// mixed-version case, driven as a full sweep alternation next door where a
	// pin can actually be distinguished from a flap.)

	// From the moment a real rank lands, the row is guarded against a WORSE-
	// sourced report — a relay restart whose first sweep read only the media
	// features an ecobee also advertises.
	if guarded := report("media-player", "feature"); guarded.DeviceClass != "smart-home" || guarded.ClassRank != "product" {
		t.Fatalf("class/rank = %q/%q, want smart-home/product — once the row carries a real rank a feature-level guess must be refused, and the pair must not split",
			guarded.DeviceClass, guarded.ClassRank)
	}
	// ...and an UNRANKED report is refused too, which is where the class parts
	// company with the name: keepNameFact rule 2 would let the same rolled-back
	// relay overwrite a better-ranked name and reset the row.
	if guarded := report("media-player", ""); guarded.DeviceClass != "smart-home" || guarded.ClassRank != "product" {
		t.Fatalf("class/rank = %q/%q, want smart-home/product — an absent rank sits at the bottom of the ladder and must not be able to remove a device's command vocabulary (REG-052)",
			guarded.DeviceClass, guarded.ClassRank)
	}
	// But the row is not FROZEN: an equal-authority restatement still lands, so
	// a corrected declaration or a genuinely changed device is not stuck.
	if corrected := report("media-player", "product"); corrected.DeviceClass != "media-player" {
		t.Fatalf("device_class = %q, want media-player — an equal-authority restatement must land, or a guarded row becomes an unclearable one", corrected.DeviceClass)
	}
}

// THE MIXED-VERSION WINDOW, on disk — the state the box is in on the day this
// ships, since every relay in the fleet sends no `class_rank` at all.
//
// With nothing ranked on either side there is no authority to reason from, and
// the tempting shortcut is to refuse on the class tokens alone: that would hold
// 192.168.50.43 at media-player with no relay upgrade whatsoever. This test
// exists because that shortcut is the remedy d321893 already rejected on live
// data, and it is far worse on DISK than it was in memory. The ecobee's sweeps
// alternate as records drop in and out, so the first sweep that misses `_ecobee`
// would become a permanent verdict — a thermostat pinned as a media player, with
// every later correct sweep refused, for the life of the file.
//
// So while nothing is ranked this layer behaves exactly as the presence-shaped
// merge it replaces did. The mirror may flap; it may never pin. #204's measured
// instance is fixed on the RELAY, whose cross-sweep memory stops the flapping
// report from ever being sent.
func TestWithNoRankOnTheWireTheMirrorNeverPinsAClassDurably(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	s := openFileStoreAt(t, path, func() int64 { return ddAppNowMs })
	defer func() { _ = s.Close() }()

	// Every report below is what a PRE-REL-110d relay sends: a class and no rank.
	for i, sweep := range []string{"smart-home", "media-player", "smart-home", "media-player", "smart-home"} {
		stored, err := s.ReplaceDiscoveredDevices(ctx, crRelay, []store.DiscoveredDevice{crRow(sweep, "")})
		if err != nil {
			t.Fatalf("sweep %d: %v", i+1, err)
		}
		if stored[0].DeviceClass != sweep {
			t.Fatalf("sweep %d reported %q and the mirror holds %q — with nothing ranked, a refusal is permanent, and a thermostat pinned as a media player on disk is worse than one that flaps",
				i+1, sweep, stored[0].DeviceClass)
		}
		if stored[0].ClassRank != "" {
			t.Fatalf("sweep %d stored class_rank %q, want the empty string — an unranked report must never be stamped with a rank nothing stated", i+1, stored[0].ClassRank)
		}
	}

	// The generic default is still refused, because that guard needs no
	// authority behind it: `unclassified` is a statement of ignorance, not a
	// competing verdict, and a restarting relay states it for every host its
	// mDNS sweep has not reached yet.
	stored, err := s.ReplaceDiscoveredDevices(ctx, crRelay, []store.DiscoveredDevice{crRow("unclassified", "")})
	if err != nil {
		t.Fatalf("generic-default report: %v", err)
	}
	if stored[0].DeviceClass != "smart-home" {
		t.Fatalf("device_class = %q, want smart-home — the gap rule must survive having no ranks to compare", stored[0].DeviceClass)
	}
}

// THE NAME'S ROLLBACK HOLE, MADE AUDIBLE (#202's decided weakness).
//
// keepNameFact rule 2 lets an unranked report discard a ranked row's ladder.
// That disposition is deliberate and stays — the alternative makes a rename
// impossible against an older relay, with no symptom an operator could act on —
// but it is unreachable from a current relay, so when it DOES fire the cause is
// a rolled-back or un-upgraded peer holding incumbency, and #202's protection
// has quietly stopped applying to that device. Silence was the part worth fixing.
//
// Asserted as behaviour rather than as a log-scrape: the reset must still HAPPEN
// (the policy is unchanged) and it must be the once-per-device path, not a line
// on every minute's report.
func TestAnUnrankedReportStillResetsTheNameLadderAndSaysSoOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	s := openFileStoreAt(t, path, func() int64 { return ddAppNowMs })
	defer func() { _ = s.Close() }()

	row := func(class, name, rank string) store.DiscoveredDevice {
		r := crRow(class, "product")
		r.Name, r.NameRank = name, rank
		return r
	}

	if _, err := s.ReplaceDiscoveredDevices(ctx, crRelay,
		[]store.DiscoveredDevice{row("smart-home", "Hallway Thermostat", "friendly")}); err != nil {
		t.Fatalf("first report: %v", err)
	}

	// A ROLLED-BACK RELAY, speaking neither REL-110c nor REL-110d: it states a
	// name and a class, and ranks neither. ONE report, TWO fields, and the two
	// requirements deliberately answer it differently.
	unranked := row("media-player", "hanger-a1b2c3", "")
	unranked.ClassRank = ""
	stored, err := s.ReplaceDiscoveredDevices(ctx, crRelay, []store.DiscoveredDevice{unranked})
	if err != nil {
		t.Fatalf("unranked report: %v", err)
	}

	// THE NAME SURRENDERS. Rule 2 still applies — the policy is not what changed
	// — because a rename must always be able to land and a wrong name is
	// cosmetic.
	if stored[0].Name != "hanger-a1b2c3" || stored[0].NameRank != "" {
		t.Fatalf("name/rank = %q/%q, want hanger-a1b2c3/unrecorded — keepNameFact rule 2 is unchanged: an unranked report is merged on presence and the row returns to unrecorded",
			stored[0].Name, stored[0].NameRank)
	}

	// THE CLASS, ON THE SAME REPORT, REFUSES — and it is a DIFFERENT class being
	// refused, or this assertion would prove nothing. A device_class governs the
	// command vocabulary (REG-052), so an un-upgraded peer must not be able to
	// strip a thermostat's commands by calling it a media player.
	if stored[0].DeviceClass != "smart-home" || stored[0].ClassRank != "product" {
		t.Fatalf("class/rank = %q/%q, want smart-home/product — the class must NOT copy keepNameFact rule 2",
			stored[0].DeviceClass, stored[0].ClassRank)
	}

	// And a repeat of the same unranked report is idempotent — both notes are
	// once per device, so nothing here may change on the second pass.
	again, err := s.ReplaceDiscoveredDevices(ctx, crRelay, []store.DiscoveredDevice{unranked})
	if err != nil {
		t.Fatalf("repeat report: %v", err)
	}
	if again[0].Name != "hanger-a1b2c3" || again[0].NameRank != "" || again[0].DeviceClass != "smart-home" {
		t.Fatalf("the repeat report changed the row: %+v", again[0])
	}

	// THE ESCAPE the name does not need and the class does: upgrade the relay
	// again and the correction lands. The class's refusal is bounded by the
	// relay's version, not by the life of the file.
	upgraded, err := s.ReplaceDiscoveredDevices(ctx, crRelay,
		[]store.DiscoveredDevice{row("media-player", "hanger-a1b2c3", "")})
	if err != nil {
		t.Fatalf("re-upgraded report: %v", err)
	}
	if upgraded[0].DeviceClass != "media-player" {
		t.Fatalf("device_class = %q, want media-player — a relay that states a rank again must be able to reclassify the device it could not correct while rolled back",
			upgraded[0].DeviceClass)
	}
}
