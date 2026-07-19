package hello

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// authoritativeSite is the app peer's own current record of this relay's
// site binding (REL-036) — a real IANA zone so a relay adopting it can feed
// it straight into rules/1's engine.SetLocation.
var authoritativeSite = SiteBinding{
	ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
	TZ:        "America/Chicago",
	Lat:       41.8781,
	Long:      -87.6298,
}

// newRelayKeypair mints a relay enrollment keypair for a handshake test.
func newRelayKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate relay keypair: %v", err)
	}
	return pub, priv
}

// serveAppPeer stands the app-peer hello server up over TLS (like the live
// feeder's HTTPS listener) and returns its base URL.
func serveAppPeer(t *testing.T, s *AppPeerServer) string {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestPerformHelloInProcess drives the whole REL-030–039 handshake in
// process: the app peer issues a challenge, the relay signs its nonce with
// its enrollment key and sends hello, and the app peer's hello-ack negotiates
// the version (REL-033/034), grants the shared feature subset dropping the
// unrecognized flag silently (REL-035), and returns its own authoritative
// site_binding (REL-036) — the exact expectations the REL-030-valid corpus
// pins.
func TestPerformHelloInProcess(t *testing.T) {
	pub, priv := newRelayKeypair(t)
	const relayID = "01J8Z4K4N5P6Q7R8S9T0V1W3A1"

	s := NewAppPeerServer(
		func(id string) (ed25519.PublicKey, bool) {
			if id == relayID {
				return pub, true
			}
			return nil, false
		},
		authoritativeSite,
		AppPeerImplementedMinors(1, 1), // {"1.1","1.0"}
		[]string{"telemetry.latest_only_v1"},
		nil,
	)
	base := serveAppPeer(t, s)

	ack, err := PerformHello(base, priv, relayID, Declaration{
		ProtocolVersion: "1.0",
		Features:        []string{"telemetry.latest_only_v1", "an-unrecognized-future-flag"},
		SiteBinding:     SiteBinding{}, // relay has no cached site pre-pull; it adopts the app peer's
		SubnetMetadata:  SubnetMetadata{AdvertisedAddress: "192.0.2.12"},
		ClockState:      ClockState{State: "trusted", Source: "ntp"},
	})
	if err != nil {
		t.Fatalf("PerformHello: %v", err)
	}

	if ack.Type != "hello-ack" {
		t.Errorf("hello-ack type = %q, want %q", ack.Type, "hello-ack")
	}
	if ack.RelayID != relayID {
		t.Errorf("hello-ack relay_id = %q, want %q", ack.RelayID, relayID)
	}
	if ack.Body.NegotiatedVersion != "1.0" {
		t.Errorf("negotiated_version = %q, want %q", ack.Body.NegotiatedVersion, "1.0")
	}
	if len(ack.Body.Features) != 1 || ack.Body.Features[0] != "telemetry.latest_only_v1" {
		t.Errorf("features = %v, want [telemetry.latest_only_v1] (unrecognized flag dropped silently)", ack.Body.Features)
	}
	if ack.Body.SiteBinding != authoritativeSite {
		t.Errorf("site_binding = %+v, want the app peer's authoritative %+v", ack.Body.SiteBinding, authoritativeSite)
	}
	if ack.Body.Deprecated == nil {
		t.Errorf("deprecated must marshal as {} (non-nil), got nil")
	}

	// REL-036: the adopted site is fed straight into rules/1's engine location;
	// prove it is a real IANA zone engine.SetLocation can load.
	if _, err := time.LoadLocation(ack.Body.SiteBinding.TZ); err != nil {
		t.Errorf("adopted site tz %q is not engine-loadable: %v", ack.Body.SiteBinding.TZ, err)
	}
}

// TestPerformHelloRefusedBadChannelBinding proves REL-032's mandatory refusal:
// a hello whose channel_binding_signature does not verify against the relay's
// enrollment-learned public key is refused CHANNEL_BINDING_INVALID even though
// the TLS handshake itself succeeded — here the app peer has learned a
// DIFFERENT public key for this relay_id than the one that signed, exactly the
// impersonation case transport auth alone cannot catch.
func TestPerformHelloRefusedBadChannelBinding(t *testing.T) {
	_, priv := newRelayKeypair(t)
	otherPub, _ := newRelayKeypair(t) // the app peer's record disagrees with priv
	const relayID = "01J8Z4K4N5P6Q7R8S9T0V1W3A1"

	s := NewAppPeerServer(
		func(id string) (ed25519.PublicKey, bool) { return otherPub, true },
		authoritativeSite,
		AppPeerImplementedMinors(1, 1),
		[]string{"telemetry.latest_only_v1"},
		nil,
	)
	base := serveAppPeer(t, s)

	_, err := PerformHello(base, priv, relayID, Declaration{
		ProtocolVersion: "1.0",
		Features:        []string{"telemetry.latest_only_v1"},
		SubnetMetadata:  SubnetMetadata{AdvertisedAddress: "192.0.2.12"},
		ClockState:      ClockState{State: "trusted", Source: "ntp"},
	})
	if err == nil {
		t.Fatalf("PerformHello accepted a hello the app peer could not channel-bind")
	}
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v (%T), want a *RefusedError", err, err)
	}
	if refused.Code != "CHANNEL_BINDING_INVALID" {
		t.Errorf("refusal code = %q, want CHANNEL_BINDING_INVALID", refused.Code)
	}
}

// TestPerformHelloRefusedMajorMismatch proves REL-033: a relay declaring a
// major the app peer implements no minor of is refused PROTOCOL_VERSION_UNSUPPORTED
// (channel binding still checked first, so this exercises the negotiation refusal
// on a genuinely bound hello).
func TestPerformHelloRefusedMajorMismatch(t *testing.T) {
	pub, priv := newRelayKeypair(t)
	const relayID = "01J8Z4K4N5P6Q7R8S9T0V1W3A1"

	s := NewAppPeerServer(
		func(id string) (ed25519.PublicKey, bool) { return pub, true },
		authoritativeSite,
		AppPeerImplementedMinors(1, 1), // only major 1
		[]string{"telemetry.latest_only_v1"},
		nil,
	)
	base := serveAppPeer(t, s)

	_, err := PerformHello(base, priv, relayID, Declaration{
		ProtocolVersion: "2.0",
		Features:        []string{"telemetry.latest_only_v1"},
		SubnetMetadata:  SubnetMetadata{AdvertisedAddress: "192.0.2.12"},
		ClockState:      ClockState{State: "trusted", Source: "ntp"},
	})
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v (%T), want a *RefusedError", err, err)
	}
	if refused.Code != "PROTOCOL_VERSION_UNSUPPORTED" {
		t.Errorf("refusal code = %q, want PROTOCOL_VERSION_UNSUPPORTED", refused.Code)
	}
}

// TestAppPeerServerNonceIsFreshAndSingleUse proves REL-030's nonce discipline
// as the app peer enforces it: each challenge carries a fresh 128-bit nonce,
// and a nonce is consumed by the hello it binds — a second hello with no fresh
// challenge outstanding cannot bind.
func TestAppPeerServerNonceIsFreshAndSingleUse(t *testing.T) {
	pub, priv := newRelayKeypair(t)
	const relayID = "01J8Z4K4N5P6Q7R8S9T0V1W3A1"

	s := NewAppPeerServer(
		func(id string) (ed25519.PublicKey, bool) { return pub, true },
		authoritativeSite,
		AppPeerImplementedMinors(1, 1),
		[]string{"telemetry.latest_only_v1"},
		nil,
	)
	base := serveAppPeer(t, s)
	client := loopbackClient()

	// Two challenges mint two distinct nonces.
	n1 := fetchChallengeNonce(t, client, base)
	n2 := fetchChallengeNonce(t, client, base)
	if n1 == n2 {
		t.Fatalf("two challenges returned the same nonce %q — not single-use", n1)
	}
	if len(n1) < 32 { // 128 bits hex-encoded
		t.Fatalf("nonce %q shorter than 128 bits", n1)
	}

	// Bind + consume the currently-outstanding nonce (n2).
	sig, err := SignChannelBinding(priv, n2)
	if err != nil {
		t.Fatalf("SignChannelBinding: %v", err)
	}
	h := Hello{Type: "hello", RelayID: relayID, Body: HelloBody{
		ProtocolVersion:         "1.0",
		Features:                []string{"telemetry.latest_only_v1"},
		ChannelBindingSignature: sig,
	}}
	if _, err := postHello(client, base, h); err != nil {
		t.Fatalf("first hello over the outstanding nonce refused: %v", err)
	}
	// A replay of the same signed hello — the nonce is now consumed — must not bind.
	if _, err := postHello(client, base, h); err == nil {
		t.Fatalf("second hello re-bound a consumed nonce (REL-030 single-use violated)")
	}
}

// --- test-local wire helpers (exercise the app peer's HTTP surface directly) ---

func fetchChallengeNonce(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	resp, err := client.Get(base + "/hello/challenge")
	if err != nil {
		t.Fatalf("GET /hello/challenge: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /hello/challenge: status %d", resp.StatusCode)
	}
	var c Challenge
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	return c.Body.Nonce
}

func postHello(client *http.Client, base string, h Hello) (HelloAck, error) {
	body, _ := json.Marshal(h)
	resp, err := client.Post(base+"/hello", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return HelloAck{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return HelloAck{}, errors.New("hello refused")
	}
	var ack HelloAck
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return HelloAck{}, err
	}
	return ack, nil
}

func loopbackClient() *http.Client {
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // loopback test
	}
}
