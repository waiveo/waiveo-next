package desiredstate

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	feederenroll "github.com/maaxton/waiveo-next/internal/feeder/enroll"
	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

const testImagePath = "../../feeder/origin/testdata/photon.png"

func loadTestImage(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(testImagePath)
	if err != nil {
		t.Fatalf("read fixture image %s: %v", testImagePath, err)
	}
	return b
}

func testFeederIdentity(t *testing.T) *signing.Identity {
	t.Helper()
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	return id
}

// newTestFeeder builds a real feeder enrollment + desired-state-pull server
// (Task 6's package) serving snap, mounted on an httptest TLS server —
// exactly the shape Pull fetches from in production, only over a loopback
// httptest listener rather than cmd/waiveo-feeder's own :7420.
func newTestFeeder(t *testing.T, id *signing.Identity, snap wire.StateSnapshotBody) *httptest.Server {
	t.Helper()

	srv, err := feederenroll.NewServer(id, snap)
	if err != nil {
		t.Fatalf("feederenroll.NewServer: %v", err)
	}

	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	return ts
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

// enrolledStore builds a store already enrolled against ts (the real
// relay/enroll.Run client flow), so it holds a persisted
// desired_state_verification_key trust anchor before Pull is exercised.
func enrolledStore(t *testing.T, ts *httptest.Server) *identity.Store {
	t.Helper()
	store := openStore(t)
	if err := relayenroll.Run(ts.URL, store); err != nil {
		t.Fatalf("relayenroll.Run: %v", err)
	}
	return store
}

// TestPullAppliesScreenProgram is Step 1's core assertion: Pull against a
// live feeder, enrollment-verified end to end, returns the applied
// screen-program's one image content item and persists last-applied.
func TestPullAppliesScreenProgram(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, []wire.PairingGrant{grant.Mint()})
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	ts := newTestFeeder(t, id, snap)
	store := enrolledStore(t, ts)

	applied, err := Pull(ts.URL, store)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	wantAssetRef := signhash.ContentID(img)
	if applied.Generation != 1 {
		t.Errorf("Generation = %d, want 1", applied.Generation)
	}
	if applied.ScreenID != snap.Sections.ScreenPrograms[0].ScreenID {
		t.Errorf("ScreenID = %q, want %q", applied.ScreenID, snap.Sections.ScreenPrograms[0].ScreenID)
	}
	if applied.ProgramRevision != snap.Sections.ScreenPrograms[0].ProgramRevision {
		t.Errorf("ProgramRevision = %q, want %q", applied.ProgramRevision, snap.Sections.ScreenPrograms[0].ProgramRevision)
	}
	if applied.Image.AssetRef != wantAssetRef {
		t.Errorf("Image.AssetRef = %q, want %q", applied.Image.AssetRef, wantAssetRef)
	}
	if applied.Image.URL == "" {
		t.Error("Image.URL is empty, want the signed content URL")
	}

	gen, hash, ok, err := store.LastAppliedGeneration()
	if err != nil {
		t.Fatalf("LastAppliedGeneration: %v", err)
	}
	if !ok {
		t.Fatal("LastAppliedGeneration ok = false after a successful Pull, want true")
	}
	if gen != 1 || hash != snap.Hash {
		t.Errorf("LastAppliedGeneration = (%d, %q), want (1, %q)", gen, hash, snap.Hash)
	}
}

// TestPullRejectsWrongKeySignedSnapshot is the load-bearing security test
// (REL-071/072, `#28`): a snapshot whose `signature` verifies under some
// key OTHER than the persisted desired_state_verification_key trust
// anchor must be rejected outright — no section applied, last-applied
// unchanged — even though the enrollment response itself carried the
// correct (real feeder) verification key.
func TestPullRejectsWrongKeySignedSnapshot(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	// Re-sign the {generation, hash} scope under a fresh, unrelated key —
	// the snapshot's `sections`/`hash` are untouched (so a hash check alone
	// would not catch this), only `signature` now verifies under a key the
	// relay never learned at enrollment.
	_, attackerPriv := signhash.GenerateKey()
	canon, err := wire.SignedScopeBytes(snap.Generation, snap.Hash)
	if err != nil {
		t.Fatalf("wire.SignedScopeBytes: %v", err)
	}
	tampered := snap
	tampered.Signature = wire.EncodeSignature(signhash.Sign(attackerPriv, canon))

	// The enrollment endpoint still hands out id's own (real) signing pub
	// as the trust anchor — only /state/pull's served snapshot is signed
	// wrong.
	ts := newTestFeeder(t, id, tampered)
	store := enrolledStore(t, ts)

	applied, err := Pull(ts.URL, store)
	if !errors.Is(err, ErrSnapshotSignatureInvalid) {
		t.Fatalf("Pull error = %v, want ErrSnapshotSignatureInvalid", err)
	}
	if applied != (Applied{}) {
		t.Errorf("Pull returned a non-zero Applied on rejection: %+v", applied)
	}

	_, _, ok, err := store.LastAppliedGeneration()
	if err != nil {
		t.Fatalf("LastAppliedGeneration: %v", err)
	}
	if ok {
		t.Error("LastAppliedGeneration ok = true after a signature-invalid rejection, want false (nothing applied)")
	}
}

// TestPullRejectsTamperedSections asserts a snapshot whose `sections` no
// longer hashes to its own `hash` field is rejected outright, without ever
// reaching signature verification.
func TestPullRejectsTamperedSections(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	tampered := snap
	tampered.Sections.ScreenPrograms = append([]wire.ScreenProgram(nil), snap.Sections.ScreenPrograms...)
	tampered.Sections.ScreenPrograms[0].ScreenID = "tampered-screen-id"
	// Hash and Signature are left as Build produced them for the ORIGINAL
	// sections — now stale relative to the tampered sections.

	ts := newTestFeeder(t, id, tampered)
	store := enrolledStore(t, ts)

	applied, err := Pull(ts.URL, store)
	if !errors.Is(err, ErrSnapshotHashMismatch) {
		t.Fatalf("Pull error = %v, want ErrSnapshotHashMismatch", err)
	}
	if applied != (Applied{}) {
		t.Errorf("Pull returned a non-zero Applied on rejection: %+v", applied)
	}

	_, _, ok, err := store.LastAppliedGeneration()
	if err != nil {
		t.Fatalf("LastAppliedGeneration: %v", err)
	}
	if ok {
		t.Error("LastAppliedGeneration ok = true after a hash-mismatch rejection, want false (nothing applied)")
	}
}

// TestPullSameGenerationIsIdempotent asserts re-pulling the same
// (verified, unchanged) generation is a no-op: it succeeds and returns the
// same applied program, and last-applied stays exactly what it was
// (REL-070).
func TestPullSameGenerationIsIdempotent(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	ts := newTestFeeder(t, id, snap)
	store := enrolledStore(t, ts)

	first, err := Pull(ts.URL, store)
	if err != nil {
		t.Fatalf("first Pull: %v", err)
	}
	genAfterFirst, hashAfterFirst, ok, err := store.LastAppliedGeneration()
	if err != nil || !ok {
		t.Fatalf("LastAppliedGeneration after first Pull: gen=%d hash=%q ok=%v err=%v", genAfterFirst, hashAfterFirst, ok, err)
	}

	second, err := Pull(ts.URL, store)
	if err != nil {
		t.Fatalf("second (idempotent) Pull: %v", err)
	}
	if second != first {
		t.Errorf("second Pull = %+v, want identical to first %+v", second, first)
	}

	genAfterSecond, hashAfterSecond, ok, err := store.LastAppliedGeneration()
	if err != nil || !ok {
		t.Fatalf("LastAppliedGeneration after second Pull: gen=%d hash=%q ok=%v err=%v", genAfterSecond, hashAfterSecond, ok, err)
	}
	if genAfterSecond != genAfterFirst || hashAfterSecond != hashAfterFirst {
		t.Errorf("last-applied changed across an idempotent re-pull: first=(%d,%q) second=(%d,%q)",
			genAfterFirst, hashAfterFirst, genAfterSecond, hashAfterSecond)
	}
}

// TestPullRejectsLowerGeneration asserts a pulled snapshot's generation
// lower than the persisted last-applied generation is rejected outright
// (REL-052), and last-applied is left unchanged.
func TestPullRejectsLowerGeneration(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	// snapshot.Build always signs generation 1 (Wave-1 first-photon).
	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	ts := newTestFeeder(t, id, snap)
	store := enrolledStore(t, ts)

	// Simulate the relay having already applied a LATER generation than
	// what this feeder is currently serving (e.g. from a previous feeder
	// process this test doesn't model).
	if err := store.SetLastAppliedGeneration(2, "sha256:"+"deadbeef"); err != nil {
		t.Fatalf("SetLastAppliedGeneration: %v", err)
	}

	applied, err := Pull(ts.URL, store)
	if !errors.Is(err, ErrGenerationRegressed) {
		t.Fatalf("Pull error = %v, want ErrGenerationRegressed", err)
	}
	if applied != (Applied{}) {
		t.Errorf("Pull returned a non-zero Applied on rejection: %+v", applied)
	}

	gen, hash, ok, err := store.LastAppliedGeneration()
	if err != nil {
		t.Fatalf("LastAppliedGeneration: %v", err)
	}
	if !ok || gen != 2 || hash != "sha256:deadbeef" {
		t.Errorf("LastAppliedGeneration = (%d, %q, ok=%v), want (2, \"sha256:deadbeef\", true) — unchanged by the regressed rejection", gen, hash, ok)
	}
}

// TestPullFailsWithoutTrustAnchor asserts Pull refuses to even attempt
// verification when the store holds no persisted
// desired_state_verification_key yet (never enrolled) — there is no trust
// anchor to check anything against.
func TestPullFailsWithoutTrustAnchor(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	ts := newTestFeeder(t, id, snap)
	store := openStore(t) // deliberately NOT enrolled

	_, err = Pull(ts.URL, store)
	if !errors.Is(err, ErrNoTrustAnchor) {
		t.Fatalf("Pull error = %v, want ErrNoTrustAnchor", err)
	}
}
