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

// keepClass merges a re-sighting's device_class without letting the generic
// default erase a learned one. Enumerate-all means several lanes observe one
// device: the neighbour lane knows only that a host exists (unclassified), while
// the mDNS lane may know it is a media player. Overwriting unconditionally
// (which every re-sighting used to do) let whichever lane swept last win, so a
// classified device flickered back to unclassified on the next neighbour sweep.
// A SPECIFIC class wins; between two specific classes the newer sighting wins
// (an actual reclassification); ClassUnclassified only fills a gap.
func keepClass(next, current string) string {
	if next != "" && next != ClassUnclassified {
		return next
	}
	if current != "" && current != ClassUnclassified {
		return current
	}
	return next
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
	Name        string
	Address     string
	Model       string
	Serial      string
	Entities    []CandidateEntity
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
	Entities     []CandidateEntity `json:"entities"`
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
	// non-UTF-8 USN would make every subsequent report unapplyable and blank the
	// whole site's device list — permanently, since nothing here expires a
	// candidate. A per-device problem must not become a site-wide outage, and the
	// place to stop it is before the poison is ever stored.
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
		c.Match = o.Match
		// keepClass, not a bare overwrite: a specific class one lane learned must
		// not be downgraded to the generic default by another lane's next sweep.
		c.DeviceClass = keepClass(o.DeviceClass, c.DeviceClass)
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
		c.Name = orKeep(o.Name, c.Name)
		c.Address = orKeep(o.Address, c.Address)
		c.Model = orKeep(o.Model, c.Model)
		c.Serial = orKeep(o.Serial, c.Serial)
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
		Entities:    append([]CandidateEntity(nil), o.Entities...),
	}
	s.order = append(s.order, key)
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
