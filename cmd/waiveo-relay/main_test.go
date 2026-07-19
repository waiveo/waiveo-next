package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/clocktrust"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
)

// TestRegisterClockHintReceivesWireHint proves the relay/1 clock.hint receiver
// (REL-133) is wired into the relay's own listener and reachable end-to-end: a
// POSTed clock.hint bounded by the relay's own cert not_after is applied to the
// runtime clock, and one past not_after+grace is declined.
func TestRegisterClockHintReceivesWireHint(t *testing.T) {
	notAfter := time.UnixMilli(1784073600000)
	certDER := selfSignedCertDER(t, notAfter)

	mux := http.NewServeMux()
	store, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctl := clocktrust.NewController(store, clocktrust.NewRuntimeClock(), nil)
	if err := registerClockHint(mux, certDER, ctl); err != nil {
		t.Fatalf("registerClockHint: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(wire string) (accepted bool, status int) {
		resp, err := http.Post(srv.URL+"/relay/v1/clock-hint", "application/json", bytes.NewBufferString(wire))
		if err != nil {
			t.Fatalf("POST clock.hint: %v", err)
		}
		defer resp.Body.Close()
		var body struct {
			Accepted bool `json:"accepted"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return body.Accepted, resp.StatusCode
	}

	// Within not_after + grace -> accepted.
	if accepted, status := post(`{"type":"clock.hint","relay_id":"01J8Z4K4N5P6Q7R8S9T0V1W3A1","body":{"ts":1752537600000}}`); !accepted || status != http.StatusOK {
		t.Errorf("in-bound clock.hint: accepted=%v status=%d, want accepted=true status=200", accepted, status)
	}
	// Past not_after + grace -> declined (still 200).
	if accepted, status := post(`{"type":"clock.hint","relay_id":"01J8Z4K4N5P6Q7R8S9T0V1W3A1","body":{"ts":1784074200001}}`); accepted || status != http.StatusOK {
		t.Errorf("out-of-bound clock.hint: accepted=%v status=%d, want accepted=false status=200", accepted, status)
	}
}

func selfSignedCertDER(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "relay"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return der
}

// TestOfflineServeFallback pins the REL-055/061 boot policy the relay applies
// when the app peer is unreachable at startup: a hello or Pull failure is
// survivable (degrade to serving the persisted last-applied snapshot offline)
// IFF a prior successful pull already persisted a last-applied generation;
// otherwise it stays fatal because there is nothing to serve. This is the
// regression guard for the defect where a boot-time app-peer failure downed the
// whole process even though a valid persisted {generation, hash,
// screen_programs} sat in the store — a restart during a disconnection that
// REL-055 exists to keep serving.
func TestOfflineServeFallback(t *testing.T) {
	bootErr := errors.New("app peer unreachable")

	// Persisted snapshot present: a boot-time failure must NOT be fatal — the
	// relay serves the persisted last-applied copy offline.
	if fatal := offlineServeFallback(bootErr, true); fatal != nil {
		t.Errorf("offlineServeFallback(err, hasPersisted=true) = %v, want nil (serve persisted offline, REL-055/061)", fatal)
	}

	// Nothing persisted: the same failure IS fatal — no program to serve.
	if fatal := offlineServeFallback(bootErr, false); fatal == nil {
		t.Error("offlineServeFallback(err, hasPersisted=false) = nil, want the original error (nothing to serve)")
	} else if !errors.Is(fatal, bootErr) {
		t.Errorf("offlineServeFallback returned %v, want the original bootErr unchanged", fatal)
	}

	// A nil bootErr is never fatal regardless of persisted state.
	if fatal := offlineServeFallback(nil, false); fatal != nil {
		t.Errorf("offlineServeFallback(nil, false) = %v, want nil", fatal)
	}
	if fatal := offlineServeFallback(nil, true); fatal != nil {
		t.Errorf("offlineServeFallback(nil, true) = %v, want nil", fatal)
	}
}

func TestLoadConfigDefaultsAreLoopback(t *testing.T) {
	cfg, err := loadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("loadConfig(defaults): %v", err)
	}
	if cfg.listen != "127.0.0.1:7421" {
		t.Errorf("default listen = %q, want 127.0.0.1:7421", cfg.listen)
	}
	if cfg.feederURL != "https://127.0.0.1:7420" {
		t.Errorf("default feederURL = %q, want https://127.0.0.1:7420", cfg.feederURL)
	}
	if cfg.pairHost != "127.0.0.1" {
		t.Errorf("default pairHost = %q, want 127.0.0.1", cfg.pairHost)
	}
	if cfg.pairPort != 7421 {
		t.Errorf("default pairPort = %d, want 7421", cfg.pairPort)
	}
}

func TestLoadConfigOnBoxOverride(t *testing.T) {
	// The on-box first-photon shape: bind LAN-reachable, tell the screen to dial
	// the box's LAN IP, keep the feeder loopback (co-located).
	env := map[string]string{
		"WAIVEO_RELAY_LISTEN":    "0.0.0.0:7421",
		"WAIVEO_RELAY_PAIR_HOST": "192.0.2.12",
		"WAIVEO_RELAY_PAIR_PORT": "7421",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig(override): %v", err)
	}
	if cfg.listen != "0.0.0.0:7421" {
		t.Errorf("listen = %q, want 0.0.0.0:7421", cfg.listen)
	}
	if cfg.pairHost != "192.0.2.12" {
		t.Errorf("pairHost = %q, want the LAN IP a screen must dial", cfg.pairHost)
	}
	if cfg.feederURL != "https://127.0.0.1:7420" {
		t.Errorf("feederURL = %q, want the loopback default (feeder is co-located)", cfg.feederURL)
	}
}

func TestLoadConfigRejectsNonIntegerPairPort(t *testing.T) {
	// A bad port must fail fast at startup, not silently emit a pairing code no
	// screen can dial.
	env := map[string]string{"WAIVEO_RELAY_PAIR_PORT": "not-a-port"}
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("loadConfig accepted a non-integer WAIVEO_RELAY_PAIR_PORT, want error")
	}
}
