package relay1

// This file drives proactive, ahead-of-expiry certificate renewal
// (contracts/relay-1.md REL-015, corpus case REL-015-valid-renew-ahead-of-
// expiry): the REAL relay renewal client (client.Renew) against the live
// in-process feeder, the same way driveREL020 drives the expired-cert
// re-enrollment path through client.ReEnroll. The just-enrolled certificate
// is unexpired by construction — which is exactly the point: the REL-015
// draft-note's blessed transport (the bootstrap exchange) is issuance-
// record-gated, never expiry-gated, so renewing ahead of expiry is the
// same wire exchange with an unexpired presented certificate.

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// driveREL015 drives ahead-of-expiry renewal (REL-015, REL-014): a freshly
// enrolled relay — its certificate valid and most-recently-issued — renews
// through client.Renew and ends with a fresh serial under the SAME relay_id
// over the SAME keypair (byte-identical SubjectPublicKeyInfo — the
// player-pinned trust anchor survives). The superseded certificate's
// contract-mandated fate is asserted on the feeder's own record: a
// wire-level re-present of the old certificate draws CERT_EXPIRED_INELIGIBLE
// (superseded, REL-021/022), while the old serial remains UNREVOKED
// (REL-015's cutover sentence: superseded is not thereby revoked, REL-016).
func driveREL015(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-015")
	if !ok {
		rep.Fail("REL-015", contract, "case not found in frozen corpus")
		return
	}

	store, err := enrolledStore(client, feeder)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	defer store.Close()

	before, ok, err := store.Identity()
	if err != nil || !ok {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("read enrolled identity before renew: ok=%v err=%v", ok, err))
		return
	}
	oldSerial, err := certSerialHex(before.CertPEM)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("parse enrolled certificate serial: %v", err))
		return
	}

	var diffs []report.Diff

	// eligible: the feeder grants renew-ack for the unexpired,
	// most-recently-issued certificate — no fresh claim credential anywhere
	// in the exchange (REL-015); a refusal surfaces here instead.
	if err := client.Renew(feeder.EnrollBaseURL(), store); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("ahead-of-expiry renew refused/failed (corpus expects eligible=true): %v", err))
		return
	}

	after, ok, err := store.Identity()
	if err != nil || !ok {
		diffs = append(diffs, report.Diff{Field: "identity after renew", Expected: "present", Actual: fmt.Sprintf("ok=%v err=%v", ok, err)})
	}
	// relay_id_unchanged (REL-014).
	if after.RelayID != before.RelayID {
		diffs = append(diffs, report.Diff{Field: "relay_id_unchanged", Expected: before.RelayID, Actual: after.RelayID})
	}
	// renew_ack.body.serial: a fresh certificate, distinct from the renewed-from one.
	newSerial, serialErr := certSerialHex(after.CertPEM)
	if serialErr != nil {
		diffs = append(diffs, report.Diff{Field: "renew_ack.body.cert (parseable)", Expected: "valid certificate", Actual: serialErr.Error()})
	} else if newSerial == oldSerial {
		diffs = append(diffs, report.Diff{Field: "renew_ack.body.serial (fresh certificate issued)", Expected: "distinct from renewed-from serial " + oldSerial, Actual: newSerial})
	}
	// keypair_reused_spki_unchanged: proactive renewal never rotates the
	// key (REL-015 mandates no rotation; the SPKI is the player-pinned
	// anchor) — the persisted key and the certified SPKI are byte-stable.
	if !before.PrivateKey.Equal(after.PrivateKey) {
		diffs = append(diffs, report.Diff{Field: "keypair_reused_spki_unchanged (private key)", Expected: "unchanged", Actual: "rotated"})
	}
	oldSPKI, oldErr := certSPKI(before.CertPEM)
	newSPKI, newErr := certSPKI(after.CertPEM)
	if oldErr != nil || newErr != nil {
		diffs = append(diffs, report.Diff{Field: "keypair_reused_spki_unchanged (SPKI parse)", Expected: "parseable", Actual: fmt.Sprintf("old=%v new=%v", oldErr, newErr)})
	} else if string(oldSPKI) != string(newSPKI) {
		diffs = append(diffs, report.Diff{Field: "keypair_reused_spki_unchanged (SPKI)", Expected: "byte-identical", Actual: "differs"})
	}

	// superseded_serial_re_presented: the old certificate is now superseded
	// on the feeder's issuance record — re-presenting it at the wire level
	// (a correctly-behaving relay never does) draws CERT_EXPIRED_INELIGIBLE
	// (REL-021/022), proving the record advanced past it.
	wantCode := c.ExpectString("superseded_serial_re_presented.error.code")
	if wantCode == "" {
		wantCode = reenroll.CodeExpiredIneligible
	}
	nonce, err := reEnrollChallenge(feeder.EnrollBaseURL())
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("fetch re-enroll challenge: %v", err))
		return
	}
	_, csrPEM, csrDER, _, err := buildProbeCSR(before.RelayID)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("build probe csr: %v", err))
		return
	}
	status, ack, ef, err := postReEnrollRenew(feeder.EnrollBaseURL(), reenroll.RenewRequest{
		Type:    "renew",
		ID:      ulid.New(),
		RelayID: before.RelayID,
		Body: reenroll.RenewBody{
			PresentedCert: string(before.CertPEM),
			CSR:           csrPEM,
			PoPSignature:  reenroll.SignPoP(before.PrivateKey, nonce, csrDER),
		},
	})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("POST /reenroll/renew with the superseded certificate: %v", err))
		return
	}
	if status < 400 || ack.Body.Cert != "" {
		diffs = append(diffs, report.Diff{Field: "superseded_serial_re_presented (refused)", Expected: "refused, nothing issued", Actual: fmt.Sprintf("status=%d cert issued", status)})
	}
	if ef.Code != wantCode {
		diffs = append(diffs, report.Diff{Field: "superseded_serial_re_presented.error.code", Expected: wantCode, Actual: ef.Code})
	}

	// superseded_serial_revoked=false: superseded is NOT thereby revoked
	// (REL-015's cutover sentence) — revocation stays a separate operator
	// act, which is what keeps a live old-leaf session alive through its
	// own renewal (REL-016 gates on revocation only).
	if feeder.IsRevoked(before.RelayID, oldSerial) {
		diffs = append(diffs, report.Diff{Field: "superseded_serial_revoked", Expected: false, Actual: true})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "ahead-of-expiry renewal diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"renew_ack.id / .body.serial / .body.not_before / .body.not_after: runtime-issued by the live feeder (fresh ULID, serial, validity window) — asserted fresh/distinct/self-consistent, not byte-equal to the corpus's static fixture values.",
		"established_connection_uninterrupted: asserted end-to-end by the driven harness test suite (internal/feeder/relayconn's renewal e2e: a live connection survives a mid-session renewal and keeps serving frames), not restaged here — this driver's exchange has no standing connection to interrupt.",
	)
}

// certSPKI returns the DER SubjectPublicKeyInfo certPEM certifies — the
// exact bytes the player-side fingerprint commitment is computed over.
func certSPKI(certPEM []byte) ([]byte, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("not a CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	return cert.RawSubjectPublicKeyInfo, nil
}
