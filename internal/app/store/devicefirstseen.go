package store

import (
	"context"
	"database/sql"
	"fmt"
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
// the same transaction. Every existing reader (the boot restore, adoption, the
// -store-check diagnostic) is untouched, and the sole writer of both copies is
// ReplaceDiscoveredDevices, which is what keeps them from drifting.
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
// # Nothing deletes from this table, and nothing updates it
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
// Growth is bounded in practice and tiny in absolute terms: one row of roughly
// fifty bytes per DISTINCT device identity the site has ever seen, against a
// relay that will not hold more than maxStoredCandidates (1024) at a time.
const deviceFirstSeenSchema = `
CREATE TABLE IF NOT EXISTS device_first_seen (
	device_id  TEXT PRIMARY KEY,
	first_seen INTEGER NOT NULL
);
`

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

// backfillDeviceFirstSeen seeds the ledger from the column it takes over, once,
// at the upgrade that introduces it.
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
// `ON CONFLICT DO NOTHING` makes it a one-time backfill that runs at every open
// and does nothing on all but the first: after the fix,
// discovered_devices.first_seen IS the ledger's own projection, so there is
// never a mirror row whose device the ledger has not already heard of.
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
func (s *Store) backfillDeviceFirstSeen(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO device_first_seen (device_id, first_seen)
		   SELECT device_id, first_seen FROM discovered_devices
		    WHERE first_seen >= ? AND first_seen <= ?
		 ON CONFLICT(device_id) DO NOTHING`,
		minPlausibleInstantMs, s.nowMs()); err != nil {
		return fmt.Errorf("store: seed the device first-seen ledger: %w", err)
	}
	return nil
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
func plantDeviceFirstSeen(ctx context.Context, tx *sql.Tx, deviceID string, appNowMs int64) (int64, error) {
	if appNowMs >= minPlausibleInstantMs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO device_first_seen (device_id, first_seen) VALUES (?, ?)
			 ON CONFLICT(device_id) DO NOTHING`,
			deviceID, appNowMs); err != nil {
			return 0, fmt.Errorf("store: plant first_seen of %s: %w", deviceID, err)
		}
	}
	var stored int64
	switch err := tx.QueryRowContext(ctx,
		`SELECT first_seen FROM device_first_seen WHERE device_id = ?`, deviceID).Scan(&stored); {
	case err == nil:
		return stored, nil
	case err == sql.ErrNoRows:
		// Nothing planted and nothing held: the clock was unusable and this
		// device has no history yet. Zero is the absent answer, not an instant.
		return 0, nil
	default:
		return 0, fmt.Errorf("store: read planted first_seen of %s: %w", deviceID, err)
	}
}
