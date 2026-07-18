// Package paircode implements the shared codec for a player/1 pairing code
// (PLY-024, relay/1 REL-126): a fixed-alphabet, hyphen-grouped string that
// deterministically encodes and decodes exactly three components — a
// relay's dial address ({host, port}), a grant_selector, and a
// fingerprint_commitment.
//
// This codec is deliberately the ONLY place either side of a pairing code
// ever packs or unpacks these fields. The relay (internal/relay/playerserver)
// calls Encode when it forms a pairing code for display (REL-126); a later
// virtual-player task calls Decode to recover the same three components
// before dialing the relay and locally verifying the fingerprint_commitment
// (PLY-052). Living in internal/shared, rather than being duplicated on
// each side, is what keeps relay-encode and player-decode from silently
// drifting apart into two incompatible framings.
//
// Wire format: [1-byte host length][host bytes][2-byte big-endian
// port][1-byte grant_selector length][grant_selector bytes][1-byte
// commitment length][commitment bytes], packed as raw bytes, then
// base32-encoded (RFC 4648 standard alphabet, no padding — alphanumeric,
// per PLY-024) and presented in fixed-size hyphen-separated groups purely
// for human readability. Decode strips the hyphens before decoding, exactly
// as PLY-024 requires ("the hyphens serving as visual grouping a decoder
// strips before decoding, never as payload").
//
// PLY-024's exact character count is explicitly still a draft-note in
// player/1 ("not yet fixed ... sized to carry all three decoded
// components"); this package's length-prefixed framing supports whatever
// concrete host/selector/commitment lengths a deployment uses, rather than
// baking in one fixed total width.
package paircode

import (
	"encoding/base32"
	"fmt"
	"strings"
)

// groupSize is the number of characters per hyphen-separated display group
// (PLY-024: "hyphen-separated groups of equal length"). Not itself
// load-bearing for decoding — Decode strips hyphens outright — only for how
// Encode presents its output.
const groupSize = 5

// maxComponentLen is the largest length any single length-prefixed
// component (host, grant_selector, or commitment) may have: a single byte
// carries the length, so 255 is the hard ceiling.
const maxComponentLen = 255

// encoding is RFC 4648 base32's standard alphabet (A-Z, 2-7 — alphanumeric,
// PLY-024), padding removed since Encode always knows its own total length.
var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Encode packs host, port, grantSelector, and commitment into a single
// deterministic pairing code. Panics if port is out of the 0-65535 range or
// any of host/grantSelector/commitment exceeds maxComponentLen bytes —
// these are all values the relay itself controls (its own dial address, its
// own minted grant_id, its own certificate's commitment), never
// user-supplied input, so a fail-fast panic on a violated internal
// invariant is appropriate here (mirroring internal/shared/tlsboot's
// GenSelfSigned panic-on-environment-failure convention), unlike Decode
// below, which handles untrusted/malformed input and must fail closed with
// an error instead.
func Encode(host string, port int, grantSelector string, commitment []byte) string {
	if port < 0 || port > 65535 {
		panic(fmt.Sprintf("paircode: Encode: port %d out of range 0-65535", port))
	}
	if len(host) > maxComponentLen {
		panic(fmt.Sprintf("paircode: Encode: host length %d exceeds %d", len(host), maxComponentLen))
	}
	if len(grantSelector) > maxComponentLen {
		panic(fmt.Sprintf("paircode: Encode: grant_selector length %d exceeds %d", len(grantSelector), maxComponentLen))
	}
	if len(commitment) > maxComponentLen {
		panic(fmt.Sprintf("paircode: Encode: commitment length %d exceeds %d", len(commitment), maxComponentLen))
	}

	buf := make([]byte, 0, 1+len(host)+2+1+len(grantSelector)+1+len(commitment))
	buf = append(buf, byte(len(host)))
	buf = append(buf, host...)
	buf = append(buf, byte(port>>8), byte(port))
	buf = append(buf, byte(len(grantSelector)))
	buf = append(buf, grantSelector...)
	buf = append(buf, byte(len(commitment)))
	buf = append(buf, commitment...)

	return group(encoding.EncodeToString(buf), groupSize)
}

// Decode reverses Encode, recovering the exact {host, port, grantSelector,
// commitment} an earlier Encode call was given. It strips hyphens (PLY-024:
// "visual grouping a decoder strips before decoding, never as payload")
// and upper-cases its input before decoding, so a lower-cased or re-keyed
// rendering of the same code still decodes.
//
// Decode fails closed: malformed base32, a truncated payload that runs out
// of bytes mid-field, or trailing bytes left over after all four fields are
// read all return a non-nil error rather than a zero-value guess or a
// panic. This matters because, unlike Encode's inputs (always relay-owned),
// Decode's input is a human-typed or scanned code — untrusted by
// construction.
func Decode(code string) (host string, port int, grantSelector string, commitment []byte, err error) {
	stripped := strings.ToUpper(strings.ReplaceAll(code, "-", ""))
	if stripped == "" {
		return "", 0, "", nil, fmt.Errorf("paircode: Decode: empty code")
	}

	buf, decErr := encoding.DecodeString(stripped)
	if decErr != nil {
		return "", 0, "", nil, fmt.Errorf("paircode: Decode: base32 decode: %w", decErr)
	}

	r := reader{buf: buf}

	host, err = r.readLenPrefixed()
	if err != nil {
		return "", 0, "", nil, fmt.Errorf("paircode: Decode: host: %w", err)
	}

	portBytes, err := r.readN(2)
	if err != nil {
		return "", 0, "", nil, fmt.Errorf("paircode: Decode: port: %w", err)
	}
	port = int(portBytes[0])<<8 | int(portBytes[1])

	grantSelector, err = r.readLenPrefixed()
	if err != nil {
		return "", 0, "", nil, fmt.Errorf("paircode: Decode: grant_selector: %w", err)
	}

	commitmentStr, err := r.readLenPrefixed()
	if err != nil {
		return "", 0, "", nil, fmt.Errorf("paircode: Decode: commitment: %w", err)
	}
	commitment = []byte(commitmentStr)

	if !r.atEnd() {
		return "", 0, "", nil, fmt.Errorf("paircode: Decode: %d trailing byte(s) after decoding all fields", r.remaining())
	}

	return host, port, grantSelector, commitment, nil
}

// reader is a minimal bounds-checked cursor over a decoded byte payload,
// used only to keep Decode's field-by-field parsing free of manual index
// arithmetic (and the off-by-one risk that comes with it).
type reader struct {
	buf []byte
	pos int
}

func (r *reader) readN(n int) ([]byte, error) {
	if r.pos+n > len(r.buf) {
		return nil, fmt.Errorf("need %d byte(s), only %d remaining", n, len(r.buf)-r.pos)
	}
	b := r.buf[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

func (r *reader) readLenPrefixed() (string, error) {
	lenByte, err := r.readN(1)
	if err != nil {
		return "", fmt.Errorf("length prefix: %w", err)
	}
	n := int(lenByte[0])
	b, err := r.readN(n)
	if err != nil {
		return "", fmt.Errorf("value (declared length %d): %w", n, err)
	}
	return string(b), nil
}

func (r *reader) atEnd() bool {
	return r.pos == len(r.buf)
}

func (r *reader) remaining() int {
	return len(r.buf) - r.pos
}

// group splits s into hyphen-separated chunks of at most n characters each,
// purely for display (PLY-024) — decoding never depends on this grouping.
func group(s string, n int) string {
	if len(s) <= n {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(s[i:end])
	}
	return b.String()
}
