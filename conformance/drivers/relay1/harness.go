package relay1

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/feeder/enroll"
	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	feedersigning "github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// helloSite is the app peer's authoritative site_binding this in-process
// feeder reports in every hello-ack (REL-036) — the same first-photon site
// cmd/waiveo-feeder wires in production, and the REL-030 corpus case's own
// expected hello_ack.body.site_binding.
var helloSite = hello.SiteBinding{
	ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
	TZ:        "America/Chicago",
	Lat:       41.8781,
	Long:      -87.6298,
}

// helloRecognizedFeatures are the relay/1 capability flags this in-process
// feeder understands, for the hello-ack shared-feature subset (REL-035) —
// matching cmd/waiveo-feeder's firstPhotonRecognizedFeatures so the REL-030
// corpus's "an-unrecognized-future-flag" is dropped silently rather than
// accidentally recognized.
var helloRecognizedFeatures = []string{"telemetry.latest_only_v1"}

var quietErrorLog = log.New(io.Discard, "", 0)

// InProcessFeeder is a live, in-process feeder that implements Feeder: the
// real relay/1 enrollment server (internal/feeder/enroll) and the real
// /relay/v1 persistent-connection server (internal/feeder/relayconn) on ONE
// mTLS TLS listener — exactly how cmd/waiveo-feeder wires them — serving a
// swappable snapshot the driver stages per case. Because the same feeder
// signing identity backs both, snapshots it signs verify against the exact
// desired_state_verification_key a relay enrolled here learned — so the
// driver can stage a VALID reapply (REL-070) as well as an impostor-signed
// rejection (REL-071).
type InProcessFeeder struct {
	id           *feedersigning.Identity
	enrollBase   string
	connSrv      *feederrelayconn.Server
	baseSections wire.Sections
	baseHash     string

	mu      sync.Mutex
	current wire.StateSnapshotBody // what state.pull currently answers with

	closeFns  []func()
	closeOnce sync.Once
}

// NewInProcessFeeder boots the enrollment + persistent-connection servers on
// one mTLS listener. The caller MUST Close it.
func NewInProcessFeeder() (*InProcessFeeder, error) {
	dir, err := os.MkdirTemp("", "relay1-driver-feeder-*")
	if err != nil {
		return nil, fmt.Errorf("relay1: os.MkdirTemp: %w", err)
	}
	f := &InProcessFeeder{}
	f.closeFns = append(f.closeFns, func() { _ = os.RemoveAll(dir) })

	id, err := feedersigning.LoadOrCreate(dir)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("relay1: feedersigning.LoadOrCreate: %w", err)
	}
	f.id = id

	// Canonical sections + hash, reused across every generation the driver
	// stages (so gen 42 and gen 43 are byte-identical in content, REL-070).
	img := []byte("relay1-conformance-driver-image-bytes")
	base, err := snapshot.Build(img, "https://198.51.100.20:5173", id, []wire.PairingGrant{grant.Mint()})
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("relay1: snapshot.Build: %w", err)
	}
	f.baseSections = base.Sections
	f.baseHash = base.Hash
	f.current = base

	enrollSrv, err := enroll.NewServer(id)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("relay1: enroll.NewServer: %w", err)
	}
	mux := http.NewServeMux()
	enrollSrv.Register(mux)

	// The persistent-connection server (REL-001's stable /relay/v1 path),
	// mounted on the SAME mux as enrollment — exactly how cmd/waiveo-feeder
	// wires it in production, so feeder.EnrollBaseURL() doubles as the
	// connection URL. Its channel-binding verification (REL-032/041) reads
	// the relay's enrollment-learned key straight off enrollSrv, keyed by
	// the connection's mTLS client-certificate identity.
	f.connSrv = feederrelayconn.New(
		f.snapshot,
		enrollSrv.RelayEnrollmentKey,
		enrollSrv.IsRevoked,
		helloSite,
		hello.AppPeerImplementedMinors(1, 1),
		helloRecognizedFeatures,
	)
	mux.Handle("/relay/v1", f.connSrv.Handler())

	tlsCfg := &tls.Config{
		// mTLS for /relay/v1 (REL-003/041), optional so the enrollment
		// bootstrap stays certificate-free — the production feeder
		// listener's exact posture, including the TLS 1.3 floor REL-040's
		// exporter derivation requires.
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  enrollSrv.ClientCAPool(),
		MinVersion: tls.VersionTLS13,
	}
	if f.enrollBase, err = f.serve(apihttp.WithTraceID(mux), tlsCfg); err != nil {
		f.Close()
		return nil, fmt.Errorf("relay1: serve: %w", err)
	}

	f.closeFns = append(f.closeFns, f.connSrv.CloseAll)
	return f, nil
}

// serve starts an HTTPS server (feeder TLS identity, plus the caller's
// client-auth posture) on a fresh loopback listener and returns its base URL.
func (f *InProcessFeeder) serve(h http.Handler, tlsCfg *tls.Config) (string, error) {
	cert, err := tls.X509KeyPair(f.id.TLSCertPEM(), f.id.TLSKeyPEM())
	if err != nil {
		return "", fmt.Errorf("tls.X509KeyPair: %w", err)
	}
	tlsCfg.Certificates = []tls.Certificate{cert}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("net.Listen: %w", err)
	}
	srv := &http.Server{
		Handler:   h,
		TLSConfig: tlsCfg,
		ErrorLog:  quietErrorLog,
	}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	f.closeFns = append(f.closeFns, func() { _ = srv.Close() })
	return "https://" + lis.Addr().String(), nil
}

// snapshot is the connection server's SnapshotProvider: the currently-staged
// snapshot (StageSnapshot), initially the base generation-1 build.
func (f *InProcessFeeder) snapshot() (wire.StateSnapshotBody, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, nil
}

// EnrollBaseURL implements Feeder. The same listener carries /relay/v1, so
// this is also the persistent-connection URL a driven client dials.
func (f *InProcessFeeder) EnrollBaseURL() string { return f.enrollBase }

// CurrentClaimToken implements Feeder.
func (f *InProcessFeeder) CurrentClaimToken() (string, error) {
	resp, err := insecureClient().Get(f.enrollBase + "/claim-token")
	if err != nil {
		return "", fmt.Errorf("GET /claim-token: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		ClaimToken string `json:"claim_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode claim-token: %w", err)
	}
	if body.ClaimToken == "" {
		return "", fmt.Errorf("empty claim_token")
	}
	return body.ClaimToken, nil
}

// StageSnapshot implements Feeder: install the snapshot the connection
// server's next state.pull answers with — the driver's per-case staging. The
// staged snapshot reuses the canonical base sections (same content, same
// hash, across every generation — REL-070's byte-identical reapply), signed
// at generation by the feeder's own signing key, or by a freshly-generated
// FOREIGN key when foreignKey is true — never the feeder's own — so a
// correct relay rejects it (REL-071).
func (f *InProcessFeeder) StageSnapshot(generation int64, foreignKey bool) error {
	priv := f.id.SigningPriv()
	next := wire.StateSnapshotBody{
		Generation: generation,
		Hash:       f.baseHash,
		Sections:   f.baseSections,
	}
	if foreignKey {
		_, priv = signhash.GenerateKey()
		next.SignedWithKey = "ed25519:impostor"
	}
	sig, err := signScope(priv, generation, f.baseHash)
	if err != nil {
		return err
	}
	next.Signature = sig

	f.mu.Lock()
	f.current = next
	f.mu.Unlock()
	return nil
}

// LastStateAck implements Feeder: the most recent wire state.ack the
// connection server received from relayID (REL-054), and whether one has
// arrived — what lets the driver diff the WIRE acknowledgment a pull's
// apply outcome produced, not just the client's local return value.
func (f *InProcessFeeder) LastStateAck(relayID string) (wire.Frame, bool) {
	return f.connSrv.LastStateAck(relayID)
}

// AppPeerLeafSPKI implements Feeder: the DER SubjectPublicKeyInfo of the
// feeder's own TLS leaf — the exact certificate serve() presents — so a
// relay dialing EnrollBaseURL under REL-136/137 pins the real presented key.
func (f *InProcessFeeder) AppPeerLeafSPKI() ([]byte, error) {
	block, _ := pem.Decode(f.id.TLSCertPEM())
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("relay1: feeder TLS cert did not PEM-decode to a CERTIFICATE block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("relay1: parse feeder TLS leaf: %w", err)
	}
	return leaf.RawSubjectPublicKeyInfo, nil
}

// Close tears the feeder down.
func (f *InProcessFeeder) Close() {
	f.closeOnce.Do(func() {
		for i := len(f.closeFns) - 1; i >= 0; i-- {
			f.closeFns[i]()
		}
	})
}

// signScope produces REL-075's signature over (generation, hash) using the
// shared wire helpers — the exact bytes a relay-side verifier reproduces.
func signScope(priv []byte, generation int64, hash string) (string, error) {
	canon, err := wire.SignedScopeBytes(generation, hash)
	if err != nil {
		return "", fmt.Errorf("wire.SignedScopeBytes: %w", err)
	}
	return wire.EncodeSignature(signhash.Sign(priv, canon)), nil
}

// insecureClient is the bootstrap-TLS client the driver uses for its own
// direct probes against the feeder (claim-token, reuse-token refusal) —
// mirroring the relay's REL-010/011 bootstrap exception (the feeder's
// self-signed listener cert has no CA to chain-validate against yet).
func insecureClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // REL-010/011 loopback bootstrap exception — driver-side probe only.
		},
	}
}

// bytesReader is a tiny helper so driver.go can build request bodies without
// importing bytes directly.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
