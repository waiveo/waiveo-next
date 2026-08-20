// device.go carries relay/1's device-plane command frames — the app-peer→relay
// `device.command` and its correlated `device.command_result` reply (REL-112).
// Like the rest of this package these are contract-derived data shapes only; the
// carriage lives in internal/feeder/relayconn (the app peer's outbound dispatch)
// and internal/relay/relayconn (the relay's inbound handler), and the resolution
// and execution the relay performs on receipt live in internal/relay/deviceplane.
//
// `device.command` is the ONLY app-initiated request/response exchange this
// contract defines: every other app-peer→relay frame is either a handshake step
// or REL-057's fire-and-forget nudge. It is therefore the frame that makes the
// app peer's own side of the connection a request originator, mirroring the
// relay's `state.pull` correlation in the opposite direction (REL-006: one
// correlation `id` shared by the pair, plus the originating operation's
// `trace_id`).
package wire

import "encoding/json"

// Frame type discriminators for the device plane's command surface (REL-112)
// and its upward candidate report (REL-110).
const (
	FrameTypeDeviceCommand       = "device.command"
	FrameTypeDeviceCommandResult = "device.command_result"
	FrameTypeDeviceCandidates    = "device.candidates"
)

// DeviceCandidatesBody is `device.candidates`'s body (REL-110/111): the relay's
// FULL current candidate set, which the app peer takes as replacing its prior
// view of that relay rather than as a delta to fold in.
type DeviceCandidatesBody struct {
	Candidates []DeviceCandidate `json:"candidates"`
}

// DeviceCandidate is one entry of that report: REL-110's original six members
// plus the device identity and entity fan-out REL-110a adds.
//
// This is the APP PEER's decode shape, deliberately kept separate from the
// relay's own producing type (internal/relay/deviceplane.Candidate) even though
// the two describe one wire object. A relay is untrusted input; sharing one
// struct would mean a change on the producing side silently changed what the
// consuming side parses, and would let the producer's field set define the
// consumer's. The two are held in agreement by driving a real report from a
// real relay through a real connection, not by sharing a definition.
//
// What this shape deliberately has NO field for is a platform row identifier.
// `device_id` and `entity_id` are derived by the app peer from REL-153's
// identity tuple (REL-110b, internal/shared/deviceid), so a relay that puts an
// `id` on the wire is not refused — it is structurally unable to be heard,
// because nothing decodes it.
//
// Match is carried as raw JSON and is not consumed by the read model: it
// records WHICH declared discovery pattern caused the observation (manifest/1
// MAN-071), which is the relay's own provenance for the sighting, not a member
// of the device the app peer lists.
//
// NameRank is REL-110c: not a fact ABOUT the device but a fact about `name` —
// which kind of record on the LAN authored it. It exists because the relay's
// ranked merge lives in a process whose memory is re-minted at every restart,
// so without it the app peer's durable mirror can only ever ask whether a
// reported name is non-empty, and a machine-generated label wins by arriving
// first after a restart and then sticks. See the token block below.
//
// ClassRank is REL-110d, the same shape one field over and for a sharper reason.
// A `device_class` is not merely rendered — it governs the command vocabulary
// (`device-class-registry/1` REG-052) — and its quality is only HALF derivable
// from the value: how CONCRETE a class is follows from the token, but how
// AUTHORITATIVE the record behind it was does not. `_ecobee._tcp` and
// `_matter._tcp` both yield `smart-home`, one because the device is a
// thermostat and one because it joined a fabric, and that difference lives in
// the mDNS service type, which this wire does not carry and should not start
// carrying. So the relay states its own verdict instead, and the consumer
// re-derives the half it can. See the token block below for why the two ranks
// are separate members with separate vocabularies.
//
// Address, Model and Serial are REL-004 additive members beyond REL-110a's own
// set, and they are what makes a reported candidate actionable rather than
// merely countable. `native_id` identifies a device; it does not say where the
// device is, so an operator shown a list of USNs has no way to tell which
// physical TV is which, and an adopted device with no address has nowhere for a
// command to go. All three are omitempty and all three may be absent: a relay
// whose sighting carried no usable LOCATION, or whose identification probe did
// not answer, reports what it does know rather than dropping the device.
type DeviceCandidate struct {
	Match        json.RawMessage   `json:"match"`
	Provenance   string            `json:"provenance"`
	Status       string            `json:"status"`
	IgnoredUntil *string           `json:"ignored_until"`
	FirstSeen    int64             `json:"first_seen"`
	LastSeen     int64             `json:"last_seen"`
	Driver       string            `json:"driver"`
	NativeID     string            `json:"native_id"`
	DeviceClass  string            `json:"device_class"`
	ClassRank    string            `json:"class_rank,omitempty"`
	Name         string            `json:"name,omitempty"`
	NameRank     string            `json:"name_rank,omitempty"`
	Address      string            `json:"address,omitempty"`
	Model        string            `json:"model,omitempty"`
	Serial       string            `json:"serial,omitempty"`
	OpenPorts    []int             `json:"open_ports,omitempty"`
	Entities     []CandidateEntity `json:"entities"`
}

// Candidate name-rank values (REL-110c): which KIND of record authored `name`,
// so a consumer that outlives the relay's own memory can refuse a worse-sourced
// name instead of taking whichever report arrived last.
//
// # Why a token and not a number
//
// The relay ranks names on an ORDERED ladder
// (internal/relay/deviceplane.NameRank), and putting that ladder's ordinals on
// the wire would put its ORDERING in the contract. Inserting a rank later would
// then renumber the existing values, and REL-004 forbids exactly that — "an
// existing message field's meaning MUST NOT change". A token names the SOURCE
// and says nothing about where it sits; each peer keeps its own ordering and a
// new token is an additive change, the same way `status` and `provenance` are
// already spelled here.
//
// # An unrecognised token is the LOWEST rank, never a refusal
//
// A consumer meeting a token a newer relay minted MUST read it as the bottom of
// its own ladder: a rank is a licence to REFUSE, and a value this side cannot
// interpret must not be honoured as one. That degrades to "this name can fill a
// gap but cannot displace a better-sourced one", which is what
// deviceplane.NameRank's own zero value means — the safe direction.
//
// # Absent is NOT `none`
//
// The member is optional and omitempty: a relay that predates REL-110c sends
// nothing, and that is "this peer does not rank names", NOT "this name is
// unranked". A consumer that collapsed the two would assert a statement no relay
// made — and would leave itself no way to tell an un-upgraded reporter from a
// genuinely unranked name.
const (
	// CandidateNameRankNone is a name the relay has no opinion about: a lane that
	// ranks nothing, or a value the relay REMEMBERED rather than just observed.
	CandidateNameRankNone = "none"
	// CandidateNameRankMachine is a label the device derived mechanically rather
	// than one anybody chose — an id, a hostname form, or a lossy rewrite of a
	// chosen name (a Cast instance truncated to 20 characters with a hex tail).
	CandidateNameRankMachine = "machine"
	// CandidateNameRankModel is a factory/model string: it identifies the
	// product, not the unit.
	CandidateNameRankModel = "model"
	// CandidateNameRankFriendly is a record whose label IS the name somebody gave
	// the device — an AirPlay or Android-TV-remote instance, a HomeKit accessory,
	// a device's own configured display name.
	CandidateNameRankFriendly = "friendly"
)

// Candidate class-rank values (REL-110d): how strongly the record behind
// `device_class` asserts what the device IS.
//
// Everything the name-rank block above argues carries over verbatim — a token
// rather than an ordinal because REL-004 forbids renumbering an existing
// member's meaning, and an unrecognised token read as the LOWEST rank the
// consumer knows because a rank is a licence to REFUSE. What follows is only
// what is DIFFERENT about the class, and each difference is a decision.
//
// # Only HALF the class's quality is on this wire, on purpose
//
// The relay ranks a class on two keys: the AUTHORITY of the record that implied
// it, and the CONCRETENESS of the class token itself. Concreteness is a pure
// function of the token — a consumer holding `media-player` and `smart-home` can
// order them without being told — so putting it here would be a member the
// consumer could compute, and a second place to keep in agreement. Authority
// cannot be computed from the token: `_ecobee._tcp` and `_matter._tcp` both
// yield `smart-home`. So this member carries authority ONLY, and a consumer is
// expected to compare (rank, its own concreteness) in that order. Comparing
// concreteness first inverts a real device: an ecobee thermostat advertising
// `_airplay` and `_spotify-connect` beside its own `_ecobee` service becomes a
// media player.
//
// # This member is SEPARATE from `name_rank`, deliberately
//
// The two are not one "rank" member with a wider vocabulary, and not an
// amendment to REL-110c. They have different vocabularies that must not be
// interchangeable, different presence obligations (`name` is optional so
// `name_rank` is conditional on it; `device_class` is REQUIRED by REL-110a so
// `class_rank` is unconditional), and — the load-bearing one — DIFFERENT
// ANSWERS FOR ABSENCE. A consumer that implemented one requirement and applied
// it to both fields would get one of them dangerously wrong.
//
// # Absent means "a peer older than REL-110d", and can mean nothing else
//
// `name_rank`'s absence has two conceivable producers, because a candidate may
// legitimately carry no `name` at all. A candidate ALWAYS carries a
// `device_class` (REL-110a), so a peer that ranks classes always has one to
// rank: absence is a statement about the PEER, never about the device. The
// `omitempty` below is therefore never exercised by a relay that speaks this
// version — see cmd/waiveo-relay's classRankToken, which has no branch returning
// the empty string. It earns its place all the same, on the DECODE side: this
// struct is also the shape the feeder rebuilds a candidate into when it restores
// its mirror at boot, and a row whose rank was never recorded must be rebuilt as
// ABSENT rather than as a claim it does not have.
const (
	// CandidateClassRankNone is a sighting with no opinion about what the device
	// is — a lane that only knows a host exists. It is the rank of the generic
	// `unclassified` class and, on a well-formed report, of nothing else.
	CandidateClassRankNone = "none"
	// CandidateClassRankFeature is a service any KIND of device may implement,
	// including membership of a home-automation fabric: a Matter or HomeKit
	// record, an AirPlay or Spotify-Connect endpoint, a file share. It classifies
	// a device nothing else recognises, and it must never outrank what the device
	// says about itself — thermostats advertise media features.
	CandidateClassRankFeature = "feature"
	// CandidateClassRankProduct is the device's OWN product service, advertised
	// because of what the device is rather than as a feature bolted onto
	// something else, or a pattern a pack DECLARED for a kind of device by name.
	CandidateClassRankProduct = "product"
)

// CandidateEntity is one addressable object a candidate device exposes
// (REL-110a): the device-native Key the relay addresses it by, the DeviceClass
// whose vocabulary governs commands to it (device-class-registry/1 REG-052),
// and its last observed State when the relay has one.
//
// Attributes carries the DRIVER-OBSERVED detail behind that State — for a Roku,
// device-class-registry/1 REG-064's `power_mode`, `active_app`, `active_app_id`,
// `app_type`, `is_screensaver`, `app_version`. State alone answers "on/idle/
// standby/off"; an operator looking at a screen that is not showing what it
// should needs to know it is sitting in the Netflix app, which State cannot say.
//
// Values are STRINGS on this wire even where the driver's own value is a bool
// (`is_screensaver`), and that is deliberate rather than lossy-by-accident:
// this is display detail crossing a trust boundary from a relay, so a
// fixed-shape map with bounded keys and values is checkable at intake in a way
// an arbitrary JSON value is not, and every consumer of it renders text.
//
// Absent means the driver reported none — never that the device has none.
//
// Key is a relay-side addressing handle, not a platform id: the app peer
// derives the entity_id from it (REL-110b) and never carries the key itself
// into a resource representation.
type CandidateEntity struct {
	Key         string            `json:"key"`
	DeviceClass string            `json:"device_class"`
	State       string            `json:"state,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
}

// Candidate status values (REL-110). A candidate is `pending` until something
// acts on it; `adopted` once it has been promoted; `ignored` while suppressed.
const (
	CandidateStatusPending = "pending"
	CandidateStatusAdopted = "adopted"
	CandidateStatusIgnored = "ignored"
)

// Candidate provenance values (REL-110): observed by the relay's own discovery,
// or asserted by an operator.
const (
	CandidateProvenanceDiscovered = "discovered"
	CandidateProvenanceManual     = "manual"
)

// CandidateIgnoredForever is REL-110's literal `ignored_until` value meaning a
// candidate is suppressed with no expiry, as opposed to a Timestamp-ms expiry.
const CandidateIgnoredForever = "forever"

// DeviceCommandBody is `device.command`'s body (REL-112):
// `{entity_id, command, params}`. `entity_id` is ALREADY resolved to one
// specific adopted entity — relay/1 accepts a single entity id and no selector
// or device-class filter (rules/1 Entity targeting resolves those before a
// command reaches this contract). `params` MAY carry credential material scoped
// to this one dispatch (REL-114): it is never persisted and never logged, so
// nothing on either side of the wire may copy it into a durable store or a log
// line.
type DeviceCommandBody struct {
	EntityID string         `json:"entity_id"`
	Command  string         `json:"command"`
	Params   map[string]any `json:"params,omitempty"`
}

// DeviceCommandResultBody is `device.command_result`'s body (REL-112):
// `{ok, error}` — `error` present if and only if `ok` is false, carrying an
// Error-taxonomy code (COMMAND_UNRESOLVED for a command the target entity's
// device class does not declare, REL-113; COMMAND_TARGET_UNREACHABLE for a
// device the relay could not reach). Per REL-007 a typed refusal already
// carried as this ack's own `error` field is NOT additionally sent as a
// top-level error frame.
type DeviceCommandResultBody struct {
	OK    bool              `json:"ok"`
	Error *CommandErrorBody `json:"error,omitempty"`
}

// CommandErrorBody is the `{code, message}` object a rejected
// `device.command_result` carries (REL-112/113), the same shape `state.ack`'s
// own error uses (AckErrorBody) — kept a distinct type so the two verbs' bodies
// evolve independently, exactly as the contract defines them separately.
type CommandErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NewDeviceCommandError builds a rejected result body carrying the given
// Error-taxonomy code and message — the one constructor both peers build a
// refusal through, so an `ok:false` result can never be emitted with an absent
// `error` object.
func NewDeviceCommandError(code, message string) DeviceCommandResultBody {
	return DeviceCommandResultBody{
		OK:    false,
		Error: &CommandErrorBody{Code: code, Message: message},
	}
}
