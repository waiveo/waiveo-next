// Package hello implements the relay/1 connection handshake's wire
// vocabulary (contracts/relay-1.md REL-030–039): the app peer's single-use
// `challenge` nonce, the relay's `hello` — channel-bound to that nonce via
// its enrollment key, carrying its protocol_version/features/site_binding/
// subnet_metadata/clock_state — and the app peer's `hello-ack`
// (negotiated_version, the shared feature subset, and its own authoritative
// site_binding).
//
// Shared by both roles — the relay (client) and the feeder (server) — the
// same way internal/relay/enroll and internal/feeder/enroll share the
// enrollment wire, so a later task (Task 4) wires both sides against these
// same types instead of each role defining its own copy.
//
// json struct tags match contracts/relay-1.md's Wire-shapes section (and
// the REL-030-valid-hello-negotiate-channel-binding.json corpus fixture)
// field-for-field; when the contract's field set changes, update the types
// here in the same change that bumps the contract.
package hello

// Challenge is the app peer's first message on a fresh authenticated
// relay/1 connection (REL-030): a fresh, single-use nonce of at least 128
// bits, unique to that connection attempt. NewChallenge mints one.
type Challenge struct {
	Type string        `json:"type"`
	Body ChallengeBody `json:"body"`
}

// ChallengeBody carries the challenge nonce (REL-030), hex-encoded.
type ChallengeBody struct {
	Nonce string `json:"nonce"`
}

// Hello is the relay's first message on the connection, sent immediately
// after Challenge (REL-031).
type Hello struct {
	Type    string    `json:"type"`
	RelayID string    `json:"relay_id"`
	Body    HelloBody `json:"body"`
}

// HelloBody is Hello's body (REL-031): the relay's declared protocol
// version and supported features, its site binding, its own canonical
// advertised address, its clock trust state, and the channel-binding
// signature (REL-032) proving possession of its enrollment private key
// over the challenge nonce.
type HelloBody struct {
	ProtocolVersion         string         `json:"protocol_version"`
	Features                []string       `json:"features"`
	SiteBinding             SiteBinding    `json:"site_binding"`
	SubnetMetadata          SubnetMetadata `json:"subnet_metadata"`
	ClockState              ClockState     `json:"clock_state"`
	ChannelBindingSignature string         `json:"channel_binding_signature"`
}

// SiteBinding carries the site a relay is bound to, and that site's
// effective timezone and coordinates (REL-036) — the same shape Hello
// sends and HelloAck returns as authoritative.
type SiteBinding struct {
	ScopeNode string  `json:"scope_node"`
	TZ        string  `json:"tz"`
	Lat       float64 `json:"lat"`
	Long      float64 `json:"long"`
}

// SubnetMetadata carries the relay's own canonical advertised address —
// the same address it advertises in its own discovery/pairing responses
// (REL-037).
type SubnetMetadata struct {
	AdvertisedAddress string `json:"advertised_address"`
}

// ClockState carries the relay's clock trust state and time source
// (REL-038). `State` is `trusted` or `untrusted`; an unrecognized `Source`
// value MUST be treated as forward-compatible data by callers, never a
// validation failure — this type itself imposes no such check.
type ClockState struct {
	State  string `json:"state"`
	Source string `json:"source"`
}

// HelloAck is the app peer's reply completing the handshake (REL-039).
type HelloAck struct {
	Type    string       `json:"type"`
	RelayID string       `json:"relay_id"`
	Body    HelloAckBody `json:"body"`
}

// HelloAckBody is HelloAck's body (REL-039): the negotiated protocol
// version (REL-033/034), the shared feature subset (REL-035), the app
// peer's own authoritative site_binding (REL-036, which a relay MUST adopt
// for the connection), and any deprecation notices for message types in
// the negotiated version. Deprecated MUST be a non-nil (possibly empty)
// map so it marshals as `{}` rather than `null` when there is nothing to
// report.
type HelloAckBody struct {
	NegotiatedVersion string                       `json:"negotiated_version"`
	Features          []string                     `json:"features"`
	SiteBinding       SiteBinding                  `json:"site_binding"`
	Deprecated        map[string]DeprecationNotice `json:"deprecated"`
}

// DeprecationNotice describes a message type that is deprecated, but still
// valid, in the negotiated protocol version (REL-039).
type DeprecationNotice struct {
	DeprecatedIn string `json:"deprecated_in"`
	RemovedIn    string `json:"removed_in"`
	Message      string `json:"message"`
}
