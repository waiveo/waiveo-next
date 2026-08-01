package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"sort"
	"testing"
)

// TestOpenCreatesExactOperationalTableSet asserts the relay's operational
// SQLite holds exactly the ten tables relay/1 REL-142/REL-130/REL-011 (plus
// player/1 PLY-091/PLY-105's own extension of that same tier, playersession.go)
// scope durable local state to — enrollment identity, last-applied generation,
// the desired-state verification key, the persisted clock floor, the app-peer
// trust pin, the bounded telemetry queue plus its loss markers and monotonic
// seq high-water (Telemetry upstream, REL-090/091), and the player-session
// pair (a minted channel token's hashed record, a one-time pairing grant's
// redemption marker) — and nothing else. In particular, no table here is
// capable of holding asset/media bytes (`#52` gateway posture): the relay's
// own content is never cached in this store — the telemetry_queue holds only
// small {seq,schema,payload,subject} event records, never asset bytes,
// telemetry_seq_high_water holds a single integer cursor, and
// player_channel_tokens holds only a token's own hash plus {screen_id,
// expires_at}, never the token itself (playersession.go's HashToken).
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
		"app_peer_trust_pin",
		"clock_floor",
		"desired_state_verification_key",
		"last_applied_generation",
		"player_channel_tokens",
		"player_redeemed_grants",
		"player_redemption_reports",
		"relay_identity",
		"telemetry_loss_marker",
		"telemetry_queue",
		"telemetry_seq_high_water",
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

// TestClockFloorIsServedFromCacheAfterTheFirstRead proves the read is cached,
// not merely fast.
//
// The floor is read on every time-windowed decision the relay makes. Because Go
// evaluates a call's arguments before the call, that included the clock handed
// to the pairing rate limiter — so an attempt the budget was about to REFUSE
// still forced a durable read, letting an unauthenticated caller make the relay
// do disk work by being turned away.
//
// Proven by removing the table out from under the store: an uncached read would
// fail, so a correct answer here can only have come from memory. That is a
// deterministic proof; timing a query would not be.
func TestClockFloorIsServedFromCacheAfterTheFirstRead(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.SetClockFloor(1_700_000_000_000); err != nil {
		t.Fatalf("SetClockFloor: %v", err)
	}
	if ms, ok, err := store.ClockFloor(); err != nil || !ok || ms != 1_700_000_000_000 {
		t.Fatalf("first ClockFloor = (%d, %v, %v); want the persisted floor", ms, ok, err)
	}

	if _, err := store.db.Exec(`DROP TABLE clock_floor`); err != nil {
		t.Fatalf("dropping the table to prove the read is cached: %v", err)
	}
	ms, ok, err := store.ClockFloor()
	if err != nil {
		t.Fatalf("ClockFloor went to the database after the first read: %v", err)
	}
	if !ok || ms != 1_700_000_000_000 {
		t.Errorf("cached ClockFloor = (%d, %v); want the floor read before the table was dropped", ms, ok)
	}
}

// TestAdvancingTheFloorInvalidatesTheCache is the half that makes caching safe
// to do at all.
//
// A cache that survived a write would report a floor LOWER than the persisted
// one — precisely how a rolled-back host clock re-opens a window REL-130 exists
// to keep closed. The floor is advance-only, so this is the only direction the
// cache can be wrong in, and it must be impossible rather than unlikely.
func TestAdvancingTheFloorInvalidatesTheCache(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.SetClockFloor(1_000); err != nil {
		t.Fatalf("SetClockFloor: %v", err)
	}
	if ms, _, _ := store.ClockFloor(); ms != 1_000 {
		t.Fatalf("primed floor = %d, want 1000", ms)
	}
	if advanced, err := store.SetClockFloor(2_000); err != nil || !advanced {
		t.Fatalf("SetClockFloor(2000) = (%v, %v); want advanced", advanced, err)
	}
	if ms, ok, err := store.ClockFloor(); err != nil || !ok || ms != 2_000 {
		t.Errorf("after an advance ClockFloor = (%d, %v, %v); want 2000 — a stale cache here would let a "+
			"rolled-back host clock re-open a window the relay had already closed (REL-130)", ms, ok, err)
	}
}

// TestClockFloorAbsenceIsCachedButARejectedAdvanceStillRefreshes covers the two
// remaining transitions: a store that has never had a floor answers ok=false
// without going back to disk, and a SetClockFloor that does NOT advance still
// leaves the next read correct rather than serving a value it assumed.
func TestClockFloorAbsenceIsCachedButARejectedAdvanceStillRefreshes(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if ms, ok, err := store.ClockFloor(); err != nil || ok || ms != 0 {
		t.Fatalf("a store with no floor = (%d, %v, %v); want (0, false, nil)", ms, ok, err)
	}
	if _, err := store.SetClockFloor(5_000); err != nil {
		t.Fatalf("SetClockFloor: %v", err)
	}
	// The absence must NOT have survived the write.
	if ms, ok, _ := store.ClockFloor(); !ok || ms != 5_000 {
		t.Fatalf("after the first floor was set, ClockFloor = (%d, %v); want (5000, true)", ms, ok)
	}
	// A rejected (non-advancing) write leaves the persisted floor alone; the next
	// read must still report it.
	if advanced, err := store.SetClockFloor(4_000); err != nil || advanced {
		t.Fatalf("SetClockFloor(4000) = (%v, %v); want not advanced (the floor is advance-only)", advanced, err)
	}
	if ms, ok, _ := store.ClockFloor(); !ok || ms != 5_000 {
		t.Errorf("after a rejected advance ClockFloor = (%d, %v); want the unchanged 5000", ms, ok)
	}
}

// TestAFailedClockFloorReadIsNotCached: a store that could not answer must be
// asked again, not remembered as broken.
//
// Caching a failure would be the worst version of this cache. The failure would
// be cached as "no floor persisted", which reads as ok=false — so one transient
// database error would permanently drop the relay back to its bare wall clock,
// and a rolled-back host clock could then re-open every window REL-130 exists to
// keep closed. That is the exact direction the advance-only argument says cannot
// happen, so it must not be reachable by another route.
func TestAFailedClockFloorReadIsNotCached(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Persisted, and the cache left COLD: the write invalidates it, so the next
	// read is the one that will fail.
	if _, err := store.SetClockFloor(9_000); err != nil {
		t.Fatalf("SetClockFloor: %v", err)
	}
	if _, err := store.db.Exec(`DROP TABLE clock_floor`); err != nil {
		t.Fatalf("dropping the table: %v", err)
	}
	if _, _, err := store.ClockFloor(); err == nil {
		t.Fatal("a read against a missing table returned no error; the rest of this test proves nothing")
	}

	// The store recovers. Recreated with the same floor the relay had persisted.
	if _, err := store.db.Exec(`CREATE TABLE clock_floor (id INTEGER PRIMARY KEY CHECK (id = 1), floor_ms INTEGER NOT NULL)`); err != nil {
		t.Fatalf("recreating the table: %v", err)
	}
	if _, err := store.db.Exec(`INSERT INTO clock_floor (id, floor_ms) VALUES (1, 9000)`); err != nil {
		t.Fatalf("restoring the row: %v", err)
	}

	ms, ok, err := store.ClockFloor()
	if err != nil {
		t.Fatalf("ClockFloor after recovery: %v", err)
	}
	if !ok || ms != 9_000 {
		t.Errorf("after a transient failure ClockFloor = (%d, %v); want (9000, true) — the failure was cached, "+
			"so this relay would run on its bare wall clock until restarted (REL-130)", ms, ok)
	}
}
