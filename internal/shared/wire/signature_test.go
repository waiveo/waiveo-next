package wire

import "testing"

// TestSignatureCodecRoundTrip asserts DecodeSignature(EncodeSignature(sig))
// reproduces a representative 64-byte ed25519 signature byte-for-byte —
// this codec is THE shared contract both the feeder (signer) and the relay
// (verifier) must use, so it must round-trip exactly.
func TestSignatureCodecRoundTrip(t *testing.T) {
	sig := make([]byte, 64)
	for i := range sig {
		sig[i] = byte(i * 7 % 256)
	}

	encoded := EncodeSignature(sig)
	decoded, err := DecodeSignature(encoded)
	if err != nil {
		t.Fatalf("DecodeSignature: %v", err)
	}
	if len(decoded) != len(sig) {
		t.Fatalf("DecodeSignature round-trip length = %d, want %d", len(decoded), len(sig))
	}
	for i := range sig {
		if decoded[i] != sig[i] {
			t.Fatalf("DecodeSignature round-trip mismatch at byte %d: got %#x, want %#x", i, decoded[i], sig[i])
		}
	}
}

// TestDecodeSignatureMalformed asserts DecodeSignature returns an error
// (never panics) on a malformed input string.
func TestDecodeSignatureMalformed(t *testing.T) {
	if _, err := DecodeSignature("not valid base64!!"); err == nil {
		t.Error("DecodeSignature(malformed) succeeded, want an error")
	}
}
