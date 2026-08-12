package ctxproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// A frame round-trips its payload byte-for-byte, and the wire form is exactly
// what CTX-001 specifies: a 4-byte big-endian length counting the PAYLOAD only.
// The on-wire assertion matters as much as the round-trip — a codec that is
// self-consistent but disagrees with the contract talks to nothing.
func TestAFrameRoundTripsAndMatchesTheSpecifiedWireForm(t *testing.T) {
	payload := []byte("hello pack")
	var buf bytes.Buffer
	if err := WriteFrame(&buf, payload); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	wire := buf.Bytes()
	if got, want := len(wire), prefixBytes+len(payload); got != want {
		t.Fatalf("frame is %d bytes, want %d (prefix counts the payload only, never itself — CTX-001)", got, want)
	}
	if got := binary.BigEndian.Uint32(wire[:prefixBytes]); got != uint32(len(payload)) {
		t.Fatalf("length prefix = %d, want %d", got, len(payload))
	}
	if !bytes.Equal(wire[prefixBytes:], payload) {
		t.Fatalf("payload on the wire = %q, want %q", wire[prefixBytes:], payload)
	}

	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-tripped %q, want %q", got, payload)
	}
}

// Several frames on one stream read back in order, each with its own boundary.
// This is what a length prefix BUYS, so it is worth asserting directly: a codec
// that only ever handles one frame per stream would pass every test above.
func TestFramesOnOneStreamKeepTheirBoundaries(t *testing.T) {
	want := [][]byte{[]byte("first"), {}, []byte("third is longer")}
	var buf bytes.Buffer
	for _, p := range want {
		if err := WriteFrame(&buf, p); err != nil {
			t.Fatalf("WriteFrame(%q): %v", p, err)
		}
	}
	for i, w := range want {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame %d: %v", i, err)
		}
		if !bytes.Equal(got, w) {
			t.Fatalf("frame %d = %q, want %q", i, got, w)
		}
	}
	if _, err := ReadFrame(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last frame = %v, want a clean io.EOF at the boundary", err)
	}
}

// A zero-length payload is a legal frame, distinct from end-of-stream. If those
// two collapsed, a peer sending an empty body would look like a peer hanging up.
func TestAnEmptyPayloadIsAFrameNotAnEndOfStream(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, nil); err != nil {
		t.Fatalf("WriteFrame(nil): %v", err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("empty frame read back as %v (len %d), want a non-nil empty payload", got, len(got))
	}
}

// CTX-002 is a ceiling, not a target: exactly MaxFrameBytes is legal and one
// byte more is refused. An off-by-one here is the difference between a working
// peer and one that cannot send its largest legal message.
func TestTheSizeCeilingAdmitsExactlyMaxAndRefusesOneMore(t *testing.T) {
	if err := WriteFrame(io.Discard, make([]byte, MaxFrameBytes)); err != nil {
		t.Fatalf("a payload of exactly MaxFrameBytes must be legal (CTX-002): %v", err)
	}
	err := WriteFrame(io.Discard, make([]byte, MaxFrameBytes+1))
	assertFrameCode(t, err, CodeFrameTooLarge)
}

// An oversize write must not put a single byte on the wire. A writer that
// emitted the prefix and then refused would desynchronize the stream — the peer
// would read the NEXT frame's bytes as this one's payload, the unrecoverable
// state CTX-004 exists to prevent.
func TestAnOversizeWriteEmitsNothingAtAll(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, make([]byte, MaxFrameBytes+1)); err == nil {
		t.Fatal("oversize write succeeded")
	}
	if buf.Len() != 0 {
		t.Fatalf("a refused write put %d bytes on the wire; it must put none", buf.Len())
	}
}

// A peer's announced length is a number THIS process would allocate, so the
// ceiling has to be checked before the allocation. Driven through a reader that
// supplies only the prefix: if the ceiling were enforced after the read, this
// would try to allocate the announced size and hang or die rather than return.
func TestAnOversizeANNOUNCEDLengthIsRefusedWithoutAllocating(t *testing.T) {
	var prefix [prefixBytes]byte
	binary.BigEndian.PutUint32(prefix[:], ^uint32(0)) // ~4 GiB, announced by a hostile peer
	_, err := ReadFrame(bytes.NewReader(prefix[:]))
	assertFrameCode(t, err, CodeFrameTooLarge)
}

// Truncation is MALFORMED_FRAME, and the two truncation sites are distinct:
// inside the prefix, and inside the payload. Both mean the framing can no longer
// be trusted (CTX-004), and neither may be reported as a clean EOF — a caller
// that read a truncated stream as an orderly close would treat a severed
// connection as a peer that finished.
func TestTruncationIsMalformedNeverACleanEOF(t *testing.T) {
	t.Run("inside the length prefix", func(t *testing.T) {
		_, err := ReadFrame(bytes.NewReader([]byte{0x00, 0x01})) // 2 of 4 prefix bytes
		assertFrameCode(t, err, CodeMalformed)
		if errors.Is(err, io.EOF) {
			t.Fatal("a truncated prefix reported a clean EOF")
		}
	})
	t.Run("inside the payload", func(t *testing.T) {
		var buf bytes.Buffer
		_ = WriteFrame(&buf, []byte("twenty bytes of body"))
		short := buf.Bytes()[:prefixBytes+5] // the prefix promises 20, only 5 arrive
		_, err := ReadFrame(bytes.NewReader(short))
		assertFrameCode(t, err, CodeMalformed)
		if errors.Is(err, io.EOF) {
			t.Fatal("a truncated payload reported a clean EOF")
		}
	})
}

// An empty stream is an orderly close, not a malformed frame — the one EOF that
// must pass through verbatim so a supervisor can tell "the pack exited" from
// "the pack is talking nonsense".
func TestAnEmptyStreamIsACleanEOF(t *testing.T) {
	_, err := ReadFrame(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream = %v, want io.EOF verbatim", err)
	}
	var fe *FrameError
	if errors.As(err, &fe) {
		t.Fatalf("empty stream reported a typed framing failure (%s); it is an orderly close", fe.Code)
	}
}

// A transport failure is not a framing verdict. A write that fails mid-frame is
// the connection's problem, and dressing it as FRAME_TOO_LARGE or
// MALFORMED_FRAME would tell a peer its message was bad when the socket broke.
func TestATransportFailureIsNotReportedAsAFramingVerdict(t *testing.T) {
	err := WriteFrame(failingWriter{}, []byte("body"))
	if err == nil {
		t.Fatal("a failing writer returned no error")
	}
	var fe *FrameError
	if errors.As(err, &fe) {
		t.Fatalf("transport failure reported as the framing code %s", fe.Code)
	}
	if !strings.Contains(err.Error(), "ctxproto:") {
		t.Fatalf("error %q does not identify its layer", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("socket gone") }

func assertFrameCode(t *testing.T, err error, want string) {
	t.Helper()
	var fe *FrameError
	if !errors.As(err, &fe) {
		t.Fatalf("error = %v (%T), want a *FrameError with code %s", err, err, want)
	}
	if fe.Code != want {
		t.Fatalf("code = %q, want %q", fe.Code, want)
	}
}
