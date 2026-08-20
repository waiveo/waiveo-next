package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	stdlog "log"
	"net"

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
//
// `name_rank` and `class_rank` are the only columns here that are not facts
// about the device: they are REL-110c's statement about `name` and REL-110d's
// about `device_class` — which kind of LAN record authored each — and they exist
// so mergeDiscovered can refuse a worse-sourced value. Both are TEXT defaulting
// to the empty string, for two reasons that both matter. A constant default is
// what makes a column ADDABLE to a store that already exists (schemamigrate.go's
// whyNotAddable), and TEXT is what lets the empty string mean UNRECORDED as a
// value distinct from the recorded-and-weakest `none`. An INTEGER column
// defaulting to 0 would collapse those two into one number, so every row written
// before the column existed would silently claim a relay had told us its name or
// class was unranked — the #197 defect verbatim, and the reason
// device_first_seen.origin is spelled the same way one file over.
//
// The two ranks are separate columns because they are separate requirements with
// separate vocabularies and — the reason that actually bites — DIFFERENT ANSWERS
// FOR UNRECORDED. keepNameFact treats an unranked report as a merge on presence
// alone; keepClassFact does not. One column could not carry both rules.
const discoveredDevicesSchema = `
CREATE TABLE IF NOT EXISTS discovered_devices (
	device_id    TEXT PRIMARY KEY,
	relay_id     TEXT NOT NULL,
	scope_node   TEXT NOT NULL,
	driver       TEXT NOT NULL,
	native_id    TEXT NOT NULL,
	device_class TEXT NOT NULL,
	class_rank   TEXT NOT NULL DEFAULT '',
	name         TEXT NOT NULL DEFAULT '',
	name_rank    TEXT NOT NULL DEFAULT '',
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
	// ClassRank is REL-110d's rank of whatever record implied DeviceClass, held
	// for the merge and for nothing else — no surface renders it, and Stored
	// deliberately does not carry it (see storedFrom in cmd/waiveo-feeder). It is
	// one of classRankNone/Feature/Product, or classRankUnrecorded for a row
	// whose class arrived before this column existed or from a relay that does
	// not rank classes. It carries AUTHORITY only: how concrete the class is
	// follows from the class token and is re-derived (classConcretenessFact).
	// DeviceClass and ClassRank MOVE AS A PAIR through every merge; keepClassFact
	// is the only thing that may set either.
	ClassRank string
	Name      string
	// NameRank is REL-110c's rank of whatever authored Name, held for the merge
	// and for nothing else — no surface renders it, and Stored deliberately does
	// not carry it (see storedFrom in cmd/waiveo-feeder). It is one of
	// nameRankNone/Machine/Model/Friendly, or nameRankUnrecorded for a row whose
	// name arrived before this column existed or from a relay that does not rank
	// names. Name and NameRank MOVE AS A PAIR through every merge; keepNameFact
	// is the only thing that may set either.
	NameRank string
	Address  string
	Model    string
	Serial   string
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
			s.noteNameRankReset(was, d, relayID)
			s.noteClassRankRefused(was, d, relayID)
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
				   (device_id, relay_id, scope_node, driver, native_id, device_class, class_rank,
				    name, name_rank, address, model, serial, first_seen, last_seen, entities, open_ports,
				    relay_last_seen)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				row.DeviceID, row.RelayID, row.ScopeNode, row.Driver, row.NativeID, row.DeviceClass, row.ClassRank,
				row.Name, row.NameRank, row.Address, row.Model, row.Serial, row.MirroredFirstSeen, row.LastSeen,
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
// The identity half of a row — scope node, driver and native id — is taken as
// reported, because a report always carries it in full and it is what the id was
// derived from. Everything below is a LEARNED fact, and a report that does not
// carry one is saying "I did not learn it", never "it is no longer true". That
// distinction is the whole content of this function, and it is the same one
// deviceplane.Store.Observe draws for the same strings inside a single relay
// process.
//
// It turned out to be only half the rule, in both planes. Presence defends
// against a report that learned NOTHING; it says nothing about one that learned
// something WORSE, and a report can be worse in ways this mirror sees every
// minute — a class regressed to the generic default, an address that has lost
// its port, a machine-generated label where a chosen name exists, a class picked
// from whichever records one sweep happened to hold. Those are ranked below
// rather than taken.
//
// # THE RANKED-FACT LEDGER, and what is deliberately NOT on it
//
// The same defect has now been fixed three times, one field at a time, each
// found by hardware rather than by reading code: address (#198-era, rank
// re-derived from the value), name (#202, REL-110c's wire member plus a durable
// column), class (#204, REL-110d — the rank had to be given a life ACROSS SWEEPS
// on the relay before either half of it could travel). Fixing the third and
// stopping would be how a fourth arrives, so this is the enumeration that says
// the class is closed rather than hoping it is.
//
// The durable surface of a relay observation is small and closed. The app peer's
// inbound frame handler (internal/feeder/relayconn/server.go) has six cases after
// the handshake — state.pull (a request; writes nothing), state.ack (an in-memory
// map), device.candidates, screen.status, discovery.scan_status, pairing.redeemed
// — and only TWO reach disk: this table (plus the device_first_seen ledger) and a
// conditional DELETE for a redeemed pairing grant, which is idempotent and has no
// merge. screen.status and discovery.scan_status are in-memory by explicit
// decision, each saying so in its own package doc. Everything else is REL-004
// additive-ignore.
//
// Walking every wire member against every column of this table, the facts with a
// QUALITY RANK are: address (ranked both sides, re-derived from the value —
// keepAddressFact), name (ranked both sides, REL-110c — keepNameFact), class
// (ranked both sides, REL-110d — keepClassFact), and match (ranked in relay
// memory by deviceplane.keepMatch and DURABLE NOWHERE: there is no column, the
// read model decodes it as raw JSON it never consumes, and the boot restore
// synthesizes `{}`. A rank whose fact never reaches disk cannot flap on disk).
// Every other fact is symmetric between the two planes: model and serial are
// presence-only on both sides, open_ports is a nil-vs-empty check on both, an
// entity's state and attributes are carried across on both. So the class this
// change closes — A FACT WHOSE QUALITY THE RELAY JUDGES AND THE DISK CANNOT —
// is now EMPTY.
//
// "Closed" means the AUTHORITY travels, and the enumeration is worth nothing if
// it is read as "both planes now decide identically". They deliberately do not,
// and the difference is not a gap: deviceplane.keepClass additionally breaks an
// equal-authority tie on how CONCRETE the class is, and keepClassFact refuses to
// restate that comparison. A tiebreak on concreteness is a bet that a
// same-authority change is a dropped mDNS record rather than a changed device,
// and a bet like that is only sound while it EXPIRES. Relay memory expires with
// the process; this table does not. Applied here the same rule freezes a device
// that genuinely stops advertising its more concrete service, permanently and
// with no operator action that clears it. So the rule that needed to travel is
// the one that could not be re-derived, and the rule that must not travel is the
// one whose validity is a function of how long it is held. A future rank should
// be checked against BOTH questions, not just the first.
//
// What remains is a DIFFERENT and smaller class, listed here because an unlisted
// gap is how this recurs: PRESENCE-ONLY MERGES WHERE QUALITY GENUINELY MATTERS.
// Two members, neither closable by a rank — the first still open, the second now
// closed and kept here for the standing trigger that would reopen it:
//
//  1. THE ENTITY FAN-OUT. The relay knows which DECLARATION authored a device's
//     entities (deviceplane.Observation.EntitySource) and that authority is
//     deliberately off the wire, so a withdrawn fan-out is indistinguishable
//     here from one not learned yet and survives on disk — mergeEntities'
//     documented KNOWN RESIDUAL, below. It needs an additive REL-110a member
//     saying "I hold no declaration for this device", NOT a rank: the missing
//     fact is a WITHDRAWAL, and a ladder cannot express one. Left because that
//     is a contract change with its own corpus and its own argument to make.
//
//  2. MODEL — and this entry now records a CLOSED question rather than an open
//     one, because leaving it phrased as a residual is how it gets re-opened and
//     re-argued from scratch. The previous version of this list called it "two
//     changes, not one": which element to read, plus a ModelSource to carry the
//     precedence the way NameSource carries the name's. It was one change (#207).
//
//     The whole defect was the element read. internal/relay/ecp decoded six
//     elements out of the 79 the lab's Roku sends, and `friendly-model-name` —
//     the only human-readable model string on OEM hardware — was in the 73 it
//     ignored, so 192.168.50.31 was stored and rendered as `100012587`, a
//     retailer's part number. The discarded precedence index sitting beside it
//     was real but was the NAME's problem generalised onto a field that does not
//     have it, and generalising a fix by shape rather than by cause is its own
//     version of the habit this ledger exists to break.
//
//     NO RANK WAS ADDED, deliberately: no ModelSource in ecp, no wire member, no
//     column here. Model has exactly ONE writer (the ECP probe); every other lane
//     reports it empty and both merge layers already refuse empty, so a rank
//     would have no competing report to rule against. It would also be pinned:
//     Model has no continuously sweeping producer at all, so by the invariant
//     above — and by deviceplane's own — every rung would sit above what any
//     sweeping lane can produce, claimable once and then permanent across
//     restarts. That is harmless only because a model is immutable, which is the
//     argument against building it rather than for it. Presence-only also keeps
//     the door open rather than shut: a rank would have had to be taught that a
//     stored part number is worse before it could be replaced, whereas a
//     presence-only merge takes any non-empty "onn•Roku TV" the moment one is
//     reported.
//
//     WHAT THAT DOES NOT MEAN, because the first draft of this entry claimed it
//     and it was not checked: the stored part numbers are NOT self-healing today.
//     The ECP probe fires only for SSDP-watched targets, core declares no SSDP
//     pattern, and box .12 currently runs with zero of them (measured 2026-08-20,
//     pid 3135258: "0 ssdp, 1 mdns live (0 pack pattern(s))"). Its one row
//     carrying a model — 192.168.50.31, model=100012587 — will hold that value
//     through any number of scheduled scans, because the scans that run are ARP
//     sweep and port scan and neither probes. Fixing which element ecp reads is
//     necessary for the correction and is not sufficient for it; the other half
//     is a pack re-declaring an SSDP pattern that device answers.
//     ecp.QueryDeviceInfo carries the evidence.
//
//     THE TRIGGER THAT REOPENS IT, stated so it is not rediscovered: a SECOND
//     lane learning to report a model. hostmdns reading the `md=` TXT key of
//     `_googlecast._tcp` is the concrete one, and such a device is already in the
//     mirror. On that day Model has two writers of different quality, so a rank
//     becomes necessary — and legal, because mDNS sweeps every 30s and can
//     therefore refresh what it claims, which today's scan-gated probe cannot.
func mergeDiscovered(prior, next DiscoveredDevice) DiscoveredDevice {
	row := next
	// Name is RANKED here now, and it took the contract change the previous
	// version of this comment declined to make. The old reading was right about
	// the mechanics and wrong about the conclusion: the quality of a name really
	// is not derivable from the string (unlike the address's port and the class's
	// generic default, both of which the two functions below re-derive app-side),
	// so ranking it here needed a wire member — and REL-004 licenses exactly
	// that, which is how `address`, `model` and `serial` already arrived. It is
	// now REL-110c's `name_rank`.
	//
	// Inheriting the relay's fix by "storing what a fixed relay reports" was the
	// half that does not work, and the box proved it. The relay's ranked merge
	// lives in a process whose whole candidate map is re-minted at every restart,
	// so the first post-restart report is whichever lane swept first — and a
	// presence-only mirror wrote that straight onto disk, where it outlives every
	// later sweep that knew better. Measured: the address survived two relay
	// restarts because this mirror ranks it; the name did not, because it did
	// not. Same commit, same machinery, one field short.
	//
	// Name and NameRank move as a PAIR — a held name stamped with a reported
	// rank, or the reverse, is a corrupted ladder entry and a silent one — so
	// they are assigned from one call and nothing else here may touch either.
	row.Name, row.NameRank = keepNameFact(next.Name, next.NameRank, prior.Name, prior.NameRank)
	// Address is ranked, because unlike the name its quality is IN THE VALUE and
	// needs no wire member to read: "192.168.50.31:8060" is a strict information
	// superset of "192.168.50.31". Presence alone let a report from a lane that
	// sees only hosts overwrite one that saw a host and a port — on the lab box,
	// 61 of 61 mirrored rows had lost their port that way. This is the same rule
	// the relay applies in memory (deviceplane.keepAddress) and the reason the two
	// layers must agree: whichever one is presence-only decides the answer.
	row.Address = keepAddressFact(next.Address, prior.Address)
	row.Model = orKeepFact(next.Model, prior.Model)
	row.Serial = orKeepFact(next.Serial, prior.Serial)
	// The class the report states is NOT simply taken as reported, though this
	// function's header used to say it was. It is stated in full by every report,
	// which makes it look declaration-side; but the relay mints the generic
	// default for a host no lane has recognised yet, so a restarted relay reports
	// `unclassified` for a device it will classify seconds later once its mDNS
	// sweep lands — and a blind take wrote that over a learned class DURABLY,
	// which is worse than the in-memory flicker the relay already fixed.
	//
	// The generic-default guard was only half the rule, exactly as presence was
	// only half the name's. It defends against a report that recognised NOTHING;
	// it said nothing about one that recognised something less AUTHORITATIVE —
	// and the relay's class pick is a function of WHICH RECORDS ONE SWEEP HELD,
	// so a sweep that misses a device's own product service reports a different
	// SPECIFIC class for an unchanged device, which this merge took as a
	// reclassification and wrote to disk (#204; the ecobee at 192.168.39.241 is
	// one missing `_ecobee._tcp` record from being called a media player).
	// REL-110d's rank is what tells the two apart, and DeviceClass and ClassRank
	// move as a PAIR — nothing else here may touch either.
	//
	// What this layer deliberately does NOT do is refuse an EQUAL-authority
	// change; keepClassFact argues why at length. The short form: that refusal is
	// a bet on a dropped record, it is only sound while it expires, and here it
	// never would.
	row.DeviceClass, row.ClassRank = keepClassFact(next.DeviceClass, next.ClassRank, prior.DeviceClass, prior.ClassRank)

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
//
// # KNOWN RESIDUAL: a WITHDRAWN fan-out is not mirrored as withdrawn
//
// Removing a pack retires its entity fan-out on the relay, which stops reporting
// and stops resolving it (deviceplane.Store.RetainDeclarations). What arrives
// here afterwards is a candidate with an empty entity list — byte-for-byte what
// a relay that has not learned the fan-out YET sends, which is common and which
// the rule above exists for, since SSDP is passive-and-scan-gated and a restarted
// relay can go a long time before a watch re-declares. The two are
// indistinguishable at this seam: REL-110a's candidate has no member saying "I
// have no declaration for this device" as opposed to "I have not heard from one".
//
// So the durable row keeps the retired entity. The LIVE view is correct
// throughout — the registry takes the report verbatim, so the entity leaves
// `GET /entities` on the very next report, and the relay refuses any command
// against it — but a feeder restart re-materializes it from here for the seconds
// until the first report lands. This is pre-existing rather than new (the
// pre-guard relay blanked fan-outs on every neighbour sweep, so the same held
// list survived the same way) and closing it means an additive REL-110a member,
// which is a contract change with its own corpus.
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

// classUnclassified is the generic device_class every relay lane mints for a
// host nothing has recognised yet — REL-110a requires a non-empty class, so
// "not yet learned" has to be spelled as a value. It is the literal
// internal/relay/deviceplane.ClassUnclassified puts on the wire; this side
// restates it rather than importing the relay's package, because the app plane
// deliberately depends on no relay code.
const classUnclassified = "unclassified"

// The class-rank vocabulary, restated app-side exactly as classUnclassified and
// the name ranks are, and for the same reason: the app plane deliberately
// depends on no relay code, so it restates the tokens relay/1 REL-110d publishes
// (internal/shared/wire.CandidateClassRank*) rather than importing the relay's
// own ordered ladder. The two are pinned in agreement by a test.
//
// classRankUnrecorded is NOT a token any relay sends — it is the empty string
// the column carries for a row this build never ranked, deliberately distinct
// from classRankNone, for the reason the schema comment gives.
const (
	classRankUnrecorded = ""
	classRankNone       = "none"
	classRankFeature    = "feature"
	classRankProduct    = "product"
)

// classRankOrder places a stored or reported class-rank token on this side's
// ladder. Everything unknown — including unrecorded — sits at the BOTTOM, on
// nameRankOrder's argument verbatim: a rank is a licence to refuse, a durable
// rank is that licence at disk lifetime, and a token this build cannot read must
// never be honoured as one.
func classRankOrder(token string) int {
	switch token {
	case classRankProduct:
		return 2
	case classRankFeature:
		return 1
	default:
		// classRankNone, classRankUnrecorded, and any token this build cannot
		// read.
		return 0
	}
}

// classRankFact is the vocabulary clamp: what a REPORTED token is allowed to
// become when it is written to disk. Unreadable becomes classRankNone — the
// honest floor, a class nothing vouches for — rather than verbatim (an untrusted
// relay's bytes in a column this store reasons over) or unrecorded (untrue: the
// relay DID state something). nameRankFact's reasoning, one field over.
func classRankFact(reported string) string {
	switch reported {
	case classRankProduct, classRankFeature, classRankNone:
		return reported
	case classRankUnrecorded:
		return classRankUnrecorded
	default:
		return classRankNone
	}
}

// classConcretenessFact is the DERIVABLE half of a class's quality: how much the
// class narrows what an operator can do with the device. It is
// deviceplane.ClassConcreteness restated app-side, exactly as classUnclassified
// is and for the same no-relay-imports reason.
//
// It being derivable is the whole reason REL-110d carries authority ONLY. A
// consumer holding two class tokens can order them for itself; it cannot know
// which mDNS service type implied each, which is why that half had to go on the
// wire. Keeping the derivable half app-side also means the guard below still
// works against a relay too old to send a rank at all — which matters, because
// the measured instance of #204 is fixed by this half alone.
func classConcretenessFact(class string) int {
	switch class {
	case "", classUnclassified:
		return 0
	case "smart-home":
		return 1
	default:
		return 2
	}
}

// classAuthorityFact is the rank a class token is ALLOWED to carry —
// deviceplane.classAuthorityOf at the durable layer, restated for the same
// no-relay-imports reason as everything else in this block, and load-bearing for
// a sharper reason than symmetry.
//
// A rank is a statement about the RECORD BEHIND a class, and behind a class of
// zero concreteness there is no record: `unclassified` is what every lane mints
// for a host it has not recognised, spelled as a value only because REL-110a
// forbids an empty `device_class`. A report that recognised nothing cannot be
// authoritative about nothing.
//
// This store reads that rank off the wire, and the relay is UNTRUSTED input here
// — that is why classRankFact exists at all. classRankFact clamps the token's
// VOCABULARY and says nothing about the pairing, so without this floor an
// enrolled relay sending {"device_class":"unclassified","class_rank":"product"}
// out-ranks every real classification on the LAN and pins the row at the generic
// default for the life of the file: nothing on the LAN mints `product` for a
// device whose records are all features, MarkStored pushes the pinned value over
// the live report in the read model, and only relay revocation clears it. The
// same shape arrives without malice from a pack that registers a device class
// whose id is literally `unclassified` (REG-010's grammar permits it; nothing
// reserves the sentinel) — discovery.Watch.observation would stamp it Product.
// Flooring the pairing is what makes "the generic default only fills a gap" a
// property of this function rather than a hope about its inputs.
func classAuthorityFact(class, rank string) string {
	if classConcretenessFact(class) == 0 && classRankOrder(rank) > 0 {
		return classRankNone
	}
	return rank
}

// keepClassFact is deviceplane.keepClass at the durable layer — and, like the
// name's, it returns the PAIR, because a class stamped with a rank that did not
// author it is a corrupted ladder entry and a silent one.
//
// It is keepNameFact's ladder rule with one addition, and the addition is the
// original guard this function has always carried:
//
//  1. A STRICTLY WORSE AUTHORITY IS REFUSED. That is what the durable rank is
//     for. The ecobee at 192.168.39.241 advertises `_ecobee` (smart-home, at
//     product) alongside `_airplay` and `_spotify-connect` (media-player, at
//     feature); a relay restart whose first sweep misses the one `_ecobee`
//     record honestly reports media-player, and without a rank on disk this
//     store took that as a reclassification and wrote it. The thermostat is one
//     missing record from being called a media player, and this is the half that
//     stops it.
//  2. THE GENERIC DEFAULT ONLY FILLS A GAP. `unclassified` is a statement of
//     ignorance, not a competing verdict, so at equal authority it never
//     displaces a class some lane learned. classAuthorityFact is what keeps that
//     true when a report claims a rank for it.
//  3. OTHERWISE THE NEWER REPORT LANDS, exactly as keepNameFact lets a rename
//     land at equal rank.
//
// # WHY RULE 3 IS NOT A CONCRETENESS TIEBREAK, which is the decision here
//
// deviceplane.keepClass DOES break an equal-authority tie on concreteness, and
// that is the fix for the measured #204 instance: 192.168.50.43's `_matter`
// (smart-home) and `_spotify-connect` (media-player) records are BOTH feature,
// so authority ties and only concreteness refuses the downgrade when a sweep
// drops the Spotify record. Restating that comparison here looks like
// defence-in-depth and is not. It is the same bet — "a same-authority change is
// a dropped record, not a changed device" — held for a DIFFERENT DURATION, and
// the duration is the entire content of the bet:
//
//   - In relay memory it expires with the process, which is tens of sweeps: long
//     enough that a dropped record is the likely explanation, short enough that
//     a real change lands.
//   - On disk it never expires. A device that PERMANENTLY stops advertising its
//     more concrete service — 192.168.50.43 with Spotify unlinked, a printer
//     that stops answering `_ipp` — reports the honest lower class on every
//     sweep, forever, and every one of them is refused. Restart the relay:
//     refused. Restart the app: the row is read back and refused. The class an
//     operator sees, and the command vocabulary REG-052 resolves from it, is
//     frozen at whatever the device used to be, with no operator action that
//     clears it short of revoking the relay. The same rule refuses a pack that
//     CORRECTS its own declared device class, which is the one input where the
//     newer statement is authoritative by construction.
//
// So the tiebreak lives at exactly one layer: the one that can bound it. This
// one defers. The cost of deferring is a single report's worth of wrong class
// after a relay restart whose first sweep missed a record — the relay's own
// cross-sweep memory has taken the field back by the next sweep — against a
// permanent, unclearable freeze. That is not a close trade.
//
// # DECISION: AN ABSENT RANK REFUSES, WHERE AN ABSENT NAME RANK SURRENDERS
//
// keepNameFact rule 2 does the opposite of rule 1 above — an unranked report
// REPLACES a better-ranked name and resets the row to unrecorded — and the class
// deliberately does not copy it. The two fields genuinely warrant different
// answers:
//
//   - The CONSEQUENCES are not symmetric. A wrong name is cosmetic and an
//     operator can see it and fix it. A wrong class governs the command
//     vocabulary (REG-052): it silently removes commands, stops class-targeted
//     automation matching, and is inherited by any adoption made while it is
//     wrong.
//   - Nor is the PRESSURE. Rule 2 exists because operators rename devices and A
//     RENAME MUST ALWAYS LAND. Nothing on a LAN renames a device's KIND — every
//     "reclassification" this subsystem has measured was a sweep artifact, which
//     is the whole content of #204 — so the requirement that shapes rule 2 has
//     no counterpart here.
//
// WHAT THAT COSTS, on the same terms rule 2's own cost is now stated in: a
// rolled-back or un-upgraded relay speaking for a device (REL-153 incumbency can
// hand it one) reports a class with no rank, which sits at the bottom, so it can
// never reclassify a row that already carries a real rank — for as long as that
// relay speaks. Bounded by the relay being upgraded again, unreachable from a
// current relay, and announced rather than silent: noteClassRankRefused. The
// name's hole loses a chosen name; this one loses a correction. Refusing is the
// right side for a field that removes commands, and the log is what makes it
// discoverable.
//
// The invariant both functions share: AN ABSENT RANK NEVER RAISES WHAT THE STORE
// WILL REFUSE. An unranked report lands at classRankUnrecorded, the bottom, so
// it can only lower a row's rank or leave it.
//
// # THE UPGRADE CASE, and the mixed-version window this ships into
//
// A row written before this column existed carries classRankUnrecorded, at the
// bottom, so it refuses nothing an authority-ranked report says. And while NO
// relay ranks classes yet — which is every relay in the fleet on the day this
// ships — every report ties at unrecorded, so rule 3 decides and this function
// behaves EXACTLY as the presence-shaped merge it replaces did. That is the
// point, not a shortfall: the pre-upgrade shape of #204 is fixed on the RELAY,
// by the cross-sweep memory that stops the flapping report from ever being sent,
// and a durable guard that tried to fix it here without authority to reason from
// would have to refuse on concreteness alone — which is the remedy d321893
// already rejected on live data, and which pins the ecobee as a media player
// permanently the first time a sweep drops its `_ecobee` record.
func keepClassFact(reportedClass, reportedRank, heldClass, heldRank string) (class, rank string) {
	reported := classAuthorityFact(reportedClass, classRankFact(reportedRank))
	held := classAuthorityFact(heldClass, heldRank)
	if r, h := classRankOrder(reported), classRankOrder(held); r != h {
		if r > h {
			return reportedClass, reported
		}
		return heldClass, held
	}
	// Equal authority. The generic default is the absence of a verdict rather
	// than a competing one, so it fills a gap and never takes a learned class;
	// anything else is the newer statement and lands.
	if classConcretenessFact(reportedClass) == 0 && classConcretenessFact(heldClass) > 0 {
		return heldClass, held
	}
	return reportedClass, reported
}

// The name-rank vocabulary, restated app-side exactly as classUnclassified is
// and for the same reason: the app plane deliberately depends on no relay code,
// so it restates the tokens relay/1 REL-110c publishes
// (internal/shared/wire.CandidateNameRank*) rather than importing the relay's
// own ordered ladder. The two are pinned in agreement by a test.
//
// nameRankUnrecorded is NOT a token any relay sends. It is the empty string the
// column carries for a row this build never ranked, and it is deliberately
// distinct from nameRankNone — see the schema comment, and #197.
const (
	nameRankUnrecorded = ""
	nameRankNone       = "none"
	nameRankMachine    = "machine"
	nameRankModel      = "model"
	nameRankFriendly   = "friendly"
)

// nameRankOrder places a stored or reported token on this side's ladder.
//
// The ORDERING is app-side on purpose and the wire carries only the token
// (wire's own note: REL-004 forbids renumbering an existing member's meaning, so
// a ladder shipped as ordinals could never gain a rank in the middle).
//
// EVERYTHING UNKNOWN SITS AT THE BOTTOM, and that is the clamp the intake
// deliberately does not perform. A token a newer — or a hostile — relay minted
// gets the weakest position rather than a strong one: it can still fill a gap
// and it can refuse nothing. That is the only safe direction, because a rank is
// a licence to REFUSE and a durable rank is that licence at DISK lifetime. A
// relay that could claim a rank above this build's top would pin a name of its
// choosing past every restart of both peers, which is the mirror image of the
// permanent pin deviceplane.NameRank's own refreshability constraint exists to
// prevent inside one process.
//
// nameRankUnrecorded lands at the bottom too, which is what makes an upgraded
// row REFUSE NOTHING: the held name's real quality is unknowable, and a store
// that guarded it would make a rename impossible on every pre-upgrade row
// forever.
func nameRankOrder(token string) int {
	switch token {
	case nameRankFriendly:
		return 3
	case nameRankModel:
		return 2
	case nameRankMachine:
		return 1
	default:
		// nameRankNone, nameRankUnrecorded, and any token this build cannot read.
		return 0
	}
}

// nameRankFact is the vocabulary clamp: what a REPORTED token is allowed to
// become when it is written to disk.
//
// An unreadable token is stored as nameRankNone rather than verbatim and rather
// than as unrecorded. Verbatim would put an untrusted relay's bytes in a column
// this store reasons over. Unrecorded would be untrue in the other direction —
// the relay DID state something, and a row that stays unrecorded forever is a
// row the ladder can never protect. `none` is the honest floor: a name nothing
// vouches for.
func nameRankFact(reported string) string {
	switch reported {
	case nameRankFriendly, nameRankModel, nameRankMachine, nameRankNone:
		return reported
	case nameRankUnrecorded:
		return nameRankUnrecorded
	default:
		return nameRankNone
	}
}

// keepNameFact is deviceplane.keepName at the durable layer — the merge whose
// absence let a relay restart write a machine-generated label over a display
// name permanently.
//
// It returns the PAIR, because storing one without the other is the failure mode
// this whole change is about. Four rules, in the order the function applies
// them:
//
//  1. A report carrying NO name is silence, not a retraction — orKeepFact's
//     existing property, and deviceplane.keepName's — so the held name AND the
//     held rank both stay. Keeping the name while re-stamping the rank would
//     leave the ladder describing a statement that was never made.
//
//  2. A report carrying a name but NO rank is a relay that does not speak
//     REL-110c, and it is handled as the contract requires: absent is "this peer
//     does not rank names", never "this name is unranked". Such a report is
//     merged EXACTLY as it was before this rule existed — presence wins — and
//     the row goes back to unrecorded. That is deliberate, and it is the choice
//     #197 got wrong twice: the alternative is to invent a rank nothing stated,
//     which would let this store assert a relay's opinion on the relay's behalf.
//     It also keeps a DOWNGRADE from bricking a device's name: a rank this store
//     held could otherwise out-rank everything an older relay can ever say, and
//     the device could never be renamed again.
//
//     WHAT RULE 2 COSTS, which this comment used to leave unsaid. It is a hole
//     by design, and the protection is only as strong as the LEAST-UPGRADED
//     relay that speaks for a device: A RELAY ROLLBACK SILENTLY RE-OPENS #202.
//     Unreachable from a current relay — cmd/waiveo-relay's nameRankToken has no
//     branch returning the empty string — but REL-153 incumbency can hand a
//     device to an older peer, and a rolled-back binary is one `cp` away on any
//     box that kept its predecessor. The disposition is still right for the NAME
//     (see below), so the answer is observability rather than policy:
//     logNameRankReset announces it once per device instead of discarding the
//     ladder in silence.
//
//     THE CLASS DELIBERATELY DOES NOT COPY THIS RULE, and keepClassFact argues
//     why at length. The short form: half a class's quality is derivable
//     app-side, so an unranked class report is not "no information"; a wrong
//     class removes commands rather than mislabelling a device; and nothing on a
//     LAN renames a device's KIND, so the rename pressure that shapes this rule
//     has no counterpart there. The invariant both functions DO share is that an
//     absent rank never RAISES what the store will refuse.
//
//  3. Otherwise the ladder decides, on keepName's own rule: same-or-better rank
//     wins immediately (a rename is the device restating itself through the
//     record it always announced, and it MUST land), a strictly worse one is
//     refused.
//
//  4. Whatever lands, lands as a pair, with the reported rank clamped to this
//     build's vocabulary (nameRankFact).
//
// # THE UPGRADE CASE, stated as a decision
//
// A row written before this column existed carries nameRankUnrecorded, and the
// question that has to be answered out loud is what an unrecorded rank REFUSES.
// The answer is NOTHING: unrecorded sits at the bottom of nameRankOrder, so the
// first report carrying any name at all wins rule 3 and replaces it. The
// alternative — treating a held name as good until something better arrives —
// would pin every pre-upgrade name against every lane on the LAN, at disk
// lifetime, with no way to tell which rows were affected. A rank the store never
// recorded is not evidence of quality, and refusing on it would be the store
// asserting a statement no relay made.
//
// The cost of that choice is honest and bounded: the first post-upgrade report
// may replace a currently-correct name with a worse-sourced one — once. It
// arrives WITH a rank, the row stops being unrecorded, and the next sweep that
// carries the better-ranked record takes it back and can then never lose it
// again. Self-healing, at the price of one report's worth of wrong. The build
// this replaces is not self-healing at any price, which is the whole point.
func keepNameFact(reportedName, reportedRank, heldName, heldRank string) (name, rank string) {
	if reportedName == "" {
		return heldName, heldRank
	}
	if reportedRank == nameRankUnrecorded {
		return reportedName, nameRankUnrecorded
	}
	if nameRankOrder(reportedRank) >= nameRankOrder(heldRank) {
		return reportedName, nameRankFact(reportedRank)
	}
	return heldName, heldRank
}

// noteNameRankReset announces, ONCE per device per process, that an unranked
// report has just discarded a ranked row's name ladder — keepNameFact rule 2
// firing for real.
//
// It exists because rule 2's disposition is right and its SILENCE is not. Rule 2
// is unreachable from a current relay (cmd/waiveo-relay always sets the member,
// and nameRankToken has no branch returning the empty string), so the only way
// to reach it is an older peer speaking for the device — a rolled-back binary,
// or an un-upgraded relay taking incumbency under REL-153. That is exactly the
// case where #202's protection quietly stops applying, and "the protection is
// only as strong as the least-upgraded relay" is not a fact an operator can
// discover from any surface. Now it is a log line naming the relay to look at.
//
// It is NOT a refusal, deliberately. Refusing the report instead would make a
// rename impossible against an older relay with no symptom an operator could act
// on, which is the trade keepNameFact already argues; the class's own answer
// diverges (keepClassFact) because a wrong class removes commands where a wrong
// name only mislabels.
//
// ONCE PER DEVICE, because reports arrive every minute for as long as the box is
// up: an unconditional line would be one per device per minute forever, which is
// the shape of logging an operator learns to filter out. The set is per-process
// and never trimmed — it is bounded by the device count, which the intake caps.
func (s *Store) noteNameRankReset(prior, reported DiscoveredDevice, relayID string) {
	if reported.Name == "" || reported.NameRank != nameRankUnrecorded {
		return
	}
	if nameRankOrder(prior.NameRank) == 0 {
		// Nothing is being given up: the row was already unrecorded, or held the
		// floor.
		return
	}
	if !s.firstRankNote("name:" + prior.DeviceID) {
		return
	}
	stdlog.Printf("store: relay %s reported device %s with a name and NO name_rank, so the stored rank %q is discarded and "+
		"the row returns to unrecorded (REL-110c: an absent rank means the peer does not rank names). A relay that speaks "+
		"REL-110c always sets the member, so this is an older or rolled-back peer — while it speaks for this device the "+
		"protection against a machine-generated label displacing a chosen one (#202) does not apply. Logged once per device.",
		relayID, prior.DeviceID, prior.NameRank)
}

// noteClassRankRefused announces, ONCE per device per process, that an UNRANKED
// report has just been refused a reclassification because the row carries a real
// rank — the class's own answer to keepNameFact rule 2, firing for real.
//
// It is the mirror of noteNameRankReset and it is deliberately narrower. A
// refusal of a RANKED report is ordinary, correct, and reachable from a current
// relay every time one restarts and its first sweep misses a record; logging
// that would be a burst of noise saying the guard works. An UNRANKED report
// being refused is not reachable from a current relay at all —
// cmd/waiveo-relay's classRankToken has no branch returning the empty string —
// so when it happens the cause is an older or rolled-back peer holding REL-153
// incumbency, and for as long as it speaks the device's class cannot be
// corrected by anything. That is a real, permanent-feeling symptom ("this
// thermostat is stuck as a media player and nothing I do changes it") whose only
// diagnosis is a relay version, which no surface otherwise connects to it.
//
// It is NOT a refusal being softened. The disposition is right — a class governs
// the command vocabulary (REG-052) and an unranked peer must not be able to
// remove commands — so what was missing was the symptom, not the policy.
func (s *Store) noteClassRankRefused(prior, reported DiscoveredDevice, relayID string) {
	if reported.ClassRank != classRankUnrecorded || reported.DeviceClass == "" {
		return
	}
	if reported.DeviceClass == prior.DeviceClass || classRankOrder(prior.ClassRank) == 0 {
		// Nothing was refused: the report agrees, or the row had no rank to
		// refuse with and rule 3 let the report land.
		return
	}
	if classConcretenessFact(reported.DeviceClass) == 0 {
		// The generic default, refused by the gap rule rather than by the
		// ladder. That is the highest-traffic path through the merge (every
		// neighbour-lane sweep) and says nothing about the peer's version.
		return
	}
	if !s.firstRankNote("class:" + prior.DeviceID) {
		return
	}
	stdlog.Printf("store: relay %s reported device %s as %q with NO class_rank, and the stored class %q at rank %q was kept "+
		"(REL-110d: an absent rank means the peer does not rank classes, which sits at the bottom of the ladder). A relay "+
		"that speaks REL-110d always sets the member, so this is an older or rolled-back peer — while it speaks for this "+
		"device its classification cannot be corrected. Upgrade that relay if the stored class is wrong. Logged once per device.",
		relayID, prior.DeviceID, reported.DeviceClass, prior.DeviceClass, prior.ClassRank)
}

// firstRankNote reports whether key has not been announced yet this process, and
// marks it. One set behind one mutex for both rank notes, keyed by note kind so
// neither suppresses the other. Its own lock rather than the store's: it is
// called inside a write transaction, which already holds mu.
func (s *Store) firstRankNote(key string) bool {
	s.rankResetMu.Lock()
	defer s.rankResetMu.Unlock()
	if s.rankResetSeen == nil {
		s.rankResetSeen = map[string]bool{}
	}
	if s.rankResetSeen[key] {
		return false
	}
	s.rankResetSeen[key] = true
	return true
}

// keepAddressFact is deviceplane.keepAddress at the durable layer: a report
// that carries only a host must not erase a port an earlier one read, but a
// report naming a DIFFERENT host is a device that moved and lands immediately,
// bare or not. What is protected is the port, not the address.
//
// The port is read with net.SplitHostPort rather than the relay's richer
// lanaddr.Split (an app package depends on no relay package), so a URL-shaped
// address — which the relay's own lanes can produce from an SSDP LOCATION —
// reads here as an uncomparable whole and simply takes the newer value, exactly
// as it did before this rule existed. The rule only ever protects a port it can
// actually see, which is the safe direction to be imprecise in.
func keepAddressFact(reported, held string) string {
	if reported == "" {
		return held
	}
	if held == "" {
		return reported
	}
	reportedHost, reportedPort := splitAddressFact(reported)
	heldHost, heldPort := splitAddressFact(held)
	if reportedHost != heldHost {
		return reported
	}
	if reportedPort == "" && heldPort != "" {
		return held
	}
	return reported
}

// splitAddressFact splits a mirrored address into host and port, treating
// anything net.SplitHostPort cannot parse as a bare host with no port.
func splitAddressFact(addr string) (host, port string) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, ""
	}
	return h, p
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
	query := `SELECT d.device_id, d.relay_id, d.scope_node, d.driver, d.native_id, d.device_class, d.class_rank,
	                 d.name, d.name_rank, d.address, d.model, d.serial, d.first_seen, d.last_seen, d.entities, d.open_ports,
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
		if err := rows.Scan(&d.DeviceID, &d.RelayID, &d.ScopeNode, &d.Driver, &d.NativeID, &d.DeviceClass, &d.ClassRank,
			&d.Name, &d.NameRank, &d.Address, &d.Model, &d.Serial, &d.MirroredFirstSeen, &d.LastSeen, &entities, &ports,
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
