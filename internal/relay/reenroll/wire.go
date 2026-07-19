package reenroll

// This file holds the Expired-certificate re-enrollment bootstrap exchange's
// wire vocabulary (contracts/relay-1.md REL-020/024/027–029): the relay's
// `renew` request, the app peer's `renew-ack`, and the typed `error` frame it
// answers with in place of a `renew-ack` on refusal. Both roles share these
// types the same way internal/relay/hello's wire types are shared by the relay
// (client) and the feeder (app-peer server) — the feeder's re-enroll handler
// (internal/feeder/enroll) decodes a RenewRequest and answers a RenewAck or an
// ErrorFrame; the relay's re-enroll client (a later task) builds the
// RenewRequest and reads the reply. The challenge that precedes this exchange
// reuses internal/relay/hello's own Challenge shape (REL-026), so it is not
// redefined here.
//
// json struct tags match contracts/relay-1.md's Wire-shapes section
// field-for-field; when the contract's field set changes, update the types
// here in the same change that bumps the contract.

// RenewRequest is the relay's `renew` presented through the
// Expired-certificate re-enrollment bootstrap exchange (REL-020, REL-027).
// Unlike the in-band renewal over an authenticated connection (REL-015), this
// bootstrap renew carries a pop_signature: the connection is not yet
// mutually-authenticated, so possession of the expired certificate's own
// private key is proven at the application layer instead (REL-027–029).
type RenewRequest struct {
	Type    string    `json:"type"`     // always "renew"
	ID      string    `json:"id"`       // echoed back in the renew-ack / error frame
	RelayID string    `json:"relay_id"` // the relay identity being re-enrolled (REL-024, unchanged)
	Body    RenewBody `json:"body"`
}

// RenewBody is RenewRequest's body. presented_cert is the (expired)
// certificate the relay is presenting for re-enrollment — the app peer reads
// only its serial, and evaluates eligibility against its OWN issuance record
// for that serial (REL-021), never trusting the presented certificate's
// embedded key. csr is the relay's fresh certificate-signing request (a brand
// new keypair, never the expired one). pop_signature proves possession of the
// presented certificate's own private key over the challenge nonce bound to
// this csr (REL-027) — see SignPoP / VerifyPoP.
type RenewBody struct {
	PresentedCert string `json:"presented_cert"`
	CSR           string `json:"csr"`
	PoPSignature  string `json:"pop_signature"`
}

// RenewAck is the app peer's success reply (REL-024): a fresh certificate
// issued under the SAME relay_id, plus the re-anchored
// desired_state_verification_key the relay replaces its persisted anchor with
// (REL-017/024). serial is the new certificate's own serial (the value that
// becomes most-recently-issued on the app peer's record); cert is that
// certificate PEM; not_before/not_after are its validity window as epoch
// milliseconds.
type RenewAck struct {
	Type    string       `json:"type"`     // always "renew-ack"
	ID      string       `json:"id"`       // the RenewRequest's id, echoed
	RelayID string       `json:"relay_id"` // the SAME relay_id (REL-024)
	Body    RenewAckBody `json:"body"`
}

// RenewAckBody is RenewAck's body.
type RenewAckBody struct {
	Serial                      string `json:"serial"`
	Cert                        string `json:"cert"`
	NotBefore                   int64  `json:"not_before"`
	NotAfter                    int64  `json:"not_after"`
	DesiredStateVerificationKey string `json:"desired_state_verification_key"`
}

// ErrorFrame is the typed refusal the app peer answers in place of a RenewAck
// (contracts/relay-1.md's RenewResponse-rejected shape): a relay/1 `error`
// message carrying the request's id, the response trace_id, and a taxonomy
// Code (CodeExpiredIneligible, CodePopInvalid, or CodeRateLimited) with a
// human-readable Message. It is the message-level error frame, distinct from
// the transport's own RFC-9457 problem+json used elsewhere.
type ErrorFrame struct {
	Type    string `json:"type"` // always "error"
	ID      string `json:"id"`
	TraceID string `json:"trace_id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
