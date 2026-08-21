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
//
// Ignored is the app's OTHER decision, and the third state beside adopted and
// plain-discovered (discovery spec §7). It reports whether the operator has
// ignored this device — set it aside as something they do not care about — which
// is durable and reversible and, unlike Adopted, reaches no relay: an ignored
// device is still discovered and still reported, just marked so a console can
// keep it out of the way. A device is never both adopted and ignored in
// practice, but the flags are independent: adopting supersedes ignoring in the
// console's reading, not in this struct.
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
	// MAC is the device's hardware address when discovery learned one, in this
	// platform's single spelling (lowercase, colon-separated).
	//
	// It is a DESCRIPTIVE fact, in the same class as Address and Model, and not
	// this row's identity: a device is keyed by REL-153's (site, driver,
	// native_id), that tuple stays off this resource, and adoption remains a
	// bodyless POST by id. What it adds is the one identifier that survives what
	// Address does not — a lease expiring, a device moving subnet — which is why
	// two hosts both calling themselves "NAS" are two rows an operator can only
	// tell apart by IP today.
	//
	// Absent when the reporting lane learned no hardware address: a device named
	// by a protocol lane carries that protocol's own id, not a MAC.
	MAC string `json:"mac,omitempty"`
	// Vendor is the organization that owns MAC's OUI, when the OUI is one this
	// build recognizes.
	//
	// Already computed and already shown — but only ever smuggled into the
	// fallback NAME ("Proxmox bc:24:11:…", candidateName), and therefore only for
	// a device that could not name itself. On the measured deployment that left
	// 12 of 63 devices — every self-named one, including both machines called
	// "NAS" — with a vendor the platform knew and no surface that said it.
	// Published as its own fact so it can be read, sorted and filtered rather
	// than parsed back out of a display string.
	Vendor  string `json:"vendor,omitempty"`
	Adopted bool   `json:"adopted"`
	Ignored bool   `json:"ignored"`
	// OpenPorts is what an active scan found listening on this device. Absent
	// until something scanned it: no port list and an empty port list are
	// different facts, and only a scan can assert the second.
	// `omitzero`, not `omitempty`: the doc above is the whole point of the
	// member, and omitempty would serve an empty list — a real scan result —
	// as an absent one, contradicting it on the wire api/1 publishes.
	OpenPorts []int `json:"open_ports,omitzero"`
	// FirstSeen is when this SITE first held a report of the device and LastSeen
	// is when its relay was last observed to have SEEN it, both in epoch
	// milliseconds on the app's own clock (SEC-066) — the pair that answers "is
	// this new to my network, or has it been here for weeks", which is the
	// question a discovery inventory exists for.
	//
	// They are stamped from the durable ledger and the mirror row
	// (internal/app/store), never from the reporting relay's numbers. A relay
	// does not persist candidates, so its idea of "first" is re-minted at every
	// relay restart and would report a four-year-old TV as new every time its
	// relay was upgraded; and its clock is unattested (REL-038's clock_state is
	// `untrusted` on every live relay), so its idea of "how long ago" is a
	// number nothing on this platform stands behind.
	//
	// Both are omitempty because zero is not a time: a device this deployment
	// has never mirrored (the store write is failing, or the row predates the
	// ledger) carries no answer, and an absent member says that where an epoch
	// timestamp would lie about it.
	FirstSeen int64 `json:"first_seen,omitempty"`
	LastSeen  int64 `json:"last_seen,omitempty"`
	// FirstSeenOrigin says whether FirstSeen is an instant this deployment
	// OBSERVED or an estimate it inherited: `planted`, `adopted` or `unrecorded`
	// (internal/app/store devicefirstseen.go has the vocabulary). Absent exactly
	// when FirstSeen is.
	//
	// It exists because a renderer cannot otherwise be honest. A deployment
	// upgraded from a build predating the durable ledger adopted its ages from a
	// column written off the reporting relay's unattested clock; on box .12 that
	// is 64 of 64 rows, 57 of them sharing one instant to the millisecond. Served
	// without this member they are indistinguishable from witnessed instants, and
	// the console drew all of them as exact ages ("3d ago") in a column headed
	// "First seen" and RANKED them against each other — a precision and a
	// comparability the numbers do not have. The caveat was documented in this
	// schema's prose and in the boot log, neither of which is where the operator
	// reading the wrong number is looking.
	FirstSeenOrigin string `json:"first_seen_origin,omitempty"`
}

// WithoutAge returns the device with BOTH age members cleared, together.
//
// They are one fact in two fields: FirstSeenOrigin qualifies FirstSeen and is
// documented above as absent exactly when FirstSeen is. Clearing the instant and
// leaving the qualifier serves an origin for a value this deployment no longer
// holds — `"first_seen_origin": "adopted"` with no age beside it — and a client
// that reads the qualifier without first checking the value it qualifies gets a
// confident answer about nothing.
//
// It is a method rather than two statements at the call site because the two
// CAN be cleared apart in Go and are wrong apart in every case. The retire
// handler's fallback path (internal/app/api devices.go) did exactly that, which
// is what this exists to make unrepeatable: there is now one operation, and its
// name is the invariant.
func (d Device) WithoutAge() Device {
	d.FirstSeen = 0
	d.FirstSeenOrigin = ""
	return d
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

	// key is the relay's entity key ("main"), kept so rematerialize can
	// RE-compose Name from whatever the device row ends up named. Name is
	// "<device name> <key>", and the device name can be overlaid from the
	// durable mirror after intake composed it, so the key has to survive the
	// composition. Unexported: it is derivable from the report and this struct's
	// exported members are api/1's, not a place to add a field.
	key string
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

	// ignored is the set of device ids the operator has ignored. It is held here
	// for the identical reason adopted is: a relay's report replaces its whole
	// view, so an ignore stamped onto a view row would be erased by the very next
	// report — which for ignore is the common case, since an ignored device keeps
	// being reported. Holding it beside the views is what makes "ignored" survive
	// a re-sighting (internal/app/store ignoreddevices.go says the same about the
	// durable half). A device off the network is ignorable too, for the same
	// reason it is adoptable when absent, so an id this registry has not heard of
	// is remembered until it reports.
	ignored map[string]bool

	// seen is when this SITE first and last held a report of each device — the
	// app's own record of a device's age, held here for the third time in this
	// struct and for the third identical reason: a relay's report replaces its
	// whole view, so anything stamped onto a view row dies with the next report.
	//
	// The values are NOT the ones the report carries. A relay does not persist
	// candidates, so its `first_seen` is only ever "since this relay process
	// started" and is re-minted from nothing at every relay restart — which is
	// exactly the defect that made the durable ledger (internal/app/store
	// devicefirstseen.go) the owner of this fact. This map is that ledger's
	// projection: the store merges, commits, and returns what stands, and
	// MarkSeen teaches it here.
	seen map[string]Seen

	// stored is what the DURABLE mirror committed for each device's learned
	// facts, held beside the views for the fourth time in this struct and, again,
	// for the same reason: a relay's report replaces its whole view.
	//
	// It closes a split that opened the moment the mirror's merge learned to
	// REFUSE part of a report. Both layers see the same reports, but only the
	// mirror remembers across a relay restart, so only the mirror can tell "the
	// relay has not learned this yet" from "it is no longer true" — a restarted
	// relay reports `unclassified` for a device its mDNS sweep has not reached,
	// and a bare host for one whose port only an SSDP LOCATION carries. The
	// mirror keeps the better answer (internal/app/store mergeDiscovered); before
	// this map the read model took the degraded one, so `GET /devices` served the
	// exact value the durable guard existed to prevent, and the two layers
	// disagreed until a feeder restart rebuilt the view FROM the mirror and
	// silently swapped the answer over with no new information from any relay.
	//
	// This is MarkSeen's shape, for MarkSeen's reason: the store merges, commits,
	// and returns what stands, and the caller teaches it here.
	stored map[string]Stored

	// Merged view, rebuilt from views on every write. Reads never touch views.
	devices  map[string]Device
	entities map[string]Entity
}

// Stored is the durable mirror's committed answer for one device's LEARNED
// facts — the ones a report can be silent or ignorant about — plus the relay
// they were committed under.
//
// RelayID is not decoration. These values are keyed by device id, which is
// derived from (site, driver, native_id) and is therefore guessable by any
// enrolled relay; REL-153 incumbency decides which relay actually speaks for a
// device, and it can hand a device over. An overlay taken under relay A must not
// be stamped onto relay B's row afterwards, or A's remembered address would
// silently describe a device B now routes.
//
// An empty member teaches nothing rather than blanking what the report carried,
// exactly as a zero instant does in MarkSeen: the mirror is a memory, and a
// memory that has nothing to say is not a retraction.
type Stored struct {
	RelayID     string
	DeviceClass string
	Name        string
	Address     string
	Model       string
	Serial      string
	OpenPorts   []int
}

// Seen is a device's age as this site knows it: when the site first held a
// report of the device, when it last did, and — for the first of those — where
// the number came from. Both instants are on the APP's clock (SEC-066) rather
// than the reporting relay's wall clock, so an operator comparing two devices is
// comparing two readings of one clock.
//
// FirstOrigin is empty exactly when FirstMs is zero, and otherwise carries the
// store's vocabulary (store.FirstSeenPlanted / Adopted / Unrecorded). It rides
// here because it must reach the api/1 Device: a value adopted from the
// pre-ledger column at an upgrade is an estimate off an unattested relay clock,
// and a surface that renders it identically to a planted instant is claiming an
// observation this deployment never made.
type Seen struct {
	FirstMs     int64
	LastMs      int64
	FirstOrigin string
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
		ignored:    map[string]bool{},
		seen:       map[string]Seen{},
		stored:     map[string]Stored{},
		nowMs:      nowMs,
	}
}

// MarkStored teaches the read model what the durable mirror committed for a
// batch of devices' learned facts (see Registry.stored) — the projection that
// keeps `GET /devices` and the mirror from answering differently about one
// device.
//
// A PROJECTION, exactly as MarkSeen is, and it runs at the same seam for the
// same reason: the mirror merges the report onto what it already held, commits,
// and returns the rows that now stand; this carries those rows into what an
// operator reads. Without it the mirror's merge is invisible to the API while
// any relay is connected and visible only after a feeder restart, which is the
// worst of both — the same device answering two different ways depending on
// which process restarted last.
//
// A device the mirror did not return keeps whatever the report gave it, so a
// report covering half a site does not disturb the other half. An empty member
// likewise teaches nothing: the mirror knowing no model is not a claim that the
// device has none.
func (r *Registry) MarkStored(stored map[string]Stored) {
	if len(stored) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range stored {
		if id == "" {
			continue
		}
		r.stored[id] = s
	}
	r.rematerialize()
}

// MarkSeen teaches the read model what the durable first/last-seen ledger holds
// for a batch of devices (see Registry.seen).
//
// A PROJECTION, exactly as MarkAdopted and MarkIgnored are: the store merges and
// commits first, and this only carries the committed answer into the rows an
// operator reads. It takes the whole batch rather than one device at a time
// because a report re-states every device a relay can see — one lock and one
// rematerialize for a report, not sixty of each.
//
// A device the store did not return keeps whatever it already had, so a report
// that mentions half a site does not blank the other half's age. An id this
// registry has not heard of is remembered anyway, for the reason the other two
// projections allow it: the boot restore runs before any relay has connected.
//
// # A zero member TEACHES NOTHING; it does not erase
//
// The two instants are merged independently, and a zero on either side leaves
// whatever is already known for that half alone. That is not politeness, it is
// the rule that keeps this from being a second eraser: zero is the ABSENT answer
// throughout this fact's chain (the store returns it, the api/1 member is
// omitted, the console draws an em dash), and "I have no answer" must never
// overwrite "I have one". Erasing is a separate, deliberate act with its own
// method — ForgetFirstSeen — called by the operation that retires a stored age.
//
// The caller used to enforce this by dropping the whole entry when first_seen
// was zero (cmd/waiveo-feeder seenFrom), which protected the age and silently
// destroyed the LAST-seen half of a retired device's record at every restart.
// Enforcing it per-member here is what lets that caller carry both.
func (r *Registry) MarkSeen(seen map[string]Seen) {
	if len(seen) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range seen {
		if id == "" {
			continue
		}
		was := r.seen[id]
		if s.FirstMs > 0 {
			// The origin belongs to the instant and moves with it, never on its
			// own: a stored age and a claim about where it came from that were
			// taken from two different readings would be worse than either.
			was.FirstMs, was.FirstOrigin = s.FirstMs, s.FirstOrigin
		}
		if s.LastMs > 0 {
			was.LastMs = s.LastMs
		}
		r.seen[id] = was
	}
	r.rematerialize()
}

// ForgetFirstSeen clears the FIRST-seen half of a device's age in the read model,
// so every later listing reports no age for it until a fresh report plants one.
//
// The read-model half of retiring a stored first_seen (internal/app/store
// RetireDeviceFirstSeen): the ledger row and its mirror projection are deleted
// first, and this teaches the running process that the value is gone. Without it
// the retire is invisible until the next restart — the store would be clear while
// this map went on serving the retired instant to every list, which is the exact
// "the database says one thing and the process says another" split MarkSeen exists
// to prevent in the other direction.
//
// LAST-seen is deliberately left exactly as it stands. It is a different fact,
// answered by a different rule, and blanking it would report a device that
// reported a minute ago as never heard from. That asymmetry is why this is not
// `delete(r.seen, id)`, which is the obvious implementation and the wrong one.
//
// Clearing a device this registry has no age for is a harmless no-op, the same
// latitude UnmarkIgnored takes.
func (r *Registry) ForgetFirstSeen(deviceID string) {
	if deviceID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	was, ok := r.seen[deviceID]
	if !ok || was.FirstMs == 0 {
		return
	}
	// The origin goes with the instant it describes. Leaving `adopted` behind on
	// a device that now has no age would make the api/1 member say where a value
	// that no longer exists came from.
	was.FirstMs, was.FirstOrigin = 0, ""
	r.seen[deviceID] = was
	r.rematerialize()
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

// MarkIgnored records that the operator has ignored deviceID, so every later
// listing of that device reports it ignored (see Registry.ignored).
//
// Like MarkAdopted it is a PROJECTION of the durable decision, never the
// decision itself: the ignored-devices row is written first, in the store, and
// this only teaches the in-memory read model what the store already committed.
// Called on the ignore operation and again for every ignored row at boot, which
// is what makes the flag survive both a restart and every relay re-report in
// between. Ignoring a device this registry has never heard of is deliberately
// allowed and remembered, exactly as MarkAdopted allows.
func (r *Registry) MarkIgnored(deviceID string) {
	if deviceID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ignored[deviceID] = true
	r.rematerialize()
}

// UnmarkIgnored clears the ignore projection for deviceID, returning it to plain
// "discovered" in every later listing. It is the read-model half of un-ignoring
// (spec §7's reversibility): the store row is deleted first, and this teaches the
// in-memory model the decision is gone. Clearing an id that was not ignored is a
// harmless no-op.
func (r *Registry) UnmarkIgnored(deviceID string) {
	if deviceID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.ignored, deviceID)
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
//
// The mirror overlay this relay contributed goes with the view. Its caller is
// deleting the relay's durable rows in the same breath (candidateMirror.Forget),
// so keeping the overlay would leave the process holding a revoked relay's
// remembered addresses and names — ready to be stamped back onto its rows if the
// same relay id ever enrolls again. The ages in `seen` are deliberately NOT
// dropped alongside: they are the SITE's record of how long a device has been
// here, not a relay's description of it, and they survive a relay being replaced
// on purpose.
func (r *Registry) Forget(relayID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.views[relayID]; !ok {
		return
	}
	delete(r.views, relayID)
	for id, s := range r.stored {
		if s.RelayID == relayID {
			delete(r.stored, id)
		}
	}
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
			d.Ignored = r.ignored[id]
			// The age too, and for the same reason — with one extra one behind
			// it: the report DOES carry a first_seen, and taking it would be
			// taking a relay's process uptime as the device's history. The
			// durable ledger's answer is stamped here instead, and a device the
			// ledger has nothing for reads zero, which serializes as absent
			// rather than as "first seen at the epoch".
			seen := r.seen[id]
			d.FirstSeen, d.LastSeen = seen.FirstMs, seen.LastMs
			d.FirstSeenOrigin = seen.FirstOrigin
			// And the LEARNED facts, which is the same move once more: the
			// mirror merged this report onto what it already held and committed
			// an answer, and that answer — not the raw report — is what this row
			// must show, or the API and the mirror describe one device two ways.
			// Guarded by relay id because a device can change hands (REL-153).
			if s, ok := r.stored[id]; ok && s.RelayID == d.RelayID {
				d.DeviceClass = orKeepStored(s.DeviceClass, d.DeviceClass)
				d.Name = orKeepStored(s.Name, d.Name)
				d.Address = orKeepStored(s.Address, d.Address)
				d.Model = orKeepStored(s.Model, d.Model)
				d.Serial = orKeepStored(s.Serial, d.Serial)
				// `!= nil`, not `len() > 0` — the same distinction this member
				// carries everywhere else, and the last place that was still
				// collapsing it. The mirror commits THREE answers, not two: no
				// row for this device (nil, nothing known), a list (a scan found
				// these), and an EMPTY list (a scan looked and found nothing
				// open). A length test folds the third into the first, so the
				// mirror's committed "nothing is open" could never reach the API.
				//
				// It showed up exactly where the durable half is load-bearing.
				// After a restart the relay's candidate store is empty by design
				// (in-memory, owner decision), so it re-reports passively with no
				// ports at all and every port an operator sees comes from here.
				// Devices with findings got them back; devices a scan had cleared
				// silently reverted to "nobody has looked" — measured as 24 of 63
				// on the dev box, one restart after the scan that cleared them.
				if s.OpenPorts != nil {
					d.OpenPorts = s.OpenPorts
				}
			}
			devs[id] = d
		}
		for id, e := range v.entities {
			if held, ok := r.incumbency[e.DeviceID]; ok && held.relayID != e.RelayID {
				continue
			}
			// An entity's display label is COMPOSED from its device's name at
			// intake, so overlaying the device's name above and leaving this
			// alone would put "onn. 4K Streaming Box" in the device list and
			// "onn Inc 48:5c:2c:31:6e:6e main" beside it in the entity list, for
			// the same device, in the same response set. Recomposed from the row
			// that was just built rather than from the view's, which is a no-op
			// whenever there is no overlay to apply.
			if d, ok := devs[e.DeviceID]; ok && e.key != "" {
				e.Name = d.Name + " " + e.key
			}
			ents[id] = e
		}
	}
	r.devices, r.entities = devs, ents
}

// orKeepStored takes the mirror's committed value when it has one, else leaves
// the reported value in place. The mirror having nothing to say about a fact is
// not a claim that the device has none — the same sentence orKeepFact writes on
// the durable side and MarkSeen's zero-instant rule writes for the ages.
func orKeepStored(stored, reported string) string {
	if stored != "" {
		return stored
	}
	return reported
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
