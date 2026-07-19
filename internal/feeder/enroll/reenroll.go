package enroll

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// handleReEnrollChallenge issues the Expired-certificate re-enrollment path's
// bootstrap challenge nonce (REL-026): a fresh, single-use nonce of at least
// 128 bits, reusing the connection handshake's own challenge shape
// (hello.NewChallenge / REL-030). It records the nonce as the one outstanding
// re-enroll challenge — Wave-1 first-photon has a single co-located relay, so
// the app peer tracks exactly one at a time, consumed by the renew that binds
// it (a per-connection nonce table lands with the concurrent transport in a
// later wave, mirroring internal/relay/hello's AppPeerServer).
func (s *Server) handleReEnrollChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	c, err := hello.NewChallenge()
	if err != nil {
		apihttp.WriteProblem(w, r, apihttp.TraceID(r), http.StatusInternalServerError, "INTERNAL", "Internal Error")
		return
	}

	s.mu.Lock()
	s.reOutstanding = c.Body.Nonce
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, c)
}

// handleReEnrollRenew is the app peer's `renew` handler for the
// Expired-certificate re-enrollment path (REL-020/022/024/025/029). In order,
// it: rate-limits per relay_id (REL-025); evaluates serial eligibility against
// its own issuance record (REL-021/022); requires and verifies the
// proof-of-possession signature against the public key on record for the
// presented serial (REL-027/028/029); and, only when all pass, issues a fresh
// certificate under the SAME relay_id and re-anchors the desired-state
// verification key (REL-024), advancing the issuance record. Every check uses
// the feeder's own trusted time and record, never the relay's clock (REL-023).
// A refusal is answered with the relay/1 `error` frame (reenroll.ErrorFrame)
// in place of a renew-ack.
func (s *Server) handleReEnrollRenew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req reenroll.RenewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeReEnrollError(w, r, "", http.StatusBadRequest, reenroll.CodePopInvalid, "malformed re-enroll renew request")
		return
	}

	// REL-025: rate-limit per relay_id, on the feeder's own trusted time
	// (REL-023), ahead of the eligibility/PoP work so it bounds churn attempts.
	if !s.reLimiter.Allow(req.RelayID, time.Now().UnixMilli()) {
		s.writeReEnrollError(w, r, req.ID, http.StatusTooManyRequests, reenroll.CodeRateLimited,
			"re-enrollment attempts for this relay_id exceeded the permitted rate")
		return
	}

	// The presented (expired) certificate's serial — the app peer reads only
	// its serial and checks it against its OWN issuance record (REL-021), never
	// trusting the presented certificate's embedded key.
	presentedSerial, ok := parsePresentedSerial(req.Body.PresentedCert)
	if !ok {
		s.writeReEnrollError(w, r, req.ID, http.StatusForbidden, reenroll.CodeExpiredIneligible,
			"presented certificate is missing or could not be parsed")
		return
	}

	// REL-021/022: serial-record eligibility. Superseded/revoked/unknown are
	// refused CERT_EXPIRED_INELIGIBLE with the record-derived message.
	if eligErr := reenroll.Eligible(s, req.RelayID, presentedSerial); eligErr != nil {
		s.writeReEnrollError(w, r, req.ID, http.StatusForbidden, eligErr.Code, eligErr.Message)
		return
	}

	// REL-029: an absent pop_signature is refused outright — a serial match is
	// necessary but never sufficient (REL-028).
	if req.Body.PoPSignature == "" {
		s.writeReEnrollError(w, r, req.ID, http.StatusForbidden, reenroll.CodePopInvalid,
			"proof-of-possession signature missing")
		return
	}

	// The fresh csr the new certificate is issued over. Its own DER is the exact
	// bytes the pop_signature binds (REL-027), and its self-signature proves the
	// relay holds the NEW keypair too.
	csr, csrDER, newPub, ok := parseFreshCSR(req.Body.CSR)
	if !ok {
		s.writeReEnrollError(w, r, req.ID, http.StatusForbidden, reenroll.CodePopInvalid,
			"re-enrollment csr is malformed or its self-signature is invalid")
		return
	}

	// The public key on record for the presented serial (REL-028) — eligibility
	// above guarantees this is present and is the presented serial's key.
	_, recordedPub, present := s.MostRecentSerial(req.RelayID)

	// Consume the outstanding challenge nonce: single-use per attempt (REL-026).
	s.mu.Lock()
	nonce := s.reOutstanding
	s.reOutstanding = ""
	s.mu.Unlock()

	// REL-028: verify the pop_signature against the recorded key over the exact
	// nonce+csr binding. An absent challenge (no nonce), a signature made with
	// any other key, or one over any other nonce/csr all fail closed (REL-029).
	if !present || nonce == "" || !reenroll.VerifyPoP(recordedPub, nonce, csrDER, req.Body.PoPSignature) {
		s.writeReEnrollError(w, r, req.ID, http.StatusForbidden, reenroll.CodePopInvalid,
			"proof-of-possession signature did not verify against the certificate on record for this serial")
		return
	}

	// REL-024: issue a fresh certificate under the SAME relay_id and advance the
	// issuance record (the new serial + key becomes most-recently-issued).
	certPEM, serial, notBefore, notAfter, err := s.issueRelayCert(csr, req.RelayID)
	if err != nil {
		apihttp.WriteProblem(w, r, apihttp.TraceID(r), http.StatusInternalServerError, "INTERNAL", "Internal Error")
		return
	}
	s.mu.Lock()
	s.relayKeys[req.RelayID] = newPub
	s.recordIssuance(req.RelayID, serial, newPub, notBefore)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, reenroll.RenewAck{
		Type:    "renew-ack",
		ID:      req.ID,
		RelayID: req.RelayID, // REL-024: unchanged relay_id
		Body: reenroll.RenewAckBody{
			Serial:    serial,
			Cert:      string(certPEM),
			NotBefore: notBefore,
			NotAfter:  notAfter,
			// REL-017/024: re-anchor the relay's persisted desired-state
			// verification key to the feeder's own current signing pub.
			DesiredStateVerificationKey: encodeVerificationKey(s.identity.SigningPub()),
		},
	})
}

// parsePresentedSerial extracts the serial (in the same string form the
// issuance record stores, big.Int base-16) from a presented certificate PEM,
// reporting ok=false for a missing or unparseable one.
func parsePresentedSerial(certPEM string) (string, bool) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return "", false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", false
	}
	return cert.SerialNumber.Text(16), true
}

// parseFreshCSR decodes and validates the relay's fresh certificate-signing
// request, returning the parsed request, its DER (the bytes the pop_signature
// binds, REL-027), and its ed25519 public key. ok is false for a malformed
// PEM/DER, a self-signature that does not verify (proof the relay holds the new
// key), or a non-ed25519 key.
func parseFreshCSR(csrPEM string) (csr *x509.CertificateRequest, der []byte, pub ed25519.PublicKey, ok bool) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, nil, nil, false
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, nil, nil, false
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, nil, nil, false
	}
	pub, ok = csr.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, nil, nil, false
	}
	return csr, block.Bytes, pub, true
}

// writeReEnrollError answers a re-enrollment refusal with the relay/1 `error`
// frame (reenroll.ErrorFrame) — carrying the request id, the response trace_id,
// and the taxonomy code+message — at the given HTTP status. This is the
// message-level error frame the contract's RenewResponse-rejected shape
// defines, distinct from the transport's RFC-9457 problem+json.
func (s *Server) writeReEnrollError(w http.ResponseWriter, r *http.Request, id string, status int, code, message string) {
	writeJSON(w, status, reenroll.ErrorFrame{
		Type:    "error",
		ID:      id,
		TraceID: apihttp.TraceID(r),
		Code:    code,
		Message: message,
	})
}
