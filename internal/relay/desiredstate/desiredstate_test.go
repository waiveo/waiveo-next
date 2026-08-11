package desiredstate

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// newTestFeeder builds a real feeder enrollment server mounted on an
// httptest TLS server — the bootstrap surface enrolledStore obtains its
// trust anchor from. Desired state itself no longer moves over HTTP: each
// test hands its snapshot straight to the verify chain (applySnapshot),
// exactly as a state.snapshot frame received on the persistent connection
// would reach it (internal/relay/relayconn.SnapshotFromFrame →
// VerifyAndApply).
func newTestFeeder(t *testing.T, id *signing.Identity) *httptest.Server {
	t.Helper()

	srv, err := feederenroll.NewServer(id)
	if err != nil {
		t.Fatalf("feederenroll.NewServer: %v", err)
	}

	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// applySnapshot runs snap through the verify chain exactly as a received
// state.snapshot frame would: marshal to the wire bytes, re-extract the raw
// `sections` (the REL-060 structural gate must see the original JSON — a
// Go-decoded Sections cannot reveal an omitted key), VerifyAndApply.
func applySnapshot(t *testing.T, store *identity.Store, snap wire.StateSnapshotBody) (Applied, error) {
	t.Helper()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return applyRawSnapshot(t, store, raw)
}

// applyRawSnapshot is applySnapshot for hand-crafted wire bytes (e.g. a body
// with a sections key omitted) that the typed wire.StateSnapshotBody could
// never itself marshal.
func applyRawSnapshot(t *testing.T, store *identity.Store, raw []byte) (Applied, error) {
	t.Helper()
	var body wire.StateSnapshotBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal snapshot body: %v", err)
	}
	var envelope struct {
		Sections json.RawMessage `json:"sections"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("extract raw sections: %v", err)
	}
	return VerifyAndApply(store, body, envelope.Sections)
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
// desired_state_verification_key trust anchor before the verify chain is exercised.
func enrolledStore(t *testing.T, ts *httptest.Server) *identity.Store {
	t.Helper()
	return enrolledStoreAt(t, ts, filepath.Join(t.TempDir(), "relay.db"))
}

// enrolledStoreAt is enrolledStore over a caller-chosen on-disk path — what a
// case that closes the store and REOPENS the same file needs, since the file is
// the only thing carried across a simulated restart.
func enrolledStoreAt(t *testing.T, ts *httptest.Server, dbPath string) *identity.Store {
	t.Helper()
	store, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := relayenroll.Run(ts.URL, store); err != nil {
		t.Fatalf("relayenroll.Run: %v", err)
	}
	return store
}

// TestApplyAppliesScreenProgram is the core assertion: a feeder-signed
// snapshot, enrollment-verified end to end, applies the screen-program's
// one image content item and persists last-applied.
func TestApplyAppliesScreenProgram(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, []wire.PairingGrant{grant.Mint()})
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	applied, err := applySnapshot(t, store, snap)
	if err != nil {
		t.Fatalf("VerifyAndApply: %v", err)
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
	if applied.Priority != snap.Sections.ScreenPrograms[0].Priority {
		t.Errorf("Priority = %q, want %q (REL-061/PLY-108, unmodified)", applied.Priority, snap.Sections.ScreenPrograms[0].Priority)
	}
	if applied.Display != snap.Sections.ScreenPrograms[0].Display {
		t.Errorf("Display = %q, want %q (REL-061/PLY-109, unmodified)", applied.Display, snap.Sections.ScreenPrograms[0].Display)
	}
	if applied.Image.AssetRef != wantAssetRef {
		t.Errorf("Image.AssetRef = %q, want %q", applied.Image.AssetRef, wantAssetRef)
	}
	if applied.Image.URL == "" {
		t.Error("Image.URL is empty, want the signed content URL")
	}
	if !reflect.DeepEqual(applied.PairingGrants, snap.Sections.PairingGrants) {
		t.Errorf("PairingGrants = %+v, want %+v (the verified snapshot's sections.pairing_grants, unmodified)", applied.PairingGrants, snap.Sections.PairingGrants)
	}

	gen, hash, ok, err := store.LastAppliedGeneration()
	if err != nil {
		t.Fatalf("LastAppliedGeneration: %v", err)
	}
	if !ok {
		t.Fatal("LastAppliedGeneration ok = false after a successful apply, want true")
	}
	if gen != 1 || hash != snap.Hash {
		t.Errorf("LastAppliedGeneration = (%d, %q), want (1, %q)", gen, hash, snap.Hash)
	}
}

// TestApplyExposesEdgeRules asserts a verified snapshot's edge_rules section
// (REL-062) is surfaced on Applied.EdgeRules unmodified — the raw rules/1
// authored-rule JSON the feeder signed, which Task 2's automationhost
// compiles + loads into the edge engine. It rides the SAME hash/signature
// verification as the screen-program: no separate trust step applies to it.
func TestApplyExposesEdgeRules(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	if len(snap.Sections.EdgeRules.Rules) == 0 {
		t.Fatal("precondition: snapshot.Build emitted no edge rules")
	}

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	applied, err := applySnapshot(t, store, snap)
	if err != nil {
		t.Fatalf("VerifyAndApply: %v", err)
	}

	if !reflect.DeepEqual(applied.EdgeRules, []json.RawMessage(snap.Sections.EdgeRules.Rules)) {
		t.Errorf("applied.EdgeRules = %s, want %s (the verified snapshot's edge_rules, unmodified)",
			applied.EdgeRules, snap.Sections.EdgeRules.Rules)
	}
}

// TestApplyRejectsWrongKeyEdgeRulesSnapshot is the signed-section-discipline
// test (REL-062/056): a snapshot whose edge_rules section was tampered and
// then re-signed under a key OTHER than the enrollment-learned trust anchor
// MUST be rejected by the SAME signature check that rejects a wrong-key
// screen-program — there is NO second trust path for edge_rules. Nothing is
// applied and last-applied is left untouched.
func TestApplyRejectsWrongKeyEdgeRulesSnapshot(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	if len(snap.Sections.EdgeRules.Rules) == 0 {
		t.Fatal("precondition: snapshot.Build emitted no edge rules to tamper with")
	}

	// Tamper the edge_rules section (swap in an attacker-authored rule), then
	// re-hash and re-sign the whole snapshot under a fresh, unrelated key —
	// hash stays internally consistent, only `signature` now verifies under a
	// key the relay never learned at enrollment.
	tampered := snap
	tampered.Sections.EdgeRules = wire.EdgeRules{
		RulesMinorVersion: "1.0",
		Rules:             []json.RawMessage{json.RawMessage(`{"id":"attacker","mode":"single","triggers":[],"conditions":[],"actions":[]}`)},
	}
	rehash, err := wire.HashSections(tampered.Sections)
	if err != nil {
		t.Fatalf("wire.HashSections: %v", err)
	}
	tampered.Hash = rehash
	_, attackerPriv := signhash.GenerateKey()
	canon, err := wire.SignedScopeBytes(tampered.Generation, tampered.Hash)
	if err != nil {
		t.Fatalf("wire.SignedScopeBytes: %v", err)
	}
	tampered.Signature = wire.EncodeSignature(signhash.Sign(attackerPriv, canon))

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	applied, err := applySnapshot(t, store, tampered)
	if !errors.Is(err, ErrSnapshotSignatureInvalid) {
		t.Fatalf("apply error = %v, want ErrSnapshotSignatureInvalid (edge_rules rides the same trust path)", err)
	}
	if !reflect.DeepEqual(applied, Applied{}) {
		t.Errorf("apply returned a non-zero Applied on rejection: %+v", applied)
	}

	_, _, ok, err := store.LastAppliedGeneration()
	if err != nil {
		t.Fatalf("LastAppliedGeneration: %v", err)
	}
	if ok {
		t.Error("LastAppliedGeneration ok = true after a wrong-key edge_rules rejection, want false (nothing applied)")
	}
}

// TestApplyRejectsWrongKeySignedSnapshot is the load-bearing security test
// (REL-071/072, `#28`): a snapshot whose `signature` verifies under some
// key OTHER than the persisted desired_state_verification_key trust
// anchor must be rejected outright — no section applied, last-applied
// unchanged — even though the enrollment response itself carried the
// correct (real feeder) verification key.
func TestApplyRejectsWrongKeySignedSnapshot(t *testing.T) {
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
	// as the trust anchor — only the applied snapshot is signed
	// wrong.
	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	applied, err := applySnapshot(t, store, tampered)
	if !errors.Is(err, ErrSnapshotSignatureInvalid) {
		t.Fatalf("apply error = %v, want ErrSnapshotSignatureInvalid", err)
	}
	if !reflect.DeepEqual(applied, Applied{}) {
		t.Errorf("apply returned a non-zero Applied on rejection: %+v", applied)
	}

	_, _, ok, err := store.LastAppliedGeneration()
	if err != nil {
		t.Fatalf("LastAppliedGeneration: %v", err)
	}
	if ok {
		t.Error("LastAppliedGeneration ok = true after a signature-invalid rejection, want false (nothing applied)")
	}
}

// TestApplyRejectsTamperedSections asserts a snapshot whose `sections` no
// longer hashes to its own `hash` field is rejected outright, without ever
// reaching signature verification.
func TestApplyRejectsTamperedSections(t *testing.T) {
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

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	applied, err := applySnapshot(t, store, tampered)
	if !errors.Is(err, ErrSnapshotHashMismatch) {
		t.Fatalf("apply error = %v, want ErrSnapshotHashMismatch", err)
	}
	if !reflect.DeepEqual(applied, Applied{}) {
		t.Errorf("apply returned a non-zero Applied on rejection: %+v", applied)
	}

	_, _, ok, err := store.LastAppliedGeneration()
	if err != nil {
		t.Fatalf("LastAppliedGeneration: %v", err)
	}
	if ok {
		t.Error("LastAppliedGeneration ok = true after a hash-mismatch rejection, want false (nothing applied)")
	}
}

// TestApplySameGenerationIsIdempotent asserts re-pulling the same
// (verified, unchanged) generation is a no-op: it succeeds and returns the
// same applied program, and last-applied stays exactly what it was
// (REL-070).
func TestApplySameGenerationIsIdempotent(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	first, err := applySnapshot(t, store, snap)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	genAfterFirst, hashAfterFirst, ok, err := store.LastAppliedGeneration()
	if err != nil || !ok {
		t.Fatalf("LastAppliedGeneration after first apply: gen=%d hash=%q ok=%v err=%v", genAfterFirst, hashAfterFirst, ok, err)
	}

	second, err := applySnapshot(t, store, snap)
	if err != nil {
		t.Fatalf("second (idempotent) apply: %v", err)
	}
	if !reflect.DeepEqual(second, first) {
		t.Errorf("second apply = %+v, want identical to first %+v", second, first)
	}

	genAfterSecond, hashAfterSecond, ok, err := store.LastAppliedGeneration()
	if err != nil || !ok {
		t.Fatalf("LastAppliedGeneration after second apply: gen=%d hash=%q ok=%v err=%v", genAfterSecond, hashAfterSecond, ok, err)
	}
	if genAfterSecond != genAfterFirst || hashAfterSecond != hashAfterFirst {
		t.Errorf("last-applied changed across an idempotent re-pull: first=(%d,%q) second=(%d,%q)",
			genAfterFirst, hashAfterFirst, genAfterSecond, hashAfterSecond)
	}
}

// TestApplyRejectsLowerGeneration asserts a pulled snapshot's generation
// lower than the persisted last-applied generation is rejected outright
// (REL-052), and last-applied is left unchanged.
func TestApplyRejectsLowerGeneration(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	// snapshot.Build always signs generation 1 (Wave-1 first-photon).
	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	// Simulate the relay having already applied a LATER generation than
	// what this feeder is currently serving (e.g. from a previous feeder
	// process this test doesn't model).
	if err := store.SetLastAppliedGeneration(2, "sha256:"+"deadbeef"); err != nil {
		t.Fatalf("SetLastAppliedGeneration: %v", err)
	}

	applied, err := applySnapshot(t, store, snap)
	if !errors.Is(err, ErrGenerationRegressed) {
		t.Fatalf("apply error = %v, want ErrGenerationRegressed", err)
	}
	if !reflect.DeepEqual(applied, Applied{}) {
		t.Errorf("apply returned a non-zero Applied on rejection: %+v", applied)
	}

	gen, hash, ok, err := store.LastAppliedGeneration()
	if err != nil {
		t.Fatalf("LastAppliedGeneration: %v", err)
	}
	if !ok || gen != 2 || hash != "sha256:deadbeef" {
		t.Errorf("LastAppliedGeneration = (%d, %q, ok=%v), want (2, \"sha256:deadbeef\", true) — unchanged by the regressed rejection", gen, hash, ok)
	}
}

// TestApplyExposesSiteEffective asserts a verified snapshot's
// revocation_and_site.site_effective (REL-066) is surfaced on
// Applied.SiteEffective unmodified — the persisted site placement a relay's
// dayparting/sun evaluation uses across a restart, riding the same
// hash/signature verification as everything else.
func TestApplyExposesSiteEffective(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	want := snap.Sections.RevocationAndSite.SiteEffective
	if want.TZ == "" {
		t.Fatal("precondition: snapshot.Build emitted an empty site_effective")
	}

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	applied, err := applySnapshot(t, store, snap)
	if err != nil {
		t.Fatalf("VerifyAndApply: %v", err)
	}
	if applied.SiteEffective != want {
		t.Errorf("applied.SiteEffective = %+v, want %+v (REL-066, unmodified)", applied.SiteEffective, want)
	}
}

// TestApplyExposesRevoked asserts a verified snapshot's
// revocation_and_site.revoked (REL-066) is surfaced on Applied.Revoked
// unmodified. Until it was, the list was decoded off the wire and dropped on
// the floor here — nothing downstream could enforce a revocation because
// nothing downstream could see one, which is what left REL-123 unimplementable
// however carefully the player/1 server checked its local set.
//
// The list is populated by hand and the snapshot re-hashed + re-signed under
// the feeder's own key, because neither feeder builder ever emits a non-empty
// `revoked` (both hardcode an empty slice, internal/feeder/snapshot) — so a
// test that only used what the feeder produces would pass against an
// implementation that dropped the field entirely.
func TestApplyExposesRevoked(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	if len(snap.Sections.RevocationAndSite.Revoked) != 0 {
		t.Fatal("precondition: snapshot.Build emitted a non-empty revoked list; this case populates its own")
	}

	want := []string{"01J8Z3K4N5P6Q7R8S9T0V1W2R1", "01J8Z3K4N5P6Q7R8S9T0V1W2R2"}
	snap.Sections.RevocationAndSite.Revoked = want
	snap.Hash, err = wire.HashSections(snap.Sections)
	if err != nil {
		t.Fatalf("wire.HashSections: %v", err)
	}
	canon, err := wire.SignedScopeBytes(snap.Generation, snap.Hash)
	if err != nil {
		t.Fatalf("wire.SignedScopeBytes: %v", err)
	}
	snap.Signature = wire.EncodeSignature(signhash.Sign(id.SigningPriv(), canon))

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	applied, err := applySnapshot(t, store, snap)
	if err != nil {
		t.Fatalf("VerifyAndApply: %v", err)
	}
	if !reflect.DeepEqual(applied.Revoked, want) {
		t.Errorf("applied.Revoked = %v, want %v (REL-066, unmodified and in order)", applied.Revoked, want)
	}
}

// TestServedDeviceInventorySurvivesTheStore is REL-063/064's durability half,
// and the counterpart of TestServedRevocationSurvivesTheStore below: the same
// verified `device_inventory` section, read back through the OFFLINE accessor
// from a store closed and reopened from its own on-disk file.
//
// The section reached the process on the returned Applied and went no further,
// so the adopted set — the relay's only statement of which devices it may drive
// — lived exactly as long as the process. That is invisible while the app peer
// is up, because the very next pull restates it. It stops being invisible on
// the boot that follows a power cut, where there is no pull: every consumer of
// the set fails closed, so the relay comes up healthy and drives nothing, and
// screen keep-alive — whose whole job is that a screen idling at Home shows
// NOTHING until a human walks past — relaunches nothing at all.
//
// The read path takes only the store: no feeder, no app peer, no connection.
func TestServedDeviceInventorySurvivesTheStore(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	want := wire.DeviceInventory{
		Devices: []json.RawMessage{json.RawMessage(
			`{"device_id":"01J8Z3K4N5P6Q7R8S9T0V1DEV1","driver":"roku","native_id":"uuid:roku:ecp:AA11","entities":[{"entity_id":"01J8Z3K4N5P6Q7R8S9T0V1ENT1","device_class":"media-player","enabled":true}]}`)},
		PackMatchPatterns: []json.RawMessage{json.RawMessage(`{"ssdp":"roku:ecp"}`)},
	}.Normalized()
	snap.Sections.DeviceInventory = want
	snap.Hash, err = wire.HashSections(snap.Sections)
	if err != nil {
		t.Fatalf("wire.HashSections: %v", err)
	}
	canon, err := wire.SignedScopeBytes(snap.Generation, snap.Hash)
	if err != nil {
		t.Fatalf("wire.SignedScopeBytes: %v", err)
	}
	snap.Signature = wire.EncodeSignature(signhash.Sign(id.SigningPriv(), canon))

	dbPath := filepath.Join(t.TempDir(), "relay.db")
	ts := newTestFeeder(t, id)
	store := enrolledStoreAt(t, ts, dbPath)

	if _, err := applySnapshot(t, store, snap); err != nil {
		t.Fatalf("VerifyAndApply: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close(): %v", err)
	}

	reopened, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open (reopen): %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	got, err := ServedDeviceInventory(reopened)
	if err != nil {
		t.Fatalf("ServedDeviceInventory: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ServedDeviceInventory after a restart = %+v, want %+v (REL-063/064 carried through REL-055/061's durable row)", got, want)
	}
}

// TestServedDeviceInventoryEmptyOnFreshStore asserts a relay that has never
// applied a generation reads back an empty inventory rather than an error — a
// boot with nothing synced must not fail on the read itself. What that empty
// set MEANS is the fail-closed answer every consumer already gives it: a relay
// that has synced no adoption decision drives nothing.
func TestServedDeviceInventoryEmptyOnFreshStore(t *testing.T) {
	id := testFeederIdentity(t)
	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	got, err := ServedDeviceInventory(store)
	if err != nil {
		t.Fatalf("ServedDeviceInventory on a store that has applied nothing: %v", err)
	}
	if len(got.Devices) != 0 || len(got.PackMatchPatterns) != 0 {
		t.Errorf("ServedDeviceInventory on a fresh store = %+v, want both arrays empty", got)
	}
	// Normalized, not nil: REL-060's own "present and empty" shape, so a caller
	// ranging over either array sees the section rather than an absence.
	if got.Devices == nil || got.PackMatchPatterns == nil {
		t.Errorf("ServedDeviceInventory on a fresh store returned a nil array (%+v); want the REL-060 empty placeholder's shape", got)
	}
}

// TestServedRevocationSurvivesTheStore is REL-123's durability half: the same
// verified `revoked` list, read back through the OFFLINE accessor from a store
// closed and reopened from its own on-disk file. Carrying it only on the
// returned Applied would leave a relay that reboots during an app-peer outage
// serving its PERSISTED programs to its PERSISTED channel tokens with an EMPTY
// revocation set, until a pull it cannot make restated one — which is exactly
// the window "a synced one MUST be enforced regardless of connectivity" closes.
//
// The read path takes only the store: no feeder, no app peer, no connection —
// so a successful read here IS the offline property, not a simulation of it.
func TestServedRevocationSurvivesTheStore(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	want := []string{"01J8Z3K4N5P6Q7R8S9T0V1W2R1", "01J8Z3K4N5P6Q7R8S9T0V1W2R2"}
	snap.Sections.RevocationAndSite.Revoked = want
	snap.Hash, err = wire.HashSections(snap.Sections)
	if err != nil {
		t.Fatalf("wire.HashSections: %v", err)
	}
	canon, err := wire.SignedScopeBytes(snap.Generation, snap.Hash)
	if err != nil {
		t.Fatalf("wire.SignedScopeBytes: %v", err)
	}
	snap.Signature = wire.EncodeSignature(signhash.Sign(id.SigningPriv(), canon))

	dbPath := filepath.Join(t.TempDir(), "relay.db")
	ts := newTestFeeder(t, id)
	store := enrolledStoreAt(t, ts, dbPath)

	if _, err := applySnapshot(t, store, snap); err != nil {
		t.Fatalf("VerifyAndApply: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close(): %v", err)
	}

	reopened, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open (reopen): %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	got, err := ServedRevocation(reopened)
	if err != nil {
		t.Fatalf("ServedRevocation: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ServedRevocation after a restart = %v, want %v (REL-066/REL-123)", got, want)
	}
}

// TestServedRevocationEmptyOnFreshStore asserts a relay that has never applied
// a generation reads back an empty set rather than an error — a boot with
// nothing synced must not fail on the read itself. REL-123's own carve-out
// covers what that empty set means: "a revocation the relay has not yet pulled
// is not yet enforceable."
func TestServedRevocationEmptyOnFreshStore(t *testing.T) {
	id := testFeederIdentity(t)
	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	got, err := ServedRevocation(store)
	if err != nil {
		t.Fatalf("ServedRevocation on a store that has applied nothing: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ServedRevocation on a fresh store = %v, want empty", got)
	}
}

// TestApplyExposesContentOrigin asserts a verified snapshot's
// revocation_and_site.content_origin (REL-061/066) is surfaced on
// Applied.ContentOrigin unmodified — the content-origin base URL a later
// relay-side schedule resolver (internal/relay/schedulehost) derives
// fetchable content URLs from. It rides the SAME hash/signature verification
// as every other section member.
func TestApplyExposesContentOrigin(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	want := snap.Sections.RevocationAndSite.ContentOrigin
	if want == "" {
		t.Fatal("precondition: snapshot.Build emitted an empty content_origin")
	}

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	applied, err := applySnapshot(t, store, snap)
	if err != nil {
		t.Fatalf("VerifyAndApply: %v", err)
	}
	if applied.ContentOrigin != want {
		t.Errorf("applied.ContentOrigin = %q, want %q (REL-061/066, unmodified)", applied.ContentOrigin, want)
	}
}

// TestApplyExposesScreenPrograms asserts a verified snapshot's full
// screen_programs array (REL-061) is surfaced on Applied.ScreenPrograms
// unmodified — carrying priority/display/content through for Task 2's
// offline program delivery.
func TestApplyExposesScreenPrograms(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	applied, err := applySnapshot(t, store, snap)
	if err != nil {
		t.Fatalf("VerifyAndApply: %v", err)
	}
	if !reflect.DeepEqual(applied.ScreenPrograms, snap.Sections.ScreenPrograms) {
		t.Errorf("applied.ScreenPrograms = %+v, want %+v (REL-061, unmodified)", applied.ScreenPrograms, snap.Sections.ScreenPrograms)
	}
}

// TestApplyExposesSchedule asserts a verified snapshot's schedule section
// (REL-065) is surfaced on Applied.Schedule unmodified — the raw
// scheduling-core rows + scope nodes the feeder signed, which a later
// relay-side resolver derives a dayparting timeline from. It rides the SAME
// hash/signature verification as every other section (no separate trust step),
// and a populated schedule leaves the byte-identical-marshaling → hash
// invariant intact: repopulate the section, recompute the hash + re-sign under
// the feeder's own key, and Pull verifies it exactly as it does every section.
func TestApplyExposesSchedule(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	// snapshot.Build now emits its own real (Wave-2 demo) schedule: all
	// seven arrays present and non-nil (REL-060/065). This test overwrites
	// the section with its own fixture rows below, so only presence is
	// asserted here — not emptiness (Task 2 authors a real demo schedule).
	built := snap.Sections.Schedule
	for name, arr := range map[string][]json.RawMessage{
		"scope_nodes":      built.ScopeNodes,
		"playlists":        built.Playlists,
		"schedules":        built.Schedules,
		"validity_windows": built.ValidityWindows,
		"dayparts":         built.Dayparts,
		"fallbacks":        built.Fallbacks,
		"preset_batches":   built.PresetBatches,
	} {
		if arr == nil {
			t.Errorf("snapshot.Build emitted a nil schedule.%s, want a present (possibly empty) slice (REL-060/065)", name)
		}
	}

	// Populate the schedule section with opaque scheduling-core rows (fixture
	// ULIDs), normalize, then re-hash + re-sign under the feeder's own key so
	// the snapshot stays internally consistent (hash covers sections, REL-053).
	sched := wire.ScheduleSection{
		ScopeNodes: []json.RawMessage{
			json.RawMessage(`{"id":"01ARZ3NDEKTSV4RRFFQ69G5FA1","revision":1,"kind":"site","tz":"America/Chicago"}`),
			json.RawMessage(`{"id":"01ARZ3NDEKTSV4RRFFQ69G5FA2","revision":1,"kind":"screen","parent":"01ARZ3NDEKTSV4RRFFQ69G5FA1"}`),
		},
		Schedules: []json.RawMessage{
			json.RawMessage(`{"id":"01ARZ3NDEKTSV4RRFFQ69G5FA4","revision":1,"scope_node":"01ARZ3NDEKTSV4RRFFQ69G5FA2"}`),
		},
	}.Normalized()
	snap.Sections.Schedule = sched
	snap.Hash, err = wire.HashSections(snap.Sections)
	if err != nil {
		t.Fatalf("wire.HashSections: %v", err)
	}
	canon, err := wire.SignedScopeBytes(snap.Generation, snap.Hash)
	if err != nil {
		t.Fatalf("wire.SignedScopeBytes: %v", err)
	}
	snap.Signature = wire.EncodeSignature(signhash.Sign(id.SigningPriv(), canon))

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	applied, err := applySnapshot(t, store, snap)
	if err != nil {
		t.Fatalf("VerifyAndApply: %v", err)
	}
	if !reflect.DeepEqual(applied.Schedule, sched) {
		t.Errorf("applied.Schedule = %+v, want %+v (REL-065, unmodified, riding the same hash/signature)", applied.Schedule, sched)
	}
}

// TestApplyRejectsIncompleteSections asserts a snapshot whose `sections`
// object omits any one of the seven REL-060 keys is rejected outright
// (ErrSectionsIncomplete) — the structural completeness gate fires before
// hash/signature verification, and nothing is applied.
func TestApplyRejectsIncompleteSections(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	// Enroll against a real feeder so the store holds the trust anchor.
	realTS := newTestFeeder(t, id)
	store := enrolledStore(t, realTS)

	// Craft a body whose sections omits `schedule` (any one key would do).
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal snap: %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("Unmarshal body: %v", err)
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(body["sections"], &sections); err != nil {
		t.Fatalf("Unmarshal sections: %v", err)
	}
	delete(sections, "schedule")
	body["sections"], err = json.Marshal(sections)
	if err != nil {
		t.Fatalf("Marshal sections-without-schedule: %v", err)
	}
	rawBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal incomplete body: %v", err)
	}

	applied, err := applyRawSnapshot(t, store, rawBody)
	if !errors.Is(err, ErrSectionsIncomplete) {
		t.Fatalf("apply error = %v, want ErrSectionsIncomplete (REL-060)", err)
	}
	if !reflect.DeepEqual(applied, Applied{}) {
		t.Errorf("apply returned a non-zero Applied on rejection: %+v", applied)
	}
	if _, _, ok, err := store.LastAppliedGeneration(); err != nil {
		t.Fatalf("LastAppliedGeneration: %v", err)
	} else if ok {
		t.Error("LastAppliedGeneration ok = true after an incomplete-sections rejection, want false (nothing applied)")
	}
}

// TestApplyIgnoresWorkflowGeneration asserts the relay accepts and
// structurally ignores a non-empty workflow_generation section (REL-068,
// RESERVED): a snapshot carrying arbitrary content there — re-hashed and
// re-signed under the feeder's own key — still applies its screen-program
// exactly as one carrying the reserved null placeholder.
func TestApplyIgnoresWorkflowGeneration(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	// Put arbitrary reserved content in workflow_generation, then re-hash +
	// re-sign under the feeder's own signing key so the snapshot is valid.
	snap.Sections.WorkflowGeneration = map[string]any{"future_reserved": []any{"anything", 42.0}}
	snap.Hash, err = wire.HashSections(snap.Sections)
	if err != nil {
		t.Fatalf("wire.HashSections: %v", err)
	}
	canon, err := wire.SignedScopeBytes(snap.Generation, snap.Hash)
	if err != nil {
		t.Fatalf("wire.SignedScopeBytes: %v", err)
	}
	snap.Signature = wire.EncodeSignature(signhash.Sign(id.SigningPriv(), canon))

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	applied, err := applySnapshot(t, store, snap)
	if err != nil {
		t.Fatalf("VerifyAndApply: %v", err)
	}
	if applied.Generation != 1 || applied.ScreenID != snap.Sections.ScreenPrograms[0].ScreenID {
		t.Errorf("applied = %+v, want the screen-program applied despite non-empty workflow_generation (REL-068)", applied)
	}
}

// TestApplyFailsWithoutTrustAnchor asserts VerifyAndApply refuses to even attempt
// verification when the store holds no persisted
// desired_state_verification_key yet (never enrolled) — there is no trust
// anchor to check anything against.
func TestApplyFailsWithoutTrustAnchor(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	_ = newTestFeeder(t, id) // the bootstrap listener exists; this relay never enrolls against it
	store := openStore(t)    // deliberately NOT enrolled

	_, err = applySnapshot(t, store, snap)
	if !errors.Is(err, ErrNoTrustAnchor) {
		t.Fatalf("apply error = %v, want ErrNoTrustAnchor", err)
	}
}
