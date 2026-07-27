// Package ingesttest builds a ready-to-drive relay-identity fixture for the
// in-process harnesses that push telemetry: a throwaway certificate authority
// standing in for the feeder's enrollment CA, one leaf certificate issued under
// it in exactly the shape internal/feeder/enroll issues (subject common name =
// the relay id, a random 128-bit serial, client+server EKU), and the matching
// eventingest.RelayAuthorizer.
//
// It exists for the same reason internal/app/auth/authtest does. Requiring a
// verified relay client certificate on POST /telemetry/v1/push made every
// in-process harness — the events/1 conformance driver, the ingest's own tests,
// the observability end-to-end — need a genuine relay identity to push as.
// Handing them a bypass switch instead ("skip the certificate check in tests")
// would have made every one of those tests prove something other than what
// ships. Minting a real CA and a real leaf is the honest fix: the harnesses
// exercise the same identity extraction, the same authorization decision, and —
// through ServerTLSConfig/ClientTLSConfig — the same TLS chain verification a
// deployed feeder performs.
//
// Two presentation forms are offered, and both are real:
//
//   - ConnectionState stamps a *http.Request.TLS for a handler driven directly
//     (httptest.NewRequest), carrying the same PeerCertificates + VerifiedChains
//     a verifying listener would have populated.
//   - ServerTLSConfig / ClientTLSConfig wire an actual TLS httptest server and
//     client, so a test can prove the LISTENER configuration produces what the
//     handler requires, rather than asserting it into existence.
package ingesttest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net/http"
	"time"
)

// Relay is one minted relay identity plus the trust material to present and
// verify it.
type Relay struct {
	// RelayID is the leaf's subject common name — the identity the ingest reads
	// off the certificate (relay/1 REL-003/041).
	RelayID string
	// Serial is the leaf's SerialNumber rendered exactly as the enrollment
	// issuance record keys it (big.Int.Text(16)), so an authorizer keyed by
	// (relay id, serial) compares equal strings.
	Serial string

	caCert   *x509.Certificate
	leafCert *x509.Certificate
	leafTLS  tls.Certificate
}

// NewRelay mints a throwaway CA and one leaf certificate for relayID.
func NewRelay(relayID string) (*Relay, error) {
	caPub, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ingesttest: generate CA key: %w", err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ingesttest-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPub, caKey)
	if err != nil {
		return nil, fmt.Errorf("ingesttest: create CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("ingesttest: parse CA certificate: %w", err)
	}

	leafPub, leafKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ingesttest: generate leaf key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("ingesttest: generate leaf serial: %w", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: relayID},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// The same two EKUs internal/feeder/enroll issues: the relay presents
		// this one leaf as a TLS client to the feeder and as the player/1 TLS
		// server to screens.
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, leafPub, caKey)
	if err != nil {
		return nil, fmt.Errorf("ingesttest: create leaf certificate: %w", err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return nil, fmt.Errorf("ingesttest: parse leaf certificate: %w", err)
	}

	return &Relay{
		RelayID:  relayID,
		Serial:   serial.Text(16),
		caCert:   caCert,
		leafCert: leafCert,
		leafTLS:  tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey, Leaf: leafCert},
	}, nil
}

// Authorizer returns the eventingest.RelayAuthorizer that admits exactly this
// relay identity and serial — the fixture's stand-in for "this feeder enrolled
// that relay and has not revoked that serial" (relay/1 REL-016/041). Its
// signature is stated structurally (a func value) rather than by importing
// eventingest, so this package stays a leaf of the dependency graph.
func (r *Relay) Authorizer() func(relayID, serial string) bool {
	return func(relayID, serial string) bool {
		return relayID == r.RelayID && serial == r.Serial
	}
}

// ConnectionState is the *tls.ConnectionState a verifying listener would have
// populated for a request presenting this relay's certificate: the leaf in
// PeerCertificates and the verified leaf→CA chain in VerifiedChains. Stamp it
// onto a request built with httptest.NewRequest.
func (r *Relay) ConnectionState() *tls.ConnectionState {
	return &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{r.leafCert},
		VerifiedChains:   [][]*x509.Certificate{{r.leafCert, r.caCert}},
	}
}

// Present stamps req with this relay's connection state.
func (r *Relay) Present(req *http.Request) { req.TLS = r.ConnectionState() }

// CAPool is the trust pool a listener verifies this relay's client certificate
// against — the fixture's stand-in for enroll.Server.ClientCAPool.
func (r *Relay) CAPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(r.caCert)
	return pool
}

// ServerTLSConfig returns the client-certificate half of a listener's TLS
// config, mirroring the feeder's own listener: verify a client certificate when
// one is offered, against the enrollment CA. It sets no server Certificates —
// httptest.NewTLSServer supplies its own — so a caller applies this to the
// server's existing config rather than replacing it.
func (r *Relay) ServerTLSConfig(base *tls.Config) *tls.Config {
	if base == nil {
		base = &tls.Config{MinVersion: tls.VersionTLS13}
	}
	base.ClientAuth = tls.VerifyClientCertIfGiven
	base.ClientCAs = r.CAPool()
	return base
}

// ClientTLSConfig makes an http.Client's transport present this relay's leaf as
// its client certificate. serverCAs is the pool the client verifies the
// listener's own certificate with (httptest.Server.Certificate, for an
// in-process server).
func (r *Relay) ClientTLSConfig(serverCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{r.leafTLS},
		RootCAs:      serverCAs,
		MinVersion:   tls.VersionTLS13,
	}
}
