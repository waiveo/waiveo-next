package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/events"

	_ "modernc.org/sqlite"
)

// This file holds the three defects `-store-check` was found to have by being
// USED — a tool that answers confidently about the wrong thing is worse than no
// tool, because the operator's next action is "proceed" — plus the exit-code
// contract that makes the answers legible to a script.
//
// Each test here fails against the code as it was:
//
//   - #195, the path: resolveStoreCheckPath did not exist. `-store-check` was a
//     bare flag.Bool and `flag.Args()` appeared nowhere in the binary, so a path
//     an operator typed was discarded in silence and the report named a
//     cwd-relative default easy to misread as the store they meant.
//   - #199, the ledger: listDeviceFirstSeen selected `origin` unconditionally, so
//     on the one shape this check exists to inspect — the table present, the
//     column pending — the whole census was traded for one error line and the
//     process exited 0.
//   - the exit codes: 0 meant "everything except three specific throws",
//     including every I-could-not-answer and the wrong-file-entirely case.

// rawExec runs statements against the store file directly, forging the shapes a
// real box arrives in.
func rawExec(t *testing.T, dsn string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("raw exec %q: %v", s, err)
		}
	}
}

// --- #195: the path ---------------------------------------------------------

// TestStoreCheckPathComesFromTheCommandLineFirst pins the resolution order and,
// with it, the defect: the path an operator NAMES wins, and the two fallbacks
// that existed before it still work. A fallback removed in the same change that
// adds its replacement is how a box's unit file stops being honoured.
func TestStoreCheckPathComesFromTheCommandLineFirst(t *testing.T) {
	const env = "/var/lib/waiveo/from-env.db"
	named := filepath.Join(t.TempDir(), "named.db")

	got, err := resolveStoreCheckPath([]string{named}, env)
	if err != nil {
		t.Fatalf("resolveStoreCheckPath with an operand: %v", err)
	}
	if got != named {
		t.Fatalf("the operand was not honoured: got %q, want %q — a path this check discards is the whole defect", got, named)
	}

	got, err = resolveStoreCheckPath(nil, env)
	if err != nil {
		t.Fatalf("resolveStoreCheckPath with no operand: %v", err)
	}
	if got != env {
		t.Fatalf("without an operand the env/default store must still be used: got %q, want %q", got, env)
	}
}

// TestStoreCheckResolvesARelativePathToAnAbsoluteOne: the report's subject must
// be an absolute path, from wherever it came.
//
// This is the half of #195 that would have prevented the incident on its own.
// The default store path is RELATIVE (`.dev/feeder-store.db`), the systemd unit
// pins it only through WorkingDirectory, and `sudo` does not change cwd — so the
// documented command, run from the directory an ssh session lands in, reported on
// a different file while printing a path an agent read as the box's store.
func TestStoreCheckResolvesARelativePathToAnAbsoluteOne(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	got, err := resolveStoreCheckPath(nil, ".dev/feeder-store.db")
	if err != nil {
		t.Fatalf("resolveStoreCheckPath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("the resolved store path %q is relative; the report would name a file whose identity depends on cwd", got)
	}
	if !strings.HasSuffix(got, filepath.Join(".dev", "feeder-store.db")) {
		t.Fatalf("resolved path %q does not end in the default store path", got)
	}
}

// TestStoreCheckRefusesASecondStorePath: silently discarding an argument is the
// defect, so two paths are refused rather than one of them being picked.
func TestStoreCheckRefusesASecondStorePath(t *testing.T) {
	_, err := resolveStoreCheckPath([]string{"/one.db", "/two.db"}, "/env.db")
	if err == nil {
		t.Fatal("two store paths were accepted; the operator cannot tell which one the report is about")
	}
	for _, want := range []string{"/one.db", "/two.db"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal %v does not name the operands it refused", err)
		}
	}
	if _, err := resolveStoreCheckPath([]string{"   "}, "/env.db"); err == nil {
		t.Fatal("an empty store path was accepted")
	}
}

// TestFeederRefusesAnOperandBeforeItsFlag is the word-order slip, and it is the
// worst argv shape the binary had.
//
// Go's flag parser stops at the first non-flag argument, so `waiveo-feeder
// <path> -store-check` never set the flag: the process fell through into the
// BOOT and opened that store for real — migrating columns, stamping the epoch,
// seeding the first-seen ledger irreversibly and binding the port. The one
// command designed never to write became the one that writes the most.
func TestFeederRefusesAnOperandBeforeItsFlag(t *testing.T) {
	var out bytes.Buffer
	if refused := refuseStrayOperands(nil, &out); refused {
		t.Fatal("an ordinary boot with no operands was refused")
	}
	if out.Len() != 0 {
		t.Fatalf("a clean boot printed %q", out.String())
	}

	out.Reset()
	if refused := refuseStrayOperands([]string{"/opt/waiveo-next/.dev/feeder-store.db", "-store-check"}, &out); !refused {
		t.Fatal("`waiveo-feeder <path> -store-check` was allowed to proceed — that boots the feeder against the store " +
			"the operator was trying to inspect read-only")
	}
	got := out.String()
	for _, want := range []string{
		"/opt/waiveo-next/.dev/feeder-store.db",
		"BOOTED THE FEEDER",
		"waiveo-feeder -store-check <store-path>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the refusal does not explain the slip (%q absent):\n%s", want, got)
		}
	}
}

// TestStoreCheckNamesTheStoreItRead: every report begins by naming the ABSOLUTE
// path it read and the build that read it. Both were missing, and both are what
// an operator needs to know the answer is about the file they meant.
//
// It is driven through a RELATIVE path on purpose, because that is the shape the
// incident had: the report printed `.dev/feeder-store.db` — the cwd-relative
// default — and an agent read it as the box's store at
// /opt/waiveo-next/.dev/feeder-store.db.
func TestStoreCheckNamesTheStoreItRead(t *testing.T) {
	dsn := seedStoreFileForCheck(t)
	base := filepath.Base(dsn)
	t.Chdir(filepath.Dir(dsn))

	var out bytes.Buffer
	if code := reportStoreIDs(base, &out); code != storeCheckClean {
		t.Fatalf("reportStoreIDs exit code = %d, want %d\n%s", code, storeCheckClean, out.String())
	}
	first, _, _ := strings.Cut(out.String(), "\n")
	if strings.Contains(first, "reading "+base) {
		t.Fatalf("the report names the store by the relative path it was handed: %q — which file that is depends on "+
			"the cwd, and misreading it is exactly how a report about the wrong store gets believed", first)
	}
	if !strings.Contains(first, string(filepath.Separator)+base) {
		t.Fatalf("the report's first line does not name the store it read as an absolute path: %q", first)
	}
	if !strings.Contains(first, buildVersion) {
		t.Fatalf("the report's first line does not name the build that produced it: %q — every sentence in the "+
			"report is about \"this build's schema\"", first)
	}
	// The sections that name a path must name the same resolved one.
	if strings.Contains(out.String(), "\n"+base+":") {
		t.Fatalf("a section names the store by its relative path:\n%s", out.String())
	}
}

// --- #199: the ledger -------------------------------------------------------

// TestStoreCheckListsTheLedgerWithTheOriginColumnPending is #199.
//
// The shape is the one an operator inspects immediately BEFORE the upgrade that
// adds `origin`: the ledger table is there, the column is not. The listing query
// named `origin` unconditionally, InspectDeviceFirstSeen discarded the whole
// value on that error, and the report printed:
//
//	the device first-seen ledger could not be read: store: list the device
//	first-seen ledger: SQL logic error: no such column: origin (1)
//
// ...and exited 0. Everything the section exists to say — the row census, how
// much of the mirror is answered, and the irreversible seed the next boot would
// perform — was lost with it, on the exact store the boot log's truncation
// referral points at.
func TestStoreCheckListsTheLedgerWithTheOriginColumnPending(t *testing.T) {
	dsn := seedStoreFileForCheck(t)

	const deviceID = "01J9DEV1CE0N0R1G1NPEND1NG0"
	rawExec(t, dsn,
		`INSERT INTO discovered_devices
		   (device_id, relay_id, scope_node, driver, native_id, device_class,
		    name, address, model, serial, first_seen, last_seen, entities, open_ports, relay_last_seen)
		 VALUES ('`+deviceID+`', 'relay-a', 'site', 'roku-ecp', 'uuid:roku:ecp:X0050PEND1NG', 'media-player',
		         'Lobby Roku', '192.168.50.31', '', '', 1787098315675, 1787098400000, '[]', '[]', 0)`,
		`INSERT INTO device_first_seen (device_id, first_seen, origin) VALUES ('`+deviceID+`', 1787098315675, 'adopted')`,
		// The pre-upgrade file shape: the table, without the column.
		`ALTER TABLE device_first_seen DROP COLUMN origin`,
	)
	before := storeFingerprint(t, dsn)

	var out bytes.Buffer
	code := reportStoreIDs(dsn, &out)
	got := out.String()

	if strings.Contains(got, "could not be read") {
		t.Fatalf("the ledger section went blind on the shape it exists to inspect:\n%s", got)
	}
	for _, want := range []string{
		// The census, which used to be discarded together with the listing.
		"device first-seen ledger: 1 row(s); 1 of 1 mirrored device(s) have a stored first_seen.",
		// The row itself, with the honest origin for a store that has nowhere to
		// record one.
		deviceID + "  first_seen=1787098315675  origin=" + store.FirstSeenUnrecorded,
		// And the pending column is still named by the schema section, so the
		// operator is told both halves.
		"device_first_seen.origin",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the pre-upgrade ledger report is missing %q:\n%s", want, got)
		}
	}
	// A pending column is work the next boot performs: 2, not 0 and not 1.
	if code != storeCheckPending {
		t.Fatalf("reportStoreIDs exit code = %d, want %d (a pending column is work the boot does)\n%s",
			code, storeCheckPending, got)
	}
	if after := storeFingerprint(t, dsn); after != before {
		t.Fatalf("the check changed the store.\n  before: %s\n   after: %s", before, after)
	}
	t.Logf("column-pending -store-check output:\n%s", got)
}

// --- the exit-code contract -------------------------------------------------

// TestStoreCheckCannotAnswerDoesNotExitZero is the contract's whole point, in
// isolation: ONE section fails to produce its answer on an otherwise conforming
// store, and the check must not report a pass.
//
// The fixture makes the ledger's row scan fail without touching the schema —
// SQLite stores a non-numeric string in an INTEGER-affinity column verbatim, so
// the shape planner finds nothing wrong and the row simply cannot be read as the
// integer it is declared to be. Before the contract, this printed one error line
// and exited 0: a script gating a deploy on the exit code was told to proceed by
// a check whose report was missing.
func TestStoreCheckCannotAnswerDoesNotExitZero(t *testing.T) {
	dsn := seedStoreFileForCheck(t)
	rawExec(t, dsn,
		`INSERT INTO device_first_seen (device_id, first_seen, origin) VALUES ('01J9BR0KENR0WFIRSTSEEN000', 'not-an-instant', 'planted')`)

	var out bytes.Buffer
	code := reportStoreIDs(dsn, &out)
	got := out.String()
	if code != storeCheckIncomplete {
		t.Fatalf("reportStoreIDs exit code = %d, want %d — a section that could not answer must not exit 0\n%s",
			code, storeCheckIncomplete, got)
	}
	for _, want := range []string{
		"the device first-seen ledger could not be read in full",
		// The census line must say it does not know, rather than print a count.
		"its rows could not be listed at all, so how many it holds is UNKNOWN",
		// The sections BELOW it still ran: the failure degrades to a named gap,
		// never to a suppressed report.
		"durable event log:",
		"VERDICT: INCOMPLETE",
		"do not read this as a pass",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ledger: 0 row(s)") {
		t.Fatalf("a ledger that could not be listed was reported as holding zero rows:\n%s", got)
	}
}

// TestStoreCheckNeverStatesACountItDidNotRead is the half the test above never
// checked, and the half that was wrong.
//
// It asserted the error line, the section after it and the verdict — never the
// census line — so it passed while the report printed "device first-seen ledger:
// 0 row(s); 3 of 4 mirrored device(s) have a stored first_seen" about a ledger
// holding three perfectly readable rows: a fabricated count, one line under a
// ratio that contradicts it arithmetically, with not one row listed. Exit 1
// protected a deploy script; the person reading the report was told a number that
// was not true, which is the failure this whole change exists to close.
//
// The fixture is the realistic pre-upgrade file, not a contrived one: the ledger
// is fine, and it is the PRE-LEDGER column that holds a value SQLite kept
// verbatim in an INTEGER-affinity column, so the seed PLAN fails while the
// listing would have succeeded. Before the fix, the plan's early return threw the
// listing away.
func TestStoreCheckNeverStatesACountItDidNotRead(t *testing.T) {
	dsn := seedStoreFileForCheck(t)
	ids := []string{
		"01J9CCCCM1RR0RR0WD3V1CE01",
		"01J9CCCCM1RR0RR0WD3V1CE02",
		"01J9CCCCM1RR0RR0WD3V1CE03",
		"01J9CCCCM1RR0RR0WD3V1CE04",
	}
	for i, id := range ids {
		firstSeen := "1787098315675"
		if i == 3 {
			// The pre-ledger column, holding what an older build left behind.
			firstSeen = `'not-an-instant'`
		}
		rawExec(t, dsn, fmt.Sprintf(
			`INSERT INTO discovered_devices
			   (device_id, relay_id, scope_node, driver, native_id, device_class,
			    name, address, model, serial, first_seen, last_seen, entities, open_ports, relay_last_seen)
			 VALUES ('%s','relay-a','site','roku-ecp','uuid:%s','media-player','Roku %d','192.168.50.31','','',%s,1787098400000,'[]','[]',0)`,
			id, id, i, firstSeen))
		if i < 3 {
			rawExec(t, dsn, fmt.Sprintf(
				`INSERT INTO device_first_seen (device_id, first_seen, origin) VALUES ('%s', 1787098315675, 'adopted')`, id))
		}
	}

	var out bytes.Buffer
	code := reportStoreIDs(dsn, &out)
	got := out.String()

	if strings.Contains(got, "ledger: 0 row(s)") {
		t.Fatalf("a three-row ledger was reported as holding zero rows:\n%s", got)
	}
	for _, want := range []string{
		// The census the check DID read, stated as read.
		"device first-seen ledger: 3 row(s); 3 of 4 mirrored device(s) have a stored first_seen.",
		// Every row, with its origin — the sentence the boot log refers operators
		// here for, and the one a plan failure used to swallow whole.
		ids[0] + "  first_seen=1787098315675  origin=" + store.FirstSeenAdopted,
		ids[1] + "  first_seen=1787098315675  origin=" + store.FirstSeenAdopted,
		ids[2] + "  first_seen=1787098315675  origin=" + store.FirstSeenAdopted,
		// The irreversibility caveat, which rides the listing and was omitted
		// with it.
		"are NOT instants this deployment observed",
		"retireDeviceFirstSeen",
		// And the seed plan is named as UNPLANNED rather than as "nothing".
		"could not be planned",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "the next boot will seed nothing into it") {
		t.Fatalf("a seed plan that FAILED was reported as a plan to seed nothing:\n%s", got)
	}
	if code != storeCheckIncomplete {
		t.Fatalf("exit code = %d, want %d\n%s", code, storeCheckIncomplete, got)
	}
	t.Logf("plan-failure -store-check output:\n%s", got)
}

// TestLedgerSectionStatesOnlyWhatItRead drives the print site directly with a
// census that has gaps in it, which is the only way to reach the branches whose
// store shapes are impractical to forge — and those are precisely the branches
// that shipped wrong.
//
// Each case names a POSITIVE claim the section used to make about something it
// had not read. Every one of them is a sentence an operator acts on: "the next
// boot creates it", "the boot will seed nothing", a row count.
func TestLedgerSectionStatesOnlyWhatItRead(t *testing.T) {
	readErr := errors.New("store: look for the device first-seen ledger table: disk I/O error")
	for _, tc := range []struct {
		name    string
		ledger  store.DeviceFirstSeenLedger
		err     error
		want    []string
		refuted []string
	}{
		{
			name:    "the table lookup failed",
			ledger:  store.DeviceFirstSeenLedger{}, // PresentKnown false
			err:     readErr,
			want:    []string{"whether the table is even on this store could not be established"},
			refuted: []string{"the table is not on this store yet", "the next boot creates it"},
		},
		{
			name:    "the table is genuinely absent",
			ledger:  store.DeviceFirstSeenLedger{PresentKnown: true, CountsKnown: true, PendingKnown: true},
			want:    []string{"the table is not on this store yet; the next boot creates it"},
			refuted: []string{"could not be established"},
		},
		{
			name: "the listing failed outright",
			ledger: store.DeviceFirstSeenLedger{
				PresentKnown: true, Present: true,
				CountsKnown: true, Mirrored: 4, Answered: 3,
				PendingKnown: true,
			},
			err:     errors.New("store: list the device first-seen ledger: no such column: origin"),
			want:    []string{"its rows could not be listed at all, so how many it holds is UNKNOWN"},
			refuted: []string{"ledger: 0 row(s)"},
		},
		{
			name: "the listing stopped partway",
			ledger: store.DeviceFirstSeenLedger{
				PresentKnown: true, Present: true,
				Rows:        []store.DeviceFirstSeenRow{{DeviceID: "01J9PART1AL0000000000000A", FirstSeen: 1787098315675, Origin: store.FirstSeenPlanted}},
				CountsKnown: true, Mirrored: 4, Answered: 3,
				PendingKnown: true,
			},
			err:     errors.New("store: list the device first-seen ledger: 2 row(s) could not be read"),
			want:    []string{"1 row(s) were listed before the listing failed, and there may be more"},
			refuted: []string{"ledger: 1 row(s);"},
		},
		{
			name: "the mirror counts failed",
			ledger: store.DeviceFirstSeenLedger{
				PresentKnown: true, Present: true, RowsComplete: true, PendingKnown: true,
			},
			err:     errors.New("store: count the discovered-device mirror: disk I/O error"),
			want:    []string{"how much of the mirrored device inventory it answers for could not be counted"},
			refuted: []string{"mirrored device(s) have a stored first_seen"},
		},
		{
			name: "the seed plan failed",
			ledger: store.DeviceFirstSeenLedger{
				PresentKnown: true, Present: true, RowsComplete: true, CountsKnown: true,
			},
			err:  errors.New("store: plan the device first-seen seed: Scan error"),
			want: []string{"could not be planned", "IRREVERSIBLY"},
			// The most dangerous sentence in the section: an unproduced plan read
			// as a plan to do nothing.
			refuted: []string{"the next boot will seed nothing into it"},
		},
		{
			name: "everything was read and there is nothing to do",
			ledger: store.DeviceFirstSeenLedger{
				PresentKnown: true, Present: true, RowsComplete: true,
				CountsKnown: true, Mirrored: 2, Answered: 2, PendingKnown: true,
			},
			want:    []string{"device first-seen ledger: 0 row(s); 2 of 2 mirrored device(s) have a stored first_seen.", "the next boot will seed nothing into it"},
			refuted: []string{"could not", "UNKNOWN"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &storeCheckReport{out: &bytes.Buffer{}}
			printDeviceFirstSeen(r, tc.ledger, tc.err)
			got := r.out.(*bytes.Buffer).String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q:\n%s", want, got)
				}
			}
			for _, refuted := range tc.refuted {
				if strings.Contains(got, refuted) {
					t.Fatalf("stated %q about something it did not read:\n%s", refuted, got)
				}
			}
		})
	}
}

// TestStoreCheckReportsTheWholeShapeWhenAColumnIsBlocked: on the one shape where
// an operator most needs the full picture — a store headed for maintenance mode
// — the check used to tell them the least. One blocked column discarded the
// created/added/divergent lists the same pass had already computed AND returned
// before the store was opened, so the row-id, org-root, ledger and event-log
// sections never ran at all.
//
// It also corrects what the refusal SAYS. Neither error InspectSchema can return
// stops the feeder from starting: both route to maintenance mode, where the
// process stays up on the same listener, /healthz reports maintenance_mode and
// every other route answers 503. "Refuse to start" and "up and serving 503s" are
// different incidents with different first moves.
func TestStoreCheckReportsTheWholeShapeWhenAColumnIsBlocked(t *testing.T) {
	dsn := seedStoreFileForCheck(t)
	rawExec(t, dsn,
		// Blocked: declared NOT NULL with no DEFAULT, which ALTER TABLE ADD COLUMN
		// cannot retrofit.
		`ALTER TABLE device_first_seen DROP COLUMN first_seen`,
		// Addable, and the thing that must still be reported beside the blockage.
		`ALTER TABLE discovered_devices DROP COLUMN open_ports`,
	)

	var out bytes.Buffer
	code := reportStoreIDs(dsn, &out)
	got := out.String()
	if code != storeCheckIncomplete {
		t.Fatalf("reportStoreIDs exit code = %d, want %d\n%s", code, storeCheckIncomplete, got)
	}
	for _, want := range []string{
		// The plan that used to be thrown away with the error.
		"discovered_devices.open_ports",
		// The blockage itself.
		"device_first_seen.first_seen",
		// What the boot ACTUALLY does about it.
		"MAINTENANCE MODE",
		"503",
		// And the sections that used to be suppressed by the early return.
		"row id",
		"org-kind scope node",
		"durable event log:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("a blocked column collapsed the report; %q absent from:\n%s", want, got)
		}
	}
	if strings.Contains(got, "refuse to start against this store") {
		t.Fatalf("the report says the feeder will refuse to start, when this refusal degrades to maintenance mode:\n%s", got)
	}
	t.Logf("blocked-column -store-check output:\n%s", got)

	// And the shape where the blocked column is the ONLY missing one: the
	// affirmative sentence must not be printed over it. `Added` is empty there,
	// so a report that keys the sentence on `Added` alone says "every column this
	// build declares is present" one line above the refusal naming the column
	// that is not.
	only := seedStoreFileForCheck(t)
	rawExec(t, only, `ALTER TABLE device_first_seen DROP COLUMN first_seen`)
	var onlyOut bytes.Buffer
	if code := reportStoreIDs(only, &onlyOut); code != storeCheckIncomplete {
		t.Fatalf("reportStoreIDs exit code = %d, want %d\n%s", code, storeCheckIncomplete, onlyOut.String())
	}
	if strings.Contains(onlyOut.String(), "every column this build declares is present") {
		t.Fatalf("the report contradicts itself: it claims every column is present and then refuses over a missing one:\n%s",
			onlyOut.String())
	}
	if !strings.Contains(onlyOut.String(), "CANNOT be added by an ALTER") {
		t.Fatalf("the report does not state that a declared column is missing and unaddable:\n%s", onlyOut.String())
	}
}

// TestStoreCheckSweepsEveryTableWhenOneIsMissing is the row-id sweep's half of
// the same defect, and the false refusal it produced.
//
// planIDRewrites walks thirteen tables and returned the first read error, so a
// store missing ONE declared table — a shape store.Open creates and proceeds
// from, because applySchemaDDL runs before MigrateRowIDs — produced "CANNOT be
// canonicalized … the feeder will refuse to start against this store", four
// lines under this report's own statement that the boot would create that table.
// A genuinely non-canonical id in a later table was never named.
func TestStoreCheckSweepsEveryTableWhenOneIsMissing(t *testing.T) {
	dsn := seedStoreFileForCheck(t)

	// `casts` is swept third of thirteen and `dayparts` fifth, so the abort used
	// to hide a real pending rewrite behind a table that simply was not there yet.
	const legacyDaypart = "01J8Z4DEMOSCREENFIRSTPHOTN"
	rawExec(t, dsn,
		`DROP TABLE casts`,
		`UPDATE dayparts SET id = '`+legacyDaypart+`' WHERE id = (SELECT id FROM dayparts ORDER BY id LIMIT 1)`,
	)

	var out bytes.Buffer
	code := reportStoreIDs(dsn, &out)
	got := out.String()
	for _, want := range []string{
		// The table it could not sweep, framed as what it is.
		"the casts table does not exist on this file",
		"the next boot creates it and proceeds",
		// And the id in a LATER table, which the abort used to hide.
		legacyDaypart,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the sweep stopped at the first table; %q absent from:\n%s", want, got)
		}
	}
	if strings.Contains(got, "REFUSE TO START") {
		t.Fatalf("the report claims the boot will refuse against a store the boot opens cleanly:\n%s", got)
	}
	if code != storeCheckPending {
		t.Fatalf("reportStoreIDs exit code = %d, want %d (a pending table and a pending rewrite are work)\n%s",
			code, storeCheckPending, got)
	}
	t.Logf("missing-table -store-check output:\n%s", got)
}

// TestStoreCheckRefusesAnEmptyFile: a zero-byte store is a truncated restore, an
// interrupted copy, or a shell redirect — and it used to be described as "every
// column this build declares is present", because the column planner drops the
// absent-table list when the file holds NONE of them (a fresh install). The
// first line an operator read was a positive schema assertion about a carcass.
func TestStoreCheckRefusesAnEmptyFile(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "truncated.db")
	if err := os.WriteFile(dsn, nil, 0o600); err != nil {
		t.Fatalf("write the empty file: %v", err)
	}

	var out bytes.Buffer
	code := reportStoreIDs(dsn, &out)
	got := out.String()
	if code != storeCheckIncomplete {
		t.Fatalf("reportStoreIDs exit code = %d, want %d for a zero-byte store\n%s", code, storeCheckIncomplete, got)
	}
	if strings.Contains(got, "every column this build declares is present") {
		t.Fatalf("an empty file was reported as schema-conforming:\n%s", got)
	}
	if !strings.Contains(got, "ZERO BYTES") {
		t.Fatalf("the report does not say what is wrong with the file:\n%s", got)
	}
}

// TestStoreCheckRefusesADirectory: "you typed the wrong path" used to arrive
// dressed as schema drift — every InspectSchema failure was rendered as "%s
// CANNOT be brought up to this build's schema", whatever it actually was.
func TestStoreCheckRefusesADirectory(t *testing.T) {
	dir := t.TempDir()

	var out bytes.Buffer
	code := reportStoreIDs(dir, &out)
	got := out.String()
	if code != storeCheckIncomplete {
		t.Fatalf("reportStoreIDs exit code = %d, want %d for a directory\n%s", code, storeCheckIncomplete, got)
	}
	if !strings.Contains(got, "is a DIRECTORY") {
		t.Fatalf("the report does not say the path is a directory:\n%s", got)
	}
	if strings.Contains(got, "brought up to this build's schema") {
		t.Fatalf("a wrong path was described as a schema problem:\n%s", got)
	}
}

// TestStoreCheckVerdictIsTheLastLine: the code and the prose are printed
// together so they cannot drift apart. An operator reading the report and a
// script reading the exit status must be told the same thing.
func TestStoreCheckVerdictIsTheLastLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want string
		fix  func(t *testing.T, dsn string)
	}{
		{"conforming", storeCheckClean, "VERDICT: NOTHING TO DO (exit 0)", func(*testing.T, string) {}},
		{"pending", storeCheckPending, "VERDICT: WORK PENDING (exit 2)", func(t *testing.T, dsn string) {
			rawExec(t, dsn, `ALTER TABLE discovered_devices DROP COLUMN open_ports`)
		}},
		{"incomplete", storeCheckIncomplete, "VERDICT: INCOMPLETE (exit 1)", func(t *testing.T, dsn string) {
			rawExec(t, dsn, `UPDATE playlists SET id = 'demo-playlist'`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := seedStoreFileForCheck(t)
			tc.fix(t, dsn)

			var out bytes.Buffer
			code := reportStoreIDs(dsn, &out)
			if code != tc.code {
				t.Fatalf("exit code = %d, want %d\n%s", code, tc.code, out.String())
			}
			lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
			verdict := ""
			for _, l := range lines {
				if strings.HasPrefix(l, "VERDICT: ") {
					verdict = l
				}
			}
			if !strings.HasPrefix(verdict, tc.want) {
				t.Fatalf("verdict line %q does not start with %q\n%s", verdict, tc.want, out.String())
			}
		})
	}
}

// liveWriter is the RUNNING FEEDER, as far as the store is concerned: a second
// connection that stays open, writes through the write-ahead log, and never
// checkpoints. It is closed at the end of the test, which is the first moment
// anything it wrote can reach the database file.
//
// The distinction is the whole point of the tests below. A writer that CLOSES —
// which is what `rawExec` does, and what the previous version of these tests
// drove — checkpoints on the way out and therefore moves the `.db` file's size
// and mtime. That is the one shape a stat-based guard can see, and it is not the
// shape the runbook prescribes.
func liveWriter(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dsn+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open a live writer on %s: %v", dsn, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, p := range []string{`PRAGMA journal_mode = WAL`, `PRAGMA wal_autocheckpoint = 0`} {
		if _, err := db.Exec(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	return db
}

// TestStoreCheckSaysWhenTheStoreMovedUnderIt: the report is a torn read of a
// moving store and is ADVERTISED for exactly that use — the flag's help says it
// is safe against a live store and the first-photon runbook says to run it with
// the old build still up. Each section is its own unsnapshotted reading, so they
// can describe different states of one store, and the output used to give the
// reader no way to see that they had.
//
// # The fixture is a LIVE WAL writer, because that is what was invisible
//
// The guard this replaces compared `os.Stat` of the database file before and
// after — the one file a WAL-mode writer does not touch. This test asserts that
// blindness directly: it records the file's size and mtime around the writes and
// FAILS if they moved, so the fixture is provably the case a stat cannot see, and
// then requires the note anyway. Against the stat guard the second half cannot
// pass; against a guard watching `PRAGMA data_version` it does.
//
// The previous version drove `rawExec`, which opens a connection, writes, and
// CLOSES it — checkpointing, and moving the database file. It therefore passed
// against the blind guard as readily as against a working one.
//
// # And it reaches the verdict
//
// A torn reading does not mean the store is faulty — a store under a running
// feeder is the documented invocation — but it does mean exit 0's promise ("the
// next boot changes nothing in this store") was made about a snapshot that no
// longer exists. Printing "re-take it before acting on anything in it" three
// lines above "VERDICT: NOTHING TO DO (exit 0)" told the human and the deploy
// gate opposite things. It lands on WORK PENDING: read the report, then decide.
func TestStoreCheckSaysWhenTheStoreMovedUnderIt(t *testing.T) {
	t.Run("quiescent", func(t *testing.T) {
		dsn := seedStoreFileForCheck(t)
		quiet := &storeCheckReport{out: &bytes.Buffer{}}
		noteConcurrentWrites(quiet, dsn, openConcurrencyWitness(quiet, dsn))
		if buf := quiet.out.(*bytes.Buffer); buf.Len() != 0 {
			t.Fatalf("an unchanged store produced a concurrency warning: %s", buf.String())
		}
		if len(quiet.gaps) != 0 || len(quiet.pending) != 0 {
			t.Fatalf("an unchanged store recorded %v / %v", quiet.gaps, quiet.pending)
		}
	})

	t.Run("a live WAL writer the database file cannot show", func(t *testing.T) {
		dsn := seedStoreFileForCheck(t)
		live := liveWriter(t, dsn)

		r := &storeCheckReport{out: &bytes.Buffer{}}
		w := openConcurrencyWitness(r, dsn)
		if w == nil {
			t.Fatalf("the witness could not be opened: %s", r.out.(*bytes.Buffer).String())
		}
		before, err := os.Stat(dsn)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}

		for i := 0; i < 25; i++ {
			if _, err := live.Exec(
				`INSERT INTO device_first_seen (device_id, first_seen, origin) VALUES (?, 1787098315675, 'planted')`,
				fmt.Sprintf("01J9L1VEWR1TER%011d", i)); err != nil {
				t.Fatalf("live write %d: %v", i, err)
			}
		}

		after, err := os.Stat(dsn)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		// The fixture's own claim, asserted so it cannot rot into the easy case:
		// 25 committed rows and the database file has not moved a byte.
		if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
			t.Fatalf("the fixture checkpointed, so it no longer tests the blind spot: %d byte(s) at %v -> %d byte(s) at %v",
				before.Size(), before.ModTime(), after.Size(), after.ModTime())
		}
		t.Logf("25 committed rows; the .db is unmoved (%d byte(s), mtime %v) — a stat guard sees nothing here",
			after.Size(), after.ModTime())

		noteConcurrentWrites(r, dsn, w)
		got := r.out.(*bytes.Buffer).String()
		for _, want := range []string{
			"CHANGED while this report was being taken",
			"another connection committed to it",
			"Re-take it against a quiescent store",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("a live WAL writer went unreported (%q missing):\n%s", want, got)
			}
		}
		if len(r.pending) != 1 {
			t.Fatalf("a torn report recorded %d pending item(s), want 1: %v", len(r.pending), r.pending)
		}
		if len(r.gaps) != 0 {
			t.Fatalf("a live store was recorded as a gap (%v); it is the documented case, not a failure", r.gaps)
		}
		if code := r.verdict(); code != storeCheckPending {
			t.Fatalf("a torn report exits %d, want %d — exit 0 promises the reader that the next boot changes nothing",
				code, storeCheckPending)
		}
	})

	// End to end, through the real entry point: a writer committing throughout
	// the report must never leave it saying "NOTHING TO DO".
	t.Run("through the whole report", func(t *testing.T) {
		dsn := seedStoreFileForCheck(t)
		live := liveWriter(t, dsn)

		// The write happens INSIDE the report, deterministically, rather than
		// from a goroutine racing it: the store-check path calls out to the store
		// package for every section, so a witness registered by the store is the
		// hook that says "the report has begun". Racing a writer against the
		// report is what a first draft of this did, and on a loaded machine the
		// report finished before the goroutine's first commit landed — a test
		// that silently stops testing when the box is busy is the shape this
		// programme keeps finding.
		var wrote int
		reportStoreIDsMidRunHookForTest = func() {
			if wrote > 0 {
				return
			}
			for i := 0; i < 5; i++ {
				if _, err := live.Exec(
					`INSERT INTO device_first_seen (device_id, first_seen, origin) VALUES (?, 1787098315675, 'planted')`,
					fmt.Sprintf("01J9DUR1NGREP0RT%09d", i)); err != nil {
					t.Errorf("live write %d during the report: %v", i, err)
					return
				}
				wrote++
			}
		}
		t.Cleanup(func() { reportStoreIDsMidRunHookForTest = nil })

		var out bytes.Buffer
		code := reportStoreIDs(dsn, &out)
		got := out.String()

		if wrote == 0 {
			t.Fatalf("nothing was written during the report, so the fixture did not reproduce a torn read:\n%s", got)
		}
		if !strings.Contains(got, "CHANGED while this report was being taken") {
			t.Fatalf("a report taken over a store being written to did not say so:\n%s", got)
		}
		if code != storeCheckPending {
			t.Fatalf("a torn report exited %d, want %d — exit 0 says 'the next boot changes nothing in this store' "+
				"about a store that moved:\n%s", code, storeCheckPending, got)
		}
		t.Logf("live-store -store-check exit=%d after %d mid-report commit(s)", code, wrote)
	})
}

// --- the event log's retention sweep ----------------------------------------

// insertEvent forges one durable event straight into the store file.
func insertEvent(t *testing.T, dsn, id, class string, tsMs int64) {
	t.Helper()
	rawExec(t, dsn, fmt.Sprintf(
		`INSERT INTO events (id, schema, ts, scope_node, trace_id, cost_class, retention_class, origin, origin_principal, payload)
		 VALUES ('%s','automation.run/1',%d,'','','','%s','','','{}')`, id, tsMs, class))
}

// TestStoreCheckPlansTheEventLogEviction: the exit-0 verdict says in so many
// words that "the next boot changes nothing in this store", and the boot's first
// act on the event log is a retention sweep that DELETES rows.
//
// The section reported only what the log currently HOLDS, so one telemetry event
// past its seven-day window — an entirely ordinary thing on a box that has been
// up a week — printed "durable event log: 1 event(s) retained, telemetry-standard=1"
// and then "VERDICT: NOTHING TO DO (exit 0)", while the very next boot deleted it.
// A deploy gate reading the exit status was told "proceed, nothing changes" by
// the one tool whose job is to say what the restart will do.
func TestStoreCheckPlansTheEventLogEviction(t *testing.T) {
	dsn := seedStoreFileForCheck(t)
	expired := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	fresh := time.Now().UnixMilli()
	insertEvent(t, dsn, "01J9EXP1REDTELEMETRYR0W01", "telemetry-standard", expired)
	insertEvent(t, dsn, "01J9EXP1REDTELEMETRYR0W02", "telemetry-standard", expired)
	insertEvent(t, dsn, "01J9FRESHTELEMETRYR0W0001", "telemetry-standard", fresh)

	var out bytes.Buffer
	code := reportStoreIDs(dsn, &out)
	got := out.String()

	for _, want := range []string{
		"durable event log: 3 event(s) retained, telemetry-standard=3",
		"the next boot's retention sweep DELETES 2 of them",
		"telemetry-standard=2",
		// The bound on what goes, so a resume gap is predictable in advance.
		"every id up to and including 01J9EXP1REDTELEMETRYR0W02 goes",
		"2 event(s) the boot's retention sweep DELETES",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the report does not plan the boot's retention sweep (%q missing):\n%s", want, got)
		}
	}
	if code != storeCheckPending {
		t.Fatalf("exit code = %d, want %d — a boot that deletes rows is not 'nothing to do'\n%s",
			code, storeCheckPending, got)
	}
	if strings.Contains(got, "VERDICT: NOTHING TO DO") {
		t.Fatalf("the verdict promised the next boot changes nothing, over a store it is about to evict from:\n%s", got)
	}

	// And the promise is kept: do what the boot does next, and it is exactly what
	// was planned.
	st, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	log, err := st.EventLog(events.DefaultRetentionPolicy(), nil, func(error) {})
	if err != nil {
		t.Fatalf("EventLog: %v", err)
	}
	pruned, err := log.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned.Rows != 2 || pruned.ByClass["telemetry-standard"] != 2 {
		t.Fatalf("the boot pruned %d (%v); the report planned 2 telemetry-standard", pruned.Rows, pruned.ByClass)
	}
	if pruned.EvictedThrough != "01J9EXP1REDTELEMETRYR0W02" {
		t.Fatalf("the boot evicted through %q; the report named 01J9EXP1REDTELEMETRYR0W02", pruned.EvictedThrough)
	}
	t.Logf("planned 2 / telemetry-standard=2; boot pruned %d %v through %s", pruned.Rows, pruned.ByClass, pruned.EvictedThrough)
}

// TestStoreCheckSaysTheSweepRetiresNothing keeps "the sweep takes nothing" and
// "the sweep was never planned" from reading the same, which is the rule the rest
// of this report keeps.
func TestStoreCheckSaysTheSweepRetiresNothing(t *testing.T) {
	dsn := seedStoreFileForCheck(t)
	insertEvent(t, dsn, "01J9FRESHTELEMETRYR0W0001", "telemetry-standard", time.Now().UnixMilli())

	var out bytes.Buffer
	code := reportStoreIDs(dsn, &out)
	got := out.String()
	if !strings.Contains(got, "the next boot's retention sweep retires none of them") {
		t.Fatalf("a sweep that takes nothing did not say so:\n%s", got)
	}
	if code != storeCheckClean {
		t.Fatalf("exit code = %d, want %d — nothing is pending on this store\n%s", code, storeCheckClean, got)
	}
}

// --- the exit code the flag parser owns -------------------------------------

// TestMalformedInvocationIsNotAContractCode: `flag.CommandLine` is created with
// flag.ExitOnError, so a malformed flag exits 2 and `-h` exits 0 — before main
// regains control, with no report and no VERDICT printed. The store-check
// contract this change minted declares 2 "WORK PENDING — read the report, then
// deploy" and 0 "NOTHING TO DO — restart normally", and the first-photon runbook
// tells an operator to gate on exactly those.
//
// So `waiveo-feeder -store-chek <path>` — one transposed letter, in the flag the
// subcommand is named for — returned the same status as a healthy store with
// pending work. Before the contract existed the same typo landed on an undefined
// code that any `!= 0` guard stopped on, so the contract made this WORSE.
//
// This drives the binary's own parser, which is the thing that has to own its
// codes; the mapping to storeCheckIncomplete is in main, one line from here.
func TestMalformedInvocationIsNotAContractCode(t *testing.T) {
	for _, argv := range [][]string{
		{"-store-chek", "/opt/waiveo-next/.dev/feeder-store.db"},
		{"-store-check", "--verbose"},
		{"-store-check=maybe"},
		{"-h"},
		{"--help"},
		{"-store-check", "-h"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			var usage bytes.Buffer
			_, _, err := parseArgs(argv, &usage)
			if err == nil {
				t.Fatalf("%v parsed cleanly; flag.ExitOnError used to exit 2 (or 0 for -h) on this, "+
					"which the store-check contract reads as WORK PENDING (or NOTHING TO DO)", argv)
			}
		})
	}
}

// TestAValidInvocationStillParses is the other half: a parser that refused
// everything would satisfy the test above and break the binary.
func TestAValidInvocationStillParses(t *testing.T) {
	for _, tc := range []struct {
		argv  []string
		check bool
		args  []string
	}{
		{argv: nil, check: false, args: nil},
		{argv: []string{"-store-check"}, check: true, args: nil},
		{argv: []string{"-store-check", "/opt/waiveo-next/.dev/feeder-store.db"}, check: true,
			args: []string{"/opt/waiveo-next/.dev/feeder-store.db"}},
		{argv: []string{"--store-check", "/tmp/a.db"}, check: true, args: []string{"/tmp/a.db"}},
	} {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			var usage bytes.Buffer
			check, args, err := parseArgs(tc.argv, &usage)
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.argv, err, usage.String())
			}
			if check != tc.check {
				t.Fatalf("store-check = %v, want %v", check, tc.check)
			}
			if strings.Join(args, " ") != strings.Join(tc.args, " ") {
				t.Fatalf("operands = %v, want %v", args, tc.args)
			}
		})
	}
}
