package main

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// candidatemirror.go makes the device plane's read model SURVIVE A RESTART.
//
// Before this file the intake was `relayconn.WithCandidateSink(deviceRegistry)`
// — a relay's `device.candidates` report went into an in-memory registry and
// nowhere else. That is a defensible model of AUTHORITY (the relay is the
// authority for its own LAN, and re-reports everything on every connection) and
// a bad model of AVAILABILITY: a feeder that has just restarted has heard from
// nobody, so `GET /devices` serves an empty page until each relay reconnects and
// reports. An operator cannot tell that apart from "there are no devices", and
// the console has nothing to draw. On a box whose relay is the thing being
// worked on, the window is not seconds.
//
// So the sink here does BOTH: it applies the report to the live registry exactly
// as before, and mirrors the applied result into the store
// (internal/app/store/discovereddevices.go), which the boot path reads back into
// the registry before the first relay connects.
//
// # The mirror follows the registry, it does not race it
//
// The registry is applied FIRST, and the rows written afterwards are the ones
// the registry actually accepted. That ordering is what keeps the durable copy
// from becoming a second opinion:
//
//   - a report the registry REFUSES (a malformed candidate, a duplicated
//     identity) writes nothing at all — REL-111 makes a report all-or-nothing,
//     and a mirror holding the half a refused report described would be a view no
//     relay ever reported;
//   - a device this relay reported but does NOT hold the routing for (REL-153a
//     incumbency, another relay is live on it) is dropped from the mirror too,
//     by asking the registry which devices it now attributes to this relay
//     rather than by re-deriving the rule here. One implementation of
//     incumbency, in one place.

// mirrorCandidatesTimeout bounds one report's store write. A report arrives on
// the connection's read loop, so the write must not be able to stall it: the
// next report is a full replace of the same set at most a minute later, which is
// exactly what makes abandoning a slow one safe.
const mirrorCandidatesTimeout = 5 * time.Second

// candidateMirror is the connection layer's CandidateSink: the live read model
// plus its durable mirror, applied in that order.
type candidateMirror struct {
	registry *devices.Registry
	st       *store.Store
}

// ApplyCandidates implements feederrelayconn.CandidateSink.
//
// relayID is the connection's AUTHENTICATED identity (REL-041/150), never a
// value the frame asserts — it is passed straight through to both the registry
// and the mirror, so neither can be made to replace another relay's rows.
//
// A mirror write failure is LOGGED, not returned. Returning it would answer the
// relay with a typed refusal of a report the read model already accepted, which
// would be false, and would make a transient disk problem look to the relay like
// a rejected report it should stop sending. The live view is correct either way;
// the mirror re-converges on the next report, a minute later.
func (m candidateMirror) ApplyCandidates(relayID string, candidates []wire.DeviceCandidate) error {
	if err := m.registry.ApplyCandidates(relayID, candidates); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), mirrorCandidatesTimeout)
	defer cancel()
	if err := m.st.ReplaceDiscoveredDevices(ctx, relayID, m.rowsFor(relayID, candidates)); err != nil {
		log.Printf("waiveo-feeder: mirroring relay %s's device report failed (the live view is unaffected; retried on the next report): %v", relayID, err)
	}
	return nil
}

// Forget implements feederrelayconn.CandidateSink: it drops both copies of what
// a relay reported.
//
// Called on REVOCATION, not on a dropped connection (the interface's own doc
// explains why). The durable half matters more than the live half here: an
// in-memory view dies with the process anyway, while a mirror that outlived a
// revocation would let a revoked relay keep describing the site across every
// future restart.
func (m candidateMirror) Forget(relayID string) {
	m.registry.Forget(relayID)

	ctx, cancel := context.WithTimeout(context.Background(), mirrorCandidatesTimeout)
	defer cancel()
	if err := m.st.ForgetDiscoveredDevices(ctx, relayID); err != nil {
		log.Printf("waiveo-feeder: clearing revoked relay %s's mirrored devices failed: %v", relayID, err)
	}
}

// rowsFor projects the candidates the registry ACCEPTED for this relay onto
// their mirror rows.
//
// The acceptance set is read back out of the registry rather than recomputed:
// the registry has just applied REL-153 incumbency, and re-deriving "does this
// relay hold this device" here would be a second implementation of a rule that
// exists to stop two relays disagreeing about one device.
//
// An IGNORED candidate is skipped, matching the registry's own intake: an
// operator suppressed the device, and mirroring it would resurrect it in the
// list on the next restart.
func (m candidateMirror) rowsFor(relayID string, candidates []wire.DeviceCandidate) []store.DiscoveredDevice {
	site := m.registry.Site()
	owned := map[string]bool{}
	for _, d := range m.registry.Devices() {
		if d.RelayID == relayID {
			owned[d.ID] = true
		}
	}

	rows := make([]store.DiscoveredDevice, 0, len(candidates))
	for _, c := range candidates {
		if c.Status == wire.CandidateStatusIgnored {
			continue
		}
		id := deviceid.Device(site, c.Driver, c.NativeID)
		if !owned[id] {
			continue
		}
		rows = append(rows, store.DiscoveredDevice{
			DeviceID:    id,
			RelayID:     relayID,
			ScopeNode:   site,
			Driver:      c.Driver,
			NativeID:    c.NativeID,
			DeviceClass: c.DeviceClass,
			Name:        c.Name,
			Address:     c.Address,
			Model:       c.Model,
			Serial:      c.Serial,
			FirstSeen:   c.FirstSeen,
			LastSeen:    c.LastSeen,
			Entities:    c.Entities,
		})
	}
	return rows
}

// restoreDeviceRegistry loads the mirrored device rows, and the adoption
// decisions taken over them, back into a freshly built registry — so the device
// list is populated the moment the API starts serving rather than whenever the
// relays next connect.
//
// The restore goes through the SAME ApplyCandidates path a live report takes,
// against reconstructed wire candidates, rather than through a private
// back-door setter. That is deliberate: the intake is the boundary that
// validates every field, derives every id, and applies incumbency, and a restore
// that bypassed it would be a second way to get rows into the registry — one
// whose output could differ from what a relay's report produces for the same
// device. Reconstructing the wire shape costs nothing and keeps exactly one
// intake.
//
// Relays are restored in id order so a boot is deterministic. Adoption is
// applied afterwards from the adopted-device rows, which are the durable
// authority for it — the mirror deliberately records no adoption flag of its own.
//
// A failure here is reported to the caller but is NOT fatal at the call site: a
// feeder that cannot pre-populate its device list still serves correctly the
// moment its relays report, and refusing to boot over a cache would turn a
// cosmetic degradation into an outage.
func restoreDeviceRegistry(ctx context.Context, st *store.Store, registry *devices.Registry) (int, error) {
	mirrored, err := st.DiscoveredDevices(ctx)
	if err != nil {
		return 0, err
	}

	byRelay := map[string][]wire.DeviceCandidate{}
	for _, d := range mirrored {
		byRelay[d.RelayID] = append(byRelay[d.RelayID], wire.DeviceCandidate{
			// `match` is the relay's own provenance for the sighting and the
			// read model does not consume it (wire.DeviceCandidate's doc), so
			// the restore states the only honest thing it can: an empty object.
			// Inventing the pattern that originally matched would be recording a
			// provenance nothing observed.
			Match:       []byte(`{}`),
			Provenance:  wire.CandidateProvenanceDiscovered,
			Status:      wire.CandidateStatusPending,
			FirstSeen:   d.FirstSeen,
			LastSeen:    d.LastSeen,
			Driver:      d.Driver,
			NativeID:    d.NativeID,
			DeviceClass: d.DeviceClass,
			Name:        d.Name,
			Address:     d.Address,
			Model:       d.Model,
			Serial:      d.Serial,
			Entities:    d.Entities,
		})
	}

	relayIDs := make([]string, 0, len(byRelay))
	for id := range byRelay {
		relayIDs = append(relayIDs, id)
	}
	sort.Strings(relayIDs)
	for _, id := range relayIDs {
		if err := registry.ApplyCandidates(id, byRelay[id]); err != nil {
			return 0, err
		}
	}

	adopted, err := st.List(ctx, store.KindAdoptedDevice, store.ListFilter{})
	if err != nil {
		return 0, err
	}
	for _, row := range adopted {
		registry.MarkAdopted(row.ID)
	}
	return len(mirrored), nil
}
