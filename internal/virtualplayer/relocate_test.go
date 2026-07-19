package virtualplayer_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"crypto/tls"

	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
	"github.com/maaxton/waiveo-next/internal/virtualplayer"
)

// ply130Case mirrors exactly the fields conformance/corpora/player-1/
// PLY-130-valid-server-moved-relocate-never-wipe.json declares — the frozen
// oracle for the reconnect/relocate never-wipe state machine
// (PLY-022/026/130/131/132/133). The test parses that file directly rather
// than hard-coding its values, so a corpus edit either keeps this test honest
// or breaks it loudly.
type ply130Case struct {
	Input struct {
		PlayerPersistedState struct {
			RelayAddress struct {
				Host string `json:"host"`
				Port int    `json:"port"`
			} `json:"relay_address"`
			TrustAnchorPresent  bool `json:"trust_anchor_present"`
			ChannelTokenPresent bool `json:"channel_token_present"`
		} `json:"player_persisted_state"`
		ConnectionAttempts []struct {
			Attempt int `json:"attempt"`
			Address struct {
				Host string `json:"host"`
				Port int    `json:"port"`
			} `json:"address"`
			Result               string `json:"result"`
			ReDiscoveryTriggered bool   `json:"re_discovery_triggered"`
			DiscoveryResponse    *struct {
				ST       string `json:"st"`
				Location string `json:"location"`
			} `json:"discovery_response"`
		} `json:"connection_attempts"`
	} `json:"input"`
	Expected struct {
		FailureClassificationAttempts1To4 string `json:"failure_classification_attempts_1_to_4"`
		BackoffCapped                     bool   `json:"backoff_capped"`
		ReDiscoveryFiredOnAttempt         int    `json:"re_discovery_fired_on_attempt"`
		RelayAddressUpdatedTo             struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		} `json:"relay_address_updated_to"`
		TrustAnchorCleared  bool   `json:"trust_anchor_cleared"`
		ChannelTokenCleared bool   `json:"channel_token_cleared"`
		RetriesIndefinitely bool   `json:"retries_indefinitely"`
		ReconnectResult     string `json:"reconnect_result"`
	} `json:"expected"`
}

// loadPLY130 reads the frozen PLY-130 corpus case relative to THIS source file
// (so the test finds it regardless of the working directory).
func loadPLY130(t *testing.T) ply130Case {
	t.Helper()
	_, self, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(self), "..", "..", "conformance", "corpora", "player-1", "PLY-130-valid-server-moved-relocate-never-wipe.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read PLY-130 corpus case: %v", err)
	}
	var c ply130Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("unmarshal PLY-130 corpus case: %v", err)
	}
	return c
}

// TestRelocateDecisionPLY130 drives virtualplayer.RelocateDecision against the
// frozen PLY-130 oracle: a paired player whose relay moved address sees its
// old-address connections fail (classified unreachable), retries with capped
// backoff, fires a fresh discovery on its re-locate multiple (PLY-132), and
// relocates to the relay's new address — clearing NEITHER its trust anchor nor
// its channel token (never-wipe, PLY-026/130). Every one of the corpus's eight
// expected fields asserted.
func TestRelocateDecisionPLY130(t *testing.T) {
	c := loadPLY130(t)

	state := virtualplayer.PersistedState{
		RelayAddress: virtualplayer.RelayAddress{
			Host: c.Input.PlayerPersistedState.RelayAddress.Host,
			Port: c.Input.PlayerPersistedState.RelayAddress.Port,
		},
		TrustAnchorPresent:  c.Input.PlayerPersistedState.TrustAnchorPresent,
		ChannelTokenPresent: c.Input.PlayerPersistedState.ChannelTokenPresent,
	}

	attempts := make([]virtualplayer.ConnectionAttempt, 0, len(c.Input.ConnectionAttempts))
	for _, a := range c.Input.ConnectionAttempts {
		at := virtualplayer.ConnectionAttempt{
			Attempt:              a.Attempt,
			Address:              virtualplayer.RelayAddress{Host: a.Address.Host, Port: a.Address.Port},
			Result:               a.Result,
			ReDiscoveryTriggered: a.ReDiscoveryTriggered,
		}
		if a.DiscoveryResponse != nil {
			at.DiscoveryResponse = &virtualplayer.DiscoveryResponse{
				ST:       a.DiscoveryResponse.ST,
				Location: a.DiscoveryResponse.Location,
			}
		}
		attempts = append(attempts, at)
	}

	got := virtualplayer.RelocateDecision(state, attempts)

	if got.FailureClassificationBeforeRelocate != c.Expected.FailureClassificationAttempts1To4 {
		t.Errorf("failure_classification = %q, want %q (PLY-133)", got.FailureClassificationBeforeRelocate, c.Expected.FailureClassificationAttempts1To4)
	}
	if got.BackoffCapped != c.Expected.BackoffCapped {
		t.Errorf("backoff_capped = %v, want %v (PLY-131)", got.BackoffCapped, c.Expected.BackoffCapped)
	}
	if got.ReDiscoveryFiredOnAttempt != c.Expected.ReDiscoveryFiredOnAttempt {
		t.Errorf("re_discovery_fired_on_attempt = %d, want %d (PLY-132)", got.ReDiscoveryFiredOnAttempt, c.Expected.ReDiscoveryFiredOnAttempt)
	}
	wantAddr := virtualplayer.RelayAddress{
		Host: c.Expected.RelayAddressUpdatedTo.Host,
		Port: c.Expected.RelayAddressUpdatedTo.Port,
	}
	if got.RelayAddressUpdatedTo != wantAddr {
		t.Errorf("relay_address_updated_to = %+v, want %+v (PLY-132/133)", got.RelayAddressUpdatedTo, wantAddr)
	}
	if got.TrustAnchorCleared != c.Expected.TrustAnchorCleared {
		t.Errorf("trust_anchor_cleared = %v, want %v (PLY-026/130 never-wipe)", got.TrustAnchorCleared, c.Expected.TrustAnchorCleared)
	}
	if got.ChannelTokenCleared != c.Expected.ChannelTokenCleared {
		t.Errorf("channel_token_cleared = %v, want %v (PLY-026/130 never-wipe)", got.ChannelTokenCleared, c.Expected.ChannelTokenCleared)
	}
	if got.RetriesIndefinitely != c.Expected.RetriesIndefinitely {
		t.Errorf("retries_indefinitely = %v, want %v (PLY-130)", got.RetriesIndefinitely, c.Expected.RetriesIndefinitely)
	}
	if got.ReconnectResult != c.Expected.ReconnectResult {
		t.Errorf("reconnect_result = %q, want %q", got.ReconnectResult, c.Expected.ReconnectResult)
	}
}

// TestRelocateDecisionNeverMovesKeepsRetryingSameAddress pins PLY-130/132's
// other half — the different-network case the clause itself calls out: a relay
// that never re-announces a new address (no discovery response ever) leaves the
// player retrying its ORIGINAL persisted address forever, never relocating and
// never clearing a credential. The persisted address MUST be exactly what it
// started as.
func TestRelocateDecisionNeverMovesKeepsRetryingSameAddress(t *testing.T) {
	orig := virtualplayer.RelayAddress{Host: "198.51.100.12", Port: 5173}
	state := virtualplayer.PersistedState{
		RelayAddress:        orig,
		TrustAnchorPresent:  true,
		ChannelTokenPresent: true,
	}
	// Eight consecutive unreachable failures, no discovery response ever.
	attempts := make([]virtualplayer.ConnectionAttempt, 0, 8)
	for i := 1; i <= 8; i++ {
		attempts = append(attempts, virtualplayer.ConnectionAttempt{
			Attempt: i,
			Address: orig,
			Result:  virtualplayer.ResultNoResponse,
		})
	}

	got := virtualplayer.RelocateDecision(state, attempts)

	if got.RelayAddressUpdatedTo != orig {
		t.Errorf("relay_address_updated_to = %+v, want the untouched original %+v (PLY-026/130 never-wipe)", got.RelayAddressUpdatedTo, orig)
	}
	if got.TrustAnchorCleared || got.ChannelTokenCleared {
		t.Errorf("connection failure alone cleared a credential: %+v (PLY-130/135 forbid it)", got)
	}
	if !got.RetriesIndefinitely {
		t.Errorf("retries_indefinitely = false, want true (PLY-130)")
	}
	if got.ReconnectResult == "success" {
		t.Errorf("reconnect_result = success with no successful attempt")
	}
	if got.FailureClassificationBeforeRelocate != virtualplayer.ClassUnreachable {
		t.Errorf("failure_classification = %q, want %q (PLY-133)", got.FailureClassificationBeforeRelocate, virtualplayer.ClassUnreachable)
	}
}

// TestBackoffIntervalCapped gives PLY-131's "capped" teeth: the reconnect
// backoff interval MUST be nondecreasing, MUST never exceed the cap however
// many consecutive failures accrue, and MUST actually reach the cap (retries
// continue indefinitely at a bounded interval).
func TestBackoffIntervalCapped(t *testing.T) {
	var prev int64 = -1
	reachedCap := false
	for n := 0; n <= 1000; n++ {
		iv := virtualplayer.BackoffInterval(n)
		if iv > virtualplayer.BackoffCapMillis {
			t.Fatalf("BackoffInterval(%d) = %d exceeds the cap %d (PLY-131)", n, iv, virtualplayer.BackoffCapMillis)
		}
		if iv < prev {
			t.Fatalf("BackoffInterval(%d) = %d < previous %d — backoff must be nondecreasing", n, iv, prev)
		}
		prev = iv
		if iv == virtualplayer.BackoffCapMillis {
			reachedCap = true
		}
	}
	if !reachedCap {
		t.Errorf("BackoffInterval never reached the cap %d over 1000 failures (PLY-131)", virtualplayer.BackoffCapMillis)
	}
	if virtualplayer.BackoffInterval(0) != 0 {
		t.Errorf("BackoffInterval(0) = %d, want 0 (no failure, no wait)", virtualplayer.BackoffInterval(0))
	}
}

// bootRelocatableRelay boots ONE player/1 relay server (one identity, one TLS
// cert, one in-memory token store) mounted on TWO loopback listeners — a
// faithful "the relay moved to a new address on the same network" simulation:
// same relay, same committed certificate (PLY-060), same channel-token store,
// two dial addresses. It returns both listeners' host/port, the shared cert's
// raw DER (what a pairing code's commitment is formed over), the verified
// applied desired state, and a closePrimary func the test calls to make the
// primary address unreachable. Both listeners are t.Cleanup-closed.
func bootRelocatableRelay(t *testing.T, feederBaseURL string) (primaryHost string, primaryPort int, secondaryHost string, secondaryPort int, certDER []byte, applied desiredstate.Applied, closePrimary func()) {
	t.Helper()

	store, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := relayenroll.Run(feederBaseURL, store); err != nil {
		t.Fatalf("relayenroll.Run: %v", err)
	}
	applied, err = desiredstate.Pull(feederBaseURL, store)
	if err != nil {
		t.Fatalf("desiredstate.Pull: %v", err)
	}

	relayID, ok, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	if !ok {
		t.Fatalf("store.Identity: no identity persisted after enrollment")
	}

	cert, der, err := relayTLSCertificateForTest(relayID.CertPEM, relayID.PrivateKey)
	if err != nil {
		t.Fatalf("relayTLSCertificateForTest: %v", err)
	}
	certDER = der

	srv, err := playerserver.NewServer(relayID.CertPEM, applied.PairingGrants)
	if err != nil {
		t.Fatalf("playerserver.NewServer: %v", err)
	}
	srv.SetProgram(applied.ProgramRevision, applied.Priority, applied.Display, []wire.LeaseContent{{
		Type:      "image",
		AssetRef:  applied.Image.AssetRef,
		URL:       applied.Image.URL,
		ExpiresAt: applied.Image.ExpiresAt,
	}}, relayID.PrivateKey)

	mux := http.NewServeMux()
	srv.Register(mux)
	handler := apihttp.WithTraceID(mux)

	serveOn := func() (string, int, *http.Server) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("net.Listen: %v", err)
		}
		httpSrv := &http.Server{
			Handler:   handler,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
			ErrorLog:  quietErrorLog,
		}
		go func() { _ = httpSrv.ServeTLS(lis, "", "") }()
		t.Cleanup(func() { _ = httpSrv.Close() })
		h, p, err := net.SplitHostPort(lis.Addr().String())
		if err != nil {
			t.Fatalf("net.SplitHostPort: %v", err)
		}
		port, err := strconv.Atoi(p)
		if err != nil {
			t.Fatalf("strconv.Atoi(%q): %v", p, err)
		}
		return h, port, httpSrv
	}

	var primarySrv *http.Server
	primaryHost, primaryPort, primarySrv = serveOn()
	secondaryHost, secondaryPort, _ = serveOn()
	closePrimary = func() { _ = primarySrv.Close() }

	return primaryHost, primaryPort, secondaryHost, secondaryPort, certDER, applied, closePrimary
}

// TestRelocateLiveNeverWipeAcrossMovedAddress exercises BOTH committed pieces
// together against a relay that "moves address": the virtual player pairs
// against the relay's primary address and pulls its program; the primary
// address then goes unreachable; a fresh program pull is classified
// `unreachable` (a network-level failure that clears NOTHING — the player keeps
// its persisted address AND channel token, PLY-130/135); the player relocates
// to the relay's re-announced secondary address (PLY-132/133) and resumes a
// successful program pull ON THE RETAINED CHANNEL TOKEN — proving relocate keeps
// credentials (never-wipe, PLY-026).
func TestRelocateLiveNeverWipeAcrossMovedAddress(t *testing.T) {
	feederBaseURL, _ := bootTestFeeder(t)
	primaryHost, primaryPort, secondaryHost, secondaryPort, certDER, applied, closePrimary := bootRelocatableRelay(t, feederBaseURL)

	if len(applied.PairingGrants) == 0 {
		t.Fatalf("applied desired state carried no pairing_grants")
	}
	code, err := playerserver.FormPairingCode(primaryHost, primaryPort, applied.PairingGrants[0], certDER)
	if err != nil {
		t.Fatalf("FormPairingCode: %v", err)
	}

	sess, err := virtualplayer.Pair(code)
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	tokenBefore := sess.ChannelToken
	if tokenBefore == "" || sess.ScreenID == "" {
		t.Fatalf("Pair returned an incomplete session: %+v", sess)
	}
	if sess.RelayAddress.Host != primaryHost || sess.RelayAddress.Port != primaryPort {
		t.Fatalf("session pinned to %+v, want the primary %s:%d", sess.RelayAddress, primaryHost, primaryPort)
	}

	// While the primary is up, a pull is ordinary success.
	okAttempt, err := sess.PullProgram()
	if err != nil {
		t.Fatalf("PullProgram (primary up): %v", err)
	}
	if okAttempt.Classification != virtualplayer.ClassResponseReceived || okAttempt.RelayResponse.ErrorCode != "" {
		t.Fatalf("primary-up pull = %+v, want an ordinary success", okAttempt)
	}

	// The relay moves: the primary address is now unreachable.
	closePrimary()

	downAttempt, err := sess.PullProgram()
	if err != nil {
		t.Fatalf("PullProgram (primary down): %v", err)
	}
	if downAttempt.Classification != virtualplayer.ClassUnreachable {
		t.Errorf("primary-down classification = %q, want unreachable (PLY-133)", downAttempt.Classification)
	}
	// never-wipe on unreachable (PLY-130/135): the failed pull cleared nothing.
	out := virtualplayer.ReconnectDecision(
		virtualplayer.PersistedState{RelayAddress: sess.RelayAddress, TrustAnchorPresent: true, ChannelTokenPresent: true},
		downAttempt,
	)
	if out.ChannelTokenCleared || out.TrustAnchorCleared || out.RelayAddressCleared {
		t.Errorf("unreachable pull cleared a credential: %+v (PLY-130/135)", out)
	}
	if sess.RelayAddress.Host != primaryHost || sess.ChannelToken != tokenBefore {
		t.Errorf("session mutated a persisted credential on an unreachable pull: addr=%+v tokenChanged=%v (never-wipe)", sess.RelayAddress, sess.ChannelToken != tokenBefore)
	}

	// The relay re-announced its new address (PLY-022); the player relocates.
	disc := virtualplayer.DiscoveryResponse{
		ST:       "urn:waiveo:service:player:1",
		Location: fmt.Sprintf("https://%s/player/v1", net.JoinHostPort(secondaryHost, strconv.Itoa(secondaryPort))),
	}
	if err := sess.Relocate(disc); err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if sess.RelayAddress.Host != secondaryHost || sess.RelayAddress.Port != secondaryPort {
		t.Errorf("post-relocate address = %+v, want the secondary %s:%d (PLY-132/133)", sess.RelayAddress, secondaryHost, secondaryPort)
	}
	if sess.ChannelToken != tokenBefore {
		t.Errorf("relocate changed the channel token — PLY-026/130 keep it")
	}

	// Resume: a program pull on the retained token against the new address.
	resumed, err := sess.PullProgram()
	if err != nil {
		t.Fatalf("PullProgram (after relocate): %v", err)
	}
	if resumed.Classification != virtualplayer.ClassResponseReceived || resumed.RelayResponse.ErrorCode != "" {
		t.Errorf("post-relocate pull = %+v, want an ordinary success on the retained token (PLY-140)", resumed)
	}
}
