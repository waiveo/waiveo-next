// Package state holds the rules/1 entity-snapshot and observation types: the
// per-tick input every trigger and condition evaluates against. Entity is a
// point-in-time snapshot of one entity (its canonical state string plus its
// typed attribute values); Observation is the classification RUL-330 defines
// of a transition between two such snapshots — whether the canonical state
// string changed, and which (if any) attributes changed in a way that
// contributes to the aggregate any-attribute-changed signal.
package state

import (
	"reflect"
	"sort"

	"github.com/maaxton/waiveo-next/internal/rules/registry"
)

// Entity is a snapshot of one entity at an instant: its identity, the device
// class it belongs to (every entity belongs to exactly one, per
// device-class-registry/1), its canonical state string, and its attribute
// values keyed by attribute name. A nil Attributes is equivalent to empty.
type Entity struct {
	ID          string
	DeviceClass string
	State       string
	Attributes  map[string]any
}

// Observation is one tick's classification of a transition into Entity, the
// current snapshot (RUL-330): whether the canonical State string changed
// since the prior snapshot, and which attributes both changed value and are
// declared `significant` for Entity.DeviceClass (device-class-registry/1
// REG-044) — the aggregate any-attribute-changed signal RUL-330 defines. A
// `cosmetic` attribute's value change is never listed here, though an
// attribute-scoped trigger/condition naming that attribute by name still
// evaluates its own transition directly from Entity.Attributes, unaffected
// by this classification (RUL-022/023, device-class-registry/1 REG-045).
type Observation struct {
	Entity       Entity
	StateChanged bool
	ChangedAttrs []string
}

// NewObservation classifies the transition from prev to curr — snapshots of
// the same entity at two successive instants — per RUL-330, consulting reg
// for curr.DeviceClass's change_emission declarations
// (device-class-registry/1 REG-044). ChangedAttrs lists, in sorted order,
// every attribute name present in curr.Attributes whose value differs from
// prev.Attributes (including one absent from prev entirely) and is declared
// `significant`; a nil prev.Attributes or curr.Attributes is treated as
// empty. Value equality uses reflect.DeepEqual so a composite attribute
// value (an array or nested map) is compared structurally, not just by
// identity or a shallow scalar check.
func NewObservation(reg registry.Registry, prev, curr Entity) Observation {
	obs := Observation{
		Entity:       curr,
		StateChanged: prev.State != curr.State,
	}
	for name, cv := range curr.Attributes {
		if pv, existed := prev.Attributes[name]; existed && reflect.DeepEqual(pv, cv) {
			continue
		}
		if reg.ChangeEmission(curr.DeviceClass, name) == "significant" {
			obs.ChangedAttrs = append(obs.ChangedAttrs, name)
		}
	}
	sort.Strings(obs.ChangedAttrs)
	return obs
}
