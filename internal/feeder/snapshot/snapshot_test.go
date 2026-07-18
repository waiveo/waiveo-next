package snapshot

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

const testImagePath = "../origin/testdata/photon.png"

func loadTestImage(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(testImagePath)
	if err != nil {
		t.Fatalf("read fixture image %s: %v", testImagePath, err)
	}
	return b
}

func testIdentity(t *testing.T) *signing.Identity {
	t.Helper()
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	return id
}

// TestBuildShape asserts Build produces generation 1, one screen-program
// carrying one image content item whose asset_ref is the image's content
// ID, and that sections carries all 7 REL-060 keys.
func TestBuildShape(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	snap, err := Build(img, "https://origin.example", id)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if snap.Generation != 1 {
		t.Errorf("Generation = %d, want 1", snap.Generation)
	}

	if len(snap.Sections.ScreenPrograms) != 1 {
		t.Fatalf("len(ScreenPrograms) = %d, want 1", len(snap.Sections.ScreenPrograms))
	}
	prog := snap.Sections.ScreenPrograms[0]
	if prog.Priority != "scheduled" {
		t.Errorf("Priority = %q, want scheduled", prog.Priority)
	}
	if prog.Display != "content" {
		t.Errorf("Display = %q, want content", prog.Display)
	}
	if len(prog.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(prog.Content))
	}

	wantAssetRef := signhash.ContentID(img)
	item := prog.Content[0]
	if item.AssetRef != wantAssetRef {
		t.Errorf("AssetRef = %q, want %q", item.AssetRef, wantAssetRef)
	}
	wantURL := "https://origin.example/content/" + wantAssetRef[len("sha256:"):]
	if item.URL != wantURL {
		t.Errorf("URL = %q, want %q", item.URL, wantURL)
	}

	// All 7 REL-060 keys must be structurally present. ScreenPrograms and
	// PairingGrants are the array-typed ones easiest to assert directly;
	// the rest are asserted via the wire package's own field-name test
	// (TestStateSnapshotBodyFieldNames) — this test only re-confirms the
	// two this task actually populates/empties.
	if snap.Sections.PairingGrants == nil || len(snap.Sections.PairingGrants) != 0 {
		t.Errorf("PairingGrants = %#v, want a non-nil empty slice (present, empty — REL-060/REL-067)", snap.Sections.PairingGrants)
	}

	// Direct end-to-end wire-shape check: marshal Build's actual output to
	// JSON and confirm sections carries exactly the 7 REL-060 keys —
	// complementing wire.TestStateSnapshotBodyFieldNames (which checks the
	// shared type in isolation) with a check against this package's real
	// Build output.
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal(snap): %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(body["sections"], &sections); err != nil {
		t.Fatalf("Unmarshal sections: %v", err)
	}
	wantKeys := []string{
		"screen_programs",
		"edge_rules",
		"device_inventory",
		"schedule",
		"revocation_and_site",
		"pairing_grants",
		"workflow_generation",
	}
	if len(sections) != len(wantKeys) {
		t.Fatalf("sections marshaled to %d keys, want exactly %d (%v); got %s", len(sections), len(wantKeys), wantKeys, body["sections"])
	}
	for _, k := range wantKeys {
		if _, ok := sections[k]; !ok {
			t.Errorf("sections JSON missing REL-060 key %q; got %s", k, body["sections"])
		}
	}
}

// TestBuildSignatureVerifies asserts the snapshot's signature verifies
// under the feeder's own signing public key over {generation, hash}, and
// fails under a different key (REL-075).
func TestBuildSignatureVerifies(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	snap, err := Build(img, "https://origin.example", id)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	canon, err := generationHashCanonBytes(snap.Generation, snap.Hash)
	if err != nil {
		t.Fatalf("generationHashCanonBytes: %v", err)
	}
	sigBytes, err := wire.DecodeSignature(snap.Signature)
	if err != nil {
		t.Fatalf("wire.DecodeSignature: %v", err)
	}

	if !signhash.Verify(id.SigningPub(), canon, sigBytes) {
		t.Error("signature did not verify under the feeder's own signing pub")
	}

	otherPub, _ := signhash.GenerateKey()
	if signhash.Verify(otherPub, canon, sigBytes) {
		t.Error("signature verified under an unrelated key — should have failed")
	}
}

// TestBuildSignatureBindsGeneration asserts the signed bytes include
// generation together with hash (REL-075), not hash alone: relabeling a
// snapshot under a different generation must invalidate its old signature.
func TestBuildSignatureBindsGeneration(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	snap, err := Build(img, "https://origin.example", id)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	sigBytes, err := wire.DecodeSignature(snap.Signature)
	if err != nil {
		t.Fatalf("wire.DecodeSignature: %v", err)
	}

	// Relabel under a different generation, same hash: the old signature
	// must not verify against the new {generation, hash} bytes.
	relabeled, err := generationHashCanonBytes(snap.Generation+1, snap.Hash)
	if err != nil {
		t.Fatalf("generationHashCanonBytes: %v", err)
	}
	if signhash.Verify(id.SigningPub(), relabeled, sigBytes) {
		t.Error("signature verified after relabeling under a different generation — should have failed (REL-075)")
	}

	// Sanity: hash alone (without generation) must not be what was signed.
	if signhash.Verify(id.SigningPub(), []byte(snap.Hash), sigBytes) {
		t.Error("signature verified over hash alone — signed scope must bind generation too (REL-075)")
	}
}

// TestHashDeterministic asserts recomputing hash from sections (same
// canonicalization Build used) reproduces the snapshot's own hash, and
// that a different image produces a different hash (REL-053).
func TestHashDeterministic(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	snap, err := Build(img, "https://origin.example", id)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	recomputed, err := hashSections(snap.Sections)
	if err != nil {
		t.Fatalf("hashSections: %v", err)
	}
	if recomputed != snap.Hash {
		t.Errorf("recomputed hash %q != snapshot hash %q", recomputed, snap.Hash)
	}

	otherImg := append([]byte(nil), img...)
	otherImg = append(otherImg, 0x00) // perturb bytes -> different content
	otherSnap, err := Build(otherImg, "https://origin.example", id)
	if err != nil {
		t.Fatalf("Build (other image): %v", err)
	}
	if otherSnap.Hash == snap.Hash {
		t.Error("different images produced the same hash — want different hashes for different sections content")
	}
}

func TestBuildRejectsNilIdentity(t *testing.T) {
	img := loadTestImage(t)
	if _, err := Build(img, "https://origin.example", nil); err == nil {
		t.Error("Build(nil identity) succeeded, want an error")
	}
}
