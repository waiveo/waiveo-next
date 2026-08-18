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
	open_ports   TEXT NOT NULL DEFAULT '[]'
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
	FirstSeen   int64
	LastSeen    int64
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
// The delete and the inserts share one transaction: a mirror that was briefly
// empty mid-refresh would be read as "this relay found nothing", which is a
// visible and alarming state to expose for a write that is not changing
// anything.
//
// A row whose DeviceID is empty is skipped rather than refusing the batch. The
// caller derives that id, so an empty one is a caller defect on ONE device, and
// failing the whole refresh over it would freeze the mirror for every other
// device the relay can see.
func (s *Store) ReplaceDiscoveredDevices(ctx context.Context, relayID string, rows []DiscoveredDevice) error {
	if relayID == "" {
		return fmt.Errorf("store: ReplaceDiscoveredDevices: relay_id must not be empty")
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM discovered_devices WHERE relay_id = ?`, relayID); err != nil {
			return fmt.Errorf("store: ReplaceDiscoveredDevices: clear relay %s: %w", relayID, err)
		}
		for _, d := range rows {
			if d.DeviceID == "" {
				continue
			}
			entities, err := json.Marshal(nonNilEntities(d.Entities))
			if err != nil {
				return fmt.Errorf("store: ReplaceDiscoveredDevices: encode entities of %s: %w", d.DeviceID, err)
			}
			ports, err := json.Marshal(nonNilPorts(d.OpenPorts))
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
				    name, address, model, serial, first_seen, last_seen, entities, open_ports)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				d.DeviceID, relayID, d.ScopeNode, d.Driver, d.NativeID, d.DeviceClass,
				d.Name, d.Address, d.Model, d.Serial, d.FirstSeen, d.LastSeen, string(entities), string(ports)); err != nil {
				return fmt.Errorf("store: ReplaceDiscoveredDevices: insert %s: %w", d.DeviceID, err)
			}
		}
		// Deliberately no bumpGeneration — see this file's header.
		return nil
	})
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
func readDiscoveredDevices(ctx context.Context, q queryer, deviceID string) ([]DiscoveredDevice, error) {
	query := `SELECT device_id, relay_id, scope_node, driver, native_id, device_class,
	                 name, address, model, serial, first_seen, last_seen, entities, open_ports
	          FROM discovered_devices`
	args := []any{}
	if deviceID != "" {
		query += ` WHERE device_id = ?`
		args = append(args, deviceID)
	}
	query += ` ORDER BY device_id`

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read discovered devices: %w", err)
	}
	defer rows.Close()

	out := []DiscoveredDevice{}
	for rows.Next() {
		var d DiscoveredDevice
		var entities, ports string
		if err := rows.Scan(&d.DeviceID, &d.RelayID, &d.ScopeNode, &d.Driver, &d.NativeID, &d.DeviceClass,
			&d.Name, &d.Address, &d.Model, &d.Serial, &d.FirstSeen, &d.LastSeen, &entities, &ports); err != nil {
			return nil, fmt.Errorf("store: scan discovered device: %w", err)
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
