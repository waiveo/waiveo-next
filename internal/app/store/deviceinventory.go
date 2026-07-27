package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/manifest"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// deviceinventory.go derives the relay/1 `device_inventory` section
// (REL-063/064) from what this store already holds. It is a DERIVATION, not a
// second authority: nothing here is stored in its own right, and the two arrays
// are read from two different existing tables for two different reasons.
//
// # `devices` — the adopted set (REL-063)
//
// Every entry is one adopted-device row (KindAdoptedDevice), projected down to
// exactly the five members REL-063 names: `{device_id, driver, native_id,
// poll_cadence_seconds, entities}`. The row's `id` IS the `device_id`
// (data-model/1 DAT-004a), and its identity is the `(site, driver, native_id)`
// tuple (REL-153) — never the relay currently reporting it, which is why no
// relay id appears anywhere in this file.
//
// The projection deliberately drops the api/1 resource baseline the row also
// carries (name, scope_node, external_id, labels, revision, timestamps). None of
// it is relay-facing, REL-063 enumerates five members and not eleven, and a
// section is signed desired state — shipping an operator's labels to the LAN
// edge because they happened to share a table would be over-sharing with no
// consumer.
//
// This is the OTHER direction of travel from the relay's discovered-candidate
// report (REL-110/111, served read-only by internal/app/devices). The relay is
// authoritative for what EXISTS; these rows are authoritative for what has been
// ADOPTED and under what policy. The pipeline that turns a reported candidate
// into an adopted row writes through the ordinary /adopted-devices resource
// family and therefore lands here automatically — nothing in this derivation
// needs to change for it to arrive.
//
// # `pack_match_patterns` — the installed packs (REL-064)
//
// REL-064 fixes the source, and it is NOT the adopted set: the section carries
// "the discovery-match patterns declared by every currently installed pack's
// device contribution (manifest/1 MAN-070/071) — the patterns a relay watches
// for during discovery INDEPENDENT of what is already adopted."
//
// Both halves of that sentence are load-bearing, and reading it as the adopted
// devices' classes (or as the intersection of the two) breaks discovery in a way
// no test of the adopted path would catch:
//
//   - Discovery's whole purpose is finding devices that are NOT yet adopted. If
//     a pattern only appeared once a device of that class had been adopted, the
//     FIRST device of every class could never be discovered — a pack could be
//     installed and its hardware would stay invisible forever.
//   - Un-adopting the last device of a class would silently blind the relay to
//     that class, so removing one device would remove the ability to ever find
//     its replacement.
//
// So the derivation reads the installed `packs` table and nothing else, and the
// adopted set has no influence on it whatsoever. What DOES change it is
// installing or uninstalling a pack that declares a `devices` contribution —
// both of which already bump the store generation in their own transaction
// (packs.go), so a changed pattern set reaches a relay by the same generation
// seam every other authored change does.
//
// `capabilities` (MAN-070's third member) is not carried: it is the capability
// grant the pack requests for the class, not a discovery-match pattern, and
// REL-064 asks for the patterns.

// packManifestDevices decodes just the `devices` contribution array out of a
// stored pack manifest. The manifest was already validated in full at install
// (internal/manifest.Validate is the install gate), so this read decodes only
// the member it needs rather than re-parsing and re-judging the whole document.
type packManifestDevices struct {
	Devices []manifest.Device `json:"devices"`
}

// packMatchPattern is one emitted `pack_match_patterns` element: MAN-070's
// device contribution reduced to the two members REL-064's own wire example
// carries, `{deviceClass, match}`. The class travels with its patterns because a
// relay that matches a pattern needs to know which device class it just found.
//
// The shape belongs to manifest/1, not relay/1, which is why the section carries
// these as raw JSON (wire.DeviceInventory) and this type exists here — beside
// the derivation that produces it — rather than in the relay/1 wire package.
type packMatchPattern struct {
	DeviceClass string           `json:"deviceClass"`
	Match       []map[string]any `json:"match"`
}

// DeviceInventory returns the `device_inventory` section (REL-063/064) derived
// from the store's current adopted-device rows and installed packs, under one
// read lock so the two arrays are a single consistent read.
//
// It exists beside DesiredState for the same reason ScopeNodes does: a caller
// that wants only this section should not pay for the scope-node tree, the six
// scheduling-core row kinds and the edge-rule bodies to answer a question about
// devices. DesiredState does NOT call it — it inlines the same unlocked core so
// every section it returns comes from one critical section (see DesiredState).
func (s *Store) DeviceInventory(ctx context.Context) (wire.DeviceInventory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return deviceInventory(ctx, s.db)
}

// deviceInventory is the unlocked core: the adopted-device entries plus the
// installed packs' declared match patterns, normalized so both arrays marshal as
// `[]` rather than `null` when empty (REL-060). Callers hold their own read lock.
func deviceInventory(ctx context.Context, q queryer) (wire.DeviceInventory, error) {
	devices, err := readAdoptedDeviceEntries(ctx, q)
	if err != nil {
		return wire.DeviceInventory{}, err
	}
	patterns, err := readPackMatchPatterns(ctx, q)
	if err != nil {
		return wire.DeviceInventory{}, err
	}
	return wire.DeviceInventory{Devices: devices, PackMatchPatterns: patterns}.Normalized(), nil
}

// readAdoptedDeviceEntries projects every adopted-device row into its REL-063
// wire entry, in row-id order (readBodies orders by id ascending, and a row id is
// a ULID, so that order is stable across reads) — a stable order is what makes an
// unchanged adopted set re-derive byte-identical section bytes, and therefore an
// unchanged `hash` (REL-053), on every rebuild.
//
// Each entry is marshaled from the typed wire.DeviceEntry (the contract's own
// five members, in the contract's own order) into the raw element the section
// carries, so the emitted bytes come from the relay/1 wire shape rather than from
// the stored row's bytes — which carry the api/1 resource baseline this section
// has no business shipping.
func readAdoptedDeviceEntries(ctx context.Context, q queryer) ([]json.RawMessage, error) {
	bodies, err := readBodies(ctx, q, string(KindAdoptedDevice))
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(bodies))
	for _, b := range bodies {
		var d datamodel.Device
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("store: decode adopted device: %w", err)
		}
		entry, err := json.Marshal(deviceEntryFor(d))
		if err != nil {
			return nil, fmt.Errorf("store: encode device inventory entry for %s: %w", d.ID, err)
		}
		out = append(out, entry)
	}
	return out, nil
}

// deviceEntryFor is the row → REL-063 entry projection: the five contract
// members and nothing else. `entities` is normalized to a non-nil empty slice so
// a device adopted before any entity policy was authored marshals `[]` rather
// than `null` — the same no-null discipline every array in a snapshot follows.
func deviceEntryFor(d datamodel.Device) wire.DeviceEntry {
	entities := make([]wire.DeviceEntity, 0, len(d.Entities))
	for _, e := range d.Entities {
		entities = append(entities, wire.DeviceEntity{
			EntityID:    e.EntityID,
			DeviceClass: e.DeviceClass,
			Enabled:     e.Enabled,
			Hidden:      e.Hidden,
			DisplayName: e.DisplayName,
			Category:    e.Category,
		})
	}
	return wire.DeviceEntry{
		DeviceID:           d.ID,
		Driver:             d.Driver,
		NativeID:           d.NativeID,
		PollCadenceSeconds: d.PollCadenceSeconds,
		Entities:           entities,
	}
}

// readPackMatchPatterns collects the discovery-match patterns every INSTALLED
// pack declares (REL-064) — never the adopted devices' classes, for the reasons
// this file's header sets out.
//
// Packs are read in id order and each pack's contributions in declaration order,
// so the emitted array is stable across reads. An entry a previous pack already
// contributed byte-for-byte is skipped: two packs declaring the identical
// contribution is one thing for a relay to watch for, not two, and collapsing it
// keeps the section (and therefore the snapshot hash) from depending on how many
// packs happen to say the same thing.
//
// A contribution declaring no patterns is not emitted: it names a device class
// but no way to ever match one, so it is not a pattern a relay can watch for.
func readPackMatchPatterns(ctx context.Context, q queryer) ([]json.RawMessage, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, manifest FROM packs ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: read pack manifests: %w", err)
	}
	defer rows.Close()

	out := []json.RawMessage{}
	seen := map[string]bool{}
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			return nil, fmt.Errorf("store: scan pack manifest: %w", err)
		}
		var m packManifestDevices
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			return nil, fmt.Errorf("store: decode manifest of pack %s: %w", id, err)
		}
		for _, d := range m.Devices {
			if len(d.Match) == 0 {
				continue
			}
			b, err := json.Marshal(packMatchPattern{DeviceClass: d.DeviceClass, Match: d.Match})
			if err != nil {
				return nil, fmt.Errorf("store: encode device match pattern of pack %s: %w", id, err)
			}
			if seen[string(b)] {
				continue
			}
			seen[string(b)] = true
			out = append(out, json.RawMessage(b))
		}
	}
	return out, rows.Err()
}
