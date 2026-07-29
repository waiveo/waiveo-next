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
// make the app adopt one, re-place another relay's device, or write anything an
// api/1 selector or authorization decision is taken from.
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
type Device struct {
	ID          string            `json:"id"`
	ExternalID  *string           `json:"external_id"`
	RelayID     string            `json:"relay_id"`
	DeviceClass string            `json:"device_class"`
	Name        string            `json:"name"`
	ScopeNode   string            `json:"scope_node"`
	Labels      map[string]string `json:"labels"`
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

	// Merged view, rebuilt from views on every write. Reads never touch views.
	devices  map[string]Device
	entities map[string]Entity
}

// New builds an empty registry whose rows are placed at siteScopeNode.
func New(siteScopeNode string) *Registry {
	return &Registry{
		site:     siteScopeNode,
		views:    map[string]*relayView{},
		devices:  map[string]Device{},
		entities: map[string]Entity{},
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
			devs[id] = d
		}
		for id, e := range v.entities {
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
