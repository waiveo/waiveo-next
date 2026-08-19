package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// discovereddevices.go persists what the relays have FOUND — the app peer's
// durable mirror of relay/1 REL-110/111's `device.candidates` reports — and
// turns one of those findings into the durable adoption record that actually
// puts a device under this platform's control.
//
// # Why a mirror exists at all
//
// internal/app/devices makes the case for holding the live view in memory: the
// relay is authoritative for its own LAN and re-reports the whole set on every
// connection, so a second copy on this side is a staler authority for data that
// arrives again anyway. That argument is sound about AUTHORITY and wrong about
// AVAILABILITY, and the difference bites at exactly one moment — a restart.
//
// A feeder that has just come up has heard from nobody. Its device list is
// empty, and stays empty until each relay reconnects and reports, which is
// seconds on a healthy box and unbounded on a box whose relay is down, whose
// LAN is partitioned, or which is being worked on precisely because something is
// broken. During that window an operator's device page shows nothing at all —
// not "stale", not "last seen 3 minutes ago", nothing — and the console offers
// no way to tell "no devices found" apart from "the process restarted". Legacy
// kept a device table for this reason and nobody ever thought of it as a second
// authority.
//
// So the rows here are a CACHE with provenance, never an authority:
//
//   - a relay's report REPLACES that relay's rows wholesale, exactly as it
//     replaces its in-memory view, so the mirror can never drift into claiming a
//     device the relay has stopped reporting;
//   - `last_seen` rides every row, so a reader can tell how old the answer is;
//   - nothing derived from these rows is signed, shipped to a relay, or used in
//     an authorization decision. Desired state is compiled from the adopted
//     rows, which are authored, not from these.
//
// # Why the writes do NOT bump the generation
//
// Every other write in this store advances the desired-state generation, which
// nudges every connected relay to re-pull a snapshot (REL-057). A discovery
// mirror must not: candidate reports arrive on a periodic cadence for as long as
// the box is up, so bumping here would make every relay on the site re-pull and
// re-verify a signed snapshot every minute forever, for a change that alters no
// byte of desired state. The rows are deliberately outside that seam.

// discoveredDevicesSchema is the mirror table. It is keyed by the DERIVED
// device_id (internal/shared/deviceid) rather than by the relay's own identity
// tuple, because that id is what every other plane addresses the device by — the
// list surface, the adopt operation, and the adopted row this table promotes a
// device into all name the same value.
//
// `relay_id` is a plain column rather than part of the key on purpose: one
// device is one row no matter how many relays can see it, and REL-153's
// incumbency rule (internal/app/devices) has already decided which relay speaks
// for it by the time a row is written here.
const discoveredDevicesSchema = `
CREATE TABLE IF NOT EXISTS discovered_devices (
	device_id    TEXT PRIMARY KEY,
	relay_id     TEXT NOT NULL,
	scope_node   TEXT NOT NULL,
	driver       TEXT NOT NULL,
	native_id    TEXT NOT NULL,
	device_class TEXT NOT NULL,
	name         TEXT NOT NULL DEFAULT '',
	address      TEXT NOT NULL DEFAULT '',
	model        TEXT NOT NULL DEFAULT '',
	serial       TEXT NOT NULL DEFAULT '',
	first_seen   INTEGER NOT NULL,
	last_seen    INTEGER NOT NULL,
	entities     TEXT NOT NULL DEFAULT '[]',
	open_ports   TEXT NOT NULL DEFAULT '[]',
	relay_last_seen INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS discovered_devices_relay ON discovered_devices (relay_id);
`

// DiscoveredDevice is one mirrored sighting: the derived device id, the relay
// that reported it, the REL-153 identity tuple it was reported under, and the
// discovered facts an operator needs to recognize the thing and this platform
// needs to reach it.
//
// Entities is the addressable fan-out the reporting relay declared (REL-110a).
// It is stored because adoption consumes it: an adopted-device row lists the
// entities whose policy it carries (REL-063), and after a restart the relay's
// report may not have arrived yet when an operator clicks adopt.
type DiscoveredDevice struct {
	DeviceID    string
	RelayID     string
	ScopeNode   string
	Driver      string
	NativeID    string
	DeviceClass string
	Name        string
	Address     string
	Model       string
	Serial      string
	// FirstSeen and LastSeen are on the APP's clock (SEC-066), never the
	// reporting relay's: when this site first held a report of the device, and
	// when the relay was last observed to have seen it. devicefirstseen.go says
	// why a relay's own numbers are not read, and ReplaceDiscoveredDevices says
	// how the second one is decided.
	//
	// FirstSeen is THE LEDGER'S answer, read through it rather than off the
	// mirror column — see readDiscoveredDevices for why those are not always the
	// same number and why every reader must get this one.
	FirstSeen int64
	LastSeen  int64
	// FirstSeenOrigin says where FirstSeen came from (store.FirstSeenPlanted /
	// Adopted / Unrecorded), and is empty exactly when FirstSeen is zero. A reader
	// that renders the instant without it is claiming an observation it may not
	// have: devicefirstseen.go's header has the vocabulary.
	FirstSeenOrigin string
	// MirroredFirstSeen is the raw `discovered_devices.first_seen` column, which
	// equals FirstSeen on every row the ledger answers for and differs on exactly
	// one population: a pre-ledger value the backfill REFUSED as implausible,
	// which stays on the file (never-wipe) with no ledger row behind it.
	//
	// It exists for one caller — ReplaceDiscoveredDevices' never-wipe
	// carry-forward, which must not blank that column while the app's clock is
	// still unusable — and no reader outside this package should want it. Using
	// it as a device's age is the defect this split exists to make impossible.
	MirroredFirstSeen int64
	// RelayLastSeen is the raw `last_seen` the reporting relay put on the wire,
	// kept for ONE purpose and never rendered: comparing it with the next
	// report's tells us whether the relay actually re-observed the device.
	// It is an opaque change-detector, not a time — see ReplaceDiscoveredDevices.
	RelayLastSeen int64
	// OpenPorts is what an active scan found listening. Mirrored so a restart
	// does not blank the column until the next scan — the same reason every
	// other discovered fact is here.
	OpenPorts []int
	Entities  []wire.CandidateEntity
}

// ReplaceDiscoveredDevices makes rows the complete mirrored set for relayID,
// deleting whatever that relay's previous report left behind — the same
// full-set-replace semantics REL-111 gives the report itself, applied to the
// durable copy so the two can never disagree about what a relay currently sees.
//
// It returns the rows AS STORED, which is not what was passed in: the durable
// merge below fills in facts the report did not carry and stamps both seen
// instants on this side's clock. The caller projects those onto the in-memory
// read model (cmd/waiveo-feeder), so the value an operator is shown is the one
// that is actually on disk rather than a second computation of it.
//
// # The two seen instants are this side's, and `last_seen` is not "now"
//
// Both are stamped from the app's own clock, for the reason devicefirstseen.go
// gives at length: a relay's timestamps come off an unattested wall clock, and
// relay/1's own `clock_state` for judging that clock is hardcoded `untrusted` in
// every live relay because the verified-time source is still unbuilt.
//
// `first_seen` is planted once and never moves. `last_seen` has to keep moving,
// and the honest question behind it is not "when did a report mentioning this
// device arrive" — the relay re-sends its whole candidate set every minute and
// internal/relay/deviceplane never expires a candidate, so a device unplugged a
// week ago is still in every report, unchanged. Dating it "now" would make the
// column say a dark device is live, which is the exact opposite of what an
// operator reads it for.
//
// So the report's own `last_seen` is used, but never as a TIME: only compared
// with the one the previous report carried for the same device. That comparison
// needs no trusted clock and no shared clock, because it asks a yes/no question
// about a number the relay controls end to end — did it CHANGE? The relay
// advances a candidate's `last_seen` only when a lane actually re-observed the
// device (deviceplane.Store.Observe), so:
//
//   - the stamp changed, or the device is new here, or the relay restarted and
//     re-minted its candidates ⇒ it was genuinely seen, and this side stamps the
//     moment it learned so;
//   - the stamp is byte-identical to last time ⇒ the relay is replaying a frozen
//     candidate, has not seen the device since, and the durable answer stays
//     exactly where it was.
//
// That makes `last_seen` freeze the moment a device goes dark, which is what
// makes "not heard from since" answerable, and it keeps `first_seen <= last_seen`
// structural rather than hopeful: both are readings of one clock, and the first
// is planted at the same instant the second is first written.
//
// The delete and the inserts share one transaction: a mirror that was briefly
// empty mid-refresh would be read as "this relay found nothing", which is a
// visible and alarming state to expose for a write that is not changing
// anything.
//
// A row whose DeviceID is empty is skipped rather than refusing the batch. The
// caller derives that id, so an empty one is a caller defect on ONE device, and
// failing the whole refresh over it would freeze the mirror for every other
// device the relay can see.
//
// # What the wholesale replace must NOT replace
//
// The mirror's rows are authored by a relay process whose memory dies at every
// restart, and a blind replace therefore writes that process's IGNORANCE over
// what this side already knew. `first_seen` was the visible case (see
// devicefirstseen.go), and it was not the only one: model and serial come only
// from an active identification probe whose cache is in relay memory, an entity's
// state comes only from the poller, and an open-port list comes only from a scan
// that runs on a pack's schedule. A restarted relay reports every one of them
// blank, and did so straight over the durable copy.
//
// So the replace MERGES against what is already stored, on exactly the rule
// internal/relay/deviceplane.Store.Observe already applies inside one relay
// process — "a scan that DID look replaces the list wholesale; an observation
// that did not look keeps what is known" — lifted from process memory to the
// durable row, where the lifetime finally matches the fact. The relay OBSERVES;
// this side REMEMBERS.
//
// # An EMPTY report does not empty the mirror
//
// A report carrying no rows for this relay leaves the relay's rows exactly as
// they are. A relay reports from its candidate store the moment a connection
// comes up (cmd/waiveo-relay's OnConnected), which on a cold host — an ARP table
// that has not warmed, a discovery sweep that has not finished — is empty
// through no fault of the LAN, and a full-set replace would read that as "this
// relay found nothing" and delete every durable fact about the site. "I have
// nothing to say" is not evidence that there is nothing there. ForgetDiscovered-
// Devices stays the only emptier, and it is called on REVOCATION, where an
// emptied mirror is exactly the intent.
func (s *Store) ReplaceDiscoveredDevices(ctx context.Context, relayID string, rows []DiscoveredDevice) ([]DiscoveredDevice, error) {
	if relayID == "" {
		return nil, fmt.Errorf("store: ReplaceDiscoveredDevices: relay_id must not be empty")
	}
	if len(rows) == 0 {
		// Nothing is deleted and nothing is written — see the header. Returning
		// no rows is the honest answer to "what did this report store": nothing
		// did, and the caller has no seen-instants to project.
		return nil, nil
	}
	stored := make([]DiscoveredDevice, 0, len(rows))
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		// Read BEFORE the delete, and across the whole mirror rather than this
		// relay's slice of it: REL-153 makes a device's identity independent of
		// the relay currently reporting it, so a device re-homed to a second
		// relay must carry its learned facts across with it rather than starting
		// again as an unknown.
		held, err := readDiscoveredDevices(ctx, tx, "")
		if err != nil {
			return err
		}
		prior := make(map[string]DiscoveredDevice, len(held))
		for _, d := range held {
			prior[d.DeviceID] = d
		}

		// One clock reading for the whole report, so every row a single report
		// stores carries the same instant and two devices in one batch are never
		// dated a query apart.
		now := s.nowMs()

		if _, err := tx.ExecContext(ctx, `DELETE FROM discovered_devices WHERE relay_id = ?`, relayID); err != nil {
			return fmt.Errorf("store: ReplaceDiscoveredDevices: clear relay %s: %w", relayID, err)
		}
		stored = stored[:0]
		for _, d := range rows {
			if d.DeviceID == "" {
				continue
			}
			was, known := prior[d.DeviceID]
			row := mergeDiscovered(was, d)
			row.RelayID = relayID
			// The relay's stamp as an opaque change-detector, never as a time —
			// see the header. Unchanged means the relay has not seen the device
			// since its last report, so the durable answer must not move.
			row.RelayLastSeen = d.LastSeen
			// A row this build has never written has no comparator — the column
			// defaults to 0 on the upgrade that adds it — so the first report
			// after an upgrade advances, which is right: that row's stored
			// last_seen came off the RELAY's clock and must be replaced by one of
			// ours, not carried forward as though it were ours.
			if known && was.LastSeen > 0 && was.RelayLastSeen > 0 && d.LastSeen == was.RelayLastSeen {
				row.LastSeen = was.LastSeen
			} else {
				row.LastSeen = now
			}
			// The durable ledger owns first_seen, and this report can only ever
			// teach it that the device exists — never when it was first seen.
			row.FirstSeen, row.FirstSeenOrigin, err = plantDeviceFirstSeen(ctx, tx, d.DeviceID, now)
			if err != nil {
				return err
			}
			// The COLUMN normally carries the ledger's answer, which is what makes
			// it the projection devicefirstseen.go says it is.
			row.MirroredFirstSeen = row.FirstSeen
			// Nothing was planted and nothing is held: the app's clock is not yet
			// usable (devicefirstseen.go). Keep whatever the column already had
			// rather than writing the projection's zero over it — on a box being
			// upgraded that column IS the pre-ledger history, and blanking it
			// would destroy the only copy before the backfill that rescues it has
			// ever run on a working clock. Never-wipe: refusing to answer must not
			// mean deleting the answer somebody else could still give.
			//
			// Only the COLUMN is carried forward, and the returned row's FirstSeen
			// stays zero. That split is the point: the pre-ledger number survives
			// on disk for the backfill to judge later, and it does not become this
			// device's age in the meantime. Carrying it into the returned row —
			// which is what the caller projects onto the read model — is how a
			// value nothing here vouches for used to reach `GET /devices`.
			if row.MirroredFirstSeen == 0 && was.MirroredFirstSeen > 0 {
				row.MirroredFirstSeen = was.MirroredFirstSeen
			}

			entities, err := json.Marshal(nonNilEntities(row.Entities))
			if err != nil {
				return fmt.Errorf("store: ReplaceDiscoveredDevices: encode entities of %s: %w", d.DeviceID, err)
			}
			ports, err := json.Marshal(nonNilPorts(row.OpenPorts))
			if err != nil {
				return fmt.Errorf("store: ReplaceDiscoveredDevices: encode open_ports of %s: %w", d.DeviceID, err)
			}
			// INSERT OR REPLACE, not INSERT: two relays can legitimately see one
			// device, and the caller has already applied REL-153 incumbency to
			// decide which one speaks for it. A raw INSERT would fail the whole
			// refresh on the loser's leftover row.
			if _, err := tx.ExecContext(ctx,
				`INSERT OR REPLACE INTO discovered_devices
				   (device_id, relay_id, scope_node, driver, native_id, device_class,
				    name, address, model, serial, first_seen, last_seen, entities, open_ports,
				    relay_last_seen)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				row.DeviceID, row.RelayID, row.ScopeNode, row.Driver, row.NativeID, row.DeviceClass,
				row.Name, row.Address, row.Model, row.Serial, row.MirroredFirstSeen, row.LastSeen,
				string(entities), string(ports), row.RelayLastSeen); err != nil {
				return fmt.Errorf("store: ReplaceDiscoveredDevices: insert %s: %w", d.DeviceID, err)
			}
			stored = append(stored, row)
		}
		// Deliberately no bumpGeneration — see this file's header.
		return nil
	})
	if err != nil {
		return nil, err
	}
	return stored, nil
}

// mergeDiscovered folds one reported row onto whatever the mirror already holds
// for that device, and returns the row to store.
//
// prior is the zero value when the device is new, which makes every rule below
// degrade to "take what was reported" with no special case: a blank held fact is
// exactly as unhelpful as a blank reported one.
//
// The identity half of a row — scope node, driver, native id, and the class the
// report states — is taken as reported, because a report always carries it in
// full and it is what the id was derived from. Everything below is a LEARNED
// fact, and a report that does not carry one is saying "I did not learn it",
// never "it is no longer true". That distinction is the whole content of this
// function, and it is the same one deviceplane.Store.Observe draws for the same
// four strings inside a single relay process.
func mergeDiscovered(prior, next DiscoveredDevice) DiscoveredDevice {
	row := next
	row.Name = orKeepFact(next.Name, prior.Name)
	// Address included deliberately, and it is the one with teeth: it is where a
	// command to an adopted device is sent, so blanking it on a thin report costs
	// control of the device. A genuinely changed address (DHCP moved it) is
	// non-empty and does overwrite, which is the case that has to keep working.
	row.Address = orKeepFact(next.Address, prior.Address)
	row.Model = orKeepFact(next.Model, prior.Model)
	row.Serial = orKeepFact(next.Serial, prior.Serial)

	// Ports: only an active scan can assert this list, and only a deployment
	// carrying the scanning pack runs one at all — every passive re-sighting in
	// between carries none. Keeping the held list when the report carries none is
	// the same rule the relay applies in memory.
	//
	// KNOWN LIMIT, stated rather than papered over: this cannot distinguish "a
	// scan looked and found nothing open" from "nobody looked", because the
	// stored column has no way to be absent (it is `[]` either way) and neither
	// does the wire. Today that costs nothing — internal/relay/portscan only
	// emits an entry for a host with a port OPEN, so a host whose ports have all
	// closed is simply missing from the result and is never observed at all, which
	// leaves the relay's own equivalent guard unreachable too. The day a scan
	// reports a scanned-but-closed host, BOTH guards need the absent/empty
	// distinction plumbed through, and this comment is where that starts.
	if len(next.OpenPorts) == 0 && len(prior.OpenPorts) > 0 {
		row.OpenPorts = prior.OpenPorts
	}

	row.Entities = mergeEntities(prior.Entities, next.Entities)
	return row
}

// mergeEntities carries an entity's learned STATE and ATTRIBUTES across a report
// that re-declares the entity without them.
//
// A discovery sighting observes that an entity exists; only the poller observes
// what it is doing. So a re-declaration arrives with a blank state for every
// entity, and taking it wholesale would blank the state an operator is watching
// — and, after a restart, would hand adoption a device whose entities describe
// nothing. A report that DOES carry a state states a new one and wins.
//
// A report that declares no entities at all keeps the held list outright, on the
// header's rule: a relay whose in-memory candidate has just been re-minted has
// not yet learned the fan-out, and a device that mirrors as entity-less is a
// device that ADOPTS as entity-less — permanently, since nothing re-derives an
// adopted row's entity list afterwards.
func mergeEntities(prior, next []wire.CandidateEntity) []wire.CandidateEntity {
	if len(next) == 0 {
		return prior
	}
	if len(prior) == 0 {
		return next
	}
	held := make(map[string]wire.CandidateEntity, len(prior))
	for _, e := range prior {
		held[e.Key] = e
	}
	out := make([]wire.CandidateEntity, 0, len(next))
	for _, e := range next {
		was, ok := held[e.Key]
		if ok {
			if e.State == "" {
				e.State = was.State
			}
			if len(e.Attributes) == 0 {
				e.Attributes = was.Attributes
			}
		}
		out = append(out, e)
	}
	return out
}

// orKeepFact returns the reported value when it carries one, else what is
// already known. The counterpart of internal/relay/deviceplane's orKeep, at the
// durable layer — see mergeDiscovered for why an empty report is silence rather
// than a retraction.
func orKeepFact(reported, held string) string {
	if reported != "" {
		return reported
	}
	return held
}

// ForgetDiscoveredDevices drops every mirrored row a relay reported.
//
// It is the durable half of internal/app/devices.Registry.Forget and carries the
// same rule: called when a relay's enrollment is REVOKED, never when its
// connection merely drops. A revoked relay must stop describing the site, and a
// mirror that outlived the revocation would keep it describing the site across
// the next restart — which is longer than the in-memory view would have.
func (s *Store) ForgetDiscoveredDevices(ctx context.Context, relayID string) error {
	if relayID == "" {
		return fmt.Errorf("store: ForgetDiscoveredDevices: relay_id must not be empty")
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM discovered_devices WHERE relay_id = ?`, relayID); err != nil {
			return fmt.Errorf("store: ForgetDiscoveredDevices: %s: %w", relayID, err)
		}
		return nil
	})
}

// DiscoveredDevices returns every mirrored sighting in device-id order — the
// same keyset order the api/1 list surface pages by, so a caller can hand the
// result straight to pagination without re-sorting.
func (s *Store) DiscoveredDevices(ctx context.Context) ([]DiscoveredDevice, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readDiscoveredDevices(ctx, s.db, "")
}

// readDiscoveredDevices is the unlocked core, optionally narrowed to one
// device_id. Callers hold their own lock (or run inside a transaction).
//
// # The age comes from the LEDGER, through a join, and not from the column
//
// `discovered_devices.first_seen` is documented as the ledger's PROJECTION
// (devicefirstseen.go) and it is that for every row the ledger answers for. It is
// not that for one population, and reading the column as though it were is how a
// refused value reached an operator's screen while the same boot's log said the
// device had no age at all:
//
//   - the backfill REFUSES a pre-ledger value it judges implausible — below the
//     plausibility floor, or in the future of this box's clock — and writes no
//     ledger row for it;
//   - it deliberately leaves the column alone, because that value is the only
//     copy of the site's pre-ledger history and never-wipe forbids destroying it
//     to make a projection tidy;
//   - so the column keeps a number this side has explicitly decided it will not
//     stand behind, and the boot restore read it straight into the read model.
//     Measured: a store whose one device carried `first_seen = 1000000` logged
//     "refused … it has no age until the next report plants one" and then served
//     `first_seen: 1000000`, which the console renders "20586d ago". A future
//     value was worse still — the console clamps a negative age, so it rendered
//     "just now" for a device the log said had no age whatever.
//
// The join makes the ledger the single answer to "how old is this device": a
// refused row reads as absent, which is what the log says and what the console
// draws as an em dash, while the refused NUMBER stays exactly where it was, named
// by every `-store-check` until a report on a working clock replaces it.
// MirroredFirstSeen carries the raw column out for the one caller that needs it.
func readDiscoveredDevices(ctx context.Context, q queryer, deviceID string) ([]DiscoveredDevice, error) {
	query := `SELECT d.device_id, d.relay_id, d.scope_node, d.driver, d.native_id, d.device_class,
	                 d.name, d.address, d.model, d.serial, d.first_seen, d.last_seen, d.entities, d.open_ports,
	                 d.relay_last_seen,
	                 COALESCE(l.first_seen, 0), COALESCE(l.origin, '')
	          FROM discovered_devices d
	          LEFT JOIN device_first_seen l ON l.device_id = d.device_id`
	args := []any{}
	if deviceID != "" {
		query += ` WHERE d.device_id = ?`
		args = append(args, deviceID)
	}
	query += ` ORDER BY d.device_id`

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read discovered devices: %w", err)
	}
	defer rows.Close()

	out := []DiscoveredDevice{}
	for rows.Next() {
		var d DiscoveredDevice
		var entities, ports, origin string
		if err := rows.Scan(&d.DeviceID, &d.RelayID, &d.ScopeNode, &d.Driver, &d.NativeID, &d.DeviceClass,
			&d.Name, &d.Address, &d.Model, &d.Serial, &d.MirroredFirstSeen, &d.LastSeen, &entities, &ports,
			&d.RelayLastSeen, &d.FirstSeen, &origin); err != nil {
			return nil, fmt.Errorf("store: scan discovered device: %w", err)
		}
		if d.FirstSeen > 0 {
			d.FirstSeenOrigin = firstSeenOrigin(origin)
		}
		if err := json.Unmarshal([]byte(entities), &d.Entities); err != nil {
			return nil, fmt.Errorf("store: decode entities of discovered device %s: %w", d.DeviceID, err)
		}
		if err := json.Unmarshal([]byte(ports), &d.OpenPorts); err != nil {
			return nil, fmt.Errorf("store: decode open_ports of discovered device %s: %w", d.DeviceID, err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// nonNilEntities normalizes a nil slice to an empty one so the stored column is
// `[]` rather than `null` — the same no-null discipline every array this store
// writes follows, and the difference between a decode that yields an empty fan-out
// and one that yields nil and reads as "unknown".
// nonNilPorts keeps an absent list from serializing as JSON null, the same rule
// the entity list follows: a decoder reading the column back must get an array.
func nonNilPorts(p []int) []int {
	if p == nil {
		return []int{}
	}
	return p
}

func nonNilEntities(in []wire.CandidateEntity) []wire.CandidateEntity {
	if in == nil {
		return []wire.CandidateEntity{}
	}
	return in
}

// ErrDiscoveredDeviceUnknown reports an adopt naming a device no relay has ever
// reported. Refused rather than adopted on the caller's word: an adoption record
// is the row desired state ships to the LAN edge (REL-063), and one filed for a
// device_id nobody has seen would instruct a relay to poll and command something
// that does not exist.
var ErrDiscoveredDeviceUnknown = errors.New("store: no discovered device with this identifier")

// AdoptDiscoveredDevice promotes one mirrored sighting into the durable
// adopted-device row (KindAdoptedDevice) that puts the device under this
// platform's control, and reports whether this call is what created it.
//
// This is the pipeline internal/app/api/identityrows.go describes and nothing
// implemented: "the pipeline that turns a reported candidate into an adopted row
// writes through the ordinary /adopted-devices resource family and therefore
// lands here automatically". It does exactly that — s.Create, the same call the
// resource family makes — so the row gets the full api/1 baseline, the identity
// validation, the generation bump, and the desired-state derivation with no
// special case anywhere downstream.
//
// The id it creates is the DISCOVERED device's id, which is derived from
// REL-153's `(site, driver, native_id)` tuple rather than minted. That is what
// makes adoption converge: the relay's report, the read model's row, the adopted
// record, and every entity id underneath all name one device, so re-adopting one
// physical unit can never produce a second device_id (DAT-004a/REL-063).
//
// Adopting an already-adopted device is a no-op returning created=false, not a
// conflict. Adoption is a state to arrive at, an operator may double-click, and a
// retry after a timeout must not be an error.
//
// Every entity the sighting reported is adopted `enabled`, un-hidden, and
// `primary`, with no display name. Those are REL-063 POLICY decisions with no
// discovered answer, and this is the only defensible default set: the point of
// adopting a device is to use it, so an adoption that arrived disabled would be
// a device that looks adopted and does nothing. An operator refines the policy
// afterwards through the /adopted-devices family, which is what that family is
// for.
func (s *Store) AdoptDiscoveredDevice(ctx context.Context, deviceID string) (created bool, err error) {
	if deviceID == "" {
		return false, fmt.Errorf("store: AdoptDiscoveredDevice: device_id must not be empty")
	}

	s.mu.RLock()
	found, err := readDiscoveredDevices(ctx, s.db, deviceID)
	s.mu.RUnlock()
	if err != nil {
		return false, err
	}
	if len(found) == 0 {
		return false, ErrDiscoveredDeviceUnknown
	}
	d := found[0]

	// Asked BEFORE the create rather than by catching its already_exists
	// validation error: "already adopted" is a success here, and distinguishing
	// it from the other reasons Create refuses an identity row by inspecting an
	// error's shape would make a benign repeat indistinguishable from a real
	// duplicate-identity conflict.
	if _, exists, gerr := s.Get(ctx, KindAdoptedDevice, deviceID); gerr != nil {
		return false, gerr
	} else if exists {
		return false, nil
	}

	body, err := json.Marshal(adoptedDeviceBody(d))
	if err != nil {
		return false, fmt.Errorf("store: AdoptDiscoveredDevice: encode %s: %w", deviceID, err)
	}
	if _, err := s.Create(ctx, KindAdoptedDevice, body); err != nil {
		return false, err
	}
	return true, nil
}

// adoptedDeviceBody projects a mirrored sighting onto the adopted-device row's
// own shape (datamodel.Device).
//
// PollCadenceSeconds is left absent rather than defaulted: REL-063 makes it an
// operator decision, an absent one means "the relay's own default", and picking
// a number here would silently impose a poll rate on every adopted device that
// nobody chose and nobody could see they had chosen.
func adoptedDeviceBody(d DiscoveredDevice) datamodel.Device {
	entities := make([]datamodel.DeviceEntity, 0, len(d.Entities))
	for _, e := range d.Entities {
		entities = append(entities, datamodel.DeviceEntity{
			// Derived, never taken from the report: both peers compute an
			// entity_id from the identity tuple plus the relay's own addressing
			// key (REL-110b), which is what lets the relay recognize the id the
			// app peer later addresses a command to.
			EntityID:    deviceid.Entity(d.ScopeNode, d.Driver, d.NativeID, e.Key),
			DeviceClass: e.DeviceClass,
			Enabled:     true,
			Category:    "primary",
		})
	}
	return datamodel.Device{
		ID:        d.DeviceID,
		ScopeNode: d.ScopeNode,
		Name:      adoptedName(d),
		Driver:    d.Driver,
		NativeID:  d.NativeID,
		Entities:  entities,
	}
}

// adoptedName is the name the adopted row is created with: what the device calls
// itself when it said anything, else its identity tuple spelled out. A device row
// MUST carry a non-empty name (datamodel.checkPlacementAndName), so falling back
// is not cosmetic — an unnamed device would be un-adoptable.
func adoptedName(d DiscoveredDevice) string {
	if d.Name != "" {
		return d.Name
	}
	return d.Driver + " " + d.NativeID
}
