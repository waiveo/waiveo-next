package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"sort"
	"testing"
)

// TestOpenCreatesExactOperationalTableSet asserts the relay's operational
// SQLite holds exactly the three tables relay/1 REL-142 scopes durable
// local state to — enrollment identity, last-applied generation, and the
// desired-state verification key — and nothing else. In particular, no
// table here is capable of holding asset/media bytes (`#52` gateway
// posture): the relay's own content is never cached in this store.
func TestOpenCreatesExactOperationalTableSet(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tables, err := store.tableNames()
	if err != nil {
		t.Fatalf("tableNames(): %v", err)
	}
	sort.Strings(tables)

	want := []string{
		"desired_state_verification_key",
		"last_applied_generation",
		"relay_identity",
	}
	if len(tables) != len(want) {
		t.Fatalf("tables = %v, want exactly %v", tables, want)
	}
	for i := range want {
		if tables[i] != want[i] {
			t.Fatalf("tables = %v, want exactly %v", tables, want)
		}
	}
}

// TestIdentityAbsentBeforeEnrollment asserts a freshly opened store, before
// any identity has been persisted, reports "not present" rather than a
// zero-value identity or an error.
func TestIdentityAbsentBeforeEnrollment(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	_, ok, err := store.Identity()
	if err != nil {
		t.Fatalf("Identity(): %v", err)
	}
	if ok {
		t.Fatal("Identity() ok = true on a fresh store, want false")
	}
}

// TestSetIdentityRoundTrip confirms SetIdentity/Identity round-trips the
// relay_id, cert PEM, and private key exactly.
func TestSetIdentityRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	certPEM := []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n")

	if err := store.SetIdentity("relay-abc123", certPEM, priv); err != nil {
		t.Fatalf("SetIdentity: %v", err)
	}

	got, ok, err := store.Identity()
	if err != nil {
		t.Fatalf("Identity(): %v", err)
	}
	if !ok {
		t.Fatal("Identity() ok = false after SetIdentity, want true")
	}
	if got.RelayID != "relay-abc123" {
		t.Errorf("RelayID = %q, want %q", got.RelayID, "relay-abc123")
	}
	if string(got.CertPEM) != string(certPEM) {
		t.Errorf("CertPEM = %q, want %q", got.CertPEM, certPEM)
	}
	if !got.PrivateKey.Equal(priv) {
		t.Error("PrivateKey round-trip mismatch")
	}
	if !ed25519.PublicKey(got.PrivateKey.Public().(ed25519.PublicKey)).Equal(pub) {
		t.Error("PrivateKey's public half does not match the original generated key")
	}
}

// TestSetIdentityOverwritesPriorRow confirms SetIdentity replaces, rather
// than accumulates, a previously persisted identity row.
func TestSetIdentityOverwritesPriorRow(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	_, priv1, _ := ed25519.GenerateKey(rand.Reader)
	_, priv2, _ := ed25519.GenerateKey(rand.Reader)

	if err := store.SetIdentity("relay-1", []byte("cert-1"), priv1); err != nil {
		t.Fatalf("first SetIdentity: %v", err)
	}
	if err := store.SetIdentity("relay-2", []byte("cert-2"), priv2); err != nil {
		t.Fatalf("second SetIdentity: %v", err)
	}

	got, ok, err := store.Identity()
	if err != nil {
		t.Fatalf("Identity(): %v", err)
	}
	if !ok {
		t.Fatal("Identity() ok = false, want true")
	}
	if got.RelayID != "relay-2" {
		t.Errorf("RelayID = %q, want %q (the second, overwriting SetIdentity)", got.RelayID, "relay-2")
	}
}

// TestDesiredStateVerificationKeyRoundTrip confirms
// SetDesiredStateVerificationKey/DesiredStateVerificationKey round-trips an
// ed25519 public key exactly.
func TestDesiredStateVerificationKeyRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}

	if _, ok, err := store.DesiredStateVerificationKey(); err != nil {
		t.Fatalf("DesiredStateVerificationKey() on a fresh store: %v", err)
	} else if ok {
		t.Fatal("DesiredStateVerificationKey() ok = true on a fresh store, want false")
	}

	if err := store.SetDesiredStateVerificationKey(pub); err != nil {
		t.Fatalf("SetDesiredStateVerificationKey: %v", err)
	}

	got, ok, err := store.DesiredStateVerificationKey()
	if err != nil {
		t.Fatalf("DesiredStateVerificationKey(): %v", err)
	}
	if !ok {
		t.Fatal("DesiredStateVerificationKey() ok = false after Set, want true")
	}
	if !got.Equal(pub) {
		t.Error("DesiredStateVerificationKey round-trip mismatch")
	}
}

// TestLastAppliedGenerationRoundTrip confirms
// SetLastAppliedGeneration/LastAppliedGeneration round-trips {generation,
// hash} exactly (REL-073: persisted beside the verification key).
func TestLastAppliedGenerationRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, _, ok, err := store.LastAppliedGeneration(); err != nil {
		t.Fatalf("LastAppliedGeneration() on a fresh store: %v", err)
	} else if ok {
		t.Fatal("LastAppliedGeneration() ok = true on a fresh store, want false")
	}

	if err := store.SetLastAppliedGeneration(7, "sha256:abc123"); err != nil {
		t.Fatalf("SetLastAppliedGeneration: %v", err)
	}

	gen, hash, ok, err := store.LastAppliedGeneration()
	if err != nil {
		t.Fatalf("LastAppliedGeneration(): %v", err)
	}
	if !ok {
		t.Fatal("LastAppliedGeneration() ok = false after Set, want true")
	}
	if gen != 7 || hash != "sha256:abc123" {
		t.Errorf("LastAppliedGeneration() = (%d, %q), want (7, %q)", gen, hash, "sha256:abc123")
	}
}

// TestReopenPersistsAcrossProcesses is the offline-boot persistence
// property (`#28`): a second Store opened on the same SQLite path reads
// back everything a prior Store persisted, without either Store needing to
// stay open concurrently — modeling a relay process restart.
func TestReopenPersistsAcrossProcesses(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}

	func() {
		store, err := Open(dbPath)
		if err != nil {
			t.Fatalf("first Open: %v", err)
		}
		defer store.Close()

		if err := store.SetIdentity("relay-persist", []byte("cert-bytes"), priv); err != nil {
			t.Fatalf("SetIdentity: %v", err)
		}
		if err := store.SetDesiredStateVerificationKey(pub); err != nil {
			t.Fatalf("SetDesiredStateVerificationKey: %v", err)
		}
	}()

	store2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("second Open (same path): %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	id, ok, err := store2.Identity()
	if err != nil {
		t.Fatalf("Identity() on reopened store: %v", err)
	}
	if !ok || id.RelayID != "relay-persist" {
		t.Fatalf("Identity() on reopened store = %+v, ok=%v, want RelayID=relay-persist, ok=true", id, ok)
	}

	key, ok, err := store2.DesiredStateVerificationKey()
	if err != nil {
		t.Fatalf("DesiredStateVerificationKey() on reopened store: %v", err)
	}
	if !ok || !key.Equal(pub) {
		t.Fatal("DesiredStateVerificationKey() did not survive reopen")
	}
}
