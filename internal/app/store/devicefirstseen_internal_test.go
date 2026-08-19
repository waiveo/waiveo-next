package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	stdlog "log"
	"path/filepath"
	"strings"
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

// stagePreLedgerRows is stagePreLedgerStore for a WHOLE SITE: one mirror row per
// entry, each carrying its own pre-ledger column value, and no ledger at all.
//
// The mixed population is the point. A run that adopts everything and a run that
// refuses everything used to produce identical output (none), and no test could
// tell them apart because nothing distinguished them; a fixture holding both at
// once is what makes the per-device account checkable rather than merely present.
func stagePreLedgerRows(t *testing.T, values map[string]int64) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")

	old, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rows := make([]DiscoveredDevice, 0, len(values))
	for id := range values {
		rows = append(rows, DiscoveredDevice{
			DeviceID: id, RelayID: "relay-a", ScopeNode: "01J8Z8D1SC0VEREDN0DE000001",
			Driver: "roku-ecp", NativeID: "uuid:roku:ecp:" + id, DeviceClass: "media-player",
			FirstSeen: 900_000, LastSeen: 900_000,
		})
	}
	if _, err := old.ReplaceDiscoveredDevices(ctx, "relay-a", rows); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if _, err := old.db.ExecContext(ctx, `DELETE FROM device_first_seen`); err != nil {
		t.Fatalf("stage the pre-ledger file: %v", err)
	}
	for id, v := range values {
		if _, err := old.db.ExecContext(ctx,
			`UPDATE discovered_devices SET first_seen = ? WHERE device_id = ?`, v, id); err != nil {
			t.Fatalf("stage the older build's value for %s: %v", id, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// captureStoreLog redirects the standard logger into buf for the rest of the
// test. The store reports through it because that is what the feeder tees into
// journald and into the console log page (the store_test package has its own
// copy for the same reason).
func captureStoreLog(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	prevOut, prevFlags := stdlog.Writer(), stdlog.Flags()
	stdlog.SetOutput(buf)
	stdlog.SetFlags(0)
	t.Cleanup(func() {
		stdlog.SetOutput(prevOut)
		stdlog.SetFlags(prevFlags)
	})
}

// The three pre-ledger populations the seed has to tell apart, spelled once.
const (
	fsRescuable  = "01J8Z8D1SC0VEREDDEV1CERESC" // a plausible instant, months before the app clock
	fsNeverSet   = "01J8Z8D1SC0VEREDDEV1CEZERO" // a relay whose clock was never set
	fsRunsAhead  = "01J8Z8D1SC0VEREDDEV1CEAHED" // a relay running ahead of this box
	fsGoodValue  = int64(1_787_098_315_675)
	fsAheadValue = fsInternalAppNow + 90*24*60*60*1000
)

// TestTheSeedSaysWhatItRescuedAndWhatItRefused is the identical-log test applied
// to the one irreversible act in the whole store open.
//
// The seed was completely silent. A run that rescued all 64 rows on box .12 and a
// run that refused all 64 produced byte-identical output — zero bytes — so an
// operator could not tell "every device kept its history" from "every device lost
// it".
//
// The assertions are: each adopted device is named with its value, with the
// `origin` the row is durably marked with and with a caveat that states the
// direction the value can be WRONG in; each refused device is named with its
// value and the reason; the summary carries both counts; and — the property the
// whole finding is about — the all-adopted log and the all-refused log are
// DIFFERENT.
//
// # Why it refuses the phrase the first version of this log used
//
// That version called an adopted value "a LOWER BOUND on this device's age, not
// an instant this side observed", flatly and with no qualification, and
// `-store-check` said the same. The file's own analysis says otherwise three
// hundred lines up: a relay whose clock ran BACKWARD but plausibly — an RTC-less
// Pi restoring fake-hwclock's last-shutdown time — wrote a value EARLIER than
// this side's true first hold, "and the rescue adopts it as an age
// OVERSTATEMENT. Nothing here can detect that." An overstated age is precisely
// NOT a lower bound. So the unqualified phrase is asserted ABSENT here: it is the
// one sentence in the account that reads like a guarantee, and it is the one the
// code cannot make.
func TestTheSeedSaysWhatItRescuedAndWhatItRefused(t *testing.T) {
	var out bytes.Buffer
	captureStoreLog(t, &out)

	path := stagePreLedgerRows(t, map[string]int64{
		fsRescuable: fsGoodValue,
		fsNeverSet:  1_000_000,
		fsRunsAhead: fsAheadValue,
	})
	out.Reset()

	upgraded, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	mixed := out.String()

	for _, want := range []string{
		fmt.Sprintf("adopted first_seen %d for device %s", fsGoodValue, fsRescuable),
		// The provenance the row is durably marked with, so the log and the ledger
		// say the same word and an operator can join one to the other.
		"origin=" + FirstSeenAdopted,
		// The half of the caveat the old phrasing left out, and the half that
		// matters when a relay's clock ran behind.
		"OVERSTATES it if that relay's clock ran behind",
		"refused first_seen 1000000 for device " + fsNeverSet,
		"below the plausibility floor",
		fmt.Sprintf("refused first_seen %d for device %s", fsAheadValue, fsRunsAhead),
		"in the future of this box's own clock",
		"1 of 3 mirrored device(s) adopted from the pre-ledger column (origin=adopted), 2 refused as implausible",
		path,
	} {
		if !strings.Contains(mixed, want) {
			t.Errorf("the seed must account for what it did; %q absent from:\n%s", want, mixed)
		}
	}
	// The guarantee the code cannot make. Asserted absent rather than merely not
	// asserted present, because "a LOWER BOUND on this device's age" is the exact
	// sentence this log used to print and the exact claim the header's
	// backward-clock case refutes.
	if strings.Contains(mixed, "LOWER BOUND on") {
		t.Errorf("the seed states an unqualified lower-bound guarantee the header says it cannot make:\n%s", mixed)
	}

	// The second boot has nothing left to ADOPT — that device is answered, and a
	// report that fires on a no-op trains people to skip it (the column pass's own
	// rule). The refusals do repeat, and that is the honest answer rather than an
	// oversight: those two devices still carry a bad stored value and still have no
	// age, exactly as an unrepairable schema difference is said out loud at every
	// boot. It self-limits, because the first report on a working clock plants a
	// value and the mirror column is overwritten with it.
	out.Reset()
	again, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second := out.String()
	if strings.Contains(second, "adopted first_seen") {
		t.Errorf("the second boot re-reported an adoption it did not make:\n%s", second)
	}
	if !strings.Contains(second, "refused first_seen 1000000 for device "+fsNeverSet) {
		t.Errorf("a device that still has no age because of a stored bad value must keep saying so:\n%s", second)
	}
}

// TestASeedWithNothingToDoIsSilent is the other half of the same rule, kept apart
// from the mixed case because it is the one that governs every ordinary boot on
// every box: when the ledger already answers for every mirrored device, the open
// says nothing whatever about it.
func TestASeedWithNothingToDoIsSilent(t *testing.T) {
	var out bytes.Buffer
	captureStoreLog(t, &out)

	path := stagePreLedgerRows(t, map[string]int64{fsRescuable: fsGoodValue})
	first, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out.Reset()
	again, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if strings.Contains(out.String(), "first_seen") || strings.Contains(out.String(), "first-seen ledger") {
		t.Fatalf("a store with nothing to seed must say nothing about the ledger:\n%s", out.String())
	}

	// And a FRESH install, which is the same silence reached from the other side.
	out.Reset()
	fresh, err := Open(filepath.Join(t.TempDir(), "app.db"), func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open a fresh store: %v", err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if strings.Contains(out.String(), "first_seen") || strings.Contains(out.String(), "first-seen ledger") {
		t.Fatalf("a fresh install must not report a seed it had nothing to do:\n%s", out.String())
	}
}

// TestARescuedRunAndARefusedRunDoNotLogTheSame is the finding stated as an
// executable claim rather than as an example: two runs whose outcomes are
// OPPOSITE must not produce the same bytes. That equality is exactly what held
// before this change, and it held for every possible input.
func TestARescuedRunAndARefusedRunDoNotLogTheSame(t *testing.T) {
	logOf := func(t *testing.T, value int64) string {
		t.Helper()
		var out bytes.Buffer
		captureStoreLog(t, &out)
		path := stagePreLedgerRows(t, map[string]int64{
			fsRescuable: value, fsNeverSet: value, fsRunsAhead: value,
		})
		out.Reset()
		s, err := Open(path, func() int64 { return fsInternalAppNow })
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return out.String()
	}

	rescued := logOf(t, fsGoodValue)
	refused := logOf(t, 1_000_000)
	if rescued == refused {
		t.Fatalf("a run that rescued every row and a run that refused every row logged identically; "+
			"that is the defect, not a detail:\n%s", rescued)
	}
	if !strings.Contains(rescued, "3 of 3 mirrored device(s) adopted") {
		t.Errorf("the all-rescued run must count what it rescued:\n%s", rescued)
	}
	if !strings.Contains(refused, "0 of 3 mirrored device(s) adopted from the pre-ledger column (origin=adopted), 3 refused") {
		t.Errorf("the all-refused run must count what it refused:\n%s", refused)
	}
}

// TestRetireClearsBothCopiesAndTheNextOpenLeavesItRetired is the correction path,
// and specifically the trap a naive version falls into.
//
// discovered_devices.first_seen is the ledger's PROJECTION and holds the same
// value. Deleting only the ledger row leaves it there — and the very next
// store.Open re-runs the seed, whose plan now MATCHES this device precisely
// because the ledger row is gone, and re-adopts the value that was just retired.
// A repair that the next reboot silently undoes is worse than none: it costs an
// operator the evidence that the value was ever wrong.
func TestRetireClearsBothCopiesAndTheNextOpenLeavesItRetired(t *testing.T) {
	ctx := context.Background()
	path := stagePreLedgerStore(t, fsGoodValue)

	s, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if v, ok := ledgerValue(t, s); !ok || v != fsGoodValue {
		t.Fatalf("fixture: ledger holds %d (present=%v), want the rescued %d", v, ok, fsGoodValue)
	}

	retired, err := s.RetireDeviceFirstSeen(ctx, preLedgerDeviceID)
	if err != nil {
		t.Fatalf("RetireDeviceFirstSeen: %v", err)
	}
	if !retired {
		t.Errorf("retiring a device that had a stored first_seen reported nothing to retire")
	}
	if v, ok := ledgerValue(t, s); ok {
		t.Errorf("the ledger still holds %d after a retire", v)
	}
	rows, err := readDiscoveredDevices(ctx, s.db, preLedgerDeviceID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read the mirror back: %v (rows=%d)", err, len(rows))
	}
	if rows[0].FirstSeen != 0 {
		t.Errorf("the mirror projection still holds %d after a retire — the ledger's own seed re-imports from "+
			"exactly this column, so leaving it is a repair the next reboot undoes", rows[0].FirstSeen)
	}
	// last_seen is a different fact answered by a different rule, and a retire that
	// blanked it would report a device that reported a minute ago as never heard from.
	if rows[0].LastSeen == 0 {
		t.Errorf("retiring first_seen also cleared last_seen")
	}
	// Retiring again is a benign no-op, the contract UnignoreDevice keeps.
	if again, err := s.RetireDeviceFirstSeen(ctx, preLedgerDeviceID); err != nil || again {
		t.Errorf("second retire = (%v, %v), want (false, nil)", again, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// THE TRAP: reboot. The seed runs again, and the retired value must not come back.
	reopened, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("reopen after a retire: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if v, ok := ledgerValue(t, reopened); ok {
		t.Fatalf("the next open re-adopted the retired value %d from the pre-ledger column; a retire that clears "+
			"only the ledger row is undone by the seed it re-enables", v)
	}

	// And the ordinary next report repairs the device rather than leaving it blank
	// forever: a retire makes a value non-permanent, it does not delete the answer.
	if got := reportOnce(t, reopened, 700).FirstSeen; got != fsInternalAppNow {
		t.Errorf("first_seen after the report following a retire = %d, want the app's own %d", got, fsInternalAppNow)
	}
}

// TestRetiringADeviceThatWasNeverPlantedIsANoOp pins the idempotence and the
// empty-id refusal, so the verb behind an api/1 DELETE cannot report a repair it
// did not make.
func TestRetiringADeviceThatWasNeverPlantedIsANoOp(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "app.db"), func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if retired, err := s.RetireDeviceFirstSeen(ctx, preLedgerDeviceID); err != nil || retired {
		t.Errorf("retiring an unknown device = (%v, %v), want (false, nil)", retired, err)
	}
	if _, err := s.RetireDeviceFirstSeen(ctx, ""); err == nil {
		t.Errorf("an empty device_id must be refused rather than retiring nothing quietly")
	}
}

// TestTheStoreCheckCensusSeesThePendingSeed is the read-only half: what
// `-store-check` prints has to be computed by the same planner the boot acts on,
// or the operator's pre-restart reading and the restart disagree.
func TestTheStoreCheckCensusSeesThePendingSeed(t *testing.T) {
	ctx := context.Background()
	path := stagePreLedgerRows(t, map[string]int64{
		fsRescuable: fsGoodValue,
		fsNeverSet:  1_000_000,
	})

	ro, err := OpenReadOnly(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	ledger, err := ro.InspectDeviceFirstSeen(ctx)
	if err != nil {
		t.Fatalf("InspectDeviceFirstSeen: %v", err)
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !ledger.Present || len(ledger.Rows) != 0 || ledger.Mirrored != 2 || ledger.Answered != 0 {
		t.Errorf("census = %+v, want the table present, empty, over 2 mirrored rows", ledger)
	}
	if len(ledger.Pending.Adopted) != 1 || ledger.Pending.Adopted[0].DeviceID != fsRescuable {
		t.Errorf("pending adoptions = %+v, want just %s", ledger.Pending.Adopted, fsRescuable)
	}
	if len(ledger.Pending.Refused) != 1 || ledger.Pending.Refused[0].DeviceID != fsNeverSet {
		t.Errorf("pending refusals = %+v, want just %s", ledger.Pending.Refused, fsNeverSet)
	}

	// And the boot does exactly what the check said it would.
	opened, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	after, err := opened.InspectDeviceFirstSeen(ctx)
	if err != nil {
		t.Fatalf("InspectDeviceFirstSeen after the boot: %v", err)
	}
	if len(after.Rows) != 1 || after.Answered != 1 || len(after.Pending.Adopted) != 0 {
		t.Errorf("after the boot the census reads %+v; the check promised one adoption and no more", after)
	}
	// The refused row stays pending forever until a report plants one, which is
	// the honest answer and must keep being reported rather than going quiet.
	if len(after.Pending.Refused) != 1 {
		t.Errorf("a refused value must keep being reported as refused, not silently forgotten: %+v", after.Pending)
	}
}

// TestARetiredDeviceIsNotReportedAsARefusedOne guards the seam between the two
// halves of this change, which is where a plausible implementation goes wrong.
//
// A retire leaves the mirror projection at ZERO. If the planner treated zero as a
// value to judge, every retired device would be reported "refused: below the
// plausibility floor" in every boot log and every `-store-check`, forever — a
// permanent complaint about a state an operator deliberately created. Zero is the
// ABSENT answer in that column, not a bad one.
func TestARetiredDeviceIsNotReportedAsARefusedOne(t *testing.T) {
	ctx := context.Background()
	path := stagePreLedgerStore(t, fsGoodValue)
	s, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.RetireDeviceFirstSeen(ctx, preLedgerDeviceID); err != nil {
		t.Fatalf("RetireDeviceFirstSeen: %v", err)
	}
	ledger, err := s.InspectDeviceFirstSeen(ctx)
	if err != nil {
		t.Fatalf("InspectDeviceFirstSeen: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(ledger.Pending.Refused) != 0 || len(ledger.Pending.Adopted) != 0 {
		t.Fatalf("a retired device is pending nothing; got %+v", ledger.Pending)
	}

	var out bytes.Buffer
	captureStoreLog(t, &out)
	reopened, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if strings.Contains(out.String(), "refused first_seen") {
		t.Fatalf("the boot complained about a device an operator deliberately retired:\n%s", out.String())
	}
}

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
//
// It also pins the OTHER half of that seam, which is not the same claim and used
// to be silently violated by it: the surviving column value must not become the
// device's AGE in the meantime. The row this returns is what the caller projects
// onto the read model and what `GET /devices` then serves, so carrying the
// pre-ledger number in it would put a value this side has explicitly declined to
// stand behind in front of an operator — while the same boot's log said the
// device had no age at all. Never-wipe governs the FILE; it does not license
// serving what was kept.
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
	stored := reportOnce(t, broken, 500)
	if stored.MirroredFirstSeen != olderBuildsValue {
		t.Fatalf("the mirror column after a report on a broken clock = %d, want the pre-ledger %d left alone — "+
			"the column is the only copy of that history until the backfill can run",
			stored.MirroredFirstSeen, olderBuildsValue)
	}
	if stored.FirstSeen != 0 {
		t.Fatalf("the row served to the read model carries first_seen = %d, want 0: the ledger holds nothing for "+
			"this device, so the honest answer is no age — serving the kept column value is how a number this "+
			"side refused reaches the console", stored.FirstSeen)
	}
	if stored.FirstSeenOrigin != "" {
		t.Fatalf("a device with no age reports origin %q, want empty", stored.FirstSeenOrigin)
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

// TestTheLedgerRecordsWhereEachAgeCameFrom is the provenance column, which is
// what finally lets every surface downstream be honest about a value.
//
// Before it, the ledger held (device_id, first_seen) and this file's own backfill
// docstring stated the consequence: "once these rows are in it NOTHING
// distinguishes a backfilled lower bound from a planted instant". That was
// written as the reason to log every adoption, and it is really the description
// of a missing column — the boot log is truncated at twenty lines and scrolls
// away, so on box .12's 64 adoptions it named 20 devices and lost 44, while the
// console went on drawing every row as an exact age because the served value
// carried nothing that would let it do otherwise.
//
// Three origins, and all three have to be readable:
//
//   - `adopted` — what the backfill copied out of the pre-ledger column;
//   - `planted` — what a report stamped from this deployment's own clock;
//   - `unrecorded` — a row written by the build that had no origin column at
//     all, which is what every one of box .12's 64 rows is. It reads as its own
//     answer rather than defaulting to either of the others, because no code can
//     prove which it was and a default would assert something untrue about a row
//     nobody can check.
func TestTheLedgerRecordsWhereEachAgeCameFrom(t *testing.T) {
	ctx := context.Background()

	// --- adopted: rescued from the older build's column ---
	adoptedPath := stagePreLedgerStore(t, fsGoodValue)
	upgraded, err := Open(adoptedPath, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	led, err := upgraded.InspectDeviceFirstSeen(ctx)
	if err != nil {
		t.Fatalf("InspectDeviceFirstSeen: %v", err)
	}
	if len(led.Rows) != 1 || led.Rows[0].Origin != FirstSeenAdopted || led.Rows[0].FirstSeen != fsGoodValue {
		t.Fatalf("the rescued row reads %+v, want one row with origin %q and value %d",
			led.Rows, FirstSeenAdopted, fsGoodValue)
	}
	if led.Unverified != 1 {
		t.Errorf("Unverified = %d, want 1 — an adopted value is not an instant this deployment observed", led.Unverified)
	}
	// And it reaches a READER, which is the only reason the column exists: the
	// row the caller projects onto the read model and the API serves.
	if got := reportOnce(t, upgraded, 500); got.FirstSeenOrigin != FirstSeenAdopted {
		t.Errorf("the mirrored row reports origin %q, want %q — a value served without its provenance is "+
			"indistinguishable from one this deployment watched", got.FirstSeenOrigin, FirstSeenAdopted)
	}

	// --- planted: stamped here, from this deployment's own clock ---
	fresh, err := Open(filepath.Join(t.TempDir(), "fresh.db"), func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open a fresh store: %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	planted := reportOnce(t, fresh, 500)
	if planted.FirstSeenOrigin != FirstSeenPlanted || planted.FirstSeen != fsInternalAppNow {
		t.Errorf("a fresh plant reports %d/%q, want %d/%q",
			planted.FirstSeen, planted.FirstSeenOrigin, fsInternalAppNow, FirstSeenPlanted)
	}
	if !FirstSeenIsObserved(planted.FirstSeenOrigin) {
		t.Errorf("a planted instant must read as OBSERVED; %q does not", planted.FirstSeenOrigin)
	}

	// --- unrecorded: box .12's exact shape — a ledger this build's column
	// arrived after. The row is left alone, and reads as what it is.
	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := Open(legacyPath, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := legacy.ReplaceDiscoveredDevices(ctx, "relay-a", []DiscoveredDevice{{
		DeviceID: preLedgerDeviceID, RelayID: "relay-a", ScopeNode: "01J8Z8D1SC0VEREDN0DE000001",
		Driver: "roku-ecp", NativeID: "uuid:roku:ecp:X1", DeviceClass: "media-player",
		LastSeen: 900_000,
	}}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	// Forge the file the previous build wrote: the ledger row is there, the
	// column that says where it came from is not.
	if _, err := legacy.db.ExecContext(ctx, `ALTER TABLE device_first_seen DROP COLUMN origin`); err != nil {
		t.Fatalf("forge the pre-provenance ledger: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(legacyPath, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("a store whose ledger predates the origin column must OPEN — the column carries a constant "+
			"DEFAULT precisely so SQLite can retrofit it: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	legacyLedger, err := reopened.InspectDeviceFirstSeen(ctx)
	if err != nil {
		t.Fatalf("InspectDeviceFirstSeen: %v", err)
	}
	if len(legacyLedger.Rows) != 1 {
		t.Fatalf("the retrofit lost the row it was supposed to describe: %+v", legacyLedger.Rows)
	}
	if legacyLedger.Rows[0].FirstSeen != fsInternalAppNow {
		t.Errorf("the retrofit changed the stored instant to %d, want the original %d left alone",
			legacyLedger.Rows[0].FirstSeen, fsInternalAppNow)
	}
	if legacyLedger.Rows[0].Origin != FirstSeenUnrecorded {
		t.Errorf("a row written before provenance was kept reads origin %q, want %q — defaulting it to %q would "+
			"claim this deployment watched something it cannot show it watched, and defaulting it to %q would "+
			"libel a value it may have planted honestly",
			legacyLedger.Rows[0].Origin, FirstSeenUnrecorded, FirstSeenPlanted, FirstSeenAdopted)
	}
	if FirstSeenIsObserved(legacyLedger.Rows[0].Origin) {
		t.Errorf("an unrecorded origin must NOT read as observed: the caution is the same as for an adopted one, " +
			"because it cannot be shown not to be one")
	}
	if legacyLedger.Unverified != 1 {
		t.Errorf("Unverified = %d, want 1", legacyLedger.Unverified)
	}
}

// TestTheSeedsTruncationPointsAtSomethingThatExists exercises the per-device
// account at box .12's actual scale, which no other case in this file reaches.
//
// The boot log caps its per-device lines at twenty. That cap is fine; what was
// not fine is what the cap's own line said. It read "… and N more adopted;
// `waiveo-feeder -store-check` lists the ledger in full", and `-store-check`
// listed no ledger rows at all — it printed three counts and the PENDING seed,
// and the pending seed is empty precisely BECAUSE the boot succeeded. With no
// provenance column the same docstring called that truncated log "the only record
// that a given device's age came from the pre-ledger column at all, and the only
// input an operator has for deciding which row to retire". So on .12's 64
// adoptions, 20 devices were named and 44 were recorded nowhere, by a sentence
// that sent the operator to a command that would tell them there was nothing to
// list.
//
// Both halves are asserted here, and each is worthless without the other: the cap
// still fires (nobody wants a thousand lines at boot), AND every adoption is
// durably marked in the ledger with the origin the referral promises, so the
// forty-four are recoverable rather than lost.
func TestTheSeedsTruncationPointsAtSomethingThatExists(t *testing.T) {
	const staged = 25 // > the log's cap of 20, as .12's 64 is

	var out bytes.Buffer
	captureStoreLog(t, &out)

	values := make(map[string]int64, staged)
	for i := range staged {
		values[fmt.Sprintf("01J8Z8D1SC0TRUNCATED%06d", i)] = fsGoodValue
	}
	path := stagePreLedgerRows(t, values)
	out.Reset()

	upgraded, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	log := out.String()

	if named := strings.Count(log, "adopted first_seen "); named != 20 {
		t.Errorf("the boot log named %d adoption(s), want the cap of 20 — a box with a thousand devices must not "+
			"scroll the rest of startup away", named)
	}
	if !strings.Contains(log, "and 5 more adopted") {
		t.Fatalf("the truncation must say how many it did NOT name:\n%s", log)
	}

	// The sentence the cap prints, taken literally and followed.
	led, err := upgraded.InspectDeviceFirstSeen(context.Background())
	if err != nil {
		t.Fatalf("InspectDeviceFirstSeen: %v", err)
	}
	if len(led.Rows) != staged {
		t.Fatalf("`-store-check` lists %d ledger row(s), want all %d: the boot log's truncation sends the operator "+
			"here, and it used to answer with three counts and nothing else", len(led.Rows), staged)
	}
	if led.Unverified != staged {
		t.Errorf("Unverified = %d, want %d", led.Unverified, staged)
	}
	// Not merely listed — listed with the fact the log was carrying. Every one of
	// the 5 the log never named is individually recoverable.
	unnamed := 0
	for _, r := range led.Rows {
		if r.Origin != FirstSeenAdopted {
			t.Fatalf("ledger row %s reads origin %q, want %q", r.DeviceID, r.Origin, FirstSeenAdopted)
		}
		if !strings.Contains(log, "adopted first_seen "+fmt.Sprint(r.FirstSeen)+" for device "+r.DeviceID) {
			unnamed++
		}
	}
	if unnamed != 5 {
		t.Errorf("%d adoption(s) went unnamed by the log, want exactly the 5 the truncation accounts for", unnamed)
	}
}

// TestInspectDeviceFirstSeenReadsALedgerWhoseOriginColumnIsPending is #199 at the
// store level: the table arrived in one build and `origin` in the next, so there
// is a real file shape — the exact one an operator inspects immediately BEFORE
// that upgrade — where the table is there and the column is not.
//
// listDeviceFirstSeen selected `origin` unconditionally, so the query failed with
// "no such column: origin" and this call returned a ZERO ledger: Present,
// Mirrored, Answered, Unverified and the whole pending seed plan discarded
// together with the one query that could not run. The report that consumes it
// printed a single error line and its caller exited 0.
func TestInspectDeviceFirstSeenReadsALedgerWhoseOriginColumnIsPending(t *testing.T) {
	ctx := context.Background()
	path := stagePreLedgerStore(t, fsGoodValue)

	// Boot once so the ledger exists and holds the adopted row...
	s, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// ...then take the column away, which is the file an unupgraded box has.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE device_first_seen DROP COLUMN origin`); err != nil {
		t.Fatalf("forge the pre-origin shape: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	ro, err := OpenReadOnly(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	ledger, err := ro.InspectDeviceFirstSeen(ctx)
	if err != nil {
		t.Fatalf("InspectDeviceFirstSeen went blind on the shape it exists to inspect: %v", err)
	}
	if !ledger.Present {
		t.Fatal("the ledger table is on this store and was reported absent")
	}
	if len(ledger.Rows) != 1 {
		t.Fatalf("rows = %+v, want the one adopted row", ledger.Rows)
	}
	if ledger.Rows[0].DeviceID != preLedgerDeviceID {
		t.Fatalf("row names %q, want %q", ledger.Rows[0].DeviceID, preLedgerDeviceID)
	}
	// A store with nowhere to record provenance reads as `unrecorded`, which is
	// what a blank means everywhere else in this file — not as an observation.
	if ledger.Rows[0].Origin != FirstSeenUnrecorded {
		t.Fatalf("origin = %q, want %q for a store whose origin column does not exist yet",
			ledger.Rows[0].Origin, FirstSeenUnrecorded)
	}
	if ledger.Unverified != 1 {
		t.Fatalf("unverified = %d, want 1: a value with no recorded provenance may not be rendered as an instant",
			ledger.Unverified)
	}
	if ledger.Mirrored != 1 || ledger.Answered != 1 {
		t.Fatalf("census = %d mirrored / %d answered, want 1/1 — the counts used to be discarded with the listing",
			ledger.Mirrored, ledger.Answered)
	}
}

// TestInspectDeviceFirstSeenReturnsWhatItReadBesideAnError is the posture the
// early returns cost: a census that fails one query answers with the rest of the
// census AND the error, so a caller can print a partial answer rather than
// silence. The fixture breaks only the row SCAN — SQLite keeps a non-numeric
// string in an INTEGER-affinity column verbatim — so the counts and the seed plan
// are unaffected and the listing alone fails.
func TestInspectDeviceFirstSeenReturnsWhatItReadBesideAnError(t *testing.T) {
	ctx := context.Background()
	path := stagePreLedgerStore(t, fsGoodValue)
	s, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE device_first_seen SET first_seen = 'not-an-instant'`); err != nil {
		t.Fatalf("plant the unscannable value: %v", err)
	}
	ledger, err := s.InspectDeviceFirstSeen(ctx)
	if err == nil {
		t.Fatal("a ledger row that cannot be scanned was reported as readable")
	}
	if !ledger.Present {
		t.Fatalf("the census was discarded with the listing error: %+v", ledger)
	}
	if ledger.Mirrored != 1 {
		t.Fatalf("mirrored = %d, want 1 — the counts were read before the listing failed", ledger.Mirrored)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestInspectDeviceFirstSeenKeepsTheListingWhenTheSEEDPLANFails is the direction
// the "returns what it did read" posture did NOT cover.
//
// Reordering the queries so the listing ran LAST did not stop the census losing
// half of itself; it only chose a different half. On a store whose pre-ledger
// `first_seen` column holds a value SQLite kept verbatim in an INTEGER-affinity
// column — an ordinary pre-upgrade file, and the reason this call exists — the
// SEED PLAN fails, and the early return then threw away the row listing: the half
// the boot log's truncation referral names ("`waiveo-feeder -store-check <store>`
// lists every ledger row and its origin"). Three readable rows came back as zero.
//
// The fixture breaks the plan and leaves the ledger perfectly readable, which is
// what makes the assertion sharp: every row must still come back.
func TestInspectDeviceFirstSeenKeepsTheListingWhenTheSEEDPLANFails(t *testing.T) {
	ctx := context.Background()
	path := stagePreLedgerStore(t, fsGoodValue)
	s, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// A second mirror row whose PRE-LEDGER column cannot be scanned. The ledger
	// itself is untouched.
	const brokenDevice = "01J9PLANBR3AK1NGD3V1CE001"
	if _, err := s.db.ExecContext(ctx, `INSERT INTO discovered_devices
		  (device_id, relay_id, scope_node, driver, native_id, device_class,
		   name, address, model, serial, first_seen, last_seen, entities, open_ports, relay_last_seen)
		VALUES (?, 'relay-a', 'site', 'roku-ecp', 'uuid:broken', 'media-player',
		        'Broken', '192.168.50.99', '', '', 'not-an-instant', 1787098400000, '[]', '[]', 0)`,
		brokenDevice); err != nil {
		t.Fatalf("plant the unscannable pre-ledger value: %v", err)
	}

	ledger, err := s.InspectDeviceFirstSeen(ctx)
	if err == nil {
		t.Fatal("a seed plan that could not be produced was reported as produced")
	}
	if ledger.PendingKnown {
		t.Fatal("a seed plan that failed is marked as known; an empty plan would then read as 'the boot seeds nothing'")
	}
	if !ledger.RowsComplete {
		t.Fatalf("the listing is marked incomplete, but only the PLAN failed: %v", err)
	}
	if len(ledger.Rows) != 1 || ledger.Rows[0].DeviceID != preLedgerDeviceID {
		t.Fatalf("rows = %+v, want the one ledger row — a plan failure must not discard the listing", ledger.Rows)
	}
	if !ledger.CountsKnown || ledger.Mirrored != 2 || ledger.Answered != 1 {
		t.Fatalf("census = %d mirrored / %d answered (known=%v), want 1 of 2",
			ledger.Mirrored, ledger.Answered, ledger.CountsKnown)
	}
}

// TestListDeviceFirstSeenKeepsTheRowsItAlreadyRead: the scan loop returned
// `nil, err` on the first row it could not read, throwing away every row already
// scanned. A two-hundred-row ledger whose LAST row holds a pre-ledger string
// listed none of them — the same "lose everything to one bad row" shape as the
// caller, one level down, and enough to defeat the caller's fix on its own.
func TestListDeviceFirstSeenKeepsTheRowsItAlreadyRead(t *testing.T) {
	ctx := context.Background()
	path := stagePreLedgerStore(t, fsGoodValue)
	s, err := Open(path, func() int64 { return fsInternalAppNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Two readable rows sorting BEFORE the unreadable one (the listing is ordered
	// by device_id), so a loop that aborts on the bad row has already scanned them.
	for _, id := range []string{"01J9AAAAREADABLER0W000001", "01J9AAAAREADABLER0W000002"} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO device_first_seen (device_id, first_seen, origin) VALUES (?, 1787098315675, 'planted')`,
			id); err != nil {
			t.Fatalf("plant %s: %v", id, err)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO device_first_seen (device_id, first_seen, origin) VALUES ('01J9ZZZZUNSCANNABLE00001', 'not-an-instant', 'planted')`); err != nil {
		t.Fatalf("plant the unscannable row: %v", err)
	}

	rows, err := listDeviceFirstSeen(ctx, s.db)
	if err == nil {
		t.Fatal("an unreadable row was reported as read")
	}
	if !strings.Contains(err.Error(), "1 row(s) could not be read") {
		t.Fatalf("the error does not say how many rows were lost: %v", err)
	}
	// The two readable rows plus the pre-ledger fixture's own row: everything the
	// listing COULD read.
	if len(rows) != 3 {
		t.Fatalf("listed %d row(s), want 3 — one unreadable row must cost that row and nothing else: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.DeviceID == "01J9ZZZZUNSCANNABLE00001" {
			t.Fatalf("the unreadable row was listed anyway: %+v", r)
		}
	}
}
