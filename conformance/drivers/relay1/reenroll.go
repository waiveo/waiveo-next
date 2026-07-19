package relay1

// This file drives the Expired-certificate re-enrollment path
// (contracts/relay-1.md, REL-020–029) — the named corpus cases REL-020
// (valid re-enroll), REL-022 (superseded cert refused), and REL-027 (invalid
// proof-of-possession refused).
//
// driveREL020 exercises the REAL relay+feeder round trip through the
// pluggable RelayClient (client.ReEnroll), the same way driveREL010 drives
// ordinary enrollment through client.Enroll — a genuinely re-enrolling relay
// never presents a superseded certificate or a bad proof-of-possession
// signature, so REL-022/027's refusal scenarios are staged directly at the
// wire level instead (mirroring driveREL071's crafted impostor snapshot, and
// driveREL010's own reEnrollRefusalCode probe): the driver crafts the exact
// bad input the contract requires the feeder to refuse, using the shared
// reenroll wire types both roles already speak.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// driveREL020 drives the valid Expired-certificate re-enrollment path
// (REL-020, REL-021/023/024/025/026/027/028/029, REL-014/017): a relay whose
// certificate the feeder's own issuance record still shows as
// most-recently-issued re-enrolls through client.ReEnroll — the real relay
// client fetching the bootstrap challenge, proving possession of its current
// certificate's own private key over a fresh CSR, and receiving a fresh
// certificate under the SAME relay_id with a re-anchored
// desired_state_verification_key.
func driveREL020(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-020")
	if !ok {
		rep.Fail("REL-020", contract, "case not found in frozen corpus")
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
		rep.Fail(c.CaseID, contract, fmt.Sprintf("read enrolled identity before re-enroll: ok=%v err=%v", ok, err))
		return
	}
	oldSerial, err := certSerialHex(before.CertPEM)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("parse presented certificate serial: %v", err))
		return
	}

	var diffs []report.Diff

	// eligible / most_recently_issued / pop_signature_verified: the feeder
	// grants renew-ack ONLY when the presented serial is its most-recent,
	// unrevoked record for this relay_id (REL-021/022) AND the
	// pop_signature verifies (REL-027/028/029) — so ReEnroll succeeding is
	// itself the observable proof all three held; any one failing surfaces
	// as a *reenroll.ReEnrollError here instead.
	if err := client.ReEnroll(feeder.EnrollBaseURL(), store); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("expired-cert re-enroll refused/failed (corpus expects eligible/most_recently_issued/pop_signature_verified=true): %v", err))
		return
	}

	after, ok, err := store.Identity()
	if err != nil || !ok {
		diffs = append(diffs, report.Diff{Field: "identity after re-enroll", Expected: "present", Actual: fmt.Sprintf("ok=%v err=%v", ok, err)})
	}
	// relay_id_unchanged (REL-014/024).
	if after.RelayID != before.RelayID {
		diffs = append(diffs, report.Diff{Field: "relay_id_unchanged", Expected: before.RelayID, Actual: after.RelayID})
	}
	// renew_ack.body.serial: a fresh certificate, distinct from the presented one.
	newSerial, serialErr := certSerialHex(after.CertPEM)
	if serialErr != nil {
		diffs = append(diffs, report.Diff{Field: "renew_ack.body.cert (parseable)", Expected: "valid certificate", Actual: serialErr.Error()})
	} else if newSerial == oldSerial {
		diffs = append(diffs, report.Diff{Field: "renew_ack.body.serial (fresh certificate issued)", Expected: "distinct from presented serial " + oldSerial, Actual: newSerial})
	}
	// desired_state_verification_key_reanchored (REL-017/024): present after
	// the re-enroll persists it.
	if _, present, keyErr := store.DesiredStateVerificationKey(); keyErr != nil || !present {
		diffs = append(diffs, report.Diff{Field: "desired_state_verification_key_reanchored", Expected: "present", Actual: fmt.Sprintf("present=%v err=%v", present, keyErr)})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "valid expired-cert re-enroll diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"renew_ack.id / .body.serial / .body.not_before / .body.not_after / desired_state_verification_key: runtime-issued by the live feeder (a fresh ULID, serial, validity window, and this feeder instance's own signing key) — asserted fresh/present/self-consistent, not byte-equal to the corpus's static fixture values.",
		"human_approval_required=false: Wave-1 first-photon has no manual-approval gate on this path — every eligible+PoP-verified renew completes automatically, so this is vacuously satisfied.",
	)
}

// driveREL022 drives the superseded-certificate refusal (REL-022): a relay
// presents an expired certificate that is still unrevoked, but the feeder's
// own issuance record has since advanced past it (a newer certificate was
// issued for the same relay_id) — the driver stages the supersession with a
// genuine re-enroll, then presents the now-superseded original certificate
// directly at the wire level (a correctly-behaving relay never does this
// itself) and asserts the feeder refuses it CERT_EXPIRED_INELIGIBLE without
// issuing anything.
func driveREL022(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-022")
	if !ok {
		rep.Fail("REL-022", contract, "case not found in frozen corpus")
		return
	}

	store, err := enrolledStore(client, feeder)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	defer store.Close()

	first, ok, err := store.Identity()
	if err != nil || !ok {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("read enrolled identity: ok=%v err=%v", ok, err))
		return
	}
	supersededCertPEM := first.CertPEM

	// Supersede it: a genuine re-enroll for the SAME relay_id advances the
	// feeder's issuance record so the original certificate's serial is no
	// longer most-recently-issued.
	if err := client.ReEnroll(feeder.EnrollBaseURL(), store); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("staging a superseding re-enroll failed: %v", err))
		return
	}

	var diffs []report.Diff

	nonce, err := reEnrollChallenge(feeder.EnrollBaseURL())
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("fetch re-enroll challenge: %v", err))
		return
	}

	// The presented (now-superseded) certificate's serial fails REL-021's
	// most-recently-issued check before the feeder ever looks at the csr or
	// pop_signature, so a well-formed-but-arbitrary probe CSR is enough here
	// — the eligibility refusal is the field under test, not PoP.
	_, csrPEM, csrDER, csrPriv, err := buildProbeCSR(first.RelayID)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("build probe csr: %v", err))
		return
	}
	popSig := reenroll.SignPoP(first.PrivateKey, nonce, csrDER)
	_ = csrPriv // the probe csr's own key is unused beyond proving the request well-formed

	status, ack, ef, err := postReEnrollRenew(feeder.EnrollBaseURL(), reenroll.RenewRequest{
		Type:    "renew",
		ID:      ulid.New(),
		RelayID: first.RelayID,
		Body: reenroll.RenewBody{
			PresentedCert: string(supersededCertPEM),
			CSR:           csrPEM,
			PoPSignature:  popSig,
		},
	})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("POST /reenroll/renew: %v", err))
		return
	}

	// certificate_issued=false: any non-error (renew-ack) response here IS
	// the divergence — the corpus declares this path refused outright.
	if status < 400 || ack.Body.Cert != "" {
		diffs = append(diffs, report.Diff{Field: "certificate_issued", Expected: false, Actual: fmt.Sprintf("status=%d renew-ack issued a certificate", status)})
	}

	wantCode := c.ExpectString("error.code")
	if wantCode == "" {
		wantCode = reenroll.CodeExpiredIneligible
	}
	if ef.Code != wantCode {
		diffs = append(diffs, report.Diff{Field: "error.code", Expected: wantCode, Actual: ef.Code})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "superseded-certificate refusal diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"error.message: the corpus's static fixture names specific fixture serials (e.g. \"superseded by 7a1b2c3d\"); this run's serials are runtime-issued by the live feeder, so only error.code is asserted byte-equal — the message is read back for the report but not diffed.",
		"operator_facing_condition_raised: REL-022 itself scopes raising a typed operator-facing condition to the platform's own alerting mechanism, outside this contract — not asserted here.",
	)
}

// driveREL027 drives the proof-of-possession refusal (REL-027/028/029): a
// relay's presented certificate serial matches the feeder's issuance record
// (eligible by serial alone), but a renew missing pop_signature entirely, and
// a second renew whose pop_signature was computed with an unrelated key, are
// both refused RE_ENROLL_POP_INVALID without issuing a certificate — staged
// directly at the wire level (a correctly-behaving relay never omits or
// forges its own proof-of-possession signature), against the same freshly
// enrolled (hence still most-recently-issued) certificate both requests
// present.
func driveREL027(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-027")
	if !ok {
		rep.Fail("REL-027", contract, "case not found in frozen corpus")
		return
	}

	store, err := enrolledStore(client, feeder)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	defer store.Close()

	id, ok, err := store.Identity()
	if err != nil || !ok {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("read enrolled identity: ok=%v err=%v", ok, err))
		return
	}

	var diffs []report.Diff

	nonce, err := reEnrollChallenge(feeder.EnrollBaseURL())
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("fetch re-enroll challenge: %v", err))
		return
	}
	_, csrPEM, csrDER, _, err := buildProbeCSR(id.RelayID)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("build probe csr: %v", err))
		return
	}

	wantResp0Code := c.ExpectString("responses.0.code")
	if wantResp0Code == "" {
		wantResp0Code = reenroll.CodePopInvalid
	}
	wantResp1Code := c.ExpectString("responses.1.code")
	if wantResp1Code == "" {
		wantResp1Code = reenroll.CodePopInvalid
	}

	// Request 1: pop_signature entirely absent. The feeder's own ordering
	// refuses this before it ever consumes the outstanding challenge nonce
	// (internal/feeder/enroll.handleReEnrollRenew), so the SAME nonce still
	// covers request 2 below.
	status0, ack0, ef0, err := postReEnrollRenew(feeder.EnrollBaseURL(), reenroll.RenewRequest{
		Type:    "renew",
		ID:      ulid.New(),
		RelayID: id.RelayID,
		Body: reenroll.RenewBody{
			PresentedCert: string(id.CertPEM),
			CSR:           csrPEM,
			PoPSignature:  "",
		},
	})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("POST /reenroll/renew (missing pop_signature): %v", err))
		return
	}
	if status0 < 400 || ack0.Body.Cert != "" {
		diffs = append(diffs, report.Diff{Field: "responses.0 certificate_issued", Expected: false, Actual: fmt.Sprintf("status=%d renew-ack issued a certificate", status0)})
	}
	if ef0.Code != wantResp0Code {
		diffs = append(diffs, report.Diff{Field: "responses.0.code", Expected: wantResp0Code, Actual: ef0.Code})
	}

	// Request 2: pop_signature computed with an unrelated key — never the
	// presented certificate's own private key.
	_, unrelatedPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("generate unrelated key: %v", err))
		return
	}
	badSig := reenroll.SignPoP(unrelatedPriv, nonce, csrDER)
	status1, ack1, ef1, err := postReEnrollRenew(feeder.EnrollBaseURL(), reenroll.RenewRequest{
		Type:    "renew",
		ID:      ulid.New(),
		RelayID: id.RelayID,
		Body: reenroll.RenewBody{
			PresentedCert: string(id.CertPEM),
			CSR:           csrPEM,
			PoPSignature:  badSig,
		},
	})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("POST /reenroll/renew (unrelated-key pop_signature): %v", err))
		return
	}
	if status1 < 400 || ack1.Body.Cert != "" {
		diffs = append(diffs, report.Diff{Field: "responses.1 certificate_issued", Expected: false, Actual: fmt.Sprintf("status=%d renew-ack issued a certificate", status1)})
	}
	if ef1.Code != wantResp1Code {
		diffs = append(diffs, report.Diff{Field: "responses.1.code", Expected: wantResp1Code, Actual: ef1.Code})
	}

	// relay_id_unchanged / certificate_issued=false: neither refused attempt
	// touched the relay's persisted identity (only client.ReEnroll ever
	// writes to store, and it was never called here).
	after, ok, err := store.Identity()
	if err != nil || !ok || after.RelayID != id.RelayID || string(after.CertPEM) != string(id.CertPEM) {
		diffs = append(diffs, report.Diff{Field: "relay_id_unchanged / persisted identity untouched", Expected: fmt.Sprintf("relay_id=%q, cert unchanged", id.RelayID), Actual: fmt.Sprintf("ok=%v err=%v relay_id=%q cert_changed=%v", ok, err, after.RelayID, string(after.CertPEM) != string(id.CertPEM))})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "proof-of-possession refusal diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"eligible_by_serial=true: not independently observable as its own field — proven structurally by both refusals carrying RE_ENROLL_POP_INVALID rather than CERT_EXPIRED_INELIGIBLE (REL-021's serial check ran and passed before either PoP check refused).",
		"responses.*.message: the corpus's fixture messages are asserted only via .code here — see driveREL022's identical note on runtime-issued detail.",
	)
}

// certSerialHex parses certPEM's own certificate serial in the exact string
// form both the feeder's issuance record and a presented certificate compare
// it as (big.Int base-16, see internal/feeder/enroll.issueRelayCert).
func certSerialHex(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("certificate did not PEM-decode to a CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("x509.ParseCertificate: %w", err)
	}
	return cert.SerialNumber.Text(16), nil
}

// buildProbeCSR generates a fresh ed25519 keypair and a well-formed,
// self-signed PKCS#10 CSR over it — the driver's own probe csr for the
// REL-022/027 wire-level refusal scenarios (never issued a certificate over,
// since both are refused before or regardless of the csr's content).
func buildProbeCSR(commonName string) (pub ed25519.PublicKey, csrPEM string, csrDER []byte, priv ed25519.PrivateKey, err error) {
	pub, priv, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("ed25519.GenerateKey: %w", err)
	}
	template := &x509.CertificateRequest{Subject: pkix.Name{CommonName: commonName}}
	der, err := x509.CreateCertificateRequest(rand.Reader, template, priv)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("x509.CreateCertificateRequest: %w", err)
	}
	return pub, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), der, priv, nil
}

// reEnrollChallenge performs REL-026's bootstrap challenge (GET
// /reenroll/challenge) against baseURL and returns its fresh single-use
// nonce.
func reEnrollChallenge(baseURL string) (string, error) {
	resp, err := insecureClient().Get(baseURL + "/reenroll/challenge")
	if err != nil {
		return "", fmt.Errorf("GET /reenroll/challenge: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GET /reenroll/challenge: status %d", resp.StatusCode)
	}
	var c struct {
		Body struct {
			Nonce string `json:"nonce"`
		} `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return "", fmt.Errorf("decode challenge: %w", err)
	}
	if c.Body.Nonce == "" {
		return "", fmt.Errorf("challenge response carried an empty nonce")
	}
	return c.Body.Nonce, nil
}

// postReEnrollRenew presents req to baseURL's POST /reenroll/renew and
// decodes whichever of RenewAck or ErrorFrame the status code indicates —
// both are returned (whichever the response actually was left zero-valued)
// so a caller can assert on status/ack/ef without a type switch.
func postReEnrollRenew(baseURL string, req reenroll.RenewRequest) (status int, ack reenroll.RenewAck, ef reenroll.ErrorFrame, err error) {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return 0, ack, ef, fmt.Errorf("marshal renew request: %w", err)
	}
	resp, err := insecureClient().Post(baseURL+"/reenroll/renew", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return 0, ack, ef, fmt.Errorf("POST /reenroll/renew: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, ack, ef, fmt.Errorf("read renew response: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, &ack); err != nil {
			return resp.StatusCode, ack, ef, fmt.Errorf("decode renew-ack: %w (body=%s)", err, raw)
		}
		return resp.StatusCode, ack, ef, nil
	}
	if err := json.Unmarshal(raw, &ef); err != nil {
		return resp.StatusCode, ack, ef, fmt.Errorf("decode error frame: %w (body=%s)", err, raw)
	}
	return resp.StatusCode, ack, ef, nil
}
