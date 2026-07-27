package archive

import (
	"errors"
	"fmt"
)

// archive/1's error taxonomy (contracts/archive-1.md "Error taxonomy"): the
// closed set of codes a reader may attribute a refusal to. The whole point of
// the taxonomy is that the codes are DISTINGUISHABLE — ARC-015 and ARC-074 both
// exist solely to forbid collapsing them into one undifferentiated failure,
// because their remedies differ ("retype the passphrase" vs "get an untampered
// copy" vs "get the missing tail"). Callers branch on Code, never on message
// text.
//
// This package raises the subset a container read can actually reach on its own
// bytes. The remainder (EPOCH_TOO_NEW, BASE_ARCHIVE_UNAVAILABLE,
// ASSET_UNAVAILABLE, PACK_YANKED_BLOCKED, DEV_CHANNEL_REFUSED) belong to a
// RESTORER — they are decided against a destination's schema epoch, asset store,
// base-archive chain, and live trust state, none of which this package has or
// wants. They are declared here anyway so the restorer that grows on top of this
// package spells them the same way rather than minting a parallel vocabulary.
const (
	// CodeFormatUnrecognized: the outer header's `format` field was not
	// `waiveo-archive`, or the outer header was not readable as one at all
	// (ARC-003).
	CodeFormatUnrecognized = "FORMAT_UNRECOGNIZED"
	// CodeVersionUnsupported: `archive_format_version`'s major component does
	// not match this reader's (ARC-004).
	CodeVersionUnsupported = "VERSION_UNSUPPORTED"
	// CodeArchiveSignatureInvalid: the header signature failed verification
	// against the header it covers (ARC-021/023), or the digest recomputed over
	// the actual streamed encrypted body did not match the header's `digest`
	// (ARC-024).
	CodeArchiveSignatureInvalid = "ARCHIVE_SIGNATURE_INVALID"
	// CodeDecryptFailed: an encrypted-body frame failed AEAD authentication
	// (ARC-014) — most commonly a wrong export passphrase.
	CodeDecryptFailed = "DECRYPT_FAILED"
	// CodeArchiveTruncated: the encrypted body reached EOF without an
	// authenticated final-marked frame, or bytes followed the final-marked frame
	// (ARC-016).
	CodeArchiveTruncated = "ARCHIVE_TRUNCATED"
	// CodeManifestInvalid: the manifest failed a required-shape, uniqueness, or
	// asset-completeness check (ARC-033/062/064).
	CodeManifestInvalid = "MANIFEST_INVALID"

	// The restorer-owned remainder of the taxonomy. Declared, not raised here.
	CodeEpochTooNew            = "EPOCH_TOO_NEW"
	CodeBaseArchiveUnavailable = "BASE_ARCHIVE_UNAVAILABLE"
	CodeAssetUnavailable       = "ASSET_UNAVAILABLE"
	CodePackYankedBlocked      = "PACK_YANKED_BLOCKED"
	CodeDevChannelRefused      = "DEV_CHANNEL_REFUSED"
)

// Error is a refusal typed with one of the taxonomy's codes. It carries the
// code as DATA rather than encoding it in the message, so ARC-015's
// "MUST be distinguishable, by its own code" is satisfied by a caller doing
// archive.Code(err) == archive.CodeDecryptFailed — a check that cannot rot when
// someone rewords the human-readable half.
type Error struct {
	// ErrCode is the taxonomy code (one of the Code* constants).
	ErrCode string
	// Detail is the human-readable explanation. It is diagnostic only; nothing
	// in this package or the contract conditions behavior on its text.
	Detail string
	// Err is the underlying cause, if any, so errors.Is/As still reach it.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("archive: %s: %s: %v", e.ErrCode, e.Detail, e.Err)
	}
	return fmt.Sprintf("archive: %s: %s", e.ErrCode, e.Detail)
}

func (e *Error) Unwrap() error { return e.Err }

// Code returns the taxonomy code err was typed with, or "" if err is not a
// typed archive refusal (a plain I/O failure, a caller misuse, a nil error).
// This is the accessor every caller and test branches on — never the message.
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.ErrCode
	}
	return ""
}

// codedf builds a typed refusal with a formatted detail.
func codedf(code string, format string, args ...any) *Error {
	return &Error{ErrCode: code, Detail: fmt.Sprintf(format, args...)}
}

// wrapf builds a typed refusal that keeps an underlying cause reachable through
// errors.Is/As while still answering Code().
func wrapf(code string, cause error, format string, args ...any) *Error {
	return &Error{ErrCode: code, Detail: fmt.Sprintf(format, args...), Err: cause}
}
