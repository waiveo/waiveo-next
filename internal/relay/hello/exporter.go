package hello

import (
	"crypto/tls"
	"encoding/hex"
	"fmt"
)

// exporterLabel is the RFC 5705 exporter label the relay/1 connection's
// challenge nonce derives under (REL-040: the nonce MUST derive from the
// connection's TLS exporter keying material, RFC 5705 / RFC 9266-style).
// SPIKE NOTE: this exact label string is a protocol constant the contract
// does not yet pin — production must record it (and its context/length) in
// contracts/relay-1.md before a second implementation exists, and a
// private-use label formally wants IANA-registry treatment per RFC 5705.
const exporterLabel = "EXPORTER-waiveo-relay1-challenge"

// exporterNonceBytes is the exporter output length: 32 bytes, comfortably
// above REL-030's 128-bit minimum challenge-nonce size.
const exporterNonceBytes = 32

// ExporterChallengeNonce derives this connection's challenge nonce from its
// TLS exporter keying material (REL-040), hex-encoded in the same grammar
// ChallengeBody.Nonce carries. Both peers of one TLS session derive the
// SAME value, which is exactly the point: the app peer sends its derivation
// as `challenge`, and a relay that derives its own copy and compares proves
// the challenge was minted on THIS TLS channel — a nonce relayed from any
// other connection cannot match, and the signature over it (REL-032) binds
// the relay's enrollment key to this channel, not just to a value.
//
// Requires a TLS session with exporter support (TLS 1.3, or 1.2 with
// extended master secret); cs.ExportKeyingMaterial fails otherwise and the
// caller MUST refuse the connection rather than fall back to a random
// nonce.
func ExporterChallengeNonce(cs *tls.ConnectionState) (string, error) {
	if cs == nil {
		return "", fmt.Errorf("hello: ExporterChallengeNonce: no TLS connection state")
	}
	ekm, err := cs.ExportKeyingMaterial(exporterLabel, nil, exporterNonceBytes)
	if err != nil {
		return "", fmt.Errorf("hello: ExporterChallengeNonce: export keying material: %w", err)
	}
	return hex.EncodeToString(ekm), nil
}
