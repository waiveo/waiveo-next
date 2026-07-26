package snapshot

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
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

	snap, err := Build(img, "https://origin.example", id, nil)
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

	// The schedule section (REL-065) now carries the real two-daypart demo
	// schedule (Task 2): exactly the seven scheduling-core row arrays are
	// present, none ever `null` (REL-060). scope_nodes/playlists/schedules/
	// dayparts/preset_batches are populated by the demo schedule;
	// validity_windows/fallbacks are unused by the demo and stay `[]`.
	var sched map[string]json.RawMessage
	if err := json.Unmarshal(sections["schedule"], &sched); err != nil {
		t.Fatalf("Unmarshal schedule section: %v; got %s", err, sections["schedule"])
	}
	schedKeys := []string{
		"scope_nodes",
		"playlists",
		"schedules",
		"validity_windows",
		"dayparts",
		"fallbacks",
		"preset_batches",
	}
	if len(sched) != len(schedKeys) {
		t.Errorf("schedule section marshaled to %d keys, want exactly %d (%v); got %s", len(sched), len(schedKeys), schedKeys, sections["schedule"])
	}
	emptyKeys := []string{"validity_windows", "fallbacks"}
	for _, k := range emptyKeys {
		if string(sched[k]) != "[]" {
			t.Errorf("schedule.%s = %s, want [] (empty array, never null — REL-060/065)", k, sched[k])
		}
	}
	populatedKeys := []string{"scope_nodes", "playlists", "schedules", "dayparts", "preset_batches"}
	for _, k := range populatedKeys {
		if string(sched[k]) == "[]" || string(sched[k]) == "null" {
			t.Errorf("schedule.%s = %s, want a populated array (the Task 2 demo schedule rows)", k, sched[k])
		}
	}
}

// TestBuildEmitsDemoEdgeRule asserts Build populates the `edge_rules`
// section (REL-062) with the Wave-1 first-automation demo rule: a
// `rules_minor_version` naming the rules/1 minor ("1.0") and exactly one
// authored rule that compiles to a genuine EDGE-class rule (a state
// trigger to:["on"] driving a device_command launch). The rule rides the
// same `sections` value everything else does, so it is covered by `hash`
// (REL-053) and transitively by `signature` (REL-075) — asserted here by
// recomputing the hash and verifying the signature with the section present.
func TestBuildEmitsDemoEdgeRule(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	snap, err := Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	er := snap.Sections.EdgeRules
	if er.RulesMinorVersion != "1.0" {
		t.Errorf("EdgeRules.RulesMinorVersion = %q, want %q (REL-062)", er.RulesMinorVersion, "1.0")
	}
	if len(er.Rules) != 1 {
		t.Fatalf("len(EdgeRules.Rules) = %d, want exactly 1 demo rule", len(er.Rules))
	}

	// The demo rule must be a REAL edge rule the relay's compiler accepts and
	// classifies edge (not app) — otherwise Task 2 could not load it into the
	// edge engine.
	entry, cerr := compile.Compile(er.Rules[0])
	if cerr != nil {
		t.Fatalf("demo edge rule did not compile: %v", cerr)
	}
	if entry.ExecutionClass != "edge" {
		t.Fatalf("demo rule ExecutionClass = %q, want edge", entry.ExecutionClass)
	}

	// It must carry a state trigger and a device_command launch action
	// (the Wave-1 first automation).
	var authored struct {
		Triggers []struct {
			Type string `json:"type"`
			To   []any  `json:"to"`
		} `json:"triggers"`
		Actions []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(er.Rules[0], &authored); err != nil {
		t.Fatalf("unmarshal demo rule: %v", err)
	}
	if len(authored.Triggers) != 1 || authored.Triggers[0].Type != "state" {
		t.Errorf("demo rule triggers = %+v, want a single state trigger", authored.Triggers)
	}
	if len(authored.Actions) != 1 || authored.Actions[0].Type != "device_command" || authored.Actions[0].Command != "launch" {
		t.Errorf("demo rule actions = %+v, want a single device_command launch", authored.Actions)
	}

	// Section rides hash + signature exactly like every other section.
	recomputed, err := hashSections(snap.Sections)
	if err != nil {
		t.Fatalf("hashSections: %v", err)
	}
	if recomputed != snap.Hash {
		t.Errorf("recomputed hash %q != snapshot hash %q — edge_rules must be covered by hash (REL-053)", recomputed, snap.Hash)
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
		t.Error("signature did not verify with a populated edge_rules section (REL-075)")
	}
}

// TestBuildEmitsSiteEffective asserts Build populates
// revocation_and_site.site_effective (REL-066) with the first-photon site's
// real timezone and coordinates — not an empty placeholder — so a relay's
// dayparting and sun/time evaluation survive a restart from the persisted
// snapshot alone. The section rides the same hash/signature as everything
// else (recompute + verify with it populated).
func TestBuildEmitsSiteEffective(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	snap, err := Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	se := snap.Sections.RevocationAndSite.SiteEffective
	if se.TZ == "" {
		t.Error("SiteEffective.TZ is empty, want the first-photon site's real IANA zone (REL-066)")
	}
	if se.Lat == 0 && se.Long == 0 {
		t.Errorf("SiteEffective = %+v, want real first-photon coordinates (REL-066)", se)
	}
	// revoked must still be a present, non-nil empty slice (REL-066/060).
	if snap.Sections.RevocationAndSite.Revoked == nil {
		t.Error("RevocationAndSite.Revoked is nil, want a present empty slice (REL-060)")
	}

	// The section rides hash + signature exactly like every other section.
	recomputed, err := hashSections(snap.Sections)
	if err != nil {
		t.Fatalf("hashSections: %v", err)
	}
	if recomputed != snap.Hash {
		t.Errorf("recomputed hash %q != snapshot hash %q — site_effective must be covered by hash (REL-053)", recomputed, snap.Hash)
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
		t.Error("signature did not verify with a populated site_effective section (REL-075)")
	}
}

// TestBuildEmitsContentOrigin asserts Build carries its contentBaseURL
// argument into revocation_and_site.content_origin (REL-061/066) — the base
// URL a screen fetches this site's content from — riding the same
// hash/signature as every other section member.
func TestBuildEmitsContentOrigin(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	snap, err := Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if got, want := snap.Sections.RevocationAndSite.ContentOrigin, "https://origin.example"; got != want {
		t.Errorf("RevocationAndSite.ContentOrigin = %q, want %q (REL-061/066)", got, want)
	}

	// The section rides hash + signature exactly like every other section.
	recomputed, err := hashSections(snap.Sections)
	if err != nil {
		t.Fatalf("hashSections: %v", err)
	}
	if recomputed != snap.Hash {
		t.Errorf("recomputed hash %q != snapshot hash %q — content_origin must be covered by hash (REL-053)", recomputed, snap.Hash)
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
		t.Error("signature did not verify with a populated content_origin field (REL-075)")
	}
}

// TestBuildSignatureVerifies asserts the snapshot's signature verifies
// under the feeder's own signing public key over {generation, hash}, and
// fails under a different key (REL-075).
func TestBuildSignatureVerifies(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	snap, err := Build(img, "https://origin.example", id, nil)
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

	snap, err := Build(img, "https://origin.example", id, nil)
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

	snap, err := Build(img, "https://origin.example", id, nil)
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
	otherSnap, err := Build(otherImg, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("Build (other image): %v", err)
	}
	if otherSnap.Hash == snap.Hash {
		t.Error("different images produced the same hash — want different hashes for different sections content")
	}
}

func TestBuildRejectsNilIdentity(t *testing.T) {
	img := loadTestImage(t)
	if _, err := Build(img, "https://origin.example", nil, nil); err == nil {
		t.Error("Build(nil identity) succeeded, want an error")
	}
}

// TestBuildWithGrantsRidesAndVerifies asserts a populated grants slice
// rides into `sections.pairing_grants` verbatim, and that the snapshot's
// hash/signature still verify with grants present (REL-053: grants are
// part of `sections`, so they're covered by `hash`; REL-075 transitively).
// This is the Task 6 carry — grant.Mint()'s one pairing grant rides the
// snapshot to the relay.
func TestBuildWithGrantsRidesAndVerifies(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	g := wire.PairingGrant{
		GrantID:                "grant-test-1",
		Purpose:                "pairing",
		ResultingPrincipalKind: "screen",
		TTL:                    900,
		RedemptionMode:         "one-time",
		IssuedAt:               1752537000000,
	}

	snap, err := Build(img, "https://origin.example", id, []wire.PairingGrant{g})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(snap.Sections.PairingGrants) != 1 || snap.Sections.PairingGrants[0] != g {
		t.Fatalf("Sections.PairingGrants = %#v, want exactly [%#v]", snap.Sections.PairingGrants, g)
	}

	recomputed, err := hashSections(snap.Sections)
	if err != nil {
		t.Fatalf("hashSections: %v", err)
	}
	if recomputed != snap.Hash {
		t.Errorf("recomputed hash %q != snapshot hash %q — grants must be covered by hash (REL-053)", recomputed, snap.Hash)
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
		t.Error("signature did not verify with a populated pairing_grants section")
	}
}

// TestBuildCastSingleItemByteIdenticalToBuild asserts BuildCast, called with
// one CastItem carrying no ContentType/DurationMS ({Bytes: img}), produces
// wire output byte-identical to Build(img, ...) itself — the "keep the
// existing single-item behavior byte-identical" property this codebase's
// new ordered-cast capability must preserve for every existing caller
// (Build's own doc comment). Both hash and full JSON marshal are compared.
func TestBuildCastSingleItemByteIdenticalToBuild(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	viaBuild, err := Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	viaCast, err := BuildCast([]CastItem{{Bytes: img}}, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("BuildCast: %v", err)
	}

	// The signature itself differs run-to-run only if the signed bytes
	// differ (ed25519 is deterministic given identical key + message), so
	// comparing hash (and the full sections marshal) is the load-bearing
	// check; Generation is a shared constant (1) on this path.
	if viaCast.Hash != viaBuild.Hash {
		t.Errorf("BuildCast single-item hash = %q, want %q (Build's own hash) — single-item behavior must stay byte-identical", viaCast.Hash, viaBuild.Hash)
	}
	if viaCast.Generation != viaBuild.Generation {
		t.Errorf("BuildCast Generation = %d, want %d", viaCast.Generation, viaBuild.Generation)
	}

	rawBuild, err := json.Marshal(viaBuild.Sections)
	if err != nil {
		t.Fatalf("Marshal(viaBuild.Sections): %v", err)
	}
	rawCast, err := json.Marshal(viaCast.Sections)
	if err != nil {
		t.Fatalf("Marshal(viaCast.Sections): %v", err)
	}
	if string(rawBuild) != string(rawCast) {
		t.Errorf("BuildCast single-item sections marshal differs from Build's:\nBuild:     %s\nBuildCast: %s", rawBuild, rawCast)
	}

	// In particular, content_type/duration_ms must be ABSENT (not merely
	// "image"/0) — this is what makes the marshal genuinely byte-identical
	// rather than coincidentally equal.
	item := viaCast.Sections.ScreenPrograms[0].Content[0]
	if item.ContentType != "" {
		t.Errorf("single-item BuildCast ContentType = %q, want \"\" (omitted on the wire)", item.ContentType)
	}
	if item.DurationMS != 0 {
		t.Errorf("single-item BuildCast DurationMS = %d, want 0 (omitted on the wire)", item.DurationMS)
	}
}

// TestBuildCastOrderedMultiItemEachIndependentlyVerifiable asserts BuildCast
// with several items produces a `content` array in the SAME order, one
// ContentRef per item, each carrying its OWN content_type/duration_ms and
// its OWN asset_ref — the per-item content-digest integrity property this
// codebase's multi-item cast support requires (every item, not just the
// first, must be independently verifiable against its own bytes).
func TestBuildCastOrderedMultiItemEachIndependentlyVerifiable(t *testing.T) {
	id := testIdentity(t)

	items := []CastItem{
		{Bytes: []byte("cast-item-one-image-bytes"), ContentType: "image", DurationMS: 8000},
		{Bytes: []byte("cast-item-two-image-bytes"), ContentType: "image", DurationMS: 6000},
		// Fixture bytes standing in for a video asset — this test exercises
		// the content_type/duration_ms plumbing only; it is not a claim
		// that these bytes are a real video (see testdata/demo's own doc
		// for this codebase's real demo assets).
		{Bytes: []byte("cast-item-three-fixture-video-bytes"), ContentType: "video", DurationMS: 12000},
	}

	snap, err := BuildCast(items, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("BuildCast: %v", err)
	}

	content := snap.Sections.ScreenPrograms[0].Content
	if len(content) != len(items) {
		t.Fatalf("len(Content) = %d, want %d", len(content), len(items))
	}

	seenAssetRefs := make(map[string]bool, len(items))
	for i, item := range items {
		got := content[i]
		wantAssetRef := signhash.ContentID(item.Bytes)
		if got.AssetRef != wantAssetRef {
			t.Errorf("item %d: AssetRef = %q, want %q", i, got.AssetRef, wantAssetRef)
		}
		if seenAssetRefs[got.AssetRef] {
			t.Errorf("item %d: asset_ref %q collides with an earlier item — fixture bytes must be distinct", i, got.AssetRef)
		}
		seenAssetRefs[got.AssetRef] = true

		wantURL := "https://origin.example/content/" + wantAssetRef[len("sha256:"):]
		if got.URL != wantURL {
			t.Errorf("item %d: URL = %q, want %q", i, got.URL, wantURL)
		}
		if got.ContentType != item.ContentType {
			t.Errorf("item %d: ContentType = %q, want %q", i, got.ContentType, item.ContentType)
		}
		if got.DurationMS != item.DurationMS {
			t.Errorf("item %d: DurationMS = %d, want %d", i, got.DurationMS, item.DurationMS)
		}
	}

	// The hash/signature cover the whole ordered array — recomputing and
	// re-verifying confirms every item (not just the first) rides REL-053's
	// hash and REL-075's signature.
	recomputed, err := hashSections(snap.Sections)
	if err != nil {
		t.Fatalf("hashSections: %v", err)
	}
	if recomputed != snap.Hash {
		t.Errorf("recomputed hash %q != snapshot hash %q", recomputed, snap.Hash)
	}
}

// TestBuildCastRejectsEmptyItems asserts BuildCast refuses a call with no
// items rather than silently building a screen-program with an empty
// `content` array — an authoring mistake this package can detect cheaply
// at the boundary rather than downstream.
func TestBuildCastRejectsEmptyItems(t *testing.T) {
	id := testIdentity(t)
	if _, err := BuildCast(nil, "https://origin.example", id, nil); err == nil {
		t.Error("BuildCast(nil items) = nil error, want an error")
	}
	if _, err := BuildCast([]CastItem{}, "https://origin.example", id, nil); err == nil {
		t.Error("BuildCast(empty items) = nil error, want an error")
	}
}
