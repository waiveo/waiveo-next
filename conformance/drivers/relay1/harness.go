package relay1

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
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
	feedersigning "github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

var quietErrorLog = log.New(io.Discard, "", 0)

// InProcessFeeder is a live, in-process feeder that implements Feeder: it runs
// the real relay/1 enrollment server (internal/feeder/enroll) over one TLS
// listener and a second, driver-controlled /state/pull listener that serves a
// swappable snapshot the driver crafts per case. Because the same
// feeder signing identity backs both, snapshots it signs verify against the
// exact desired_state_verification_key a relay enrolled here learned — so the
// driver can stage a VALID reapply (REL-070) as well as an impostor-signed
// rejection (REL-071).
type InProcessFeeder struct {
	id           *feedersigning.Identity
	enrollBase   string
	stateBase    string
	baseSections wire.Sections
	baseHash     string

	mu      sync.Mutex
	current wire.StateSnapshotBody // what /state/pull currently serves

	closeFns  []func()
	closeOnce sync.Once
}

// NewInProcessFeeder boots the enrollment server and the state-pull listener.
// The caller MUST Close it.
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

	// Enrollment server (the fixed snapshot it serves at its own /state/pull
	// is unused by this driver — desired-state cases pull from f.stateBase).
	enrollSrv, err := enroll.NewServer(id, base)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("relay1: enroll.NewServer: %w", err)
	}
	enrollMux := http.NewServeMux()
	enrollSrv.Register(enrollMux)
	if f.enrollBase, err = f.serve(apihttp.WithTraceID(enrollMux)); err != nil {
		f.Close()
		return nil, fmt.Errorf("relay1: serve enroll: %w", err)
	}

	// Driver-controlled /state/pull serving f.current.
	stateMux := http.NewServeMux()
	stateMux.HandleFunc("/state/pull", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		snap := f.current
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snap)
	})
	if f.stateBase, err = f.serve(apihttp.WithTraceID(stateMux)); err != nil {
		f.Close()
		return nil, fmt.Errorf("relay1: serve state: %w", err)
	}

	return f, nil
}

// serve starts an HTTPS server (feeder TLS identity) on a fresh loopback
// listener and returns its base URL.
func (f *InProcessFeeder) serve(h http.Handler) (string, error) {
	cert, err := tls.X509KeyPair(f.id.TLSCertPEM(), f.id.TLSKeyPEM())
	if err != nil {
		return "", fmt.Errorf("tls.X509KeyPair: %w", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("net.Listen: %w", err)
	}
	srv := &http.Server{
		Handler:   h,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		ErrorLog:  quietErrorLog,
	}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	f.closeFns = append(f.closeFns, func() { _ = srv.Close() })
	return "https://" + lis.Addr().String(), nil
}

// EnrollBaseURL implements Feeder.
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

// SignedSnapshotURL implements Feeder: serve a snapshot at generation validly
// signed by the feeder's own signing key.
func (f *InProcessFeeder) SignedSnapshotURL(generation int64) (string, error) {
	sig, err := signScope(f.id.SigningPriv(), generation, f.baseHash)
	if err != nil {
		return "", err
	}
	f.setCurrent(wire.StateSnapshotBody{
		Generation: generation,
		Hash:       f.baseHash,
		Signature:  sig,
		Sections:   f.baseSections,
	})
	return f.stateBase, nil
}

// WrongKeySnapshotURL implements Feeder: serve a snapshot at generation signed
// by a freshly-generated FOREIGN key — never the feeder's own — so a correct
// relay rejects it (REL-071).
func (f *InProcessFeeder) WrongKeySnapshotURL(generation int64) (string, error) {
	_, foreignPriv := signhash.GenerateKey()
	sig, err := signScope(foreignPriv, generation, f.baseHash)
	if err != nil {
		return "", err
	}
	f.setCurrent(wire.StateSnapshotBody{
		Generation:    generation,
		Hash:          f.baseHash,
		Signature:     sig,
		SignedWithKey: "ed25519:impostor",
		Sections:      f.baseSections,
	})
	return f.stateBase, nil
}

func (f *InProcessFeeder) setCurrent(b wire.StateSnapshotBody) {
	f.mu.Lock()
	f.current = b
	f.mu.Unlock()
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
