package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	stdlog "log"
)

// devicefirstseen.go holds the one thing about a discovered device that the
// relay reporting it structurally CANNOT know: when this site first saw it.
//
// # The defect
//
// `first_seen` is the only field that answers "is this device new to my
// network, or has it been here for weeks" — the question the discovery
// inventory exists to help an operator answer, and the difference between a new
// arrival worth investigating and long-standing furniture. It was always
// populated, always plausible, and always wrong.
//
// The relay stamps it when a candidate is new to its IN-MEMORY candidate map
// (internal/relay/deviceplane.Store.Observe). That map is empty at every relay
// start — the relay never persists candidates, by owner decision — so after a
// relay restart EVERY device on the LAN looks brand new, and the mirror wrote
// that straight over the durable value it already held. Measured on box .12 on
// three separate occasions: all 61 rows carried a `first_seen` equal, to the
// second, to the relay's process start. The feeder restored the right value at
// boot and the relay's next report destroyed it a second later.
//
// # Why the value lives here rather than in discovered_devices
//
// A relay's report REPLACES that relay's mirror rows wholesale — DELETE then
// INSERT in one transaction (discovereddevices.go) — which is right for a cache
// of what a relay currently sees and fatal for a fact that must outlive every
// report. That is the identical argument ignoreddevices.go makes for holding an
// operator's ignore decision in its own identity-keyed table, and this table is
// its twin: the replace never deletes from it, so the value survives the report
// that no longer mentions the device, the relay restart that forgets it, and the
// relay swap that re-homes it.
//
// Keyed by the DERIVED device_id and nothing else. REL-153 scopes a device's
// identity to `(site, driver, native_id)` and explicitly not to the relay that
// happens to report it, so a per-identity fact must not be scoped by relay
// either: re-homing a device to a second relay serving the same site resolves to
// the same device_id, and must therefore resolve to the same history.
//
// discovered_devices.first_seen stays, as a PROJECTION of this ledger written in
// the same transaction, and the sole writer of both copies is
// ReplaceDiscoveredDevices, which is what keeps them from drifting.
//
// READERS go through the ledger, not the column — readDiscoveredDevices joins it
// — because "projection" turned out to be true of every row but one population.
// The backfill below refuses a pre-ledger value it judges implausible and
// deliberately leaves the column holding it (never-wipe: it is the only copy of
// that history). Read as the projection it usually is, that leftover became the
// device's age: a boot logged "refused … it has no age until the next report
// plants one" and then served the refused number to the console in the same
// breath. The join makes the ledger the single answer, and the column stays a
// durable record the seed keeps judging rather than a second opinion anyone
// serves.
//
// # The value is the APP's own observation, and the relay's clock is never read
//
// first_seen is planted ONCE, from the app's SEC-066 clock floor, at the first
// report this site durably stored for the device — and then never moves, in
// either direction, for the life of the row.
//
// The relay's own `first_seen`/`last_seen` numbers do not enter the arithmetic
// at all. They cannot: they are stamped on the relay's BARE HOST WALL CLOCK
// (cmd/waiveo-relay passes `time.Now().UnixMilli` into every discovery lane),
// and nothing on this platform attests that clock. relay/1 REL-038 defines a
// `clock_state` for exactly this judgement — and the live relay hardcodes it to
// `{untrusted, cold_boot}` at hello (cmd/waiveo-relay/main.go) because the
// verified-time source REL-132 requires is a deliberate later concern:
// ApplyVerifiedTime has no caller outside its own tests. So today EVERY relay's
// stamps are unattested, and a design that reads them is reading a number no
// part of this system stands behind.
//
// An earlier draft of this fix did read them — as a DURATION between the two
// stamps, on the theory that a duration survives an offset the absolute
// readings do not. It does not, and the reason is in the producing code:
// deviceplane.Store.Observe stamps `FirstSeen` once at candidate birth and
// never revises it, while re-observation advances `LastSeen`. Any clock STEP
// between those two moments lands entirely inside the reported "duration". On a
// Pi — no RTC, fake-hwclock restoring the last shutdown time at boot, discovery
// lanes firing seconds before NTP corrects it — that step is the normal case,
// and it is a FORWARD step, so the report claims the device has been watched for
// exactly as long as the box was switched off. Measured against the draft: a
// three-day power-off stamped a device discovered moments ago as three days old,
// permanently; a virgin clock stamped a fresh-install device at the clamp's own
// ceiling, permanently. That inverts the signal this field exists to give — a
// brand-new site reporting its whole inventory as month-old furniture — and it
// is worse than the defect it replaced, whose wrong value at least re-derived to
// a plausible "now" at the next relay restart.
//
// What is lost by refusing the relay's evidence is the window where a relay was
// up and this side was not writing (box .12 spent seven days with a mirror that
// could not write a row): those devices read as first seen when the mirror
// recovered rather than when the relay first saw them. That is the honest
// answer to the question the field actually asks — "when did THIS SITE first
// hold a report of this device" — and it is a bounded error tied to a real site
// event, against a systematic one that would mis-age every device on every
// fresh install. A witnessed fact beats hearsay from an unattested clock.
//
// # Nothing AUTOMATIC deletes from this table, and nothing updates it
//
// Not the replace, not a relay's revocation (ForgetDiscoveredDevices), not a
// device dropping off the LAN. A device that vanishes for a month and comes back
// is exactly the case the column exists to answer, and repairs/1 REP-012 already
// settled the same question for issues: "a resolved issue MUST retain its
// first_seen_at. An operator asking 'was this happening before?' is asking a
// question only the retained record answers." Never-wipe says the same thing
// from the other direction — discarding the history to bound a table is not a
// fix.
//
// Plant-once also makes the merge idempotent and commutative with no comparison
// to get wrong: two relays reporting one device (REL-111a) converge on the same
// answer regardless of arrival order, a replayed report changes nothing, and
// there is no arm of the statement through which a later value can overwrite an
// earlier one — which is the whole class of defect this file exists to close.
//
// # …but plant-once shipped as a ratchet with no escape hatch, and that was wrong
//
// The first cut of this file had NO update arm and NO delete arm anywhere, which
// made a wrong value permanent for the life of the row. On box .12 that landed
// immediately: 64 of 64 rows were seeded from the pre-ledger column (below) and
// not one of them can be corrected, so a value that is provably later than a
// backup file already listing those devices is frozen at that reading forever.
// Pre-fix, the wrong value at least re-derived at every relay restart.
//
// This platform has already decided that a one-way ratchet needs one sanctioned
// way down. SEC-066's clock floor is exactly such a ratchet and it did not ship
// without `clock_floor.reset` (SEC-075), admitted on SEC-076's reasoning that
// the verb "adds audit, never new reach". RetireDeviceFirstSeen is the same
// admission for this ledger, and it is the ONLY arm through which a stored value
// ever leaves: an explicit, authorized, per-device operator act on a row someone
// can prove wrong. It is not a bulk repair and it cannot invent a better answer
// — a retire can only make a device read YOUNGER, since the next report plants
// "now" — which is why there is no operator-SET verb beside it (SEC-067 refuses
// unverifiable time claims everywhere else, and no verified time source is wired
// on this platform at all).
//
// Growth is bounded in practice and tiny in absolute terms: one row of roughly
// fifty bytes per DISTINCT device identity the site has ever seen, against a
// relay that will not hold more than maxStoredCandidates (1024) at a time.
//
// # …and it shipped with no way to tell a witnessed instant from a rescued guess
//
// The first cut of this table held (device_id, first_seen) and nothing else, and
// its own backfill docstring said the consequence out loud: "the ledger carries
// no provenance column, so once these rows are in it NOTHING distinguishes a
// backfilled lower bound from a planted instant." That was written as a
// justification for logging every adoption; it is really a description of a
// missing column. The boot log is not a record — it is truncated at twenty lines,
// it scrolls away, and on box .12 (64 adoptions) it named two thirds of the
// devices and lost the rest. Meanwhile the console renders every row of this
// ledger identically, as an exact age, because the served value carries nothing
// that would let it do otherwise.
//
// `origin` is that column. It is written by the two — and only two — writers that
// can put a value here, it costs one short string per device, and it is what lets
// every surface downstream state what it actually knows:
//
//   - `planted` — stamped by plantDeviceFirstSeen from the app's own SEC-066
//     clock at a report this side durably stored. An observation.
//   - `adopted` — copied by backfillDeviceFirstSeen out of the pre-ledger column
//     at the upgrade that introduced this table. Read off the reporting relay's
//     unattested clock at that relay's last restart; an ESTIMATE, and not an
//     instant anything on this side observed.
//   - the empty string, read back as `unrecorded` — a row written by the build
//     that had no origin column. Box .12's 64 rows are exactly this, and every
//     one of them is in fact an adoption; but no code can prove that per row, so
//     the honest answer is that the provenance was not recorded rather than a
//     guess dressed as one.
//
// Adding it is an ALTER on every store that already has the table, which is why
// it is `TEXT NOT NULL DEFAULT ”` — a constant default is what makes the column
// addable at all (schemamigrate.go's whyNotAddable), and the empty string is the
// only default that does not assert something untrue about rows written before
// it existed.
const deviceFirstSeenSchema = `
CREATE TABLE IF NOT EXISTS device_first_seen (
	device_id  TEXT PRIMARY KEY,
	first_seen INTEGER NOT NULL,
	origin     TEXT NOT NULL DEFAULT ''
);
`

// The three answers to "where did this device's stored age come from", as the
// api/1 Device.first_seen_origin enum spells them.
//
// FirstSeenUnrecorded is not stored: it is what the empty string on disk READS
// AS, so exactly one vocabulary reaches a caller and no surface has to know that
// a blank is the pre-column state. The other two are stored verbatim.
const (
	FirstSeenPlanted    = "planted"
	FirstSeenAdopted    = "adopted"
	FirstSeenUnrecorded = "unrecorded"
)

// firstSeenOrigin renders a stored origin for a reader. It exists so the blank
// that an upgraded row carries is translated in ONE place rather than at each of
// the four surfaces that would otherwise each decide what a blank means.
func firstSeenOrigin(stored string) string {
	switch stored {
	case FirstSeenPlanted, FirstSeenAdopted:
		return stored
	default:
		return FirstSeenUnrecorded
	}
}

// FirstSeenIsObserved reports whether an origin means "this deployment watched
// this happen" — the one reading that entitles a surface to render the value as
// an exact instant.
//
// Both other answers are unverified in the SAME way and are deliberately not
// distinguished here: an `adopted` value is known to have come off an unattested
// relay clock, and an `unrecorded` one cannot be shown not to have. A caller
// asking "may I present this as an observation" gets false for both.
func FirstSeenIsObserved(origin string) bool { return origin == FirstSeenPlanted }

// minPlausibleInstantMs is the floor below which an epoch-millisecond value is
// not a time this platform could have observed: 2020-09-13, years before
// waiveo-next existed.
//
// It is a REFUSAL threshold, never a clamp. A value below it is not a weak
// observation to be bounded into shape — it is a clock that was never set, and
// bounding it would manufacture a durable fact ("first seen exactly N days ago")
// that no evidence supports and that plant-once could never afterwards correct.
// Refusing leaves the device with no answer, which reads as an em dash in the
// console and is repaired by the first report taken on a working clock.
//
// Two things are measured against it, and nothing else is:
//
//   - the app's own clock at the moment of a plant. ClockFloor.Now() is
//     max(floor, wall) and a box that has never established a floor has none
//     (SEC-066), so on a host with a dead RTC the reading before NTP lands is
//     the bare wall clock at or near the epoch. Planting that would pin the
//     device to 1970 forever.
//   - a value being adopted from the pre-ledger column at upgrade
//     (backfillDeviceFirstSeen), which is the ONE remaining path by which a
//     relay-clock reading can enter this table at all.
const minPlausibleInstantMs = 1_600_000_000_000

// FirstSeenAdoption is one mirror row whose pre-ledger `first_seen` the backfill
// took into the ledger — or, from the read-only planner, one it WOULD take at the
// next open, exactly as ColumnAddition doubles for both readings.
//
// FirstSeen is an ESTIMATE read off the reporting relay's unattested clock, not
// an instant this side observed: see backfillDeviceFirstSeen for what the value
// actually attests and, just as importantly, for which direction it can be wrong
// in.
type FirstSeenAdoption struct {
	DeviceID  string
	FirstSeen int64
}

// UnverifiedFirstSeenSentence is the ONE sentence every surface uses to say what an
// adopted value is. It is a constant rather than four hand-written phrasings
// because the previous four disagreed with each other and with this file's own
// header: the boot log and `-store-check` called it "a LOWER BOUND on this
// device's age" without qualification, while the header at
// backfillDeviceFirstSeen analyses the case where it OVERSTATES the age (a relay
// whose clock ran behind wrote a value earlier than this side's true first hold)
// and says nothing can detect it. Both cannot be true, and the operator was told
// only the half that sounds like a guarantee.
//
// So it claims exactly what the value supports: where it was read, that it is
// usually — not always — a lower bound, and which way it fails.
// It is exported because `-store-check` (cmd/waiveo-feeder) has to say the same
// thing: a caveat that two surfaces word differently is a caveat an operator has
// to reconcile.
const UnverifiedFirstSeenSentence = "read off the reporting relay's own unattested clock rather than observed here, " +
	"so it is USUALLY a lower bound on the device's age and OVERSTATES it if that relay's clock ran behind"

// FirstSeenRefusal is one mirror row the backfill declined to adopt, with the
// value it declined and why. Reason is written for the operator reading a boot
// log, not for a caller switching on it — the same contract SchemaDrift.Reason
// keeps.
type FirstSeenRefusal struct {
	DeviceID  string
	FirstSeen int64
	Reason    string
}

// DeviceFirstSeenBackfill reports what the seed did — or, from
// InspectDeviceFirstSeen, what the next open would do.
//
// Both lists are empty on every boot after the first and on every fresh install,
// which is the overwhelmingly common case: no mirror row is missing a ledger row,
// so nothing is planned, nothing is written and nothing is said.
//
// Unlanded is the VERIFICATION half and must always be empty. It is the planned
// adoptions that were still missing a ledger row after the insert ran — the
// migration idiom this store already applies to its columns (schemamigrate.go
// re-plans inside the transaction as its own proof of idempotence), applied to
// the one irreversible act in the whole open. A non-empty Unlanded means the
// insert and the plan disagree about the same store, which no operator should
// have to infer from a device that quietly has no age.
type DeviceFirstSeenBackfill struct {
	Adopted  []FirstSeenAdoption
	Refused  []FirstSeenRefusal
	Unlanded []FirstSeenAdoption
	// AtMs is the app clock the FUTURE bound was judged against, carried so the
	// report can state what "the future" meant on the box that made the decision.
	AtMs int64
}

// empty reports whether the pass had nothing whatever to consider — the silent
// case, and the ONLY one that is allowed to be silent.
func (m DeviceFirstSeenBackfill) empty() bool {
	return len(m.Adopted) == 0 && len(m.Refused) == 0 && len(m.Unlanded) == 0
}

// DeviceFirstSeenRow is one stored ledger row as a reader sees it: the device,
// the instant that stands for it, and where that instant came from.
//
// Origin is already translated (firstSeenOrigin), so a caller never sees the
// blank an upgraded row carries on disk.
type DeviceFirstSeenRow struct {
	DeviceID  string
	FirstSeen int64
	Origin    string
}

// DeviceFirstSeenLedger is the ledger's own census: how much of it there is, how
// much of the mirror it answers for, WHAT IS ACTUALLY IN IT, and what the next
// open would seed into it. It backs `waiveo-feeder -store-check`, which is the one
// command an operator runs BEFORE a restart — so the seed a boot is about to
// perform, and the ages it is about to freeze, are readable in advance rather than
// only reconstructible from the boot log afterwards.
//
// Rows is the listing, and it is the half that was missing. The census used to be
// counts only, which made the boot log's own referral — "`waiveo-feeder
// -store-check` lists the ledger in full", printed at the truncation cap — false
// in two directions at once: this call issued no SELECT over the ledger's rows at
// all, and the pending list it did print is empty precisely BECAUSE the boot
// succeeded. An operator following that sentence after box .12's 64-row seed
// would have been told there was nothing to list.
//
// Present is false when the store file does not carry the table yet, which is the
// state box .12 was in and which `-store-check` could not name: the column pass
// deliberately says nothing about an absent TABLE (schemamigrate.go's
// planSchemaColumns), so the ledger has to report its own pendency.
//
// # Every number carries a flag saying whether it was READ
//
// This value is returned beside an error rather than instead of one, so a caller
// printing it is printing a PARTIAL census — and a partial census whose gaps look
// like zeroes is worse than no census. A false `PresentKnown` and a zero
// `Present` are the same two bits as "the table is genuinely absent"; a failed
// listing and an empty ledger are both `len(Rows) == 0`. `-store-check` printed
// exactly that: "device first-seen ledger: 0 row(s); 3 of 4 mirrored device(s)
// have a stored first_seen", a fabricated count of a three-row ledger, one line
// under a ratio that contradicts it arithmetically.
//
// So each answer travels with the bit that says it IS an answer. A caller must
// not state a number whose flag is false — it must say it could not read it.
type DeviceFirstSeenLedger struct {
	Present bool
	// PresentKnown is false when this census could not determine whether the
	// ledger table is on the store at all. Present is then meaningless, and in
	// particular is NOT "the table is absent".
	PresentKnown bool
	Rows         []DeviceFirstSeenRow
	// RowsComplete is false when the listing failed — either outright, or partway
	// through, in which case Rows holds the rows read BEFORE the failure and
	// there may be more. It is never "the ledger holds len(Rows) rows".
	RowsComplete bool
	Mirrored     int
	Answered     int
	// CountsKnown covers Mirrored AND Answered together, because they are only
	// ever said together, as a ratio ("N of M mirrored devices have a stored
	// first_seen"). A ratio with one half missing is not a number an operator can
	// act on, so one flag gates the pair rather than tempting a caller to print
	// half of it.
	CountsKnown bool
	Pending     DeviceFirstSeenBackfill
	// PendingKnown is false when the seed the next open would perform could not be
	// planned. An empty Pending is then NOT "the boot will seed nothing" — it is
	// "this check does not know what the boot will seed", which is the opposite
	// reading of the most consequential thing a boot does.
	PendingKnown bool
	// Unverified counts the rows whose origin is NOT `planted` — the ones no
	// surface may render as an exact instant. It is carried rather than recounted
	// by each caller so the headline number and the listing cannot disagree. It
	// counts over Rows, so it inherits RowsComplete: a zero under an incomplete
	// listing means "none among the rows that were read".
	Unverified int
}

// backfillDeviceFirstSeen seeds the ledger from the column it takes over, once,
// at the upgrade that introduces it, and REPORTS what it did.
//
// A box that has been running since before this table existed holds 61 rows
// whose first_seen is the wrong value — the last relay boot, on the relay's
// clock. Wrong is not the same as worthless: it is still a real instant at which
// this side demonstrably held a report of that device, and it is EARLIER than
// anything a post-upgrade report can produce, since the first report after an
// upgrade can only say "seen now". Letting the ledger start empty would
// therefore restamp every device on the site as new at the moment of the
// upgrade — which is defect #196 one last time, committed by its own fix, and
// exactly the "solve a data problem by discarding the data" move never-wipe
// forbids.
//
// # What an adopted value actually attests, and why that is not what it looks like
//
// It was read off a different clock (the reporting relay's) at a different event
// (that relay's last restart), so it is not an instant anything on this side
// observed, and two backfilled values are not even comparable with each other,
// let alone with a planted one. Usually it is a lower bound on the device's age
// — "this side held a report of this device no later than X" — and it is NOT
// reliably even that: see the bounds section below for the direction in which it
// can overstate instead. The console rendered it as an exact age ("3 days ago")
// and the device list SORTS on it, which claims a precision and a comparability
// the number does not have.
//
// That is a real defect and it is NOT repaired by refusing the rescue. Refusing
// restamps every device as new at the upgrade instant, which is strictly further
// from the truth in the same direction — on .12, 57 of 64 rows share one instant
// to the millisecond, so refusing would swap 57 identical wrong values for 64
// identical wrong values three and a half hours later, recovering no information
// at all. The rescue is the tighter of the two wrong answers. What was missing
// was not a better value but the three things that now ride with it: the row's
// own `origin` (this file's header), which is what finally lets a reader tell a
// rescued estimate from a witnessed instant instead of inferring it from a log
// line; an account of the rescue an operator can read
// (reportDeviceFirstSeenBackfill, and the `-store-check` listing); and a way to
// retire a row that is provably wrong (RetireDeviceFirstSeen).
//
// # The bounds, and the one direction they still do not cover
//
// The two bounds are what stop the rescue importing a lie. The old column was
// written from an unattested relay clock (see the header), and a relay whose
// clock had never been set wrote a near-epoch value that plant-once could then
// never correct — the console would read "20833d ago" for that device forever,
// on exactly the device population this backfill exists to rescue. So a value
// this side could not have observed is not adopted: not one below
// minPlausibleInstantMs, and not one in the FUTURE of the app's own clock, which
// is what a relay running ahead produced and which would render as a negative
// age. A device whose old value is refused is not lost — it simply has no answer
// until the next report plants one.
//
// Both bounds are ABSOLUTE, and that leaves one direction open, stated here
// rather than papered over: a relay whose clock ran BACKWARD but plausibly — the
// fake-hwclock-on-an-RTC-less-Pi case this file's header analyses and rejects for
// the duration draft — wrote a value earlier than this side's true first hold,
// and the rescue adopts it as an age OVERSTATEMENT. Nothing here can detect that,
// because the only instant that could bound it from below is one this deployment
// can attest about ITSELF, and every candidate for that (the org-root row's
// created_at, the schema-epoch stamp) is younger than the rows a RESTORE
// legitimately carries — so a floor built on one would refuse real history, which
// is the worse error and the one never-wipe names. It is left uncovered on
// purpose, and the escape hatch is what makes that acceptable: an overstatement
// is precisely the shape RetireDeviceFirstSeen repairs, since retiring can only
// move a value forward.
//
// # Mechanism
//
// Plan, apply, verify, report — the shape #194 established for the column pass
// rather than a fifth style. The plan is taken first and classified in Go so the
// report can name every device and every refusal reason; the existing set-based
// INSERT then does the work unchanged; and the plan is re-taken afterwards as the
// proof that what was planned is what landed.
//
// `ON CONFLICT DO NOTHING` makes it a one-time backfill that runs at every open
// and does nothing on all but the first: after the fix,
// discovered_devices.first_seen IS the ledger's own projection, so there is
// normally never a mirror row whose device the ledger has not already heard of.
// "Normally" is doing real work there and is why this stays a plan rather than a
// one-shot flag: a plant on an unusable clock leaves the projection holding a
// pre-ledger value with no ledger row behind it (ReplaceDiscoveredDevices keeps
// it deliberately), so the rescue can legitimately fire at a LATER open than the
// upgrade one.
func (s *Store) backfillDeviceFirstSeen(ctx context.Context) (DeviceFirstSeenBackfill, error) {
	now := s.nowMs()
	plan, err := planDeviceFirstSeenBackfill(ctx, s.db, now)
	if err != nil {
		return DeviceFirstSeenBackfill{}, err
	}
	if len(plan.Adopted) == 0 {
		// Nothing to insert. Refusals are still carried out to the report: a run
		// that refused every candidate and a run that had no candidates are
		// different facts, and the whole point of this pass reporting at all is
		// that they used to produce identical output.
		return plan, nil
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO device_first_seen (device_id, first_seen, origin)
		   SELECT device_id, first_seen, ? FROM discovered_devices
		    WHERE first_seen >= ? AND first_seen <= ?
		 ON CONFLICT(device_id) DO NOTHING`,
		FirstSeenAdopted, minPlausibleInstantMs, now); err != nil {
		return DeviceFirstSeenBackfill{}, fmt.Errorf("store: seed the device first-seen ledger: %w", err)
	}
	// The verification, taken against the same clock reading so the second plan
	// judges the FUTURE bound exactly as the first did. Anything still planned for
	// adoption did not land; by construction that list is empty, and it is asserted
	// rather than assumed because this is the one act in the whole open that
	// nothing afterwards can revise.
	after, err := planDeviceFirstSeenBackfill(ctx, s.db, now)
	if err != nil {
		return DeviceFirstSeenBackfill{}, err
	}
	plan.Unlanded = after.Adopted
	return plan, nil
}

// planDeviceFirstSeenBackfill decides what the seed would take and what it would
// refuse, and writes nothing. It is the single source of the plan for the boot's
// own write, for that write's verification, and for the read-only `-store-check`
// report — so what an operator is shown before a restart is computed by the code
// that later acts, which is the property planSchemaColumns exists to hold for
// columns.
//
// A mirror row whose column is zero or negative is not a candidate at all and
// appears in neither list. Zero is the ABSENT answer in this projection, not a
// bad one — it is what a plant on an unusable clock leaves behind, and what
// RetireDeviceFirstSeen leaves behind on purpose — so reporting it as "refused:
// below the plausibility floor" would put a line in every boot log for every
// device an operator had deliberately retired, forever.
//
// It tolerates the ledger table being absent, which is not a hypothetical: the
// table is created by applySchemaDDL, and `-store-check` reads a store that has
// not been opened for real yet. In that state every candidate mirror row is
// pending, which is exactly what the next boot will conclude a moment after it
// creates the table.
func planDeviceFirstSeenBackfill(ctx context.Context, q queryer, nowMs int64) (DeviceFirstSeenBackfill, error) {
	out := DeviceFirstSeenBackfill{AtMs: nowMs}

	mirror, err := tableExists(ctx, q, "discovered_devices")
	if err != nil {
		return DeviceFirstSeenBackfill{}, err
	}
	if !mirror {
		return out, nil
	}
	ledger, err := tableExists(ctx, q, "device_first_seen")
	if err != nil {
		return DeviceFirstSeenBackfill{}, err
	}

	// Ordered by device_id so two readings of an unchanged store are byte-identical
	// — the same reason planSchemaColumns walks its tables sorted, and what makes
	// the boot log and `-store-check` comparable between runs.
	query := `SELECT d.device_id, d.first_seen FROM discovered_devices d
	           WHERE d.first_seen > 0 ORDER BY d.device_id`
	if ledger {
		query = `SELECT d.device_id, d.first_seen FROM discovered_devices d
		           LEFT JOIN device_first_seen l ON l.device_id = d.device_id
		          WHERE l.device_id IS NULL AND d.first_seen > 0
		          ORDER BY d.device_id`
	}
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return DeviceFirstSeenBackfill{}, fmt.Errorf("store: plan the device first-seen seed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id    string
			value int64
		)
		if err := rows.Scan(&id, &value); err != nil {
			return DeviceFirstSeenBackfill{}, fmt.Errorf("store: plan the device first-seen seed: %w", err)
		}
		switch {
		case value < minPlausibleInstantMs:
			out.Refused = append(out.Refused, FirstSeenRefusal{DeviceID: id, FirstSeen: value, Reason: fmt.Sprintf(
				"below the plausibility floor %d (2020-09-13): the relay that wrote it had a clock that was never set",
				minPlausibleInstantMs)})
		case value > nowMs:
			out.Refused = append(out.Refused, FirstSeenRefusal{DeviceID: id, FirstSeen: value, Reason: fmt.Sprintf(
				"in the future of this box's own clock (%d): the relay that wrote it was running ahead, and the age would render negative",
				nowMs)})
		default:
			out.Adopted = append(out.Adopted, FirstSeenAdoption{DeviceID: id, FirstSeen: value})
		}
	}
	if err := rows.Err(); err != nil {
		return DeviceFirstSeenBackfill{}, fmt.Errorf("store: plan the device first-seen seed: %w", err)
	}
	return out, nil
}

// reportDeviceFirstSeenBackfill is the boot log's account of the seed, on the
// same terms as the column migration's and the stored-row fault report's
// (schemamigrate.go, priorfaults.go): silent when nothing happened, and otherwise
// said once, at open, naming every device.
//
// It exists because the seed was completely silent — a run that rescued all 64
// rows on box .12 and a run that refused all 64 produced byte-identical output,
// which is zero bytes. That is the #191/#194 lesson reappearing inside a
// brand-new mechanism, in the one place where the outcome is irreversible: an
// adopted value is frozen as a lower bound and a refused device has no age at all
// until a fresh report plants one.
//
// Per-device lines rather than counts alone, and the cap on them is now safe to
// have. It was not before. The previous version capped adoptions at twenty and
// pointed the operator at `waiveo-feeder -store-check` "to list the ledger in
// full" — a command that issued no SELECT over the ledger's rows and whose
// pending list is empty precisely because the boot succeeded. With no provenance
// column, that truncated log was by its own admission the ONLY record that a
// device's age came from the pre-ledger column, so on box .12's 64 adoptions
// forty-four devices had no record anywhere. The `origin` column is what makes
// this log an account rather than the archive: every adoption is durably marked
// `adopted` in the row itself, `-store-check` lists them all, and the api/1
// Device carries the same fact to the console. The referral below is true now
// because there is something behind it.
func reportDeviceFirstSeenBackfill(dsn string, m DeviceFirstSeenBackfill) {
	if m.empty() {
		// The genuinely-nothing-happened case: every boot after the first, and
		// every fresh install. It is the only silence this function allows.
		return
	}
	// Bounded for the reason every other per-item report in this store is bounded:
	// a box with a thousand mirrored devices must not scroll the rest of startup
	// off the screen. Both referrals below name what actually lists the rest —
	// adoptions are durable rows `-store-check` enumerates, refusals stay pending
	// forever and are enumerated as pending.
	//
	// Every referral below NAMES the store. A referral that does not is one an
	// operator follows against whatever file their shell's cwd resolves — the
	// check took no path argument at all until #195 was fixed, so these lines
	// prescribed a command that could not be aimed at the store they are about.
	const shown = 20
	for i, a := range m.Adopted {
		if i == shown {
			stdlog.Printf("store: … and %d more adopted, each marked origin=%s in the ledger; `waiveo-feeder "+
				"-store-check %s` lists every ledger row and its origin", len(m.Adopted)-shown, FirstSeenAdopted, dsn)
			break
		}
		stdlog.Printf("store: adopted first_seen %d for device %s from the pre-ledger column (origin=%s) — %s",
			a.FirstSeen, a.DeviceID, FirstSeenAdopted, UnverifiedFirstSeenSentence)
	}
	for i, r := range m.Refused {
		if i == shown {
			stdlog.Printf("store: … and %d more refused; `waiveo-feeder -store-check %s` lists them in full",
				len(m.Refused)-shown, dsn)
			break
		}
		stdlog.Printf("store: refused first_seen %d for device %s: %s; the device reads as having NO age — the "+
			"value stays on the file and is named by every `waiveo-feeder -store-check %s` until a report on a "+
			"working clock plants a real one", r.FirstSeen, r.DeviceID, r.Reason, dsn)
	}
	stdlog.Printf("store: seeded the device first-seen ledger in %s: %d of %d mirrored device(s) adopted from the "+
		"pre-ledger column (origin=%s), %d refused as implausible; an adopted value is %s, and nothing moves it "+
		"afterwards except an operator retiring the row",
		dsn, len(m.Adopted), len(m.Adopted)+len(m.Refused), FirstSeenAdopted, len(m.Refused), UnverifiedFirstSeenSentence)
	if len(m.Unlanded) == 0 {
		return
	}
	stdlog.Printf("store: WARNING — %d of the %d planned adoption(s) did NOT land in %s and the ledger disagrees "+
		"with the plan that judged it; those devices carry no age, and the next open will plan them again",
		len(m.Unlanded), len(m.Adopted), dsn)
	for i, a := range m.Unlanded {
		if i == shown {
			stdlog.Printf("store: … and %d more that did not land", len(m.Unlanded)-shown)
			break
		}
		stdlog.Printf("store: first_seen %d for device %s was planned and is still not in the ledger",
			a.FirstSeen, a.DeviceID)
	}
}

// InspectDeviceFirstSeen reports the ledger's census, ITS CONTENTS, and the seed
// the NEXT open would perform, reading and writing nothing beyond the queries.
//
// It is what `-store-check` prints, and it closes the second half of the same
// silence: on box .12 that check named the pending relay_last_seen COLUMN and
// never the pending device_first_seen TABLE, because the column pass passes over
// an absent table by design. Asked here, the ledger answers for itself.
//
// The row LISTING is what makes the boot log's truncation referral true. Without
// it this call answered a question nobody asked — three counts — while the one
// sentence in the boot log that pointed here promised the rows, and the rows were
// exactly what an operator needs: `origin` is what says which of them may be
// rendered as an instant and which are candidates for a retire.
//
// # Every query is ASKED, and each answer says whether it arrived (#199)
//
// Every failure here used to return `DeviceFirstSeenLedger{}` — Present,
// Mirrored, Answered, Unverified and the whole PENDING seed plan discarded
// together with the one query that failed. On the pre-upgrade shape
// (device_first_seen present, `origin` still pending) that turned the section
// the boot log's truncation referral points AT into a single error line, and
// `-store-check` exited 0 over it.
//
// Making the listing column-aware fixed that ONE shape. Reordering the queries so
// the listing ran last did not fix the posture, it only chose a different half to
// throw away: a store whose pre-ledger `first_seen` column is gone, or holds a
// value that will not scan, fails the seed PLAN — and the early return then
// discarded the listing, which is the very half the boot log's referral names
// ("`waiveo-feeder -store-check <store>` lists every ledger row and its origin").
// One arrangement lost the irreversible-seed plan, the other lost the rows.
//
// So there is no early return left. Every query is attempted, whatever the ones
// before it did; each answer sets its own `…Known` flag; and the failures come
// back joined, in one error, naming every gap. Order is now cosmetic — nothing
// downstream of a failure is skipped — which is the only arrangement under which
// this call cannot be made to lose a half by choosing the right fixture.
func (s *Store) InspectDeviceFirstSeen(ctx context.Context) (DeviceFirstSeenLedger, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var (
		out  DeviceFirstSeenLedger
		errs []error
	)
	if present, err := tableExists(ctx, s.db, "device_first_seen"); err != nil {
		errs = append(errs, fmt.Errorf("store: look for the device first-seen ledger table: %w", err))
	} else {
		out.Present, out.PresentKnown = present, true
	}

	switch mirror, err := tableExists(ctx, s.db, "discovered_devices"); {
	case err != nil:
		errs = append(errs, fmt.Errorf("store: look for the discovered-device mirror table: %w", err))
	case !mirror:
		// No mirror at all: nothing to answer for, and 0-of-0 IS the answer rather
		// than a gap.
		out.CountsKnown = true
	default:
		counted := true
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM discovered_devices`).Scan(&out.Mirrored); err != nil {
			errs = append(errs, fmt.Errorf("store: count the discovered-device mirror: %w", err))
			counted = false
		}
		switch {
		case !out.PresentKnown:
			// Whether the ledger table exists is unknown, so how much of the mirror
			// it answers for is unknowable — a zero here would read as "nothing is
			// answered", which is a different fact entirely.
			counted = false
		case out.Present:
			if err := s.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM discovered_devices d
				   JOIN device_first_seen l ON l.device_id = d.device_id`).Scan(&out.Answered); err != nil {
				errs = append(errs, fmt.Errorf("store: count the answered mirror rows: %w", err))
				counted = false
			}
		}
		out.CountsKnown = counted
	}

	if pending, err := planDeviceFirstSeenBackfill(ctx, s.db, s.nowMs()); err != nil {
		errs = append(errs, err)
	} else {
		out.Pending, out.PendingKnown = pending, true
	}

	if out.Present {
		rows, err := listDeviceFirstSeen(ctx, s.db)
		out.Rows = rows
		if err != nil {
			errs = append(errs, err)
		} else {
			out.RowsComplete = true
		}
		for _, r := range out.Rows {
			if !FirstSeenIsObserved(r.Origin) {
				out.Unverified++
			}
		}
	}
	return out, errors.Join(errs...)
}

// listDeviceFirstSeen reads every ledger row in device-id order, so two readings
// of an unchanged store are byte-identical and an operator can diff them — the
// same reason planDeviceFirstSeenBackfill sorts.
//
// # `origin` is PROBED, not assumed (#199)
//
// The column arrived after the table did, so there is a real store shape — the
// one an operator inspects immediately before that upgrade — where the table is
// there and the column is not. Selecting `origin` unconditionally made this
// query fail with "no such column: origin" on exactly that shape, and the
// failure took the entire ledger section with it while the check still exited 0.
//
// A store missing the column reads every row with origin `unrecorded`, which is
// what a blank means everywhere else in this file (firstSeenOrigin) and is the
// truthful answer: nothing recorded where those values came from, because the
// column that records it does not exist yet. The pending column is named by the
// schema section of the same report, so the operator is told both halves.
//
// # One unreadable row costs that row, and nothing else
//
// The scan loop used to `return nil, err` on the first row it could not read,
// which threw away every row already scanned — so a ledger of two hundred rows
// whose two-hundredth holds a pre-ledger string listed NONE of them. That is the
// same "lose the whole answer to one bad row" shape the caller was just fixed
// for, one level down, and it defeats the fix above it: a caller that faithfully
// prints what it was handed still prints nothing.
//
// So a row that will not scan is counted and skipped, the rest are listed, and
// the returned error names how many were lost. Rows that WERE read are always
// returned, error or not; the caller is told the listing is incomplete by the
// error, never by a short slice it cannot distinguish from an empty ledger.
func listDeviceFirstSeen(ctx context.Context, q queryer) ([]DeviceFirstSeenRow, error) {
	hasOrigin, err := columnExists(ctx, q, "device_first_seen", "origin")
	if err != nil {
		return nil, fmt.Errorf("store: list the device first-seen ledger: %w", err)
	}
	originExpr := `''`
	if hasOrigin {
		originExpr = `origin`
	}
	rows, err := q.QueryContext(ctx,
		`SELECT device_id, first_seen, `+originExpr+` FROM device_first_seen ORDER BY device_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list the device first-seen ledger: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		out      []DeviceFirstSeenRow
		unread   int
		firstErr error
	)
	for rows.Next() {
		var (
			r      DeviceFirstSeenRow
			origin string
		)
		if err := rows.Scan(&r.DeviceID, &r.FirstSeen, &origin); err != nil {
			unread++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		r.Origin = firstSeenOrigin(origin)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: list the device first-seen ledger: %w", err)
	}
	if unread > 0 {
		return out, fmt.Errorf("store: list the device first-seen ledger: %d row(s) could not be read and are missing from the listing; the first: %w",
			unread, firstErr)
	}
	return out, nil
}

// RetireDeviceFirstSeen clears one device's stored first_seen — the ledger row
// AND the mirror projection of it — so the next report this site stores plants a
// fresh one from the app's own clock.
//
// It is the escape hatch plant-once shipped without (see this file's header), and
// it is deliberately the smallest one that works: it retires a value, it cannot
// set one, and it acts on a single named device. There is no bulk arm, because
// the wrong values this exists for are wrong INDIVIDUALLY — the operator has
// evidence about one device, not a rule about all of them — and a bulk retire on
// a site whose ledger is mostly rescued lower bounds would restamp the whole
// inventory as new, which is defect #196's exact shape wearing a repair's name.
//
// # Both copies, in one transaction, or the retire is a no-op that looks like a fix
//
// This is the part a naive version gets wrong, and it is worth the paragraph.
// discovered_devices.first_seen is this ledger's PROJECTION and it holds the same
// value. Deleting only the ledger row leaves it there, and two separate paths put
// it straight back:
//
//   - the very next store.Open re-runs backfillDeviceFirstSeen, whose plan now
//     MATCHES this device precisely because the ledger row is gone, and re-adopts
//     the value that was just retired — permanently. That is on its own decisive:
//     a retire that left the column would be undone by the next restart, and
//     would come back marked `adopted` as though it had been rescued.
//   - it is also what keeps the two copies from disagreeing at all. Readers take
//     the age through the ledger now (readDiscoveredDevices' join), so a leftover
//     column value would no longer be SERVED — but it would still be judged, and
//     re-adopting it is the same wrong answer arriving one boot later.
//
// So both go, together, under one transaction — and the caller must also clear
// the RUNNING process's in-memory copy (devices.Registry.ForgetFirstSeen), which
// is the third place the value lives and the only one a database write cannot
// reach.
//
// last_seen is deliberately untouched. It is a different fact, answered by a
// different rule (ReplaceDiscoveredDevices' change-detector), and a retire that
// blanked it would tell an operator a device that reported a minute ago has never
// been heard from.
//
// Idempotent: retiring a device with no stored answer is a no-op returning
// retired=false, the same contract UnignoreDevice keeps. The id is not checked
// against the mirror — a device that is currently powered off is exactly the one
// whose frozen age an operator may want cleared.
func (s *Store) RetireDeviceFirstSeen(ctx context.Context, deviceID string) (retired bool, err error) {
	if deviceID == "" {
		return false, fmt.Errorf("store: RetireDeviceFirstSeen: device_id must not be empty")
	}
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM device_first_seen WHERE device_id = ?`, deviceID)
		if err != nil {
			return fmt.Errorf("store: RetireDeviceFirstSeen: delete %s: %w", deviceID, err)
		}
		fromLedger, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: RetireDeviceFirstSeen: rows affected %s: %w", deviceID, err)
		}
		// Narrowed to a row that actually carries a value, so RowsAffected answers
		// "was there something to retire" rather than "does the device have a mirror
		// row" — which is what makes a second retire report false rather than true.
		res, err = tx.ExecContext(ctx,
			`UPDATE discovered_devices SET first_seen = 0 WHERE device_id = ? AND first_seen != 0`, deviceID)
		if err != nil {
			return fmt.Errorf("store: RetireDeviceFirstSeen: clear the projection of %s: %w", deviceID, err)
		}
		fromMirror, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: RetireDeviceFirstSeen: rows affected %s: %w", deviceID, err)
		}
		// Either copy counts. A pre-ledger store can hold the projection with no
		// ledger row behind it at all, and clearing that IS the retirement — it is
		// also the only way to stop the next open re-adopting it.
		retired = fromLedger > 0 || fromMirror > 0
		// Deliberately no bumpGeneration: this fact reaches no relay and compiles
		// into no desired state, exactly as the ignore decision beside it does not.
		return nil
	})
	if err != nil {
		return false, err
	}
	return retired, nil
}

// plantDeviceFirstSeen records that this site now holds a report of deviceID,
// and returns the instant that stands for it — the value the mirror row is then
// written with.
//
// It runs inside the caller's transaction (ReplaceDiscoveredDevices' own), which
// is what makes the ledger and the projection commit together and what keeps two
// relays' reports from interleaving a read against another's write.
//
// One statement, and it has no update arm at all: `ON CONFLICT DO NOTHING` is
// the rule "the first answer is the answer" expressed where SQLite enforces it,
// rather than as a read-modify-write this code would have to get right under
// concurrency. The follow-up SELECT reads what stands rather than assuming the
// write won, since on the common path (a device already known, reported again a
// minute later) it deliberately did not.
//
// A zero return means the site has no answer for this device: the app's clock is
// not yet usable and nothing was planted (see minPlausibleInstantMs). The
// callers treat that as absent — the API omits the member, the console renders
// an em dash — rather than as an instant, and the next report on a working clock
// plants the real value.
//
// The ORIGIN comes back with the value rather than being assumed by the caller,
// and that matters on the common path: this function usually plants nothing,
// because the row is already there, and what stands may be an `adopted` estimate
// from the upgrade rather than the `planted` instant a caller would otherwise
// infer from having just called a function named plant. The SELECT reads both
// columns for the same reason it reads the value at all — what stands is the
// answer, not what this statement tried to write.
func plantDeviceFirstSeen(ctx context.Context, tx *sql.Tx, deviceID string, appNowMs int64) (int64, string, error) {
	if appNowMs >= minPlausibleInstantMs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO device_first_seen (device_id, first_seen, origin) VALUES (?, ?, ?)
			 ON CONFLICT(device_id) DO NOTHING`,
			deviceID, appNowMs, FirstSeenPlanted); err != nil {
			return 0, "", fmt.Errorf("store: plant first_seen of %s: %w", deviceID, err)
		}
	}
	var (
		stored int64
		origin string
	)
	switch err := tx.QueryRowContext(ctx,
		`SELECT first_seen, origin FROM device_first_seen WHERE device_id = ?`, deviceID).Scan(&stored, &origin); {
	case err == nil:
		return stored, firstSeenOrigin(origin), nil
	case err == sql.ErrNoRows:
		// Nothing planted and nothing held: the clock was unusable and this
		// device has no history yet. Zero is the absent answer, not an instant,
		// and an absent answer has no origin to report.
		return 0, "", nil
	default:
		return 0, "", fmt.Errorf("store: read planted first_seen of %s: %w", deviceID, err)
	}
}
