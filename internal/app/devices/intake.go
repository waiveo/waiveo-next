package devices

import (
	"fmt"
	"unicode/utf8"

	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
	"github.com/maaxton/waiveo-next/internal/shared/macvendor"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// intake.go is the ONE path a relay's `device.candidates` report becomes rows
// on (relay/1 REL-110/110a/110b/111/111a). Everything a relay sends is
// untrusted input, so this file is written as a boundary, not a mapper.
//
// Three properties it holds, and how:
//
//  1. A relay cannot name a row. Nothing here reads an identifier off the wire
//     — wire.DeviceCandidate has no field for one — and every id is DERIVED
//     from REL-153's identity tuple on this side (internal/shared/deviceid).
//     A hostile relay therefore cannot address a row into another relay's
//     device, or into an id an adopted record already uses for something else:
//     the only ids it can reach are the ones its own asserted identity derives
//     to. That the derived value is a canonical ULID (DAT-005a) is structural,
//     not checked — any 128 bits encode to one.
//
//  2. A rejected report changes nothing. The whole report is validated and
//     converted BEFORE the registry is touched, and a single bad candidate
//     rejects the report rather than being skipped. A report is a full-set
//     replace (REL-111): applying the good half of one would install a view
//     that is not the relay's actual view — silently deleting the devices whose
//     candidates happened to be the malformed ones. Refusing outright leaves
//     the prior view exactly as it was, which is both the safe answer and the
//     honest one.
//
//  3. The work is bounded before it is done. Counts and lengths are checked
//     against fixed caps first, so a report cannot make the app peer allocate
//     or hash proportionally to whatever a relay felt like sending. The
//     transport's own frame cap (internal/feeder/relayconn) is the outer bound;
//     these are the inner ones, and they are what keep a single 1 MiB frame
//     from turning into a million derivations.

// Intake caps. Every one of these bounds work done on untrusted input, and each
// is far above any real LAN: a site with more than 4096 discoverable devices, or
// a device exposing more than 256 addressable entities, is not a deployment
// this platform has — it is a relay misreporting or attacking.
const (
	maxCandidatesPerReport = 4096
	maxEntitiesPerDevice   = 256
	maxIdentityFieldBytes  = 256
	maxNameBytes           = 1024

	// maxNameRankBytes bounds REL-110c's `name_rank`. It is an ENUM TOKEN, not
	// free text, and the longest one this build knows is 8 bytes ("friendly") —
	// so this cap is generous for a token a later minor adds and still far too
	// small for anything else. It is a byte bound and NOT a vocabulary check on
	// purpose; the two guards are split, and validateCandidate says why.
	maxNameRankBytes = 32

	// maxClassRankBytes bounds REL-110d's `class_rank`, on the identical
	// reasoning one field over — the longest token this build knows is 7 bytes
	// ("product").
	//
	// It is its OWN constant rather than a reuse of maxNameRankBytes even though
	// the two numbers are equal today. They bound different vocabularies in
	// different requirements; tying them means a later minor that lengthens a
	// name token silently moves the class bound too, which is a limit changing
	// for a reason that has nothing to do with the thing it limits.
	maxClassRankBytes = 32

	// maxOpenPortsPerCandidate bounds the numeric list a relay may report for one
	// device. The scanning lane's own cap is smaller; this is the wire's limit on
	// a report from any relay, trusted to have that lane or not.
	maxOpenPortsPerCandidate = 32
	maxStateBytes            = 256
	// A driver reports a small, fixed set of attributes behind an entity's
	// state (device-class-registry/1 REG-064 names six for a media player).
	// This cap is an order of magnitude above that, and it exists for the same
	// reason every other cap in this file does: the map arrives from a relay,
	// is copied into a row, and is rendered into every entity list read.
	maxEntityAttributes    = 64
	maxAttributeKeyBytes   = 128
	maxAttributeValueBytes = 512
)

// ApplyCandidates replaces the app peer's whole view of relayID with the
// candidate set in one `device.candidates` report (REL-110/111).
//
// relayID MUST be the connection's own AUTHENTICATED relay identity — the mTLS
// client-certificate identity the connection layer established (REL-041/150) —
// never the `relay_id` the frame asserts. A relay that could name another relay
// in its own report would be able to replace that relay's entire device view,
// which is why the caller passes this in rather than this function reading it
// off the report.
//
// The report is applied whole or not at all; the returned error names the first
// reason it was refused, and on error the registry is untouched.
func (r *Registry) ApplyCandidates(relayID string, candidates []wire.DeviceCandidate) error {
	if relayID == "" {
		return fmt.Errorf("devices: a candidate report carries no authenticated relay identity")
	}
	if len(candidates) > maxCandidatesPerReport {
		return fmt.Errorf("devices: relay %s reported %d candidates, over the %d cap", relayID, len(candidates), maxCandidatesPerReport)
	}

	view := newRelayView(0)
	seenIdentity := map[string]bool{}

	for i, c := range candidates {
		if err := validateCandidate(c); err != nil {
			return fmt.Errorf("devices: relay %s candidate[%d]: %w", relayID, i, err)
		}
		// Length-prefixed, not separator-joined: a NUL is valid UTF-8 and can
		// appear inside a native_id off the LAN, so ("a","b\x00c") and
		// ("a\x00b","c") would collide on a joined key while deriving DIFFERENT
		// device ids — refusing a report as "one device, two candidates" for two
		// genuinely distinct devices. This is the ambiguity deviceid length-
		// prefixes against, two lines from the derivation itself.
		identity := deviceid.IdentityKey(c.Driver, c.NativeID)
		if seenIdentity[identity] {
			// Two candidates for one (driver, native_id) is one device claimed
			// twice: REL-153 makes that tuple the identity, so the report does
			// not describe a set the app peer can hold, and guessing which one
			// the relay meant would be inventing.
			return fmt.Errorf("devices: relay %s candidate[%d]: (driver, native_id) %q/%q is reported twice — one device, two candidates (REL-153)",
				relayID, i, c.Driver, c.NativeID)
		}
		seenIdentity[identity] = true

		// An ignored candidate is a device an operator suppressed. Listing it
		// as a live device would make the suppression cosmetic, so it is
		// carried in the report (REL-110 requires that) and deliberately not
		// materialized as a row.
		if c.Status == wire.CandidateStatusIgnored {
			continue
		}

		deviceRowID := deviceid.Device(r.site, c.Driver, c.NativeID)
		view.devices[deviceRowID] = Device{
			ID:          deviceRowID,
			RelayID:     relayID,
			DeviceClass: c.DeviceClass,
			Name:        candidateName(c),
			ScopeNode:   r.site,
			// Labels are api/1 authored data (API-042) and the input to every
			// label selector: a relay has no field to set them and gets an
			// empty map, never a nil one — openapi requires the member present.
			Labels: map[string]string{},
			// The discovered reachability/identification facts, carried
			// through as reported. They are DESCRIPTIVE, not authoritative:
			// nothing here is an identifier, nothing keys a row, and nothing
			// an authorization decision reads — so a relay writing them can
			// mislead an operator's eyes but cannot reach anything the
			// header's threat argument protects.
			Address:   c.Address,
			Model:     c.Model,
			Serial:    c.Serial,
			OpenPorts: c.OpenPorts,
			// Derived here rather than stored, because the input already is:
			// `native_id` is mirrored and is rebuilt on the restore path, so
			// recomputing costs a map lookup and adds no column, no migration
			// and no second copy to drift.
			MAC:    candidateMAC(c),
			Vendor: candidateVendor(c),
		}
		for _, e := range c.Entities {
			entityRowID := deviceid.Entity(r.site, c.Driver, c.NativeID, e.Key)
			view.entities[entityRowID] = Entity{
				ID:          entityRowID,
				DeviceID:    deviceRowID,
				RelayID:     relayID,
				DeviceClass: e.DeviceClass,
				Name:        candidateName(c) + " " + e.Key,
				ScopeNode:   r.site,
				Labels:      map[string]string{},
				State:       e.State,
				// Copied, never aliased: the decoded report is the caller's and
				// this row outlives the call.
				Attributes: copyAttributes(e.Attributes),
				// Kept so the merge can re-compose Name if the device's own name
				// is overlaid from the durable mirror (Registry.rematerialize).
				key: e.Key,
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	view.seq = r.seq

	// Incumbency is decided HERE, from one reading of the clock, before the
	// merge reads it (REL-153a/b). A device this relay may not route is dropped
	// from its own view rather than filtered later: the view is what the merge
	// and every future rematerialize consume, so a row left in it would be
	// re-considered on every subsequent report and would take the device the
	// moment the incumbent's window lapsed — silently completing a capture the
	// relay attempted minutes earlier.
	nowMs := r.now()
	for id := range view.devices {
		if r.routableBy(id, relayID, nowMs) {
			continue
		}
		delete(view.devices, id)
		for eid, e := range view.entities {
			if e.DeviceID == id {
				delete(view.entities, eid)
			}
		}
	}

	r.views[relayID] = view
	r.rematerialize()
	return nil
}

// validateCandidate enforces REL-110/110a's own shape on one untrusted
// candidate. Every check refuses the report rather than repairing the value:
// a repaired identity is a different device than the one the relay reported.
func validateCandidate(c wire.DeviceCandidate) error {
	switch c.Provenance {
	case wire.CandidateProvenanceDiscovered, wire.CandidateProvenanceManual:
	default:
		return fmt.Errorf("provenance %q is not one of %q/%q (REL-110)",
			c.Provenance, wire.CandidateProvenanceDiscovered, wire.CandidateProvenanceManual)
	}
	switch c.Status {
	case wire.CandidateStatusPending, wire.CandidateStatusAdopted, wire.CandidateStatusIgnored:
	default:
		return fmt.Errorf("status %q is not one of pending/adopted/ignored (REL-110)", c.Status)
	}
	// REL-110's iff: ignored_until is present exactly while ignored. A
	// candidate claiming an expiry it is not entitled to, or an ignored one
	// with no expiry, is a report this app peer cannot interpret.
	if (c.IgnoredUntil != nil) != (c.Status == wire.CandidateStatusIgnored) {
		return fmt.Errorf("ignored_until is present iff status is ignored (REL-110); status=%q present=%v",
			c.Status, c.IgnoredUntil != nil)
	}
	if err := checkField("driver", c.Driver, maxIdentityFieldBytes, true); err != nil {
		return err
	}
	if err := checkField("native_id", c.NativeID, maxIdentityFieldBytes, true); err != nil {
		return err
	}
	if err := checkField("device_class", c.DeviceClass, maxIdentityFieldBytes, true); err != nil {
		return err
	}
	// `class_rank` (REL-110d) is bounded and NOT vocabulary-checked, for exactly
	// the reasons the `name_rank` block below sets out at length — same split,
	// same clamp location (internal/app/store.classRankFact), same "an unknown
	// token reads as the bottom of the ladder" direction.
	if err := checkField("class_rank", c.ClassRank, maxClassRankBytes, false); err != nil {
		return err
	}
	if err := checkField("name", c.Name, maxNameBytes, false); err != nil {
		return err
	}
	// `name_rank` (REL-110c) is the first member off this wire that is a LICENCE
	// TO REFUSE rather than a value to render, and it is the only one whose
	// effect outlives the report: the durable mirror stores it and consults it on
	// every later report from every lane. So it gets TWO guards, deliberately
	// split, because they answer different questions and belong in different
	// places.
	//
	// HERE, the bound on SIZE. Everything else off this wire is bounded before
	// it is copied anywhere, and a rank is no different — it lands in a durable
	// column, so an unbounded token would be an unbounded durable write.
	//
	// NOT here, the bound on VOCABULARY, and that is the load-bearing decision.
	// An unrecognised token is NOT a reason to refuse the report. A refusal here
	// throws away the whole candidate set (see this file's header: a full-set
	// replace applied by halves is worse than not applied at all), so one
	// malformed rank from a newer or a hostile relay would cost the site every
	// LATER report too — a much bigger lever than the one it is defending. The
	// vocabulary is clamped instead at the layer that ACTS on the rank
	// (internal/app/store.nameRankFact), where an unknown token reads as the
	// bottom of the ladder: it can fill a gap and can refuse nothing, which is
	// the direction that cannot be weaponised. A relay claiming a rank this
	// build has never heard of therefore gains exactly nothing by it.
	//
	// WHAT A REFUSED REPORT ACTUALLY COSTS, traced rather than assumed. This
	// paragraph used to say a refusal "would blank a site's device list", and
	// that is not what happens — the trade is still right, but the true sentence
	// is a better argument for it. ApplyCandidates validates the WHOLE report
	// before it writes r.views, and the feeder's mirror returns before
	// ReplaceDiscoveredDevices, so a refused report leaves both the live view and
	// the durable rows exactly as they were. Nothing is lost. What happens
	// instead is that the view FREEZES: the relay re-sends the same poisoned
	// candidate every minute, every report is refused, `last_seen` stops
	// advancing, and no surface says why — measured as 5 of 5 devices
	// unrefreshed on a single 33-byte token. Silent and indefinite beats
	// destructive, which is why the bound stays; and it is precisely why the
	// VOCABULARY check must not be here, where an unknown token would freeze a
	// site for the crime of a relay being newer than this build.
	if err := checkField("name_rank", c.NameRank, maxNameRankBytes, false); err != nil {
		return err
	}
	// The three learned facts are optional but still bounded and still
	// UTF-8-checked: they are rendered into a JSON response an operator reads,
	// and checkField's own doc is the argument for why "merely descriptive" is
	// not a reason to skip the check.
	if err := checkField("address", c.Address, maxIdentityFieldBytes, false); err != nil {
		return err
	}
	if err := checkField("model", c.Model, maxIdentityFieldBytes, false); err != nil {
		return err
	}
	// Ports are attacker-controlled like every other field off this wire, and
	// they are the only NUMERIC one — so the bound is on count and range rather
	// than bytes. A relay claiming 60,000 open ports, or port 0, or a negative
	// port, is either broken or hostile; either way the report is refused rather
	// than rendered.
	if len(c.OpenPorts) > maxOpenPortsPerCandidate {
		return fmt.Errorf("open_ports: %d ports reported, at most %d are accepted", len(c.OpenPorts), maxOpenPortsPerCandidate)
	}
	for _, p := range c.OpenPorts {
		if p < 1 || p > 65535 {
			return fmt.Errorf("open_ports: %d is not a port number", p)
		}
	}
	if err := checkField("serial", c.Serial, maxIdentityFieldBytes, false); err != nil {
		return err
	}
	if len(c.Entities) > maxEntitiesPerDevice {
		return fmt.Errorf("%d entities, over the %d cap", len(c.Entities), maxEntitiesPerDevice)
	}
	seenKey := map[string]bool{}
	for j, e := range c.Entities {
		if err := checkField(fmt.Sprintf("entities[%d].key", j), e.Key, maxIdentityFieldBytes, true); err != nil {
			return err
		}
		if err := checkField(fmt.Sprintf("entities[%d].device_class", j), e.DeviceClass, maxIdentityFieldBytes, true); err != nil {
			return err
		}
		if err := checkField(fmt.Sprintf("entities[%d].state", j), e.State, maxStateBytes, false); err != nil {
			return err
		}
		if len(e.Attributes) > maxEntityAttributes {
			return fmt.Errorf("entities[%d] reports %d attributes, over the %d cap", j, len(e.Attributes), maxEntityAttributes)
		}
		for k, v := range e.Attributes {
			// Keys are checked as strictly as values: an attribute NAME is
			// rendered beside its value in an operator's entity list and is a
			// JSON object key in the response, so an unbounded or non-UTF-8 one
			// is the same hazard as an unbounded value.
			if err := checkField(fmt.Sprintf("entities[%d].attributes key", j), k, maxAttributeKeyBytes, true); err != nil {
				return err
			}
			if err := checkField(fmt.Sprintf("entities[%d].attributes[%q]", j, k), v, maxAttributeValueBytes, false); err != nil {
				return err
			}
		}
		if seenKey[e.Key] {
			// Two entities under one key derive to one entity_id, so the second
			// would silently overwrite the first and one of the device's
			// addressable surfaces would vanish.
			return fmt.Errorf("entities[%d].key %q is reported twice on one device", j, e.Key)
		}
		seenKey[e.Key] = true
	}
	return nil
}

// copyAttributes returns an independent copy of a reported attribute map, or
// nil for an empty one — so a row never aliases the decoded report, and an
// entity whose driver reported nothing carries no `attributes` member at all
// rather than an empty object an operator would read as "it has none".
func copyAttributes(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// checkField applies the three checks every string off this wire gets: present
// (when required), within its byte cap, and valid UTF-8.
//
// The UTF-8 check is not cosmetic. These strings are hashed into row ids,
// compared for equality, and rendered into JSON responses; invalid UTF-8 would
// be replaced by U+FFFD somewhere downstream, at which point two distinct
// devices could render as one name while deriving two ids — or one device's
// identity could be spelled two ways that hash differently but read
// identically to an operator deciding whether to adopt it.
func checkField(name, value string, maxBytes int, required bool) error {
	if required && value == "" {
		return fmt.Errorf("%s is required (REL-110a)", name)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s is %d bytes, over the %d cap", name, len(value), maxBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	return nil
}

// candidateName is the row's `name`: the device's own self-reported name when
// it has one, else its identity tuple spelled out — and, for a host the network
// revealed only as a bare MAC, spelled out with the MAC's VENDOR in place of the
// bare driver token when the OUI is one we recognize.
//
// This is the device calling itself something, which IS a discovered fact — as
// distinct from the display name an operator gives it, which is authored policy
// on the adopted record (REL-063) and never comes from here. The vendor is the
// same kind of fact: it is read out of the MAC the relay already reported, not
// authored, and it only ever REPLACES the driver token in the fallback — a
// device that named itself (`c.Name != ""`) keeps its own name untouched, so
// this can never override a better name a lane already learned (macvendor, and
// the name-quality merge in relay/hostmdns that chose `c.Name` in the first
// place).
// candidateMAC is the device's hardware address when its `native_id` IS one.
//
// `native_id` is driver-specific: the neighbour lane keys a host by its MAC, and
// a protocol lane keys by that protocol's id (an ECP serial, a UUID). So this
// asks whether the value spells an address rather than assuming the driver, and
// answers "" when it does not — which is the honest answer for a device no lane
// ever saw at layer 2.
//
// Canonical, not raw: the value is shown to an operator and searched by them, and
// two rows spelling one address differently read as two devices.
func candidateMAC(c wire.DeviceCandidate) string {
	return macvendor.Canonical(c.NativeID)
}

// candidateVendor is the OUI's registered organization, for the same value.
//
// It is the fact candidateName has been reading all along and spending on a name
// fallback. Computed for EVERY device here, not only the unnamed ones — a device
// that names itself has a vendor too, and it was the only thing standing between
// an operator and knowing that the box calling itself "NAS" is a Synology.
func candidateVendor(c wire.DeviceCandidate) string {
	vendor, _ := macvendor.Vendor(c.NativeID)
	return vendor
}

func candidateName(c wire.DeviceCandidate) string {
	if c.Name != "" {
		return c.Name
	}
	if vendor, ok := macvendor.Vendor(c.NativeID); ok {
		return vendor + " " + c.NativeID
	}
	return c.Driver + " " + c.NativeID
}
