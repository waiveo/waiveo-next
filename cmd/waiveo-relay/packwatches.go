package main

// packwatches.go is the REL-064 JOIN: pack-declared discovery patterns arrive
// in every signed desired-state snapshot's device_inventory section, and until
// this file existed the relay only COUNTED them (relaystatus) — the watch set
// was a hardcoded literal, so installing a pack that declares how its devices
// look changed nothing about what any relay swept for. Now every inventory
// apply derives the patterns into lane watches and installs them, so a pack
// install/remove changes discovery at the very next generation, no restart —
// on every relay the deployment has, including separately-installed ones,
// because the patterns ride the same signed channel everything else does.

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/discovery"
	"github.com/maaxton/waiveo-next/internal/relay/mdns"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// Drivers pattern-derived candidates are reported under. A pattern carries a
// device class and match forms (MAN-070/071) and NO driver — the driver names
// the protocol namespace its native_id is injective within (REL-153), and for
// a pattern-derived watch that protocol is the discovery lane itself: an SSDP
// hit's native_id is its USN, an mDNS hit's its instance name. Binding a
// pattern to a CONTROL driver (the way the builtin roku:ecp watch carries
// roku-ecp) is the driver-binding question the plan tracks separately; until
// it lands, pattern-discovered devices are listable and adoptable but not
// drivable, and that is stated here rather than guessed around.
const patternSSDPDriver = "ssdp"

// packPattern is one REL-064 pack_match_patterns element: manifest/1's device
// contribution reduced to `{deviceClass, match}` (the producer's shape —
// internal/app/store readPackMatchPatterns).
type packPattern struct {
	DeviceClass string            `json:"deviceClass"`
	Match       []json.RawMessage `json:"match"`
}

// patternWatchSets derives lane watch sets from the section's raw patterns.
//
// Every match is parsed under MAN-071's exactly-one-form rule
// (deviceplane.ParseMatch); the form routes it to a lane. macOui matches are
// valid, undeliverable, and COUNTED — no lane implements MAC-OUI matching yet
// (plan gap G2) — so the caller can log what was declared and not delivered
// rather than silently absorbing it. Malformed entries are counted for the
// same reason: the section was signed and hash-checked, so a malformed
// pattern means a producer bug, and a log line beats a silent skip.
func patternWatchSets(patterns []json.RawMessage) (ssdpW []discovery.Watch, mdnsW []mdns.Watch, macOui, malformed int) {
	for _, raw := range patterns {
		var p packPattern
		if err := json.Unmarshal(raw, &p); err != nil || p.DeviceClass == "" {
			malformed++
			continue
		}
		entities := []deviceplane.CandidateEntity{{Key: mainEntityKey, DeviceClass: p.DeviceClass}}
		for _, rawMatch := range p.Match {
			m, err := deviceplane.ParseMatch(rawMatch)
			if err != nil {
				malformed++
				continue
			}
			switch {
			case m.SSDP != "":
				ssdpW = append(ssdpW, discovery.Watch{
					Match:       m,
					Driver:      patternSSDPDriver,
					DeviceClass: p.DeviceClass,
					Entities:    entities,
				})
			case m.MDNS != "":
				mdnsW = append(mdnsW, mdns.Watch{
					Match:       m,
					Driver:      mdnsDriver,
					DeviceClass: p.DeviceClass,
					Entities:    entities,
				})
			default:
				macOui++
			}
		}
	}
	return ssdpW, mdnsW, macOui, malformed
}

// mergeSSDPWatches appends pattern-derived watches to the builtin set,
// BUILTIN-FIRST: a pattern whose search target collides with a builtin watch
// is dropped, because the builtin carries declaration-side facts a pattern
// cannot (a control driver, a default port, an identify probe wired in main)
// and losing those to a colliding pack declaration would make installing a
// pack DEGRADE discovery of the very devices it declares. The builtin set
// shrinks as extraction moves those facts into pack declarations; the
// precedence rule is what makes that migration safe to do one watch at a time.
func mergeSSDPWatches(builtin, derived []discovery.Watch) []discovery.Watch {
	taken := make(map[string]bool, len(builtin))
	out := append([]discovery.Watch(nil), builtin...)
	for _, w := range builtin {
		taken[w.Match.Key()] = true
	}
	for _, w := range derived {
		if taken[w.Match.Key()] {
			continue
		}
		taken[w.Match.Key()] = true
		out = append(out, w)
	}
	return out
}

// mergeMDNSWatches is mergeSSDPWatches for the mDNS lane. Collision keys fold
// case the same way the lane's own lookup does (RFC 1035 §2.3.3): two
// spellings of one service type are one watch, not two.
func mergeMDNSWatches(builtin, derived []mdns.Watch) []mdns.Watch {
	taken := make(map[string]bool, len(builtin))
	out := append([]mdns.Watch(nil), builtin...)
	for _, w := range builtin {
		taken[foldMDNSKey(w)] = true
	}
	for _, w := range derived {
		if taken[foldMDNSKey(w)] {
			continue
		}
		taken[foldMDNSKey(w)] = true
		out = append(out, w)
	}
	return out
}

func foldMDNSKey(w mdns.Watch) string {
	return "mdns:" + strings.ToLower(w.Match.MDNS)
}

// discoveryWatchApplier returns the per-inventory-apply hook that installs the
// generation's pattern-derived watches into both lanes (REL-064). Either lane
// may be nil (its env switch is off); the hook still logs what the generation
// declared, so a deployment with discovery off can see what it is missing.
//
// The hook runs on every apply — boot's is covered by the lanes' initial
// sets, and every live apply lands here via rePuller.applyInventory — and a
// full replace per apply is idempotent, so re-applying an unchanged
// generation is harmless (REL-070 suppresses the re-run anyway).
func discoveryWatchApplier(disc *discovery.Discoverer, mdnsL *mdns.Listener, builtinSSDP []discovery.Watch, builtinMDNS []mdns.Watch) func(wire.DeviceInventory) {
	return func(inv wire.DeviceInventory) {
		ssdpW, mdnsW, macOui, malformed := patternWatchSets(inv.PackMatchPatterns)
		nSSDP, nMDNS := 0, 0
		if disc != nil {
			nSSDP = disc.SetWatches(mergeSSDPWatches(builtinSSDP, ssdpW))
		}
		if mdnsL != nil {
			nMDNS = mdnsL.SetWatches(mergeMDNSWatches(builtinMDNS, mdnsW))
		}
		// Undeliverable declarations are SAID, not absorbed: a pack may declare
		// an mDNS match on a deployment that never opted into the 5353 bind
		// (the mDNS lane is env-gated — binding multicast is the deployment's
		// call, not a pack's), exactly as macOui is declared with no lane
		// implementing it yet. Either way the operator reading this line can
		// tell "the pack is wrong" from "the relay cannot deliver it here".
		undeliveredMDNS := 0
		if mdnsL == nil {
			undeliveredMDNS = len(mdnsW)
		}
		log.Printf("waiveo-relay discovery watches: %d ssdp, %d mdns live (%d pack pattern(s); %d mdns undeliverable — lane off, %d macOui not yet implemented, %d malformed)",
			nSSDP, nMDNS, len(inv.PackMatchPatterns), undeliveredMDNS, macOui, malformed)
	}
}
