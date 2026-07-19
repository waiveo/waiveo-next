// Package reenroll implements relay/1's Expired-certificate re-enrollment
// path (REL-020–029): the hardened recovery that lets a relay whose
// certificate expired during a long power-off recover its identity without a
// fresh claim credential, by proving possession of the expired certificate's
// own private key rather than re-claiming.
//
// This file holds the eligibility half (REL-021/022): a serial-record check,
// deliberately NOT a revocation-list scan. The app peer decides whether a
// presented expired certificate qualifies purely from its own issuance record
// for that relay_id — the ordered set of serials + public keys it has issued —
// admitting only the most-recently-issued, non-revoked serial and refusing a
// superseded or revoked one (CERT_EXPIRED_INELIGIBLE). A revocation list is
// the wrong oracle here: an old-enough revoked entry may already have aged out
// of it, so a stolen superseded certificate could slip through; the issuance
// record cannot forget that a newer certificate was issued (REL-021).
//
// Eligibility is evaluated against the app peer's own record and trusted time,
// never anything the relay's own untrusted clock asserts (REL-023) — this
// package takes only the record and the presented serial, no relay-supplied
// timestamp.
package reenroll

import "crypto/ed25519"

// Error taxonomy codes (contracts/relay-1.md, Error taxonomy) this path
// raises. Task 4 maps a ReEnrollError.Code onto an apihttp Problem `code`
// (REL-007's error frame).
const (
	// CodeExpiredIneligible refuses a presented expired certificate that is
	// superseded, revoked, or not the most-recently-issued on record
	// (REL-021/022).
	CodeExpiredIneligible = "CERT_EXPIRED_INELIGIBLE"
	// CodePopInvalid refuses a renew whose proof-of-possession signature is
	// absent, malformed, or does not verify (REL-027/028/029).
	CodePopInvalid = "RE_ENROLL_POP_INVALID"
	// CodeRateLimited refuses a renew once this relay_id's per-window bound
	// is exceeded (REL-025).
	CodeRateLimited = "RE_ENROLL_RATE_LIMITED"
)

// ReEnrollError is a typed refusal of the re-enrollment path carrying an
// Error-taxonomy Code and a human-readable Message. It is a Go error; Task 4
// renders it as the `error` frame / Problem body a peer receives (REL-007).
type ReEnrollError struct {
	Code    string
	Message string
}

// Error implements the error interface.
func (e *ReEnrollError) Error() string {
	return e.Code + ": " + e.Message
}

// IssuanceRecord is the app peer's own record of the certificates it has
// issued, indexed by relay_id — the sole oracle REL-021 permits for this
// path's eligibility (never a revocation list). The feeder's enroll.Server
// satisfies it structurally.
type IssuanceRecord interface {
	// MostRecentSerial returns the serial + public key of the
	// most-recently-issued certificate on record for relayID, and whether any
	// issuance is on record at all. serial is the same string form a relay
	// presents (its certificate's own SerialNumber).
	MostRecentSerial(relayID string) (serial string, pub ed25519.PublicKey, ok bool)
	// IsRevoked reports whether the certificate with this serial, issued to
	// relayID, has been revoked.
	IsRevoked(relayID, serial string) bool
}

// Eligible reports whether the certificate with presentedSerial qualifies for
// Expired-certificate re-enrollment under relayID, evaluated purely from rec
// (REL-021 — the issuance record, never a revocation list). It returns nil
// when presentedSerial is the most-recently-issued serial on record for
// relayID and is not revoked; otherwise a CERT_EXPIRED_INELIGIBLE
// ReEnrollError (REL-022): a relay_id with no issuance on record, a superseded
// serial (a newer certificate has since been issued), or a revoked serial are
// all refused outright — a credential-integrity signal, never a grant.
func Eligible(rec IssuanceRecord, relayID, presentedSerial string) *ReEnrollError {
	mostRecent, _, ok := rec.MostRecentSerial(relayID)
	if !ok {
		return &ReEnrollError{
			Code:    CodeExpiredIneligible,
			Message: "no certificate on record for this relay_id",
		}
	}
	if presentedSerial != mostRecent {
		return &ReEnrollError{
			Code:    CodeExpiredIneligible,
			Message: "presented certificate serial " + presentedSerial + " is superseded by " + mostRecent + " for this relay_id",
		}
	}
	if rec.IsRevoked(relayID, presentedSerial) {
		return &ReEnrollError{
			Code:    CodeExpiredIneligible,
			Message: "presented certificate serial " + presentedSerial + " is revoked for this relay_id",
		}
	}
	return nil
}
