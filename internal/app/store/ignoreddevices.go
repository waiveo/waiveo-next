package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ignoreddevices.go persists the operator's decision to IGNORE a discovered
// device — the third state a device can be in beside "discovered" and "adopted"
// (discovery spec §7: "every row carries a decision — Adopt / Ignore inline;
// ignored is visible and reversible").
//
// # Why its own table, and why NOT a resource Kind
//
// Ignore is deliberately unlike adoption. Adoption (KindAdoptedDevice) compiles
// into the signed desired-state a relay is sent (REL-063): an adopted device is
// polled and driven, so the decision must REACH the relay. Ignore reaches
// nobody — an ignored device is simply marked in the operator's view and left
// out of the way; the relay keeps reporting it and the mirror keeps holding it.
// So an ignore decision is a purely app-side annotation: it authors no
// desired-state, carries no revision, and MUST NOT bump the generation (that
// would make every relay re-pull a signed snapshot for a change no relay can
// see). That is exactly the discovered_devices mirror's own posture, and this
// table sits beside it rather than in the generic CRUD Kind machinery.
//
// # Why it survives a re-sighting
//
// The discovered_devices mirror is REPLACED wholesale on every relay report
// (ReplaceDiscoveredDevices), and the in-memory read model is rebuilt from each
// report the same way. An ignore stored inside either would be erased the next
// time the relay mentioned the device — which is the common case, not the edge
// one. Holding the decision in its own durable table, keyed by the derived
// device_id, is what makes "ignored" outlive both the next report and a restart:
// the read model re-applies it after every rebuild (internal/app/devices
// MarkIgnored), and the boot projection re-applies it from here.
const ignoredDevicesSchema = `
CREATE TABLE IF NOT EXISTS ignored_devices (
	device_id  TEXT PRIMARY KEY,
	ignored_at INTEGER NOT NULL
);
`

// IgnoreDevice records that an operator has ignored the device with this derived
// id, and reports whether this call is what created the record.
//
// Idempotent by construction: ignoring an already-ignored device is a no-op that
// returns created=false, so a double-click or a retried request is not an error
// (the same contract adoption keeps). ignoredAt is stamped for provenance only;
// nothing reads it back yet, and re-ignoring does not move it.
//
// The id is NOT checked against the discovered mirror here. A device that is
// currently powered off is absent from every relay's report yet may be exactly
// the one an operator wants to keep ignored, so the decision is remembered for
// an id this table has never otherwise heard of — the same latitude MarkAdopted
// takes, and for the same reason.
func (s *Store) IgnoreDevice(ctx context.Context, deviceID string, ignoredAt int64) (created bool, err error) {
	if deviceID == "" {
		return false, fmt.Errorf("store: IgnoreDevice: device_id must not be empty")
	}
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO ignored_devices (device_id, ignored_at) VALUES (?, ?)
			 ON CONFLICT(device_id) DO NOTHING`,
			deviceID, ignoredAt)
		if err != nil {
			return fmt.Errorf("store: IgnoreDevice: insert %s: %w", deviceID, err)
		}
		// RowsAffected is 0 on the ON CONFLICT no-op and 1 on a fresh insert, which
		// is exactly the created/not-created distinction the caller reports.
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: IgnoreDevice: rows affected %s: %w", deviceID, err)
		}
		created = n > 0
		// Deliberately no bumpGeneration — see this file's header.
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// UnignoreDevice removes the ignore decision for this device, and reports whether
// a decision existed to remove.
//
// Reversibility is the whole point (spec §7: "reversible… never a hidden trash
// can"): un-ignoring returns the device to plain "discovered", which the read
// model reflects the moment MarkIgnored's counterpart clears the flag.
// Un-ignoring a device that was not ignored is a no-op returning removed=false —
// benign, for the same reason the ignore side is idempotent.
func (s *Store) UnignoreDevice(ctx context.Context, deviceID string) (removed bool, err error) {
	if deviceID == "" {
		return false, fmt.Errorf("store: UnignoreDevice: device_id must not be empty")
	}
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM ignored_devices WHERE device_id = ?`, deviceID)
		if err != nil {
			return fmt.Errorf("store: UnignoreDevice: delete %s: %w", deviceID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: UnignoreDevice: rows affected %s: %w", deviceID, err)
		}
		removed = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

// ListIgnoredDeviceIDs returns every ignored device id, ordered, for the boot
// projection that re-teaches the in-memory read model which devices are ignored
// after a restart (cmd/waiveo-feeder, mirroring the adopted-row projection).
func (s *Store) ListIgnoredDeviceIDs(ctx context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, `SELECT device_id FROM ignored_devices ORDER BY device_id`)
	if err != nil {
		return nil, fmt.Errorf("store: ListIgnoredDeviceIDs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: ListIgnoredDeviceIDs: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: ListIgnoredDeviceIDs: rows: %w", err)
	}
	return ids, nil
}
