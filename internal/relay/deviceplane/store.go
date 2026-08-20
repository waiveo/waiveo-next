package deviceplane

import (
	"sync"
	"unicode/utf8"

	"github.com/maaxton/waiveo-next/internal/relay/lanaddr"
	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
)

// This file carries the candidate Store: the relay's view of the devices its
// own discovery has observed but that are not (or no longer) adopted, and the
// full-set device.candidates report the app peer replaces its prior view with
// (contracts/relay-1.md REL-110/110a/110b/111/111a).
//
// A candidate is one DEVICE, not one pattern hit. REL-110 says the relay
// reports "devices its own discovery has observed", and REL-110a gives each
// candidate the `(driver, native_id)` half of the identity tuple REL-153 scopes
// a device_id to. That is why this store keys by identity (REL-111a) rather
// than by `match`: a LAN with two Rokus answers ONE declared search target, so
// a match-keyed store would collapse both devices into a single candidate and
// the app peer could never list, address, or adopt either of them
// individually. `match` is retained on each candidate as the provenance of the
// sighting — which declared pattern found it — never as its identity.

// Provenance records how a candidate came to the relay's attention (REL-110):
// discovered by the relay's own listeners, or manually asserted by an operator.
type Provenance string

const (
	// ProvenanceDiscovered marks a candidate the relay's own discovery observed.
	ProvenanceDiscovered Provenance = "discovered"
	// ProvenanceManual marks a candidate an operator asserted by hand.
	ProvenanceManual Provenance = "manual"
)

// Status is a candidate's lifecycle position (REL-110): pending (observed,
// not acted on), adopted (promoted to an entity), or ignored (suppressed
// until ignored_until).
type Status string

const (
	// StatusPending is an observed, not-yet-acted-on candidate.
	StatusPending Status = "pending"
	// StatusAdopted is a candidate promoted to an adopted entity.
	StatusAdopted Status = "adopted"
	// StatusIgnored is a candidate suppressed until IgnoredUntil.
	StatusIgnored Status = "ignored"
)

// IgnoredForever is REL-110's literal ignored_until value meaning a candidate
// is suppressed with no expiry (as opposed to a Timestamp-ms expiry).
// Bounds on what a sighting off the LAN may contain. They mirror the app-side
// intake caps (internal/app/devices) deliberately: a candidate that could not
// survive intake must never reach the store, or it poisons every later report.
//
// maxStoredCandidates additionally keeps a flood from growing a report until it
// exceeds the relay connection's frame limit — which would tear the connection
// down and take state sync, command dispatch and pairing with it, rather than
// being refused. It is far above any real site's device count.
const (
	maxObservationFieldBytes = 256
	maxObservedEntities      = 256
	maxStoredCandidates      = 1024
)

// observationFieldOK reports whether one identity/label field off the LAN is
// safe to store: within the byte cap and valid UTF-8. An EMPTY value passes —
// several of these are optional, and the caller checks the required ones.
func observationFieldOK(v string) bool {
	return len(v) <= maxObservationFieldBytes && utf8.ValidString(v)
}

const IgnoredForever = "forever"

// ClassUnclassified is the device_class a host carries until something
// recognises it — a first-class value, not a blank, because REL-110a requires a
// non-empty class. It is the ONE generic default every lane mints under and the
// store's merge treats as "not yet learned": a sighting carrying it never
// downgrades a candidate another lane has already classified (keepClass).
const ClassUnclassified = "unclassified"

// ClassRank is how strongly the RECORD behind a sighting asserts what the device
// IS — the class's answer to the question NameRank answers for the name, and
// authored in the same place for the same reason: beside the mDNS service-type
// table in hostmdns, where the knowledge lives, then carried up on the
// Observation to the one point every lane converges.
//
// # WHY THIS LADDER EXISTS AT ALL, AND WHY IT IS NOT NEW KNOWLEDGE
//
// The relay already knew every value below. hostmdns.classAuthority has ranked
// service types since #203, and betterClass has picked between them on
// (authority, concreteness). What it did NOT do was remember the verdict: the
// `host` struct holding it was declared INSIDE Lane.sweep, so the rank was
// discarded the moment the sweep returned, and keepClass below — which is the
// merge that decides what SURVIVES a sweep — only ever saw the class token.
// Two sweeps of one device therefore compared as specific-vs-specific and the
// newer one simply won.
//
// That made the class a function of THE RECORDS ONE SWEEP HELD rather than of
// the device, and sweeps have been measured dropping records nondeterministically
// (see nameRankFor's `_display._tcp` note: three consecutive dumps 12s apart
// disagreed about one host's services). Measured on box .12 by replaying a real
// `avahi-browse -a -t -r -p -k` through the real rules: 192.168.50.43 advertises
// `_matter._tcp` (smart-home) and `_spotify-connect._tcp` (media-player), both at
// Feature. Drop the single `_spotify-connect` record from a sweep and the pick is
// smart-home — which the old keepClass took as a genuine reclassification, and
// which the app peer's durable mirror then wrote to DISK. The device_class
// governs the command vocabulary (REG-052), so this is not a wrong label; it is
// a device that loses commands and stops matching class-targeted automation,
// silently, because one UDP record was missing from one 30-second window.
//
// # THE LADDER IS AUTHORITY ONLY; CONCRETENESS IS THE SECOND KEY
//
// betterClass compares two things and only ONE of them belongs here. Authority —
// is this the device's own product service, or a feature bolted onto it — is a
// fact about the mDNS SERVICE TYPE, and the service type is dropped by every
// layer above hostmdns. Concreteness is a pure function of the class TOKEN, so
// any layer holding the token can re-derive it (ClassConcreteness below, and the
// app peer's own restatement of it). Only the half that cannot be re-derived
// needs to be remembered and carried, which is why this ladder has three values
// and not six.
//
// # THE REFRESHABILITY CONSTRAINT APPLIES HERE TOO
//
// NameRank's hard rule — A RANK MAY NOT SIT ABOVE ANY RANK A CONTINUOUSLY
// SWEEPING LANE CAN PRODUCE UNLESS ITS OWN SOURCE SWEEPS TOO — is a property of
// ranked merges, not of names. Both ranks below are minted by hostmdns, which
// re-reads the whole avahi cache every 30 seconds, and by the declared lanes,
// which re-observe on every packet. Nothing here is claimable once and then
// unrestatable, so a device that genuinely changes what it advertises is
// reclassified rather than pinned.
//
// That holds for the DECLARED source only because of the withdrawal seam, and
// the claim is worth nothing without it. A pack's watch re-observes for as long
// as the pack is installed, but a pack that is REMOVED stops observing — and
// silence is indistinguishable from a quiet device, so no rule reading sightings
// can retract what it said. Its Product rank would simply out-rank every
// hostmdns sighting for the rest of the process. RetainDeclarations is where
// that retraction happens, for the same reason and at the same seam as the
// entity fan-out's.
type ClassRank int

const (
	// ClassRankNone is "this sighting has no opinion about what the device is":
	// the neighbour and port-scan lanes, which know only that a host exists and
	// mint ClassUnclassified, and any lane not yet taught to rank. It is the ZERO
	// VALUE on purpose — a lane added later that sets a DeviceClass and forgets
	// the rank can fill a gap and can never displace a ranked classification.
	ClassRankNone ClassRank = iota
	// ClassRankFeature is a service any KIND of device may implement, including
	// fabric membership: `_matter`/`_hap` (a Matter/HomeKit fabric), `_airplay`
	// and `_spotify-connect` (media features), `_smb`/`_afpovertcp` (any computer
	// shares files). Every one of those is a lab sighting on a device of a
	// different kind — the ecobee at 192.168.39.241 advertises THREE of them —
	// which is why a feature can classify a device nothing else recognises and
	// must never outrank what the device says about itself.
	ClassRankFeature
	// ClassRankProduct is the device's OWN product service, advertised because of
	// what the device is rather than as a feature bolted onto something else
	// (`_ecobee`, `_home-assistant`, `_ipp`), or a pack's DECLARED watch — a
	// human wrote down "a device answering this pattern is a media player", which
	// is the strongest statement this relay ever has about a device's kind.
	ClassRankProduct
)

// ClassConcreteness ranks how much a class narrows what an operator can DO with
// a device — device_class governs the command vocabulary (REG-052). A media
// player, a printer and a storage box each name a concrete function; smart-home
// is the broad bucket a bare fabric membership lands in; the generic default and
// the empty string name nothing.
//
// It is EXPORTED and it lives here rather than in hostmdns because it is the
// second key of every ranked class comparison in this process — the sweep's own
// pick (hostmdns.betterClass) and the cross-sweep merge (keepClass) must use one
// definition or the two disagree about the same pair of tokens. It is also the
// half of the rank that is DERIVABLE from the value, which is what lets the app
// peer restate it instead of needing a second wire member for it.
func ClassConcreteness(class string) int {
	switch class {
	case "", ClassUnclassified:
		return 0
	case "smart-home":
		return 1
	default:
		return 2
	}
}

// classAuthorityOf is the rank a class token is ALLOWED to carry, and it exists
// because a rank is a statement about the RECORD BEHIND a class — and behind a
// class of zero concreteness there is no record.
//
// ClassUnclassified is not a classification, it is the absence of one: every
// lane mints it for a host it has not recognised, and REL-110a's requirement
// that `device_class` be non-empty is the only reason it is spelled as a value
// at all. A sighting that recognised nothing cannot be authoritative about
// nothing, so its rank is ClassRankNone whatever the caller stamped.
//
// Without this floor the ladder inverts. Both declared lanes —
// discovery.Watch.observation and mdns's watch sighting — stamp ClassRankProduct
// on the watch's DeviceClass UNCONDITIONALLY, deliberately, so a pack can
// correct an inference; and `unclassified` is a legal Class identifier under
// device-class-registry/1 REG-010 (`^[a-z][a-z0-9-]*$`, nothing reserves the
// sentinel), so a pack declaring that class mints (unclassified, Product) — a
// generic default that OUTRANKS every real classification on the LAN and pins
// the candidate there. The guard this whole function exists to preserve,
// defeated by the rank meant to strengthen it.
//
// It is applied at this store rather than in each lane on purpose: Observe is
// the one point every lane converges on, and a floor duplicated per lane is a
// floor a lane added later forgets. The two writes below (the re-sighting merge
// in keepClass, and the first-sighting store) are the only places a ClassRank
// enters this process's memory.
func classAuthorityOf(class string, rank ClassRank) ClassRank {
	if ClassConcreteness(class) == 0 {
		return ClassRankNone
	}
	return rank
}

// keepClass merges a re-sighting's device_class by ranking WHOSE STATEMENT IT IS
// and only then how specific it is — betterClass's own ordering, applied across
// sweeps instead of within one.
//
// It returns the PAIR, for keepName's reason: a class stamped with another
// sighting's rank is a corrupted ladder entry and a silent one.
//
// The comparison is the pair (classAuthorityOf(class, rank),
// ClassConcreteness(class)) taken lexicographically, with same-or-better
// winning. Every rule the old presence-shaped merge had falls out of it, which
// is why there are no special cases left:
//
//   - THE GENERIC DEFAULT STILL ONLY FILLS A GAP. ClassUnclassified is
//     (None, 0), the bottom of both keys — floored there by classAuthorityOf
//     rather than trusted to arrive there — so it wins only against another
//     (None, 0) and can never take a class some lane learned. That is the
//     original guard, unchanged in effect.
//   - A GENUINE RECLASSIFICATION STILL LANDS. Two equally-sourced, equally
//     concrete classes — a media player that is now reported as a printer —
//     compare equal and the newer sighting wins on recency, exactly as keepName
//     lets a rename land at equal rank.
//   - AN EQUAL-AUTHORITY DOWNGRADE IS NOW REFUSED, and this is the fix.
//     192.168.50.43's two records are BOTH Feature, so authority ties and
//     concreteness decides: media-player (2) holds against smart-home (1), and a
//     sweep that happens to miss `_spotify-connect` changes nothing.
//   - AUTHORITY IS COMPARED FIRST, and that ordering is load-bearing in the
//     opposite direction. The ecobee at 192.168.39.241 holds smart-home from
//     `_ecobee` at Product against media-player from `_airplay` at Feature.
//     Concreteness alone would call the thermostat a media player; only
//     authority-first keeps it a thermostat.
//
// # THE CONCRETENESS TIEBREAK LIVES HERE AND ONLY HERE, and that is a decision
//
// The third rule above is a bet: that a same-authority class change is more
// likely a sweep that dropped a record than a device that changed kind. The bet
// is sound HERE and only here, because here it is BOUNDED — this store dies with
// the process, and a process spans tens of sweeps, which is the timescale over
// which a dropped-record explanation is the likely one. The app peer's durable
// mirror deliberately does NOT restate it (internal/app/store.keepClassFact):
// the identical bet held at DISK lifetime never expires, so a device that
// genuinely stops advertising its more concrete service could never be
// reclassified again by anything. One heuristic, at the one layer that can
// bound it, with the layer that cannot deferring to this one.
//
// THE ACCEPTED COST, stated because it is real and because it is what that bound
// buys: a device that PERMANENTLY stops advertising its more concrete or
// better-sourced service keeps its old class for the life of this process, and
// so does a device whose pack CORRECTS a declared class to a less concrete one
// under the same match key (an equal-authority restatement, refused by the same
// rule). Both land on the next relay restart, and neither reaches disk as a
// permanent state. That is keepName's own accepted cost one field over, bounded
// the same way — every rank here is one a sweeping lane restates.
func keepClass(next string, nextRank ClassRank, held string, heldRank ClassRank) (string, ClassRank) {
	nextRank, heldRank = classAuthorityOf(next, nextRank), classAuthorityOf(held, heldRank)
	if nextRank != heldRank {
		if nextRank > heldRank {
			return next, nextRank
		}
		return held, heldRank
	}
	if ClassConcreteness(next) >= ClassConcreteness(held) {
		return next, nextRank
	}
	return held, heldRank
}

// NameRank is how good the SOURCE of a discovered name is — and "source" means
// the RECORD that authored the string, never the lane that carried it.
//
// That distinction is the whole point, and it was learned from a real device.
// One onn 4K box on the lab LAN alternated between "onn. 4K Streaming Box" and
// "onn.-4K-Streaming-Bo", and both strings came from ONE lane, ONE sweep, ONE
// avahi dump: `_androidtvremote2._tcp` announces the box's display name, while
// `_googlecast._tcp` announces a 20-char truncated hostname form. A rank keyed
// on the lane scores those identically and the loser still wins by arriving
// later. The same is true inside the SSDP lane, which returns an owner-set name
// or a factory model string from one probe depending on the device. So the rank
// is authored where the knowledge lives — beside the mDNS service-type table in
// hostmdns, beside the element precedence in ecp — and travels on the
// Observation to the one place every lane converges.
//
// Ranking by the SHAPE of the string instead (a space beats a hyphen, longer
// beats shorter) was rejected on evidence from the same LAN: an ecobee
// advertises "Upstairs" and "ecobee-ares", and shape picks the machine name;
// six of the twelve named hosts on the box have no space in their name at all.
// Shape is a per-string proxy for a per-source fact, so it also has no memory —
// two equally-shaped names still flap. It survives only as a tiebreak WITHIN a
// rank, where hostmdns already uses it.
//
// The zero value is the SAFE one. A lane added later that sets a Name and
// forgets the rank gets NameRankNone, which can fill a gap but can never
// displace a better-sourced name — the same "the convergence point owns the
// policy so a later lane inherits it" argument Observe makes for the address
// check below.
//
// # THE LADDER'S HARD CONSTRAINT: EVERY RANK MUST BE REFRESHABLE
//
// A rank is a licence to REFUSE other sources, and a held rank is remembered
// until this process exits. So a rank that a source can CLAIM but that nothing
// on this relay ever RESTATES is a permanent pin: the first sighting that
// reaches it freezes the name against every lane that keeps sweeping, and the
// device can never be renamed. That is the mirror image of #198 and no better —
// a stale name and a flapping one are both "the console does not show what the
// device is called".
//
// The first draft of this ladder had one, and it took a live probe to see it.
// The top rank was NameRankOwner, minted only from ECP's `user-device-name`
// (192.168.50.31 answers `<user-device-name>The Hanger</user-device-name>` —
// read off the device). But the SSDP lane PROBES only inside an operator's
// `discovery.scan`: Discoverer.Run is passive-only by owner decision, and
// identityOf's passive branch replays a cached answer with no TTL check at all.
// So `Owner` was claimed once, at the first scan, and then nothing could restate
// it — every 30-second mDNS sweep of a renamed device reported the new name at
// `Friendly` and was refused, forever.
//
// The rule that falls out, and the one to check before adding a rank: A RANK MAY
// NOT SIT ABOVE ANY RANK A CONTINUOUSLY SWEEPING LANE CAN PRODUCE UNLESS ITS OWN
// SOURCE SWEEPS TOO. Every rank below is reachable by hostmdns, which re-reads
// the whole avahi cache every 30 seconds, so every rank can be displaced by a
// device restating itself. The scan-gated ECP probe therefore contributes at
// `Friendly` and `Model` — ranks hostmdns also mints — rather than above them.
type NameRank int

const (
	// NameRankNone is "this sighting has no opinion about the name" — the
	// neighbour lane, which sets no name at all; a lane that has not been taught
	// to rank one; and a value a lane REMEMBERS rather than just observed
	// (discovery.identityOf's cache replay), which is a name worth keeping and
	// not a statement worth ranking.
	NameRankNone NameRank = iota
	// NameRankMachine is a label the device derived MECHANICALLY rather than one
	// anybody chose: an id (a Matter node, a Spotify Connect endpoint), a
	// hostname form, or a LOSSY REWRITE of a chosen name. Google Cast appends a
	// 32-hex tail to a 20-char truncation of the hostname; `_display._tcp`
	// truncates to 20 characters and appends a `-XXX` disambiguator. Better than
	// nothing, worse than the intact name the same device announces elsewhere.
	NameRankMachine
	// NameRankModel is a factory/model string — ECP's `default-device-name`, a
	// printer's IPP instance. It identifies the product, not the unit, so it
	// loses to a name somebody actually gave the device.
	NameRankModel
	// NameRankFriendly is a record whose label IS the name somebody chose for the
	// device: an AirPlay or Android-TV-remote instance, a HomeKit accessory, and
	// ECP's `user-device-name`/`friendly-device-name` read from the device's own
	// configuration. This is the TOP of the ladder on purpose — see the
	// refreshability constraint above. Adding a rank after this one is the
	// change that constraint exists to stop; factrank_test.go reads this block
	// and fails if one appears.
	NameRankFriendly

	// NOTHING GOES HERE. A rank declared below NameRankFriendly outranks it, and
	// the only source that could mint one is the scan-gated ECP probe. Read the
	// refreshability constraint on NameRank before adding one anyway.
)

// keepName merges a re-sighting's name by ranking WHOSE STATEMENT IT IS, which
// is keepClass's shape generalised from one field to the fact underneath it:
// rank decides whose statement counts, recency decides what that source says.
//
//   - Same-or-better rank always wins, immediately. A genuine rename is the
//     device restating itself through the record it always announced — the
//     owner renames the onn, `_androidtvremote2` says "Living Room" on the next
//     30s sweep, same rank, and it lands. This is exactly keepClass's "between
//     two specific classes the newer sighting wins (an actual reclassification)".
//   - A strictly worse-ranked name never displaces a better-ranked one. That is
//     the fix: the Cast instance name stops overwriting the display name every
//     time the remote service happens to be missing from an avahi dump.
//   - An empty name never wins, which is orKeep's existing property, preserved.
//
// A RENAME MUST ALWAYS LAND, and that requirement is what shapes the ladder
// above rather than this function. Refusal is only ever safe because every rank
// is one that a lane sweeping every 30 seconds can produce, so the top of the
// ladder is always being restated: the renamed device says its new name at the
// same rank as its old one and wins on recency. Take that property away — put a
// rank at the top that only a scan-gated probe can mint — and this same `>=`
// silently becomes a permanent pin. The bug is in the ladder; this function just
// obeys it. Adding a rank means re-reading the refreshability constraint on
// NameRank, not this comment.
//
// Deliberately NOT a time-decay window ("the better source has been quiet for
// N minutes, let the worse one through"). A decay needs an honest freshness
// signal and this merge has none: it sees strings, not observation times, and
// one lane hands it a REMEMBERED value indistinguishable from a fresh one (a
// passive SSDP sighting replays a cached ECP answer — internal/relay/discovery
// identityOf). That replay is answered at the source instead, by ranking a
// recalled name NameRankNone: memory keeps its ability to fill an empty slot and
// loses its ability to out-shout a lane that just looked. Wanting a real decay
// means first carrying the OBSERVATION time of the fact up from every lane,
// which is a separate change with its own evidence to gather.
func keepName(next string, nextRank NameRank, held string, heldRank NameRank) (string, NameRank) {
	if next == "" {
		return held, heldRank
	}
	if nextRank >= heldRank {
		return next, nextRank
	}
	return held, heldRank
}

// keepAddress merges a re-sighting's address without letting a lane that sees
// only a host overwrite one that saw a host AND a port.
//
// This is the same defect as the name, on the field with teeth. The SSDP lane
// reads a LOCATION and reports "192.168.50.31:8060"; the neighbour and host-mDNS
// lanes report the bare "192.168.50.31" they read out of the kernel table and
// the avahi cache. Both are non-empty, so a presence-only merge let the last
// sweep win and the port was gone within 30 seconds — on the lab box, 61 of 61
// mirrored rows carried a bare address. It is harmless there only by the
// accident that a missing port falls through to the Roku driver's own default;
// a device on a non-standard port, or a driver with no default, is dialed at the
// wrong place forever.
//
// A port is a strict information SUPERSET of the same host without one, so this
// needs no source table — unlike the name, the ranking fact is in the value.
// But it ranks only within ONE endpoint: an address naming a DIFFERENT host is a
// device that moved (DHCP renewed its lease), which is new reachability and must
// land immediately even when it arrives bare. What is protected is the port, not
// the address.
func keepAddress(next, held string) string {
	if next == "" {
		return held
	}
	if held == "" {
		return next
	}
	nextHost, nextPort, nextOK := lanaddr.Split(next)
	heldHost, heldPort, heldOK := lanaddr.Split(held)
	if !nextOK || !heldOK || nextHost != heldHost {
		// A genuine move (or a shape this package cannot compare): the newest
		// sighting is the best answer available about where the device is.
		return next
	}
	if nextPort == 0 && heldPort != 0 {
		return held
	}
	return next
}

// keepMatch merges the sighting's `match` — MAN-071's record of WHICH DECLARED
// PATTERN found the device — on keepClass's rule rather than last-writer.
//
// A MacOui match is not really a pattern that found anything: MACIdentity mints
// one mechanically for every MAC-resolved sighting, so it is the Match analogue
// of ClassUnclassified — the generic default every lane stamps when it has
// nothing more specific to say. An SSDP or mDNS match names an actual declared
// search target a device answered. Letting the generic one overwrite the
// specific one on the next neighbour sweep would replace the provenance of the
// sighting with the provenance of whoever swept last, which is the same shape as
// the class flicker and is refused the same way.
//
// STATED PLAINLY: THIS GUARD HAS NO LIVE INSTANCE TODAY, and saying so is the
// point. On a MAC-resolvable segment discovery.canonicalize rewrites an SSDP
// sighting's Match to the MAC form BEFORE it reaches this merge — deliberately,
// so the sighting lands on the neighbour lane's key — so a specific match never
// arrives on a key a generic one can also reach. All 61 mirrored rows on box .12
// are driver `net` + MAC, which is that path. The declared match survives only
// for a cross-subnet SSDP device, and no MacOui sighting reaches those keys. So
// this is a rule waiting on a caller (canonicalize keeping the declared match,
// which is a separate argument), not a fix for something observed. It is kept
// because it is correct and free, and it is written down as unexercised because
// a guard nothing authors is how this subsystem keeps shipping half a fix.
func keepMatch(next, held Match) Match {
	if matchRank(next) >= matchRank(held) {
		return next
	}
	return held
}

// matchRank orders the three MAN-071 match forms by how much they say about how
// a device was found: nothing, the mechanical MAC default, or a declared search
// target the device actually answered.
func matchRank(m Match) int {
	switch {
	case m.SSDP != "" || m.MDNS != "":
		return 2
	case m.MacOui != "":
		return 1
	default:
		return 0
	}
}

// candidatesMessageType is the device.candidates envelope type (REL-110).
const candidatesMessageType = "device.candidates"

// CandidateEntity is one addressable object a candidate device exposes
// (REL-110a): the device-native Key the relay addresses it by, the DeviceClass
// governing its command vocabulary, and its last observed State when known.
//
// Key is the relay's own addressing handle. It is NOT an entity_id: the app
// peer derives that from the key and the device's identity (REL-110b), and this
// relay derives the identical value to resolve an inbound command
// (Store.ResolveEntity).
//
// Attributes is the driver-observed detail behind State (REG-064: power_mode,
// active_app, app_type, ...), carried upward beside it so an operator can see
// WHAT a screen is doing and not only that it is on. Like State it is written
// only by the poller (SetEntityObservation), never by a discovery sighting.
type CandidateEntity struct {
	Key         string            `json:"key"`
	DeviceClass string            `json:"device_class"`
	State       string            `json:"state,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
}

// Observation is one sighting of a physical device, as a discovery lane reports
// it into the Store. It is the input side of a Candidate: everything the relay
// LEARNED, with none of the lifecycle state the Store itself maintains.
//
// Driver and NativeID are the two site-scoped halves of REL-153's identity
// tuple and together key the store (REL-111a). Match is the declared pattern
// that produced the sighting (MAN-071). Entities are the addressable handles
// the driver exposes for a device of this class.
// Address, Model and Serial are the REACHABILITY-and-identification half a
// sighting can carry, and all three are optional in the same way and for the
// same reason: they are learned, not asserted by the identity tuple.
//
// Address is the dialable "host:port" the responder pointed at (an SSDP
// LOCATION, or the packet's source paired with the watch's declared control
// port). Without it a candidate can be listed and adopted and then never
// commanded — an adopted device with nowhere to send a command is the defect
// this field exists to close.
//
// Model and Serial come from probing that address over the driver's own
// protocol (for Roku, ECP's `/query/device-info`). A device that answers is
// confirmed to speak the driver the watch declared; one that does not is still
// a real sighting, reported with these empty rather than dropped.
type Observation struct {
	Match       Match
	Provenance  Provenance
	Driver      string
	NativeID    string
	DeviceClass string
	// ClassRank says how strongly the record behind DeviceClass asserts what the
	// device is, so the merge can refuse a worse-sourced classification of the
	// same device (keepClass). A lane that sets DeviceClass and leaves this zero
	// gets ClassRankNone: its class can fill a gap and can never displace a
	// ranked one — the same safe zero value NameRank has, and the reason a lane
	// added later inherits the policy instead of having to remember it.
	ClassRank ClassRank
	Name      string
	// NameRank says how good the record that authored Name is, so the merge can
	// refuse a strictly worse statement about the same device (keepName). A lane
	// that sets Name and leaves this zero gets NameRankNone: its name can fill a
	// gap and can never displace a better-sourced one.
	NameRank NameRank
	Address  string
	Model    string
	Serial   string
	// OpenPorts is what an active port scan found listening. Nil means "this
	// observation did not scan", which is NOT the same as "nothing is open" — see
	// the merge rule in mergeInto.
	OpenPorts []int
	Entities  []CandidateEntity
	// EntitySource names the DECLARATION that authored Entities — the declared
	// watch's Match.Key(), set by the two lanes that sweep for a declared pattern
	// and left empty by every lane that merely finds hosts.
	//
	// It exists because "Entities is empty" is two different sentences and the
	// merge has to tell them apart. The neighbour, port-scan and host-mDNS lanes
	// know a host exists and nothing about what it exposes, so they say nothing;
	// a watch that a pack has narrowed to a smaller fan-out says something, and
	// it happens to be shorter. Keying the merge on the DECLARATION rather than
	// on the list's length lets an entity-less sweep leave the fan-out alone
	// while a re-declaration still replaces it in full, including down to none.
	//
	// It is also the handle RetainDeclarations pulls when a pack is REMOVED,
	// which is the case no sighting can express: the withdrawn watch simply
	// stops observing, and silence is indistinguishable from a device being
	// quiet. Nothing here expires a candidate, so without that call a removed
	// pack's entities would stay reported — and stay command-resolvable — for
	// the rest of this process's life.
	EntitySource string
}

// identityKey is the store's key: REL-153's `(driver, native_id)` pair under
// the same NUL-separated spelling the app-side device row's own duplicate check
// uses (internal/datamodel.checkDeviceEntry), so the two planes agree on when
// two records name one device. The site is not part of the key because a relay
// serves exactly one site.
// identityKey is the store key for one REL-153 identity.
//
// It length-prefixes each part rather than joining on a separator, for the same
// reason deviceid.Device does: a NUL is valid UTF-8 and may appear inside a
// native_id off the LAN, so ("a", "b\x00c") and ("a\x00b", "c") would share a
// separator-joined key while deriving DIFFERENT device ids. Two records that the
// derivation calls distinct must not collide here, or a report is refused as
// "one device, two candidates" for devices that are genuinely two.
func (o Observation) identityKey() string { return identityKeyOf(o.Driver, o.NativeID) }

// identityKeyOf is the injective join both planes key on — the SHARED one, so
// the relay and the app cannot drift on when two records name one device.
func identityKeyOf(driver, nativeID string) string {
	return deviceid.IdentityKey(driver, nativeID)
}

// Candidate is one entry in a device.candidates report (REL-110/110a): a
// discovery Match, how it was learned (Provenance), its lifecycle Status, an
// IgnoredUntil that is present if and only if Status is ignored (a Timestamp-ms
// string or the literal "forever"), first/last-seen Timestamp-ms marks, and the
// device identity and entity fan-out REL-110a adds. Serialized to the corpus's
// field order/shape.
type Candidate struct {
	Match        Match             `json:"match"`
	Provenance   Provenance        `json:"provenance"`
	Status       Status            `json:"status"`
	IgnoredUntil *string           `json:"ignored_until"`
	FirstSeen    int64             `json:"first_seen"`
	LastSeen     int64             `json:"last_seen"`
	Driver       string            `json:"driver"`
	NativeID     string            `json:"native_id"`
	DeviceClass  string            `json:"device_class"`
	Name         string            `json:"name,omitempty"`
	Address      string            `json:"address,omitempty"`
	Model        string            `json:"model,omitempty"`
	Serial       string            `json:"serial,omitempty"`
	OpenPorts    []int             `json:"open_ports,omitempty"`
	Entities     []CandidateEntity `json:"entities"`
	// NameRank is how good the source of Name was, remembered so the NEXT
	// sighting can be refused if it is worse (keepName).
	//
	// It is EXPORTED, and the reason overturns what this comment used to say.
	// The old reading was that the rank is relay-local memory that must never
	// reach the wire, because REL-110a fixes the candidate's member set. That
	// was only ever half true: the member set is fixed for the members REL-110a
	// NAMES, and REL-004 licenses an additive optional one — which is how
	// `address`, `model` and `serial` already got here. Keeping the rank local
	// made the name the one ranked fact that dies at a relay restart: the
	// process's whole map is re-minted, so the first post-restart report is
	// whatever lane swept first, and the app peer's durable mirror had no way to
	// tell that report from a better one. Measured on the lab box, the address
	// survived two relay restarts (the mirror ranks it) and the name did not.
	// So the rank now travels — REL-110c — and cmd/waiveo-relay's
	// toWireCandidates is the one place it is projected onto the wire.
	//
	// `json:"-"` all the same, and that is a DELIBERATE DIVERGENCE from
	// wire.DeviceCandidate rather than an oversight. This type's tags are the
	// frozen REL-110 corpus's shape (conformance/corpora/relay-1), and the rank
	// on the wire is a TOKEN whose ordering is deliberately not on the wire
	// (wire.CandidateNameRank*), while in here it is an ordered int the merge
	// compares. Encoding the int would put a shape on the corpus oracle that no
	// peer ever parses; the projection states the token instead, field by field,
	// which is what that projection is for.
	NameRank NameRank `json:"-"`
	// ClassRank is how strongly the record behind DeviceClass asserted it,
	// remembered so the NEXT sighting can be refused if it is worse (keepClass).
	//
	// It is here for the reason NameRank is, one step further along: the rank
	// used to live only inside a hostmdns sweep, so it did not survive even to
	// the NEXT SWEEP of the same process, let alone a relay restart. Carrying it
	// on the candidate is what makes keepClass a cross-sweep merge instead of a
	// re-decision from scratch every 30 seconds.
	//
	// `json:"-"` for exactly the reason NameRank carries it: this type's tags are
	// the frozen REL-110 corpus's shape (conformance/corpora/relay-1), which two
	// gates diff as a full JSON tree, and the rank on the wire is a TOKEN whose
	// ordering is deliberately not on the wire (wire.CandidateClassRank*).
	// Encoding the int here would put a shape on the corpus oracle that no peer
	// ever parses; cmd/waiveo-relay's toWireCandidates states the token instead.
	// This tag, not `omitempty` over in wire, is what keeps the corpus
	// byte-identical — see the note on wire.DeviceCandidate.ClassRank.
	ClassRank ClassRank `json:"-"`
	// entitySource is the declaration whose fan-out Entities currently holds —
	// the Observation.EntitySource that last stated it. Still unexported, for
	// the reason NameRank no longer qualifies under: this is merge memory with
	// no consumer beyond this process. Nothing downstream can act on it, so
	// putting it on the wire would be a member the app peer stores and never
	// reads — the "KNOWN RESIDUAL" mergeEntities documents needs a member saying
	// a fan-out was WITHDRAWN, which is a different fact than this one.
	entitySource string
}

// CandidatesBody is the device.candidates message body (REL-110): the full
// current set of candidates.
type CandidatesBody struct {
	Candidates []Candidate `json:"candidates"`
}

// CandidatesReport is the full device.candidates message the relay sends the
// app peer (REL-110/111): a type + relay_id envelope wrapping the full-set
// candidate body the peer replaces its prior view of this relay with.
type CandidatesReport struct {
	Type    string         `json:"type"`
	RelayID string         `json:"relay_id"`
	Body    CandidatesBody `json:"body"`
}

// Store is the relay's candidate set, keyed by REL-153 device identity so
// re-observations of the same device dedup rather than accumulate (REL-111a).
// It tracks first-observed order so a full-set Report is deterministic, and is
// safe for concurrent Observe/mutate/Report.
//
// site is the app peer's authoritative site scope node, adopted from hello-ack
// (REL-036) and required only for ResolveEntity — the derivation both peers
// must agree on (REL-110b). Until a site is adopted the store still observes
// and reports; it simply cannot resolve an inbound command yet, which is the
// truthful answer for a relay that has not completed a handshake.
type Store struct {
	mu      sync.Mutex
	relayID string
	site    string
	order   []string              // identity keys in first-observed order
	byKey   map[string]*Candidate // identity key -> candidate
}

// NewStore returns an empty candidate Store that stamps its device.candidates
// reports with relayID (REL-110).
func NewStore(relayID string) *Store {
	return &Store{relayID: relayID, byKey: make(map[string]*Candidate)}
}

// SetSite adopts the app peer's authoritative site scope node (REL-036), the
// third member of REL-153's identity tuple. It is what lets ResolveEntity
// derive the same entity ids the app peer derives (REL-110b). Called on every
// hello-ack, including a redial's.
func (s *Store) SetSite(scopeNode string) {
	s.mu.Lock()
	s.site = scopeNode
	s.mu.Unlock()
}

// Observe records that discovery saw the device o describes at atMs (a
// Timestamp-ms). On first sight it inserts a pending candidate with
// first_seen == last_seen == atMs; a re-observation of the same DEVICE (by
// REL-153 identity) bumps last_seen forward (never backward), never moves
// first_seen, and refreshes the mutable facts a later sighting can legitimately
// correct — the match that found it this time, its self-reported name, its
// class, and its entity fan-out. Lifecycle state (status, ignored_until) is the
// store's own and is never reset by a re-observation.
//
// An observation carrying no identity is DROPPED rather than stored: without
// `(driver, native_id)` there is nothing the app peer could key, list, or
// address it as (REL-110a/153), and a candidate the app peer must discard is
// not one worth reporting.
func (s *Store) Observe(o Observation, atMs int64) {
	if o.Driver == "" || o.NativeID == "" {
		return
	}
	// A sighting's identity comes off UNAUTHENTICATED multicast — an SSDP USN or
	// an mDNS PTR record — so anything on the LAN chooses these bytes. They are
	// validated HERE, at the only layer that knows they are hostile, and a
	// sighting that fails is DROPPED exactly as an identity-less one already is.
	//
	// Dropping is not fastidiousness. The app's intake applies a candidate report
	// all-or-nothing, on the sound ground that a partial replace would delete the
	// devices whose candidates happened to be malformed. But the malformed
	// candidate is attacker-CHOSEN: one spoofed reply carrying an over-long or
	// non-UTF-8 USN would make every subsequent report unapplyable.
	//
	// WHAT THAT COSTS, traced rather than assumed — this sentence used to say it
	// would "blank the whole site's device list", and it does not. The app's
	// intake validates the whole report BEFORE it touches its read model, and the
	// feeder's mirror returns before its durable write, so a refused report leaves
	// both exactly as they were. The real consequence is the other failure mode:
	// the site's device list FREEZES at its last good report, indefinitely and
	// silently, because this store never expires a candidate and re-sends the same
	// poison every minute. Nothing is lost and nothing is ever refreshed —
	// measured as 5 of 5 devices unrefreshed on one over-long token. That is a
	// weaker claim than "blanked" and a stronger argument for stopping the poison
	// here, since here is the only place it can be stopped before it becomes
	// permanent. Its twin lives at internal/app/devices/intake.go and says the
	// same thing; if either moves, both do.
	if !observationFieldOK(o.Driver) || !observationFieldOK(o.NativeID) ||
		!observationFieldOK(o.DeviceClass) || !observationFieldOK(o.Name) ||
		!observationFieldOK(o.Address) || !observationFieldOK(o.Model) || !observationFieldOK(o.Serial) {
		return
	}
	if len(o.Entities) > maxObservedEntities {
		return
	}
	for _, e := range o.Entities {
		if !observationFieldOK(e.Key) || !observationFieldOK(e.DeviceClass) || !observationFieldOK(e.State) {
			return
		}
	}
	// The address gets a second, stronger check than the other fields, because
	// it is the only one this relay ACTS on: AddressFor hands it to the
	// adoption gate, and every command and every state poll for an adopted
	// device is dispatched at whatever it says. A sighting is the one input a
	// spoofer fully controls, so an address that does not name a host on this
	// box's own network is refused here as well as at the lane that parsed it
	// (internal/relay/lanaddr says what that means and why) — this store is
	// where every lane converges, so it is where a lane added later inherits
	// the policy instead of having to remember it.
	//
	// BLANKED rather than dropped, unlike the poison cases above. Those are
	// values that would break the app peer's whole report; this one is a device
	// that genuinely was seen and merely cannot be reached at what it claimed.
	// Blanking also means orKeep below leaves an already-known good address in
	// place, so a spoofed sighting cannot cost a real device its reachability.
	if o.Address != "" && !lanaddr.Dialable(o.Address) {
		o.Address = ""
	}
	if len(s.byKey) >= maxStoredCandidates {
		if _, known := s.byKey[o.identityKey()]; !known {
			// Full, and this is a NEW identity. Refusing the newcomer rather than
			// evicting keeps a flood from displacing the real devices already
			// found; the cap exists so a spoofer cannot grow this store until a
			// report stops fitting in a frame, which closes the connection rather
			// than being refused.
			return
		}
	}
	key := o.identityKey()
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.byKey[key]; ok {
		if atMs > c.LastSeen {
			c.LastSeen = atMs
		}
		// keepMatch, not a bare overwrite: the mechanical MAC default every
		// MAC-resolved lane stamps must not erase the declared search target a
		// device actually answered.
		c.Match = keepMatch(o.Match, c.Match)
		// keepClass, not a bare overwrite, and RANKED rather than merely specific:
		// a class one lane learned must not be downgraded to the generic default
		// by another lane's next sweep, NOR to a worse-sourced specific class by
		// a sweep that happened to miss the record the better one came from.
		// DeviceClass and ClassRank move as a PAIR for keepName's reason — a
		// class stamped with a rank that did not author it is a silent lie to the
		// next merge — so nothing else here may touch either.
		c.DeviceClass, c.ClassRank = keepClass(o.DeviceClass, o.ClassRank, c.DeviceClass, c.ClassRank)
		// Entity STATE is carried across a re-sighting, for exactly the reason
		// the learned facts below are: a discovery sighting never observes state
		// (an SSDP NOTIFY says a device exists, not that it is playing), so the
		// incoming entity list has a blank State for every entity. Replacing the
		// slice wholesale would therefore delete what the POLLER observed
		// (SetEntityState) on every multicast packet — and since a live LAN
		// announces constantly, an operator would watch the state an entity just
		// reported blink back to empty. The state a sighting does not carry is
		// "not learned here", never "no longer true", so a surviving entity keeps
		// the state already held unless this sighting states a new one.
		//
		// And the LIST itself is replaced only by a lane that HAS a DECLARATION.
		// The entity fan-out comes off the watch a pack declared, so the
		// neighbour, port-scan and host-mDNS lanes — which know a host exists but
		// nothing about what it exposes — all build an Observation with no watch
		// behind it. This used to be an unconditional wholesale replace, so any
		// one of their sweeps DELETED the SSDP watch's declared fan-out, and with
		// it every handle the relay addresses the device by: ResolveEntity and
		// SetEntityObservation both walk this list, so while it was blanked an
		// inbound command resolved to nothing (COMMAND_UNRESOLVED) and a polled
		// state had nowhere to land. It also destroyed the poller's work for good
		// — the carry below is rebuilt from the list, so re-declaring the entity
		// later brought it back stateless.
		//
		// The test is EntitySource, not `len(o.Entities) > 0`, and the difference
		// is a shrink. Length answers "did this sighting bring entities", which
		// conflates the lane that has nothing to say with the watch whose pack
		// narrowed its fan-out to fewer — or to none — so a declared removal could
		// never land and the list could only ever grow. The declaration answers
		// "is this sighting entitled to state the fan-out at all", which is the
		// actual question, and lets a re-declaration replace it in full in both
		// directions. Withdrawal of the whole declaration is a third case neither
		// test can see, because a removed watch stops observing rather than
		// observing nothing; RetainDeclarations handles it from the apply seam.
		if o.EntitySource != "" {
			prevState := make(map[string]string, len(c.Entities))
			prevAttrs := make(map[string]map[string]string, len(c.Entities))
			for _, e := range c.Entities {
				if e.State != "" {
					prevState[e.Key] = e.State
				}
				// Attributes are carried across a re-sighting for the identical
				// reason State is: a discovery packet observes neither, so an
				// incoming entity list has both blank and replacing the slice
				// wholesale would delete what the poller learned.
				if len(e.Attributes) > 0 {
					prevAttrs[e.Key] = e.Attributes
				}
			}
			c.Entities = append([]CandidateEntity(nil), o.Entities...)
			for i := range c.Entities {
				if c.Entities[i].State == "" {
					c.Entities[i].State = prevState[c.Entities[i].Key]
				}
				if len(c.Entities[i].Attributes) == 0 {
					c.Entities[i].Attributes = prevAttrs[c.Entities[i].Key]
				}
			}
			c.entitySource = o.EntitySource
		}
		// The four LEARNED facts are refreshed only when the new sighting
		// actually carries one, unlike the declaration-side facts above which
		// every sighting states in full.
		//
		// An empty value here means "this sighting did not learn it", never
		// "the device no longer has one": a NOTIFY whose LOCATION header is
		// absent or malformed, or an identification probe that timed out while
		// the device was mid-reboot, both arrive as blanks. Overwriting with
		// them would delete a working address — and with it the ability to
		// command an adopted device — because one packet out of hundreds was
		// thin. A genuinely changed address (DHCP moved the device) is non-empty
		// and does overwrite, which is the case that has to keep working.
		//
		// Presence alone turned out to be only half the rule. It defends against
		// a sighting that learned NOTHING; it does nothing about one that learned
		// something WORSE, and two of these four have a strictly-worse form that a
		// live LAN produces constantly — a machine-generated mDNS instance name
		// where a display name exists, and a bare host where a host:port was read.
		// keepName and keepAddress rank those; keepClass ranked the same defect
		// for the class first, and each of them still refuses an empty value, so
		// this is a generalisation of orKeep and not a replacement for it.
		c.Name, c.NameRank = keepName(o.Name, o.NameRank, c.Name, c.NameRank)
		c.Address = keepAddress(o.Address, c.Address)
		// Model and Serial stay presence-only, and #207 confirmed that rather than
		// changing it. They have exactly ONE writer (the ECP identification probe),
		// so no second lane can undercut them, and the only worse-value path is a
		// re-probe of a mid-reboot device answering a lower rung of ecp's model
		// ladder than the first probe read. Ranking that means carrying the ECP
		// element up the same way NameRank is — cheap, but enforcement with no
		// observed instance to enforce against, so it waits for one.
		//
		// #207 was that field's real defect and it was NOT this: ecp was reading
		// the wrong element out of the document entirely, so the best value never
		// reached this merge to be arbitrated. Worth noting which way that cuts —
		// presence-only is what would LET a corrected value replace a stored part
		// number, where a rank would first have had to be taught the old one was
		// worse. It does not cause that replacement: this merge only ever sees what
		// the ECP probe reports, and on box .12 the probe is currently issued for no
		// address at all (zero SSDP watches — see the reachability note on
		// ecp.QueryDeviceInfo). So the stored 100012587 persists here, correctly and
		// by design, until a probe actually reports something else. The trigger that
		// would change this answer is a second lane reporting a model at all;
		// internal/app/store's mergeDiscovered ledger carries it.
		c.Model = orKeep(o.Model, c.Model)
		c.Serial = orKeep(o.Serial, c.Serial)
		// Ports follow the same rule every LEARNED fact here follows, and the
		// reason is the one device_class was taught the hard way: the passive
		// lanes re-observe a device every 30s carrying no ports at all, so
		// overwriting would blank a scan's findings seconds after it made them.
		// A scan that DID look replaces the list wholesale (a port that closed
		// must disappear); an observation that did not look keeps what is known.
		if o.OpenPorts != nil {
			c.OpenPorts = o.OpenPorts
		}
		return
	}
	s.byKey[key] = &Candidate{
		Match:       o.Match,
		Provenance:  o.Provenance,
		Status:      StatusPending,
		FirstSeen:   atMs,
		LastSeen:    atMs,
		Driver:      o.Driver,
		NativeID:    o.NativeID,
		DeviceClass: o.DeviceClass,
		Name:        o.Name,
		Address:     o.Address,
		Model:       o.Model,
		Serial:      o.Serial,
		OpenPorts:   o.OpenPorts,
		Entities:    append([]CandidateEntity(nil), o.Entities...),
		// Remembered from the first sighting so the SECOND one can be ranked
		// against it. A name with no rank behind it is indistinguishable from one
		// nothing vouches for, which is what the merge would then have to assume.
		NameRank: o.NameRank,
		// Likewise for the class, and this is the hop whose absence made #204 a
		// defect rather than a nicety: without it the FIRST sighting's authority
		// was already gone by the time the second arrived, so keepClass had
		// nothing to compare and every sweep re-decided from scratch.
		//
		// Floored through classAuthorityOf on the way in, not merely on the way
		// through keepClass: a first sighting has no held pair to be compared
		// against, so an unfloored (unclassified, Product) would be STORED as
		// written and then reported upward as a generic default with product
		// authority behind it.
		ClassRank: classAuthorityOf(o.DeviceClass, o.ClassRank),
		// Likewise: which declaration authored this fan-out, so RetainDeclarations
		// can drop it when that declaration goes away. A candidate first seen by a
		// lane with no watch records no source, which is right — it has no fan-out
		// to attribute.
		entitySource: o.EntitySource,
	}
	s.order = append(s.order, key)
}

// RetainDeclarations drops every entity fan-out authored by a declaration that
// is no longer live, and reports how many candidates it cleared.
//
// This is the WITHDRAWAL half of the fan-out rule, and it has to be a call
// rather than a merge because withdrawal has no sighting. Entities are
// declaration-side: they come off a pack-declared watch, and every signed
// desired-state apply REPLACES the whole watch set (discovery.SetWatches,
// mdns.SetWatches). Remove or disable the pack and its watch simply stops
// observing — which is byte-for-byte what a device being switched off looks
// like, so no rule reading sightings can tell them apart. Meanwhile the other
// lanes keep re-observing the same host with no declaration behind them, so the
// candidate keeps the withdrawn fan-out; nothing here expires a candidate, and
// the store lives as long as the process. Without this call the relay would go
// on REPORTING a removed pack's entities and, worse, go on RESOLVING inbound
// commands to them (ResolveEntity walks this list) until the next restart.
//
// live is the set of Match.Key() values the apply just installed, across every
// lane, so the caller states the whole world once rather than this store trying
// to guess which lane owns which key. An empty set is meaningful and honest: a
// generation that declares no watches at all clears every declared fan-out.
//
// A candidate with no recorded source is untouched — it never had a declared
// fan-out to lose.
//
// # THE CLASS AUTHORITY IS WITHDRAWN TOO, and #204 is what made that necessary
//
// A declared watch does not only state a fan-out. It states the strongest CLASS
// claim this relay ever has — discovery.Watch.observation stamps
// ClassRankProduct on the watch's DeviceClass, deliberately, so a pack can
// correct hostmdns's inference — and canonicalize re-keys that sighting onto the
// MAC, which is the same store key hostmdns writes. So before #204 the
// withdrawal seam had one declaration-authored fact to withdraw and now it has
// two, and the second one is the one that REFUSES things: left behind, a removed
// pack's Product rank goes on out-ranking every hostmdns sighting for the rest
// of the process, and the class it authored can never be corrected by the lane
// that can actually see the device.
//
// The rank is dropped to ClassRankNone and the CLASS TOKEN IS KEPT, which is the
// whole shape of the fix. Withdrawing a declaration retracts its AUTHORITY, not
// the observation — nothing on the LAN just changed, and blanking the class to
// ClassUnclassified would report a device as unrecognised between this apply and
// the next sweep, which is a worse lie than a stale-but-plausible class. At
// ClassRankNone the next sweeping lane to say anything at all outranks it, so
// the field re-converges within one sweep; the concreteness half of keepClass
// still stops the neighbour lane's generic default from taking it in the
// meantime.
//
// It is keyed on entitySource, the only declaration handle a candidate carries.
// That is exact for a candidate whose class the declaration authored, and
// conservative for one whose class hostmdns won: the latter is down-ranked
// without cause, and hostmdns re-states its rank on the next 30-second sweep and
// takes the field straight back. A conservative down-rank self-heals; a stale
// authority does not, which is why the imprecision is on this side.
func (s *Store) RetainDeclarations(live map[string]bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cleared := 0
	for _, c := range s.byKey {
		if c.entitySource == "" || live[c.entitySource] {
			continue
		}
		c.Entities, c.entitySource = nil, ""
		c.ClassRank = ClassRankNone
		cleared++
	}
	return cleared
}

// orKeep returns next when it carries a value, else the value already held.
// See Observe's re-observation branch for why a blank never overwrites.
func orKeep(next, held string) string {
	if next != "" {
		return next
	}
	return held
}

// SetEntityObservation records what a driver has OBSERVED for one entity of one
// candidate device — its canonical state (REL-110a: `state` present only once
// the relay has observed one) and the driver-level attributes behind it
// (REG-064) — so both ride the next full-set `device.candidates` report upward
// and become what an operator reads off the app peer's entity list.
//
// The two travel together, in one call, because they are one observation: a
// state and a set of attributes written by separate calls could be halves of
// two different polls, and an operator reading "standby" beside "active_app:
// Netflix" has no way to know which one is current.
//
// entityID is the app-peer-visible id, resolved the same way ResolveEntity
// resolves a command's target: by re-deriving every id this relay's own
// candidates could have (REL-110b) and matching. Nothing here trusts an
// identifier off the wire — the caller is this relay's own poller, and the
// derivation is the check.
//
// It reports false when no candidate of this relay derives to entityID (or no
// site has been adopted yet, so nothing derives at all). An IGNORED candidate
// is deliberately NOT excluded: suppression is an instruction not to ACT on a
// device, and continuing to report what a suppressed device is doing is
// exactly what lets an operator decide to un-suppress it.
func (s *Store) SetEntityObservation(entityID, entityState string, attrs map[string]string) bool {
	if !observationFieldOK(entityState) {
		return false
	}
	// Every attribute key and value is held to the same bound the state string
	// is: this map is reported upward verbatim, and an unbounded one would let a
	// driver grow a report until it no longer fits a frame.
	for k, v := range attrs {
		if !observationFieldOK(k) || !observationFieldOK(v) {
			return false
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.site == "" || entityID == "" {
		return false
	}
	for _, key := range s.order {
		c := s.byKey[key]
		for i, e := range c.Entities {
			if deviceid.Entity(s.site, c.Driver, c.NativeID, e.Key) == entityID {
				c.Entities[i].State = entityState
				// A copy, not the caller's map: the poller reuses its own
				// snapshot maps, and a shared reference would let a later poll
				// mutate what this candidate reports mid-report.
				if len(attrs) == 0 {
					c.Entities[i].Attributes = nil
				} else {
					cp := make(map[string]string, len(attrs))
					for k, v := range attrs {
						cp[k] = v
					}
					c.Entities[i].Attributes = cp
				}
				return true
			}
		}
	}
	return false
}

// AddressFor is the relay-local address book behind an adopted device: it
// returns the last observed LAN address of the candidate with this REL-153
// identity, if this relay has seen one.
//
// It is the join that makes adoption actionable. The app peer adopts a device
// by its `(driver, native_id)` identity and ships that decision down in
// `device_inventory` (REL-063); nothing in that section is dialable, because
// nothing in it could be — the app peer has never been on this LAN. This relay
// HAS, so it is the only party that can turn the adopted identity back into an
// endpoint, and this is the lookup that does it.
//
// It reports ok=false for an unknown identity, for one this relay has an entry
// for but no address (a lane that cannot see one), and — deliberately — for an
// IGNORED candidate, for the same reason ResolveEntity refuses one: suppressing
// a device is an instruction not to act on it, and handing out its address
// would make the suppression cosmetic.
func (s *Store) AddressFor(driver, nativeID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byKey[identityKeyOf(driver, nativeID)]
	if !ok || c.Status == StatusIgnored || c.Address == "" {
		return "", false
	}
	return c.Address, true
}

// Key returns the store key for one REL-153 identity — what Adopt and Ignore
// name a candidate by.
func Key(driver, nativeID string) string { return identityKeyOf(driver, nativeID) }

// Adopt marks the candidate with the given identity Key adopted and clears any
// ignored_until (REL-110: ignored_until is present only while ignored). A
// key naming no known candidate is a no-op.
func (s *Store) Adopt(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.byKey[key]; ok {
		c.Status = StatusAdopted
		c.IgnoredUntil = nil
	}
}

// Ignore marks the candidate with the given identity Key ignored until `until`
// — a Timestamp-ms string or IgnoredForever. Passing nil is treated as
// IgnoredForever so the REL-110 iff invariant (ignored_until present while
// ignored) always holds. A key naming no known candidate is a no-op.
func (s *Store) Ignore(key string, until *string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.byKey[key]; ok {
		c.Status = StatusIgnored
		if until == nil {
			forever := IgnoredForever
			until = &forever
		}
		c.IgnoredUntil = until
	}
}

// Report returns the relay's full current candidate set as a device.candidates
// message (REL-110/111): every known candidate in first-observed order, with
// REL-110's iff invariant enforced on emit (ignored_until is carried only for
// ignored candidates, cleared to null/absent otherwise). The body's
// candidates array is always non-nil (an empty relay reports an empty set,
// not a null). Each call reflects the complete current view — a full replace,
// not a delta.
func (s *Store) Report() CandidatesReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	cands := make([]Candidate, 0, len(s.order))
	for _, key := range s.order {
		c := *s.byKey[key] // copy — never expose internal pointers
		c.Entities = append([]CandidateEntity(nil), c.Entities...)
		if c.Status != StatusIgnored {
			c.IgnoredUntil = nil
		}
		cands = append(cands, c)
	}
	return CandidatesReport{
		Type:    candidatesMessageType,
		RelayID: s.relayID,
		Body:    CandidatesBody{Candidates: cands},
	}
}

// ResolveEntity is an EntityResolver over the relay's own candidate set: it
// maps an entity_id the app peer addressed back to the candidate device that
// exposes it and that entity's device class (REL-112/113).
//
// It exists because a DISCOVERED device has no adopted record yet, so nothing
// has ever told this relay what id the app peer knows its entities by. REL-110b
// closes that: both peers derive the id from the same identity tuple, so the
// relay can recompute every id it could possibly have been addressed at and
// match on it — an equality check against values this relay derived itself,
// never an identifier it accepted from the wire.
//
// It reports ok=false while no site has been adopted (nothing to derive
// against) and for any id no candidate of this relay derives to — in which case
// the command is refused COMMAND_UNRESOLVED without touching a device, exactly
// as an unknown entity id always is.
//
// An IGNORED candidate resolves to nothing: suppressing a device is an
// instruction not to act on it, and honouring it for listing while still
// executing commands against it would make the suppression cosmetic.
func (s *Store) ResolveEntity(entityID string) (deviceID, deviceClass string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.site == "" || entityID == "" {
		return "", "", false
	}
	for _, key := range s.order {
		c := s.byKey[key]
		if c.Status == StatusIgnored {
			continue
		}
		for _, e := range c.Entities {
			if deviceid.Entity(s.site, c.Driver, c.NativeID, e.Key) == entityID {
				return deviceid.Device(s.site, c.Driver, c.NativeID), e.DeviceClass, true
			}
		}
	}
	return "", "", false
}
