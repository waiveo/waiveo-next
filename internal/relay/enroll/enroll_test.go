package enroll

import (
	"crypto/ed25519"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	feederenroll "github.com/maaxton/waiveo-next/internal/feeder/enroll"
	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

const testImagePath = "../../feeder/origin/testdata/photon.png"

// newTestFeeder builds a real feeder enrollment server (Task 6's package),
// mounted on an httptest TLS server — exactly the shape Run enrolls
// against in production, only over a loopback httptest listener rather
// than cmd/waiveo-feeder's own :7420.
func newTestFeeder(t *testing.T) (*httptest.Server, *signing.Identity) {
	t.Helper()

	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}

	img, err := os.ReadFile(testImagePath)
	if err != nil {
		t.Fatalf("read fixture image %s: %v", testImagePath, err)
	}

	g := grant.Mint()
	snap, err := snapshot.Build(img, "https://origin.example", id, []wire.PairingGrant{g})
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	srv, err := feederenroll.NewServer(id, snap)
	if err != nil {
		t.Fatalf("feederenroll.NewServer: %v", err)
	}

	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)

	return ts, id
}

func openStore(t *testing.T) *identity.Store {
	t.Helper()
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestRunEnrollsAndPersistsFeederSigningKey is Step 1's core assertion:
// after Run against a live feeder, the store holds the feeder's own
// desired-state signing public key — the enrollment-anchored trust anchor
// (`#28`, REL-071) — plus a relay_id and a certificate.
func TestRunEnrollsAndPersistsFeederSigningKey(t *testing.T) {
	ts, feederID := newTestFeeder(t)
	store := openStore(t)

	if err := Run(ts.URL, store); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, ok, err := store.DesiredStateVerificationKey()
	if err != nil {
		t.Fatalf("DesiredStateVerificationKey(): %v", err)
	}
	if !ok {
		t.Fatal("DesiredStateVerificationKey() ok = false after Run, want true")
	}
	if !got.Equal(feederID.SigningPub()) {
		t.Errorf("persisted verification key = %x, want the feeder's own SigningPub() %x", []byte(got), []byte(feederID.SigningPub()))
	}

	id, ok, err := store.Identity()
	if err != nil {
		t.Fatalf("Identity(): %v", err)
	}
	if !ok {
		t.Fatal("Identity() ok = false after Run, want true")
	}
	if id.RelayID == "" {
		t.Error("persisted RelayID is empty")
	}
	if len(id.CertPEM) == 0 {
		t.Error("persisted CertPEM is empty")
	}
	if len(id.PrivateKey) != ed25519.PrivateKeySize {
		t.Errorf("persisted PrivateKey length = %d, want %d", len(id.PrivateKey), ed25519.PrivateKeySize)
	}
}

// TestRunDoesNotReEnrollAlreadyEnrolledStore is Step 1's persistence-across-
// restart assertion: a second Run against a store that already holds a
// persisted identity must NOT contact the feeder again (a fresh claim token
// each Run would otherwise be consumed uselessly) — it reads the persisted
// identity back unchanged.
func TestRunDoesNotReEnrollAlreadyEnrolledStore(t *testing.T) {
	ts, _ := newTestFeeder(t)
	store := openStore(t)

	if err := Run(ts.URL, store); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	firstID, _, err := store.Identity()
	if err != nil {
		t.Fatalf("Identity() after first Run: %v", err)
	}

	// A second Run must succeed without needing a fresh claim token — the
	// feeder's claim-token endpoint mints only one pending token at a time
	// (feeder/enroll's own single-flight semantics), so if Run tried to
	// re-enroll here it would either reuse (and get CLAIM_TOKEN_INVALID on)
	// the already-redeemed token, or silently mint and burn a second one.
	// Either way, the persisted identity must come back unchanged.
	if err := Run(ts.URL, store); err != nil {
		t.Fatalf("second Run (already enrolled): %v", err)
	}

	secondID, ok, err := store.Identity()
	if err != nil {
		t.Fatalf("Identity() after second Run: %v", err)
	}
	if !ok {
		t.Fatal("Identity() ok = false after second Run, want true")
	}
	if secondID.RelayID != firstID.RelayID {
		t.Errorf("RelayID changed across the second Run: first = %q, second = %q (Run must not re-enroll)", firstID.RelayID, secondID.RelayID)
	}
}

// TestRunAgainstFreshStoreOnSamePathReadsPersistedIdentity models the
// offline-boot persistence property (`#28`): a second process (a second
// *identity.Store opened on the same SQLite path) sees the identity/key the
// first process's Run persisted, and Run against it is a no-op.
func TestRunAgainstFreshStoreOnSamePathReadsPersistedIdentity(t *testing.T) {
	ts, feederID := newTestFeeder(t)
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	store1, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("first identity.Open: %v", err)
	}
	if err := Run(ts.URL, store1); err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantRelayID, _, err := store1.Identity()
	if err != nil {
		t.Fatalf("Identity() on store1: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close store1: %v", err)
	}

	store2, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("second identity.Open (same path): %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	if err := Run(ts.URL, store2); err != nil {
		t.Fatalf("Run on reopened store: %v", err)
	}

	got, ok, err := store2.Identity()
	if err != nil {
		t.Fatalf("Identity() on store2: %v", err)
	}
	if !ok || got.RelayID != wantRelayID.RelayID {
		t.Fatalf("Identity() on reopened store = %+v, ok=%v, want RelayID=%q", got, ok, wantRelayID.RelayID)
	}

	key, ok, err := store2.DesiredStateVerificationKey()
	if err != nil {
		t.Fatalf("DesiredStateVerificationKey() on store2: %v", err)
	}
	if !ok || !key.Equal(feederID.SigningPub()) {
		t.Fatal("DesiredStateVerificationKey() did not survive reopen")
	}
}

// TestDecodeVerificationKeyRejectsWrongLength is the carry-forward guard
// (Task 2 finding): a `desired_state_verification_key` that doesn't decode
// to exactly ed25519.PublicKeySize bytes must be rejected with a typed
// error before it ever reaches signhash.Verify (which would otherwise
// receive a wrong-length key) or gets persisted into the store.
func TestDecodeVerificationKeyRejectsWrongLength(t *testing.T) {
	tests := map[string]string{
		"too short":      "ed25519:" + hex.EncodeToString(make([]byte, ed25519.PublicKeySize-1)),
		"too long":       "ed25519:" + hex.EncodeToString(make([]byte, ed25519.PublicKeySize+1)),
		"empty":          "ed25519:",
		"missing prefix": hex.EncodeToString(make([]byte, ed25519.PublicKeySize)),
		"not hex":        "ed25519:not-hex-at-all",
	}

	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := decodeVerificationKey(in)
			if err == nil {
				t.Fatalf("decodeVerificationKey(%q) succeeded, want a typed error", in)
			}
		})
	}
}

// TestDecodeVerificationKeyAccepts is the sanity check that a well-formed
// key of exactly the right shape decodes cleanly.
func TestDecodeVerificationKeyAccepts(t *testing.T) {
	pub := make(ed25519.PublicKey, ed25519.PublicKeySize)
	for i := range pub {
		pub[i] = byte(i)
	}
	in := "ed25519:" + hex.EncodeToString(pub)

	got, err := decodeVerificationKey(in)
	if err != nil {
		t.Fatalf("decodeVerificationKey(%q): %v", in, err)
	}
	if !got.Equal(pub) {
		t.Errorf("decodeVerificationKey(%q) = %x, want %x", in, []byte(got), []byte(pub))
	}
}
