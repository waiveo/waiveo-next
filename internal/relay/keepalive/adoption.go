package keepalive

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// mediaPlayerClass is the device-class-registry/1 class whose vocabulary owns
// the `launch` command (REG-066) this package dispatches. An adopted entity of
// any other class has no channel to re-launch, so it is not a keep-alive
// target even though it is genuinely adopted.
const mediaPlayerClass = "media-player"

// AdoptionSet answers keepalive's adoption gate — "may this relay drive this
// entity?" — from the app peer's own adopted-device statement, and keeps that
// answer current as generations are applied.
//
// # Why the gate exists at all
//
// Everything else in this package is about deciding WHEN a screen needs its
// channel re-launched. This decides WHETHER the screen is ours to touch, and
// it is a different question with a different failure mode.
//
// Keepalive's targets come from the relay's ECP target list, which is an
// addressing fact: "these are Rokus this relay can reach." Adoption is a
// policy statement the app peer authors and ships down in the signed snapshot
// (relay/1 REL-063 — "the adoption decision the app authors and ships down,
// never a copy of what a relay discovered"): "these are the screens this
// deployment drives." During the coexistence period those two sets are
// deliberately NOT equal — the legacy stack is still running its own
// watchdog against screens on the same LAN, and this relay can reach every one
// of them.
//
// Two controllers re-launching one Roku is not a redundant no-op. It is an
// observed flapping failure: each relaunch drops the other's session, both
// watchdogs then see a screen that is not where they left it, and the TV
// cycles. Addressability is therefore not permission, and this type is the
// place that distinction is enforced rather than assumed.
//
// # Fail-closed, deliberately
//
// A zero-value AdoptionSet adopts NOTHING, and IsAdopted on a nil *AdoptionSet
// is false. A relay that has not yet applied a generation cannot know which
// screens are its own, and the safe answer to "may I relaunch a screen I know
// nothing about?" is no. The cost of being wrong in that direction is a screen
// that stays at Home for one more generation-apply; the cost of the other
// direction is a wall of TVs fighting two controllers.
//
// # Currency
//
// Adoption changes whenever an operator adopts or disables a device, which
// reaches this relay as a new desired-state generation. Apply is therefore
// called on every applied generation — boot AND live re-pull — not once at
// wiring time. A set installed only at boot would leave a screen adopted this
// afternoon un-driven until the process restarted, which is precisely the
// staleness class the live-apply path exists to close.
type AdoptionSet struct {
	mu      sync.RWMutex
	adopted map[string]bool
	// generation is the desired-state generation the current set came from,
	// used only to log an actual change and to ignore a stale apply.
	generation int64
}

// NewAdoptionSet returns an empty set — nothing adopted until a generation is
// applied. See the type doc's fail-closed note.
func NewAdoptionSet() *AdoptionSet {
	return &AdoptionSet{adopted: map[string]bool{}}
}

// Apply installs the adopted entity set carried by generation's
// `device_inventory` section, replacing whatever was installed before.
//
// Replacement, not merge: REL-063's section is a complete statement of the
// adopted set, so an entity ABSENT from the new generation has been
// un-adopted, and merging would make un-adoption impossible to express — the
// exact shape of bug that leaves this relay driving a screen an operator
// explicitly handed back to the legacy stack.
//
// A generation not strictly greater than the applied one is ignored, matching
// the monotonicity every other apply path enforces (REL-052/070): a late
// in-flight apply from a superseded generation must not resurrect an old
// adoption set over a newer one.
//
// A malformed entry is skipped, not fatal. The alternative — refusing the
// whole set — degrades toward "nothing is adopted", which silently disables
// this capability fleet-wide over one bad row; skipping degrades toward "this
// one device is not driven", which is both smaller and the fail-closed
// direction for the entity actually affected.
func (a *AdoptionSet) Apply(generation int64, inv wire.DeviceInventory) {
	adopted := adoptedEntityIDs(inv)

	a.mu.Lock()
	defer a.mu.Unlock()
	if generation <= a.generation && a.generation != 0 {
		return
	}
	changed := len(adopted) != len(a.adopted)
	if !changed {
		for id := range adopted {
			if !a.adopted[id] {
				changed = true
				break
			}
		}
	}
	a.adopted = adopted
	a.generation = generation
	if changed {
		log.Printf("keepalive: adoption set from generation %d — %d screen(s) this relay may re-launch", generation, len(adopted))
	}
}

// IsAdopted reports whether entityID is an adopted, enabled media-player this
// relay may drive. It is the func wired into Config.Adopted.
//
// A nil receiver answers false rather than panicking: Config.Adopted is
// REQUIRED, and a wiring mistake that leaves it unset must degrade to "drive
// nothing", never to "drive everything".
func (a *AdoptionSet) IsAdopted(entityID string) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.adopted[entityID]
}

// adoptedEntityIDs decodes REL-063's `devices` array into the set of entity
// ids this relay may drive: every entity of an adopted device that is
// `enabled` AND of the media-player class.
//
// `hidden` is deliberately NOT consulted. It is a console presentation
// decision ("do not list this entity in the UI"), and reading it as "do not
// operate this entity" would silently stop keeping a working screen alive
// because someone tidied a device list.
func adoptedEntityIDs(inv wire.DeviceInventory) map[string]bool {
	adopted := make(map[string]bool, len(inv.Devices))
	for _, raw := range inv.Devices {
		var entry wire.DeviceEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			// Skipped, not fatal — see Apply's own doc.
			log.Printf("keepalive: skipping an unreadable device_inventory entry (%v); its entities are not adopted for keep-alive", err)
			continue
		}
		for _, ent := range entry.Entities {
			if ent.EntityID == "" || !ent.Enabled || ent.DeviceClass != mediaPlayerClass {
				continue
			}
			adopted[ent.EntityID] = true
		}
	}
	return adopted
}
