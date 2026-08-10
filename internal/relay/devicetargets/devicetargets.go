// Package devicetargets answers the one question standing between an adopted
// device and a command that actually moves it: WHERE do I send this?
//
// The relay already holds both halves of that answer and nothing joined them.
//
//   - The app peer decides what is ADOPTED and ships that decision down in the
//     signed snapshot's `device_inventory` (relay/1 REL-063) — `{device_id,
//     driver, native_id, entities[{entity_id, device_class, enabled, …}]}`.
//     Every identifier a command is addressed by is in there. Nothing dialable
//     is, and nothing could be: the app peer has never been on this LAN.
//   - The relay's own discovery has SEEN the devices on that LAN and knows what
//     address each answered from (deviceplane.Store.AddressFor). It has no
//     opinion at all about which of them anyone adopted — REL-110b is explicit
//     that discovery is not adoption.
//
// A Registry is the intersection, and the intersection is the SAFETY PROPERTY,
// not a convenience: an entity is controllable if and only if the app peer
// adopted it AND enabled it AND this relay can currently locate it. Every Roku
// this relay can see but nobody adopted resolves to nothing and every command
// to it is refused, which is what keeps a second controller — the legacy stack
// still driving the screens that have not been migrated — from finding this one
// fighting it for the same device. That failure mode is not hypothetical; it is
// the specific reason the parity plan writes the gate as "per-screen adoption,
// never a blanket default".
//
// # The override, and why it stays
//
// A deployment may still pin an entity to an address out of band
// (WAIVEO_RELAY_ECP_TARGETS). An override is not adoption-gated, because it IS
// the adoption decision — someone typed it into this relay's own configuration.
// It exists for the cases the discovered path structurally cannot serve: a
// device on a subnet SSDP does not cross, one whose LOCATION is wrong, or a
// bring-up before any adoption record exists at all. It wins over the
// discovered address for the same entity, since a stated fact beats an inferred
// one.
package devicetargets

import (
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// Endpoint is a dialable device address: a host and a port, with 0 meaning
// "the driver's own default". It is deliberately a plain pair rather than one
// of the driver packages' Target types — internal/relay/ecp and
// internal/relay/ecppoll each declare their own, and this package resolves
// endpoints for both without either depending on the other or on this one.
type Endpoint struct {
	Host string
	Port int
}

// AddressSource is the relay's own view of where the devices it has discovered
// answered from. *deviceplane.Store satisfies it directly via AddressFor — the
// identical method signature, no adapter — so this package depends on the
// address-book CAPABILITY rather than on the candidate store's concrete type,
// and a test can supply a two-line fake.
type AddressSource interface {
	AddressFor(driver, nativeID string) (string, bool)
}

// adopted is one entity the app peer has adopted and enabled, as this package
// needs it: the device it belongs to (REL-115's serialization key and the
// identity its address is looked up by) and the class whose vocabulary its
// commands resolve against (REG-052).
type adopted struct {
	deviceID    string
	deviceClass string
	driver      string
	nativeID    string
}

// Registry resolves an entity_id to the endpoint the relay may drive it at,
// and to the device/class a command against it resolves under. It is safe for
// concurrent use: the desired-state re-pull loop replaces the adopted set while
// dispatch, polling, and the state reporter all read it.
type Registry struct {
	mu sync.Mutex

	// overrides is the deployment-stated entity_id → Endpoint map. It is fixed
	// at construction (it comes from the process environment) and is never
	// adoption-gated — see the package doc.
	overrides map[string]Endpoint

	// byEntity is the last applied `device_inventory`, indexed by entity_id and
	// filtered to the entities that are actually drivable: enabled ones only. A
	// disabled entity is an adoption record that says "do not act on this", so
	// it is absent here rather than present-and-refused-later, and no caller can
	// forget to check.
	byEntity map[string]adopted

	addresses AddressSource
}

// New builds a Registry over the deployment's override map (may be nil) and
// the relay's discovered-address source (may be nil, in which case only
// overrides ever resolve — the shape of a relay with no discovery lane on).
// The adopted set starts empty: until a desired-state generation has been
// applied, this relay has been told nothing about what is adopted, and
// refusing every non-overridden entity is the truthful answer.
func New(overrides map[string]Endpoint, addresses AddressSource) *Registry {
	cp := make(map[string]Endpoint, len(overrides))
	for id, e := range overrides {
		cp[id] = e
	}
	return &Registry{
		overrides: cp,
		byEntity:  map[string]adopted{},
		addresses: addresses,
	}
}

// SetInventory replaces the adopted set from a verified snapshot's
// `device_inventory.devices` (REL-063). It is a full REPLACE, matching the
// section's own semantics: a snapshot states the whole adopted set, so a device
// absent from a newer generation has been UN-adopted and must stop being
// drivable the moment that generation applies. Folding entries in as a delta
// would make un-adoption unexpressible.
//
// Entries are raw JSON because the whole section is (the relay re-marshals the
// snapshot to recompute REL-053's hash and a typed round trip would not be
// byte-identical). An entry that does not decode, or that is missing the
// identity a lookup needs, is SKIPPED with the rest of the section still
// applied: the section is already signature-verified, so a malformed entry is a
// producer defect rather than an attack, and refusing the whole inventory over
// one bad row would un-adopt every good device on the site. It returns how many
// entities are drivable, for the boot/apply log line.
func (r *Registry) SetInventory(devices []json.RawMessage) int {
	next := map[string]adopted{}
	for _, raw := range devices {
		var entry wire.DeviceEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		if entry.DeviceID == "" || entry.Driver == "" || entry.NativeID == "" {
			continue
		}
		for _, e := range entry.Entities {
			if e.EntityID == "" || !e.Enabled {
				continue
			}
			next[e.EntityID] = adopted{
				deviceID:    entry.DeviceID,
				deviceClass: e.DeviceClass,
				driver:      entry.Driver,
				nativeID:    entry.NativeID,
			}
		}
	}
	r.mu.Lock()
	r.byEntity = next
	r.mu.Unlock()
	return len(next)
}

// Target resolves entityID to the endpoint a command or a poll goes to, or
// reports ok=false — which every caller MUST treat as "this relay may not drive
// this entity", never as "use a default".
//
// An override wins outright. Otherwise the entity must be BOTH adopted+enabled
// (SetInventory) AND currently locatable by this relay's discovery
// (AddressSource), and an address that does not parse into a host resolves to
// nothing rather than to a half-formed endpoint.
func (r *Registry) Target(entityID string) (Endpoint, bool) {
	r.mu.Lock()
	if ep, ok := r.overrides[entityID]; ok {
		r.mu.Unlock()
		return ep, true
	}
	a, ok := r.byEntity[entityID]
	src := r.addresses
	r.mu.Unlock()

	if !ok || src == nil {
		return Endpoint{}, false
	}
	addr, ok := src.AddressFor(a.driver, a.nativeID)
	if !ok {
		return Endpoint{}, false
	}
	return ParseEndpoint(addr)
}

// Targets is the whole currently drivable set, entity_id → Endpoint: every
// override, plus every adopted+enabled entity this relay can presently locate.
// It is what the pollers are (re)configured from, so that the set of devices
// whose STATE the relay reads is exactly the set whose commands it will accept
// — one gate, not two that can drift.
func (r *Registry) Targets() map[string]Endpoint {
	r.mu.Lock()
	ids := make([]string, 0, len(r.byEntity)+len(r.overrides))
	for id := range r.byEntity {
		ids = append(ids, id)
	}
	for id := range r.overrides {
		if _, dup := r.byEntity[id]; !dup {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()

	out := make(map[string]Endpoint, len(ids))
	for _, id := range ids {
		if ep, ok := r.Target(id); ok {
			out[id] = ep
		}
	}
	return out
}

// ResolveEntity is a deviceplane.EntityResolver over the ADOPTED set: it maps
// an entity_id to its owning device_id (REL-115's per-device serialization key)
// and the device class whose vocabulary the command resolves against
// (REL-112/113).
//
// It complements deviceplane.Store.ResolveEntity rather than replacing it. The
// store resolves what this relay has DISCOVERED, which REL-110b deliberately
// admits so an operator can wake or blink a device while deciding whether to
// adopt it. This one resolves what the app peer has ADOPTED, which is the set
// that survives the device going quiet on discovery for a sweep or two — and,
// unlike the discovered set, it is the set whose device_id is the app peer's
// own row id rather than a locally derived one.
func (r *Registry) ResolveEntity(entityID string) (deviceID, deviceClass string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.byEntity[entityID]
	if !ok {
		return "", "", false
	}
	return a.deviceID, a.deviceClass, true
}

// ParseEndpoint turns an observed LAN address into a dialable Endpoint. It
// accepts the three shapes the relay actually sees:
//
//	http://192.168.50.31:8060/     an SSDP LOCATION URL (Roku's own form)
//	192.168.50.31:8060             a bare host:port
//	192.168.50.31                  a bare host, port left 0 (driver default)
//
// The URL form's PORT is taken, not defaulted: a Roku states its ECP port in
// LOCATION, and a device reachable on a non-standard port would otherwise be
// dialed on 8060 and silently never answer. A URL with no port yields port 0,
// which is the driver's default — the same as the bare-host case, and the right
// answer for a responder that stated a scheme's implied port.
//
// It reports ok=false for anything with no host at all, so an unparseable
// address fails closed at resolution rather than producing a request to "http://:8060".
func ParseEndpoint(addr string) (Endpoint, bool) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return Endpoint{}, false
	}

	if strings.Contains(addr, "://") {
		u, err := url.Parse(addr)
		if err != nil || u.Hostname() == "" {
			return Endpoint{}, false
		}
		port := 0
		if p := u.Port(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n <= 0 {
				return Endpoint{}, false
			}
			port = n
		}
		return Endpoint{Host: u.Hostname(), Port: port}, true
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		// No port present: the whole string is the host. An IPv6 literal
		// without brackets lands here too and is used as-is — net.JoinHostPort
		// at dial time re-brackets it correctly.
		return Endpoint{Host: strings.Trim(addr, "[]")}, true
	}
	if host == "" {
		return Endpoint{}, false
	}
	n, err := strconv.Atoi(portStr)
	if err != nil || n <= 0 {
		return Endpoint{}, false
	}
	return Endpoint{Host: host, Port: n}, true
}
