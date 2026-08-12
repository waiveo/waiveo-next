// Package ctxproto implements the ctx/1 wire framing (contracts/ctx-1.md
// CTX-001–004) — the bottom of the host↔pack runtime every extension will speak.
//
// This layer moves BYTES, not meaning. A frame is a 4-byte unsigned big-endian
// length followed by exactly that many payload bytes, and the payload is opaque
// here: CTX-003's "the payload is a msgpack map with type/id/trace_id" is the
// next layer's rule, and keeping the two apart means the framing can be tested
// exhaustively against hostile lengths without a serializer in the loop.
//
// The length prefix counts the payload ONLY, never itself (CTX-001), so a reader
// can allocate and frame without parsing — which is also why the size ceiling is
// enforced BEFORE the allocation rather than after the read.
package ctxproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxFrameBytes is CTX-002's ceiling on an encoded payload: 16 MiB. A frame at
// exactly the ceiling is legal; the refusal is for one byte more.
const MaxFrameBytes = 16 << 20

// prefixBytes is the fixed width of the length prefix (CTX-001).
const prefixBytes = 4

// Error codes this layer raises, spelled exactly as the ctx/1 error taxonomy
// publishes them so a peer can branch on the string it receives.
const (
	CodeFrameTooLarge = "FRAME_TOO_LARGE"
	CodeMalformed     = "MALFORMED_FRAME"
)

// FrameError is a typed framing refusal carrying the taxonomy Code a peer
// branches on and a human Message.
//
// CTX-002 and CTX-004 are DIFFERENT failures and must stay distinguishable: an
// oversize frame is a well-formed peer asking for too much (the connection may
// survive if the sender backs off), while a malformed one means the stream's
// framing is no longer trustworthy and CTX-004 requires closing rather than
// resynchronizing. Collapsing them into one error would erase the difference
// between "refuse this message" and "this connection is finished".
type FrameError struct {
	Code    string
	Message string
}

func (e *FrameError) Error() string { return e.Code + ": " + e.Message }

func frameErr(code, format string, args ...any) *FrameError {
	return &FrameError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WriteFrame writes one frame: the length prefix, then payload.
//
// An oversize payload is refused BEFORE any byte reaches w. A writer that
// emitted the prefix and then failed would leave the stream desynchronized —
// the peer would read the next frame's bytes as this one's payload — and that
// is precisely the unrecoverable state CTX-004 exists to prevent. Refusing
// up front keeps a rejected write a non-event on the wire.
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxFrameBytes {
		return frameErr(CodeFrameTooLarge,
			"payload is %d bytes, over the %d-byte ctx/1 frame ceiling (CTX-002)", len(payload), MaxFrameBytes)
	}
	var prefix [prefixBytes]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(payload)))
	if _, err := w.Write(prefix[:]); err != nil {
		return fmt.Errorf("ctxproto: write frame length: %w", err)
	}
	// A zero-length payload is a legal frame. Writing it is a no-op on most
	// writers, but the prefix above already told the peer to expect nothing, so
	// the two agree either way.
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("ctxproto: write frame payload: %w", err)
	}
	return nil
}

// ReadFrame reads exactly one frame's payload from r.
//
// A clean io.EOF at a frame boundary is returned VERBATIM, so a caller can tell
// an orderly close from a truncated frame. An EOF anywhere inside a frame —
// mid-prefix or mid-payload — is a truncation, reported as MALFORMED_FRAME:
// the peer promised N bytes and the stream ended early, so the framing is no
// longer trustworthy and CTX-004 says close rather than resynchronize.
func ReadFrame(r io.Reader) ([]byte, error) {
	var prefix [prefixBytes]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		if errors.Is(err, io.EOF) {
			// Nothing at all arrived: an orderly close between frames.
			return nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, frameErr(CodeMalformed,
				"stream ended inside the %d-byte length prefix (CTX-001)", prefixBytes)
		}
		return nil, fmt.Errorf("ctxproto: read frame length: %w", err)
	}

	n := binary.BigEndian.Uint32(prefix[:])
	// Checked BEFORE allocating. The length is a number a PEER chose, so
	// allocating it first would let one frame header ask this process for 4 GiB
	// — a refusal that arrives after the allocation is not a refusal.
	if n > MaxFrameBytes {
		return nil, frameErr(CodeFrameTooLarge,
			"peer announced a %d-byte payload, over the %d-byte ctx/1 frame ceiling (CTX-002)", n, MaxFrameBytes)
	}
	if n == 0 {
		return []byte{}, nil
	}

	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, frameErr(CodeMalformed,
				"stream ended before the announced %d payload bytes arrived (CTX-004)", n)
		}
		return nil, fmt.Errorf("ctxproto: read frame payload: %w", err)
	}
	return payload, nil
}
