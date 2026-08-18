package main

import (
	"context"
	"log"
	"sort"
	"sync"
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
	// faults is shared by every copy of this value (it is held as a pointer and
	// the methods take value receivers), which is what lets the sink recognize
	// the SECOND failure as related to the first.
	faults *mirrorFaults
	// nowMs is the app's persisted monotonic clock floor (SEC-066), the same
	// reading every row this process stamps comes from — NOT the host clock. It
	// is only used to say how long a run of failures has lasted, but a duration
	// an operator will correlate against stored rows and audit events has to be
	// measured on the clock those were written on.
	nowMs func() int64
}

// newCandidateMirror wires a sink with its own failure tracker and the process's
// one clock.
func newCandidateMirror(registry *devices.Registry, st *store.Store, nowMs func() int64) candidateMirror {
	return candidateMirror{registry: registry, st: st, faults: &mirrorFaults{}, nowMs: nowMs}
}

// mirrorFaults is what a durable write that keeps failing says for itself.
//
// # Why it exists
//
// The mirror write is deliberately LOGGED rather than returned (see
// ApplyCandidates), and for a transient disk fault that is right: the next
// report is along in a minute and re-converges the whole set. But the same
// handling is applied to a fault that can NEVER re-converge, and there the
// per-failure log line is the entire escalation ladder. Box .12 spent seven days
// with a mirror that could not write a single row, and said so 1017 times, one
// identical line per report, each one carrying the reassurance that it would be
// "retried on the next report" — which was true and useless, because the retry
// could not succeed. Nothing counted them, nothing compared the first to the
// last, and nobody read the thousandth.
//
// # What it changes
//
// Not the disposition — the write failure is still not returned to the relay,
// for the reason ApplyCandidates gives. What changes is that a repeating failure
// stops reading like a fresh one. The first is reported immediately, the count
// doubles between reports after that, and a continuing fault is never quiet for
// more than mirrorFaultMaxQuietMs — so box .12's exact outage (1017 failed
// writes over seven days) produces 33 lines instead of 1017, each carrying how
// many reports have failed and how long it has been going, and the last of them
// lands about four hours rather than days before the end. A recovery is too:
// "no issues" and "the reporting stopped" are different facts, and only one of
// them deserves silence.
//
// A nil *mirrorFaults reports every failure, which is the behaviour this
// replaced: a sink built without a tracker degrades to noisy rather than silent.
type mirrorFaults struct {
	mu           sync.Mutex
	consecutive  int
	sinceMs      int64
	lastReportMs int64
	nextReport   int
}

// mirrorFaultMaxQuietMs is the longest a CONTINUING fault may go unmentioned.
//
// Doubling alone is not enough on its own: after nine or ten reports the gap
// grows past a day, so the tail of a long outage looks exactly like a healthy
// mirror to anyone reading the last day of logs — which is how a fault gets
// missed twice. Six hours means no window of that length during an outage is
// ever silent, while a week-long fault still costs a few dozen lines rather than
// a thousand.
const mirrorFaultMaxQuietMs = 6 * 60 * 60 * 1000

// failed records one failed durable write and reports whether this one should be
// logged. It also returns how many consecutive failures there have now been and
// how long the run has lasted, so the log line can carry both.
func (f *mirrorFaults) failed(nowMs int64) (report bool, consecutive int, lasted time.Duration) {
	if f == nil {
		return true, 0, 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consecutive == 0 {
		f.sinceMs = nowMs
		f.lastReportMs = nowMs
		f.nextReport = 1
	}
	f.consecutive++
	elapsed := time.Duration(nowMs-f.sinceMs) * time.Millisecond
	// Two rules, and a line goes out when EITHER is met. Doubling on the count
	// carries the early burst — the first failure is worth a line immediately,
	// the thousandth is not worth nine hundred more than the hundredth — and the
	// quiet ceiling carries the tail, where doubling alone would go silent for
	// longer than anyone's log window.
	byCount := f.consecutive >= f.nextReport
	byTime := nowMs-f.lastReportMs >= mirrorFaultMaxQuietMs
	if !byCount && !byTime {
		return false, f.consecutive, elapsed
	}
	if byCount {
		f.nextReport *= 2
	}
	f.lastReportMs = nowMs
	return true, f.consecutive, elapsed
}

// pending reports whether a run of failures is in progress. It exists so the
// SUCCESS path — every report on every healthy box, forever — can skip the
// recovery check without reading a clock it has nothing to measure.
func (f *mirrorFaults) pending() bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.consecutive > 0
}

// recovered clears the run and reports what it was, so a mirror that starts
// working again says so once. A mirror that was never failing says nothing.
func (f *mirrorFaults) recovered(nowMs int64) (report bool, consecutive int, lasted time.Duration) {
	if f == nil {
		return false, 0, 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consecutive == 0 {
		return false, 0, 0
	}
	consecutive = f.consecutive
	lasted = time.Duration(nowMs-f.sinceMs) * time.Millisecond
	f.consecutive, f.nextReport = 0, 0
	return true, consecutive, lasted
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
// a rejected report it should stop sending. The live view is correct either way.
//
// What that reasoning does NOT license is treating every failure as transient.
// A transient one re-converges on the next report a minute later; a permanent
// one (a store that cannot be written at all, a column the file does not have)
// never does, and this call site cannot tell them apart from one error. So the
// disposition stays and the REPORTING learns to count: mirrorFaults distinguishes
// the first failure from the thousandth and says how long it has been going.
func (m candidateMirror) ApplyCandidates(relayID string, candidates []wire.DeviceCandidate) error {
	if err := m.registry.ApplyCandidates(relayID, candidates); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), mirrorCandidatesTimeout)
	defer cancel()
	if err := m.st.ReplaceDiscoveredDevices(ctx, relayID, m.rowsFor(relayID, candidates)); err != nil {
		if report, consecutive, lasted := m.faults.failed(m.nowMs()); report {
			if consecutive <= 1 {
				log.Printf("waiveo-feeder: mirroring relay %s's device report FAILED; the live view is unaffected, "+
					"but nothing is being written to the durable mirror and a restart will lose the device list: %v", relayID, err)
			} else {
				log.Printf("waiveo-feeder: mirroring relay %s's device report has now failed %d time(s) in a row over %s; "+
					"this is not re-converging on its own and needs looking at: %v",
					relayID, consecutive, lasted.Round(time.Second), err)
			}
		}
		return nil
	}
	if !m.faults.pending() {
		return nil
	}
	if report, consecutive, lasted := m.faults.recovered(m.nowMs()); report {
		log.Printf("waiveo-feeder: mirroring relay %s's device report is working again after %d consecutive failure(s) over %s",
			relayID, consecutive, lasted.Round(time.Second))
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
			OpenPorts:   c.OpenPorts,
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
// # A REVOKED relay's rows are dropped, not replayed
//
// Revocation is the mechanism that ends a relay's authority to describe this
// site (API-140/142). Before this file, a restart enforced it for free: the
// view was in memory and died with the process. The durable mirror inverted
// that, and closed only the ONLINE half of the gap — candidateMirror.Forget is
// called by relayconn's per-connection revocation watcher, which fires only
// while the relay is CONNECTED. Revoke a relay that is offline (the usual case:
// it is being revoked because it was stolen, decommissioned or compromised),
// or have the feeder down when the watcher would have fired, and the rows
// simply stayed — and came back in GET /devices and GET /entities on every
// subsequent boot, still adoptable. The same held after a transient
// ForgetDiscoveredDevices error, which is logged and never retried, on a relay
// that will never report again to re-converge.
//
// So the restore asks the durable question itself, at the moment it would
// otherwise resurrect the rows: is this relay revoked? A revoked relay's rows
// are DELETED here rather than merely skipped, which makes the answer
// self-healing — the offline-revocation and failed-delete cases both converge
// on the next boot instead of being re-decided forever. A delete that itself
// fails is logged and the rows are still withheld from the registry: the
// revocation is enforced this boot regardless, and the delete is retried the
// next one.
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
			OpenPorts:   d.OpenPorts,
			Entities:    d.Entities,
		})
	}

	relayIDs := make([]string, 0, len(byRelay))
	for id := range byRelay {
		relayIDs = append(relayIDs, id)
	}
	sort.Strings(relayIDs)
	restored := 0
	for _, id := range relayIDs {
		revoked, err := st.IsRevoked(ctx, store.RevocationSubjectRelay, id)
		if err != nil {
			return 0, err
		}
		if revoked {
			log.Printf("waiveo-feeder: relay %s is revoked; dropping its %d mirrored device(s) rather than restoring them",
				id, len(byRelay[id]))
			if err := st.ForgetDiscoveredDevices(ctx, id); err != nil {
				log.Printf("waiveo-feeder: clearing revoked relay %s's mirrored devices failed (they stay out of the "+
					"device list this boot; retried on the next): %v", id, err)
			}
			continue
		}
		if err := registry.ApplyCandidates(id, byRelay[id]); err != nil {
			return 0, err
		}
		restored += len(byRelay[id])
	}

	adopted, err := st.List(ctx, store.KindAdoptedDevice, store.ListFilter{})
	if err != nil {
		return 0, err
	}
	for _, row := range adopted {
		registry.MarkAdopted(row.ID)
	}

	// The operator's IGNORE decisions, re-applied the same way and for the same
	// reason: they are held beside the relay views, not on the mirrored rows, so a
	// restart must re-teach them before the console renders. Unlike the adopted
	// rows these are not a resource Kind — they are a lightweight app-side table
	// (store ignoreddevices.go) — so they are listed by their own accessor.
	ignoredIDs, err := st.ListIgnoredDeviceIDs(ctx)
	if err != nil {
		return 0, err
	}
	for _, id := range ignoredIDs {
		registry.MarkIgnored(id)
	}
	return restored, nil
}
