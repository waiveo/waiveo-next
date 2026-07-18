package wire

import (
	"encoding/base64"
	"fmt"
)

// EncodeSignature and DecodeSignature are THE canonical on-wire encoding
// for a relay/1 `signature` field (REL-075): raw ed25519 signature bytes,
// base64-standard-encoded. relay/1 mandates no specific signature-field
// grammar beyond "a signature"; base64-std is this package's own choice —
// but it must be made exactly once here, because both the feeder (which
// signs a snapshot and encodes the result with EncodeSignature) and the
// relay (which decodes a received signature with DecodeSignature before
// calling signhash.Verify) must agree on it byte-for-byte, or every
// signature silently fails to verify. Do not reimplement this encoding
// elsewhere — import and call these instead.
func EncodeSignature(sig []byte) string {
	return base64.StdEncoding.EncodeToString(sig)
}

// DecodeSignature reverses EncodeSignature, yielding the raw signature
// bytes signhash.Verify expects. It returns an error (never panics) on a
// malformed input string.
func DecodeSignature(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("wire: decode signature: %w", err)
	}
	return b, nil
}
