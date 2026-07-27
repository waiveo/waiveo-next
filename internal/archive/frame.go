package archive

import (
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// DefaultFrameSize is the plaintext each body frame carries when Options.
// FrameSize is unset: 1 MiB, the contract's own draft-note proposal (ARC-013).
// It is the read-side and write-side memory floor — a reader buffers one frame
// at a time and nothing larger, which is what makes ARC-080/081's "memory use
// bounded independent of workspace size" true of a 22 GiB container.
const DefaultFrameSize = 1 << 20

// maxFrameCiphertext caps the ciphertext length a READER will honor from a
// frame's 4-byte length prefix. That prefix sits inside the encrypted body, so
// unlike every header field it is covered by no signature at the moment it is
// read — only by the whole-body digest, which cannot be checked until the body
// has been read. A hostile prefix could therefore ask for a 4 GiB allocation
// before anything has had a chance to refuse it. 64 MiB is 64x the default frame
// size (generous room for a writer that chose a larger one) and small enough
// that honoring a lie costs a bounded allocation.
const maxFrameCiphertext = 64 << 20

// The associated data ARC-013 requires every frame to bind: a boolean
// final-flag, one byte. Exactly one frame — the last — is written with aadFinal;
// every other frame is written with aadNonFinal.
//
// The flag is NOT transmitted anywhere. It exists only as AEAD associated data,
// which is precisely what makes ARC-014's "a reader MUST treat a frame's
// final-flag as trustworthy once, and only once, that frame has itself
// authenticated" hold by construction: a reader LEARNS the flag by discovering
// which of the two associated-data values authenticates the frame, so a
// forged or flipped flag is not a thing that can exist — flipping it means the
// frame does not authenticate at all. A flag carried in the clear (say, in a
// spare bit of the length prefix) would have needed a separate argument for why
// it cannot be flipped; this needs none.
var (
	aadNonFinal = []byte{0x00}
	aadFinal    = []byte{0x01}
)

// deriveFrameNonce produces frame seq's XChaCha20-Poly1305 nonce from the
// per-archive base nonce (ARC-013).
//
// Scheme: the 24-byte base nonce is copied verbatim, then the frame's sequence
// number is XORed in as a big-endian uint64 over the LAST 8 bytes. XOR against a
// fixed base is injective in seq, so distinct sequence numbers give distinct
// nonces with certainty — not merely with high probability — and frame 0's nonce
// is the base nonce itself. The leading 16 bytes stay untouched, so the whole
// 128-bit random prefix survives into every frame's nonce: XChaCha20's wide
// nonce is exactly what makes this arithmetic trivially collision-free, which is
// why the contract proposes it.
//
// Combined with ARC-017's requirement that the base nonce be fresh uniform
// randomness per archive, a (key, nonce) pair therefore cannot repeat within an
// archive (injectivity) and cannot plausibly repeat across archives (128 random
// bits in the untouched prefix, on top of a per-archive random KDF salt already
// making the key itself per-archive).
//
// Reordering resistance falls out of the same derivation for free: a frame moved
// to a different position is decrypted under a different nonce and simply fails
// to authenticate, so the sequence number does not additionally need to be bound
// as associated data.
func deriveFrameNonce(base []byte, seq uint64) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	copy(nonce, base)

	var seqBE [8]byte
	binary.BigEndian.PutUint64(seqBE[:], seq)
	for i := 0; i < len(seqBE); i++ {
		nonce[chacha20poly1305.NonceSizeX-len(seqBE)+i] ^= seqBE[i]
	}
	return nonce
}

// frameWriter is the write half of ARC-013: it accumulates plaintext, seals it
// into independently authenticated frames of at most frameSize plaintext bytes
// each, and emits `4-byte big-endian ciphertext length || ciphertext+tag`.
//
// Its one subtle rule is the final flag. A frame may only be emitted as
// non-final once the writer KNOWS more plaintext exists, because exactly one
// frame — the last — carries the final flag (ARC-013). So a full buffer is not
// flushed eagerly: it is flushed at the moment the next byte arrives, and
// whatever remains at Close becomes the final frame. Close always emits, even
// with an empty buffer, so a container always contains at least one frame and a
// reader always has a final-marked frame to find (ARC-016).
type frameWriter struct {
	dst       io.Writer
	aead      cipher.AEAD
	baseNonce []byte
	frameSize int

	buf    []byte // pending plaintext, never longer than frameSize
	ct     []byte // reusable seal destination, so steady state allocates nothing
	seq    uint64
	closed bool
}

func newFrameWriter(dst io.Writer, aead cipher.AEAD, baseNonce []byte, frameSize int) *frameWriter {
	return &frameWriter{
		dst:       dst,
		aead:      aead,
		baseNonce: baseNonce,
		frameSize: frameSize,
		buf:       make([]byte, 0, frameSize),
		ct:        make([]byte, 0, frameSize+aead.Overhead()),
	}
}

func (w *frameWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("archive: write to a closed frame writer")
	}
	total := len(p)
	for len(p) > 0 {
		if len(w.buf) == w.frameSize {
			// The buffer is full AND more input remains, so this frame is
			// provably not the last one — only now may it go out as non-final.
			if err := w.emit(false); err != nil {
				return total - len(p), err
			}
		}
		n := w.frameSize - len(w.buf)
		if n > len(p) {
			n = len(p)
		}
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
	}
	return total, nil
}

// Close emits whatever plaintext remains as the FINAL frame (ARC-013) and is
// idempotent. It emits even when nothing remains, guaranteeing at least one
// frame per container.
func (w *frameWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.emit(true)
}

func (w *frameWriter) emit(final bool) error {
	aad := aadNonFinal
	if final {
		aad = aadFinal
	}
	nonce := deriveFrameNonce(w.baseNonce, w.seq)
	w.ct = w.aead.Seal(w.ct[:0], nonce, w.buf, aad)

	var lenBE [4]byte
	binary.BigEndian.PutUint32(lenBE[:], uint32(len(w.ct)))
	if _, err := w.dst.Write(lenBE[:]); err != nil {
		return fmt.Errorf("archive: write frame %d length: %w", w.seq, err)
	}
	if _, err := w.dst.Write(w.ct); err != nil {
		return fmt.Errorf("archive: write frame %d: %w", w.seq, err)
	}

	w.buf = w.buf[:0]
	w.seq++
	return nil
}

// frameReader is the read half of ARC-013/014/016/024: an io.Reader over the
// decrypted plaintext of a container's body, which authenticates every frame,
// enforces the frame sequence's completeness, and recomputes the body digest
// over the ciphertext bytes exactly as they appeared (ARC-020).
//
// Every refusal is STICKY. Once a frame fails to authenticate, no further frame
// is attempted and no plaintext from the failed frame is ever handed out
// (ARC-014) — the consumer above (zstd, then tar) sees the same typed error on
// every subsequent Read, and Open reports that error in preference to whatever
// confused thing the decompressor said about the truncated stream it was handed.
type frameReader struct {
	src       io.Reader
	aead      cipher.AEAD
	baseNonce []byte

	hasher     hash.Hash // over the body region's bytes exactly as they appear
	wantDigest string    // the header's `digest`, hex (ARC-002/024)

	plain    []byte // undelivered plaintext of the frame just authenticated
	seq      uint64
	sawFinal bool
	err      error // sticky; the first refusal wins
}

func newFrameReader(src io.Reader, aead cipher.AEAD, baseNonce []byte, hasher hash.Hash, wantDigest string) *frameReader {
	return &frameReader{
		src:        src,
		aead:       aead,
		baseNonce:  baseNonce,
		hasher:     hasher,
		wantDigest: wantDigest,
	}
}

func (r *frameReader) Read(p []byte) (int, error) {
	for {
		if r.err != nil {
			return 0, r.err
		}
		if len(r.plain) > 0 {
			n := copy(p, r.plain)
			r.plain = r.plain[n:]
			return n, nil
		}
		if r.sawFinal {
			return 0, io.EOF
		}
		if err := r.nextFrame(); err != nil {
			r.err = err
			return 0, err
		}
	}
}

// nextFrame reads, authenticates, and stages one frame's plaintext. Everything
// ARC-016 and ARC-024 require of the sequence as a whole happens here, at the
// moment the FINAL frame authenticates — which is the only moment a reader may
// trust that a frame is the last one.
func (r *frameReader) nextFrame() error {
	var lenBE [4]byte
	if _, err := io.ReadFull(r.src, lenBE[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// EOF without ever having authenticated a final-marked frame: the
			// tail is missing (ARC-016). Every frame delivered so far
			// authenticated individually, which is exactly why per-frame
			// authentication alone cannot catch this and this check must exist.
			return codedf(CodeArchiveTruncated,
				"reached EOF after %d frame(s) without an authenticated final-marked frame", r.seq)
		}
		return fmt.Errorf("archive: read frame %d length: %w", r.seq, err)
	}
	r.hasher.Write(lenBE[:])

	n := int(binary.BigEndian.Uint32(lenBE[:]))
	if n > maxFrameCiphertext {
		// A length no honest writer produces. Refused as DECRYPT_FAILED rather
		// than honored: the bytes cannot be a frame this reader would ever have
		// been able to authenticate, and the alternative — allocating what the
		// prefix asks for so the AEAD can say the same thing — is the allocation
		// this cap exists to prevent.
		return codedf(CodeDecryptFailed,
			"frame %d declares %d ciphertext bytes, beyond the %d-byte maximum this reader will buffer",
			r.seq, n, maxFrameCiphertext)
	}
	if n < r.aead.Overhead() {
		return codedf(CodeDecryptFailed,
			"frame %d declares %d ciphertext bytes, too few to carry an authentication tag", r.seq, n)
	}

	ct := make([]byte, n)
	if _, err := io.ReadFull(r.src, ct); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return codedf(CodeArchiveTruncated,
				"frame %d claims %d ciphertext bytes but the file ends first", r.seq, n)
		}
		return fmt.Errorf("archive: read frame %d: %w", r.seq, err)
	}
	r.hasher.Write(ct)

	// Trial-authenticate against both associated-data values. The final-flag is
	// carried nowhere but here, so the flag IS whichever value authenticates —
	// non-final first, since all but one frame are non-final. Failing both is
	// ARC-014: abort immediately, emit nothing from this frame, attempt no
	// further frames (the caller makes the error sticky).
	nonce := deriveFrameNonce(r.baseNonce, r.seq)
	plain, err := r.aead.Open(nil, nonce, ct, aadNonFinal)
	final := false
	if err != nil {
		plain, err = r.aead.Open(nil, nonce, ct, aadFinal)
		if err != nil {
			return codedf(CodeDecryptFailed,
				"frame %d failed authentication (wrong export passphrase, or the archive was tampered with or corrupted)", r.seq)
		}
		final = true
	}

	r.seq++
	if final {
		r.sawFinal = true
		if err := r.checkTail(); err != nil {
			return err
		}
		if err := r.checkDigest(); err != nil {
			return err
		}
	}
	r.plain = plain
	return nil
}

// checkTail enforces ARC-016's second condition: no byte may follow the
// final-marked frame. A frame appended after the final one authenticates on its
// own terms, so nothing but this explicit look-ahead can catch a body that grew
// a tail — and a body that grew a tail is a body someone edited.
func (r *frameReader) checkTail() error {
	var probe [1]byte
	n, err := io.ReadFull(r.src, probe[:])
	if n > 0 {
		return codedf(CodeArchiveTruncated, "at least one byte follows the final-marked frame")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("archive: read past final frame: %w", err)
	}
	return nil
}

// checkDigest is ARC-024: recompute sha256 over the encrypted body's actual
// streamed bytes and compare against the header's `digest`.
//
// It runs only once ARC-016's completeness check has passed, exactly as ARC-024
// orders it — a truncated body would fail this comparison too, but reporting
// "signature invalid" for missing bytes would send an operator looking for
// tampering when the remedy is to fetch a complete copy.
//
// The header signature (ARC-023) already proved the recorded digest value was
// validly signed; this proves the bytes actually delivered are the bytes that
// value describes. Passing both is what makes a restore's verification cover the
// BODY, not merely the header's claim about it.
func (r *frameReader) checkDigest() error {
	got := hex.EncodeToString(r.hasher.Sum(nil))
	if got != r.wantDigest {
		return codedf(CodeArchiveSignatureInvalid,
			"body digest %s does not match the signed header's digest %s", got, r.wantDigest)
	}
	return nil
}

// Finish reports the body's verdict once its consumer is done. It must be called
// after the plaintext has been consumed, because a consumer can legitimately
// stop early: a tar reader stops at the archive's end-of-file marker and never
// touches the bytes after it, so the final frame — and with it ARC-016's
// completeness check and ARC-024's digest comparison — would otherwise never be
// reached. Finish drains whatever remains to force both.
func (r *frameReader) Finish() error {
	buf := make([]byte, 32*1024)
	for r.err == nil && !r.sawFinal {
		if _, err := r.Read(buf); err != nil {
			break // io.EOF, or a typed refusal already stuck onto r.err
		}
	}
	if r.err != nil {
		return r.err
	}
	if !r.sawFinal {
		return codedf(CodeArchiveTruncated,
			"reached EOF after %d frame(s) without an authenticated final-marked frame", r.seq)
	}
	return nil
}
