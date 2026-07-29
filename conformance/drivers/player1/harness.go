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
	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	feedersigning "github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	relayclient "github.com/maaxton/waiveo-next/internal/relay/relayconn"
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

// InProcessRelayOption configures NewInProcessRelay. The zero value of every
// option preserves today's loopback-only behavior (bind and dial both
// "127.0.0.1", exactly what every existing driver test uses) — see
// WithBindHost/WithDialHost's own doc for when a genuinely remote (on-LAN)
// player device needs something else.
type InProcessRelayOption func(*inProcessRelayConfig)

// inProcessRelayConfig is InProcessRelayOption's target — unexported, so the
// zero value (both hosts empty) is only ever observed inside NewInProcessRelay
// itself, which fills in the "127.0.0.1"/bindHost defaults before use.
type inProcessRelayConfig struct {
	bindHost string
	dialHost string
}

// WithBindHost overrides the interface the in-process feeder+relay TCP
// listeners bind to (default "127.0.0.1", loopback-only). An actual on-LAN
// player device (RemoteECPPlayerTarget) cannot reach a loopback-bound
// listener, so remote-target mode binds "0.0.0.0" (every interface) instead
// — pair a wildcard/unspecified bindHost ("0.0.0.0", "::", or "" — see
// isWildcardBindHost) with WithDialHost (see its own doc), or NewInProcessRelay
// returns an error rather than silently dialing an address no player can reach.
func WithBindHost(host string) InProcessRelayOption {
	return func(c *inProcessRelayConfig) { c.bindHost = host }
}

// WithDialHost overrides the host embedded into a formed pairing code's dial
// address and the feeder's own content-origin base URL (the address a player
// actually connects to) — which may differ from bindHost, e.g. bind
// "0.0.0.0" to accept from any interface but dial the box's one specific
// LAN IP a Roku on the same subnet can reach. Defaults to bindHost when
// unset, which is correct both for the loopback-only default (bindHost IS
// dialable) and for a bindHost that is itself already a concrete dialable
// address — but NOT for a wildcard/unspecified bindHost (isWildcardBindHost),
// which NewInProcessRelay refuses outright rather than silently defaulting to
// an undialable address.
func WithDialHost(host string) InProcessRelayOption {
	return func(c *inProcessRelayConfig) { c.dialHost = host }
}

// NewInProcessRelay boots the feeder+relay stack and returns a ready Relay.
// The caller MUST Close it. With no options this is loopback-only, exactly
// as before remote-target support existed; WithBindHost/WithDialHost are
// what let RemoteECPPlayerTarget's own driver test point this same stack at
// an actual on-LAN device instead of the in-process VirtualPlayerTarget.
func NewInProcessRelay(opts ...InProcessRelayOption) (*InProcessRelay, error) {
	cfg := inProcessRelayConfig{bindHost: "127.0.0.1"}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.dialHost == "" {
		// A wildcard/unspecified bind address (WithBindHost("0.0.0.0") or
		// "::", the whole point of which is to accept connections from any
		// interface — and net.Listen treats an empty host string the exact
		// same way) is never itself a meaningful dial target: defaulting
		// dialHost to it here would silently embed that unreachable address
		// into every formed pairing code's dial address and content-origin
		// URL, which no real player can ever reach. This is specifically the
		// WithBindHost-without-WithDialHost misuse — a bindHost that is
		// ALREADY a concrete dialable address (including the loopback
		// default) is exactly the case this default-to-bindHost fallback
		// exists for, and is left unchanged below.
		if isWildcardBindHost(cfg.bindHost) {
			return nil, fmt.Errorf("player1: NewInProcessRelay: WithBindHost(%q) is a wildcard/unspecified bind address with no WithDialHost given — a formed pairing code would embed %q as the address a player dials, which is not reachable; pass WithDialHost explicitly", cfg.bindHost, cfg.bindHost)
		}
		cfg.dialHost = cfg.bindHost
	}

	r := &InProcessRelay{rec: &pairRecorder{}}

	feederBaseURL, cleanupFeeder, err := bootFeeder(cfg.bindHost, cfg.dialHost)
	if err != nil {
		return nil, fmt.Errorf("player1: boot feeder: %w", err)
	}
	r.closeFns = append(r.closeFns, cleanupFeeder)

	if err := r.bootRelay(feederBaseURL, cfg.bindHost, cfg.dialHost); err != nil {
		r.Close()
		return nil, fmt.Errorf("player1: boot relay: %w", err)
	}
	return r, nil
}

// isWildcardBindHost reports whether host is a wildcard/unspecified bind
// address — one that tells net.Listen "every interface", never one a remote
// player could ever dial. "" (net.Listen's own "all interfaces" spelling,
// e.g. WithBindHost("") or a zero-valued host string reaching WithBindHost by
// accident) and any IP net.ParseIP resolves to the unspecified address
// ("0.0.0.0", "::", and their equivalent forms, e.g. "0:0:0:0:0:0:0:0") all
// count; a hostname or a concrete IP (including loopback) does not.
func isWildcardBindHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// bootFeeder boots an in-process feeder over a real TLS listener bound to
// bindHost — the content origin a player later fetches from DIRECT
// (PLY-084), so it needs a concrete dialable address, not an httptest fake
// transport. The returned baseURL (and so every content-origin URL a formed
// Lease ever carries) is composed from dialHost, not bindHost, so a remote
// player dials an address it can actually reach even when bindHost is the
// unroutable wildcard "0.0.0.0".
func bootFeeder(bindHost, dialHost string) (baseURL string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "player1-driver-feeder-*")
	if err != nil {
		return "", nil, fmt.Errorf("os.MkdirTemp: %w", err)
	}
	id, err := feedersigning.LoadOrCreate(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("feedersigning.LoadOrCreate: %w", err)
	}

	lis, err := net.Listen("tcp", net.JoinHostPort(bindHost, "0"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("net.Listen: %w", err)
	}
	_, listenPort, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		_ = lis.Close()
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("net.SplitHostPort(%q): %w", lis.Addr().String(), err)
	}
	baseURL = "https://" + net.JoinHostPort(dialHost, listenPort)

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

	enrollSrv, err := enroll.NewServer(id)
	if err != nil {
		_ = lis.Close()
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("enroll.NewServer: %w", err)
	}

	// The /relay/v1 persistent-connection server: desired state moves only
	// over the authenticated connection now (REL-050), so this in-process
	// feeder mounts it beside enrollment, exactly as cmd/waiveo-feeder does.
	connSrv := feederrelayconn.New(
		func() (wire.StateSnapshotBody, error) { return snap, nil },
		enrollSrv.RelayEnrollmentKey,
		enrollSrv.IsRevoked,
		hello.SiteBinding{},
		hello.AppPeerImplementedMinors(1, 1),
		nil,
	)

	mux := http.NewServeMux()
	mux.Handle("/content/", contentStore.Handler())
	enrollSrv.Register(mux)
	mux.Handle("/relay/v1", connSrv.Handler())

	cert, err := tls.X509KeyPair(id.TLSCertPEM(), id.TLSKeyPEM())
	if err != nil {
		_ = lis.Close()
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("tls.X509KeyPair: %w", err)
	}

	srv := &http.Server{
		Handler: apihttp.WithTraceID(mux),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			// mTLS for /relay/v1 (REL-003/041), optional so enrollment and
			// content stay certificate-free — the production feeder
			// listener's exact posture.
			ClientAuth: tls.VerifyClientCertIfGiven,
			ClientCAs:  enrollSrv.ClientCAPool(),
			MinVersion: tls.VersionTLS13,
		},
		ErrorLog: quietErrorLog,
	}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	return baseURL, func() { _ = srv.Close(); _ = os.RemoveAll(dir) }, nil
}

// bootRelay enrolls a fresh relay against feederBaseURL, pulls+verifies its
// desired state, and serves player/1 over its own TLS listener (bound to
// bindHost) with the /player/v1/pair recorder wired in. The Relay's
// BaseURL/host/port a formed pairing code dials are composed from dialHost,
// not bindHost — see bootFeeder's own doc for why the two can differ.
func (r *InProcessRelay) bootRelay(feederBaseURL, bindHost, dialHost string) error {
	store, err := identity.Open(":memory:")
	if err != nil {
		return fmt.Errorf("identity.Open: %w", err)
	}
	r.closeFns = append(r.closeFns, func() { _ = store.Close() })

	if err := relayenroll.Run(feederBaseURL, store); err != nil {
		return fmt.Errorf("relayenroll.Run: %w", err)
	}
	// Pull the desired state over the persistent connection (dial subsumes
	// the challenge → hello → hello-ack handshake), verifying + applying
	// through the shared chain — cmd/waiveo-relay's own boot sequence.
	conn, err := relayclient.Dial(relayclient.Config{
		URL:   feederBaseURL,
		Store: store,
		Declaration: hello.Declaration{
			ProtocolVersion: "1.0",
			ClockState:      hello.ClockState{State: "untrusted", Source: "cold_boot"},
		},
	})
	if err != nil {
		return fmt.Errorf("relayclient.Dial: %w", err)
	}
	reply, err := conn.Pull("", nil)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("state.pull: %w", err)
	}
	body, rawSections, err := relayclient.SnapshotFromFrame(reply)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("relayclient.SnapshotFromFrame: %w", err)
	}
	applied, err := desiredstate.VerifyAndApply(store, body, rawSections)
	_ = conn.Close()
	if err != nil {
		return fmt.Errorf("desiredstate.VerifyAndApply: %w", err)
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

	pairingSrv, err := playerserver.NewServer(relayID.CertPEM, applied.PairingGrants, playerserver.WallClockMs)
	if err != nil {
		return fmt.Errorf("playerserver.NewServer: %w", err)
	}
	// The relay's own Lease-signing identity (PLY-090), installed before any
	// program: a driver relay whose fixture snapshot carried no screen_programs
	// entry still has to answer a paired player with data-model/1's terminal
	// default (DAT-118), and that Lease is signed like any other.
	pairingSrv.SetSigningKey(relayID.PrivateKey)
	// Served for the snapshot's OWN screen (applied.ScreenID): a relay serves a
	// program per screen identity row (REL-061), so the id here has to be the
	// one the fixture's pairing grants redeem into (snapshot.FixtureScreenID),
	// or every paired driver player would be served the terminal default.
	pairingSrv.SetProgram(applied.Generation, applied.ScreenID, applied.ProgramRevision, applied.Priority, applied.Display, []wire.LeaseContent{{
		Type:      "image",
		AssetRef:  applied.Image.AssetRef,
		URL:       applied.Image.URL,
		ExpiresAt: applied.Image.ExpiresAt,
	}})

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

	lis, err := net.Listen("tcp", net.JoinHostPort(bindHost, "0"))
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

	_, p, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		return fmt.Errorf("net.SplitHostPort: %w", err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return fmt.Errorf("strconv.Atoi(%q): %w", p, err)
	}
	r.host, r.port = dialHost, port
	r.baseURL = "https://" + net.JoinHostPort(dialHost, p)
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
