package hello

import "crypto/ed25519"

// Declaration is everything a relay declares in its hello (REL-031) except the
// channel_binding_signature — the persistent-connection dialer
// (internal/relay/relayconn.Dial) computes that from the challenge nonce and
// the relay's enrollment key (REL-032/040). SiteBinding is the relay's own
// cached copy, if any; it is only a declaration, since the app peer's
// hello-ack returns the authoritative site_binding the relay then adopts
// (REL-036).
type Declaration struct {
	ProtocolVersion string
	Features        []string
	SiteBinding     SiteBinding
	SubnetMetadata  SubnetMetadata
	ClockState      ClockState
}

// RelayKeyLookup resolves a relay's enrollment-learned public key — the key
// the app peer recorded for this relay when it issued its certificate
// (Enrollment, REL-012). VerifyChannelBinding checks a hello's
// channel_binding_signature against exactly this key (REL-032), never one
// derived from the hello itself. ok is false for a relay the app peer never
// enrolled, which the /relay/v1 server (internal/feeder/relayconn) treats as
// an unverifiable channel binding and refuses (CHANNEL_BINDING_INVALID) —
// only at the hello, never pre-hello, so an unauthenticated prober gets no
// enrollment oracle. The identity looked up is the connection's
// mTLS-authenticated client-certificate identity, never the self-asserted
// hello.relay_id (REL-041).
type RelayKeyLookup func(relayID string) (ed25519.PublicKey, bool)
