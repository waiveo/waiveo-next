// Package devices is the app-side device registry: the physical devices behind
// each site's relay and the entities those devices expose (api/openapi.yaml's
// `devices` and `entities` families).
//
// It is a READ model, not an authoring store. A device and its entities enter
// this registry through the relay's own discovery plane (relay/1 REL-110/111's
// `device.candidates` report) — never through an api/1 create — so this package
// exposes no revision, no optimistic-concurrency envelope, and no HTTP-facing
// write path. What the app genuinely needs from it is two things the device
// plane cannot work without:
//
//   - the list surface GET /api/v1/devices and GET /api/v1/entities serve; and
//   - the resolution an entity-addressed command needs — which relay owns this
//     entity_id — since relay/1 REL-112 addresses a device operation by a single
//     already-resolved entity and the app peer must know which of its connections
//     to dispatch that command down.
//
// # Listing a discovered device is not adopting it
//
// A device appearing here the moment a relay reports it does NOT make discovery
// equal adoption, and the distinction is the reason this package holds so
// little. The relay is authoritative for what EXISTS on its own LAN; the app is
// authoritative for what has been ADOPTED and under what policy. Poll cadence,
// and per-entity enabled/hidden/display-name/category, are DECISIONS no
// discovery sweep can make (relay/1 REL-063) — they live in the durable,
// authored adopted-device rows the /adopted-devices family writes
// (internal/app/store, internal/app/api/identityrows.go), and NOTHING a relay
// sends can create, alter, or delete one.
//
// So what a report can author here is deliberately bounded to the discovered
// facts themselves: what the device is, what it is called by itself, and what
// entities it exposes. Everything else on a row the app decides — a row's
// identifiers are DERIVED on this side (internal/shared/deviceid), its scope
// placement is the relay's own site node, and its labels and external_id are
// api/1 authored data a relay has no field to set. A hostile or misconfigured
// relay can therefore make the app list a device that is not there; it cannot
// make the app adopt one, or write anything an api/1 selector or authorization
// decision is taken from.
//
// # One thing a relay CAN write, and it is the routing input
//
// That paragraph used to end "…re-place another relay's device, or write
// anything an api/1 selector or authorization decision is taken from", and the
// second half of that is true in a way that misleads. `RelayID` is neither a
// selector nor an authorization input — it is the DISPATCH input, the field that
// decides which relay an operator command is sent to. And a relay can write it:
// candidate views from every relay merge into one map keyed by the derived device
// id and dispatch routes to the entity's RelayID. Under REL-153's original
// most-recent-reporter rule that write was unguarded, which is the capture the
// next paragraph describes and the one after it closes.
//
// So a second ENROLLED relay that reports another relay's `(driver, native_id)`
// captures that entity's commands — including `params`, which REL-114 explicitly
// permits to carry per-dispatch credential material. The tuple is guessable by
// construction: `native_id` is an SSDP USN, discoverable by anything on any
// relay's LAN.
//
// # How it is closed
//
// REL-153a/b now hold a device's routing with its INCUMBENT — the relay
// currently reporting it — and a different relay's report does not take that
// routing while the incumbent is still reporting the same device. Incumbency is
// per device rather than per relay, so a relay that is healthy but no longer
// lists a device it used to has gone silent about THAT device and yields it
// after IncumbencyWindowMs, with no operator action. That is what keeps the two
// ordinary cases — replaced hardware, and a device moved onto another relay's
// network — automatic, while a live incumbent's device is unclaimable.
//
// The scenario was always bounded to an ENROLLED relay, so it was an insider or
// compromised-relay case rather than an open-LAN one; the open-LAN half was the
// kill switch, closed separately.
//
// One consequence is accepted rather than worked around: while an incumbent
// holds a device it has stopped reporting, that device is listed by nobody. The
// incumbent no longer sees it and the only relay that does may not yet speak for
// it, so attributing it to either would assert something this registry cannot
// support. A brief absence is a smaller harm than a device listed under a relay
// that merely guessed its tuple.
//
// # Why rows are held in memory
//
// The relay is the authority for what exists on its own LAN and re-reports it
// on every connection (REL-110/111's full-set replace), so a durable copy on
// this side would be a second, staler authority for data the relay hands over
// again anyway. What IS durable is the adoption record, which is a different
// row in a different store with a different owner.
package devices

import (
	"fmt"
	"sort"
	"sync"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// Device is one physical device behind a relay — the openapi Device schema,
// field for field. Labels are the key→value map api/1's label-selector
// grammar evaluates (API-040/042).
//
// ExternalID is api/1's client-assignable identity slot (API-100/104): it is
// carried in every representation unchanged, and is a pointer so an unset one
// serializes as JSON null rather than an empty string that a round trip would
// silently turn into a set-but-blank value.
//
// RelayID is the relay's enrollment-assigned identity (relay/1 REL-012/014),
// NOT a ULID: the enrollment path mints it, this registry only reads and routes
// by it (openapi RelayId). Per REL-153 it records which relay MOST RECENTLY
// reported this device, and is not part of the device's identity — two relays
// serving one site that both see one device hold one row between them.
//
// Address, Model and Serial are the discovered REACHABILITY-and-identification
// facts (relay/1 REL-110a plus the relay's own identification probe): where the
// relay found the device on its LAN, and what the device said it is when asked.
// They are omitempty because a sighting can legitimately carry none of them —
// but a device with no address is one nothing can command, which is why the
// field is served rather than dropped: an operator can SEE that the relay found
// a device it cannot reach.
//
// Adopted is the app's own decision, not a discovered fact, and it is the one
// member of this row a relay has no influence over at all. It reports whether an
// adopted-device row exists for this device (the durable adoption record the
// desired-state `device_inventory` section compiles from, REL-063) — the answer
// to "is this device under our control", which is the question an operator
// looking at a discovered-device list is actually asking.
type Device struct {
	ID          string            `json:"id"`
	ExternalID  *string           `json:"external_id"`
	RelayID     string            `json:"relay_id"`
	DeviceClass string            `json:"device_class"`
	Name        string            `json:"name"`
	ScopeNode   string            `json:"scope_node"`
	Labels      map[string]string `json:"labels"`
	Address     string            `json:"address,omitempty"`
	Model       string            `json:"model,omitempty"`
	Serial      string            `json:"serial,omitempty"`
	Adopted     bool              `json:"adopted"`
}

// Entity is one addressable object a device exposes — the openapi Entity schema,
// field for field. It is the unit rules/1 entity references resolve to and the
// unit a device command is addressed to (REL-112).
//
// DeviceID names the physical device this entity belongs to. It matters beyond
// provenance: a device fans out to many entities, and the relay serializes
// command dispatch per DEVICE, not per entity (REL-115), so two commands to two
// entities of one device contend for the same physical target.
//
// State is the entity's last reported state, a value from its device class's own
// state vocabulary; it is absent until the relay has reported one.
type Entity struct {
	ID          string            `json:"id"`
	ExternalID  *string           `json:"external_id"`
	DeviceID    string            `json:"device_id"`
	RelayID     string            `json:"relay_id"`
	DeviceClass string            `json:"device_class"`
	Name        string            `json:"name"`
	ScopeNode   string            `json:"scope_node"`
	Labels      map[string]string `json:"labels"`
	State       string            `json:"state,omitempty"`
	// Attributes is the driver-observed detail behind State (relay/1 REL-110a,
	// device-class-registry/1 REG-064): for a Roku, `power_mode`,
	// `active_app`, `app_type` and the rest. Absent until the relay has
	// reported some — an operator sees "this screen is on" from State and
	// "…showing Netflix" only from here.
	Attributes map[string]string `json:"attributes,omitempty"`
}

// relayView is one relay's CURRENT reported set, plus the sequence number of
// the report that established it. The registry holds one per relay rather than
// one merged pile of rows, which is what makes REL-111's full-set replace
// expressible: replacing a relay's view is replacing exactly its own rows, and
// cannot disturb another relay's.
type relayView struct {
	seq      uint64
	devices  map[string]Device
	entities map[string]Entity
}

func newRelayView(seq uint64) *relayView {
	return &relayView{seq: seq, devices: map[string]Device{}, entities: map[string]Entity{}}
}

// Registry holds the current device and entity view: one relayView per relay,
// and the merged rows the api layer reads. Safe for concurrent use — the api
// layer reads it from request goroutines while the connection layer refreshes
// it from a relay's reports.
//
// site is the scope node every row this registry holds is placed at: the
// deployment's own site node, which is also the site_binding a relay adopts
// (REL-036). Placement is an app-side decision — a relay has no field to state
// one — and until a device is adopted, the only placement the app can honestly
// claim for it is the site its relay serves.
type Registry struct {
	mu   sync.RWMutex
	site string
	seq  uint64

	views map[string]*relayView

	// incumbency records, per DEVICE, which relay currently routes it and when
	// that relay last reported it (REL-153a/b). It is what stops a second
	// enrolled relay taking a live device's routing by naming its guessable
	// (driver, native_id) tuple — and what lets one take it anyway once the
	// incumbent has genuinely stopped reporting it.
	incumbency map[string]incumbent

	// nowMs is the clock incumbency windows are measured against. Injected
	// rather than read from the wall, so a test can drive the window's boundary
	// instead of sleeping through it.
	nowMs func() int64

	// adopted is the set of device ids an adoption record exists for. It is held
	// BESIDE the relay views rather than on the rows themselves, for exactly the
	// reason incumbency is: a relay's report replaces its whole view, so anything
	// stored inside a view is erased by the next report. Adoption is the app's
	// own decision and must survive every report — including one that no longer
	// mentions the device at all, which is a device temporarily off the network,
	// not a device un-adopted.
	adopted map[string]bool

	// Merged view, rebuilt from views on every write. Reads never touch views.
	devices  map[string]Device
	entities map[string]Entity
}

// incumbent is one device's current routing holder.
type incumbent struct {
	relayID string
	// lastSeenMs is when the incumbent last REPORTED this device, not when it
	// last spoke at all. A relay that is healthy and reporting, but no longer
	// lists a device it used to, has gone silent about that device (REL-153b).
	lastSeenMs int64
}

// IncumbencyWindowMs is how long a device's routing stays with the relay that
// currently reports it after that relay stops reporting it (REL-153c).
//
// Fifteen minutes. Shorter would hand a device away during an incumbent's
// ordinary restart — both a live-capture opportunity and an operational
// surprise. Much longer would make replacing hardware feel broken and push an
// operator toward whatever manual override exists, which is the outcome an
// automatic rule is meant to avoid.
const IncumbencyWindowMs int64 = 15 * 60 * 1000

// New builds an empty registry whose rows are placed at siteScopeNode.
//
// nowMs is the clock REL-153b's incumbency window is measured against. A nil
// clock is treated as "no clock", which makes every incumbent permanent — the
// safe direction, since it refuses re-homing rather than allowing capture.
func New(siteScopeNode string, nowMs func() int64) *Registry {
	return &Registry{
		site:       siteScopeNode,
		views:      map[string]*relayView{},
		devices:    map[string]Device{},
		entities:   map[string]Entity{},
		incumbency: map[string]incumbent{},
		adopted:    map[string]bool{},
		nowMs:      nowMs,
	}
}

// MarkAdopted records that an adoption record exists for deviceID, so every
// later listing of that device reports it (see Registry.adopted).
//
// It is a PROJECTION of the durable adopted-device row, never the adoption
// itself: the row is written first, in the store, and this only teaches the
// in-memory read model what the store already committed. Called on the adopt
// operation and again for every existing row at boot, which is what makes the
// flag survive a restart even though the rows around it do not.
//
// Marking a device this registry has never heard of is deliberately allowed and
// remembered: an adopted device that is currently powered off will be reported
// again eventually, and the flag has to be waiting for it when it is.
func (r *Registry) MarkAdopted(deviceID string) {
	if deviceID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adopted[deviceID] = true
	r.rematerialize()
}

// now reads the injected clock, or 0 when there is none.
func (r *Registry) now() int64 {
	if r.nowMs == nil {
		return 0
	}
	return r.nowMs()
}

// routableBy reports whether relayID may hold deviceID's routing right now, and
// records the claim when it may.
//
// Called under r.mu with the report's own instant, once per device in the
// reporting relay's view. The three cases are REL-153a/b in full: an unheld
// device is claimed, the incumbent refreshes its own hold, and a challenger is
// refused while the incumbent is inside its window and accepted once it is not.
func (r *Registry) routableBy(deviceID, relayID string, nowMs int64) bool {
	held, ok := r.incumbency[deviceID]
	switch {
	case !ok, held.relayID == relayID:
		r.incumbency[deviceID] = incumbent{relayID: relayID, lastSeenMs: nowMs}
		return true
	case nowMs-held.lastSeenMs > IncumbencyWindowMs:
		// The incumbent has been silent about this device for longer than the
		// window, so the challenger takes it with no operator action — this is
		// the hardware-replacement and device-moved path (REL-153b).
		r.incumbency[deviceID] = incumbent{relayID: relayID, lastSeenMs: nowMs}
		return true
	default:
		return false
	}
}

// Site reports the scope node this registry places its rows at.
func (r *Registry) Site() string { return r.site }

// PutDevice inserts or replaces a single device in its relay's view.
//
// The row's own id MUST be a canonical ULID (data-model/1 DAT-005a): it is the
// keyset-pagination sort key api/1's cursor convention pages by (API-034), so a
// non-ULID id would page out of order and could sit either side of a cursor
// boundary depending on the comparison. RelayID is only required to be present:
// it is minted by the enrollment path, not by this platform's row-id rule, and
// is deliberately not a ULID in the running system.
//
// A rejected row is not stored — a registry that quietly accepted a malformed
// id would surface it later as a corrupt page boundary or an unroutable
// command.
//
// This is a single-row write and therefore does NOT replace the relay's view
// the way a REL-111 report does; ApplyCandidates is that path.
func (r *Registry) PutDevice(d Device) error {
	if !ulid.Valid(d.ID) {
		return fmt.Errorf("devices: device id %q is not a canonical ULID", d.ID)
	}
	if d.RelayID == "" {
		return fmt.Errorf("devices: device %s carries no relay_id — there would be no connection to command it through", d.ID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.viewFor(d.RelayID)
	v.devices[d.ID] = d
	r.rematerialize()
	return nil
}

// PutEntity inserts or replaces an entity in its relay's view, under the same
// rules PutDevice applies, plus its owning device's id.
func (r *Registry) PutEntity(e Entity) error {
	if !ulid.Valid(e.ID) {
		return fmt.Errorf("devices: entity id %q is not a canonical ULID", e.ID)
	}
	if !ulid.Valid(e.DeviceID) {
		return fmt.Errorf("devices: entity %s device_id %q is not a canonical ULID", e.ID, e.DeviceID)
	}
	if e.RelayID == "" {
		return fmt.Errorf("devices: entity %s carries no relay_id — there would be no connection to command it through", e.ID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.viewFor(e.RelayID)
	v.entities[e.ID] = e
	r.rematerialize()
	return nil
}

// viewFor returns relayID's view, creating it, and stamps it as the most
// recent report. Callers hold the write lock.
func (r *Registry) viewFor(relayID string) *relayView {
	v, ok := r.views[relayID]
	if !ok {
		v = newRelayView(0)
		r.views[relayID] = v
	}
	r.seq++
	v.seq = r.seq
	return v
}

// Forget drops a relay's whole view and rebuilds the merged rows without it.
//
// Nothing removed a view before this: a view was written by the intake and
// deleted by nobody, so a relay that disconnected — or one whose enrollment was
// REVOKED — kept serving rows from `/devices` and `/entities` indefinitely, out
// of its last full report, along with that report's memory. Commands against
// those entities did fail typed, so this was stale authority rather than
// misrouting; but a revoked relay continuing to describe the site is exactly the
// thing revocation is for.
//
// It is deliberately NOT called on a brief disconnect by the caller that merely
// notices a connection drop — see the caller for that argument. Forgetting a
// relay that reconnects a second later would blank its devices in between, and a
// device flickering out of the read model is worse than one described a moment
// too long.
//
// Forgetting an unknown relay is a no-op, so a caller may call it without first
// asking whether the relay ever reported.
func (r *Registry) Forget(relayID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.views[relayID]; !ok {
		return
	}
	delete(r.views, relayID)
	r.rematerialize()
}

// rematerialize rebuilds the merged rows from every relay's view, oldest report
// first, so a row two relays both report ends up carrying the RELAY_ID OF THE
// ONE THAT REPORTED IT MOST RECENTLY — REL-153's own rule ("the app peer's own
// records reflect only which relay most recently reported it"), and the answer
// to what is authoritative when two reports of one device disagree.
//
// Deriving rather than mutating in place is what makes a relay going quiet
// correct: a device only disappears when NO relay's current view still holds
// it, so one relay dropping a device it can no longer see does not delete a row
// another relay is still reporting. Callers hold the write lock.
func (r *Registry) rematerialize() {
	order := make([]*relayView, 0, len(r.views))
	for _, v := range r.views {
		order = append(order, v)
	}
	sort.Slice(order, func(i, j int) bool { return order[i].seq < order[j].seq })

	devs := make(map[string]Device, len(r.devices))
	ents := make(map[string]Entity, len(r.entities))
	for _, v := range order {
		for id, d := range v.devices {
			// REL-153a: a later report does not take a live incumbent's
			// routing. Without this line the loop is last-writer-wins, and the
			// last writer is whoever most recently guessed the tuple.
			if held, ok := r.incumbency[id]; ok && held.relayID != d.RelayID {
				continue
			}
			// Stamped here rather than carried on the view's row: the merged row
			// is rebuilt from scratch on every write, so this is the only place
			// a fact that outlives a report can be attached to one.
			d.Adopted = r.adopted[id]
			devs[id] = d
		}
		for id, e := range v.entities {
			if held, ok := r.incumbency[e.DeviceID]; ok && held.relayID != e.RelayID {
				continue
			}
			ents[id] = e
		}
	}
	r.devices, r.entities = devs, ents
}

// Devices returns every device in id order — the stable keyset order api/1's
// pagination pages through (API-034). Ids are derived rather than minted
// (internal/shared/deviceid), so that order is stable and total but not
// chronological; pagination needs the former, and nothing here needs the latter.
func (r *Registry) Devices() []Device {
	r.mu.RLock()
	out := make([]Device, 0, len(r.devices))
	for _, d := range r.devices {
		out = append(out, d)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Entities returns every entity in id order, under the same keyset rule as
// Devices.
func (r *Registry) Entities() []Entity {
	r.mu.RLock()
	out := make([]Entity, 0, len(r.entities))
	for _, e := range r.entities {
		out = append(out, e)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Device resolves one device by id, reporting whether it is known. It is the
// lookup the adopt operation runs first: adopting a device no relay has ever
// reported would file an adoption record against a device that may not exist,
// so the operation is refused here rather than writing one on a client's word.
func (r *Registry) Device(id string) (Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.devices[id]
	return d, ok
}

// Entity resolves one entity by id, reporting whether it is known. This is the
// lookup a command dispatch runs first: an entity no relay has reported has no
// relay to dispatch through and no device class to resolve the command against,
// so the command is refused here rather than sent to a guessed relay.
func (r *Registry) Entity(id string) (Entity, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entities[id]
	return e, ok
}
