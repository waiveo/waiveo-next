package player1

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/maaxton/waiveo-next/internal/feeder/enroll"
	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	feedersigning "github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// quietErrorLog silences net/http.Server's per-connection error logging for
// these in-process servers — the MITM case (PLY-057) deliberately aborts the
// TLS handshake at the pinning check, which http.Server would otherwise log
// as a confusing "bad certificate" line next to a PASSing driver.
var quietErrorLog = log.New(io.Discard, "", 0)

// InProcessRelay is a live, in-process feeder+relay stack that implements
// Relay: a real feeder signs one desired-state generation, a real relay
// enrolls against it, pulls+verifies it, and serves player/1's pairing +
// program surface over a real loopback TLS listener — mirroring
// internal/virtualplayer's own TestPhoton boot, minus the *testing.T
// coupling so the driver, the green test, and the teeth meta-test can all
// reuse it. The /player/v1/pair route is wrapped with a recorder so the
// differential oracle can observe the wire (request + response bodies, call
// count) it cannot MITM.
type InProcessRelay struct {
	baseURL  string
	host     string
	port     int
	certDER  []byte
	rec      *pairRecorder
	closeFns []func()

	// grants is the pool of fresh, single-use ("one-time") pairing grants the
	// feeder minted into the applied desired state; grantMu/grantCursor hand
	// out a distinct one per formed code, so each driven case redeems its own
	// grant rather than exhausting a shared one.
	grantMu     sync.Mutex
	grants      []wire.PairingGrant
	grantCursor int

	closeOnce sync.Once
}

// grantPoolSize is how many single-use grants the harness feeder mints —
// enough for every driven player/1 case (plus the teeth run) to redeem a
// fresh one, with headroom.
const grantPoolSize = 8

// nextGrant hands out the next unused grant from the pool.
func (r *InProcessRelay) nextGrant() (wire.PairingGrant, error) {
	r.grantMu.Lock()
	defer r.grantMu.Unlock()
	if r.grantCursor >= len(r.grants) {
		return wire.PairingGrant{}, fmt.Errorf("player1: grant pool exhausted (%d grants) — mint more in bootFeeder", len(r.grants))
	}
	g := r.grants[r.grantCursor]
	r.grantCursor++
	return g, nil
}

// pairRecorder captures every /player/v1/pair request and response body plus
// a call count, resettable between cases.
type pairRecorder struct {
	mu    sync.Mutex
	count int
	reqs  [][]byte
	resps [][]byte
}

func (p *pairRecorder) record(req, resp []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
	p.reqs = append(p.reqs, req)
	p.resps = append(p.resps, resp)
}

func (p *pairRecorder) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count = 0
	p.reqs = nil
	p.resps = nil
}

func (p *pairRecorder) snapshot() (int, [][]byte, [][]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count, append([][]byte(nil), p.reqs...), append([][]byte(nil), p.resps...)
}

// NewInProcessRelay boots the feeder+relay stack and returns a ready Relay.
// The caller MUST Close it.
func NewInProcessRelay() (*InProcessRelay, error) {
	r := &InProcessRelay{rec: &pairRecorder{}}

	feederBaseURL, cleanupFeeder, err := bootFeeder()
	if err != nil {
		return nil, fmt.Errorf("player1: boot feeder: %w", err)
	}
	r.closeFns = append(r.closeFns, cleanupFeeder)

	if err := r.bootRelay(feederBaseURL); err != nil {
		r.Close()
		return nil, fmt.Errorf("player1: boot relay: %w", err)
	}
	return r, nil
}

// bootFeeder boots an in-process feeder over a real loopback TLS listener —
// the content origin a player later fetches from DIRECT (PLY-084), so it
// needs a concrete dialable address, not an httptest fake transport.
func bootFeeder() (baseURL string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "player1-driver-feeder-*")
	if err != nil {
		return "", nil, fmt.Errorf("os.MkdirTemp: %w", err)
	}
	id, err := feedersigning.LoadOrCreate(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("feedersigning.LoadOrCreate: %w", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("net.Listen: %w", err)
	}
	baseURL = "https://" + lis.Addr().String()

	img := []byte("player1-conformance-driver-image-bytes")
	contentStore := origin.New()
	contentStore.Add(img)

	grants := make([]wire.PairingGrant, grantPoolSize)
	for i := range grants {
		grants[i] = grant.Mint()
	}
	snap, err := snapshot.Build(img, baseURL, id, grants)
	if err != nil {
		_ = lis.Close()
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("snapshot.Build: %w", err)
	}

	enrollSrv, err := enroll.NewServer(id, snap)
	if err != nil {
		_ = lis.Close()
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("enroll.NewServer: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/content/", contentStore.Handler())
	enrollSrv.Register(mux)

	cert, err := tls.X509KeyPair(id.TLSCertPEM(), id.TLSKeyPEM())
	if err != nil {
		_ = lis.Close()
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("tls.X509KeyPair: %w", err)
	}

	srv := &http.Server{
		Handler:   apihttp.WithTraceID(mux),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		ErrorLog:  quietErrorLog,
	}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	return baseURL, func() { _ = srv.Close(); _ = os.RemoveAll(dir) }, nil
}

// bootRelay enrolls a fresh relay against feederBaseURL, pulls+verifies its
// desired state, and serves player/1 over its own loopback TLS listener with
// the /player/v1/pair recorder wired in.
func (r *InProcessRelay) bootRelay(feederBaseURL string) error {
	store, err := identity.Open(":memory:")
	if err != nil {
		return fmt.Errorf("identity.Open: %w", err)
	}
	r.closeFns = append(r.closeFns, func() { _ = store.Close() })

	if err := relayenroll.Run(feederBaseURL, store); err != nil {
		return fmt.Errorf("relayenroll.Run: %w", err)
	}
	applied, err := desiredstate.Pull(feederBaseURL, store)
	if err != nil {
		return fmt.Errorf("desiredstate.Pull: %w", err)
	}
	if len(applied.PairingGrants) == 0 {
		return fmt.Errorf("applied desired state carried no pairing_grants")
	}
	r.grants = applied.PairingGrants

	relayID, ok, err := store.Identity()
	if err != nil {
		return fmt.Errorf("store.Identity: %w", err)
	}
	if !ok {
		return fmt.Errorf("no identity persisted after enrollment")
	}

	cert, der, err := relayTLSCertificate(relayID.CertPEM, relayID.PrivateKey)
	if err != nil {
		return fmt.Errorf("relayTLSCertificate: %w", err)
	}
	r.certDER = der

	pairingSrv, err := playerserver.NewServer(relayID.CertPEM, applied.PairingGrants)
	if err != nil {
		return fmt.Errorf("playerserver.NewServer: %w", err)
	}
	pairingSrv.SetProgram(applied.ProgramRevision, applied.Priority, applied.Display, []wire.LeaseContent{{
		Type:      "image",
		AssetRef:  applied.Image.AssetRef,
		URL:       applied.Image.URL,
		ExpiresAt: applied.Image.ExpiresAt,
	}}, relayID.PrivateKey)

	mux := http.NewServeMux()
	pairingSrv.Register(mux)

	rec := r.rec
	spy := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/player/v1/pair" {
			mux.ServeHTTP(w, req)
			return
		}
		reqBody, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
		cw := &capturingWriter{ResponseWriter: w}
		mux.ServeHTTP(cw, req)
		rec.record(reqBody, cw.body.Bytes())
	})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("net.Listen: %w", err)
	}
	srv := &http.Server{
		Handler:   apihttp.WithTraceID(spy),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		ErrorLog:  quietErrorLog,
	}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	r.closeFns = append(r.closeFns, func() { _ = srv.Close() })

	h, p, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		return fmt.Errorf("net.SplitHostPort: %w", err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return fmt.Errorf("strconv.Atoi(%q): %w", p, err)
	}
	r.host, r.port = h, port
	r.baseURL = "https://" + lis.Addr().String()
	return nil
}

// capturingWriter tees the handler's response body so the recorder can read
// it while still writing it to the real client.
type capturingWriter struct {
	http.ResponseWriter
	body bytes.Buffer
}

func (c *capturingWriter) Write(b []byte) (int, error) {
	c.body.Write(b)
	return c.ResponseWriter.Write(b)
}

// BaseURL implements Relay.
func (r *InProcessRelay) BaseURL() string { return r.baseURL }

// FormPairingCode implements Relay: a valid code over a FRESH grant,
// committing to the relay's real presented certificate.
func (r *InProcessRelay) FormPairingCode() (string, error) {
	g, err := r.nextGrant()
	if err != nil {
		return "", err
	}
	return playerserver.FormPairingCode(r.host, r.port, g, r.certDER)
}

// FormPairingCodePair implements Relay: over ONE fresh grant, returns both a
// correct code (commitment over the relay's real cert) and a mismatched code
// (commitment over a DIFFERENT, self-signed cert the relay never presents —
// the MITM / substituted-cert scenario). The two codes referencing the SAME
// grant is what lets PLY-057 prove the rejected mismatched attempt did not
// consume the grant: the correct code over the same grant must still redeem.
func (r *InProcessRelay) FormPairingCodePair() (correct, mismatched string, err error) {
	g, err := r.nextGrant()
	if err != nil {
		return "", "", err
	}
	correct, err = playerserver.FormPairingCode(r.host, r.port, g, r.certDER)
	if err != nil {
		return "", "", err
	}
	otherCertPEM, _ := tlsboot.GenSelfSigned()
	block, _ := pem.Decode(otherCertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", "", fmt.Errorf("player1: mismatched code: self-signed cert did not PEM-decode")
	}
	mismatched, err = playerserver.FormPairingCode(r.host, r.port, g, block.Bytes)
	if err != nil {
		return "", "", err
	}
	return correct, mismatched, nil
}

// PairCallCount implements Relay.
func (r *InProcessRelay) PairCallCount() int { n, _, _ := r.rec.snapshot(); return n }

// PairRequests implements Relay.
func (r *InProcessRelay) PairRequests() [][]byte { _, reqs, _ := r.rec.snapshot(); return reqs }

// PairResponses implements Relay.
func (r *InProcessRelay) PairResponses() [][]byte { _, _, resps := r.rec.snapshot(); return resps }

// Reset implements Relay.
func (r *InProcessRelay) Reset() { r.rec.reset() }

// Close tears the whole stack down.
func (r *InProcessRelay) Close() {
	r.closeOnce.Do(func() {
		for i := len(r.closeFns) - 1; i >= 0; i-- {
			r.closeFns[i]()
		}
	})
}

// relayTLSCertificate mirrors cmd/waiveo-relay's own relayTLSCertificate:
// builds a tls.Certificate from the enrolled identity and returns the leaf
// DER — the same bytes a pairing code's commitment is computed over.
func relayTLSCertificate(certPEM []byte, priv ed25519.PrivateKey) (tls.Certificate, []byte, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return tls.Certificate{}, nil, fmt.Errorf("identity cert did not PEM-decode to a CERTIFICATE block")
	}
	return cert, block.Bytes, nil
}
