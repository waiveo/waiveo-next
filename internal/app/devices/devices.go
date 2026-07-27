// Package devices is the app-side device registry: the adopted physical devices
// behind each site's relay and the entities those devices expose (api/openapi.yaml's
// `devices` and `entities` families).
//
// It is a READ model, not an authoring store. A device and its entities enter this
// registry through the relay's own discovery and adoption plane (relay/1 Device
// plane) — never through an api/1 create — so this package exposes no revision, no
// optimistic-concurrency envelope, and no HTTP-facing write path. What the app
// genuinely needs from it is two things the device plane cannot work without:
//
//   - the list surface GET /api/v1/devices and GET /api/v1/entities serve; and
//   - the resolution an entity-addressed command needs — which relay owns this
//     entity_id — since relay/1 REL-112 addresses a device operation by a single
//     already-resolved entity and the app peer must know which of its connections
//     to dispatch that command down.
//
// Rows are held in memory. The relay is the authority for what exists on its own
// LAN and re-reports it on every connection (REL-110/111's full-set replace), so a
// durable copy on this side would be a second, staler authority for data the
// relay hands over again anyway. The pipeline that populates this registry from a
// relay's own `device.candidates` reports is a separate concern; this package is
// the shape that pipeline fills and the api layer reads.
package devices

import (
	"fmt"
	"sort"
	"sync"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// Device is one adopted physical device behind a relay — the openapi Device
// schema, field for field. Labels are the key→value map api/1's label-selector
// grammar evaluates (API-040/042).
//
// ExternalID is api/1's client-assignable identity slot (API-100/104): it is
// carried in every representation unchanged, and is a pointer so an unset one
// serializes as JSON null rather than an empty string that a round trip would
// silently turn into a set-but-blank value.
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

// Registry holds the current device and entity view. Safe for concurrent use:
// the api layer reads it from request goroutines while the connection layer
// refreshes it from a relay's reports.
type Registry struct {
	mu       sync.RWMutex
	devices  map[string]Device
	entities map[string]Entity
}

// New builds an empty registry.
func New() *Registry {
	return &Registry{
		devices:  map[string]Device{},
		entities: map[string]Entity{},
	}
}

// PutDevice inserts or replaces a device by its id.
//
// Both ids are required to be canonical ULIDs (data-model/1 DAT-005a): the id is
// the keyset-pagination sort key api/1's cursor convention pages by (API-034), so
// a non-ULID id would page out of order, and relay_id is what selects the
// connection a command travels down. A rejected row is not stored — a registry
// that quietly accepted a malformed id would surface it later as an
// unroutable command or a corrupt page boundary.
func (r *Registry) PutDevice(d Device) error {
	if !ulid.Valid(d.ID) {
		return fmt.Errorf("devices: device id %q is not a canonical ULID", d.ID)
	}
	if !ulid.Valid(d.RelayID) {
		return fmt.Errorf("devices: device %s relay_id %q is not a canonical ULID", d.ID, d.RelayID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.devices[d.ID] = d
	return nil
}

// PutEntity inserts or replaces an entity by its id, under the same id rules
// PutDevice applies, plus its owning device's.
func (r *Registry) PutEntity(e Entity) error {
	if !ulid.Valid(e.ID) {
		return fmt.Errorf("devices: entity id %q is not a canonical ULID", e.ID)
	}
	if !ulid.Valid(e.DeviceID) {
		return fmt.Errorf("devices: entity %s device_id %q is not a canonical ULID", e.ID, e.DeviceID)
	}
	if !ulid.Valid(e.RelayID) {
		return fmt.Errorf("devices: entity %s relay_id %q is not a canonical ULID", e.ID, e.RelayID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entities[e.ID] = e
	return nil
}

// Devices returns every device in id (ULID, therefore creation) order — the
// stable keyset order api/1's pagination pages through (API-034).
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
// lookup a command dispatch runs first: an entity nobody has adopted has no
// relay to dispatch through and no device class to resolve the command against,
// so the command is refused here rather than sent to a guessed relay.
func (r *Registry) Entity(id string) (Entity, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entities[id]
	return e, ok
}
