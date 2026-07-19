package hello

import "crypto/ed25519"

// BuildHelloAck computes the app peer's hello-ack in response to a relay's
// hello, in the order REL-030–039 requires: it first verifies
// channel_binding_signature against the relay's enrollment-learned public
// key over the challenge nonce (REL-032) — refusing before looking at
// anything else in the message, since an unverified hello must not
// influence negotiation at all — then negotiates protocol_version
// (REL-033/034), computes the shared feature subset (REL-035), and reports
// the app peer's own authoritative site_binding/deprecated set (REL-036,
// REL-039).
//
// relayPub MUST be the public key the app peer learned for THIS relay at
// enrollment (REL-032) — never one derived from hello itself. On channel-
// binding failure or a protocol major mismatch, BuildHelloAck returns a
// zero HelloAck and a non-nil error (ErrChannelBindingInvalid or
// ErrProtocolVersionUnsupported): the caller MUST refuse the connection
// with the corresponding typed error in place of sending hello-ack, exactly
// as REL-032/033 require — regardless of whether the connection's own
// mutual-TLS handshake already succeeded.
func BuildHelloAck(
	h Hello,
	nonceHex string,
	relayPub ed25519.PublicKey,
	appPeerImplementedMinors []string,
	appPeerRecognizedFeatures []string,
	siteBinding SiteBinding,
	deprecated map[string]DeprecationNotice,
) (HelloAck, error) {
	if err := VerifyChannelBinding(relayPub, nonceHex, h.Body.ChannelBindingSignature); err != nil {
		return HelloAck{}, err
	}

	negotiated, err := Negotiate(h.Body.ProtocolVersion, appPeerImplementedMinors)
	if err != nil {
		return HelloAck{}, err
	}

	if deprecated == nil {
		deprecated = map[string]DeprecationNotice{}
	}

	return HelloAck{
		Type:    "hello-ack",
		RelayID: h.RelayID,
		Body: HelloAckBody{
			NegotiatedVersion: negotiated,
			Features:          IntersectFeatures(h.Body.Features, appPeerRecognizedFeatures),
			SiteBinding:       siteBinding,
			Deprecated:        deprecated,
		},
	}, nil
}
