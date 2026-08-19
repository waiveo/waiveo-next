package store_test

import (
	"context"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// devicefirstseen_test.go drives the axis every earlier discovery test was
// missing: a report from a relay that has just RESTARTED, and a report from a
// relay whose clock cannot be believed.
//
// Every existing restart case in this package and in cmd/waiveo-feeder restarts
// the FEEDER — reopen the store, restore the registry — which is the half that
// always worked. The half that never did is the other process: a relay does not
// persist candidates, so its in-memory candidate map is empty at every start and
// every device on the LAN looks brand new to it. Box .12 measured the
// consequence three times: all 61 rows carrying a first_seen equal, to the
// second, to the relay's process start.
//
// The second axis is the one adversarial review opened. A relay's timestamps
// come off an unattested wall clock — relay/1's own `clock_state` is hardcoded
// `untrusted` in every live relay — so a design that reads them at all can be
// made to write a durable lie by nothing worse than a Pi booting with a stale
// clock. Half the cases here report a clock that has jumped and assert the
// durable value did not move.

// fsClock is a clock a case can advance by hand. The stored instants are stamped
// from the STORE's clock (the app's SEC-066 floor in a deployment), so a case
// about "an hour later" has to move the clock rather than sleep.
type fsClock struct{ ms int64 }

func (c *fsClock) now() int64      { return c.ms }
func (c *fsClock) advance(d int64) { c.ms += d }

// fsOpen opens an in-memory store on a hand-driven clock.
func fsOpen(t *testing.T, c *fsClock) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:", c.now)
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// fsRelayBase is the reporting relay's own wall clock in these cases,
// deliberately a hundred days adrift from the app clock the store runs on. The
// two are different machines' readings of the time and NOTHING in this store may
// depend on them agreeing, on their difference, or on either being sane.
const fsRelayBase int64 = 1_791_360_000_000

const fsDay int64 = 24 * 60 * 60 * 1000

// fsRow is one reported row with the two seen instants named, since they are the
// subject of every case here.
func fsRow(deviceID, relayID, nativeID string, firstSeen, lastSeen int64) store.DiscoveredDevice {
	d := discovered(deviceID, relayID, nativeID)
	d.FirstSeen, d.LastSeen = firstSeen, lastSeen
	return d
}

// fsAged is the same row expressed the way a report actually reads: the relay's
// current instant, and how long before it the relay believes it first saw the
// device.
func fsAged(deviceID, relayID, nativeID string, relayNowMs, ageMs int64) store.DiscoveredDevice {
	return fsRow(deviceID, relayID, nativeID, relayNowMs-ageMs, relayNowMs)
}

// fsStored reads one mirrored row back, failing the case when it is absent.
func fsStored(t *testing.T, s *store.Store, deviceID string) store.DiscoveredDevice {
	t.Helper()
	rows, err := s.DiscoveredDevices(context.Background())
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	for _, d := range rows {
		if d.DeviceID == deviceID {
			return d
		}
	}
	t.Fatalf("device %s is not mirrored; rows = %+v", deviceID, rows)
	return store.DiscoveredDevice{}
}

// TestFirstSeenSurvivesARelayRestart is defect #196 itself.
//
// The relay is up for an hour, then restarts. Its second report carries a
// first_seen it minted at its own process start — the same instant as its
// last_seen, because to a freshly started relay every device was first seen just
// now. The durable value must not move.
func TestFirstSeenSurvivesARelayRestart(t *testing.T) {
	ctx := context.Background()
	clk := &fsClock{ms: 1_800_000_000_000}
	s := fsOpen(t, clk)

	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase, 30*60*1000),
	}); err != nil {
		t.Fatalf("first report: %v", err)
	}
	born := fsStored(t, s, ddDeviceA).FirstSeen
	if born != clk.now() {
		t.Fatalf("first_seen at birth = %d, want this site's own clock %d", born, clk.now())
	}

	// An hour passes and the relay process restarts. Its candidate map is empty,
	// so it re-mints first_seen at its own start — note the timestamps are also
	// on a completely different scale, which is what a restarted process's clock
	// readings look like.
	clk.advance(60 * 60 * 1000)
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsRow(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", 42, 300),
	}); err != nil {
		t.Fatalf("report after the relay restart: %v", err)
	}

	got := fsStored(t, s, ddDeviceA)
	if got.FirstSeen != born {
		t.Errorf("first_seen = %d after a relay restart, want the durable %d unchanged — "+
			"a relay's memory is shorter than this record and must not overwrite it", got.FirstSeen, born)
	}
	// last_seen is the other half of the pair and must NOT be frozen here: the
	// restarted relay re-minted this candidate, which it could only do by
	// actually seeing the device again.
	if got.LastSeen != clk.now() {
		t.Errorf("last_seen = %d, want %d — a re-minted candidate is a fresh sighting", got.LastSeen, clk.now())
	}
	if got.FirstSeen > got.LastSeen {
		t.Errorf("first_seen %d is after last_seen %d — a row whose two instants are readings of two clocks",
			got.FirstSeen, got.LastSeen)
	}
}

// TestFirstSeenIsPlantedOnceAndNeverMoves is the merge rule stated as a
// property, over the two directions a report can push and the two arms a
// statement can have.
//
// It matters that BOTH directions are refused. "Never forward" is defect #196.
// "Never backward" is the finding adversarial review proved against the first
// attempt at this fix: a report may not lower a durable value either, because
// the only evidence it could offer for an earlier sighting is arithmetic on an
// unattested clock, and a merge that can be pushed down can be pushed down
// permanently.
func TestFirstSeenIsPlantedOnceAndNeverMoves(t *testing.T) {
	ctx := context.Background()
	clk := &fsClock{ms: 1_800_000_000_000}
	s := fsOpen(t, clk)

	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase, 0),
	}); err != nil {
		t.Fatalf("first report: %v", err)
	}
	planted := fsStored(t, s, ddDeviceA).FirstSeen
	if planted != clk.now() {
		t.Fatalf("first_seen at birth = %d, want %d", planted, clk.now())
	}

	// Forward: a relay that has forgotten everything says "first seen just now".
	clk.advance(60_000)
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase+60_000, 0),
	}); err != nil {
		t.Fatalf("forgetful report: %v", err)
	}
	if got := fsStored(t, s, ddDeviceA).FirstSeen; got != planted {
		t.Errorf("first_seen = %d after a re-minted report, want %d — never forward", got, planted)
	}

	// Backward: the same relay now claims it has been watching for two hours.
	clk.advance(60_000)
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase+120_000, 2*60*60*1000),
	}); err != nil {
		t.Fatalf("backdated report: %v", err)
	}
	if got := fsStored(t, s, ddDeviceA).FirstSeen; got != planted {
		t.Errorf("first_seen = %d after a backdated report, want %d — a relay's clock is not evidence", got, planted)
	}
}

// TestARelayClockCorrectionDoesNotAgeTheSite is the blocking finding, reproduced
// as the case that proved it.
//
// A relay boots with a stale clock — REL-136's cold boot, and the ordinary Pi
// with no RTC — and stamps both devices at that reading. NTP then corrects it
// and one device is re-observed, so that candidate's last_seen jumps forward
// while its first_seen, which deviceplane stamps once at birth and never
// revises, stays behind. The gap between the two is now the size of the clock
// correction, and a design that reads it as the device's age backdated the whole
// site by it — permanently, because nothing afterwards could raise it again.
func TestARelayClockCorrectionDoesNotAgeTheSite(t *testing.T) {
	ctx := context.Background()
	clk := &fsClock{ms: 1_800_000_000_000}
	s := fsOpen(t, clk)

	const staleReading = 500_000
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsRow(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", staleReading, staleReading),
		fsRow(ddDeviceB, "relay-a", "uuid:roku:ecp:X2", staleReading, staleReading),
	}); err != nil {
		t.Fatalf("pre-correction report: %v", err)
	}
	born := clk.now()

	clk.advance(60_000)
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		// A was re-observed after the correction; B was not.
		fsRow(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", staleReading, fsRelayBase),
		fsRow(ddDeviceB, "relay-a", "uuid:roku:ecp:X2", staleReading, staleReading),
	}); err != nil {
		t.Fatalf("post-correction report: %v", err)
	}

	for _, id := range []string{ddDeviceA, ddDeviceB} {
		if got := fsStored(t, s, id).FirstSeen; got != born {
			t.Errorf("%s first_seen = %d after the relay's clock was corrected, want %d — "+
				"the correction is news about the relay's clock, not about the device's age (backdated %d days)",
				id, got, born, (born-got)/fsDay)
		}
	}

	// An hour of healthy reports must not drift it either: a value that can be
	// pushed once can be pushed every minute.
	for i := 0; i < 60; i++ {
		clk.advance(60_000)
		if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
			fsRow(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", staleReading, fsRelayBase+int64(i+1)*60_000),
			fsRow(ddDeviceB, "relay-a", "uuid:roku:ecp:X2", staleReading, staleReading),
		}); err != nil {
			t.Fatalf("healthy report %d: %v", i, err)
		}
	}
	if got := fsStored(t, s, ddDeviceA).FirstSeen; got != born {
		t.Errorf("first_seen = %d an hour later, want %d", got, born)
	}
}

// TestAForwardClockStepDoesNotAgeANewDevice is the same lie told at BIRTH, which
// is where it does the most damage: on a fresh install every device is
// discovered inside the pre-NTP window, so accepting it mis-ages the entire
// inventory from the first boot and plant-once could never repair it.
//
// The relay here is an ordinary Pi: no RTC, fake-hwclock restoring the last
// shutdown time, so it boots three days behind, discovers the device seconds
// later, and NTP steps it forward. Its report says, in perfect good faith, that
// it has been watching a device it met moments ago for three days.
func TestAForwardClockStepDoesNotAgeANewDevice(t *testing.T) {
	ctx := context.Background()
	clk := &fsClock{ms: 1_800_000_000_000}
	s := fsOpen(t, clk)

	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase, 3*fsDay),
		// And its neighbour on a relay whose clock was never set at all.
		fsRow(ddDeviceB, "relay-a", "uuid:roku:ecp:X2", 1, fsRelayBase),
	}); err != nil {
		t.Fatalf("report: %v", err)
	}

	for _, id := range []string{ddDeviceA, ddDeviceB} {
		got := fsStored(t, s, id).FirstSeen
		if got != clk.now() {
			t.Errorf("%s first_seen = %d, want %d — a device discovered moments ago reads as %d days old",
				id, got, clk.now(), (clk.now()-got)/fsDay)
		}
	}
}

// TestABrokenAppClockPlantsNoAgeAndHeals is the other end of the same rule. The
// value is stamped from the app's clock, and a box with a dead RTC reads at or
// near the epoch until NTP lands — SEC-066's floor is 0 on a deployment that has
// never advanced one. Storing that reading would pin the device to 1970 forever
// under a merge that never moves, which is the worst outcome available: the
// console would show it as fifty years old and no later evidence could reach it.
//
// So nothing is planted at all, the row reads absent (the API omits the member,
// the console renders an em dash), and the first report on a working clock
// plants the real answer.
func TestABrokenAppClockPlantsNoAgeAndHeals(t *testing.T) {
	ctx := context.Background()
	clk := &fsClock{ms: 1_000_000_000} // 1970-01-12
	s := fsOpen(t, clk)

	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase, 0),
	}); err != nil {
		t.Fatalf("report on a broken clock: %v", err)
	}
	if got := fsStored(t, s, ddDeviceA).FirstSeen; got != 0 {
		t.Errorf("first_seen = %d, want 0 (absent) — an epoch reading is not a time and cannot be unwritten", got)
	}

	clk.ms = 1_800_000_000_000 // NTP lands
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase+60_000, 0),
	}); err != nil {
		t.Fatalf("report on a working clock: %v", err)
	}
	if got := fsStored(t, s, ddDeviceA).FirstSeen; got != clk.now() {
		t.Errorf("first_seen = %d after the clock was fixed, want %d — refusing to answer must be repairable", got, clk.now())
	}
}

// TestLastSeenFreezesWhenTheDeviceGoesDark is what makes "not heard from since"
// answerable, and it is not free: internal/relay/deviceplane never expires a
// candidate, so an unplugged device is still in every report the relay sends,
// carrying the identical frozen stamps forever. Dating the row from the report's
// arrival — or from the batch's own newest stamp, which on a site whose devices
// all went dark together IS the frozen one — would say a week-dead device was
// seen just now.
func TestLastSeenFreezesWhenTheDeviceGoesDark(t *testing.T) {
	ctx := context.Background()
	clk := &fsClock{ms: 1_800_000_000_000}
	s := fsOpen(t, clk)

	report := []store.DiscoveredDevice{fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase, fsDay)}
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", report); err != nil {
		t.Fatalf("first report: %v", err)
	}
	wentDark := clk.now()

	// The device is unplugged. The relay keeps re-sending the identical candidate
	// every minute for a week.
	for day := 1; day <= 7; day++ {
		clk.advance(fsDay)
		if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", report); err != nil {
			t.Fatalf("day %d report: %v", day, err)
		}
		if got := fsStored(t, s, ddDeviceA).LastSeen; got != wentDark {
			t.Fatalf("day %d: last_seen = %d, want it frozen at %d — the relay re-sent a candidate it has not re-observed, "+
				"and the row now claims the device was seen %ds ago", day, got, wentDark, (clk.now()-got)/1000)
		}
	}
	if age := clk.now() - fsStored(t, s, ddDeviceA).LastSeen; age != 7*fsDay {
		t.Errorf("after a week dark the row reads %d ms old, want %d", age, 7*fsDay)
	}
}

// TestLastSeenAdvancesWhenTheRelaySeesItAgain is the guard on the guard: freezing
// a stale row must not become never advancing one. The relay advances a
// candidate's last_seen only when a lane actually re-observed the device, so a
// changed stamp is a sighting and the row follows it.
func TestLastSeenAdvancesWhenTheRelaySeesItAgain(t *testing.T) {
	ctx := context.Background()
	clk := &fsClock{ms: 1_800_000_000_000}
	s := fsOpen(t, clk)

	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase, fsDay),
		fsAged(ddDeviceB, "relay-a", "uuid:roku:ecp:X2", fsRelayBase, fsDay),
	}); err != nil {
		t.Fatalf("first report: %v", err)
	}
	together := clk.now()

	// An hour later the relay has re-observed A and not B — the ordinary mixed
	// case, and the one a whole-batch reference point gets wrong in the other
	// direction by dating B from A's sighting.
	clk.advance(60 * 60 * 1000)
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase+60*60*1000, fsDay+60*60*1000),
		fsAged(ddDeviceB, "relay-a", "uuid:roku:ecp:X2", fsRelayBase, fsDay),
	}); err != nil {
		t.Fatalf("second report: %v", err)
	}

	if got := fsStored(t, s, ddDeviceA).LastSeen; got != clk.now() {
		t.Errorf("re-observed device last_seen = %d, want %d", got, clk.now())
	}
	if got := fsStored(t, s, ddDeviceB).LastSeen; got != together {
		t.Errorf("un-re-observed device last_seen = %d, want it left at %d — one device's sighting is not another's",
			got, together)
	}
}

// TestFirstSeenOutlivesTheRowItself is why the value lives in its own table.
//
// A device that drops off the LAN is deleted from the mirror by the very next
// report (the replace is a full set). If first_seen lived only in that row, a TV
// unplugged for a weekend would come back as a brand-new device — which is
// precisely the question an operator uses the column to answer.
func TestFirstSeenOutlivesTheRowItself(t *testing.T) {
	ctx := context.Background()
	clk := &fsClock{ms: 1_800_000_000_000}
	s := fsOpen(t, clk)

	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase, 0),
		fsAged(ddDeviceB, "relay-a", "uuid:roku:ecp:X2", fsRelayBase, 0),
	}); err != nil {
		t.Fatalf("first report: %v", err)
	}
	born := fsStored(t, s, ddDeviceA).FirstSeen

	// The device is unplugged. The next report does not mention it, and its row
	// is deleted — the behaviour a full-set replace is supposed to have.
	clk.advance(60_000)
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceB, "relay-a", "uuid:roku:ecp:X2", fsRelayBase+60_000, 0),
	}); err != nil {
		t.Fatalf("report without the device: %v", err)
	}
	rows, err := s.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	if len(rows) != 1 || rows[0].DeviceID != ddDeviceB {
		t.Fatalf("mirror = %+v, want only the still-reported device — the replace must still be a full set", rows)
	}

	// It comes back on Monday.
	clk.advance(3 * fsDay)
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase+400_000, 0),
		fsAged(ddDeviceB, "relay-a", "uuid:roku:ecp:X2", fsRelayBase+400_000, 0),
	}); err != nil {
		t.Fatalf("report after the device returns: %v", err)
	}
	if got := fsStored(t, s, ddDeviceA).FirstSeen; got != born {
		t.Errorf("first_seen = %d after the device returned, want the original %d — "+
			"a device that vanishes and comes back is not a new device", got, born)
	}
}

// TestFirstSeenFollowsTheDeviceAcrossRelays holds REL-153 on the durable side: a
// device's identity is scoped to (site, driver, native_id) and explicitly not to
// the relay reporting it, so re-homing it to a second relay serving the same
// site must not restart its history.
func TestFirstSeenFollowsTheDeviceAcrossRelays(t *testing.T) {
	ctx := context.Background()
	clk := &fsClock{ms: 1_800_000_000_000}
	s := fsOpen(t, clk)

	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase, 0),
	}); err != nil {
		t.Fatalf("relay-a report: %v", err)
	}
	born := fsStored(t, s, ddDeviceA).FirstSeen

	// relay-a is replaced by new hardware. relay-b picks the device up, and its
	// own memory of it is seconds old.
	clk.advance(fsDay)
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-b", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-b", "uuid:roku:ecp:X1", 5_000, 1_000),
	}); err != nil {
		t.Fatalf("relay-b report: %v", err)
	}

	got := fsStored(t, s, ddDeviceA)
	if got.FirstSeen != born {
		t.Errorf("first_seen = %d after re-homing, want the original %d (REL-153: identity is not scoped to a relay)", got.FirstSeen, born)
	}
	if got.RelayID != "relay-b" {
		t.Errorf("relay_id = %q, want relay-b — the record follows the device, the attribution follows the report", got.RelayID)
	}
}

// TestAnEmptyReportDoesNotEmptyTheMirror closes the whole-row instance of the
// same defect class. A relay reports from its candidate store the moment a
// connection comes up, which on a cold host is empty through no fault of the
// LAN, and a full-set replace read that as "this relay found nothing" and
// deleted every durable fact about the site.
func TestAnEmptyReportDoesNotEmptyTheMirror(t *testing.T) {
	ctx := context.Background()
	clk := &fsClock{ms: 1_800_000_000_000}
	s := fsOpen(t, clk)

	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase, 0),
	}); err != nil {
		t.Fatalf("first report: %v", err)
	}

	stored, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", nil)
	if err != nil {
		t.Fatalf("empty report: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("an empty report reported %d stored row(s), want none — it wrote nothing", len(stored))
	}
	rows, err := s.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	if len(rows) != 1 || rows[0].DeviceID != ddDeviceA {
		t.Errorf("mirror = %+v, want the device still there — silence is not evidence that the LAN is empty", rows)
	}

	// Revocation is still allowed to empty it, and is the ONLY thing that is.
	if err := s.ForgetDiscoveredDevices(ctx, "relay-a"); err != nil {
		t.Fatalf("ForgetDiscoveredDevices: %v", err)
	}
	rows, err = s.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices after forget: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("mirror after revocation = %+v, want empty", rows)
	}
}

// TestAThinReportKeepsWhatWasLearned is the rest of the class, in the shape the
// relay's own store already states it: "a scan that DID look replaces the list
// wholesale; an observation that did not look keeps what is known." Model and
// serial come only from an identification probe, ports only from a scheduled
// scan, entity state only from the poller — a restarted relay carries none of
// them, and every one used to be written straight over the durable copy.
func TestAThinReportKeepsWhatWasLearned(t *testing.T) {
	ctx := context.Background()
	clk := &fsClock{ms: 1_800_000_000_000}
	s := fsOpen(t, clk)

	rich := fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase, 0)
	rich.OpenPorts = []int{8060, 8443}
	rich.Entities = []wire.CandidateEntity{{
		Key: "main", DeviceClass: "media-player", State: "playing",
		Attributes: map[string]string{"app": "Netflix"},
	}}
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{rich}); err != nil {
		t.Fatalf("rich report: %v", err)
	}

	// The relay restarts. It re-declares the device from a passive sighting: the
	// identity is there, everything learned is not.
	clk.advance(60_000)
	thin := fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", 900, 400)
	thin.Name, thin.Model, thin.Serial, thin.Address = "", "", "", ""
	thin.OpenPorts = nil
	thin.Entities = []wire.CandidateEntity{{Key: "main", DeviceClass: "media-player"}}
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{thin}); err != nil {
		t.Fatalf("thin report: %v", err)
	}

	got := fsStored(t, s, ddDeviceA)
	if got.Name != "Lobby TV" || got.Model != "Roku Ultra" || got.Serial != "X00500ABC123" {
		t.Errorf("name/model/serial = %q/%q/%q, want them kept — a blank is 'I did not learn it', not 'it is gone'",
			got.Name, got.Model, got.Serial)
	}
	if got.Address != "192.168.50.31:8060" {
		t.Errorf("address = %q, want it kept — blanking it costs control of an adopted device", got.Address)
	}
	if len(got.OpenPorts) != 2 {
		t.Errorf("open_ports = %v, want the scan's findings kept until another scan looks", got.OpenPorts)
	}
	if len(got.Entities) != 1 || got.Entities[0].State != "playing" || got.Entities[0].Attributes["app"] != "Netflix" {
		t.Errorf("entities = %+v, want the polled state carried across a re-declaration", got.Entities)
	}

	// A report that DOES carry a value still overwrites — the case that has to
	// keep working, since a device really can move address or be renamed.
	clk.advance(60_000)
	moved := fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", 1_400, 400)
	moved.Name, moved.Address = "Lobby TV (new)", "192.168.50.44:8060"
	moved.Entities = []wire.CandidateEntity{{Key: "main", DeviceClass: "media-player", State: "idle"}}
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{moved}); err != nil {
		t.Fatalf("moved report: %v", err)
	}
	got = fsStored(t, s, ddDeviceA)
	if got.Name != "Lobby TV (new)" || got.Address != "192.168.50.44:8060" {
		t.Errorf("name/address = %q/%q, want the newly reported values — keep-when-blank must not become never-update",
			got.Name, got.Address)
	}
	if len(got.Entities) != 1 || got.Entities[0].State != "idle" {
		t.Errorf("entity state = %+v, want the newly reported one", got.Entities)
	}
}

// TestAnEntitylessReportKeepsTheFanOut is the same rule aimed at the one field
// whose loss never self-heals. AdoptDiscoveredDevice copies the mirrored entity
// list into the authored adoption row, and nothing re-derives that row
// afterwards — so an adopt clicked in the seconds after a relay restart used to
// produce a permanently entity-less device, which is a device that cannot be
// commanded at all.
func TestAnEntitylessReportKeepsTheFanOut(t *testing.T) {
	ctx := context.Background()
	clk := &fsClock{ms: 1_800_000_000_000}
	s := fsOpen(t, clk)

	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", fsRelayBase, 0),
	}); err != nil {
		t.Fatalf("first report: %v", err)
	}

	clk.advance(60_000)
	bare := fsAged(ddDeviceA, "relay-a", "uuid:roku:ecp:X1", 900, 400)
	bare.Entities = nil
	if _, err := s.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{bare}); err != nil {
		t.Fatalf("entity-less report: %v", err)
	}

	got := fsStored(t, s, ddDeviceA)
	if len(got.Entities) != 1 || got.Entities[0].Key != "main" {
		t.Errorf("entities = %+v, want the known fan-out kept — an entity-less mirror row adopts as an uncommandable device", got.Entities)
	}
}
