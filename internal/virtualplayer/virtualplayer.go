// Package virtualplayer implements a complete player/1 client
// (contracts/player-1.md) in Go: the "software photon" — every step a real
// screen's player/1 stack performs, from a human-typed pairing code through
// pinned pairing, program pull, lease verification, and a direct content
// fetch, ending in the exact bytes a screen would display. It exists to
// prove the whole first-photon flow end-to-end with zero hardware (Wave-1
// first-photon Task 12), and gates every hardware task in §10: a real Roku
// player is only trustworthy once this software client is green against the
// same relay/feeder wire contract.
//
// This package deliberately does NOT import internal/relay/playerserver:
// like internal/relay/enroll mirrors internal/feeder/enroll's wire shapes
// field-for-field rather than importing the feeder's server-side package,
// virtualplayer mirrors playerserver's PairingRequest/PairingResponse/
// ProgramPullRequest/LeaseResponse/LeaseAckRequest shapes here as its own
// unexported types. This is the wire consumer on the other side of that
// exact contract, and must decode exactly what the relay sends — never an
// invented shape, and never a shortcut import that would let this "player"
// silently depend on relay-internal Go types no real (e.g. BrightScript)
// player could ever share.
//
// The one deliberate exception is internal/shared/wire.Lease itself: Lease's
// signed-bytes canonicalization (wire.LeaseSignedBytes) is explicitly a
// shared, must-not-drift helper both the relay (signing) and a player
// (verifying) are required to call — see wire/lease.go's own doc — so this
// package embeds wire.Lease exactly as playerserver.LeaseResponse does,
// rather than re-declaring an equivalent struct that could silently drift on
// field order and break signature verification.
package virtualplayer

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/paircode"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// hardwareID/playerVersion/declaredContentTypes are this virtual player's
// own fixed capabilities declaration (PLY-012): a hard-coded software
// player identity, ahead of any real per-device hardware_id provisioning
// scheme (out of this task's scope).
const (
	hardwareID    = "virtualplayer-first-photon"
	playerVersion = "1.0.0-virtualplayer"
	httpTimeout   = 10 * time.Second
)

// declaredContentTypes is the content_types this virtual player declares
// support for on every request that carries capabilities (PLY-012/013/096):
// image and video, matching contracts/player-1.md's own worked examples.
var declaredContentTypes = []string{"image", "video"}

// ErrCommitmentMismatch is returned when a TLS connection's certificate
// SubjectPublicKeyInfo does not match the pairing code's own
// fingerprint_commitment (PLY-052/056/057) — the concrete MITM-detection
// failure: the relay this connection actually reached is not the one the
// out-of-band pairing code was formed for, so this client discards the
// connection and refuses to proceed to redemption.
var ErrCommitmentMismatch = errors.New("virtualplayer: TLS peer certificate's SPKI does not match the pairing code's fingerprint_commitment (PLY-057) — refusing to proceed, possible MITM")

// Photon runs the full player/1 client thread for pairingCode, in order,
// per contracts/player-1.md: decode the pairing code (PLY-024), a TLS
// bootstrap fetch with certificate verification disabled that locally pins
// every connection to pairingCode's own out-of-band fingerprint_commitment
// (PLY-040/041/052/053/054/056/057), redeem the grant into a channel token
// (PLY-030–033/038), pull the signed program lease (PLY-080/090), verify the
// lease's signature against the pinned relay certificate's own public key
// (PLY-090), acknowledge it (PLY-091), fetch the one image content item
// DIRECT from its feeder content-origin URL — never through the relay
// (PLY-084) — and verify the fetched bytes' content address against the
// lease's own asset_ref before returning them.
//
// fingerprint_commitment, once decoded from pairingCode, is used ONLY
// locally (inside this process's own TLS certificate-pinning checks,
// pinnedTLSConfig below) — it is never marshaled into any request this
// function sends the relay (PLY-054/056): the mirrored wire request types
// below (pairingRequest, programPullRequest, leaseAckRequest) carry no such
// field, so there is no code path that could put it on the wire even by
// accident.
//
// On any failure — an unparsable pairing code, a commitment mismatch, a
// redemption/program/lease-verification/content-integrity failure — Photon
// returns a nil byte slice and a non-nil, wrapped error; it never returns a
// partial or unverified result.
func Photon(pairingCode string) ([]byte, error) {
	host, port, grantSelector, commitment, err := paircode.Decode(pairingCode)
	if err != nil {
		return nil, fmt.Errorf("virtualplayer: Photon: decode pairing code: %w", err)
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	// PLY-040/041: TLS bootstrap fetch with verification disabled, pinned
	// locally to pairingCode's own OOB commitment (PLY-052/053/054). A
	// mismatch fails the handshake itself — Photon returns before any HTTP
	// request is ever attempted against this relay (PLY-056/057).
	certDER, err := bootstrapFetchAndPin(addr, commitment)
	if err != nil {
		return nil, fmt.Errorf("virtualplayer: Photon: %w", err)
	}

	relayPub, err := publicKeyFromCertDER(certDER)
	if err != nil {
		return nil, fmt.Errorf("virtualplayer: Photon: relay certificate public key: %w", err)
	}

	// Every later connection this client makes to the relay reuses the SAME
	// pinned check (steady-state pinning, PLY-090's "the same trust anchor
	// its... connection to this relay is itself pinned to") — not just the
	// initial bootstrap fetch.
	client := pinnedClient(commitment)
	base := "https://" + addr

	pairResp, err := redeem(client, base, grantSelector)
	if err != nil {
		return nil, fmt.Errorf("virtualplayer: Photon: redeem: %w", err)
	}
	// PLY-038: pin/verify (above) must have already succeeded before this
	// credential is ever used — it has, since redeem only ran after
	// bootstrapFetchAndPin returned successfully.
	if pairResp.PairingStatus != "redeemed" || pairResp.ChannelToken == "" {
		return nil, fmt.Errorf("virtualplayer: Photon: redeem: pairing_status = %q, want %q with a channel_token", pairResp.PairingStatus, "redeemed")
	}

	lease, err := pullProgram(client, base, pairResp.ChannelToken)
	if err != nil {
		return nil, fmt.Errorf("virtualplayer: Photon: program: %w", err)
	}

	if err := verifyLeaseSignature(lease, relayPub); err != nil {
		return nil, fmt.Errorf("virtualplayer: Photon: %w", err)
	}

	if err := ackLease(client, base, lease.LeaseID); err != nil {
		return nil, fmt.Errorf("virtualplayer: Photon: lease ack: %w", err)
	}

	if len(lease.Content) == 0 {
		return nil, fmt.Errorf("virtualplayer: Photon: lease carries no content items")
	}
	item := lease.Content[0]

	// PLY-084: fetched DIRECT from the feeder's own content-origin URL,
	// never through the relay.
	imageBytes, err := fetchContent(item.URL)
	if err != nil {
		return nil, fmt.Errorf("virtualplayer: Photon: fetch content: %w", err)
	}

	if got := signhash.ContentID(imageBytes); got != item.AssetRef {
		return nil, fmt.Errorf("virtualplayer: Photon: content integrity check failed: fetched bytes hash to %q, lease asset_ref is %q", got, item.AssetRef)
	}

	return imageBytes, nil
}

// pinnedTLSConfig returns a *tls.Config that disables ordinary certificate
// chain verification (there is no CA yet to chain-validate a relay's
// self-signed bootstrap certificate against, PLY-040/041) and instead
// verifies, on every single connection made with it, that the peer
// certificate's SubjectPublicKeyInfo matches commitment — the pairing
// code's own out-of-band value (PLY-052/053/054). This is the ONLY place
// commitment is ever compared against anything: always locally, inside this
// process, never sent to or derived from the connection itself (PLY-056).
func pinnedTLSConfig(commitment []byte) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // PLY-040/041/052: no CA to chain-validate against yet; VerifyConnection enforces the OOB pairing-code commitment on every connection instead.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("virtualplayer: TLS connection presented no certificate")
			}
			ok, err := tlsboot.VerifyCommitmentForCertDER(cs.PeerCertificates[0].Raw, commitment)
			if err != nil {
				return fmt.Errorf("virtualplayer: verify fingerprint_commitment: %w", err)
			}
			if !ok {
				return ErrCommitmentMismatch
			}
			return nil
		},
	}
}

// bootstrapFetchAndPin performs player/1's TLS bootstrap fetch
// (PLY-040/041): it dials addr over TLS using pinnedTLSConfig(commitment),
// which both disables ordinary chain verification and enforces the OOB
// commitment check on this very handshake. On a commitment mismatch, the
// handshake itself fails and this function returns a non-nil error without
// ever completing a connection this client could send a request over
// (PLY-056/057: discard, do NOT proceed). On success, it returns the
// fetched leaf certificate's raw DER — the SAME certificate whose SPKI just
// verified against commitment.
func bootstrapFetchAndPin(addr string, commitment []byte) ([]byte, error) {
	dialer := &net.Dialer{Timeout: httpTimeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, pinnedTLSConfig(commitment))
	if err != nil {
		return nil, fmt.Errorf("TLS bootstrap fetch: dial %s: %w", addr, err)
	}
	defer conn.Close()

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("TLS bootstrap fetch: relay presented no certificate")
	}
	return certs[0].Raw, nil
}

// publicKeyFromCertDER parses der as an X.509 certificate and returns its
// ed25519 public key — the key a Lease's signature must verify against
// (PLY-090), since every identity in this codebase (relay/feeder/lease
// signing) is ed25519.
func publicKeyFromCertDER(der []byte) (ed25519.PublicKey, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("certificate public key is %T, want ed25519.PublicKey", cert.PublicKey)
	}
	return pub, nil
}

// pinnedClient returns an *http.Client whose transport re-checks
// commitment (pinnedTLSConfig) on every connection it makes — the same
// pinning bootstrapFetchAndPin performed for the initial handshake, applied
// uniformly to every later request this client sends the relay.
func pinnedClient(commitment []byte) *http.Client {
	return &http.Client{
		Timeout:   httpTimeout,
		Transport: &http.Transport{TLSClientConfig: pinnedTLSConfig(commitment)},
	}
}

// capabilities mirrors internal/relay/playerserver.Capabilities
// (PLY-012): `{content_types, player_version}`.
type capabilities struct {
	ContentTypes  []string `json:"content_types"`
	PlayerVersion string   `json:"player_version"`
}

// pairingRequest mirrors internal/relay/playerserver.PairingRequest
// (PLY-030): `{hardware_id, grant_selector, capabilities}`. commitment
// (fingerprint_commitment) deliberately has no field here — see Photon's own
// doc (PLY-054/056).
type pairingRequest struct {
	HardwareID    string       `json:"hardware_id"`
	GrantSelector string       `json:"grant_selector,omitempty"`
	Capabilities  capabilities `json:"capabilities"`
}

// trustAnchor mirrors internal/relay/playerserver.TrustAnchor (PLY-042).
type trustAnchor struct {
	Covers []string `json:"covers"`
	PEM    string   `json:"pem"`
}

// pairingResponse mirrors internal/relay/playerserver.PairingResponse
// (PLY-032/033).
type pairingResponse struct {
	TrustAnchors  []trustAnchor `json:"trust_anchors"`
	PairingStatus string        `json:"pairing_status"`
	PollToken     string        `json:"poll_token,omitempty"`
	ChannelToken  string        `json:"channel_token,omitempty"`
	ScreenID      string        `json:"screen_id,omitempty"`
	IssuedAt      int64         `json:"issued_at,omitempty"`
	ExpiresAt     int64         `json:"expires_at,omitempty"`
}

// programPullRequest mirrors internal/relay/playerserver.ProgramPullRequest
// (PLY-080).
type programPullRequest struct {
	Capabilities capabilities `json:"capabilities"`
	Generation   string       `json:"generation,omitempty"`
}

// leaseResponse mirrors internal/relay/playerserver.LeaseResponse: the
// shared wire.Lease (PLY-090's own fields, in declaration order — see
// wire.LeaseSignedBytes' doc for why this embeds the shared type rather than
// re-declaring an equivalent struct) plus the trailing Signature field.
type leaseResponse struct {
	wire.Lease
	Signature string `json:"signature"`
}

// leaseAckRequest mirrors internal/relay/playerserver.LeaseAckRequest
// (PLY-091): `{lease_id, accepted, reason?}`.
type leaseAckRequest struct {
	LeaseID  string `json:"lease_id"`
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

// problemBody decodes just enough of api/1's RFC 9457 Problem shape
// (API-010, reused by player/1 via PLY-005) to surface a readable error from
// a non-2xx response.
type problemBody struct {
	Title string `json:"title"`
	Code  string `json:"code"`
}

// redeem performs PLY-030–033: POST /player/v1/pair with grantSelector,
// returning the decoded PairingResponse.
func redeem(client *http.Client, base, grantSelector string) (pairingResponse, error) {
	reqBody, err := json.Marshal(pairingRequest{
		HardwareID:    hardwareID,
		GrantSelector: grantSelector,
		Capabilities:  capabilities{ContentTypes: declaredContentTypes, PlayerVersion: playerVersion},
	})
	if err != nil {
		return pairingResponse{}, fmt.Errorf("marshal PairingRequest: %w", err)
	}

	resp, err := client.Post(base+"/player/v1/pair", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return pairingResponse{}, fmt.Errorf("POST /player/v1/pair: %w", err)
	}

	var out pairingResponse
	if err := decodeOrProblem(resp, &out); err != nil {
		return pairingResponse{}, fmt.Errorf("POST /player/v1/pair: %w", err)
	}
	return out, nil
}

// pullProgram performs PLY-080: GET /player/v1/program, bearer-authorized
// with channelToken, returning the decoded, still-unverified LeaseResponse.
func pullProgram(client *http.Client, base, channelToken string) (leaseResponse, error) {
	reqBody, err := json.Marshal(programPullRequest{
		Capabilities: capabilities{ContentTypes: declaredContentTypes, PlayerVersion: playerVersion},
	})
	if err != nil {
		return leaseResponse{}, fmt.Errorf("marshal ProgramPullRequest: %w", err)
	}

	req, err := http.NewRequest(http.MethodGet, base+"/player/v1/program", bytes.NewReader(reqBody))
	if err != nil {
		return leaseResponse{}, fmt.Errorf("build GET /player/v1/program request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+channelToken)

	resp, err := client.Do(req)
	if err != nil {
		return leaseResponse{}, fmt.Errorf("GET /player/v1/program: %w", err)
	}

	var out leaseResponse
	if err := decodeOrProblem(resp, &out); err != nil {
		return leaseResponse{}, fmt.Errorf("GET /player/v1/program: %w", err)
	}
	return out, nil
}

// verifyLeaseSignature performs PLY-090: recomputes lease's canonical
// signed bytes (wire.LeaseSignedBytes) and verifies its signature against
// relayPub — the pinned relay certificate's own public key, never any key
// the wire response itself could supply.
func verifyLeaseSignature(lease leaseResponse, relayPub ed25519.PublicKey) error {
	sigBytes, err := wire.DecodeSignature(lease.Signature)
	if err != nil {
		return fmt.Errorf("decode lease signature: %w", err)
	}
	canon, err := wire.LeaseSignedBytes(lease.Lease)
	if err != nil {
		return fmt.Errorf("canonicalize lease: %w", err)
	}
	if !signhash.Verify(relayPub, canon, sigBytes) {
		return errors.New("lease signature did not verify against the pinned relay certificate's public key (PLY-090)")
	}
	return nil
}

// ackLease performs PLY-091: POST /player/v1/lease/ack {lease_id,
// accepted: true}.
func ackLease(client *http.Client, base, leaseID string) error {
	reqBody, err := json.Marshal(leaseAckRequest{LeaseID: leaseID, Accepted: true})
	if err != nil {
		return fmt.Errorf("marshal LeaseAckRequest: %w", err)
	}

	resp, err := client.Post(base+"/player/v1/lease/ack", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("POST /player/v1/lease/ack: %w", err)
	}

	var out map[string]json.RawMessage
	return decodeOrProblem(resp, &out)
}

// fetchContent performs PLY-084's direct content fetch: a plain HTTPS GET
// against url (the feeder's own content-origin URL, never the relay).
// InsecureSkipVerify is acceptable here — per this task's own scope, Wave-1
// first-photon's loopback content fetch's integrity guarantee is the
// asset_ref content-address check the caller performs on the returned
// bytes, not this TLS channel.
func fetchContent(url string) ([]byte, error) {
	client := &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // PLY-084: integrity is the asset_ref content-address check, not this TLS channel — see doc above.
		},
	}

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, string(b))
	}
	return io.ReadAll(resp.Body)
}

// decodeOrProblem decodes resp's JSON body into out on a 2xx status,
// closing resp.Body either way. On a non-2xx status, it returns a
// descriptive error built from api/1's Problem shape (problemBody) when the
// body decodes as one, falling back to the raw body text otherwise.
func decodeOrProblem(resp *http.Response, out any) error {
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		var pb problemBody
		if json.Unmarshal(b, &pb) == nil && pb.Code != "" {
			return fmt.Errorf("status %d: %s: %s", resp.StatusCode, pb.Code, pb.Title)
		}
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(b))
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response body: %w", err)
	}
	return nil
}
